package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const diffPlanBody = `{"app":"api","image":"ghcr.io/acme/api@sha256:9f","roles":["web"],"diff":{"changes":[` +
	`{"area":"image","change":"sha256:41ab1c2d3e4f… -> sha256:9f2c0d1e2f3a…   (v1.4.2 -> v1.4.3)"},` +
	`{"area":"web","change":"replicas 2 -> 3"},` +
	`{"area":"env","change":"+ FEATURE_X (plain)"},` +
	`{"area":"env","change":"~ NOTE (plain) \"a\" -> \"\u001b[31mred\""}]}}`

// planDiffAgent answers a dry run with diffPlanBody and records what it was asked.
func planDiffAgent(t *testing.T, plan string) (cfg string, paths func() []string) {
	t.Helper()
	var mu sync.Mutex
	var seen []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Method+" "+r.URL.RequestURI())
		mu.Unlock()
		fmt.Fprint(w, plan)
	}))
	t.Cleanup(server.Close)
	return ingressCLIConfig(t, server.URL), func() []string { mu.Lock(); defer mu.Unlock(); return append([]string{}, seen...) }
}

func apiManifestDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "berthy.yml"), []byte("project: demo\nnetwork: demo\napps:\n  api:\n    kind: image\n    image: alpine\n    roles:\n      web: {}\n"), 0600)
	return dir
}

func TestDryRunDeployPrintsTheDiffUnderTheAppName(t *testing.T) {
	cfg, _ := planDiffAgent(t, diffPlanBody)
	for _, cmd := range [][]string{{"deploy", "api", "--version", "v1", "--manifest-dir", apiManifestDir(t)}, {"rollback", "api"}} {
		var out, errout bytes.Buffer
		args := append(cmd, "-e", "dev", "--config", cfg, "--dry-run")
		if code := Run(args, &out, &errout); code != 0 {
			t.Fatalf("%v: exit %d: %s", cmd, code, errout.String())
		}
		want := "api\n" +
			"  image    sha256:41ab1c2d3e4f… -> sha256:9f2c0d1e2f3a…   (v1.4.2 -> v1.4.3)\n" +
			"  web      replicas 2 -> 3\n" +
			"  env      + FEATURE_X (plain)\n" +
			"  env      ~ NOTE (plain) \"a\" -> \"\\x1b[31mred\"\n"
		if !strings.HasPrefix(out.String(), want) {
			t.Fatalf("%v output:\n%s\nwant it to start with:\n%s", cmd, out.String(), want)
		}
		// The rest of the plan is still shown.
		if !strings.Contains(out.String(), "ghcr.io/acme/api@sha256:9f") || strings.Contains(out.String(), "diff:") {
			t.Fatalf("%v output:\n%s", cmd, out.String())
		}
	}
}

func TestDryRunDeployOfANewAppOrOfNoChangesSaysSo(t *testing.T) {
	for plan, want := range map[string]string{
		`{"app":"api","roles":["web"],"diff":{"new":true}}`:  "api\n  new app\n",
		`{"app":"api","roles":["web"],"diff":{"same":true}}`: "api\n  no changes: the same release would be recorded\n",
	} {
		cfg, _ := planDiffAgent(t, plan)
		var out, errout bytes.Buffer
		if code := Run([]string{"deploy", "api", "--version", "v1", "-e", "dev", "--config", cfg, "--manifest-dir", apiManifestDir(t), "--dry-run"}, &out, &errout); code != 0 {
			t.Fatalf("exit %d: %s", code, errout.String())
		}
		if !strings.HasPrefix(out.String(), want) {
			t.Fatalf("output:\n%s", out.String())
		}
	}
}

func TestDryRunDeployJSONCarriesTheDiff(t *testing.T) {
	cfg, _ := planDiffAgent(t, diffPlanBody)
	var out, errout bytes.Buffer
	if code := Run([]string{"deploy", "api", "--version", "v1", "-e", "dev", "--config", cfg, "--manifest-dir", apiManifestDir(t), "--dry-run", "--json"}, &out, &errout); code != 0 {
		t.Fatalf("exit %d: %s", code, errout.String())
	}
	var plan struct {
		Diff struct {
			Changes []struct{ Area, Change string }
		}
	}
	if err := json.Unmarshal(out.Bytes(), &plan); err != nil || len(plan.Diff.Changes) != 4 || plan.Diff.Changes[1].Change != "replicas 2 -> 3" {
		t.Fatalf("%v\n%s", err, out.String())
	}
}

func TestDryRunDeployWithoutADiffPrintsThePlanAsBefore(t *testing.T) {
	cfg, _ := planDiffAgent(t, `{"app":"api","roles":["web"],"changes":["web: image differs"]}`)
	var out, errout bytes.Buffer
	if code := Run([]string{"deploy", "api", "--version", "v1", "-e", "dev", "--config", cfg, "--manifest-dir", apiManifestDir(t), "--dry-run"}, &out, &errout); code != 0 {
		t.Fatalf("exit %d: %s", code, errout.String())
	}
	if !strings.HasPrefix(out.String(), "app: api\n") || !strings.Contains(out.String(), "- web: image differs") {
		t.Fatalf("output:\n%s", out.String())
	}
}

