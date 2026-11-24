package cli

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// rotateAgent answers `key rotate` with status and body, and keeps what it was sent.
type rotateAgent struct {
	status int
	body   string
	got    []string
}

func (a *rotateAgent) serve(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		a.got = append(a.got, r.Method+" "+r.URL.Path+" "+string(data))
		w.WriteHeader(a.status)
		fmt.Fprint(w, a.body)
	}))
}

func runKeyRotate(t *testing.T, a *rotateAgent, args ...string) (code int, out, errout string) {
	t.Helper()
	server := a.serve(t)
	defer server.Close()
	var stdout, stderr bytes.Buffer
	argv := append([]string{"key", "rotate", "-e", "dev", "--config", ingressCLIConfig(t, server.URL)}, args...)
	code = Run(argv, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestKeyRotatePrintsTheNewKeyIDAndHowToCopyTheKeyButNeverTheKey(t *testing.T) {
	a := &rotateAgent{status: 200, body: `{"old_key_id":"91c0aa11bb22cc33","new_key_id":"3f2a9c01e4b7d210","values":12,"key_file":"/var/lib/berthy/key"}`}
	code, out, errout := runKeyRotate(t, a)
	if code != 0 || errout != "" {
		t.Fatalf("exit %d out %q err %q", code, out, errout)
	}
	if len(a.got) != 1 || a.got[0] != `POST /v1/projects/demo/environments/dev/key/rotate {"dry_run":false}` {
		t.Fatalf("request: %v", a.got)
	}
	for _, want := range []string{
		"rotated the state key: 12 values resealed, key 3f2a9c01e4b7d210 replaces 91c0aa11bb22cc33",
		"the previous key is kept on the server until the next state snapshot succeeds",
		"> ~/demo.dev.key.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestKeyRotateDryRunSaysItChangedNothing(t *testing.T) {
	a := &rotateAgent{status: 200, body: `{"old_key_id":"91c0aa11bb22cc33","values":12,"key_file":"/var/lib/berthy/key","dry_run":true}`}
	code, out, errout := runKeyRotate(t, a, "--dry-run")
	if code != 0 || errout != "" {
		t.Fatalf("exit %d out %q err %q", code, out, errout)
	}
	if len(a.got) != 1 || !strings.HasSuffix(a.got[0], `{"dry_run":true}`) {
		t.Fatalf("request: %v", a.got)
	}
	if !strings.Contains(out, "dry run: a rotation would seal 12 values again with a new key; nothing changed") || strings.Contains(out, "copy the new key") {
		t.Fatalf("output:\n%s", out)
	}
}

func TestKeyRotateJSONPrintsTheAnswer(t *testing.T) {
	body := `{"old_key_id":"91c0aa11bb22cc33","new_key_id":"3f2a9c01e4b7d210","values":12,"key_file":"/var/lib/berthy/key"}`
	code, out, _ := runKeyRotate(t, &rotateAgent{status: 200, body: body}, "--json")
	if code != 0 || !strings.Contains(out, `"new_key_id": "3f2a9c01e4b7d210"`) {
		t.Fatalf("exit %d out %q", code, out)
	}
}

func TestKeyRotateReportsARefusal(t *testing.T) {
	a := &rotateAgent{status: 409, body: `{"code":"conflict","message":"the state key cannot be rotated while an operation runs or the deploy lock is held"}`}
	code, out, errout := runKeyRotate(t, a)
	if code != 1 || out != "" || !strings.Contains(errout, "the state key cannot be rotated while an operation runs or the deploy lock is held") {
		t.Fatalf("exit %d out %q err %q", code, out, errout)
	}
}

func TestKeyNeedsRotate(t *testing.T) {
	for _, args := range [][]string{{"key"}, {"key", "show"}, {"key", "rotate", "extra"}} {
		var out, errout bytes.Buffer
		if code := Run(append(args, "-e", "dev"), &out, &errout); code != 2 || !strings.Contains(errout.String(), "usage: key rotate") {
			t.Errorf("%v: exit %d %q", args, code, errout.String())
		}
	}
}

func TestKeyRotateShowsTimestampedPosixKeyCopy(t *testing.T) {
	old := keyCopyNow
	keyCopyNow = func() time.Time { return time.Date(2026, 11, 22, 15, 30, 12, 0, time.UTC) }
	t.Cleanup(func() { keyCopyNow = old })
	for _, tc := range []struct{ path, quoted string }{{"/srv/key", "/srv/key"}, {"/srv/keys/my key", "'/srv/keys/my key'"}} {
		a := &rotateAgent{status: 200, body: `{"old_key_id":"old","new_key_id":"new","key_file":"` + tc.path + `"}`}
		code, out, errout := runKeyRotate(t, a)
		want := `or, on the server, in a POSIX shell, copy it by hand and then move the file to another machine: (umask 077; set -C; sudo cat ` + tc.quoted + ` > ~/demo.dev.key.20261122T153012Z)`
		if code != 0 || !strings.Contains(out, want) || !strings.Contains(out, "run berthy setup for this environment again") || strings.Contains(out, "Windows PowerShell:") {
			t.Fatalf("exit %d: %s %s", code, out, errout)
		}
	}
}
