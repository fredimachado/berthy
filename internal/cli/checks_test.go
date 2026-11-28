package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

const checksBody = `[{"name":"disk:/","state":"ok","detail":"40.0% used","since":"2026-11-16T10:00:00Z"},{"name":"probe:site","state":"critical","detail":"HTTP 503","since":"2026-11-16T10:05:00Z"}]`

const clusterChecksBody = `[{"server":"hills\u001b[31m","name":"public_ports","state":"warning","detail":"published port","since":"2026-11-16T10:00:00Z"},{"server":"","name":"telegram","state":"ok","detail":"bot token secret present","since":"2026-11-16T10:05:00Z"}]`

func TestChecksClusterUsesSelectedEnvironmentPrefix(t *testing.T) {
	requests := make(chan string, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case requests <- r.Method + " " + r.URL.RequestURI() + " " + r.Header.Get("Authorization"):
		default:
			t.Error("unexpected additional request")
		}
		fmt.Fprint(w, `[]`)
	}))
	defer server.Close()
	t.Setenv("CLUSTER_ALPHA_TOKEN", "alpha-token")
	t.Setenv("CLUSTER_BRAVO_TOKEN", "bravo-token")
	config := filepath.Join(t.TempDir(), "cli.json")
	if err := os.WriteFile(config, fmt.Appendf(nil, `{"projects":{
		"alpha":{"environments":{"dev":{"url":%q,"token_env":"CLUSTER_ALPHA_TOKEN"}}},
		"bravo":{"environments":{"dev":{"url":%q,"token_env":"CLUSTER_BRAVO_TOKEN"}}}
	}}`, server.URL+"/proxy/team", server.URL), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		project, want string
		cluster       bool
	}{
		{"alpha", "GET /proxy/team/v1/projects/alpha/environments/dev/cluster/checks Bearer alpha-token", true},
		{"bravo", "GET /v1/projects/bravo/environments/dev/cluster/checks Bearer bravo-token", true},
		{"alpha", "GET /proxy/team/v1/projects/alpha/environments/dev/checks Bearer alpha-token", false},
		{"bravo", "GET /v1/projects/bravo/environments/dev/checks Bearer bravo-token", false},
	} {
		args := []string{"checks", "-e", "dev", "--project", tc.project, "--config", config, "--json"}
		if tc.cluster {
			args = append(args, "--cluster")
		}
		var out, diag bytes.Buffer
		if code := Run(args, &out, &diag); code != 0 || out.String() != "[]\n" {
			t.Errorf("%s (cluster=%v): exit %d, stdout %q, stderr %q", tc.project, tc.cluster, code, out.String(), diag.String())
			continue
		}
		select {
		case got := <-requests:
			if got != tc.want {
				t.Errorf("request = %q, want %q", got, tc.want)
			}
		default:
			t.Error("checks made no request")
		}
	}
}

func TestChecksClusterPrintsServerAndPreservesJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/cluster/checks") {
			fmt.Fprint(w, clusterChecksBody)
			return
		}
		fmt.Fprint(w, checksBody)
	}))
	defer server.Close()
	args := []string{"checks", "-e", "dev", "--project", "demo", "--config", ingressCLIConfig(t, server.URL)}
	run := func(flags ...string) string {
		t.Helper()
		var out, diag bytes.Buffer
		if code := Run(append(args, flags...), &out, &diag); code != 0 {
			t.Errorf("checks %v: exit %d, stderr %s", flags, code, diag.String())
		}
		return out.String()
	}
	ordinary := strings.Split(strings.TrimSuffix(run(), "\n"), "\n")
	if strings.Join(strings.Fields(ordinary[0]), " ") != "NAME STATE SINCE DETAIL" || len(ordinary) != 3 || !strings.HasPrefix(ordinary[1], "disk:/") {
		t.Errorf("ordinary table = %q", ordinary)
	}
	cluster := strings.Split(strings.TrimSuffix(run("--cluster"), "\n"), "\n")
	nameCol := strings.Index(cluster[0], "NAME")
	if strings.Join(strings.Fields(cluster[0]), " ") != "SERVER NAME STATE SINCE DETAIL" || len(cluster) != 3 ||
		nameCol < 0 || len(cluster[1]) < nameCol || len(cluster[2]) < nameCol ||
		!strings.HasPrefix(cluster[1], `hills\x1b[31m`) || !strings.HasPrefix(cluster[1][nameCol:], "public_ports") ||
		!strings.HasPrefix(cluster[2][nameCol:], "telegram") || strings.TrimSpace(cluster[2][:nameCol]) != "" {
		t.Errorf("cluster table = %q", cluster)
	}
	var got, want []map[string]string
	if err := json.Unmarshal([]byte(clusterChecksBody), &want); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(run("--cluster", "--json")), &got); err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("cluster JSON = %v (%v), want %v", got, err, want)
	}
}

