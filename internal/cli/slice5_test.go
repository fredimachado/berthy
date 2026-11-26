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
	var plain bytes.Buffer
	errs.Reset()
	if code := Run([]string{"config", "show", "-e", "dev", "--manifest-dir", dir}, &plain, &errs); code != 0 {
		t.Fatalf("plain config: %d %s", code, errs.String())
	}
	wantConfig := "apps: \n  web: \n    deploy: \n      deploy_timeout: 120s\n      drain_timeout: 30s\n      retain: 5\n      start_attempts: 3\n    env: \n      plain: \n        status: failed\n    hooks: \n    image: alpine\n    kind: image\n    roles: \n      web: \n        replicas: 1\nnetwork: demo\nproject: demo\nprotected: false\n"
	if plain.String() != wantConfig {
		t.Errorf("plain config = %q, want %q", plain.String(), wantConfig)
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
	plain.Reset()
	errs.Reset()
	watchOperation(context.Background(), client, base, "token", "/v1/", "op", "/v1/operations/op/events", &plain, &errs, false, "restart", "dev", "demo")
	if want := "nested: \n  state: failed\nreason: bad\\x1b\nstate: failed\n"; plain.String() != want {
		t.Errorf("plain operation = %q, want %q", plain.String(), want)
	}

}

func TestDiffStylesStructuredMarkers(t *testing.T) {
	var out, plain, errs bytes.Buffer
	slice5Destination(t, &out)
	data := []byte(`{"changes":[{"area":"a\u001b","change":"- old"},{"area":"env","change":"x + value"},{"area":"env","change":"+ added"},{"area":"web","change":"replicas 2 -> 1"},{"area":"env","change":"~ X -> + value"}]}`)
	if printDiff(&out, &errs, data) != 0 || printDiff(&plain, &errs, data) != 0 {
		t.Fatal(errs.String())
	}
	if want := "a\\x1b       - old\nenv      x + value\nenv      + added\nweb      replicas 2 -> 1\nenv      ~ X -> + value\n"; plain.String() != want {
		t.Errorf("plain diff = %q, want %q", plain.String(), want)
	}
	for _, want := range []string{"\x1b[31m- \x1b[0mold", "\x1b[32m+ \x1b[0madded", "\x1b[33m~ \x1b[0mX -> + value", "a\\x1b", "replicas 2 -> 1"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q in %q", want, out.String())
		}
	}
	if strings.Contains(out.String(), "x \x1b[32m+") {
		t.Errorf("marker inside value styled: %q", out.String())
	}
	for _, tc := range []struct{ data, want string }{{`{"new":true}`, "new app\n"}, {`{"same":true}`, "no changes: the same release would be recorded\n"}} {
		out.Reset()
		if printDiff(&out, &errs, []byte(tc.data)) != 0 || out.String() != tc.want {
			t.Errorf("special diff = %q, want %q", out.String(), tc.want)
		}
	}
	plain.Reset()
	if printDryRun(&plain, &errs, []byte(`{"app":"api","diff":"invalid","status":"failed"}`)) != 0 {
		t.Fatal(errs.String())
	}
	if want := "app: api\ndiff: invalid\nstatus: failed\n"; plain.String() != want {
		t.Errorf("malformed fallback = %q, want %q", plain.String(), want)
	}
}

