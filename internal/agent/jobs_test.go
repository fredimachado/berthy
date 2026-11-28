package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
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

func (b *blockingExec) Run(ctx context.Context, _ manifest.Manifest, _ string, _ manifest.Job, _ int64) (jobs.Outcome, error) {
	if b.started != nil {
		b.started <- struct{}{}
	}
	if b.release != nil {
		<-b.release
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	code := b.code
	return jobs.Outcome{Exit: &code, Container: "container-1"}, nil
}

func jobScheduler(s *state.Store, exec jobs.Runner) *jobs.Scheduler {
	return &jobs.Scheduler{
		Env:   "lab",
		Store: s,
		Manifest: func(ctx context.Context) (manifest.Manifest, error) {
			return runningManifest(ctx, s)
		},
		Runner: exec,
		Log:    func(string) {},
	}
}

func TestJobChecksReportTheLastFinishedRun(t *testing.T) {
	s := openIngressStore(t)
	ctx := context.Background()
	recordRelease(t, s, "release-jobs", jobsManifest)
	w := NewMonitoring(monitoringConfig(""), nil, &deploy.Controller{Store: s, Environment: "lab"}, func(string) {})
	now := at(10, 0, 30)
	w.now = func() time.Time { return now }
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
	bad, _ := s.StartJobRun(ctx, state.JobRun{Job: "tick", App: "api", Trigger: "manual", Started: now})
	if got := tick.Run(ctx); got.State != monitor.Unknown {
		t.Fatalf("while the first run is running: %+v", got)
	}
	if err := s.FinishJobRun(ctx, bad, "failed", &three, "exit status 3"); err != nil {
		t.Fatal(err)
	}
	if got := tick.Run(ctx); got.State != monitor.Critical || got.Detail != "exit status 3" {
		t.Fatalf("after a failure: %+v", got)
	}
	// A failed run that kept its output names the command that shows it.
	again, _ := s.StartJobRun(ctx, state.JobRun{Job: "tick", App: "api", Trigger: "manual", Started: now})
	if err := s.FinishJobRunWithOutput(ctx, again, "failed", &three, "exit status 3", "boom"); err != nil {
		t.Fatal(err)
	}
	if got := tick.Run(ctx); got.State != monitor.Critical || got.Detail != "exit status 3: berthy job logs tick shows its output" {
		t.Fatalf("after a failure with output: %+v", got)
	}
	if got := checks["job:dump"].Run(ctx); got.State != monitor.Unknown {
		t.Fatalf("another job is affected: %+v", got)
	}
	// A skipped occurrence changes nothing.
	if _, err := s.StartJobRun(ctx, state.JobRun{Job: "tick", App: "api", Trigger: "schedule", Scheduled: now.Truncate(time.Minute), Started: now, Status: "skipped", Detail: "previous run still running"}); err != nil {
		t.Fatal(err)
	}
	if got := tick.Run(ctx); got.State != monitor.Critical {
		t.Fatalf("after a skip: %+v", got)
	}
	zero := 0
	good, _ := s.StartJobRun(ctx, state.JobRun{Job: "tick", App: "api", Trigger: "manual", Started: now})
	if err := s.FinishJobRun(ctx, good, "succeeded", &zero, ""); err != nil {
		t.Fatal(err)
	}
	if got := tick.Run(ctx); got.State != monitor.OK {
		t.Fatalf("after a success: %+v", got)
	}
	// A run that an orderly agent stop cut short changes nothing, in either direction.
	cut, _ := s.StartJobRun(ctx, state.JobRun{Job: "tick", App: "api", Trigger: "manual", Started: now})
	if err := s.FinishJobRun(ctx, cut, "interrupted", nil, "agent stopped during the run"); err != nil {
		t.Fatal(err)
	}
	if got := tick.Run(ctx); got.State != monitor.OK {
		t.Fatalf("after an interrupted run: %+v", got)
	}
	dumpCut, _ := s.StartJobRun(ctx, state.JobRun{Job: "dump", App: "db", Trigger: "manual", Started: now})
	if err := s.FinishJobRun(ctx, dumpCut, "interrupted", nil, "agent stopped during the run"); err != nil {
		t.Fatal(err)
	}
	if got := checks["job:dump"].Run(ctx); got.State != monitor.Unknown || got.Detail != "not run yet" {
		t.Fatalf("a job with only an interrupted run: %+v", got)
	}
}

func TestJobCheckWarnsForFirstUnhandledSlot(t *testing.T) {
	for _, tc := range []struct {
		name, cron        string
		handled, boundary time.Time
		detail            string
	}{
		{"daily", "30 2 * * *", time.Date(2026, 11, 15, 2, 30, 0, 0, time.UTC), at(3, 30, 0), "2026-11-16T02:30:00Z"},
		{"ten minutes", "*/10 * * * *", at(10, 0, 0), at(10, 20, 0), "2026-11-16T10:10:00Z"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := openIngressStore(t)
			ctx := context.Background()
			recordRelease(t, s, "release", strings.Replace(jobsManifest, "* * * * *", tc.cron, 1))
			id, err := s.StartJobRun(ctx, state.JobRun{Job: "tick", App: "api", Trigger: "schedule", Scheduled: tc.handled, Started: tc.handled})
			if err != nil {
				t.Fatal(err)
			}
			if err := s.FinishJobRun(ctx, id, "succeeded", nil, ""); err != nil {
				t.Fatal(err)
			}
			now := tc.handled
			w := NewMonitoring(monitoringConfig(""), nil, &deploy.Controller{Store: s, Environment: "lab"}, func(string) {})
			w.now = func() time.Time { return now }
			var check monitor.Check
			for _, c := range w.checks(ctx) {
				if c.Name() == "job:tick" {
					check = c
				}
			}
			if check == nil {
				t.Fatal("missing job check")
			}
			now = tc.boundary
			if got := check.Run(ctx); got.State != monitor.OK {
				t.Errorf("at boundary: %+v", got)
			}
			for _, delta := range []time.Duration{time.Second, 2 * time.Hour} {
				now = tc.boundary.Add(delta)
				if got := check.Run(ctx); got.State != monitor.Warning || got.Detail != "missed scheduled run at "+tc.detail {
					t.Errorf("after boundary: %+v", got)
				}
			}
		})
	}
}

