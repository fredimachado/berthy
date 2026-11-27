package deploy

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"berthy/internal/api"
	"berthy/internal/engine"
)

func exited(code int) api.Replica {
	return api.Replica{State: "exited", Health: "none", ExitCode: &code}
}

func up(health string) api.Replica { return api.Replica{State: "running", Health: health} }

func TestComposeVerdict(t *testing.T) {
	tests := []struct {
		name         string
		services     map[string][]api.Replica
		ready        bool
		pending      string
		failure      string
		anyFailureOf string
	}{
		{"no containers", map[string][]api.Replica{}, true, "", "", ""},
		{"running without a health check", map[string][]api.Replica{"web": {up("none")}}, true, "", "", ""},
		{"running and healthy", map[string][]api.Replica{"web": {up("healthy")}}, true, "", "", ""},
		{"a finished one-shot", map[string][]api.Replica{"job": {exited(0)}, "web": {up("healthy")}}, true, "", "", ""},
		{"still starting", map[string][]api.Replica{"web": {up("starting")}}, false, "web", "", ""},
		{"created", map[string][]api.Replica{"web": {{State: "created", Health: "none"}}}, false, "web", "", ""},
		{"restarting", map[string][]api.Replica{"web": {{State: "restarting", Health: "none"}}}, false, "web", "", ""},
		{"one-shot failed", map[string][]api.Replica{"job": {exited(1)}, "web": {up("healthy")}}, false, "", "service job exited with code 1", ""},
		{"dead", map[string][]api.Replica{"web": {{State: "dead", Health: "none"}}}, false, "", "", "service web exited with code"},
		{"unhealthy", map[string][]api.Replica{"web": {up("unhealthy")}}, false, "", "service web is unhealthy", ""},
		{"a failure wins over pending", map[string][]api.Replica{"a": {up("starting")}, "z": {exited(2)}}, false, "", "service z exited with code 2", ""},
		{"the first pending by name", map[string][]api.Replica{"b": {up("starting")}, "a": {up("starting")}}, false, "a", "", ""},
		{"one of two replicas pending", map[string][]api.Replica{"web": {up("healthy"), up("starting")}}, false, "web", "", ""},
	}
	for _, tt := range tests {
		ready, pending, failure := composeVerdict(tt.services)
		if ready != tt.ready || pending != tt.pending {
			t.Errorf("%s: ready %v pending %q, want %v %q", tt.name, ready, pending, tt.ready, tt.pending)
		}
		if tt.anyFailureOf != "" {
			if !strings.HasPrefix(failure, tt.anyFailureOf) {
				t.Errorf("%s: failure %q, want prefix %q", tt.name, failure, tt.anyFailureOf)
			}
		} else if failure != tt.failure {
			t.Errorf("%s: failure %q, want %q", tt.name, failure, tt.failure)
		}
	}
}

const (
	psBad  = `{"ExitCode":3,"Health":"","Service":"bad","State":"exited","Status":"Exited (3) 3 seconds ago"}`
	psJob  = `{"ExitCode":0,"Health":"","Service":"job","State":"exited","Status":"Exited (0) 3 seconds ago"}`
	psWeb  = `{"ExitCode":0,"Health":"healthy","Service":"web","State":"running","Status":"Up 3 seconds (healthy)"}`
	psWeb0 = `{"ExitCode":0,"Health":"starting","Service":"web","State":"running","Status":"Up 1 second (health: starting)"}`
	psWebU = `{"ExitCode":0,"Health":"unhealthy","Service":"web","State":"running","Status":"Up 5 seconds (unhealthy)"}`
)

