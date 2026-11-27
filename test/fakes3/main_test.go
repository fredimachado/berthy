package main

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"berthy/internal/objectstore"
)

const (
	testBucket    = "berthy-test"
	testAccessKey = "BERTHYTESTACCESS"
	testSecretKey = "berthy-test-secret-key-7f3a9c"
)

// startFake serves a fresh fake on loopback. The control plane is reached from the test's own loopback address.
func startFake(t *testing.T) (*fake, string) {
	t.Helper()
	f, err := newFake(testBucket)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, srv.URL
}

// startFakeTCP serves on a real loopback listener. Use it when a held S3 request must not stall other
// clients: httptest can serialize keep-alive traffic behind one blocked handler.
// Teardown cancels ctx and releases the hold, then joins the worker, then closes the server, so a
// Put does not retry against a closed listener on the real clock.
func startFakeTCP(t *testing.T) (*fake, string, context.Context, *workerJoin) {
	t.Helper()
	f, err := newFake(testBucket)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: f}
	ctx, cancel := context.WithCancel(context.Background())
	j := &workerJoin{}
	t.Cleanup(func() {
		cancel()
		f.clearHold()
		if j.done != nil && !j.took {
			<-j.done
		}
		srv.Close()
		ln.Close()
	})
	go srv.Serve(ln)
	return f, "http://" + ln.Addr().String(), ctx, j
}

// workerJoin receives the worker's one result. Teardown joins unless take already did.
type workerJoin struct {
	done chan error
	took bool
}

func (j *workerJoin) bind(done chan error) { j.done = done }

func (j *workerJoin) take() error {
	err := <-j.done
	j.took = true
	return err
}

// do is for worker goroutines: it must not call t.Fatal, which would exit without signalling.
func do(method, url, body string) (*http.Response, error) {
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	return http.DefaultClient.Do(req)
}

func holdWaitingAt(t *testing.T, base string) holdView {
	t.Helper()
	var view holdView
	decodeJSON(t, send(t, http.MethodGet, base+"/_fake/hold", ""), &view)
	return view
}

func send(t *testing.T, method, url, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func decodeJSON(t *testing.T, resp *http.Response, into any) {
	t.Helper()
	if err := json.NewDecoder(resp.Body).Decode(into); err != nil {
		t.Fatalf("decode control response: %v", err)
	}
}

func objectsAt(t *testing.T, base, prefix string) []map[string]any {
	t.Helper()
	var out []map[string]any
	decodeJSON(t, send(t, http.MethodGet, base+"/_fake/objects?prefix="+prefix, ""), &out)
	return out
}

func uploadsAt(t *testing.T, base string) []map[string]any {
	t.Helper()
	var out []map[string]any
	decodeJSON(t, send(t, http.MethodGet, base+"/_fake/uploads", ""), &out)
	return out
}

func TestControlPlaneAnswersOnlyLoopback(t *testing.T) {
	f, err := newFake(testBucket)
	if err != nil {
		t.Fatal(err)
	}
	for _, remote := range []string{"192.0.2.7:5000", "10.0.0.5:4000"} {
		req := httptest.NewRequest(http.MethodGet, "/_fake/objects", nil)
		req.RemoteAddr = remote
		rec := httptest.NewRecorder()
		f.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("control plane from %s: %d", remote, rec.Code)
		}
	}
	req := httptest.NewRequest(http.MethodGet, "/_fake/objects", nil)
	req.RemoteAddr = "127.0.0.1:5000"
	rec := httptest.NewRecorder()
	f.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("loopback control plane: %d", rec.Code)
	}
}