func TestHeldAnswerAndApproveShowTheDiff(t *testing.T) {
	held := `{"approval_id":"ap1","expires":"2026-11-16T12:15:00Z","plan":` + diffPlanBody + `}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Berthy-Approval", "pending")
		w.WriteHeader(http.StatusAccepted)
		fmt.Fprint(w, held)
	}))
	defer server.Close()
	cfg := ingressCLIConfig(t, server.URL)
	var out, errout bytes.Buffer
	if code := Run([]string{"restart", "api", "-e", "dev", "--config", cfg}, &out, &errout); code != exitHeld {
		t.Fatalf("exit %d", code)
	}
	for _, want := range []string{"plan:\n  diff:\n    image    sha256:41ab1c2d3e4f… -> sha256:9f2c0d1e2f3a…   (v1.4.2 -> v1.4.3)\n    web      replicas 2 -> 3\n    env      + FEATURE_X (plain)\n", "  app: api\n"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "area:") || strings.Contains(out.String(), "\x1b") {
		t.Errorf("the diff is printed raw:\n%q", out.String())
	}
}

func TestApprovalsShowTheDiffOfEachPendingApproval(t *testing.T) {
	approvalsNow = func() time.Time { return time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC) }
	t.Cleanup(func() { approvalsNow = time.Now })
	row := `{"id":"ap1","actor":"claude","kind":"deploy","summary":"deploy api sha256:aaaaaaaaaaaa","plan":` + diffPlanBody + `,"created":"2026-11-16T11:58:30Z","expires":"2026-11-16T12:13:30Z","status":"pending"}`
	plain := `{"id":"ap3","actor":"claude","kind":"restart","summary":"restart api","plan":{"app":"api"},"created":"2026-11-16T11:58:30Z","expires":"2026-11-16T12:13:30Z","status":"pending"}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "["+row+","+plain+"]") }))
	defer server.Close()
	cfg := ingressCLIConfig(t, server.URL)
	var out, errout bytes.Buffer
	if code := Run([]string{"approvals", "-e", "dev", "--config", cfg}, &out, &errout); code != 0 {
		t.Fatalf("exit %d: %s", code, errout.String())
	}
	want := "ap1: changes of api\n  image    sha256:41ab1c2d3e4f… -> sha256:9f2c0d1e2f3a…   (v1.4.2 -> v1.4.3)\n  web      replicas 2 -> 3\n"
	if !strings.Contains(out.String(), want) || strings.Contains(out.String(), "ap3: changes") {
		t.Fatalf("output:\n%s", out.String())
	}
	out.Reset()
	if code := Run([]string{"approvals", "-e", "dev", "--config", cfg, "--json"}, &out, &errout); code != 0 || !strings.Contains(out.String(), `"area": "web"`) {
		t.Fatalf("json: %d\n%s", code, out.String())
	}
}

func TestHistoryDiffAsksForTheDiffAndPrintsIt(t *testing.T) {
	cfg, paths := planDiffAgent(t, `{"changes":[{"area":"image","change":"sha256:aaaaaaaaaaaa… -> sha256:bbbbbbbbbbbb…"},{"area":"web","change":"replicas 1 -> 2"}]}`)
	var out, errout bytes.Buffer
	if code := Run([]string{"history", "api", "--diff", "5", "-e", "dev", "--config", cfg}, &out, &errout); code != 0 {
		t.Fatalf("exit %d: %s", code, errout.String())
	}
	if out.String() != "image    sha256:aaaaaaaaaaaa… -> sha256:bbbbbbbbbbbb…\nweb      replicas 1 -> 2\n" {
		t.Fatalf("output %q", out.String())
	}
	out.Reset()
	if code := Run([]string{"history", "api", "--diff", "5", "3", "-e", "dev", "--config", cfg, "--json"}, &out, &errout); code != 0 || !strings.Contains(out.String(), `"area": "web"`) {
		t.Fatalf("json: %d %s", code, out.String())
	}
	want := []string{"GET /v1/projects/demo/environments/dev/history/api/diff?release=5", "GET /v1/projects/demo/environments/dev/history/api/diff?base=3&release=5"}
	if got := paths(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("requests %q", got)
	}
}

func TestHistoryDiffTakesReleaseNumbersOnly(t *testing.T) {
	cfg, paths := planDiffAgent(t, `{}`)
	for _, args := range [][]string{
		{"history", "api", "--diff", "x"},
		{"history", "api", "--diff", "0"},
		{"history", "api", "--diff", "5", "-1"},
		{"history", "api", "--diff", "5", "3", "2"},
		{"history", "api", "3"},
		{"history", "api", "--diff"},
	} {
		var out, errout bytes.Buffer
		if code := Run(append(args, "-e", "dev", "--config", cfg), &out, &errout); code != 2 {
			t.Errorf("%v: exit %d, %s", args, code, errout.String())
		}
	}
	if got := paths(); len(got) != 0 {
		t.Fatalf("a refused command sent %q", got)
	}
}
