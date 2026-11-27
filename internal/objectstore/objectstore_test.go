package objectstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"berthy/internal/dialtest"

	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"
)

const (
	testBucket    = "berthy-test"
	testAccessKey = "BERTHYTESTACCESS"
	testSecretKey = "berthy-test-secret-key-7f3a9c"
)

// fakeS3 is an in-process gofakes3 server on loopback. It counts multipart uploads created and parts sent, so a test
// can tell whether an upload used multipart. gofakes3 does not check signatures; the client still signs its requests.
type fakeS3 struct {
	URL     string
	creates atomic.Int64
	parts   atomic.Int64
}

func startFake(t *testing.T) *fakeS3 {
	t.Helper()
	backend := s3mem.New()
	if err := backend.CreateBucket(testBucket); err != nil {
		t.Fatal(err)
	}
	s3 := gofakes3.New(backend).Server()
	f := &fakeS3{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Has("partNumber") {
			f.parts.Add(1)
		}
		if r.Method == http.MethodPost && q.Has("uploads") {
			f.creates.Add(1)
		}
		s3.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	f.URL = srv.URL
	return f
}

func newStore(t *testing.T, endpoint string) Store {
	t.Helper()
	s, err := New(Config{Endpoint: endpoint, Bucket: testBucket, AccessKey: testAccessKey, SecretKey: testSecretKey})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// incompleteUploads lists the multipart uploads still in progress in the bucket.
func incompleteUploads(t *testing.T, s Store) []string {
	t.Helper()
	client := s.(*s3Store).client
	var keys []string
	for u := range client.ListIncompleteUploads(context.Background(), testBucket, "", true) {
		if u.Err != nil {
			t.Fatal(u.Err)
		}
		keys = append(keys, u.Key)
	}
	return keys
}

func sha(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// pattern returns n deterministic bytes.
func pattern(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7 + i>>8)
	}
	return b
}

// unsized hides the reader's length, so the client sees an upload of unknown size.
type unsized struct{ io.Reader }

// failingReader returns data until left bytes have been read, then returns err.
type failingReader struct {
	r    io.Reader
	left int
	err  error
}

func (f *failingReader) Read(p []byte) (int, error) {
	if f.left <= 0 {
		return 0, f.err
	}
	if len(p) > f.left {
		p = p[:f.left]
	}
	n, err := f.r.Read(p)
	f.left -= n
	return n, err
}

// cancelingReader cancels the upload's context once left bytes have been read. It keeps returning data, so the
// cancellation is what stops the upload.
type cancelingReader struct {
	r      io.Reader
	left   int
	cancel context.CancelFunc
}

func (c *cancelingReader) Read(p []byte) (int, error) {
	if c.left <= 0 && c.cancel != nil {
		c.cancel()
		c.cancel = nil
	}
	if len(p) > c.left && c.left > 0 {
		p = p[:c.left]
	}
	n, err := c.r.Read(p)
	c.left -= n
	return n, err
}

func TestPutGetRoundTripKeepsMetadata(t *testing.T) {
	f := startFake(t)
	s := newStore(t, f.URL)
	ctx := context.Background()
	const key = "files/uploads/2026-11-16.tar.zst"
	body := []byte("archive bytes")
	n, err := s.Put(ctx, key, bytes.NewReader(body), map[string]string{"sha256": sha(body), "Berthy-App": "files"})
	if err != nil || n != int64(len(body)) {
		t.Fatalf("put: %d %v", n, err)
	}
	rc, info, err := s.Get(ctx, key)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	got, err := io.ReadAll(rc)
	rc.Close()
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("body: %q %v", got, err)
	}
	if info.Key != key || info.Size != int64(len(body)) || info.Meta["sha256"] != sha(body) || info.Meta["berthy-app"] != "files" {
		t.Fatalf("get info: %+v", info)
	}
	stat, err := s.Stat(ctx, key)
	if err != nil || stat.Size != info.Size || stat.Meta["berthy-app"] != "files" || stat.Modified.IsZero() {
		t.Fatalf("stat: %+v %v", stat, err)
	}
	if _, _, err := s.Get(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get missing: %v", err)
	}
}