func TestParseComposePS(t *testing.T) {
	check := func(name string, got map[string][]api.Replica) {
		t.Helper()
		if len(got) != 3 || len(got["bad"]) != 1 || len(got["job"]) != 1 || len(got["web"]) != 1 {
			t.Fatalf("%s: %+v", name, got)
		}
		bad, job, web := got["bad"][0], got["job"][0], got["web"][0]
		if bad.State != "exited" || bad.ExitCode == nil || *bad.ExitCode != 3 || bad.Health != "none" {
			t.Errorf("%s: bad %+v", name, bad)
		}
		if !job.FinishedOneShot() {
			t.Errorf("%s: job %+v is not a finished one-shot", name, job)
		}
		if web.State != "running" || web.Health != "healthy" || web.ExitCode != nil {
			t.Errorf("%s: web %+v", name, web)
		}
	}
	got, err := parseComposePS([]byte(psBad + "\n" + psJob + "\n" + psWeb + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	check("lines", got)
	got, err = parseComposePS([]byte("[" + psBad + "," + psJob + "," + psWeb + "]"))
	if err != nil {
		t.Fatal(err)
	}
	check("array", got)
	for _, blank := range []string{"", "\n", "  \n\n"} {
		if got, err = parseComposePS([]byte(blank)); err != nil || len(got) != 0 {
			t.Errorf("blank %q: %v %v", blank, got, err)
		}
	}
	for _, bad := range []string{"not json", psWeb + "\nnot json", `{"State":"running"}`, `[{"State":"running"}]`} {
		if _, err = parseComposePS([]byte(bad)); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
	zero, three := 0, 3
	for _, tt := range []struct {
		name, field string
		want        *int
	}{
		{"omitted", "", nil},
		{"null", `,"ExitCode":null`, nil},
		{"zero", `,"ExitCode":0`, &zero},
		{"three", `,"ExitCode":3`, &three},
	} {
		const id = "bd1d07291e78b59bc4ebd04a5b7059c3c394111ed8f6022c35b2efef27aa70a6"
		row := `{"ID":"` + id + `","Service":"job","State":"exited"` + tt.field + `}`
		for name, input := range map[string]string{"lines": row + "\n", "array": "[" + row + "]"} {
			t.Run(tt.name+"/"+name, func(t *testing.T) {
				got, err := parseComposePS([]byte(input))
				if err != nil || len(got["job"]) != 1 {
					t.Fatalf("got %+v, %v", got, err)
				}
				r := got["job"][0]
				if r.ExitCode == nil && tt.want != nil || r.ExitCode != nil && (tt.want == nil || *r.ExitCode != *tt.want) {
					t.Errorf("unexpected exit code in %+v", r)
				}
				if r.ContainerID != id {
					t.Errorf("container ID %q, want %q", r.ContainerID, id)
				}
			})
		}
	}
}

// composeOutcome deploys composeFileOne with the polls of ps the test gives and returns the finished operation.
func composeOutcome(t *testing.T, runner *fakeCompose) (*Controller, string, string) {
	t.Helper()
	c, s := composeController(t, runner, newComposeDocker())
	op, _, err := admit(t, c, context.Background(), Actor{ID: "ci"}, composeReq("wait", composeFileOne, composeApp(false)))
	if err != nil {
		t.Fatal(err)
	}
	got := waitComposeOperation(t, s, op.ID)
	if got.State == "succeeded" {
		if _, err := s.Applied(context.Background(), "db"); err != nil {
			t.Fatalf("no release recorded: %v", err)
		}
	}
	return c, got.State, got.Err
}

func TestComposeDeploySucceedsWithFinishedOneShot(t *testing.T) {
	runner := &fakeCompose{config: configOne, hashes: hashOne, ps: []string{psJob + "\n" + psWeb}}
	_, state, err := composeOutcome(t, runner)
	if state != "succeeded" {
		t.Fatalf("deploy %s: %s", state, err)
	}
	if got := strings.Join(runner.upArgs, " "); got != "up -d --remove-orphans" {
		t.Fatalf("up args %q", got)
	}
	if runner.psRuns() == 0 {
		t.Fatal("no ps poll judged the containers")
	}
}

func TestComposeDeployFailsWhenOneShotFails(t *testing.T) {
	runner := &fakeCompose{config: configOne, hashes: hashOne, ps: []string{`{"ExitCode":1,"Health":"","Service":"job","State":"exited"}` + "\n" + psWeb}}
	c, state, err := composeOutcome(t, runner)
	if state != "failed" || err != "service job exited with code 1" {
		t.Fatalf("deploy %s: %q", state, err)
	}
	if rel, e := c.Store.Applied(context.Background(), "db"); e == nil {
		t.Fatalf("release recorded for a failed deploy: %+v", rel)
	}
}

func TestComposeDeployFailsWhenUnhealthy(t *testing.T) {
	runner := &fakeCompose{config: configOne, hashes: hashOne, ps: []string{psWebU}}
	_, state, err := composeOutcome(t, runner)
	if state != "failed" || err != "service web is unhealthy" {
		t.Fatalf("deploy %s: %q", state, err)
	}
}

func TestComposeDeployReadsAgainBeforeCallingAProjectReady(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const jobRunning = `{"ExitCode":0,"Health":"","Service":"job","State":"running"}`
		const jobFailed = `{"ExitCode":3,"Health":"","Service":"job","State":"exited"}`
		runner := &fakeCompose{ps: []string{jobRunning + "\n" + psWeb, jobFailed + "\n" + psWeb}}
		c := Controller{Compose: runner}
		checkComposeWaitResult(t, c.waitCompose(t.Context(), "shop-db", t.TempDir(), time.Now().Add(time.Minute), time.Minute),
			Refusal("service job exited with code 3"))
	})
}

func TestComposeDeployWaitsForStartingService(t *testing.T) {
	runner := &fakeCompose{config: configOne, hashes: hashOne, ps: []string{psWeb0, psWeb}}
	_, state, err := composeOutcome(t, runner)
	if state != "succeeded" {
		t.Fatalf("deploy %s: %s", state, err)
	}
	if n := runner.psRuns(); n < 2 {
		t.Fatalf("%d ps polls, want at least 2", n)
	}
}

func TestComposeDeployTimesOut(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := Controller{Compose: &fakeCompose{ps: []string{psWeb0}}}
		start := time.Now()
		checkComposeWaitResult(t, c.waitCompose(t.Context(), "shop-db", t.TempDir(), start.Add(time.Second), time.Second),
			Refusal("service web not ready after 1s"))
		if took := time.Since(start); took != time.Second {
			t.Fatalf("the wait took %v for a 1s budget", took)
		}
	})
}

