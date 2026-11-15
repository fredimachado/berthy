package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// testToken is a bot token of the documented shape, made up for these tests.
const testToken = "123456789:TEST-token-not-real"

func newTestFake(t *testing.T, token string) (*httptest.Server, string) {
	t.Helper()
	statePath := filepath.Join(t.TempDir(), "messages.json")
	f := &fake{token: token, statePath: statePath, logw: io.Discard, now: func() time.Time { return time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC) }}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return srv, statePath
}

func postForm(t *testing.T, target string, form url.Values) *http.Response {
	t.Helper()
	resp, err := http.PostForm(target, form)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func readMessages(t *testing.T, srv *httptest.Server) []message {
	t.Helper()
	resp, err := http.Get(srv.URL + "/_fake/messages")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out []message
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestSendMessageRecordsChatAndTextNeverTheToken(t *testing.T) {
	srv, statePath := newTestFake(t, testToken)
	resp := postForm(t, srv.URL+"/bot"+testToken+"/sendMessage", url.Values{"chat_id": {"-1001234567890"}, "text": {"🔴 lab probe:site: HTTP 503"}, "disable_web_page_preview": {"true"}})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var reply struct {
		OK bool `json:"ok"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&reply); err != nil || !reply.OK {
		t.Fatalf("reply %+v %v", reply, err)
	}
	msgs := readMessages(t, srv)
	if len(msgs) != 1 || msgs[0].ChatID != "-1001234567890" || msgs[0].Text != "🔴 lab probe:site: HTTP 503" {
		t.Fatalf("recorded: %+v", msgs)
	}
	dumped, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(dumped, []byte(testToken)) || !bytes.Contains(dumped, []byte("probe:site")) {
		t.Fatalf("dump: %s", dumped)
	}
}

func TestSendMessageChecksTokenAndFields(t *testing.T) {
	srv, _ := newTestFake(t, testToken)
	if resp := postForm(t, srv.URL+"/bot"+"wrong-token"+"/sendMessage", url.Values{"chat_id": {"1"}, "text": {"x"}}); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong token: %d", resp.StatusCode)
	}
	if resp := postForm(t, srv.URL+"/bot"+testToken+"/sendMessage", url.Values{"chat_id": {"1"}}); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing text: %d", resp.StatusCode)
	}
	if resp, err := http.Get(srv.URL + "/bot" + testToken + "/sendMessage"); err != nil || resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET sendMessage: %v %v", resp, err)
	}
	if len(readMessages(t, srv)) != 0 {
		t.Fatal("refused messages were recorded")
	}
}

func TestAnyTokenIsAcceptedWithoutATokenFile(t *testing.T) {
	srv, _ := newTestFake(t, "")
	if resp := postForm(t, srv.URL+"/bot42:anything/sendMessage", url.Values{"chat_id": {"1"}, "text": {"x"}}); resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestFailModeAnswersAStatusAndRecordsNothing(t *testing.T) {
	srv, _ := newTestFake(t, testToken)
	resp := postForm(t, srv.URL+"/_fake/fail", url.Values{})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty control body: %d", resp.StatusCode)
	}
	control := func(body string) int {
		t.Helper()
		r, err := http.Post(srv.URL+"/_fake/fail", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		return r.StatusCode
	}
	if code := control(`{"status":503}`); code != http.StatusOK {
		t.Fatalf("set failure: %d", code)
	}
	if resp := postForm(t, srv.URL+"/bot"+testToken+"/sendMessage", url.Values{"chat_id": {"1"}, "text": {"x"}}); resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("injected failure: %d", resp.StatusCode)
	}
	if len(readMessages(t, srv)) != 0 {
		t.Fatal("failed send was recorded")
	}
	if code := control(`{"status":0}`); code != http.StatusOK {
		t.Fatalf("clear failure: %d", code)
	}
	if resp := postForm(t, srv.URL+"/bot"+testToken+"/sendMessage", url.Values{"chat_id": {"1"}, "text": {"back"}}); resp.StatusCode != http.StatusOK {
		t.Fatalf("after clearing: %d", resp.StatusCode)
	}
	for _, body := range []string{`{"status":200}`, `{"status":42}`, `{"status":503,"extra":1}`, `not json`} {
		if code := control(body); code != http.StatusBadRequest {
			t.Errorf("%s accepted with %d", body, code)
		}
	}
}

func TestResetClearsTheRecord(t *testing.T) {
	srv, statePath := newTestFake(t, testToken)
	postForm(t, srv.URL+"/bot"+testToken+"/sendMessage", url.Values{"chat_id": {"1"}, "text": {"one"}})
	postForm(t, srv.URL+"/_fake/reset", url.Values{})
	if msgs := readMessages(t, srv); len(msgs) != 0 {
		t.Fatalf("messages after reset: %+v", msgs)
	}
	dumped, err := os.ReadFile(statePath)
	if err != nil || strings.Contains(string(dumped), "one") {
		t.Fatalf("dump after reset: %s %v", dumped, err)
	}
}

func TestControlPlaneAndUnknownRoutesAreNotBotAPI(t *testing.T) {
	srv, _ := newTestFake(t, testToken)
	for _, path := range []string{"/elsewhere", "/bot" + testToken, "/_fake/unknown"} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s: %d", path, resp.StatusCode)
		}
	}
}

func TestCheckLoopbackRejectsPublicAddresses(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:18788", "[::1]:18788", "localhost:18788"} {
		if err := checkLoopback(addr); err != nil {
			t.Errorf("checkLoopback(%q) = %v", addr, err)
		}
	}
	for _, addr := range []string{"0.0.0.0:18788", ":18788", "10.0.0.5:18788", "not-an-address"} {
		if err := checkLoopback(addr); err == nil {
			t.Errorf("checkLoopback(%q) accepted a non-loopback address", addr)
		}
	}
}

func TestReadTokenTrimsAndRejectsEmpty(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "token")
	if err := os.WriteFile(good, []byte(testToken+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if got, err := readToken(good); err != nil || got != testToken {
		t.Fatalf("readToken = %q, %v", got, err)
	}
	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, []byte(" \n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readToken(empty); err == nil {
		t.Fatal("readToken accepted an empty file")
	}
}

func TestRunRefusesInvalidInvocations(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	for _, args := range [][]string{
		{"-listen", "0.0.0.0:18788"},
		{"-listen", "127.0.0.1:18788", "extra"},
		{"-listen", "127.0.0.1:18788", "-token-file", missing},
	} {
		var logw bytes.Buffer
		if code := run(args, &logw); code != 2 {
			t.Errorf("%v: exit %d (%s)", args, code, logw.String())
		}
	}
}