func TestJobCheckUsesScheduledOccurrencesOfEveryStatus(t *testing.T) {
	s := openIngressStore(t)
	ctx := context.Background()
	recordRelease(t, s, "release", strings.Replace(jobsManifest, "* * * * *", "*/10 * * * *", 1))
	now := at(10, 0, 0)
	w := NewMonitoring(monitoringConfig(""), nil, &deploy.Controller{Store: s, Environment: "lab"}, func(string) {})
	w.now = func() time.Time { return now }
	var check monitor.Check
	for _, c := range w.checks(ctx) {
		if c.Name() == "job:tick" {
			check = c
		}
	}
	if check == nil {
		t.Fatal("missing job check")
	}
	add := func(trigger string, scheduled time.Time, status string, output string) {
		t.Helper()
		run := state.JobRun{Job: "tick", App: "api", Trigger: trigger, Scheduled: scheduled, Started: now}
		if status == "skipped" {
			run.Status = status
		}
		id, err := s.StartJobRun(ctx, run)
		if err != nil {
			t.Fatal(err)
		}
		if status != "running" && status != "skipped" {
			if err := s.FinishJobRunWithOutput(ctx, id, status, nil, "exit status 3", output); err != nil {
				t.Fatal(err)
			}
		}
	}
	assert := func(want string, detail string) {
		t.Helper()
		got := check.Run(ctx)
		if got.State != want || got.Detail != detail {
			t.Errorf("got %+v, want %s %q", got, want, detail)
		}
	}
	add("schedule", at(10, 0, 0), "succeeded", "")
	now = at(10, 20, 1)
	assert(monitor.Warning, "missed scheduled run at 2026-11-16T10:10:00Z")
	add("manual", time.Time{}, "succeeded", "")
	add("backup", time.Time{}, "succeeded", "")
	assert(monitor.Warning, "missed scheduled run at 2026-11-16T10:10:00Z")
	add("schedule", at(10, 20, 0), "skipped", "")
	assert(monitor.OK, "last run succeeded")
	now = at(10, 40, 1)
	add("schedule", at(10, 30, 0), "running", "")
	assert(monitor.OK, "last run succeeded")
	add("schedule", at(10, 40, 0), "interrupted", "")
	now = at(11, 0, 1)
	assert(monitor.Warning, "missed scheduled run at 2026-11-16T10:50:00Z")
	add("manual", time.Time{}, "failed", "boom")
	assert(monitor.Critical, "exit status 3: berthy job logs tick shows its output")
}

