package state

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func minute(n int) time.Time { return time.Date(2026, 11, 16, 10, n, 0, 0, time.UTC) }

func TestStartJobRunRefusesASecondRunOfTheSameOccurrence(t *testing.T) {
	s, _, _ := testStore(t)
	ctx := context.Background()
	first, err := s.StartJobRun(ctx, JobRun{Job: "tick", App: "api", Trigger: "schedule", Scheduled: minute(1), Started: minute(1), Container: "c1"})
	if err != nil || first == 0 {
		t.Fatalf("first run: %d %v", first, err)
	}
	if _, err = s.StartJobRun(ctx, JobRun{Job: "tick", App: "api", Trigger: "schedule", Scheduled: minute(1), Started: minute(1)}); !errors.Is(err, ErrConflict) {
		t.Fatalf("same occurrence: %v", err)
	}
	if _, err = s.StartJobRun(ctx, JobRun{Job: "tick", App: "api", Trigger: "schedule", Scheduled: minute(2), Started: minute(2)}); err != nil {
		t.Fatalf("next occurrence: %v", err)
	}
	if _, err = s.StartJobRun(ctx, JobRun{Job: "other", App: "api", Trigger: "schedule", Scheduled: minute(1), Started: minute(1)}); err != nil {
		t.Fatalf("other job, same minute: %v", err)
	}
	// A skipped run also uses up its occurrence.
	if _, err = s.StartJobRun(ctx, JobRun{Job: "slow", App: "api", Trigger: "schedule", Scheduled: minute(3), Started: minute(3), Finished: minute(3), Status: "skipped", Detail: "previous run still running"}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.StartJobRun(ctx, JobRun{Job: "slow", App: "api", Trigger: "schedule", Scheduled: minute(3), Started: minute(3)}); !errors.Is(err, ErrConflict) {
		t.Fatalf("occurrence of a skipped run: %v", err)
	}
}

func TestManualJobRunsNeverConflict(t *testing.T) {
	s, _, _ := testStore(t)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, err := s.StartJobRun(ctx, JobRun{Job: "tick", App: "api", Trigger: "manual", Started: minute(i)}); err != nil {
			t.Fatalf("manual run %d: %v", i, err)
		}
	}
	runs, err := s.JobRuns(ctx, "tick", 10)
	if err != nil || len(runs) != 3 || !runs[0].Scheduled.IsZero() || runs[0].Trigger != "manual" {
		t.Fatalf("runs: %+v %v", runs, err)
	}
}