func TestPutStreamsUnknownSizeWithMultipart(t *testing.T) {
	f := startFake(t)
	s := newStore(t, f.URL)
	ctx := context.Background()
	const key = "big/volume.tar.zst"
	const size = 20 << 20
	src := pattern(size)
	n, err := s.Put(ctx, key, unsized{bytes.NewReader(src)}, nil)
	if err != nil || n != size {
		t.Fatalf("put: %d %v", n, err)
	}
	if creates := f.creates.Load(); creates != 1 {
		t.Fatalf("20 MiB upload created %d multipart uploads, want 1", creates)
	}
	rc, info, err := s.Get(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil || sha(got) != sha(src) || info.Size != size {
		t.Fatalf("round trip: size %d sha match %v err %v", info.Size, sha(got) == sha(src), err)
	}
}

func TestPutSendsOnePartPerPartSize(t *testing.T) {
	f := startFake(t)
	s := newStore(t, f.URL)
	s.(*s3Store).partSize = 5 << 20
	ctx := context.Background()
	const size = 12 << 20
	src := pattern(size)
	if _, err := s.Put(ctx, "parts/three.bin", unsized{bytes.NewReader(src)}, nil); err != nil {
		t.Fatal(err)
	}
	if parts := f.parts.Load(); parts != 3 {
		t.Fatalf("12 MiB in 5 MiB parts sent %d parts, want 3", parts)
	}
	rc, _, err := s.Get(ctx, "parts/three.bin")
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	if got, err := io.ReadAll(rc); err != nil || sha(got) != sha(src) {
		t.Fatalf("parts reassembled wrong: %d bytes %v", len(got), err)
	}
}

func TestPutWhoseReaderFailsLeavesNoObjectAndNoUpload(t *testing.T) {
	f := startFake(t)
	s := newStore(t, f.URL)
	ctx := context.Background()
	const key = "big/failed.tar.zst"
	src := &failingReader{r: bytes.NewReader(pattern(20 << 20)), left: 17 << 20, err: errors.New("source read error")}
	_, err := s.Put(ctx, key, unsized{src}, nil)
	if err == nil || !strings.HasPrefix(err.Error(), "upload failed") {
		t.Fatalf("put error: %v", err)
	}
	if _, err := s.Stat(ctx, key); !errors.Is(err, ErrNotFound) {
		t.Fatalf("object exists after failed put: %v", err)
	}
	if uploads := incompleteUploads(t, s); len(uploads) != 0 {
		t.Fatalf("in-progress uploads left behind: %v", uploads)
	}
}

func TestPutCancelledDuringUploadLeavesNoObjectAndNoUpload(t *testing.T) {
	f := startFake(t)
	s := newStore(t, f.URL)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	const key = "big/cancelled.tar.zst"
	src := &cancelingReader{r: bytes.NewReader(pattern(20 << 20)), left: 17 << 20, cancel: cancel}
	_, err := s.Put(ctx, key, unsized{src}, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("put error: %v", err)
	}
	if _, err := s.Stat(context.Background(), key); !errors.Is(err, ErrNotFound) {
		t.Fatalf("object exists after cancelled put: %v", err)
	}
	if uploads := incompleteUploads(t, s); len(uploads) != 0 {
		t.Fatalf("in-progress uploads left behind after cancel: %v", uploads)
	}
}

func TestPutWithCancelledContextStoresNothing(t *testing.T) {
	f := startFake(t)
	s := newStore(t, f.URL)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	const key = "small/cancelled.bin"
	_, err := s.Put(ctx, key, bytes.NewReader([]byte("x")), nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("put error: %v", err)
	}
	if _, err := s.Stat(context.Background(), key); !errors.Is(err, ErrNotFound) {
		t.Fatalf("object exists after cancelled put: %v", err)
	}
}

func TestPutRejectsInvalidMetadataBeforeSending(t *testing.T) {
	f := startFake(t)
	s := newStore(t, f.URL)
	ctx := context.Background()
	for _, meta := range []map[string]string{{"bad key": "x"}, {"sha256": "line\nbreak"}, {"": "x"}} {
		if _, err := s.Put(ctx, "meta.bin", strings.NewReader("x"), meta); err == nil {
			t.Errorf("metadata %v accepted", meta)
		}
	}
	if _, err := s.Stat(ctx, "meta.bin"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("object stored despite invalid metadata: %v", err)
	}
}

func TestListIsSortedRecursiveAndCarriesMetadata(t *testing.T) {
	f := startFake(t)
	s := newStore(t, f.URL)
	ctx := context.Background()
	for _, key := range []string{"logs/b.txt", "logs/a.txt", "logs/nested/c.txt", "other/x.txt"} {
		if _, err := s.Put(ctx, key, strings.NewReader(key), map[string]string{"kind": key}); err != nil {
			t.Fatal(err)
		}
	}
	list, err := s.List(ctx, "logs/")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"logs/a.txt", "logs/b.txt", "logs/nested/c.txt"}
	if len(list) != len(want) {
		t.Fatalf("list: %+v", list)
	}
	for i, o := range list {
		if o.Key != want[i] || o.Size != int64(len(want[i])) {
			t.Fatalf("entry %d: %+v", i, o)
		}
		stat, err := s.Stat(ctx, o.Key)
		if err != nil || stat.Meta["kind"] != want[i] {
			t.Fatalf("stat metadata %d: %+v %v", i, stat, err)
		}
	}
}

