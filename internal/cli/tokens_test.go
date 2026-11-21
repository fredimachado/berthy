package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestParseExpires(t *testing.T) {
	now := time.Date(2026, 11, 18, 12, 30, 0, 0, time.UTC)
	for in, want := range map[string]time.Time{
		"90d":        now.Add(90 * 24 * time.Hour),
		"1d":         now.Add(24 * time.Hour),
		"36h":        now.Add(36 * time.Hour),
		"90m":        now.Add(90 * time.Minute),
		"2027-02-16": time.Date(2027, 2, 16, 0, 0, 0, 0, time.UTC),
	} {
		got, err := parseExpires(in, now)
		if err != nil || !got.Equal(want) {
			t.Errorf("%q: %v %v, want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"", "0d", "-1d", "d", "1.5d", "abc", "0h", "-2h", "2026-11-18", "2026-02-30", "2025-01-01", "2027-2-3", "10d5h"} {
		if got, err := parseExpires(in, now); err == nil {
			t.Errorf("%q was accepted: %v", in, got)
		}
	}
}

// tokenAgent is an agent that answers the tokens routes with canned bodies and keeps what it was asked.
type tokenAgent struct {
	mu       sync.Mutex
	requests []string
	bodies   []string
	status   int
	answer   string
}

func (a *tokenAgent) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		a.mu.Lock()
		a.requests = append(a.requests, r.Method+" "+r.URL.Path)
		a.bodies = append(a.bodies, string(body))
		a.mu.Unlock()
		if a.status != 0 {
			w.WriteHeader(a.status)
		}
		fmt.Fprint(w, a.answer)
	})
}

func runToken(t *testing.T, a *tokenAgent, args ...string) (int, string, string) {
	t.Helper()
	server := httptest.NewServer(a.handler())
	defer server.Close()
	cfg := ingressCLIConfig(t, server.URL)
	var out, errout bytes.Buffer
	code := Run(append([]string{"token"}, append(args, "-e", "dev", "--config", cfg)...), &out, &errout)
	return code, out.String(), errout.String()
}

func TestTokenCreateSendsTheRequestAndShowsTheTokenOnce(t *testing.T) {
	tokensNow = func() time.Time { return time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC) }
	t.Cleanup(func() { tokensNow = time.Now })
	value := "bty.dev." + strings.Repeat("a", 64)
	a := &tokenAgent{answer: `{"name":"ci","scope":"deploy","agent":false,"expires":"2027-02-16T00:00:00Z","token":"` + value + `"}`}
	code, out, errout := runToken(t, a, "create", "ci", "--scope", "deploy", "--expires", "2027-02-16")
	if code != 0 || errout != "" {
		t.Fatalf("exit %d: %s %s", code, out, errout)
	}
	if out != "token ci (deploy, expires 2027-02-16), shown once, store it now: "+value+"\n" {
		t.Fatalf("output: %q", out)
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte(a.bodies[0]), &sent); err != nil || a.requests[0] != "POST /v1/projects/demo/environments/dev/tokens" {
		t.Fatalf("request: %v %s", a.requests, a.bodies[0])
	}
	if sent["name"] != "ci" || sent["scope"] != "deploy" || sent["agent"] != false || sent["expires"] != "2027-02-16T00:00:00Z" || sent["dry_run"] != false {
		t.Fatalf("body: %v", sent)
	}
}

func TestTokenCreateDefaultsToReadAndSaysAgent(t *testing.T) {
	a := &tokenAgent{answer: `{"name":"claude","scope":"read","agent":true,"token":"bty.dev.x"}`}
	code, out, _ := runToken(t, a, "create", "claude", "--agent")
	if code != 0 || out != "token claude (read, agent), shown once, store it now: bty.dev.x\n" {
		t.Fatalf("exit %d: %q", code, out)
	}
	var sent map[string]any
	_ = json.Unmarshal([]byte(a.bodies[0]), &sent)
	if sent["scope"] != "read" || sent["agent"] != true {
		t.Fatalf("body: %v", sent)
	}
	if _, has := sent["expires"]; has {
		t.Fatalf("an expiry was sent: %v", sent)
	}
}

func TestTokenCreateJSONPrintsTheAnswer(t *testing.T) {
	a := &tokenAgent{answer: `{"name":"ci","scope":"deploy","agent":false,"token":"bty.dev.zz"}`}
	code, out, _ := runToken(t, a, "create", "ci", "--scope", "deploy", "--json")
	var got struct{ Token string }
	if code != 0 || json.Unmarshal([]byte(out), &got) != nil || got.Token != "bty.dev.zz" {
		t.Fatalf("exit %d: %q", code, out)
	}
}

func TestTokenDryRunsSayWhatWouldHappen(t *testing.T) {
	a := &tokenAgent{answer: `{"action":"token create","name":"ci","scope":"deploy","agent":false}`}
	code, out, _ := runToken(t, a, "create", "ci", "--scope", "deploy", "--dry-run")
	if code != 0 || out != "would create token ci (deploy)\n" || !strings.Contains(a.bodies[0], `"dry_run":true`) {
		t.Fatalf("create: %d %q %s", code, out, a.bodies[0])
	}
	a = &tokenAgent{answer: `{"action":"token revoke","name":"ci","scope":"deploy","agent":false}`}
	code, out, _ = runToken(t, a, "revoke", "ci", "--dry-run")
	if code != 0 || out != "would revoke token ci (deploy)\n" || a.requests[0] != "DELETE /v1/projects/demo/environments/dev/tokens/ci" || !strings.Contains(a.bodies[0], `"dry_run":true`) {
		t.Fatalf("revoke: %d %q %v %s", code, out, a.requests, a.bodies[0])
	}
}

