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

const backupSum = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

const backupEntries = `[
 {"key":"api/uploads/uploads/20261116T100000Z-2.tar.gz","backup":"uploads","app":"api","volume":"uploads","size":1536,"sha256":"` + backupSum + `","finished":"2026-11-16T10:00:03Z"},
 {"key":"api/uploads/uploads/20261115T100000Z-1.tar.gz","backup":"uploads","app":"api","volume":"uploads","size":12,"sha256":"` + backupSum + `","finished":"2026-11-15T10:00:01Z"}]`

func TestBackupListPrintsATableOrJSON(t *testing.T) {
	var seen string
	body := backupEntries
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Method + " " + r.URL.RequestURI() + " " + r.Header.Get("Authorization")
		fmt.Fprint(w, body)
	}))
	defer server.Close()
	cfg := ingressCLIConfig(t, server.URL)
	var out, errout bytes.Buffer
	if code := Run([]string{"backup", "list", "-e", "dev", "--config", cfg}, &out, &errout); code != 0 {
		t.Fatalf("exit %d: %s", code, errout.String())
	}
	if seen != "GET /v1/environments/dev/backups Bearer test-token" {
		t.Fatalf("request: %q", seen)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[0], "BACKUP") || !strings.Contains(lines[0], "SIZE") || !strings.Contains(lines[0], "SHA256") || !strings.Contains(lines[0], "KEY") {
		t.Fatalf("table:\n%s", out.String())
	}
	for _, want := range []string{"uploads", "api", "1.5 KiB", backupSum, "2026-11-16T10:00:03Z", "api/uploads/uploads/20261116T100000Z-2.tar.gz"} {
		if !strings.Contains(lines[1], want) {
			t.Errorf("row lacks %q: %q", want, lines[1])
		}
	}
	if !strings.Contains(lines[2], "12 B") {
		t.Errorf("row: %q", lines[2])
	}
	out.Reset()
	if code := Run([]string{"backup", "list", "-e", "dev", "--config", cfg, "--backup", "uploads", "--json"}, &out, &errout); code != 0 || !strings.Contains(out.String(), `"key": "api/uploads/uploads/20261116T100000Z-2.tar.gz"`) || !strings.Contains(out.String(), `"size": 1536`) {
		t.Fatalf("json exit %d:\n%s", code, out.String())
	}
	if !strings.Contains(seen, "/backups?backup=uploads ") {
		t.Fatalf("filter request: %q", seen)
	}
	body = `[]`
	out.Reset()
	if code := Run([]string{"backup", "list", "-e", "dev", "--config", cfg}, &out, &errout); code != 0 || strings.TrimSpace(out.String()) != "no backups" {
		t.Fatalf("empty: %d %q", code, out.String())
	}
}

func TestBackupListShowsAgentRefusals(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		fmt.Fprint(w, `{"code":"forbidden","message":"forbidden"}`)
	}))
	defer server.Close()
	var out, errout bytes.Buffer
	if code := Run([]string{"backup", "list", "-e", "dev", "--config", ingressCLIConfig(t, server.URL)}, &out, &errout); code != 1 || !strings.Contains(errout.String(), "HTTP 403") {
		t.Fatalf("exit %d: %q", code, errout.String())
	}
}

// backupAgent answers the requests of "backup run": the start, then the polls of the run.
type backupAgent struct {
	mu       sync.Mutex
	polls    int
	finishAt int
	final    string
	post     func(w http.ResponseWriter)
	requests []string
}

func (a *backupAgent) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		a.requests = append(a.requests, r.Method+" "+r.URL.RequestURI())
		switch {
		case r.Method == "POST" && r.URL.Path == "/v1/environments/dev/backups/uploads/run":
			if a.post != nil {
				a.post(w)
				return
			}
			w.WriteHeader(202)
			fmt.Fprint(w, `{"backup_run_id":7}`)
		case r.Method == "GET" && r.URL.Path == "/v1/environments/dev/backups/uploads/runs":
			a.polls++
			if a.polls < a.finishAt {
				fmt.Fprint(w, `[{"id":7,"backup":"uploads","trigger":"manual","started":"2026-11-16T10:00:00Z","status":"running"}]`)
				return
			}
			fmt.Fprint(w, a.final)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
			w.WriteHeader(404)
		}
	})
}

