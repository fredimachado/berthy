package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestFake(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "requests.jsonl")
	f := &fake{recordPath: path, logw: io.Discard, now: func() time.Time { return time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC) }}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return srv, path
}

func do(t *testing.T, method, target, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, target, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer abc")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func listed(t *testing.T, srv *httptest.Server) []request {
	t.Helper()
	resp := do(t, http.MethodGet, srv.URL+"/_fake/requests", "")
	var out []request
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestRequestsAreRecordedWithMethodPathHeadersAndBody(t *testing.T) {
	srv, path := newTestFake(t)
	if resp := do(t, http.MethodPost, srv.URL+"/hook/x", `{"kind":"check"}`); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status %d", resp.StatusCode)
	}
	do(t, http.MethodGet, srv.URL+"/beat", "")
	got := listed(t, srv)
	if len(got) != 2 || got[0].Method != "POST" || got[0].Path != "/hook/x" || got[0].Body != `{"kind":"check"}` || got[0].Headers["Authorization"][0] != "Bearer abc" || got[1].Method != "GET" || got[1].Path != "/beat" {
		t.Fatalf("recorded %+v", got)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	var lines int
	sc := bufio.NewScanner(file)
	for sc.Scan() {
		var r request
		if json.Unmarshal(sc.Bytes(), &r) != nil || r.Path == "" {
			t.Fatalf("line %q", sc.Text())
		}
		lines++
	}
	if lines != 2 {
		t.Fatalf("%d lines in the record file", lines)
	}
}

func TestFailAnswersAnErrorStatusAndStillRecords(t *testing.T) {
	srv, _ := newTestFake(t)
	if resp := do(t, http.MethodPost, srv.URL+"/_fake/fail", `{"status":503}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("fail: %d", resp.StatusCode)
	}
	if resp := do(t, http.MethodGet, srv.URL+"/beat", ""); resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if len(listed(t, srv)) != 1 {
		t.Fatal("the failed request was not recorded")
	}
	for _, body := range []string{`{"status":200}`, `{"status":0,"x":1}`, `nope`} {
		if resp := do(t, http.MethodPost, srv.URL+"/_fake/fail", body); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: %d", body, resp.StatusCode)
		}
	}
	do(t, http.MethodPost, srv.URL+"/_fake/fail", `{"status":0}`)
	if resp := do(t, http.MethodGet, srv.URL+"/beat", ""); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("after resume: %d", resp.StatusCode)
	}
}

func TestResetClearsTheRecordAndTheFile(t *testing.T) {
	srv, path := newTestFake(t)
	do(t, http.MethodGet, srv.URL+"/beat", "")
	do(t, http.MethodPost, srv.URL+"/_fake/reset", "")
	b, err := os.ReadFile(path)
	if err != nil || len(bytes.TrimSpace(b)) != 0 || len(listed(t, srv)) != 0 {
		t.Fatalf("record after reset: %q %v", b, err)
	}
}

func TestListenMustBeLoopback(t *testing.T) {
	for addr, ok := range map[string]bool{"127.0.0.1:1": true, "[::1]:1": true, "localhost:1": true, "0.0.0.0:1": false, "10.0.0.5:1": false, "nope": false} {
		if (checkLoopback(addr) == nil) != ok {
			t.Errorf("%s: want ok=%v", addr, ok)
		}
	}
}
