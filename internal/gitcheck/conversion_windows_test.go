//go:build windows

package gitcheck

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"testing/synctest"
)

func TestConversionJobNested(t *testing.T) {
	if len(os.Args) > 1 && os.Args[len(os.Args)-1] == "nested-job" {
		cmd := exec.CommandContext(context.Background(), "git", "--version")
		if err := runConversion(context.Background(), cmd); err != nil {
			t.Fatal(err)
		}
		return
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(context.Background(), exe, "-test.run=^TestConversionJobNested$", "--", "nested-job")
	output := &strings.Builder{}
	cmd.Stdout, cmd.Stderr = output, output
	if err := runConversion(context.Background(), cmd); err != nil {
		t.Fatalf("nested job: %v: %s", err, output.String())
	}
}

func TestConversionJobStartErrors(t *testing.T) {
	ctx := context.Background()
	if err := runConversion(ctx, exec.CommandContext(ctx, "this-git-command-does-not-exist-berthy")); err == nil {
		t.Fatal("missing command succeeded")
	}
	ctx, cancel := context.WithCancel(ctx)
	cancel()
	if err := runConversion(ctx, exec.CommandContext(ctx, os.Args[0])); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled start: %v", err)
	}
}

func TestJobDrainUsesAccounting(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
		defer cancel()
		calls := 0
		err := waitJobDrain(ctx, func() (uint32, error) {
			calls++
			if calls == 1 {
				return 1, nil
			}
			return 0, nil
		})
		if err != nil || calls != 2 {
			t.Fatalf("drain: calls %d, error %v", calls, err)
		}
	})
}

func TestJobDrainAccountingTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
		defer cancel()
		if err := waitJobDrain(ctx, func() (uint32, error) { return 1, nil }); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("drain: %v", err)
		}
	})
}

func TestCancelConversionFallsBack(t *testing.T) {
	failure := errors.New("job termination failed")
	killed := false
	err := cancelConversion(func() error { return failure }, func() error { killed = true; return nil })
	if !killed || !errors.Is(err, failure) {
		t.Fatalf("kill %v, err %v", killed, err)
	}
}

func TestJobDrainEmptyAndQueryError(t *testing.T) {
	calls := 0
	if err := waitJobDrain(context.Background(), func() (uint32, error) { calls++; return 0, nil }); err != nil || calls != 1 {
		t.Fatalf("empty job: calls %d, error %v", calls, err)
	}
	failure := errors.New("accounting unavailable")
	if err := waitJobDrain(context.Background(), func() (uint32, error) { return 0, failure }); !errors.Is(err, failure) {
		t.Fatalf("accounting error: %v", err)
	}
}

func TestConversionWaitBoundsStuckWorker(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		worker := make(chan error)
		defer close(worker)
		result := make(chan error, 1)
		go func() { result <- awaitConversion(ctx, worker) }()
		cancel()
		deadline, stop := context.WithTimeout(context.Background(), 2*gitTimeout)
		defer stop()
		select {
		case err := <-result:
			if !errors.Is(err, errJobDrain) || !errors.Is(err, context.Canceled) {
				t.Fatalf("stuck worker: %v", err)
			}
		case <-deadline.Done():
			t.Fatal("cancelled conversion did not return within cleanup budget")
		}
	})
}

func TestConversionWaitPreservesWorkerErrorAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	failure := errors.New("worker failed")
	worker := make(chan error, 1)
	worker <- failure
	err := awaitConversion(ctx, worker)
	if !errors.Is(err, context.Canceled) || !errors.Is(err, failure) || errors.Is(err, errJobDrain) {
		t.Fatalf("finished worker: %v", err)
	}
}