func TestKnownRecordEntryStyles(t *testing.T) {
	var out, plain, errs bytes.Buffer
	slice5Destination(t, &out)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/history/api"):
			io.WriteString(w, `[{"app":"api","kind":"deploy","operation_id":"op1","state":"failed","reason":"bad","extra":{"state":"failed"}}]`)
		case strings.HasSuffix(r.URL.Path, "/lock"):
			io.WriteString(w, `{"locked":true,"holder":"operator","reason":"freeze","extra":{"locked":true}}`)
		case strings.HasSuffix(r.URL.Path, "/secrets"):
			io.WriteString(w, `[{"name":"API_KEY","registry":"registry.example"}]`)
		default:
			io.WriteString(w, `{}`)
		}
	}))
	defer server.Close()
	cfg := ingressCLIConfig(t, server.URL)
	dir := apiManifestDir(t)
	for _, tc := range []struct {
		name       string
		args       []string
		span, want string
	}{
		{"history", []string{"history", "api"}, "\x1b[31mfailed\x1b[0m", "app: api\nextra: \n  state: failed\nkind: deploy\noperation_id: op1\nreason: bad\nstate: failed\n\n"},
		{"lock", []string{"lock", "status"}, "\x1b[33mtrue\x1b[0m", "extra: \n  locked: true\nholder: operator\nlocked: true\nreason: freeze\n"},
		{"secrets", []string{"secret", "list"}, "\x1b[35mAPI_KEY\x1b[0m", "name: API_KEY\nregistry: registry.example\n\n"},
		{"manifest secrets", []string{"secret", "list", "--manifest-dir", dir}, "\x1b[35mAPI_KEY\x1b[0m", "name: API_KEY\nregistry: registry.example\n\nevery secret the manifest needs is stored\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out.Reset()
			plain.Reset()
			errs.Reset()
			args := append(append([]string{}, tc.args...), "-e", "dev", "--config", cfg)
			if code := Run(args, &out, &errs); code != 0 {
				t.Fatalf("exit %d: %s", code, errs.String())
			}
			if !strings.Contains(out.String(), tc.span) {
				t.Errorf("missing %q in %q", tc.span, out.String())
			}
			if tc.name == "history" && strings.Count(out.String(), "\x1b[31mfailed\x1b[0m") != 1 {
				t.Errorf("nested value styled: %q", out.String())
			}
			if tc.name == "lock" && strings.Count(out.String(), "\x1b[33mtrue\x1b[0m") != 1 {
				t.Errorf("nested value styled: %q", out.String())
			}
			if code := Run(args, &plain, &errs); code != 0 || plain.String() != tc.want {
				t.Errorf("plain exit %d = %q, want %q; %s", code, plain.String(), tc.want, errs.String())
			}
			out.Reset()
			plain.Reset()
			jsonArgs := append(append([]string{}, args...), "--json")
			if a, b := Run(jsonArgs, &out, &errs), Run(jsonArgs, &plain, &errs); a != 0 || b != 0 || out.String() != plain.String() || strings.Contains(out.String(), "\x1b") {
				t.Errorf("json outputs %q / %q (exits %d / %d): %s", out.String(), plain.String(), a, b, errs.String())
			}
		})
	}
	out.Reset()
	errs.Reset()
	if printHistory(&out, &errs, []byte(`{"state":"failed","name":"API_KEY"}`)) != 0 || strings.Contains(out.String(), "\x1b[31mfailed") || strings.Contains(out.String(), "\x1b[35mAPI_KEY") {
		t.Errorf("history fallback: %q", out.String())
	}

	// An unrecognized row in a collection must not acquire a known shape.
	out.Reset()
	errs.Reset()
	if printHistory(&out, &errs, []byte(`[{"state":"failed","other":"unknown"}]`)) != 0 || strings.Contains(out.String(), "\x1b[31mfailed") {
		t.Errorf("unknown history row: %q", out.String())
	}
	unknown := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `[{"name":"API_KEY","status":"failed"}]`)
	}))
	defer unknown.Close()
	unknownConfig := ingressCLIConfig(t, unknown.URL)
	out.Reset()
	errs.Reset()
	if code := Run([]string{"secret", "list", "-e", "dev", "--config", unknownConfig}, &out, &errs); code != 0 || strings.Contains(out.String(), "\x1b[35mAPI_KEY") || strings.Contains(out.String(), "\x1b[31mfailed") {
		t.Errorf("unknown secret row: %d %q %q", code, out.String(), errs.String())
	}
	free := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `{"locked":false}`) }))
	defer free.Close()
	freeConfig := ingressCLIConfig(t, free.URL)
	out.Reset()
	errs.Reset()
	if code := Run([]string{"lock", "status", "-e", "dev", "--config", freeConfig}, &out, &errs); code != 0 || !strings.Contains(out.String(), "\x1b[32mfalse\x1b[0m") {
		t.Errorf("free lock: %d %q %q", code, out.String(), errs.String())
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