func TestListDoesNotStatObjects(t *testing.T) {
	backend := s3mem.New()
	if err := backend.CreateBucket(testBucket); err != nil {
		t.Fatal(err)
	}
	inner := gofakes3.New(backend).Server()
	var denyHead atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// After seeding, HeadObject under other/ is denied; listing remains allowed.
		if denyHead.Load() && r.Method == http.MethodHead && strings.Contains(r.URL.Path, "/other/") {
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusForbidden)
			io.WriteString(w, `<?xml version="1.0"?><Error><Code>AccessDenied</Code><Message>denied</Message></Error>`)
			return
		}
		inner.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	s := newStore(t, srv.URL)
	ctx := context.Background()
	for _, key := range []string{"demo/prod/a.db", "other/prod/secret.db"} {
		if _, err := s.Put(ctx, key, strings.NewReader(key), nil); err != nil {
			t.Fatal(err)
		}
	}
	denyHead.Store(true)
	if _, err := s.Stat(ctx, "other/prod/secret.db"); err == nil || !strings.Contains(err.Error(), "access denied") {
		t.Fatalf("stat other: %v", err)
	}
	list, err := s.List(ctx, "")
	if err != nil {
		t.Fatalf("list stated a denied object: %v", err)
	}
	if len(list) != 2 || list[0].Key != "demo/prod/a.db" || list[1].Key != "other/prod/secret.db" {
		t.Fatalf("list: %+v", list)
	}
}

func TestDeleteIsIdempotent(t *testing.T) {
	f := startFake(t)
	s := newStore(t, f.URL)
	ctx := context.Background()
	if _, err := s.Put(ctx, "gone.bin", strings.NewReader("x"), nil); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, "gone.bin"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := s.Delete(ctx, "gone.bin"); err != nil {
		t.Fatalf("delete missing: %v", err)
	}
	if _, err := s.Stat(ctx, "gone.bin"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("stat after delete: %v", err)
	}
}

func TestErrorsNeverCarryTheSecretKeyOrRequestURLs(t *testing.T) {
	denied := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>AccessDenied</Code><Message>denied</Message></Error>`)
	}))
	defer denied.Close()
	s := newStore(t, denied.URL)
	_, err := s.Stat(context.Background(), "x")
	if err == nil || !strings.Contains(err.Error(), "access denied") {
		t.Fatalf("access denied: %v", err)
	}
	checkHygiene(t, err)

	endpoint := dialtest.HangUp(t)
	down := newStore(t, endpoint)
	_, err = down.Put(context.Background(), "x", strings.NewReader("x"), nil)
	if err == nil || !strings.HasPrefix(err.Error(), "upload failed: object storage unavailable") {
		t.Fatalf("unreachable: %v", err)
	}
	checkHygiene(t, err)
}

func checkHygiene(t *testing.T, err error) {
	t.Helper()
	msg := err.Error()
	for _, leak := range []string{testSecretKey, testAccessKey, "?", "X-Amz", "Signature", "http://", "https://"} {
		if strings.Contains(msg, leak) {
			t.Errorf("error %q contains %q", msg, leak)
		}
	}
}

func TestNewRejectsEndpointsThatCannotCarryCredentials(t *testing.T) {
	for _, endpoint := range []string{"http://10.0.0.5:9000", "http://s3.example.com", "https://s3.example.com/bucket", "https://s3.example.com?x=1", "https://user:pw@s3.example.com", "ftp://s3.example.com", "https://"} {
		_, err := New(Config{Endpoint: endpoint, Bucket: testBucket, AccessKey: testAccessKey, SecretKey: testSecretKey})
		if err == nil {
			t.Errorf("endpoint %q accepted", endpoint)
		}
	}
}

func TestNewDefaultsEndpointAndRegion(t *testing.T) {
	s, err := New(Config{Bucket: testBucket, AccessKey: testAccessKey, SecretKey: testSecretKey})
	if err != nil {
		t.Fatal(err)
	}
	impl := s.(*s3Store)
	if impl.client.EndpointURL().String() != "https://s3.amazonaws.com" || impl.region != "us-east-1" || !impl.secure {
		t.Fatalf("defaults: %s %s", impl.client.EndpointURL(), impl.region)
	}
}

func TestNewRequiresCredentials(t *testing.T) {
	if _, err := New(Config{Endpoint: "http://127.0.0.1:1", Bucket: testBucket, SecretKey: testSecretKey}); err == nil {
		t.Fatal("missing access key accepted")
	}
}