func TestClusterFlagCannotSelectASecretOrTestCommand(t *testing.T) {
	var requests, sends atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if strings.HasSuffix(r.URL.Path, "/alerts/test") {
			sends.Add(1)
		}
		fmt.Fprint(w, `[]`)
	}))
	defer server.Close()
	read := fakeTerminal(t, "unused-secret")
	withStdinFile(t, "unused-secret\n")
	target := []string{"--cluster", "-e", "dev", "--project", "demo", "--config", ingressCLIConfig(t, server.URL)}
	for _, prefix := range [][]string{{"secret", "set", "HOOK"}, {"alerts", "test"}, {"ps"}, {"doctor"}, {"jobs"}} {
		var out, diag bytes.Buffer
		if code := Run(append(prefix, target...), &out, &diag); code != 2 || !strings.Contains(diag.String(), "--cluster") {
			t.Errorf("%v: exit %d, stderr %q", prefix, code, diag.String())
		}
	}
	offset, err := os.Stdin.Seek(0, io.SeekCurrent)
	if err != nil || offset != 0 || read.Load() != 0 || requests.Load() != 0 || sends.Load() != 0 {
		t.Fatalf("refused commands used stdin or HTTP: offset %d (%v), hidden reads %d, HTTP %d, sends %d", offset, err, read.Load(), requests.Load(), sends.Load())
	}
	var out, diag bytes.Buffer
	if code := Run(append([]string{"checks", "--json"}, target...), &out, &diag); code != 0 || out.String() != "[]\n" || requests.Load() != 1 || sends.Load() != 0 {
		t.Errorf("checks positive control: exit %d, stdout %q, stderr %q, HTTP %d, sends %d", code, out.String(), diag.String(), requests.Load(), sends.Load())
	}
}

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

// TestPrintChecksMakesAgentTextTerminalSafe: a check's name, state and detail come from the
// agent and must not rewrite the terminal; a tab in a non-final cell becomes spaces so columns stay under their headers.
func TestPrintChecksMakesAgentTextTerminalSafe(t *testing.T) {
	body := []byte(`[{"name":"probe:\tsite","state":"ok\u001b","detail":"HTTP \u001b[31m503","since":"2026-11-16T10:00:00Z"}]`)
	var out, errout bytes.Buffer
	if code := printChecks(&out, &errout, body, false, false); code != 0 {
		t.Fatalf("exit %d: %s", code, errout.String())
	}
	text := out.String()
	if strings.ContainsAny(text, "\x1b\t") {
		t.Fatalf("raw ESC or tab reached the terminal:\n%q", text)
	}
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], "NAME") || !strings.Contains(lines[0], "DETAIL") {
		t.Fatalf("header:\n%s", text)
	}
	if !strings.Contains(lines[1], "probe:    site") {
		t.Errorf("tab in name not shown as four spaces: %q", lines[1])
	}
	for _, want := range []string{`ok\x1b`, `HTTP \x1b[31m503`, "2026-11-16T10:00:00Z"} {
		if !strings.Contains(lines[1], want) {
			t.Errorf("row lacks %q: %q", want, lines[1])
		}
	}
	stateCol := strings.Index(lines[0], "STATE")
	if stateCol < 0 || stateCol >= len(lines[1]) || !strings.HasPrefix(lines[1][stateCol:], "ok") {
		t.Errorf("STATE cell not under its header (col %d):\nheader %q\nrow    %q", stateCol, lines[0], lines[1])
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
