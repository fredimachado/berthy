package cli

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const checksBody = `[{"name":"disk:/","state":"ok","detail":"40.0% used","since":"2026-11-16T10:00:00Z"},{"name":"probe:site","state":"critical","detail":"HTTP 503","since":"2026-11-16T10:05:00Z"}]`

func TestChecksPrintsATableOrJSON(t *testing.T) {
	var seen string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Method + " " + r.URL.Path + " " + r.Header.Get("Authorization")
		fmt.Fprint(w, checksBody)
	}))
	defer server.Close()
	cfg := ingressCLIConfig(t, server.URL)

	var out, errout bytes.Buffer
	if code := Run([]string{"checks", "-e", "dev", "--config", cfg}, &out, &errout); code != 0 {
		t.Fatalf("exit %d: %s", code, errout.String())
	}
	if seen != "GET /v1/projects/demo/environments/dev/checks Bearer test-token" {
		t.Fatalf("request: %q", seen)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[0], "NAME") || !strings.Contains(lines[0], "DETAIL") {
		t.Fatalf("table header:\n%s", out.String())
	}
	if !strings.Contains(lines[2], "probe:site") || !strings.Contains(lines[2], "critical") || !strings.Contains(lines[2], "2026-11-16T10:05:00Z") || !strings.HasSuffix(lines[2], "HTTP 503") {
		t.Fatalf("row: %q", lines[2])
	}

	out.Reset()
	errout.Reset()
	if code := Run([]string{"checks", "-e", "dev", "--config", cfg, "--json"}, &out, &errout); code != 0 {
		t.Fatalf("json exit %d: %s", code, errout.String())
	}
	if !strings.Contains(out.String(), "\n  {\n") || !strings.Contains(out.String(), `"name": "probe:site"`) {
		t.Fatalf("json output:\n%s", out.String())
	}
}

func TestChecksWithoutStatesSaysSo(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `[]`)
	}))
	defer server.Close()
	var out, errout bytes.Buffer
	if code := Run([]string{"checks", "-e", "dev", "--config", ingressCLIConfig(t, server.URL)}, &out, &errout); code != 0 || strings.TrimSpace(out.String()) != "no checks yet" {
		t.Fatalf("exit %d output %q", code, out.String())
	}
}

func TestChecksRefusalExitsNonZero(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"code":"unauthorized","message":"unauthorized"}`)
	}))
	defer server.Close()
	var out, errout bytes.Buffer
	if code := Run([]string{"checks", "-e", "dev", "--config", ingressCLIConfig(t, server.URL)}, &out, &errout); code != 1 || !strings.Contains(errout.String(), "HTTP 401") {
		t.Fatalf("exit %d stderr %q", code, errout.String())
	}
}

func TestAlertsTestSendsAndReportsTheOutcome(t *testing.T) {
	var method, path, auth string
	status := http.StatusOK
	answer := `{"channels":[{"channel":"telegram","ok":true},{"channel":"webhook","ok":true}]}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path, auth = r.Method, r.URL.Path, r.Header.Get("Authorization")
		if status != http.StatusOK {
			w.WriteHeader(status)
			fmt.Fprint(w, `{"code":"alerts_not_configured","message":"alerts are not configured"}`)
			return
		}
		fmt.Fprint(w, answer)
	}))
	defer server.Close()
	cfg := ingressCLIConfig(t, server.URL)

	var out, errout bytes.Buffer
	if code := Run([]string{"alerts", "test", "-e", "dev", "--config", cfg}, &out, &errout); code != 0 {
		t.Fatalf("exit %d: %s", code, errout.String())
	}
	if method != "POST" || path != "/v1/projects/demo/environments/dev/alerts/test" || auth != "Bearer test-token" {
		t.Fatalf("request %s %s %q", method, path, auth)
	}
	if out.String() != "telegram: ok\nwebhook: ok\n" {
		t.Fatalf("output %q", out.String())
	}

	// A channel that failed is named with its reason, and the exit code says so.
	answer = `{"channels":[{"channel":"telegram","ok":true},{"channel":"webhook","ok":false,"error":"webhook returned HTTP 502"}]}`
	out.Reset()
	errout.Reset()
	if code := Run([]string{"alerts", "test", "-e", "dev", "--config", cfg}, &out, &errout); code != 1 || out.String() != "telegram: ok\nwebhook: failed: webhook returned HTTP 502\n" {
		t.Fatalf("one channel down: exit %d output %q stderr %q", code, out.String(), errout.String())
	}
	out.Reset()
	if code := Run([]string{"alerts", "test", "-e", "dev", "--config", cfg, "--json"}, &out, &errout); code != 1 || !strings.Contains(out.String(), `"error": "webhook returned HTTP 502"`) {
		t.Fatalf("json: exit %d output %q", code, out.String())
	}

	status = http.StatusBadRequest
	out.Reset()
	errout.Reset()
	if code := Run([]string{"alerts", "test", "-e", "dev", "--config", cfg}, &out, &errout); code != 1 || strings.TrimSpace(errout.String()) != "agent returned HTTP 400: alerts are not configured" {
		t.Fatalf("refused: exit %d stderr %q", code, errout.String())
	}
}

func TestAlertsCommandValidatesItsArguments(t *testing.T) {
	cfg := ingressCLIConfig(t, "http://127.0.0.1:1")
	for _, tc := range []struct {
		args []string
		want int
	}{
		{[]string{"alerts", "-e", "dev", "--config", cfg}, 2},
		{[]string{"alerts", "send", "-e", "dev", "--config", cfg}, 2},
		{[]string{"alerts", "test", "--dry-run", "-e", "dev", "--config", cfg}, 2},
		{[]string{"alerts", "test", "extra", "-e", "dev", "--config", cfg}, 2},
	} {
		var out, errout bytes.Buffer
		if code := Run(tc.args, &out, &errout); code != tc.want {
			t.Errorf("%v: exit %d, want %d (%s)", tc.args, code, tc.want, errout.String())
		}
	}
}