func TestComposeDeployRetriesAPSThatFails(t *testing.T) {
	runner := &fakeCompose{config: configOne, hashes: hashOne, ps: []string{psWeb}, psFails: 2}
	_, state, err := composeOutcome(t, runner)
	if state != "succeeded" || runner.psRuns() <= runner.psFails {
		t.Fatalf("deploy %s: %q after %d polls", state, err, runner.psRuns())
	}
}

func TestComposeDeployFailsWhenPSFailsUntilTheDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := Controller{Compose: &fakeCompose{ps: []string{psWeb}, psFails: 1 << 30}}
		checkComposeWaitResult(t, c.waitCompose(t.Context(), "shop-db", t.TempDir(), time.Now().Add(time.Second), time.Second), errComposeUp)
	})
}

func TestComposeDeployFailsWhenPSOutputIsGarbage(t *testing.T) {
	runner := &fakeCompose{config: configOne, hashes: hashOne, ps: []string{"not json"}}
	_, state, err := composeOutcome(t, runner)
	if state != "failed" || err != string(errComposeUp) {
		t.Fatalf("deploy %s: %q", state, err)
	}
}

const composeHoldRefusal = Refusal("Compose project did not stay ready long enough")

func TestComposeWaitMessagesArePublic(t *testing.T) {
	for _, s := range []string{"service job exited with code 1", "service web exited with code -1", "service web is unhealthy", "service web not ready after 120s", "service a.b-c_1 not ready after 1s", string(composeHoldRefusal)} {
		if !PublicMessage(s) {
			t.Errorf("%q is not public", s)
		}
	}
	for _, s := range []string{"service web exited with code 1234", "service web not ready after s", "service web is unhealthy; rm -rf", "service  is unhealthy"} {
		if PublicMessage(s) {
			t.Errorf("%q is public", s)
		}
	}
}

func TestProjectEventsMatchContainersOfTheProject(t *testing.T) {
	match := projectEvents("shop-db")
	mine := engine.Event{Type: "container", Action: "die", Attributes: map[string]string{"com.docker.compose.project": "shop-db"}}
	other := engine.Event{Type: "container", Action: "die", Attributes: map[string]string{"com.docker.compose.project": "other"}}
	resync := engine.Event{Type: engine.ResyncType, Action: engine.ResyncAction}
	if !match(mine) || !match(resync) || match(other) || match(engine.Event{Type: "container", Action: "die"}) {
		t.Fatal("the project match is wrong")
	}
}

