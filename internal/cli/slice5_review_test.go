package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"berthy/internal/deploy"
)

func TestSlice5ShapeBoundaries(t *testing.T) {
	var out, pipe, errs bytes.Buffer
	slice5Destination(t, &out)
	body := `{}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) }))
	defer srv.Close()
	cfg := ingressCLIConfig(t, srv.URL)
	for _, tc := range []struct {
		name             string
		args             []string
		data, want, span string
		colored          bool
	}{
		{"tunnel array", []string{"ingress", "status"}, `{"tunnel":[{"status":"failed"}]}`, "tunnel: \n  status: failed\n\n", "\x1b[31mfailed\x1b[0m", false},
		{"tunnel object", []string{"ingress", "status"}, `{"tunnel":{"status":"failed"}}`, "tunnel: \n  status: failed\n", "\x1b[31mfailed\x1b[0m", true},
		{"cloudflare unavailable", []string{"ingress", "status"}, `{"cloudflare":"unavailable","tunnel":{"name":"x"}}`, "cloudflare: unavailable\ntunnel: \n  name: x\n", "\x1b[31munavailable\x1b[0m", true},
		{"malformed ingress maps", []string{"ingress", "status"}, `{"cloudflare":{"status":"failed"},"tunnel":{"status":{"status":"failed"}}}`, "cloudflare: \n  status: failed\ntunnel: \n  status: \n    status: failed\n", "\x1b[31mfailed\x1b[0m", false},
		{"cloudflare available", []string{"ingress", "status"}, `{"cloudflare":"available"}`, "cloudflare: available\n", "\x1b[32mavailable\x1b[0m", true},
		{"audit object", []string{"audit"}, `{"outcome":"failed"}`, "outcome: failed\n", "\x1b[31mfailed\x1b[0m", false},
		{"audit rows", []string{"audit"}, `[{"outcome":"failed"}]`, "outcome: failed\n\n", "\x1b[31mfailed\x1b[0m", true},
		{"stats malformed array", []string{"stats"}, `[{"docker_error":"bad"}]`, "docker_error: bad\n\n", "\x1b[31mbad\x1b[0m", false},
		{"stats sample", []string{"stats"}, `{"docker_error":"bad"}`, "docker_error: bad\n", "\x1b[31mbad\x1b[0m", true},
		{"stats since", []string{"stats", "--since", "5m"}, `[{"docker_error":"bad"}]`, "docker_error: bad\n\n", "\x1b[31mbad\x1b[0m", true},
		{"stats since object", []string{"stats", "--since", "5m"}, `{"docker_error":"bad"}`, "docker_error: bad\n", "\x1b[31mbad\x1b[0m", false},
		{"ps roles object", []string{"ps"}, `[{"roles":{"replicas":[{"state":"failed"}]}}]`, "roles: \n  replicas: \n    state: failed\n\n\n", "\x1b[31mfailed\x1b[0m", false},
		{"ps replicas object", []string{"ps"}, `[{"roles":[{"replicas":{"state":"failed"}}]}]`, "roles: \n  replicas: \n    state: failed\n\n\n", "\x1b[31mfailed\x1b[0m", false},
		{"stats container object", []string{"stats"}, `{"containers":{"error":"bad"}}`, "containers: \n  error: bad\n", "\x1b[31mbad\x1b[0m", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body = tc.data
			out.Reset()
			pipe.Reset()
			errs.Reset()
			args := append(append([]string{}, tc.args...), "-e", "dev", "--config", cfg)
			if code := Run(args, &out, &errs); code != 0 {
				t.Fatalf("exit %d: %s", code, errs.String())
			}
			if got := strings.Contains(out.String(), tc.span); got != tc.colored {
				t.Errorf("color %v in %q", got, out.String())
			}
			if code := Run(args, &pipe, &errs); code != 0 || pipe.String() != tc.want {
				t.Errorf("plain %d %q want %q: %s", code, pipe.String(), tc.want, errs.String())
			}
			out.Reset()
			pipe.Reset()
			args = append(args, "--json")
			if a, b := Run(args, &out, &errs), Run(args, &pipe, &errs); a != 0 || b != 0 || out.String() != pipe.String() || strings.Contains(out.String(), "\x1b") {
				t.Errorf("json %d %d %q %q", a, b, out.String(), pipe.String())
			}
		})
	}
	body = "event: stats\ndata: {\"docker_error\":\"bad\"}\n\nevent: done\ndata: {\"complete\":true}\n\n"
	out.Reset()
	pipe.Reset()
	errs.Reset()
	args := []string{"stats", "--watch", "-e", "dev", "--config", cfg}
	if code := Run(args, &out, &errs); code != 0 || !strings.Contains(out.String(), "\x1b[31mbad\x1b[0m") {
		t.Errorf("stats watch: %d %q %q", code, out.String(), errs.String())
	}
	if code := Run(args, &pipe, &errs); code != 0 || pipe.String() != "docker_error: bad\n" {
		t.Errorf("stats watch plain: %d %q %q", code, pipe.String(), errs.String())
	}

}

func TestSlice5PreviewMarkersBeforeSanitization(t *testing.T) {
	var out, pipe bytes.Buffer
	slice5Destination(t, &out)
	plan := `{"app":"api","image":"example/api@sha256:abc","roles":["web"],"diff":{"changes":[{"area":"a\u001b","change":"+\tTAB"},{"area":"env","change":"-\nLINE"},{"area":"env","change":"+ OK"},{"area":"env","change":"~ source-secret"}]}}`
	if err := showImagePlan(&out, []byte(plan), "dest-secret", false, "", "source-secret"); err != nil {
		t.Fatal(err)
	}
	if err := showImagePlan(&pipe, []byte(plan), "dest-secret", false, "", "source-secret"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "\x1b[32m+ \x1b[0m   TAB") || strings.Contains(out.String(), "\x1b[31m- \x1b[0mLINE") || !strings.Contains(out.String(), "\x1b[32m+ \x1b[0mOK") || strings.Contains(out.String(), "source-secret") {
		t.Errorf("preview: %q", out.String())
	}
	want := "api\n  a\\x1b    +    TAB\n  env      - LINE\n  env      + OK\n  env      ~ [redacted]\nimage: example/api@sha256:abc\nroles: \n  - web\n"
	if pipe.String() != want {
		t.Errorf("plain preview = %q, want %q", pipe.String(), want)
	}
	held := `{"approval_id":"ap1","expires":"2030-01-01T00:00:00Z","plan":` + plan + `}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(202); fmt.Fprint(w, held) }))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	out.Reset()
	pipe.Reset()
	var errs bytes.Buffer
	f := commandFlags{redactToken: "source-secret"}
	if code := sendCommand(context.Background(), http.DefaultClient, u, "/", "dest-secret", "POST", "deploys", struct{}{}, f, "deploy", &out, &errs); code != exitHeld {
		t.Fatalf("held %d: %s", code, errs.String())
	}
	if strings.Contains(out.String(), "\x1b[32m+ \x1b[0m   TAB") || !strings.Contains(out.String(), "\x1b[32m+ \x1b[0mOK") || strings.Contains(out.String(), "source-secret") {
		t.Errorf("held: %q", out.String())
	}
	if code := sendCommand(context.Background(), http.DefaultClient, u, "/", "dest-secret", "POST", "deploys", struct{}{}, f, "deploy", &pipe, &errs); code != exitHeld {
		t.Fatal(code)
	}
	wantHeld := "held for approval: ap1 (expires 2030-01-01T00:00:00Z)\nplan:\n  diff:\n    a\\x1b    +    TAB\n    env      - LINE\n    env      + OK\n    env      ~ [redacted]\n  app: api\n  image: example/api@sha256:abc\n  roles: \n    - web\n"
	if pipe.String() != wantHeld {
		t.Errorf("held plain = %q, want %q", pipe.String(), wantHeld)
	}
	out.Reset()
	pipe.Reset()
	errs.Reset()
	f.json = true
	if code := sendCommand(context.Background(), http.DefaultClient, u, "/", "dest-secret", "POST", "deploys", struct{}{}, f, "deploy", &out, &errs); code != exitHeld {
		t.Fatal(code)
	}
	if code := sendCommand(context.Background(), http.DefaultClient, u, "/", "dest-secret", "POST", "deploys", struct{}{}, f, "deploy", &pipe, &errs); code != exitHeld {
		t.Fatal(code)
	}
	if out.String() != pipe.String() || strings.ContainsAny(out.String(), "\x1b\r") || strings.Contains(out.String(), "source-secret") {
		t.Errorf("held json: %q / %q", out.String(), pipe.String())
	}
}