func TestJobRunLifecycleAndNewestFirst(t *testing.T) {
	s, _, _ := testStore(t)
	ctx := context.Background()
	a, err := s.StartJobRun(ctx, JobRun{Job: "tick", App: "api", Trigger: "schedule", Scheduled: minute(1), Started: minute(1), Container: "c1"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.StartJobRun(ctx, JobRun{Job: "tick", App: "api", Trigger: "schedule", Scheduled: minute(2), Started: minute(2)})
	if err != nil {
		t.Fatal(err)
	}
	runs, err := s.JobRuns(ctx, "tick", 10)
	if err != nil || len(runs) != 2 || runs[0].ID != b || runs[1].ID != a {
		t.Fatalf("order: %+v %v", runs, err)
	}
	if r := runs[1]; r.Status != "running" || !r.Finished.IsZero() || r.ExitCode != nil || r.Container != "c1" || !r.Scheduled.Equal(minute(1)) || !r.Started.Equal(minute(1)) {
		t.Fatalf("running run: %+v", r)
	}
	three := 3
	if err = s.FinishJobRun(ctx, a, "failed", &three, "exit status 3"); err != nil {
		t.Fatal(err)
	}
	if runs, _ = s.JobRuns(ctx, "tick", 1); len(runs) != 1 {
		t.Fatalf("limit: %+v", runs)
	}
	runs, _ = s.JobRuns(ctx, "tick", 10)
	if r := runs[1]; r.Status != "failed" || r.ExitCode == nil || *r.ExitCode != 3 || r.Detail != "exit status 3" || r.Finished.IsZero() {
		t.Fatalf("finished run: %+v", r)
	}
	if err = s.FinishJobRun(ctx, a, "succeeded", nil, ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("finishing a finished run: %v", err)
	}
	if err = s.FinishJobRun(ctx, b, "running", nil, ""); err == nil {
		t.Fatal("finished a run as running")
	}
	if err = s.FinishJobRun(ctx, b, "succeeded", nil, strings.Repeat("x", 5000)); err != nil {
		t.Fatal(err)
	}
	if runs, _ = s.JobRuns(ctx, "tick", 1); len(runs[0].Detail) > 1024 || runs[0].ExitCode != nil {
		t.Fatalf("detail not bounded or exit code invented: %d %v", len(runs[0].Detail), runs[0].ExitCode)
	}
}

func TestLastJobRunsIgnoresSkippedInterruptedAndRunning(t *testing.T) {
	s, _, _ := testStore(t)
	ctx := context.Background()
	zero := 0
	ok, _ := s.StartJobRun(ctx, JobRun{Job: "tick", App: "api", Trigger: "schedule", Scheduled: minute(1), Started: minute(1)})
	if err := s.FinishJobRun(ctx, ok, "succeeded", &zero, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartJobRun(ctx, JobRun{Job: "tick", App: "api", Trigger: "schedule", Scheduled: minute(2), Started: minute(2), Finished: minute(2), Status: "skipped", Detail: "previous run still running"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartJobRun(ctx, JobRun{Job: "tick", App: "api", Trigger: "schedule", Scheduled: minute(3), Started: minute(3)}); err != nil {
		t.Fatal(err)
	}
	stopped, _ := s.StartJobRun(ctx, JobRun{Job: "tick", App: "api", Trigger: "schedule", Scheduled: minute(4), Started: minute(4)})
	if err := s.FinishJobRun(ctx, stopped, "interrupted", nil, "agent stopped during the run"); err != nil {
		t.Fatal(err)
	}
	bad, _ := s.StartJobRun(ctx, JobRun{Job: "fail", App: "api", Trigger: "manual", Started: minute(1)})
	three := 3
	if err := s.FinishJobRun(ctx, bad, "failed", &three, "exit status 3"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartJobRun(ctx, JobRun{Job: "never", App: "api", Trigger: "manual", Started: minute(1)}); err != nil {
		t.Fatal(err)
	}
	last, err := s.LastJobRuns(ctx)
	if err != nil || len(last) != 2 {
		t.Fatalf("last runs: %+v %v", last, err)
	}
	if last["tick"].ID != ok || last["tick"].Status != "succeeded" || last["fail"].Status != "failed" || last["fail"].Detail != "exit status 3" {
		t.Fatalf("last runs: %+v", last)
	}
}

func TestAbandonRunningJobsFailsOnlyRunningRuns(t *testing.T) {
	s, _, _ := testStore(t)
	ctx := context.Background()
	done, _ := s.StartJobRun(ctx, JobRun{Job: "a", App: "api", Trigger: "manual", Started: minute(1)})
	if err := s.FinishJobRun(ctx, done, "succeeded", nil, ""); err != nil {
		t.Fatal(err)
	}
	s.StartJobRun(ctx, JobRun{Job: "a", App: "api", Trigger: "manual", Started: minute(2)})
	s.StartJobRun(ctx, JobRun{Job: "b", App: "api", Trigger: "schedule", Scheduled: minute(2), Started: minute(2)})
	n, err := s.AbandonRunningJobs(ctx)
	if err != nil || n != 2 {
		t.Fatalf("abandoned %d: %v", n, err)
	}
	runs, _ := s.JobRuns(ctx, "a", 10)
	if runs[0].Status != "failed" || runs[0].Detail != "agent restarted during the run" || runs[0].Finished.IsZero() || runs[1].Status != "succeeded" {
		t.Fatalf("runs: %+v", runs)
	}
	if n, err = s.AbandonRunningJobs(ctx); err != nil || n != 0 {
		t.Fatalf("second pass: %d %v", n, err)
	}
}

func TestJobRunValidationAndRetention(t *testing.T) {
	s, _, _ := testStore(t)
	ctx := context.Background()
	for _, bad := range []JobRun{
		{App: "api", Trigger: "manual", Started: minute(1)},
		{Job: "tick", Trigger: "manual", Started: minute(1)},
		{Job: "tick", App: "api", Trigger: "cron", Started: minute(1)},
		{Job: "tick", App: "api", Trigger: "manual", Started: minute(1), Status: "succeeded"},
		{Job: "tick", App: "api", Trigger: "manual"},
	} {
		if _, err := s.StartJobRun(ctx, bad); err == nil || errors.Is(err, ErrConflict) {
			t.Errorf("accepted %+v: %v", bad, err)
		}
	}
	first, _ := s.StartJobRun(ctx, JobRun{Job: "tick", App: "api", Trigger: "manual", Started: minute(0)})
	if err := s.FinishJobRun(ctx, first, "succeeded", nil, ""); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < maxJobRuns+20; i++ {
		if _, err := s.StartJobRun(ctx, JobRun{Job: "tick", App: "api", Trigger: "schedule", Scheduled: minute(1).Add(time.Duration(i) * time.Minute), Started: minute(1), Finished: minute(1), Status: "skipped", Detail: "previous run still running"}); err != nil {
			t.Fatal(err)
		}
	}
	runs, err := s.JobRuns(ctx, "tick", 5000)
	if err != nil || len(runs) > maxJobRuns+1 {
		t.Fatalf("history not pruned: %d %v", len(runs), err)
	}
	if last, _ := s.LastJobRuns(ctx); last["tick"].ID != first {
		t.Fatalf("the last finished run was pruned: %+v", last)
	}
}

func TestUpgradeVersionFiveAddsJobRunsTable(t *testing.T) {
	s, dir, key := testStore(t)
	if _, err := s.db.Exec(`DROP TABLE job_runs; PRAGMA user_version=5`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	upgraded, err := Open(dir, key, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()
	var version int
	if err = upgraded.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 6 {
		t.Fatalf("migration: %d %v", version, err)
	}
	ctx := context.Background()
	if _, err = upgraded.StartJobRun(ctx, JobRun{Job: "tick", App: "api", Trigger: "schedule", Scheduled: minute(1), Started: minute(1)}); err != nil {
		t.Fatalf("job_runs table unusable after upgrade: %v", err)
	}
	if _, err = upgraded.StartJobRun(ctx, JobRun{Job: "tick", App: "api", Trigger: "schedule", Scheduled: minute(1), Started: minute(1)}); !errors.Is(err, ErrConflict) {
		t.Fatalf("occurrence index missing after upgrade: %v", err)
	}
}

func TestSetJobRunContainerRecordsWhereARunWent(t *testing.T) {
	s, _, _ := testStore(t)
	ctx := context.Background()
	id, err := s.StartJobRun(ctx, JobRun{Job: "tick", App: "api", Trigger: "manual", Started: minute(1)})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SetJobRunContainer(ctx, id, "abc123"); err != nil {
		t.Fatal(err)
	}
	if runs, _ := s.JobRuns(ctx, "tick", 1); runs[0].Container != "abc123" {
		t.Fatalf("container: %+v", runs[0])
	}
	if err = s.SetJobRunContainer(ctx, id+1, "x"); !errors.Is(err, ErrConflict) {
		t.Fatalf("unknown run: %v", err)
	}
}
