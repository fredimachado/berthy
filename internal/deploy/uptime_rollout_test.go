package deploy

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"berthy/internal/engine"
)

func TestUptimeRolloutCountsDistinctCleanExits(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		first := failedTask("first", "complete", "op")
		second := failedTask("second", "complete", "op")
		second.DesiredState = "running"
		old := failedTask("old", "complete", "prior")
		shutdown := failedTask("stopped", "shutdown", "op")
		foreign := failedTask("foreign", "complete", "op")
		foreign.ServiceID = "other"
		running := runningTask("healthy", "op")
		e := &seqHealth{step: func(poll int) []engine.ServiceTask {
			switch poll {
			case 1, 2:
				return []engine.ServiceTask{running, first, old, shutdown, foreign}
			case 3:
				return []engine.ServiceTask{running}
			default:
				return []engine.ServiceTask{running, second}
			}
		}}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, err := waitHealthy(ctx, e, "s", "op", 1, startBudget{limit: 2, countComplete: true}, rolloutTiming{hold: rolloutMonitor})
		var gaveUp *errStartAttempts
		if !errors.As(err, &gaveUp) || gaveUp.Failed != 2 || gaveUp.Limit != 2 || e.polls != 4 {
			t.Fatalf("error %v after %d polls, want two distinct failed starts at poll 4", err, e.polls)
		}
	})
}

func TestUptimeRolloutCanRecoverBelowLimit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const hold = 5 * time.Second
		var exitObserved time.Time
		e := &seqHealth{step: func(poll int) []engine.ServiceTask {
			if poll < 3 {
				return []engine.ServiceTask{runningTask("initial", "op")}
			}
			if exitObserved.IsZero() {
				exitObserved = time.Now()
			}
			return []engine.ServiceTask{failedTask("initial", "complete", "op"), runningTask("replacement", "op")}
		}}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := waitHealthy(ctx, e, "s", "op", 1, startBudget{limit: 2, countComplete: true}, rolloutTiming{hold: hold}); err != nil {
			t.Fatal(err)
		}
		if exitObserved.IsZero() || time.Since(exitObserved) != hold {
			t.Fatalf("confirmed after %s from the clean exit, want %s", time.Since(exitObserved), hold)
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
