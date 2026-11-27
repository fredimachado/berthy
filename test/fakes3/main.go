// Command fakes3 is an S3 server for tests and the WSL end-to-end runs. It serves gofakes3's in-memory backend on a
// loopback address, and a control plane under /_fake/ that is also loopback only. The control plane injects failures
// and reports what is stored.
//
// gofakes3 does not check request signatures. The client still signs its requests, but the fake accepts any signature.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"
)

const controlPrefix = "/_fake/"

// failOps are the S3 operations a failure can be injected into.
var failOps = map[string]bool{"UploadPart": true, "PutObject": true, "CompleteMultipartUpload": true, "GetObject": true}

var bucketPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)

// failRule fails the request after skip matching requests have passed. It fires once, then is cleared.
type failRule struct {
	op     string
	skip   int
	status int
}

type fake struct {
	bucket  string
	backend *s3mem.Backend
	s3      http.Handler

	mu     sync.Mutex
	rule   *failRule
	holdOp string
	holdCh chan struct{} // closed by DELETE /_fake/hold; nil when no hold is armed
}

func newFake(bucket string) (*fake, error) {
	backend := s3mem.New()
	if err := backend.CreateBucket(bucket); err != nil {
		return nil, err
	}
	return &fake{bucket: bucket, backend: backend, s3: gofakes3.New(backend).Server()}, nil
}

func (f *fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, controlPrefix) {
		if !fromLoopback(r.RemoteAddr) {
			http.Error(w, "the control plane answers only on loopback", http.StatusForbidden)
			return
		}
		f.control(w, r)
		return
	}
	if op := s3Op(r); op != "" {
		f.waitHold(op)
		if status, fire := f.takeFailure(op); fire {
			_, _ = io.Copy(io.Discard, r.Body)
			writeError(w, status)
			return
		}
	}
	f.s3.ServeHTTP(w, r)
}

// waitHold blocks while a hold is armed for op. The mutex is not held across the wait, so DELETE /_fake/hold can release it.
func (f *fake) waitHold(op string) {
	f.mu.Lock()
	ch, holdOp := f.holdCh, f.holdOp
	f.mu.Unlock()
	if ch != nil && holdOp == op {
		<-ch
	}
}

// s3Op names the operation a request performs when it is one that a failure can be injected into, and is empty otherwise.
func s3Op(r *http.Request) string {
	_, key, found := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if !found || key == "" {
		return "" // a bucket-level request
	}
	q := r.URL.Query()
	switch r.Method {
	case http.MethodPut:
		switch {
		case q.Has("uploadId") && q.Has("partNumber"):
			return "UploadPart"
		case q.Has("uploadId") || q.Has("partNumber") || r.Header.Get("X-Amz-Copy-Source") != "":
			return ""
		}
		return "PutObject"
	case http.MethodPost:
		if q.Has("uploadId") {
			return "CompleteMultipartUpload"
		}
	case http.MethodGet:
		if !q.Has("uploads") && !q.Has("uploadId") {
			return "GetObject"
		}
	}
	return ""
}

func (f *fake) takeFailure(op string) (int, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.rule == nil || f.rule.op != op {
		return 0, false
	}
	if f.rule.skip > 0 {
		f.rule.skip--
		return 0, false
	}
	status := f.rule.status
	f.rule = nil
	return status, true
}

