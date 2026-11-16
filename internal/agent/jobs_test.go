package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"berthy/internal/api"
	"berthy/internal/deploy"
	"berthy/internal/engine"
	"berthy/internal/jobs"
	"berthy/internal/manifest"
	"berthy/internal/monitor"
	"berthy/internal/state"
)

// jobsManifest has an image app with one role, a Compose app, two scheduled jobs and a job that only runs on demand.
const jobsManifest = `{"project":"demo","network":"demo",
	"apps":{"api":{"kind":"image","image":"ghcr.io/acme/api","roles":{"web":{}}},"db":{"kind":"compose","file":"db/compose.yml"}},
	"jobs":{
		"tick":{"app":"api","role":"web","schedule":"* * * * *","command":["date"],"timeout":"30s"},
		"dump":{"app":"db","service":"postgres","schedule":"30 2 * * *","command":["pg_dump"]},
		"manual":{"app":"api","command":["true"]}}}`

type blockingExec struct {
	mu      sync.Mutex
	code    int
	release chan struct{}
	started chan struct{}
}

func (b *blockingExec) ExecDiscard(ctx context.Context, id string, cmd []string) (int, error) {
	if b.started != nil {
		b.started <- struct{}{}
	}
	if b.release != nil {
		<-b.release
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.code, nil
}

type fixedTarget struct{}

func (fixedTarget) Container(context.Context, manifest.Manifest, manifest.Job) (string, error) {
	return "container-1", nil
}

func jobScheduler(s *state.Store, exec jobs.Executor) *jobs.Scheduler {
	return &jobs.Scheduler{
		Env:   "lab",
		Store: s,
		Manifest: func(ctx context.Context) (manifest.Manifest, error) {
			m, _, err := s.LatestManifest(ctx)
			return m, err
		},
		Target: fixedTarget{},
		Exec:   exec,
		Log:    func(string) {},
	}
}

func TestJobChecksReportTheLastFinishedRun(t *testing.T) {
	s := openIngressStore(t)
	ctx := context.Background()
	recordRelease(t, s, "release-jobs", jobsManifest)
	w := NewMonitoring(monitoringConfig(""), nil, nil, &deploy.Controller{Store: s, Environment: "lab"}, func(string) {})
	checks := map[string]monitor.Check{}
	for _, c := range w.checks(ctx) {
		checks[c.Name()] = c
	}
	if checks["job:tick"] == nil || checks["job:dump"] == nil {
		t.Fatalf("scheduled jobs have no checks: %d checks", len(checks))
	}
	if checks["job:manual"] != nil {
		t.Fatal("a job without a schedule has a check")
	}
	tick := checks["job:tick"]
	if tick.Interval() != 30*time.Second || tick.Failures() != 1 {
		t.Fatalf("check settings: %v %d", tick.Interval(), tick.Failures())
	}
	if got := tick.Run(ctx); got.State != monitor.Unknown || got.Detail != "not run yet" {
		t.Fatalf("before any run: %+v", got)
	}
	three := 3
	bad, _ := s.StartJobRun(ctx, state.JobRun{Job: "tick", App: "api", Trigger: "manual", Started: time.Now()})
	if got := tick.Run(ctx); got.State != monitor.Unknown {
		t.Fatalf("while the first run is running: %+v", got)
	}
	if err := s.FinishJobRun(ctx, bad, "failed", &three, "exit status 3"); err != nil {
		t.Fatal(err)
	}
	if got := tick.Run(ctx); got.State != monitor.Critical || got.Detail != "exit status 3" {
		t.Fatalf("after a failure: %+v", got)
	}
	if got := checks["job:dump"].Run(ctx); got.State != monitor.Unknown {
		t.Fatalf("another job is affected: %+v", got)
	}
	// A skipped occurrence changes nothing.
	if _, err := s.StartJobRun(ctx, state.JobRun{Job: "tick", App: "api", Trigger: "schedule", Scheduled: time.Now().Truncate(time.Minute), Started: time.Now(), Status: "skipped", Detail: "previous run still running"}); err != nil {
		t.Fatal(err)
	}
	if got := tick.Run(ctx); got.State != monitor.Critical {
		t.Fatalf("after a skip: %+v", got)
	}
	zero := 0
	good, _ := s.StartJobRun(ctx, state.JobRun{Job: "tick", App: "api", Trigger: "manual", Started: time.Now()})
	if err := s.FinishJobRun(ctx, good, "succeeded", &zero, ""); err != nil {
		t.Fatal(err)
	}
	if got := tick.Run(ctx); got.State != monitor.OK {
		t.Fatalf("after a success: %+v", got)
	}
	// A run that an orderly agent stop cut short changes nothing, in either direction.
	cut, _ := s.StartJobRun(ctx, state.JobRun{Job: "tick", App: "api", Trigger: "manual", Started: time.Now()})
	if err := s.FinishJobRun(ctx, cut, "interrupted", nil, "agent stopped during the run"); err != nil {
		t.Fatal(err)
	}
	if got := tick.Run(ctx); got.State != monitor.OK {
		t.Fatalf("after an interrupted run: %+v", got)
	}
	dumpCut, _ := s.StartJobRun(ctx, state.JobRun{Job: "dump", App: "db", Trigger: "manual", Started: time.Now()})
	if err := s.FinishJobRun(ctx, dumpCut, "interrupted", nil, "agent stopped during the run"); err != nil {
		t.Fatal(err)
	}
	if got := checks["job:dump"].Run(ctx); got.State != monitor.Unknown || got.Detail != "not run yet" {
		t.Fatalf("a job with only an interrupted run: %+v", got)
	}
}

func jobsHandler(t *testing.T, s *state.Store, exec jobs.Executor) http.Handler {
	t.Helper()
	return NewDeploymentHandlerWithJobs(monitoringConfig(""), nil, nil, &deploy.Controller{Store: s, Environment: "lab"}, nil, jobScheduler(s, exec))
}

func TestJobsListShowsNextAndLastRun(t *testing.T) {
	s := openIngressStore(t)
	ctx := context.Background()
	recordRelease(t, s, "release-jobs", jobsManifest)
	id, _ := s.StartJobRun(ctx, state.JobRun{Job: "tick", App: "api", Trigger: "manual", Started: time.Now()})
	three := 3
	if err := s.FinishJobRun(ctx, id, "failed", &three, "exit status 3"); err != nil {
		t.Fatal(err)
	}
	h := jobsHandler(t, s, &blockingExec{})
	w := serveRequest(t, h, http.MethodGet, "/v1/environments/lab/jobs", monitoringToken("a"))
	if w.Code != http.StatusOK {
		t.Fatalf("list: %d %s", w.Code, w.Body.String())
	}
	var list []api.JobStatus
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil || len(list) != 3 {
		t.Fatalf("list: %v %s", err, w.Body.String())
	}
	if list[0].Name != "dump" || list[0].Target != "postgres" || list[0].Next == nil || list[0].Last != nil {
		t.Fatalf("dump: %+v", list[0])
	}
	if list[1].Name != "manual" || list[1].Next != nil || list[1].Schedule != "" || list[1].Target != "web" {
		t.Fatalf("manual: %+v", list[1])
	}
	if tick := list[2]; tick.Name != "tick" || tick.Timeout != "30s" || tick.Next == nil || tick.Last == nil || tick.Last.Status != "failed" || tick.Last.Detail != "exit status 3" {
		t.Fatalf("tick: %+v", tick)
	}
	if strings.Contains(w.Body.String(), `"container"`) && strings.Contains(w.Body.String(), `"output"`) {
		t.Fatalf("output in the list: %s", w.Body.String())
	}
	if w = serveRequest(t, h, http.MethodGet, "/v1/environments/lab/jobs", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("no token: %d", w.Code)
	}
	if w = serveRequest(t, h, http.MethodPost, "/v1/environments/lab/jobs", monitoringToken("b")); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST jobs: %d", w.Code)
	}
	empty := NewDeploymentHandler(monitoringConfig(""), nil, nil, &deploy.Controller{Store: openIngressStore(t), Environment: "lab"})
	if w = serveRequest(t, empty, http.MethodGet, "/v1/environments/lab/jobs", monitoringToken("a")); strings.TrimSpace(w.Body.String()) != "[]" {
		t.Fatalf("no manifest: %d %q", w.Code, w.Body.String())
	}
	if w = serveRequest(t, NewDeploymentHandler(monitoringConfig(""), nil, nil, nil), http.MethodGet, "/v1/environments/lab/jobs", monitoringToken("a")); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("no state: %d", w.Code)
	}
}