func TestServiceEventsMatchTheServiceByIDOrLabel(t *testing.T) {
	match := serviceEvents("s1")
	byLabel := engine.Event{Type: "container", Attributes: map[string]string{"com.docker.swarm.service.id": "s1"}}
	byID := engine.Event{Type: "service", ID: "s1"}
	if !match(byLabel) || !match(byID) || match(engine.Event{Type: "service", ID: "s2"}) || match(engine.Event{Type: "container", ID: "s1"}) {
		t.Fatal("the service match is wrong")
	}
}

// evComposeDocker is a Compose engine that also hears Docker's events, with the feed driven by the test.
type evComposeDocker struct {
	*composeDocker
	events chan engine.Event
	mu     sync.Mutex
	match  func(engine.Event) bool
}

func (e *evComposeDocker) Watch(match func(engine.Event) bool) (<-chan engine.Event, func()) {
	e.mu.Lock()
	e.match = match
	e.mu.Unlock()
	return e.events, func() {}
}

func (e *evComposeDocker) EventsLive() bool { return true }

func (e *evComposeDocker) matcher() func(engine.Event) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.match
}

func startComposeWait(t *testing.T, runner *fakeCompose, hold *time.Duration) (*evComposeDocker, chan error) {
	t.Helper()
	// Outside the confirmation boundary, the live feed is the only way to prompt another poll within the budget.
	paceWith(t, 500*time.Millisecond, time.Hour, 0)
	docker := &evComposeDocker{composeDocker: newComposeDocker(), events: make(chan engine.Event, 4)}
	c := Controller{Engine: docker, Compose: runner, rolloutHold: hold}
	dir, deadline := t.TempDir(), time.Now().Add(time.Minute)
	done := goWait(t, t.Context(), func(ctx context.Context) error {
		return c.waitCompose(ctx, "shop-db", dir, deadline, time.Minute)
	})
	synctest.Wait()
	return docker, done
}

func sendComposeHealth(t *testing.T, docker *evComposeDocker) {
	t.Helper()
	ev := engine.Event{Type: "container", Action: "health_status", Attributes: map[string]string{"com.docker.compose.project": "shop-db"}}
	if match := docker.matcher(); match == nil || !match(ev) {
		t.Fatal("the wait ignores a health event of its project")
	}
	docker.events <- ev
	synctest.Wait()
}

func checkComposeWaitResult(t *testing.T, got, want error) {
	t.Helper()
	if got != want {
		t.Fatalf("wait returned %v, want %v", got, want)
	}
}

func requireComposeWaitResult(t *testing.T, done <-chan error, want error) {
	t.Helper()
	select {
	case err := <-done:
		checkComposeWaitResult(t, err, want)
	default:
		t.Fatalf("the wait has not returned, want %v", want)
	}
}

func requireComposeWaiting(t *testing.T, done <-chan error, when string) {
	t.Helper()
	select {
	case err := <-done:
		t.Fatalf("wait returned %v %s", err, when)
	default:
	}
}

func TestComposeWaitEndsAtAHealthEventOfTheProject(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runner := &fakeCompose{ps: []string{psWeb0, psWeb}}
		docker, done := startComposeWait(t, runner, zeroTestRolloutHold())
		requireComposeWaiting(t, done, "while the service was starting")
		sendComposeHealth(t, docker)
		requireComposeWaitResult(t, done, nil)
	})
}

func TestComposeRequiresFullReadyHold(t *testing.T) {
	t.Run("event before boundary", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			runner := &fakeCompose{ps: []string{psWeb}}
			docker, done := startComposeWait(t, runner, nil)
			time.Sleep(2 * time.Second)
			synctest.Wait()
			requireComposeWaiting(t, done, "before the readiness hold ended")
			polls := runner.psRuns()
			sendComposeHealth(t, docker)
			if runner.psRuns() <= polls {
				t.Fatal("the event did not wake a poll during the hold")
			}
			time.Sleep(rolloutMonitor - 2*time.Second - time.Millisecond)
			synctest.Wait()
			requireComposeWaiting(t, done, "before the readiness hold ended")
			polls = runner.psRuns()
			time.Sleep(time.Millisecond)
			synctest.Wait()
			requireComposeWaitResult(t, done, nil)
			if runner.psRuns() <= polls {
				t.Fatal("the hold ended without a fresh poll")
			}
		})
	})
	t.Run("read crossing boundary", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			release := make(chan struct{})
			runner := &fakeCompose{onPS: func(ctx context.Context, poll int) error {
				if poll == 2 {
					select {
					case <-release:
					case <-ctx.Done():
						return ctx.Err()
					}
				}
				return nil
			}}
			docker, done := startComposeWait(t, runner, nil)
			time.Sleep(rolloutMonitor - time.Second)
			sendComposeHealth(t, docker)
			time.Sleep(2 * time.Second)
			close(release)
			synctest.Wait()
			requireComposeWaitResult(t, done, nil)
			if runner.psRuns() <= 2 {
				t.Fatal("a poll begun before the boundary confirmed readiness when it finished afterward")
			}
		})
	})
}