func TestFailInjectionFailsTheNextMatchingRequestOnce(t *testing.T) {
	_, base := startFake(t)
	// Fail the first UploadPart with a non-retryable status, so the client gives up at once. The client sends a 20 MiB
	// body as one part, because its part size is 64 MiB.
	send(t, http.MethodPost, base+"/_fake/fail", `{"op":"UploadPart","after":0,"status":403}`)
	s, err := objectstore.New(objectstore.Config{Endpoint: base, Bucket: testBucket, AccessKey: testAccessKey, SecretKey: testSecretKey})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	src := bytes.Repeat([]byte("x"), 20<<20)
	if _, err := s.Put(ctx, "big.bin", struct{ io.Reader }{bytes.NewReader(src)}, nil); err == nil {
		t.Fatal("upload succeeded through an injected UploadPart failure")
	}
	if objs := objectsAt(t, base, ""); len(objs) != 0 {
		t.Fatalf("object left after failed upload: %v", objs)
	}
	if uploads := uploadsAt(t, base); len(uploads) != 0 {
		t.Fatalf("multipart upload left after failed upload: %v", uploads)
	}
	// The rule fired once. The same upload now succeeds.
	if _, err := s.Put(ctx, "big.bin", struct{ io.Reader }{bytes.NewReader(src)}, nil); err != nil {
		t.Fatalf("second upload after the one-shot rule: %v", err)
	}
	if objs := objectsAt(t, base, ""); len(objs) != 1 || objs[0]["key"] != "big.bin" {
		t.Fatalf("objects after success: %v", objs)
	}
}

func TestTransientServerErrorIsRetriedByTheClient(t *testing.T) {
	_, base := startFake(t)
	send(t, http.MethodPost, base+"/_fake/fail", `{"op":"UploadPart","after":0,"status":500}`)
	s, err := objectstore.New(objectstore.Config{Endpoint: base, Bucket: testBucket, AccessKey: testAccessKey, SecretKey: testSecretKey})
	if err != nil {
		t.Fatal(err)
	}
	src := bytes.Repeat([]byte("y"), 20<<20)
	if _, err := s.Put(context.Background(), "retried.bin", struct{ io.Reader }{bytes.NewReader(src)}, nil); err != nil {
		t.Fatalf("a single 500 should be retried by the client: %v", err)
	}
}

func TestHoldBlocksMatchingRequestsUntilCleared(t *testing.T) {
	f, base, ctx, j := startFakeTCP(t)
	if resp := send(t, http.MethodPost, base+"/_fake/hold", `{"op":"UploadPart"}`); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("arm hold: %d", resp.StatusCode)
	}
	h := f.currentHold()
	if h == nil {
		t.Fatal("hold was not armed")
	}
	s, err := objectstore.New(objectstore.Config{Endpoint: base, Bucket: testBucket, AccessKey: testAccessKey, SecretKey: testSecretKey})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		var err error
		defer func() { done <- err }()
		src := bytes.Repeat([]byte("h"), 20<<20)
		_, err = s.Put(ctx, "held.bin", struct{ io.Reader }{bytes.NewReader(src)}, nil)
	}()
	j.bind(done)
	// Synchronize on UploadPart entering waitHold, not on CreateMultipartUpload (which happens before the hold).
	select {
	case <-h.entered:
	case err := <-done:
		j.took = true
		t.Fatalf("upload finished without entering hold: %v", err)
	}
	if view := holdWaitingAt(t, base); view.Waiting < 1 || view.Op != "UploadPart" {
		t.Fatalf("hold view while blocked: %+v", view)
	}
	// Still in waitHold: the part has not reached the S3 handler, so nothing is stored.
	if objs := objectsAt(t, base, ""); len(objs) != 0 {
		t.Fatalf("object stored before release: %v", objs)
	}
	select {
	case err := <-done:
		j.took = true
		t.Fatalf("upload finished while held: %v", err)
	default:
	}
	if resp := send(t, http.MethodDelete, base+"/_fake/hold", ""); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("clear hold: %d", resp.StatusCode)
	}
	if err := j.take(); err != nil {
		t.Fatalf("upload after release: %v", err)
	}
	if objs := objectsAt(t, base, ""); len(objs) != 1 || objs[0]["key"] != "held.bin" {
		t.Fatalf("objects after held upload: %v", objs)
	}
}