func TestJobRunsListNewestFirstWithALimit(t *testing.T) {
	s := openIngressStore(t)
	ctx := context.Background()
	recordRelease(t, s, "release-jobs", jobsManifest)
	for i := 0; i < 3; i++ {
		id, _ := s.StartJobRun(ctx, state.JobRun{Job: "tick", App: "api", Trigger: "manual", Started: time.Now()})
		if err := s.FinishJobRun(ctx, id, "succeeded", nil, ""); err != nil {
			t.Fatal(err)
		}
	}
	h := jobsHandler(t, s, &blockingExec{})
	get := func(path string) (int, []api.JobRun) {
		w := serveRequest(t, h, http.MethodGet, path, monitoringToken("a"))
		var runs []api.JobRun
		_ = json.Unmarshal(w.Body.Bytes(), &runs)
		return w.Code, runs
	}
	code, runs := get("/v1/environments/lab/jobs/tick/runs")
	if code != 200 || len(runs) != 3 || runs[0].ID < runs[1].ID {
		t.Fatalf("runs: %d %+v", code, runs)
	}
	if code, runs = get("/v1/environments/lab/jobs/tick/runs?limit=1"); code != 200 || len(runs) != 1 {
		t.Fatalf("limit: %d %+v", code, runs)
	}
	for _, bad := range []string{"0", "101", "x", "-1"} {
		if code, _ = get("/v1/environments/lab/jobs/tick/runs?limit=" + bad); code != 400 {
			t.Errorf("limit %s: %d", bad, code)
		}
	}
	if code, runs = get("/v1/environments/lab/jobs/dump/runs"); code != 200 || len(runs) != 0 {
		t.Fatalf("job without runs: %d %+v", code, runs)
	}
	if code, _ = get("/v1/environments/lab/jobs/nothing/runs"); code != 404 {
		t.Fatalf("unknown job: %d", code)
	}
}