func TestJobCheckRestartsGraceAfterScheduleRedeploy(t *testing.T) {
	s := openIngressStore(t)
	ctx := context.Background()
	daily := strings.Replace(jobsManifest, "* * * * *", "30 2 * * *", 1)
	recordRelease(t, s, "daily", daily)
	old := at(2, 30, 0)
	id, err := s.StartJobRun(ctx, state.JobRun{Job: "tick", App: "api", Trigger: "schedule", Scheduled: old, Started: old})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.FinishJobRun(ctx, id, "succeeded", nil, ""); err != nil {
		t.Fatal(err)
	}
	now := at(8, 5, 0)
	w := NewMonitoring(monitoringConfig(""), nil, &deploy.Controller{Store: s, Environment: "lab"}, func(string) {})
	w.now = func() time.Time { return now }
	jobCheckFrom := func() monitor.Check {
		t.Helper()
		for _, c := range w.checks(ctx) {
			if c.Name() == "job:tick" {
				return c
			}
		}
		t.Fatal("missing check")
		return nil
	}
	_ = jobCheckFrom()
	now = at(12, 5, 0)
	recordAppRelease(t, s, "hourly", "api", strings.Replace(daily, "30 2 * * *", "0 * * * *", 1))
	check := jobCheckFrom()
	if got := check.Run(ctx); got.State != monitor.OK {
		t.Errorf("on redeploy: %+v", got)
	}
	now = at(13, 30, 0)
	check = jobCheckFrom()
	now = at(14, 0, 0)
	if got := check.Run(ctx); got.State != monitor.OK {
		t.Errorf("boundary: %+v", got)
	}
	now = at(14, 0, 1)
	if got := check.Run(ctx); got.State != monitor.Warning || got.Detail != "missed scheduled run at 2026-11-16T13:00:00Z" {
		t.Errorf("after grace: %+v", got)
	}
}

func TestJobCheckRestartsGraceAfterTimezoneOnlyApply(t *testing.T) {
	for _, withOld := range []bool{false, true} {
		t.Run(strconv.FormatBool(withOld), func(t *testing.T) {
			f := newApprovalFixture(t, false, nil)
			ctx := context.Background()
			daily := strings.Replace(jobsManifest, "* * * * *", "30 2 * * *", 1)
			recordRelease(t, f.s, "daily", daily)
			if withOld {
				old := at(2, 30, 0)
				id, err := f.s.StartJobRun(ctx, state.JobRun{Job: "tick", App: "api", Trigger: "schedule", Scheduled: old, Started: old})
				if err != nil {
					t.Fatal(err)
				}
				if err := f.s.FinishJobRun(ctx, id, "succeeded", nil, ""); err != nil {
					t.Fatal(err)
				}
			}
			now := at(16, 0, 0)
			w := NewMonitoring(monitoringConfig(""), nil, f.ctrl, func(string) {})
			w.now = func() time.Time { return now }
			get := func(w *Monitoring) monitor.Check {
				t.Helper()
				for _, c := range w.checks(ctx) {
					if c.Name() == "job:tick" {
						return c
					}
				}
				t.Fatal("missing check")
				return nil
			}
			_ = get(w)
			before, err := f.s.History(ctx, "")
			if err != nil {
				t.Fatal(err)
			}
			now = at(18, 0, 0)
			tokyo := strings.Replace(daily, `"network":"demo"`, `"network":"demo","timezone":"Asia/Tokyo"`, 1)
			applyChanges(t, f, apDeployToken, applyBody(t, tokyo, false))
			after, err := f.s.History(ctx, "")
			if err != nil || len(before) != len(after) {
				t.Fatalf("release history changed: %d %d %v", len(before), len(after), err)
			}
			check := get(w)
			fresh := NewMonitoring(monitoringConfig(""), nil, f.ctrl, func(string) {})
			fresh.now = func() time.Time { return now }
			freshCheck := get(fresh)
			now = at(18, 30, 0).Add(24 * time.Hour)
			want := monitor.Unknown
			if withOld {
				want = monitor.OK
			}
			for _, c := range []monitor.Check{check, freshCheck} {
				if got := c.Run(ctx); got.State != want {
					t.Errorf("boundary: %+v", got)
				}
			}
			now = now.Add(time.Second)
			for _, c := range []monitor.Check{check, freshCheck} {
				if got := c.Run(ctx); got.State != monitor.Warning || got.Detail != "missed scheduled run at 2026-11-17T17:30:00Z" {
					t.Errorf("after grace: %+v", got)
				}
			}
		})
	}
}

