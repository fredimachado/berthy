package jobs

import (
	"context"
	"errors"
	"testing"
	"time"

	"berthy/internal/manifest"
)

// blockingJob is a Runner that runs until its context ends, and says so on started. The output it leaves is what a stopped
// container would still hold.
func blockingJob(started chan<- struct{}) Runner {
	return runnerFunc(func(ctx context.Context, _ manifest.Job) (Outcome, error) {
		started <- struct{}{}
		<-ctx.Done()
		return Outcome{Container: "container-1", Output: "last lines\n"}, ctx.Err()
	})
}

func TestCancelStopsARunningJobAndRecordsWhoCancelledIt(t *testing.T) {
	onDemand := tickJob()
	onDemand.Schedule = ""
	r := newRig(t, map[string]manifest.Job{"slow": onDemand})
	started := make(chan struct{}, 1)
	r.sched.Runner = blockingJob(started)
	id, err := r.sched.Start(context.Background(), "slow", "manual")
	if err != nil {
		t.Fatal(err)
	}
	<-started
	if err = r.sched.Cancel(context.Background(), "slow", "tok"); err != nil {
		t.Fatal(err)
	}
	r.sched.wait()
	runs := r.runs(t, "slow")
	if len(runs) != 1 || runs[0].ID != id || runs[0].Status != "failed" || runs[0].Detail != "cancelled by tok" || runs[0].Container != "container-1" {
		t.Fatalf("runs: %+v", runs)
	}
	if _, out, err := r.store.JobOutput(context.Background(), "slow", id); err != nil || out != "last lines\n" {
		t.Fatalf("a cancelled run keeps the last lines it wrote: %q %v", out, err)
	}
	// The job can run again: the cancel did not leave it marked running.
	r.sched.Runner = r.exec
	if _, err = r.sched.Start(context.Background(), "slow", "manual"); err != nil {
		t.Fatalf("start again: %v", err)
	}
	r.sched.wait()
}

func TestCancelStopsAScheduledRunToo(t *testing.T) {
	r := newRig(t, map[string]manifest.Job{"tick": tickJob()})
	started := make(chan struct{}, 1)
	r.sched.Runner = blockingJob(started)
	r.sched.step(context.Background())
	<-started
	if err := r.sched.Cancel(context.Background(), "tick", "tok"); err != nil {
		t.Fatal(err)
	}
	r.sched.wait()
	if runs := r.runs(t, "tick"); len(runs) != 1 || runs[0].Status != "failed" || runs[0].Detail != "cancelled by tok" {
		t.Fatalf("runs: %+v", runs)
	}
}

func TestCancelOfAJobThatIsNotRunningOrNotKnown(t *testing.T) {
	r := newRig(t, map[string]manifest.Job{"tick": tickJob()})
	if err := r.sched.Cancel(context.Background(), "tick", "tok"); !errors.Is(err, ErrJobNotRunning) {
		t.Fatalf("a job that is not running: %v", err)
	}
	if err := r.sched.Cancel(context.Background(), "missing", "tok"); !errors.Is(err, ErrUnknownJob) {
		t.Fatalf("an unknown job: %v", err)
	}
}

// A run that ended on its own before the cancel took effect is recorded as it ended.
func TestACancelDoesNotRewriteARunThatSucceeded(t *testing.T) {
	onDemand := tickJob()
	onDemand.Schedule = ""
	r := newRig(t, map[string]manifest.Job{"slow": onDemand})
	started, release := make(chan struct{}, 1), make(chan struct{})
	r.sched.Runner = runnerFunc(func(ctx context.Context, _ manifest.Job) (Outcome, error) {
		started <- struct{}{}
		<-release
		zero := 0
		return Outcome{Exit: &zero}, nil
	})
	if _, err := r.sched.Start(context.Background(), "slow", "manual"); err != nil {
		t.Fatal(err)
	}
	<-started
	if err := r.sched.Cancel(context.Background(), "slow", "tok"); err != nil {
		t.Fatal(err)
	}
	close(release)
	r.sched.wait()
	if runs := r.runs(t, "slow"); len(runs) != 1 || runs[0].Status != "succeeded" {
		t.Fatalf("runs: %+v", runs)
	}
}

func TestCancelWaitsForNothingAndTheRunEndsPromptly(t *testing.T) {
	onDemand := tickJob()
	onDemand.Schedule = ""
	r := newRig(t, map[string]manifest.Job{"slow": onDemand})
	started := make(chan struct{}, 1)
	r.sched.Runner = blockingJob(started)
	if _, err := r.sched.Start(context.Background(), "slow", "manual"); err != nil {
		t.Fatal(err)
	}
	<-started
	begin := time.Now()
	if err := r.sched.Cancel(context.Background(), "slow", "tok"); err != nil {
		t.Fatal(err)
	}
	r.sched.wait()
	if time.Since(begin) > 5*time.Second {
		t.Fatal("the cancel took long")
	}
}
