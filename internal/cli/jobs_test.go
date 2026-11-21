package cli

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const jobsBody = `[
 {"name":"cleanup","app":"api","target":"web","schedule":"30 2 * * *","timeout":"1h0m0s","next":"2026-11-17T02:30:00Z","last":{"id":4,"job":"cleanup","trigger":"schedule","started":"2026-11-16T02:30:00Z","finished":"2026-11-16T02:30:02Z","status":"failed","exit_code":3,"detail":"exit status 3"}},
 {"name":"manual","app":"db","target":"postgres","timeout":"1h0m0s"}]`

func TestJobsPrintsATableOrJSON(t *testing.T) {
	var seen string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Method + " " + r.URL.Path + " " + r.Header.Get("Authorization")
		fmt.Fprint(w, jobsBody)
	}))
	defer server.Close()
	cfg := ingressCLIConfig(t, server.URL)
	var out, errout bytes.Buffer
	if code := Run([]string{"jobs", "-e", "dev", "--config", cfg}, &out, &errout); code != 0 {
		t.Fatalf("exit %d: %s", code, errout.String())
	}
	if seen != "GET /v1/projects/demo/environments/dev/jobs Bearer test-token" {
		t.Fatalf("request: %q", seen)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[0], "NAME") || !strings.Contains(lines[0], "NEXT") || !strings.Contains(lines[0], "LAST") {
		t.Fatalf("table:\n%s", out.String())
	}
	for _, want := range []string{"cleanup", "api/web", "30 2 * * *", "2026-11-17 02:30 UTC", "failed", "2026-11-16T02:30:02Z", "exit status 3"} {
		if !strings.Contains(lines[1], want) {
			t.Errorf("row lacks %q: %q", want, lines[1])
		}
	}
	for _, want := range []string{"manual", "db/postgres", "on demand"} {
		if !strings.Contains(lines[2], want) {
			t.Errorf("row lacks %q: %q", want, lines[2])
		}
	}
	out.Reset()
	if code := Run([]string{"jobs", "-e", "dev", "--config", cfg, "--json"}, &out, &errout); code != 0 || !strings.Contains(out.String(), `"name": "cleanup"`) {
		t.Fatalf("json exit %d:\n%s", code, out.String())
	}
	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `[]`) }))
	defer empty.Close()
	out.Reset()
	if code := Run([]string{"jobs", "-e", "dev", "--config", ingressCLIConfig(t, empty.URL)}, &out, &errout); code != 0 || strings.TrimSpace(out.String()) != "no jobs" {
		t.Fatalf("empty: %d %q", code, out.String())
	}
}

// The NEXT column shows the next occurrence on the clock of the manifest's zone, then in UTC.
func TestJobsShowsTheNextOccurrenceInTheManifestsZoneAndInUTC(t *testing.T) {
	body := `[
 {"name":"lisbon","app":"api","schedule":"0 2 * * *","timezone":"Europe/Lisbon","timeout":"1h0m0s","next":"2026-11-20T02:00:00Z","next_local":"2026-11-20T02:00:00+00:00"},
 {"name":"summer","app":"api","schedule":"0 2 * * *","timezone":"Europe/Lisbon","timeout":"1h0m0s","next":"2026-07-20T01:00:00Z","next_local":"2026-07-20T02:00:00+01:00"},
 {"name":"tokyo","app":"api","schedule":"0 2 * * *","timezone":"Asia/Tokyo","timeout":"1h0m0s","next":"2026-11-19T17:00:00Z","next_local":"2026-11-20T02:00:00+09:00"},
 {"name":"utc","app":"api","schedule":"0 2 * * *","timezone":"UTC","timeout":"1h0m0s","next":"2026-11-20T02:00:00Z"},
 {"name":"manual","app":"api","timeout":"1h0m0s"}]`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) }))
	defer server.Close()
	var out, errout bytes.Buffer
	if code := Run([]string{"jobs", "-e", "dev", "--config", ingressCLIConfig(t, server.URL)}, &out, &errout); code != 0 {
		t.Fatalf("exit %d: %s", code, errout.String())
	}
	rows := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n")[1:] {
		rows[strings.Fields(line)[0]] = line
	}
	for name, want := range map[string]string{
		"lisbon": "2026-11-20 02:00 WET (02:00Z)",
		"summer": "2026-07-20 02:00 WEST (01:00Z)",
		"tokyo":  "2026-11-20 02:00 JST (2026-11-19 17:00Z)",
		"utc":    "2026-11-20 02:00 UTC",
	} {
		if !strings.Contains(rows[name], want) {
			t.Errorf("%s row lacks %q: %q", name, want, rows[name])
		}
	}
	if strings.Contains(rows["utc"], "(") {
		t.Errorf("a UTC job shows the UTC time twice: %q", rows["utc"])
	}
}

// jobAgent answers the three requests of "job run". The run reports running for the first polls polls, then finished.
type jobAgent struct {
	mu       sync.Mutex
	polls    int
	finishAt int
	final    string // the run once it has finished
	post     func(w http.ResponseWriter)
	timeout  string
	requests []string
}