func TestJobCheckKeepsLastGoodRebuildOnInvalidSchedule(t *testing.T) {
	s := openIngressStore(t)
	ctx := context.Background()
	hourly := strings.Replace(jobsManifest, "* * * * *", "0 * * * *", 1)
	recordRelease(t, s, "valid", hourly)
	now := at(12, 5, 0)
	w := NewMonitoring(monitoringConfig(""), nil, &deploy.Controller{Store: s, Environment: "lab"}, func(string) {})
	w.now = func() time.Time { return now }
	var previous monitor.Check
	for _, c := range w.checks(ctx) {
		if c.Name() == "job:tick" {
			previous = c
		}
	}
	if previous == nil {
		t.Fatal("missing valid check")
	}
	now = at(13, 30, 0)
	invalid, err := runningManifest(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	job := invalid.Jobs["tick"]
	job.Schedule = "invalid"
	invalid.Jobs["tick"] = job
	if got, err := w.manifestChecks(ctx, invalid); err == nil || got != nil {
		t.Errorf("invalid rebuild returned %d checks, error %v", len(got), err)
	}
	now = at(14, 0, 1)
	if got := previous.Run(ctx); got.State != monitor.Warning || got.Detail != "missed scheduled run at 2026-11-16T13:00:00Z" {
		t.Errorf("previous check: %+v", got)
	}
	recordAppRelease(t, s, "recovered", "api", hourly)
	var recovered monitor.Check
	for _, c := range w.checks(ctx) {
		if c.Name() == "job:tick" {
			recovered = c
		}
	}
	if recovered == nil {
		t.Fatal("recovery lost check")
	}
	if got := recovered.Run(ctx); got.State != monitor.Warning || got.Detail != "missed scheduled run at 2026-11-16T13:00:00Z" {
		t.Errorf("recovered check: %+v", got)
	}
}

func jobsHandler(t *testing.T, s *state.Store, exec jobs.Runner) http.Handler {
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
	w := serveRequest(t, h, http.MethodGet, "/v1/projects/demo/environments/lab/jobs", monitoringToken("a"))
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
	if w = serveRequest(t, h, http.MethodGet, "/v1/projects/demo/environments/lab/jobs", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("no token: %d", w.Code)
	}
	if w = serveRequest(t, h, http.MethodPost, "/v1/projects/demo/environments/lab/jobs", monitoringToken("b")); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST jobs: %d", w.Code)
	}
	empty := NewDeploymentHandler(monitoringConfig(""), nil, nil, &deploy.Controller{Store: openIngressStore(t), Environment: "lab"})
	if w = serveRequest(t, empty, http.MethodGet, "/v1/projects/demo/environments/lab/jobs", monitoringToken("a")); strings.TrimSpace(w.Body.String()) != "[]" {
		t.Fatalf("no manifest: %d %q", w.Code, w.Body.String())
	}
	if w = serveRequest(t, NewDeploymentHandler(monitoringConfig(""), nil, nil, nil), http.MethodGet, "/v1/projects/demo/environments/lab/jobs", monitoringToken("a")); w.Code != http.StatusServiceUnavailable {
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
	code, runs := get("/v1/projects/demo/environments/lab/jobs/tick/runs")
	if code != 200 || len(runs) != 3 || runs[0].ID < runs[1].ID {
		t.Fatalf("runs: %d %+v", code, runs)
	}
	if code, runs = get("/v1/projects/demo/environments/lab/jobs/tick/runs?limit=1"); code != 200 || len(runs) != 1 {
		t.Fatalf("limit: %d %+v", code, runs)
	}
	for _, bad := range []string{"0", "101", "x", "-1"} {
		if code, _ = get("/v1/projects/demo/environments/lab/jobs/tick/runs?limit=" + bad); code != 400 {
			t.Errorf("limit %s: %d", bad, code)
		}
	}
	if code, runs = get("/v1/projects/demo/environments/lab/jobs/dump/runs"); code != 200 || len(runs) != 0 {
		t.Fatalf("job without runs: %d %+v", code, runs)
	}
	if code, _ = get("/v1/projects/demo/environments/lab/jobs/nothing/runs"); code != 404 {
		t.Fatalf("unknown job: %d", code)
	}
}

func TestJobLogsNeedDeployScopeAndShowTheOutputOfAFailedRun(t *testing.T) {
	s := openIngressStore(t)
	ctx := context.Background()
	recordRelease(t, s, "release-jobs", jobsManifest)
	three := 3
	failed, _ := s.StartJobRun(ctx, state.JobRun{Job: "tick", App: "api", Trigger: "manual", Started: time.Now()})
	if err := s.FinishJobRunWithOutput(ctx, failed, "failed", &three, "exit status 3", "boom [redacted]\n"); err != nil {
		t.Fatal(err)
	}
	ok, _ := s.StartJobRun(ctx, state.JobRun{Job: "tick", App: "api", Trigger: "manual", Started: time.Now()})
	if err := s.FinishJobRun(ctx, ok, "succeeded", nil, ""); err != nil {
		t.Fatal(err)
	}
	h := jobsHandler(t, s, &blockingExec{})
	path := "/v1/projects/demo/environments/lab/jobs/tick/logs"
	if w := serveRequest(t, h, http.MethodGet, path, monitoringToken("a")); w.Code != http.StatusForbidden {
		t.Fatalf("read token: %d", w.Code)
	}
	if w := serveRequest(t, h, http.MethodGet, path, ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("no token: %d", w.Code)
	}
	if w := serveRequest(t, h, http.MethodPost, path, monitoringToken("b")); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST logs: %d", w.Code)
	}
	get := func(path string) (int, api.JobLogs) {
		w := serveRequest(t, h, http.MethodGet, path, monitoringToken("b"))
		var logs api.JobLogs
		_ = json.Unmarshal(w.Body.Bytes(), &logs)
		return w.Code, logs
	}
	// Without a run, the newest run that kept output answers, though a newer one succeeded.
	code, logs := get(path)
	if code != 200 || logs.Job != "tick" || logs.RunID != failed || logs.Status != "failed" || logs.Detail != "exit status 3" || logs.Finished == nil || logs.Output != "boom [redacted]\n" {
		t.Fatalf("newest: %d %+v", code, logs)
	}
	if code, logs = get(path + "?run=" + strconv.FormatInt(ok, 10)); code != 200 || logs.RunID != ok || logs.Status != "succeeded" || logs.Output != "" {
		t.Fatalf("a successful run: %d %+v", code, logs)
	}
	for _, bad := range []string{"x", "-1", "0"} {
		if code, _ = get(path + "?run=" + bad); code != 400 {
			t.Errorf("run %s: %d", bad, code)
		}
	}
	for _, missing := range []string{"/v1/projects/demo/environments/lab/jobs/dump/logs", "/v1/projects/demo/environments/lab/jobs/nothing/logs", path + "?run=999"} {
		if code, _ = get(missing); code != 404 {
			t.Errorf("%s: %d", missing, code)
		}
	}
}