func TestHoldValidationAndIgnoresOtherOps(t *testing.T) {
	f, base, _, j := startFakeTCP(t)
	for _, body := range []string{
		`{"op":"HeadBucket"}`,
		`{"op":"UploadPart","extra":true}`,
		`not json`,
	} {
		if resp := send(t, http.MethodPost, base+"/_fake/hold", body); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("hold %s: %d", body, resp.StatusCode)
		}
	}
	if resp := send(t, http.MethodPost, base+"/_fake/hold", `{"op":"UploadPart"}`); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("arm hold: %d", resp.StatusCode)
	}
	h := f.currentHold()
	if h == nil {
		t.Fatal("hold was not armed")
	}
	// Non-matching ops must finish without entering the hold. If a regression held them, entered
	// fires and this fails instead of hanging on the blocked request.
	done := make(chan error, 1)
	go func() {
		var err error
		defer func() { done <- err }()
		put, err := do(http.MethodPut, base+"/"+testBucket+"/other.txt", "ok")
		if err != nil {
			return
		}
		defer put.Body.Close()
		if put.StatusCode != http.StatusOK {
			err = fmt.Errorf("seed put: %d", put.StatusCode)
			return
		}
		got, err := do(http.MethodGet, base+"/"+testBucket+"/other.txt", "")
		if err != nil {
			return
		}
		defer got.Body.Close()
		if got.StatusCode != http.StatusOK {
			err = fmt.Errorf("GetObject under UploadPart hold: %d", got.StatusCode)
			return
		}
	}()
	j.bind(done)
	select {
	case <-h.entered:
		t.Fatal("a non-matching request entered the UploadPart hold")
	case err := <-done:
		j.took = true
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestArmingASecondHoldReportsOnlyTheNewWaiters(t *testing.T) {
	f, base, ctx, j := startFakeTCP(t)
	if resp := send(t, http.MethodPost, base+"/_fake/hold", `{"op":"UploadPart"}`); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("arm hold: %d", resp.StatusCode)
	}
	h1 := f.currentHold()
	if h1 == nil {
		t.Fatal("hold was not armed")
	}
	s, err := objectstore.New(objectstore.Config{Endpoint: base, Bucket: testBucket, AccessKey: testAccessKey, SecretKey: testSecretKey})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		var err error
		defer func() { done <- err }()
		src := bytes.Repeat([]byte("h"), 20<<20)
		_, err = s.Put(ctx, "held.bin", struct{ io.Reader }{bytes.NewReader(src)}, nil)
	}()
	j.bind(done)
	select {
	case <-h1.entered:
	case err := <-done:
		j.took = true
		t.Fatalf("upload finished without entering hold: %v", err)
	}
	if view := holdWaitingAt(t, base); view.Op != "UploadPart" || view.Waiting < 1 {
		t.Fatalf("hold view while blocked: %+v", view)
	}
	if resp := send(t, http.MethodPost, base+"/_fake/hold", `{"op":"PutObject"}`); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("re-arm: %d", resp.StatusCode)
	}
	select {
	case <-h1.release:
	default:
		// Unblock the first handler before failing; cleanup only clears the new hold.
		close(h1.release)
		t.Fatal("re-arm left the previous hold's release open")
	}
	h2 := f.currentHold()
	if h2 == nil || h2 == h1 {
		t.Fatalf("re-arm should install a new hold, got %v after %v", h2, h1)
	}
	view := holdWaitingAt(t, base)
	if view.Op != "PutObject" {
		t.Fatalf("GET op after re-arm: %+v", view)
	}
	if view.Waiting != 0 {
		t.Fatalf("GET waiting after re-arm reports leftover waiters: %+v", view)
	}
}