func TestJobRunNeedsDeployScopeAndRunsInTheBackground(t *testing.T) {
	s := openIngressStore(t)
	ctx := context.Background()
	recordRelease(t, s, "release-jobs", jobsManifest)
	exec := &blockingExec{release: make(chan struct{}), started: make(chan struct{}, 2)}
	h := jobsHandler(t, s, exec)
	path := "/v1/environments/lab/jobs/tick/run"
	if w := serveRequest(t, h, http.MethodPost, path, monitoringToken("a")); w.Code != http.StatusForbidden {
		t.Fatalf("read token: %d", w.Code)
	}
	if w := serveRequest(t, h, http.MethodPost, path, ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("no token: %d", w.Code)
	}
	if w := serveRequest(t, h, http.MethodGet, path, monitoringToken("b")); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET run: %d", w.Code)
	}
	w := serveRequest(t, h, http.MethodPost, path, monitoringToken("b"))
	var accepted struct {
		RunID int64 `json:"run_id"`
	}
	if w.Code != http.StatusAccepted || json.Unmarshal(w.Body.Bytes(), &accepted) != nil || accepted.RunID == 0 {
		t.Fatalf("deploy token: %d %s", w.Code, w.Body.String())
	}
	<-exec.started
	if w = serveRequest(t, h, http.MethodPost, path, monitoringToken("b")); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "job is already running") {
		t.Fatalf("second run: %d %s", w.Code, w.Body.String())
	}
	if w = serveRequest(t, h, http.MethodPost, "/v1/environments/lab/jobs/nothing/run", monitoringToken("b")); w.Code != http.StatusNotFound {
		t.Fatalf("unknown job: %d", w.Code)
	}
	runs, _ := s.JobRuns(ctx, "tick", 5)
	if len(runs) != 1 || runs[0].ID != accepted.RunID || runs[0].Status != "running" || runs[0].Trigger != "manual" {
		t.Fatalf("running run: %+v", runs)
	}
	close(exec.release)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if runs, _ = s.JobRuns(ctx, "tick", 5); runs[0].Status != "running" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("run did not finish")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if runs[0].Status != "succeeded" {
		t.Fatalf("finished run: %+v", runs[0])
	}
	// The audit log names who asked, and the job as the caller of the run itself.
	rows, err := s.AuditLog(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	var asked, ran bool
	for _, a := range rows {
		asked = asked || a.Action == "job_start:tick" && a.ActorID != "job:tick" && a.Outcome == "started"
		ran = ran || a.Action == "job_run:tick" && a.ActorID == "job:tick" && a.Outcome == "succeeded"
	}
	if !asked || !ran {
		t.Fatalf("audit: %+v", rows)
	}
}

func TestJobRunWithoutASchedulerIsUnavailable(t *testing.T) {
	s := openIngressStore(t)
	recordRelease(t, s, "release-jobs", jobsManifest)
	h := NewDeploymentHandler(monitoringConfig(""), nil, nil, &deploy.Controller{Store: s, Environment: "lab"})
	if w := serveRequest(t, h, http.MethodPost, "/v1/environments/lab/jobs/tick/run", monitoringToken("b")); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("no scheduler: %d %s", w.Code, w.Body.String())
	}
}

func TestNewJobsNeedsStateAndDocker(t *testing.T) {
	s := openIngressStore(t)
	if NewJobs(monitoringConfig(""), nil, &deploy.Controller{Store: s, Environment: "lab"}, func(string) {}) != nil {
		t.Fatal("scheduler without Docker")
	}
	if NewJobs(monitoringConfig(""), engine.New(filepath.Join(t.TempDir(), "none.sock")), nil, func(string) {}) != nil {
		t.Fatal("scheduler without state")
	}
	if NewJobs(monitoringConfig(""), engine.New(filepath.Join(t.TempDir(), "none.sock")), &deploy.Controller{Store: s, Environment: "lab"}, func(string) {}) == nil {
		t.Fatal("no scheduler with state and Docker")
	}
}

