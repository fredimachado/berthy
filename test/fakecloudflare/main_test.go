package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"berthy/internal/cloudflare/cftest"
)

func TestCheckLoopbackAcceptsOnlyLoopbackAddresses(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:18787", "[::1]:18787", "localhost:18787"} {
		if err := checkLoopback(addr); err != nil {
			t.Errorf("checkLoopback(%q) = %v, want nil", addr, err)
		}
	}
	for _, addr := range []string{"0.0.0.0:18787", ":18787", "10.0.0.5:18787", "192.0.2.1:18787", "not-an-address"} {
		if err := checkLoopback(addr); err == nil {
			t.Errorf("checkLoopback(%q) accepted a non-loopback address", addr)
		}
	}
}

func TestReadTokenTrimsAndRejectsEmpty(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "token")
	if err := os.WriteFile(good, []byte("abc123\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := readToken(good); err != nil || got != "abc123" {
		t.Fatalf("readToken = %q, %v", got, err)
	}
	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, []byte(" \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readToken(empty); err == nil {
		t.Fatal("readToken accepted an empty file")
	}
	if _, err := readToken(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("readToken accepted a missing file")
	}
}

func TestRunRefusesInvalidInvocations(t *testing.T) {
	dir := t.TempDir()
	token := filepath.Join(dir, "token")
	if err := os.WriteFile(token, []byte("tok"), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := map[string][]string{
		"missing account": {"-token-file", token, "-zones", "example.com"},
		"missing token":   {"-account", "acct", "-zones", "example.com"},
		"missing zones":   {"-account", "acct", "-token-file", token},
		"public listen":   {"-listen", "0.0.0.0:0", "-account", "acct", "-token-file", token, "-zones", "example.com"},
		"extra argument":  {"-account", "acct", "-token-file", token, "-zones", "example.com", "stray"},
	}
	for name, args := range cases {
		var out bytes.Buffer
		if code := run(args, &out); code != 2 {
			t.Errorf("%s: exit %d, want 2 (output %q)", name, code, out.String())
		}
	}
}

// newTestServer serves newMux over httptest, with the state dump at statePath.
func newTestServer(t *testing.T, statePath string) (*httptest.Server, *cftest.Fake) {
	t.Helper()
	f := cftest.New("api-token", "acct")
	f.AddZone("example.com")
	srv := httptest.NewServer(newMux(f, statePath, io.Discard))
	t.Cleanup(srv.Close)
	return srv, f
}

func post(t *testing.T, url, body string) *http.Response {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestControlPlaneSeedsInspectsAndInjectsFaults(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	srv, _ := newTestServer(t, statePath)

	resp := post(t, srv.URL+"/_fake/tunnels", `{"name":"legacy","remote":true,"config":{"ingress":[{"hostname":"legacy.example.net","service":"http://legacy:80"},{"service":"http_status:404"}]}}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("seed tunnel: HTTP %d", resp.StatusCode)
	}
	var seeded struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&seeded); err != nil || seeded.ID == "" {
		t.Fatalf("seed tunnel response: %v", err)
	}

	// The state dump is rewritten before the response is sent, so the file already holds the new tunnel.
	dumped, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(dumped, []byte(`"name": "legacy"`)) {
		t.Fatalf("state dump after seeding lacks the tunnel:\n%s", dumped)
	}

	if resp := post(t, srv.URL+"/_fake/records", `{"zone":"example.com","record":{"type":"TXT","name":"txt.example.com","content":"v=spf1 -all"}}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("seed record: HTTP %d", resp.StatusCode)
	}
	if resp := post(t, srv.URL+"/_fake/records", `{"zone":"missing.test","record":{"type":"A","name":"x.missing.test","content":"192.0.2.1"}}`); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("record in an unknown zone: HTTP %d, want 404", resp.StatusCode)
	}
	if resp := post(t, srv.URL+"/_fake/status", `{"tunnel":"`+seeded.ID+`","status":"healthy","connections":2}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("set status: HTTP %d", resp.StatusCode)
	}
	if resp := post(t, srv.URL+"/_fake/status", `{"tunnel":"`+seeded.ID+`","status":"sunny","connections":2}`); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown status: HTTP %d, want 400", resp.StatusCode)
	}
	if resp := post(t, srv.URL+"/_fake/tunnels", `{"name":"x","bogus":true}`); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown field: HTTP %d, want 400", resp.StatusCode)
	}

	state, err := http.Get(srv.URL + "/_fake/state")
	if err != nil {
		t.Fatal(err)
	}
	defer state.Body.Close()
	var snap struct {
		Tunnels []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"tunnels"`
		Records map[string][]struct {
			Type string `json:"type"`
		} `json:"records"`
		Writes []struct {
			Kind string `json:"kind"`
		} `json:"writes"`
	}
	if err := json.NewDecoder(state.Body).Decode(&snap); err != nil {
		t.Fatal(err)
	}
	if len(snap.Tunnels) != 1 || snap.Tunnels[0].ID != seeded.ID || snap.Tunnels[0].Status != "healthy" {
		t.Fatalf("tunnels = %+v", snap.Tunnels)
	}
	if got := snap.Records["example.com"]; len(got) != 1 || got[0].Type != "TXT" {
		t.Fatalf("records = %+v", got)
	}
	if len(snap.Writes) != 0 {
		t.Fatalf("seeding produced API writes: %+v", snap.Writes)
	}

	// An injected fault refuses the API write, and clearing it lets the next write through.
	if resp := post(t, srv.URL+"/_fake/fail", `{"method":"PUT","prefix":"/accounts/","status":500}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("inject fault: HTTP %d", resp.StatusCode)
	}
	cfg := `{"config":{"ingress":[{"service":"http_status:404"}]}}`
	req, err := http.NewRequest(http.MethodPut, srv.URL+"/client/v4/accounts/acct/cfd_tunnel/"+seeded.ID+"/configurations", strings.NewReader(cfg))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer api-token")
	req.Header.Set("Content-Type", "application/json")
	put, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	put.Body.Close()
	if put.StatusCode != http.StatusInternalServerError {
		t.Fatalf("PUT under the injected fault: HTTP %d, want 500", put.StatusCode)
	}
	clearReq, err := http.NewRequest(http.MethodDelete, srv.URL+"/_fake/fail", nil)
	if err != nil {
		t.Fatal(err)
	}
	cleared, err := http.DefaultClient.Do(clearReq)
	if err != nil {
		t.Fatal(err)
	}
	cleared.Body.Close()
	if cleared.StatusCode != http.StatusOK {
		t.Fatalf("clear faults: HTTP %d", cleared.StatusCode)
	}
	req, err = http.NewRequest(http.MethodPut, srv.URL+"/client/v4/accounts/acct/cfd_tunnel/"+seeded.ID+"/configurations", strings.NewReader(cfg))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer api-token")
	req.Header.Set("Content-Type", "application/json")
	put, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	put.Body.Close()
	if put.StatusCode != http.StatusOK {
		t.Fatalf("PUT after clearing: HTTP %d, want 200", put.StatusCode)
	}
	dumped, err = os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(dumped), "api-token") {
		t.Fatal("state dump holds the API token")
	}
	if !bytes.Contains(dumped, []byte(`"kind": "injected_failure"`)) || !bytes.Contains(dumped, []byte(`"kind": "config_put"`)) {
		t.Fatalf("state dump lacks the refused and the accepted write:\n%s", dumped)
	}
}

func TestAPIIsServedUnderTheAgentsBasePath(t *testing.T) {
	srv, _ := newTestServer(t, "")
	req, err := http.NewRequest(http.MethodGet, srv.URL+"/client/v4/user/tokens/verify", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer api-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("verify: HTTP %d", resp.StatusCode)
	}
	var out struct {
		Success bool `json:"success"`
		Result  struct {
			Status string `json:"status"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || !out.Success || out.Result.Status != "active" {
		t.Fatalf("verify body: %+v, %v", out, err)
	}
	bad, err := http.NewRequest(http.MethodGet, srv.URL+"/client/v4/user/tokens/verify", nil)
	if err != nil {
		t.Fatal(err)
	}
	bad.Header.Set("Authorization", "Bearer wrong")
	wrong, err := http.DefaultClient.Do(bad)
	if err != nil {
		t.Fatal(err)
	}
	wrong.Body.Close()
	if wrong.StatusCode != http.StatusForbidden {
		t.Fatalf("wrong token: HTTP %d, want 403", wrong.StatusCode)
	}
}