func runBackup(t *testing.T, a *backupAgent, extra ...string) (int, string, string) {
	t.Helper()
	old := backupPollInterval
	backupPollInterval = time.Millisecond
	defer func() { backupPollInterval = old }()
	server := httptest.NewServer(a.handler(t))
	defer server.Close()
	args := append([]string{"backup", "run", "uploads", "-e", "dev", "--config", ingressCLIConfig(t, server.URL)}, extra...)
	var out, errout bytes.Buffer
	code := Run(args, &out, &errout)
	return code, out.String(), errout.String()
}

const succeededBackup = `[{"id":7,"backup":"uploads","app":"api","volume":"uploads","destination":"offsite","key":"api/uploads/uploads/20261116T100000Z-7.tar.gz","trigger":"manual","started":"2026-11-16T10:00:00Z","finished":"2026-11-16T10:00:03Z","status":"succeeded","size":1536,"sha256":"` + backupSum + `"}]`

func TestBackupRunWaitsForSuccess(t *testing.T) {
	a := &backupAgent{finishAt: 3, final: succeededBackup}
	code, out, errout := runBackup(t, a)
	if code != 0 {
		t.Fatalf("exit %d out %q err %q", code, out, errout)
	}
	for _, want := range []string{"run: 7", "status: succeeded", "key: api/uploads/uploads/20261116T100000Z-7.tar.gz", "size: 1536", "sha256: " + backupSum} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if a.polls != 3 {
		t.Fatalf("polls: %d", a.polls)
	}
	if got := a.requests[len(a.requests)-1]; got != "GET /v1/environments/dev/backups/uploads/runs?limit=1" {
		t.Fatalf("poll request: %s", got)
	}
	a = &backupAgent{finishAt: 1, final: succeededBackup}
	code, out, _ = runBackup(t, a, "--json")
	if code != 0 || !strings.Contains(out, `"status": "succeeded"`) || !json.Valid([]byte(out)) {
		t.Fatalf("json exit %d out %q", code, out)
	}
}

func TestBackupRunFailureExitsNonZeroWithTheDetail(t *testing.T) {
	failed := `[{"id":7,"backup":"uploads","app":"api","volume":"uploads","destination":"offsite","key":"k","trigger":"manual","started":"2026-11-16T10:00:00Z","finished":"2026-11-16T10:00:01Z","status":"failed","detail":"upload failed: object storage unavailable"}]`
	code, out, _ := runBackup(t, &backupAgent{finishAt: 1, final: failed})
	if code != 1 || !strings.Contains(out, "status: failed") || !strings.Contains(out, "detail: upload failed: object storage unavailable") {
		t.Fatalf("exit %d out %q", code, out)
	}
	code, out, _ = runBackup(t, &backupAgent{finishAt: 1, final: failed}, "--json")
	if code != 1 || !strings.Contains(out, `"detail": "upload failed: object storage unavailable"`) {
		t.Fatalf("json exit %d out %q", code, out)
	}
}

func TestBackupRunRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		status int
		body   string
		want   string
	}{
		"forbidden": {403, `{"code":"forbidden","message":"forbidden"}`, "HTTP 403"},
		"running":   {409, `{"code":"conflict","message":"a backup of this volume is already running"}`, "already running"},
		"unknown":   {404, `{"code":"not_found","message":"not found"}`, "HTTP 404"},
		"unmounted": {400, `{"code":"operation_failed","message":"backup uploads: volume ghost is not mounted by app api"}`, "is not mounted by app api"},
	} {
		a := &backupAgent{post: func(w http.ResponseWriter) { w.WriteHeader(tc.status); fmt.Fprint(w, tc.body) }}
		code, _, errout := runBackup(t, a)
		if code != 1 || !strings.Contains(errout, tc.want) {
			t.Errorf("%s: exit %d stderr %q", name, code, errout)
		}
		if len(a.requests) != 1 {
			t.Errorf("%s: kept going after the refusal: %v", name, a.requests)
		}
	}
}

func TestBackupUsage(t *testing.T) {
	cfg := ingressCLIConfig(t, "http://127.0.0.1:1")
	for _, args := range [][]string{
		{"backup", "-e", "dev"},
		{"backup", "run", "-e", "dev"},
		{"backup", "run", "a", "b", "-e", "dev"},
		{"backup", "list", "extra", "-e", "dev"},
		{"backup", "delete", "x", "-e", "dev"},
		{"deploy", "api", "-e", "dev", "--version", "v1", "--into", "x"},
	} {
		var out, errout bytes.Buffer
		if code := Run(append(args, "--config", cfg), &out, &errout); code != 2 {
			t.Errorf("%v: exit %d: %q", args, code, errout.String())
		}
	}
}