func TestSlice5LargeOpaqueNumberPreservesMarkers(t *testing.T) {
	var out, plain, errs bytes.Buffer
	slice5Destination(t, &out)
	plan := `{"app":"api","image":"example/api@sha256:abc","roles":["web"],"opaque":{"number":1e309},"diff":{"changes":[{"area":"env","change":"+\tTAB"},{"area":"env","change":"+ OK"}]}}`
	held := `{"approval_id":"ap1","expires":"2030-01-01T00:00:00Z","plan":` + plan + `}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusAccepted); fmt.Fprint(w, held) }))
	defer server.Close()
	base, _ := url.Parse(server.URL)
	for _, tc := range []struct {
		name string
		run  func(*bytes.Buffer) int
		want string
	}{
		{"preview", func(w *bytes.Buffer) int {
			if err := showImagePlan(w, []byte(plan), "dest-secret", false, "", "source-secret"); err != nil {
				t.Error(err)
				return 1
			}
			return 0
		}, "api\n  env      +    TAB\n  env      + OK\nimage: example/api@sha256:abc\nopaque: \n  number: 1e309\nroles: \n  - web\n"},
		{"held promotion", func(w *bytes.Buffer) int {
			return sendCommand(context.Background(), http.DefaultClient, base, "/", "dest-secret", "POST", "deploys", struct{}{}, commandFlags{redactToken: "source-secret"}, "deploy", w, &errs)
		}, "held for approval: ap1 (expires 2030-01-01T00:00:00Z)\nplan:\n  diff:\n    env      +    TAB\n    env      + OK\n  app: api\n  image: example/api@sha256:abc\n  opaque: \n    number: 1e309\n  roles: \n    - web\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out.Reset()
			plain.Reset()
			errs.Reset()
			wantCode := 0
			if tc.name == "held promotion" {
				wantCode = exitHeld
			}
			if code := tc.run(&out); code != wantCode {
				t.Fatalf("exit %d: %s", code, errs.String())
			}
			if strings.Contains(out.String(), "\x1b[32m+ \x1b[0m   TAB") || !strings.Contains(out.String(), "\x1b[32m+ \x1b[0mOK") {
				t.Errorf("marker spans: %q", out.String())
			}
			if code := tc.run(&plain); code != wantCode || plain.String() != tc.want {
				t.Errorf("plain %d = %q, want %q", code, plain.String(), tc.want)
			}
		})
	}
	out.Reset()
	plain.Reset()
	errs.Reset()
	f := commandFlags{redactToken: "source-secret", json: true}
	if code := sendCommand(context.Background(), http.DefaultClient, base, "/", "dest-secret", "POST", "deploys", struct{}{}, f, "deploy", &out, &errs); code != exitHeld {
		t.Fatal(code)
	}
	if code := sendCommand(context.Background(), http.DefaultClient, base, "/", "dest-secret", "POST", "deploys", struct{}{}, f, "deploy", &plain, &errs); code != exitHeld {
		t.Fatal(code)
	}
	const safeJSON = `{"approval_id":"ap1","expires":"2030-01-01T00:00:00Z","plan":{"app":"api","diff":{"changes":[{"area":"env","change":"+    TAB"},{"area":"env","change":"+ OK"}]},"image":"example/api@sha256:abc","opaque":{"number":1e309},"roles":["web"]}}`
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, []byte(safeJSON), "", "  "); err != nil {
		t.Fatal(err)
	}
	if want := pretty.String() + "\n"; out.String() != want || plain.String() != want {
		t.Errorf("held JSON = %q / %q, want %q", out.String(), plain.String(), want)
	}

}

func TestSlice5UnavailableMarkersFailClosed(t *testing.T) {
	var out bytes.Buffer
	diff := deploy.Diff{Changes: []deploy.DiffChange{{Area: "env", Change: "+    TAB"}}}
	printStyledDiffMarkers(&out, "", diff, cliStyle{color: true}, diffMarkers([]byte(`{"diff":`), false))
	if strings.Contains(out.String(), "\x1b[32m") || !strings.Contains(out.String(), "+    TAB") {
		t.Errorf("unavailable source markers: %q", out.String())
	}
}