func TestTokenRevokeSaysSo(t *testing.T) {
	a := &tokenAgent{answer: `{"ok":true,"name":"ci"}`}
	code, out, _ := runToken(t, a, "revoke", "ci")
	if code != 0 || out != "revoked token ci\n" || a.requests[0] != "DELETE /v1/projects/demo/environments/dev/tokens/ci" || !strings.Contains(a.bodies[0], `"dry_run":false`) {
		t.Fatalf("%d %q %v %s", code, out, a.requests, a.bodies[0])
	}
}

func TestTokenListPrintsATable(t *testing.T) {
	tokensNow = func() time.Time { return time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC) }
	t.Cleanup(func() { tokensNow = time.Now })
	a := &tokenAgent{answer: `[
 {"name":"admin","scope":"admin","agent":false,"origin":"config","last_used":"2026-11-18T11:58:00Z"},
 {"name":"ci","scope":"deploy","agent":false,"origin":"api","created":"2026-11-17T09:00:00Z","expires":"2027-02-16T00:00:00Z","last_used":"2026-11-18T11:00:00Z"},
 {"name":"claude","scope":"deploy","agent":true,"origin":"api","created":"2026-11-10T09:00:00Z","expires":"2026-11-15T00:00:00Z"}]`}
	code, out, _ := runToken(t, a, "list")
	if code != 0 || a.requests[0] != "GET /v1/projects/demo/environments/dev/tokens" {
		t.Fatalf("%d %q %v", code, out, a.requests)
	}
	want := []string{
		"NAME    SCOPE   AGENT  ORIGIN  CREATED     EXPIRES                 LAST USED",
		"admin   admin   no     config  -           never                   2m ago",
		"ci      deploy  no     api     2026-11-17  2027-02-16              1h ago",
		"claude  deploy  yes    api     2026-11-10  2026-11-15 (expired)    never",
	}
	got := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(got) != len(want) {
		t.Fatalf("output:\n%s", out)
	}
	for i := range want {
		if strings.Join(strings.Fields(got[i]), " ") != strings.Join(strings.Fields(want[i]), " ") {
			t.Errorf("line %d:\n got %q\nwant %q", i, got[i], want[i])
		}
	}
	code, out, _ = runToken(t, &tokenAgent{answer: `[]`}, "list")
	if code != 0 || out != "no tokens\n" {
		t.Fatalf("empty: %d %q", code, out)
	}
	code, out, _ = runToken(t, &tokenAgent{answer: `[{"name":"ci"}]`}, "list", "--json")
	if code != 0 || !strings.Contains(out, `"name": "ci"`) {
		t.Fatalf("json: %d %q", code, out)
	}
}

func TestTokenRefusalsAreShownAndFail(t *testing.T) {
	a := &tokenAgent{status: 403, answer: `{"code":"forbidden","message":"an agent token cannot manage tokens"}`}
	code, out, errout := runToken(t, a, "create", "x", "--scope", "read")
	if code != 1 || out != "" || !strings.Contains(errout, "an agent token cannot manage tokens") {
		t.Fatalf("%d %q %q", code, out, errout)
	}
	a = &tokenAgent{status: 401, answer: `{"code":"unauthorized","message":"unauthorized"}`}
	if code, _, errout = runToken(t, a, "list"); code != 1 || !strings.Contains(errout, "HTTP 401") {
		t.Fatalf("%d %q", code, errout)
	}
}

func TestTokenUsageErrorsSendNothing(t *testing.T) {
	for _, args := range [][]string{
		{},
		{"create"},
		{"create", "a", "b"},
		{"create", "bad name"},
		{"create", "ci", "--scope", "root"},
		{"create", "ci", "--expires", "soon"},
		{"create", "ci", "--expires", "2020-01-01"},
		{"revoke"},
		{"revoke", "a", "b"},
		{"list", "extra"},
		{"list", "--scope", "read"},
		{"revoke", "ci", "--agent"},
		{"revoke", "ci", "--expires", "1d"},
		{"list", "--dry-run"},
		{"frobnicate"},
	} {
		a := &tokenAgent{answer: `{}`}
		code, out, _ := runToken(t, a, args...)
		if code != 2 || out != "" || len(a.requests) != 0 {
			t.Errorf("%v: exit %d, output %q, requests %v", args, code, out, a.requests)
		}
	}
}

func TestOtherCommandsRefuseTheTokenFlags(t *testing.T) {
	a := &tokenAgent{answer: `{}`}
	server := httptest.NewServer(a.handler())
	defer server.Close()
	cfg := ingressCLIConfig(t, server.URL)
	for _, flag := range [][]string{{"--scope", "read"}, {"--expires", "1d"}, {"--agent"}} {
		var out, errout bytes.Buffer
		if code := Run(append([]string{"restart", "api", "-e", "dev", "--config", cfg}, flag...), &out, &errout); code != 2 || len(a.requests) != 0 {
			t.Errorf("%v: exit %d, %q, requests %v", flag, code, errout.String(), a.requests)
		}
	}
}
