package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"berthy/internal/api"
	"berthy/internal/version"
)

const versionsManifest = "project: demo\nnetwork: demo\napps:\n  api:\n    kind: image\n    image: alpine\n    roles:\n      web: {}\n"

func TestVersionsContract(t *testing.T) {
	var calls atomic.Int32
	response := `{"repository":"docker.io/library/alpine","tags":["one","two"]}`
	status := 200
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path == "/v1/environments/dev/agent" {
			http.NotFound(w, r)
			return
		}
		if r.URL.Path != "/v1/projects/demo/environments/dev/images/tags" || r.Method != "POST" || r.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		var raw map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
			t.Error(err)
		}
		var q api.ImageLookup
		b, _ := json.Marshal(raw)
		_ = json.Unmarshal(b, &q)
		if q.App != "api" || q.Manifest.Project != "demo" || raw["tag"] != nil {
			t.Errorf("bad lookup: %s", b)
		}
		w.WriteHeader(status)
		fmt.Fprint(w, response)
	}))
	defer server.Close()
	cfg := ingressCLIConfig(t, server.URL)
	dir := writeAliasDir(t, versionsManifest)
	t.Setenv("BERTHY_ENV", "dev")
	forceStderrTerminal(t, true)
	run := func(args ...string) (int, string, string) {
		var out, errs bytes.Buffer
		all := append([]string{"versions"}, args...)
		all = append(all, "--project", "demo", "--config", cfg, "--manifest-dir", dir)
		code := Run(all, &out, &errs)
		return code, out.String(), errs.String()
	}
	for _, tc := range []struct {
		name, response, want string
		json                 bool
	}{
		{"text", `{"repository":"docker.io/library/alpine","tags":["one","two"]}`, "one\ntwo\n", false},
		{"json", `{"repository":"docker.io/library/alpine","tags":["one","two"]}`, "{\"repository\":\"docker.io/library/alpine\",\"tags\":[\"one\",\"two\"]}\n", true},
		{"empty", `{"repository":"docker.io/library/alpine","tags":[]}`, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response = tc.response
			args := []string{"api"}
			if tc.json {
				args = append(args, "--json")
			}
			code, out, err := run(args...)
			if code != 0 || out != tc.want || err != "" {
				t.Fatalf("exit %d stdout %q stderr %q", code, out, err)
			}
		})
	}
	for _, bad := range []string{`{`, `{"repository":"docker.io/library/alpine"}`, `{"repository":"docker.io/library/alpine","tags":null}`, `{"repository":"alpine","tags":[]}`, `{"repository":"docker.io/library/alpine","tags":["ok",""]}`, `{"repository":"docker.io/library/alpine","tags":["ok","bad tag"]}`, `{"repository":"docker.io/library/alpine","tags":["x","x"]}`, `{"repository":"docker.io/library/alpine","tags":["ok"]} garbage`} {
		response = bad
		code, out, _ := run("api")
		if code == 0 || out != "" {
			t.Errorf("accepted %q: %d %q", bad, code, out)
		}
	}
	for _, args := range [][]string{{}, {"api", "extra"}, {"api", "--dry-run"}, {"api", "--"}, {"api", "--", "x"}, {"missing"}} {
		before := calls.Load()
		code, out, _ := run(args...)
		if code == 0 || out != "" || calls.Load() != before {
			t.Errorf("accepted %v: %d %q calls %d", args, code, out, calls.Load()-before)
		}
	}
	status = 403
	response = `{"message":"test-token"}`
	code, out, err := run("api")
	if code == 0 || out != "" || !strings.Contains(err, "human") || strings.Contains(err, "test-token") {
		t.Errorf("403: %d %q %q", code, out, err)
	}
	status = 500
	code, out, err = run("api")
	if code == 0 || out != "" || strings.Contains(err, "test-token") {
		t.Errorf("500: %d %q %q", code, out, err)
	}
	status = 404
	code, out, err = run("api")
	if code == 0 || out != "" || !strings.Contains(err, "upgrade") {
		t.Errorf("404: %d %q %q", code, out, err)
	}
}

func TestVersionsMinimumAgentVersion(t *testing.T) {
	old := version.Version
	version.Version = "v1.0.0"
	defer func() { version.Version = old }()
	var tags atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/agent") {
			fmt.Fprint(w, `{"version":"v0.1.0"}`)
			return
		}
		tags.Add(1)
	}))
	defer server.Close()
	cfg := ingressCLIConfig(t, server.URL)
	dir := writeAliasDir(t, "min_version: 0.9.0\n"+versionsManifest)
	var out, err bytes.Buffer
	code := Run([]string{"versions", "api", "-e", "dev", "--config", cfg, "--manifest-dir", dir}, &out, &err)
	if code == 0 || out.Len() != 0 || tags.Load() != 0 || !strings.Contains(err.String(), "upgrade") {
		t.Fatalf("exit %d stdout %q stderr %q calls %d", code, out.String(), err.String(), tags.Load())
	}
}

