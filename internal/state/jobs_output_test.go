package state

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFinishJobRunKeepsTheOutputOfAFailedRunSealed(t *testing.T) {
	s, dir, key := testStore(t)
	ctx := context.Background()
	failed, err := s.StartJobRun(ctx, JobRun{Job: "tick", App: "api", Trigger: "manual", Started: minute(1)})
	if err != nil {
		t.Fatal(err)
	}
	three := 3
	if err = s.FinishJobRunWithOutput(ctx, failed, "failed", &three, "exit status 3", "boom: the-secret-line\n"); err != nil {
		t.Fatal(err)
	}
	ok, err := s.StartJobRun(ctx, JobRun{Job: "tick", App: "api", Trigger: "manual", Started: minute(2)})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.FinishJobRun(ctx, ok, "succeeded", nil, ""); err != nil {
		t.Fatal(err)
	}
	if runs, err := s.JobRuns(ctx, "tick", 10); err != nil || len(runs) != 2 || runs[0].HasOutput || !runs[1].HasOutput {
		t.Fatalf("which runs have output: %+v %v", runs, err)
	}
	// With no id, the newest run that kept output answers, though a newer run succeeded.
	run, out, err := s.JobOutput(ctx, "tick", 0)
	if err != nil || run.ID != failed || run.Status != "failed" || out != "boom: the-secret-line\n" {
		t.Fatalf("newest output: %+v %q %v", run, out, err)
	}
	run, out, err = s.JobOutput(ctx, "tick", ok)
	if err != nil || run.ID != ok || out != "" {
		t.Fatalf("a successful run keeps nothing: %+v %q %v", run, out, err)
	}
	if _, _, err = s.JobOutput(ctx, "other", failed); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("another job's run: %v", err)
	}
	if _, _, err = s.JobOutput(ctx, "tick", failed+100); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("unknown run: %v", err)
	}
	if _, _, err = s.JobOutput(ctx, "none", 0); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("a job without output: %v", err)
	}
	s.Close()
	// The output is sealed: the file holds no plain text of it.
	raw, err := os.ReadFile(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("the-secret-line")) {
		t.Fatal("the output is stored as plain text")
	}
	reopened, err := Open(dir, key, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if _, out, err = reopened.JobOutput(ctx, "tick", failed); err != nil || out != "boom: the-secret-line\n" {
		t.Fatalf("after reopening: %q %v", out, err)
	}
}

func TestFinishJobRunKeepsTheLastBytesOfALongOutput(t *testing.T) {
	s, _, _ := testStore(t)
	ctx := context.Background()
	id, err := s.StartJobRun(ctx, JobRun{Job: "tick", App: "api", Trigger: "manual", Started: minute(1)})
	if err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("a", MaxJobOutput) + "the end"
	if err = s.FinishJobRunWithOutput(ctx, id, "failed", nil, "timed out after 5s", long); err != nil {
		t.Fatal(err)
	}
	_, out, err := s.JobOutput(ctx, "tick", id)
	if err != nil || len(out) != MaxJobOutput || !strings.HasSuffix(out, "the end") {
		t.Fatalf("output of %d bytes: %v", len(out), err)
	}
}

func TestUpgradeVersionSixteenAddsTheOutputOfAJobRun(t *testing.T) {
	s, dir, key := testStore(t)
	if _, err := s.db.Exec(`ALTER TABLE job_runs DROP COLUMN output_sealed; PRAGMA user_version=16`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	upgraded, err := Open(dir, key, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()
	ctx := context.Background()
	id, err := upgraded.StartJobRun(ctx, JobRun{Job: "tick", App: "api", Trigger: "manual", Started: minute(1)})
	if err != nil {
		t.Fatal(err)
	}
	if err = upgraded.FinishJobRunWithOutput(ctx, id, "failed", nil, "exit status 1", "why"); err != nil {
		t.Fatalf("job_runs lacks the output column after the upgrade: %v", err)
	}
}
