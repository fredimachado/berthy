package deploy

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"berthy/internal/engine"
)

type uptimeHealthSequence struct{ *seqHealth }

func (uptimeHealthSequence) InspectContainerHealth(context.Context, string) (string, error) {
	return "none", nil
}

func TestUptimeRolloutCountsDistinctCleanExits(t *testing.T) {
	for _, terminal := range []struct {
		state string
		code  int
	}{{"complete", 0}, {"failed", 1}} {
		t.Run(terminal.state, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				first := failedTask("first", terminal.state, "op")
				second := failedTask("second", terminal.state, "op")
				first.ExitCode, second.ExitCode = &terminal.code, &terminal.code
				second.DesiredState = "running"
				old := failedTask("old", "complete", "prior")
				shutdown := failedTask("stopped", "shutdown", "op")
				foreign := failedTask("foreign", "complete", "op")
				foreign.ServiceID = "other"
				running := runningTask("healthy", "op")
				running.RunningSince = time.Now().Add(-time.Second)
				e := uptimeHealthSequence{&seqHealth{step: func(poll int) []engine.ServiceTask {
					switch poll {
					case 1, 2:
						return []engine.ServiceTask{running, first, old, shutdown, foreign}
					case 3:
						return []engine.ServiceTask{running}
					default:
						return []engine.ServiceTask{running, second}
					}
				}}}
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				_, err := waitHealthy(ctx, e, "s", "op", 1, startBudget{limit: 2, uptime: time.Second}, rolloutTiming{hold: rolloutMonitor})
				var gaveUp *errStartAttempts
				if !errors.As(err, &gaveUp) || gaveUp.Failed != 2 || gaveUp.Limit != 2 || e.polls != 4 {
					t.Fatalf("error %v after %d polls, want two distinct failed starts at poll 4", err, e.polls)
				}
			})
		})
	}
}

func TestUptimeRolloutCanRecoverBelowLimit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const hold = 5 * time.Second
		const threshold = 2 * time.Second
		var exitObserved time.Time
		initial := runningTask("initial", "op")
		initial.RunningSince = time.Now().Add(-threshold)
		e := uptimeHealthSequence{&seqHealth{step: func(poll int) []engine.ServiceTask {
			if poll < 3 {
				return []engine.ServiceTask{initial}
			}
			if exitObserved.IsZero() {
				exitObserved = time.Now()
			}
			replacement := runningTask("replacement", "op")
			replacement.RunningSince = exitObserved
			return []engine.ServiceTask{failedTask("initial", "complete", "op"), replacement}
		}}}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := waitHealthy(ctx, e, "s", "op", 1, startBudget{limit: 2, uptime: threshold}, rolloutTiming{hold: hold}); err != nil {
			t.Fatal(err)
		}
		if exitObserved.IsZero() || time.Since(exitObserved) != threshold+hold {
			t.Fatalf("confirmed after %s from the clean exit, want %s", time.Since(exitObserved), threshold+hold)
		}
	})
}

func TestCommandHealthRetainsExistingCompletionPolicy(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		running := runningTask("healthy", "op")
		completed := []engine.ServiceTask{running, failedTask("one", "complete", "op"), failedTask("two", "complete", "op")}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := waitHealthy(ctx, healthFake{tasks: completed, status: "healthy"}, "s", "op", 1, startBudget{limit: 2}, rolloutTiming{hold: rolloutMonitor}); err != nil {
			t.Fatalf("command check counted clean exits: %v", err)
		}
		failed := append(completed, failedTask("three", "failed", "op"), failedTask("four", "rejected", "op"))
		_, err := waitHealthy(ctx, healthFake{tasks: failed, status: "healthy"}, "s", "op", 1, startBudget{limit: 2}, rolloutTiming{hold: rolloutMonitor})
		var gaveUp *errStartAttempts
		if !errors.As(err, &gaveUp) || gaveUp.Failed != 2 || gaveUp.Limit != 2 {
			t.Fatalf("error %v, want the failed and rejected tasks to exhaust the budget", err)
		}
	})
}

func TestUptimeWaitRequiresEveryReplicaToReachThreshold(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const threshold = 20 * time.Second
		now := time.Now()
		older, younger := runningTask("older", "op"), runningTask("younger", "op")
		older.Slot, older.RunningSince = 2, now.Add(-threshold)
		younger.Slot, younger.RunningSince = 7, now.Add(-threshold+125*time.Millisecond)
		unknown := runningTask("unknown", "op")
		unknown.Slot, unknown.Timestamp = 7, older.RunningSince
		unidentified := older
		unidentified.Slot, unidentified.ID = 7, ""
		old := older
		old.Slot, old.Labels = 7, map[string]string{"berthy.operation": "previous"}
		probe := healthFake{tasks: []engine.ServiceTask{older, younger, unknown, unidentified, old}, status: "none"}
		ctx, cancel := context.WithTimeout(context.Background(), threshold)
		defer cancel()
		done := make(chan error, 1)
		go func() {
			_, err := waitHealthy(ctx, probe, "s", "op", 2, startBudget{uptime: threshold}, rolloutTiming{})
			done <- err
		}()
		defer func() { cancel(); <-done }()
		synctest.Wait()
		if probeErr := ctx.Err(); probeErr != nil || !time.Now().Equal(now) {
			t.Fatalf("initial poll moved the clock: %v", probeErr)
		}
		select {
		case err := <-done:
			done <- err
			t.Fatalf("wait finished before the younger task qualified: %v", err)
		default:
		}
		err := <-done
		done <- err
		if err != nil || time.Since(now) != 125*time.Millisecond {
			t.Fatalf("wait after %s: %v, want success at the younger task's threshold", time.Since(now), err)
		}
		// The same tasks cannot satisfy a command role without healthy Docker evidence.
		commandCtx, stop := context.WithTimeout(context.Background(), time.Second)
		defer stop()
		if _, err := waitHealthy(commandCtx, probe, "s", "op", 2, startBudget{}, rolloutTiming{}); err == nil {
			t.Fatal("command role accepted running age without Docker health")
		}
	})
}