func TestComposeReadyHoldRestartsAfterGap(t *testing.T) {
	for _, tt := range []struct {
		name string
		gap  string
		err  error
	}{
		{name: "pending service", gap: psWeb0},
		{name: "transient ps error", gap: psWeb, err: errors.New("compose could not run")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				runner := &fakeCompose{ps: []string{psWeb, psWeb, tt.gap, psWeb}, onPS: func(_ context.Context, poll int) error {
					if poll == 3 {
						return tt.err
					}
					return nil
				}}
				docker, done := startComposeWait(t, runner, nil)
				time.Sleep(250 * time.Millisecond)
				sendComposeHealth(t, docker)
				time.Sleep(1750 * time.Millisecond)
				synctest.Wait()
				requireComposeWaiting(t, done, "before the gap in readiness")
				sendComposeHealth(t, docker)
				requireComposeWaiting(t, done, "after the gap interrupted the hold")
				polls := runner.psRuns()
				time.Sleep(rolloutMonitor - time.Second)
				synctest.Wait()
				if runner.psRuns() != polls {
					t.Fatal("the previous hold still scheduled a poll after the gap")
				}
				sendComposeHealth(t, docker)
				if runner.psRuns() <= polls {
					t.Fatal("the event did not wake a poll after recovery")
				}
				time.Sleep(rolloutMonitor - time.Millisecond)
				synctest.Wait()
				requireComposeWaiting(t, done, "before the replacement hold ended")
				time.Sleep(time.Millisecond)
				synctest.Wait()
				requireComposeWaitResult(t, done, nil)
			})
		})
	}
}

func TestComposeExitDuringReadyHold(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const running = `{"Service":"worker","State":"running"}`
		const failed = `{"Service":"worker","State":"exited","ExitCode":1}`
		runner := &fakeCompose{ps: []string{running, running, failed}}
		docker, done := startComposeWait(t, runner, nil)
		time.Sleep(time.Second)
		sendComposeHealth(t, docker)
		time.Sleep(2 * time.Second)
		sendComposeHealth(t, docker)
		requireComposeWaitResult(t, done, Refusal("service worker exited with code 1"))
	})
}

func TestComposeDeadlineCutsOffReadyHold(t *testing.T) {
	for _, tt := range []struct{ name, ps string }{{"ready project", psWeb}, {"empty project", ""}} {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				c := Controller{Compose: &fakeCompose{ps: []string{tt.ps}}}
				checkComposeWaitResult(t, c.waitCompose(t.Context(), "shop-db", t.TempDir(), time.Now().Add(time.Second), time.Second), composeHoldRefusal)
			})
		})
	}
	t.Run("up consumed budget", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			runner := &fakeCompose{}
			c := Controller{Compose: runner}
			checkComposeWaitResult(t, c.waitCompose(t.Context(), "shop-db", t.TempDir(), time.Now(), time.Second), errComposeUp)
			if runner.psRuns() != 0 {
				t.Fatal("started a readiness read after the budget was spent")
			}
		})
	})
	t.Run("late confirmation", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			paceWith(t, time.Hour, time.Hour, 0)
			var readExpired bool
			deadline := time.Now().Add(rolloutMonitor + time.Second)
			runner := &fakeCompose{onPS: func(ctx context.Context, poll int) error {
				if poll == 2 {
					// Model a read that returns valid output despite the deadline passing in flight.
					time.Sleep(2 * time.Second)
					readExpired = ctx.Err() == context.DeadlineExceeded
				}
				return nil
			}}
			c := Controller{Compose: runner}
			checkComposeWaitResult(t, c.waitCompose(t.Context(), "shop-db", t.TempDir(), deadline, rolloutMonitor+time.Second), composeHoldRefusal)
			if !readExpired {
				t.Fatal("the deploy deadline did not bound the confirmation read")
			}
		})
	})
}