func (a *jobAgent) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/environments/dev/agent" {
			// The CLI asks an agent that answered 404 whether it is one of a release before projects. This one is not.
			w.WriteHeader(404)
			return
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		a.requests = append(a.requests, r.Method+" "+r.URL.RequestURI())
		switch {
		case r.Method == "POST" && r.URL.Path == "/v1/projects/demo/environments/dev/jobs/tick/run":
			if a.post != nil {
				a.post(w)
				return
			}
			w.WriteHeader(202)
			fmt.Fprint(w, `{"run_id":7}`)
		case r.Method == "GET" && r.URL.Path == "/v1/projects/demo/environments/dev/jobs":
			fmt.Fprintf(w, `[{"name":"tick","app":"api","timeout":%q}]`, a.timeout)
		case r.Method == "GET" && r.URL.Path == "/v1/projects/demo/environments/dev/jobs/tick/runs":
			a.polls++
			if a.polls < a.finishAt {
				fmt.Fprint(w, `[{"id":7,"job":"tick","trigger":"manual","started":"2026-11-16T10:00:00Z","status":"running"}]`)
				return
			}
			fmt.Fprint(w, a.final)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
			w.WriteHeader(404)
		}
	})
}

func runJob(t *testing.T, a *jobAgent, extra ...string) (int, string, string) {
	t.Helper()
	old := jobPollInterval
	jobPollInterval = time.Millisecond
	defer func() { jobPollInterval = old }()
	server := httptest.NewServer(a.handler(t))
	defer server.Close()
	args := append([]string{"job", "run", "tick", "-e", "dev", "--config", ingressCLIConfig(t, server.URL)}, extra...)
	var out, errout bytes.Buffer
	code := Run(args, &out, &errout)
	return code, out.String(), errout.String()
}

func TestJobRunWaitsForSuccess(t *testing.T) {
	a := &jobAgent{finishAt: 3, timeout: "1h0m0s", final: `[{"id":7,"job":"tick","trigger":"manual","started":"2026-11-16T10:00:00Z","finished":"2026-11-16T10:00:01Z","status":"succeeded","exit_code":0}]`}
	code, out, errout := runJob(t, a)
	if code != 0 || !strings.Contains(out, "succeeded") || !strings.Contains(out, "exit_code: 0") {
		t.Fatalf("exit %d out %q err %q", code, out, errout)
	}
	if a.polls != 3 {
		t.Fatalf("polls: %d", a.polls)
	}
	if got := a.requests[len(a.requests)-1]; got != "GET /v1/projects/demo/environments/dev/jobs/tick/runs?limit=1" {
		t.Fatalf("poll request: %s", got)
	}
}

func TestJobRunFailureExitsNonZeroWithTheDetail(t *testing.T) {
	a := &jobAgent{finishAt: 1, timeout: "1h0m0s", final: `[{"id":7,"job":"tick","trigger":"manual","started":"2026-11-16T10:00:00Z","finished":"2026-11-16T10:00:01Z","status":"failed","exit_code":3,"detail":"exit status 3"}]`}
	code, out, _ := runJob(t, a)
	if code != 1 || !strings.Contains(out, "failed") || !strings.Contains(out, "exit status 3") || !strings.Contains(out, "exit_code: 3") {
		t.Fatalf("exit %d out %q", code, out)
	}
	a = &jobAgent{finishAt: 1, timeout: "1h0m0s", final: a.final}
	code, out, _ = runJob(t, a, "--json")
	if code != 1 || !strings.Contains(out, `"status": "failed"`) || !strings.Contains(out, `"detail": "exit status 3"`) {
		t.Fatalf("json exit %d out %q", code, out)
	}
}

func TestJobRunRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		status int
		body   string
		want   string
	}{
		"forbidden": {403, `{"code":"forbidden","message":"forbidden"}`, "HTTP 403"},
		"running":   {409, `{"code":"conflict","message":"job is already running"}`, "job is already running"},
		"unknown":   {404, `{"code":"not_found","message":"not found"}`, "HTTP 404"},
	} {
		a := &jobAgent{post: func(w http.ResponseWriter) { w.WriteHeader(tc.status); fmt.Fprint(w, tc.body) }}
		code, _, errout := runJob(t, a)
		if code != 1 || !strings.Contains(errout, tc.want) {
			t.Errorf("%s: exit %d stderr %q", name, code, errout)
		}
		if len(a.requests) != 1 {
			t.Errorf("%s: kept going after the refusal: %v", name, a.requests)
		}
	}
}

func TestJobRunStopsWaitingAfterTheJobsTimeout(t *testing.T) {
	// The run never finishes; the timeout is the job's 1s plus the 30s grace, which the test shortens.
	old := jobWaitGrace
	jobWaitGrace = 0
	defer func() { jobWaitGrace = old }()
	a := &jobAgent{finishAt: 1 << 30, timeout: "1s"}
	start := time.Now()
	code, _, errout := runJob(t, a)
	if code != 1 || !strings.Contains(errout, "did not finish") || time.Since(start) > 10*time.Second {
		t.Fatalf("exit %d stderr %q after %v", code, errout, time.Since(start))
	}
}

func TestJobUsage(t *testing.T) {
	var out, errout bytes.Buffer
	if code := Run([]string{"job", "run", "-e", "dev", "--config", ingressCLIConfig(t, "http://127.0.0.1:1")}, &out, &errout); code != 2 || !strings.Contains(errout.String(), "usage: job run NAME") {
		t.Fatalf("missing name: %d %q", code, errout.String())
	}
	errout.Reset()
	if code := Run([]string{"job", "list", "x", "-e", "dev", "--config", ingressCLIConfig(t, "http://127.0.0.1:1")}, &out, &errout); code != 2 {
		t.Fatalf("unknown subcommand: %d %q", code, errout.String())
	}
}
