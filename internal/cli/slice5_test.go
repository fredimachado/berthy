package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"berthy/internal/deploy"
)

func slice5Destination(t *testing.T, dst io.Writer) {
	t.Helper()
	t.Setenv("NO_COLOR", "")
	width, vt := progressWidth, progressVT
	progressWidth = func(w io.Writer) (int, error) {
		if w == dst {
			return 80, nil
		}
		return 0, errors.New("not a terminal")
	}
	progressVT = func(w io.Writer, _ string) bool { return w == dst }
	t.Cleanup(func() { progressWidth, progressVT = width, vt })
}

func TestKeyedTextStylesOnlySemanticFields(t *testing.T) {
	var out, errs bytes.Buffer
	slice5Destination(t, &out)
	server := doctorVersionServer(t, "v1.0.0", true)
	cfg := ingressCLIConfig(t, server.URL)
	if Run([]string{"doctor", "-e", "dev", "--config", cfg}, &out, &errs) != 1 {
		t.Fatalf("doctor: %s", errs.String())
	}
	if !strings.Contains(out.String(), "\x1b[31mfalse\x1b[0m") {
		t.Errorf("critical doctor check not styled: %q", out.String())
	}
	// Severity is derived from both booleans, and nested user maps are not checks.
	out.Reset()
	errs.Reset()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"checks":[{"name":"good","ok":true,"critical":true,"detail":"yes"},{"name":"warn","ok":false,"critical":false,"detail":"no"},{"name":"fatal","ok":false,"critical":true,"detail":"no"}]}`)
	}))
	defer srv.Close()
	cfg2 := ingressCLIConfig(t, srv.URL)
	if Run([]string{"doctor", "-e", "dev", "--config", cfg2}, &out, &errs) != 1 {
		t.Fatal("critical check must fail")
	}
	for _, span := range []string{"\x1b[32mgood\x1b[0m", "\x1b[33mwarn\x1b[0m", "\x1b[31mfatal\x1b[0m"} {
		if !strings.Contains(out.String(), span) {
			t.Errorf("doctor missing %q: %q", span, out.String())
		}
	}
	out.Reset()
	errs.Reset()
	ps := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `[{"roles":[{"replicas":[{"state":"failed","health":"healthy","extra":{"status":"failed"}}]}]}]`)
	}))
	defer ps.Close()
	cfg3 := ingressCLIConfig(t, ps.URL)
	if Run([]string{"ps", "-e", "dev", "--config", cfg3}, &out, &errs) != 0 {
		t.Fatal(errs.String())
	}
	if !strings.Contains(out.String(), "\x1b[31mfailed\x1b[0m") || !strings.Contains(out.String(), "\x1b[32mhealthy\x1b[0m") || strings.Count(out.String(), "\x1b[31mfailed\x1b[0m") != 1 {
		t.Errorf("replicas: %q", out.String())
	}

	// A real manifest remains generic even when an environment variable is named status.
	dir := t.TempDir()
	manifest := "project: demo\nnetwork: demo\napps:\n  web:\n    kind: image\n    image: alpine\n    roles:\n      web: {}\n    env:\n      plain:\n        status: failed\n"
	if err := os.WriteFile(filepath.Join(dir, "berthy.yml"), []byte(manifest), 0600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	errs.Reset()
	if code := Run([]string{"config", "show", "-e", "dev", "--manifest-dir", dir}, &out, &errs); code != 0 {
		t.Fatalf("config show %d: %s", code, errs.String())
	}
	if strings.Contains(out.String(), "\x1b[31mfailed") || !strings.Contains(out.String(), "failed") {
		t.Errorf("manifest: %q", out.String())
	}
	// The watcher owns the operation record; its opaque nested object stays generic.
	out.Reset()
	errs.Reset()
	client := &http.Client{Transport: progressRoundTrip(func(r *http.Request) (*http.Response, error) {
		body := `{"state":"failed","reason":"bad\u001b","nested":{"state":"failed"}}`
		if strings.HasSuffix(r.URL.Path, "/events") {
			body = "event: failed\ndata: {}\n\nevent: terminal\ndata: {}\n\n"
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: r}, nil
	})}
	base, _ := url.Parse("http://agent.invalid")
	watchOperation(context.Background(), client, base, "token", "/v1/", "op", "/v1/operations/op/events", &out, &errs, false, "restart", "dev", "demo")
	if !strings.Contains(out.String(), "\x1b[31mfailed\x1b[0m") || !strings.Contains(out.String(), "\x1b[31mbad\\x1b\x1b[0m") || strings.Count(out.String(), "\x1b[31mfailed\x1b[0m") != 1 {
		t.Errorf("operation: %q errors %q", out.String(), errs.String())
	}

}

func TestDiffStylesStructuredMarkers(t *testing.T) {
	var out, errs bytes.Buffer
	slice5Destination(t, &out)
	data := []byte(`{"changes":[{"area":"env","change":"+ FOO"},{"area":"web","change":"replicas 2 -> 1"},{"area":"env","change":"~ X -> + value"}]}`)
	if printDiff(&out, &errs, data) != 0 {
		t.Fatal(errs.String())
	}
	for _, want := range []string{"\x1b[32m+ \x1b[0mFOO", "\x1b[33m~ \x1b[0mX -> + value", "replicas 2 -> 1"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q in %q", want, out.String())
		}
	}
	var plain bytes.Buffer
	d := deploy.Diff{Changes: []deploy.DiffChange{{Area: "a\x1b", Change: "- old"}, {Area: "env", Change: "x + value"}, {Area: "env", Change: "+ added"}}}
	printStyledDiff(&plain, "  ", d, cliStyle{})
	if got, want := plain.String(), "  a\\x1b       - old\n  env      x + value\n  env      + added\n"; got != want {
		t.Errorf("plain diff = %q, want %q", got, want)
	}
	var styled bytes.Buffer
	printStyledDiff(&styled, "", d, cliStyle{color: true})
	if !strings.Contains(styled.String(), "\x1b[31m- \x1b[0mold") || strings.Contains(styled.String(), "x \x1b[32m+") || !strings.Contains(styled.String(), "a\\x1b") {
		t.Errorf("styled diff: %q", styled.String())
	}
	for _, tc := range []struct {
		d    deploy.Diff
		want string
	}{{deploy.Diff{New: true}, "new app\n"}, {deploy.Diff{Same: true}, "no changes: the same release would be recorded\n"}} {
		styled.Reset()
		printStyledDiff(&styled, "", tc.d, cliStyle{color: true})
		if styled.String() != tc.want {
			t.Errorf("special diff: %q", styled.String())
		}
	}
	malformed := map[string]any{"diff": "invalid"}
	if _, ok := takeDiff(malformed); ok || malformed["diff"] != "invalid" {
		t.Errorf("malformed diff removed: %#v", malformed)
	}

}

func TestBufferedImagePlanUsesDestinationStyle(t *testing.T) {
	var out, pipe bytes.Buffer
	slice5Destination(t, &out)
	data := []byte(`{"app":"api","image":"example/api@sha256:abc","roles":["web"],"diff":{"changes":[{"area":"env","change":"+ FOO"}]}}`)
	if err := showImagePlan(&out, data, "", false, ""); err != nil {
		t.Fatal(err)
	}
	if err := showImagePlan(&pipe, data, "", false, ""); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "\x1b[32m+ \x1b[0mFOO") {
		t.Errorf("destination lacks color: %q", out.String())
	}
	if pipe.String() != "api\n  env      + FOO\nimage: example/api@sha256:abc\nroles: \n  - web\n" {
		t.Errorf("pipe: %q", pipe.String())
	}
}