func TestJobRunNeedsDeployScopeAndRunsInTheBackground(t *testing.T) {
	s := openIngressStore(t)
	ctx := context.Background()
	recordRelease(t, s, "release-jobs", jobsManifest)
	exec := &blockingExec{release: make(chan struct{}), started: make(chan struct{}, 2)}
	sched := jobScheduler(s, exec)
	h := NewDeploymentHandlerWithJobs(monitoringConfig(""), nil, nil, &deploy.Controller{Store: s, Environment: "lab"}, nil, sched)
	path := "/v1/projects/demo/environments/lab/jobs/tick/run"
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
	if w = serveRequest(t, h, http.MethodPost, "/v1/projects/demo/environments/lab/jobs/nothing/run", monitoringToken("b")); w.Code != http.StatusNotFound {
		t.Fatalf("unknown job: %d", w.Code)
	}
	runs, _ := s.JobRuns(ctx, "tick", 5)
	if len(runs) != 1 || runs[0].ID != accepted.RunID || runs[0].Status != "running" || runs[0].Trigger != "manual" {
		t.Fatalf("running run: %+v", runs)
	}
	close(exec.release)
	// Drain the background run the way agent shutdown does: Wait returns only after the
	// finished status and its job_run audit entry are both recorded.
	sched.Wait()
	runs, _ = s.JobRuns(ctx, "tick", 5)
	if len(runs) != 1 || runs[0].Status != "succeeded" {
		t.Fatalf("finished run: %+v", runs)
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
	if w := serveRequest(t, h, http.MethodPost, "/v1/projects/demo/environments/lab/jobs/tick/run", monitoringToken("b")); w.Code != http.StatusServiceUnavailable {
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
	if got := names(api.App{Ref: api.AppRef{Kind: "compose", Name: "demo-lab-db"}}); got != "dump" {
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
	w := serveRequest(t, h, http.MethodGet, "/v1/projects/demo/environments/lab/apps/api", monitoringToken("a"))
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
	w = serveRequest(t, h, http.MethodGet, "/v1/projects/demo/environments/lab/apps/api", monitoringToken("a"))
	if w.Code != http.StatusOK || bytes.Contains(w.Body.Bytes(), []byte(`"jobs"`)) {
		t.Fatalf("app without jobs: %d %s", w.Code, w.Body.String())
	}
}