func TestRestoreSendsTheKeyAndTheNewVolume(t *testing.T) {
	var path, body string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if r.Method != "POST" || r.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("bad request %s", r.Method)
		}
		path, body = r.URL.Path, string(b)
		fmt.Fprint(w, `{"app":"","image":"","roles":[],"restore":{"key":"api/uploads/uploads/k.tar.gz","backup":"uploads","app":"api","volume":"uploads","into":"restored-1","size":1536,"sha256":"`+backupSum+`","finished":"2026-11-16T10:00:03Z"}}`)
	}))
	defer server.Close()
	cfg := ingressCLIConfig(t, server.URL)
	var out, errs bytes.Buffer
	if code := Run([]string{"restore", "api/uploads/uploads/k.tar.gz", "--into", "restored-1", "-e", "dev", "--config", cfg, "--dry-run"}, &out, &errs); code != 0 {
		t.Fatalf("exit %d: %s", code, errs.String())
	}
	var req map[string]any
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	if path != "/v1/environments/dev/restore" || req["key"] != "api/uploads/uploads/k.tar.gz" || req["into"] != "restored-1" || req["dry_run"] != true || req["request_id"] == "" {
		t.Fatalf("%s %s", path, body)
	}
	for _, want := range []string{"restore api/uploads/uploads/k.tar.gz", "into volume restored-1", "uploads", "1.5 KiB", backupSum} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("plan output lacks %q:\n%s", want, out.String())
		}
	}
	out.Reset()
	if code := Run([]string{"restore", "k", "--into", "restored-1", "-e", "dev", "--config", cfg, "--dry-run", "--json"}, &out, &errs); code != 0 || !json.Valid(out.Bytes()) {
		t.Fatalf("json: exit %d: %s", code, out.String())
	}
}

func TestRestoreReportsTheOperation(t *testing.T) {
	state := "succeeded"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/environments/dev/restore":
			w.WriteHeader(202)
			fmt.Fprint(w, `{"operation_id":"op1","events_url":"/v1/environments/dev/operations/op1/events"}`)
		case "/v1/environments/dev/operations/op1/events":
			fmt.Fprint(w, "id: 1\nevent: accepted\ndata: {}\n\nid: 2\nevent: step\ndata: {\"step\":\"restoring k into volume restored-1\"}\n\nid: 3\nevent: step\ndata: {\"step\":\"restored volume restored-1\"}\n\n")
		case "/v1/environments/dev/operations/op1":
			if state == "failed" {
				fmt.Fprint(w, `{"state":"failed","kind":"restore","error":"backup checksum mismatch"}`)
				return
			}
			fmt.Fprint(w, `{"state":"succeeded","kind":"restore"}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()
	cfg := ingressCLIConfig(t, server.URL)
	var out, errs bytes.Buffer
	if code := Run([]string{"restore", "k", "--into", "restored-1", "-e", "dev", "--config", cfg}, &out, &errs); code != 0 {
		t.Fatalf("exit %d: %s", code, errs.String())
	}
	for _, want := range []string{"restoring k into volume restored-1", "restored volume restored-1"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output lacks %q:\n%s", want, out.String())
		}
	}
	out.Reset()
	state = "failed"
	if code := Run([]string{"restore", "k", "--into", "restored-1", "-e", "dev", "--config", cfg}, &out, &errs); code != 1 || !strings.Contains(out.String(), "backup checksum mismatch") {
		t.Fatalf("failed restore: exit %d: %s", code, out.String())
	}
}

func TestRestoreUsageAndRefusals(t *testing.T) {
	cfg := ingressCLIConfig(t, "http://127.0.0.1:1")
	for _, args := range [][]string{
		{"restore", "-e", "dev"},
		{"restore", "k", "-e", "dev"},
		{"restore", "k", "j", "--into", "v", "-e", "dev"},
	} {
		var out, errout bytes.Buffer
		if code := Run(append(args, "--config", cfg), &out, &errout); code != 2 {
			t.Errorf("%v: exit %d: %q", args, code, errout.String())
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		fmt.Fprint(w, `{"code":"forbidden","message":"forbidden"}`)
	}))
	defer server.Close()
	var out, errout bytes.Buffer
	if code := Run([]string{"restore", "k", "--into", "v", "-e", "dev", "--config", ingressCLIConfig(t, server.URL)}, &out, &errout); code != 1 || !strings.Contains(errout.String(), "HTTP 403") {
		t.Fatalf("exit %d: %q", code, errout.String())
	}
}