func TestVersionsAliasAndIncompleteResponse(t *testing.T) {
	var calls atomic.Int32
	broken := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/v1/projects/demo/environments/dev/images/tags" {
			t.Errorf("wrong scope %s", r.URL.Path)
		}
		if broken {
			w.Header().Set("Content-Length", "200")
			fmt.Fprint(w, `{"repository":"docker.io/library/alpine","tags":["one"]}`)
			return
		}
		fmt.Fprint(w, `{"repository":"docker.io/library/alpine","tags":["one"]}`)
	}))
	defer server.Close()
	cfg := ingressCLIConfig(t, server.URL)
	dir := writeAliasDir(t, versionsManifest+"aliases:\n  tags: versions api\n")
	t.Setenv("BERTHY_ENV", "dev")
	var out, errs bytes.Buffer
	code := Run([]string{"tags", "--config", cfg, "--manifest-dir", dir}, &out, &errs)
	if code != 0 || out.String() != "one\n" {
		t.Fatalf("alias: %d %q %q", code, out.String(), errs.String())
	}
	broken = true
	out.Reset()
	errs.Reset()
	code = Run([]string{"versions", "api", "--config", cfg, "--manifest-dir", dir}, &out, &errs)
	if code == 0 || out.Len() != 0 {
		t.Fatalf("partial: %d %q %q", code, out.String(), errs.String())
	}
	before := calls.Load()
	out.Reset()
	errs.Reset()
	code = Run([]string{"tags", "--config", cfg, "--manifest-dir", dir, "-e", "other"}, &out, &errs)
	if code == 0 || out.Len() != 0 || calls.Load() != before {
		t.Fatalf("alias scope: %d %q %q", code, out.String(), errs.String())
	}
}

func TestVersionsRejectsComposeApp(t *testing.T) {
	var calls atomic.Int32
	server := countingAgent(&calls)
	defer server.Close()
	cfg := ingressCLIConfig(t, server.URL)
	dir := writeAliasDir(t, "project: demo\nnetwork: demo\napps:\n  api:\n    kind: compose\n    file: compose.yml\n")
	var out, errs bytes.Buffer
	code := Run([]string{"versions", "api", "-e", "dev", "--config", cfg, "--manifest-dir", dir}, &out, &errs)
	if code == 0 || out.Len() != 0 || calls.Load() != 0 || !strings.Contains(errs.String(), "image app") {
		t.Fatalf("exit %d out %q err %q calls %d", code, out.String(), errs.String(), calls.Load())
	}
}

func TestVersionsRejectsAmbiguousResponse(t *testing.T) {
	response := ""
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(response)) }))
	defer server.Close()
	cfg := ingressCLIConfig(t, server.URL)
	dir := writeAliasDir(t, versionsManifest)
	for _, tc := range []struct{ name, body string }{
		{"duplicate tags", `{"repository":"docker.io/library/alpine","tags":["v1"],"tags":[]}`},
		{"duplicate repository", `{"repository":"other","repository":"docker.io/library/alpine","tags":["v1"]}`},
		{"case variant tags", `{"repository":"docker.io/library/alpine","tags":["v1"],"Tags":[]}`},
		{"case variant repository", `{"repository":"other","Repository":"docker.io/library/alpine","tags":["v1"]}`},
		{"variant alone", `{"Repository":"docker.io/library/alpine","tags":["v1"]}`},
		{"escaped duplicate", `{"repository":"docker.io/library/alpine","tags":["v1"],"\u0074ags":[]}`},
		{"invalid utf8 unknown key", "{\"repository\":\"docker.io/library/alpine\",\"tags\":[\"v1\"],\"bad\xff\":1}"},
		{"wrong repository type", `{"repository":null,"tags":[]}`},
		{"wrong tags type", `{"repository":"docker.io/library/alpine","tags":{}}`},
		{"not object", `[]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response = tc.body
			for _, jsonOutput := range []bool{false, true} {
				args := []string{"versions", "api", "-e", "dev", "--config", cfg, "--manifest-dir", dir}
				if jsonOutput {
					args = append(args, "--json")
				}
				var out, errs bytes.Buffer
				code := Run(args, &out, &errs)
				if code == 0 || out.Len() != 0 {
					t.Errorf("json=%t: exit %d stdout %q stderr %q", jsonOutput, code, out.String(), errs.String())
				}
			}
		})
	}
	response = `{"future":{"tags":["ignored"]},"repository":"docker.io/library/alpine","tags":[]}`
	var out, errs bytes.Buffer
	code := Run([]string{"versions", "api", "-e", "dev", "--config", cfg, "--manifest-dir", dir, "--json"}, &out, &errs)
	if code != 0 || out.String() != `{"repository":"docker.io/library/alpine","tags":[]}`+"\n" {
		t.Fatalf("forward-compatible empty: %d %q %q", code, out.String(), errs.String())
	}
}