func TestAppJobsMatchesImageAndComposeApps(t *testing.T) {
	s := openIngressStore(t)
	ctx := context.Background()
	recordRelease(t, s, "release-jobs", jobsManifest)
	d := &deploy.Controller{Store: s, Environment: "lab"}
	names := func(app api.App) string {
		var out []string
		for _, j := range appJobs(ctx, d, app, time.Now()) {
			out = append(out, j.Name)
		}
		return strings.Join(out, ",")
	}
	if got := names(api.App{Ref: api.AppRef{Kind: "image", Name: "api"}}); got != "manual,tick" {
		t.Errorf("image app: %q", got)
	}
	// A Compose app is listed under its project name, which the manifest's project and app name make.
	if got := names(api.App{Ref: api.AppRef{Kind: "compose", Name: "demo-db"}}); got != "dump" {
		t.Errorf("compose app: %q", got)
	}
	if got := names(api.App{Ref: api.AppRef{Kind: "compose", Name: "api"}}); got != "" {
		t.Errorf("unmanaged compose project that shares an app's name: %q", got)
	}
	if got := names(api.App{Ref: api.AppRef{Kind: "swarm", Name: "api"}}); got != "" {
		t.Errorf("unmanaged swarm service that shares an app's name: %q", got)
	}
	empty := &deploy.Controller{Store: openIngressStore(t), Environment: "lab"}
	if got := appJobs(ctx, empty, api.App{Ref: api.AppRef{Kind: "image", Name: "api"}}, time.Now()); got != nil {
		t.Errorf("jobs without a manifest: %+v", got)
	}
	if got := appJobs(ctx, nil, api.App{Ref: api.AppRef{Kind: "image", Name: "api"}}, time.Now()); got != nil {
		t.Errorf("jobs without state: %+v", got)
	}
}

// fakeDocker serves the few Docker routes describe needs, over a Unix socket.
func fakeDocker(t *testing.T) *engine.Engine {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "d.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Skipf("no Unix sockets here: %v", err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/containers/json"), strings.HasSuffix(r.URL.Path, "/services") && r.Method == "GET":
			w.Write([]byte(`[]`))
		default:
			w.WriteHeader(404)
			w.Write([]byte(`{"message":"no such service"}`))
		}
	})}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })
	return engine.New(sock)
}

func TestDescribeIncludesTheAppsJobs(t *testing.T) {
	s := openIngressStore(t)
	recordBoundRelease(t, s, "release-jobs", jobsManifest, []state.Binding{{App: "api", Role: "web", Name: "lab-api-web", ServiceID: "svc-1"}})
	ctx := context.Background()
	id, _ := s.StartJobRun(ctx, state.JobRun{Job: "tick", App: "api", Trigger: "manual", Started: time.Now()})
	if err := s.FinishJobRun(ctx, id, "succeeded", nil, ""); err != nil {
		t.Fatal(err)
	}
	h := NewDeploymentHandler(monitoringConfig(""), fakeDocker(t), nil, &deploy.Controller{Store: s, Environment: "lab"})
	w := serveRequest(t, h, http.MethodGet, "/v1/environments/lab/apps/api", monitoringToken("a"))
	var app api.App
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &app) != nil {
		t.Fatalf("describe: %d %s", w.Code, w.Body.String())
	}
	if len(app.Jobs) != 2 || app.Jobs[0].Name != "manual" || app.Jobs[1].Name != "tick" || app.Jobs[1].Last == nil || app.Jobs[1].Last.Status != "succeeded" || app.Jobs[1].Next == nil {
		t.Fatalf("jobs: %+v", app.Jobs)
	}
	var plain map[string]json.RawMessage
	json.Unmarshal(w.Body.Bytes(), &plain)
	if _, ok := plain["jobs"]; !ok {
		t.Fatal("jobs missing from the JSON")
	}
	// An app without jobs keeps its old shape.
	recordBoundRelease(t, s, "release-nojobs", monitoredManifest(``), []state.Binding{{App: "api", Role: "web", Name: "lab-api-web", ServiceID: "svc-1"}})
	w = serveRequest(t, h, http.MethodGet, "/v1/environments/lab/apps/api", monitoringToken("a"))
	if w.Code != http.StatusOK || bytes.Contains(w.Body.Bytes(), []byte(`"jobs"`)) {
		t.Fatalf("app without jobs: %d %s", w.Code, w.Body.String())
	}
}