func writeError(w http.ResponseWriter, status int) {
	code := map[int]string{http.StatusForbidden: "AccessDenied", http.StatusInternalServerError: "InternalError", http.StatusServiceUnavailable: "ServiceUnavailable"}[status]
	if code == "" {
		code = "InjectedFailure"
	}
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>%s</Code><Message>injected by fakes3</Message><RequestId>fakes3</RequestId></Error>`, code)
}

func (f *fake) control(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case controlPrefix + "fail":
		switch r.Method {
		case http.MethodPost:
			f.setRule(w, r)
		case http.MethodDelete:
			f.mu.Lock()
			f.rule = nil
			f.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	case controlPrefix + "hold":
		switch r.Method {
		case http.MethodPost:
			f.setHold(w, r)
		case http.MethodDelete:
			f.clearHold()
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	case controlPrefix + "objects":
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		objs, err := f.objects(r.URL.Query().Get("prefix"))
		writeJSON(w, objs, err)
	case controlPrefix + "uploads":
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		uploads, err := f.uploads()
		writeJSON(w, uploads, err)
	default:
		http.NotFound(w, r)
	}
}

type failRequest struct {
	Op     string `json:"op"`
	After  int    `json:"after"`
	Status int    `json:"status"`
}

type holdRequest struct {
	Op string `json:"op"`
}

// setRule installs a failure. The matching request after the first After ones fails with Status, which defaults to 500.
func (f *fake) setRule(w http.ResponseWriter, r *http.Request) {
	var req failRequest
	dec := json.NewDecoder(io.LimitReader(r.Body, 4096))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil || !failOps[req.Op] || req.After < 0 {
		http.Error(w, "invalid failure rule", http.StatusBadRequest)
		return
	}
	if req.Status == 0 {
		req.Status = http.StatusInternalServerError
	}
	if req.Status < 400 || req.Status > 599 {
		http.Error(w, "invalid failure status", http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	f.rule = &failRule{op: req.Op, skip: req.After, status: req.Status}
	f.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

// setHold arms a hold so matching S3 requests wait until clearHold. A prior hold is released first.
func (f *fake) setHold(w http.ResponseWriter, r *http.Request) {
	var req holdRequest
	dec := json.NewDecoder(io.LimitReader(r.Body, 4096))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil || !failOps[req.Op] {
		http.Error(w, "invalid hold", http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	if f.holdCh != nil {
		close(f.holdCh)
	}
	f.holdOp = req.Op
	f.holdCh = make(chan struct{})
	f.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (f *fake) clearHold() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.holdCh != nil {
		close(f.holdCh)
		f.holdCh = nil
		f.holdOp = ""
	}
}

type objectView struct {
	Key  string            `json:"key"`
	Size int64             `json:"size"`
	Meta map[string]string `json:"meta"`
}

// objects lists the stored objects under prefix, with their user metadata.
func (f *fake) objects(prefix string) ([]objectView, error) {
	list, err := f.backend.ListBucket(f.bucket, &gofakes3.Prefix{HasPrefix: prefix != "", Prefix: prefix}, gofakes3.ListBucketPage{})
	if err != nil {
		return nil, err
	}
	out := []objectView{}
	for _, c := range list.Contents {
		obj, err := f.backend.HeadObject(f.bucket, c.Key)
		if err != nil {
			return nil, err
		}
		meta := map[string]string{}
		for k, v := range obj.Metadata {
			if lk := strings.ToLower(k); strings.HasPrefix(lk, "x-amz-meta-") {
				meta[strings.TrimPrefix(lk, "x-amz-meta-")] = v
			}
		}
		out = append(out, objectView{Key: c.Key, Size: c.Size, Meta: meta})
	}
	return out, nil
}

type uploadView struct {
	Key      string `json:"key"`
	UploadID string `json:"upload_id"`
}

// uploads lists the multipart uploads in progress. gofakes3 keeps them in a private uploader, so the list comes from
// the S3 handler's own ListMultipartUploads.
func (f *fake) uploads() ([]uploadView, error) {
	req, err := http.NewRequest(http.MethodGet, "/"+f.bucket+"?uploads", nil)
	if err != nil {
		return nil, err
	}
	rec := &bufferWriter{header: http.Header{}}
	f.s3.ServeHTTP(rec, req)
	if rec.status != 0 && rec.status != http.StatusOK {
		return nil, fmt.Errorf("list uploads: status %d", rec.status)
	}
	var res struct {
		Uploads []struct {
			Key      string `xml:"Key"`
			UploadID string `xml:"UploadId"`
		} `xml:"Upload"`
	}
	if err := xml.Unmarshal(rec.body.Bytes(), &res); err != nil {
		return nil, err
	}
	out := []uploadView{}
	for _, u := range res.Uploads {
		out = append(out, uploadView{Key: u.Key, UploadID: u.UploadID})
	}
	return out, nil
}

// bufferWriter is the in-process response writer for the S3 handler's own calls.
type bufferWriter struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (b *bufferWriter) Header() http.Header         { return b.header }
func (b *bufferWriter) WriteHeader(code int)        { b.status = code }
func (b *bufferWriter) Write(p []byte) (int, error) { return b.body.Write(p) }

func writeJSON(w http.ResponseWriter, v any, err error) {
	if err != nil {
		http.Error(w, "control query failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func fromLoopback(remote string) bool {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// checkFlags refuses a listen address that is not loopback, because the fake accepts any signature, and a bucket name
// that S3 would refuse.
func checkFlags(listen, bucket string) error {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return fmt.Errorf("listen address %q: %w", listen, err)
	}
	ip := net.ParseIP(host)
	if !strings.EqualFold(host, "localhost") && (ip == nil || !ip.IsLoopback()) {
		return fmt.Errorf("listen address %q must be a loopback address", listen)
	}
	if !bucketPattern.MatchString(bucket) {
		return fmt.Errorf("bucket %q is not a valid S3 bucket name", bucket)
	}
	return nil
}

type summary struct {
	Listen  string    `json:"listen"`
	Bucket  string    `json:"bucket"`
	PID     int       `json:"pid"`
	Started time.Time `json:"started"`
}

func serve(listen, bucket, statePath string) error {
	if err := checkFlags(listen, bucket); err != nil {
		return err
	}
	f, err := newFake(bucket)
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return err
	}
	if statePath != "" {
		b, err := json.MarshalIndent(summary{Listen: ln.Addr().String(), Bucket: bucket, PID: os.Getpid(), Started: time.Now().UTC()}, "", "  ")
		if err == nil {
			err = os.WriteFile(statePath, append(b, '\n'), 0o600)
		}
		if err != nil {
			ln.Close()
			return err
		}
	}
	srv := &http.Server{Handler: f, ReadHeaderTimeout: 10 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		srv.Close()
	}()
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func main() {
	listen := flag.String("listen", "127.0.0.1:19900", "loopback address to serve on")
	bucket := flag.String("bucket", "", "bucket to create at start")
	state := flag.String("state", "", "optional path for a JSON summary, written once the server listens")
	flag.Parse()
	if err := serve(*listen, *bucket, *state); err != nil {
		fmt.Fprintln(os.Stderr, "fakes3:", err)
		os.Exit(1)
	}
}