func TestFailRuleValidationAndClear(t *testing.T) {
	_, base := startFake(t)
	for _, body := range []string{
		`{"op":"HeadBucket","after":0,"status":500}`,
		`{"op":"PutObject","after":-1,"status":500}`,
		`{"op":"PutObject","after":0,"status":200}`,
		`{"op":"PutObject","after":0,"status":500,"extra":true}`,
		`not json`,
	} {
		if resp := send(t, http.MethodPost, base+"/_fake/fail", body); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("rule %s: %d", body, resp.StatusCode)
		}
	}
	send(t, http.MethodPost, base+"/_fake/fail", `{"op":"PutObject","after":0,"status":500}`)
	if resp := send(t, http.MethodDelete, base+"/_fake/fail", ""); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("clear: %d", resp.StatusCode)
	}
	resp := send(t, http.MethodPut, base+"/"+testBucket+"/cleared.txt", "fine")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("put after clear: %d", resp.StatusCode)
	}
}

func TestObjectsListsSizeAndMetadata(t *testing.T) {
	_, base := startFake(t)
	req, _ := http.NewRequest(http.MethodPut, base+"/"+testBucket+"/a/b.txt", strings.NewReader("abc"))
	req.Header.Set("X-Amz-Meta-Sha256", "deadbeef")
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("put: %v %v", resp, err)
	}
	objs := objectsAt(t, base, "a/")
	if len(objs) != 1 || objs[0]["key"] != "a/b.txt" || objs[0]["size"] != float64(3) {
		t.Fatalf("objects: %v", objs)
	}
	meta, _ := objs[0]["meta"].(map[string]any)
	if meta["sha256"] != "deadbeef" {
		t.Fatalf("meta: %v", objs[0]["meta"])
	}
	if empty := objectsAt(t, base, "none/"); empty == nil || len(empty) != 0 {
		t.Fatalf("empty listing should be [] not null: %v", empty)
	}
}

func TestUploadsListsInProgressMultipartUploads(t *testing.T) {
	_, base := startFake(t)
	resp := send(t, http.MethodPost, base+"/"+testBucket+"/big.bin?uploads", "")
	var created struct {
		UploadID string `xml:"UploadId"`
	}
	body, _ := io.ReadAll(resp.Body)
	if err := xml.Unmarshal(body, &created); err != nil || created.UploadID == "" {
		t.Fatalf("create upload: %s %v", body, err)
	}
	uploads := uploadsAt(t, base)
	if len(uploads) != 1 || uploads[0]["key"] != "big.bin" || uploads[0]["upload_id"] != created.UploadID {
		t.Fatalf("uploads: %v", uploads)
	}
	del := send(t, http.MethodDelete, base+"/"+testBucket+"/big.bin?uploadId="+created.UploadID, "")
	if del.StatusCode != http.StatusNoContent {
		t.Fatalf("abort: %d", del.StatusCode)
	}
	if left := uploadsAt(t, base); len(left) != 0 {
		t.Fatalf("upload still listed after abort: %v", left)
	}
}

func TestFlagsRefuseNonLoopbackListenAndBadBucket(t *testing.T) {
	if err := checkFlags("0.0.0.0:19900", testBucket); err == nil {
		t.Error("wildcard listen address accepted")
	}
	if err := checkFlags("192.0.2.9:19900", testBucket); err == nil {
		t.Error("non-loopback listen address accepted")
	}
	if err := checkFlags("127.0.0.1:19900", "Bad_Bucket"); err == nil {
		t.Error("invalid bucket accepted")
	}
	if err := checkFlags("127.0.0.1:19900", ""); err == nil {
		t.Error("missing bucket accepted")
	}
	if err := checkFlags("127.0.0.1:19900", testBucket); err != nil {
		t.Errorf("valid flags refused: %v", err)
	}
	if err := checkFlags("localhost:19900", testBucket); err != nil {
		t.Errorf("localhost refused: %v", err)
	}
}
