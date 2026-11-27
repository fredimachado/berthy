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

func up(health string) composeObservation {
	return composeObservation{replica: api.Replica{State: "running", Health: health}}
}

func TestComposeVerdict(t *testing.T) {
	zero, three := 0, 3
	finished := composeObservation{replica: api.Replica{State: "exited", ExitCode: &zero}}
	tests := []struct {
		name     string
		services map[string][]composeObservation
		ready    bool
		pending  string
		failure  string
	}{
		{"no containers", nil, true, "", ""},
		{"running without a health check", map[string][]composeObservation{"web": {up("none")}}, true, "", ""},
		{"running and healthy", map[string][]composeObservation{"web": {up("healthy")}}, true, "", ""},
		{"a finished one-shot", map[string][]composeObservation{"job": {finished}, "web": {up("healthy")}}, true, "", ""},
		{"still starting", map[string][]composeObservation{"web": {up("starting")}}, false, "web", ""},
		{"created", map[string][]composeObservation{"web": {{replica: api.Replica{State: "created"}}}}, false, "web", ""},
		{"exit code unknown", map[string][]composeObservation{"job": {{incomplete: true}}}, false, "", ""},
		{"failure wins over incomplete", map[string][]composeObservation{"job": {{incomplete: true}}, "z": {{failure: "service z exited with code 3"}}}, false, "", "service z exited with code 3"},
		{"failure wins over pending", map[string][]composeObservation{"a": {up("starting")}, "z": {{failure: "service z exited with code 2"}}}, false, "", "service z exited with code 2"},
		{"first pending by name", map[string][]composeObservation{"b": {up("starting")}, "a": {up("starting")}}, false, "a", ""},
		{"first failure by name", map[string][]composeObservation{"z": {{failure: "service z is unhealthy"}}, "a": {{failure: "service a exited with code 3"}}}, false, "", "service a exited with code 3"},
		{"one of two replicas pending", map[string][]composeObservation{"web": {up("healthy"), up("starting")}}, false, "web", ""},
	}
	for _, tt := range tests {
		ready, pending, failure := composeVerdict(tt.services)
		if ready != tt.ready || pending != tt.pending {
			t.Errorf("%s: ready %v pending %q, want %v %q", tt.name, ready, pending, tt.ready, tt.pending)
		}
		if failure != tt.failure {
			t.Errorf("%s: failure %q, want %q", tt.name, failure, tt.failure)
		}
	}
	for _, tt := range []struct {
		name, psCode string
		inspectCode  *int
		want         error
	}{
		{"both exit codes missing", "", nil, errComposeUp},
		{"null exit codes", `,"ExitCode":null`, nil, errComposeUp},
		{"inspect supplies zero", "", &zero, nil},
		{"inspect supplies failure", "", &three, Refusal("service worker exited with code 3")},
		{"stale zero does not fill a missing result", `,"ExitCode":0`, nil, errComposeUp},
	} {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ps := `{"ID":"` + workerID + `","Service":"worker","State":"exited"` + tt.psCode + `}`
				docker := &composeWaitDocker{composeDocker: newComposeDocker(), inspect: func(id string) (engine.ContainerSnapshot, error) {
					return engine.ContainerSnapshot{ID: id, State: "exited", ExitCode: tt.inspectCode, RestartCount: &zero}, nil
				}}
				c := Controller{Compose: &fakeCompose{ps: []string{ps}}, Engine: docker, rolloutHold: zeroTestRolloutHold()}
				checkComposeWaitResult(t, c.waitCompose(t.Context(), "shop-db", t.TempDir(), time.Now(), time.Now().Add(time.Second), time.Second), tt.want)
			})
		})
	}
}

const (
	psBad  = `{"ID":"bad","ExitCode":3,"Health":"","Service":"bad","State":"exited","Status":"Exited (3) 3 seconds ago"}`
	psJob  = `{"ID":"job","ExitCode":0,"Health":"","Service":"job","State":"exited","Status":"Exited (0) 3 seconds ago"}`
	psWeb  = `{"ID":"web","ExitCode":0,"Health":"healthy","Service":"web","State":"running","Status":"Up 3 seconds (healthy)"}`
	psWeb0 = `{"ID":"web-starting","ExitCode":0,"Health":"starting","Service":"web","State":"running","Status":"Up 1 second (health: starting)"}`
	psWebU = `{"ID":"web-unhealthy","ExitCode":0,"Health":"unhealthy","Service":"web","State":"running","Status":"Up 5 seconds (unhealthy)"}`
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
		row := `{"ID":"` + workerID + `","Service":"job","State":"exited"` + tt.field + `}`
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
				if r.ContainerID != workerID {
					t.Errorf("container ID %q, want %q", r.ContainerID, workerID)
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
	for _, tt := range []struct{ name, row, want string }{
		{"exited", `{"ID":"job","ExitCode":1,"Service":"job","State":"exited"}`, "service job exited with code 1"},
		{"dead", `{"ID":"job","Service":"job","State":"dead"}`, "service job exited with code -1"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			runner := &fakeCompose{config: configOne, hashes: hashOne, ps: []string{tt.row + "\n" + psWeb}}
			c, state, err := composeOutcome(t, runner)
			if state != "failed" || err != tt.want {
				t.Fatalf("deploy %s: %q, want %q", state, err, tt.want)
			}
			if rel, e := c.Store.Applied(context.Background(), "db"); e == nil {
				t.Fatalf("release recorded for a failed deploy: %+v", rel)
			}
		})
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
		const jobRunning = `{"ID":"job-running","ExitCode":0,"Health":"","Service":"job","State":"running"}`
		const jobFailed = `{"ID":"job-running","ExitCode":3,"Health":"","Service":"job","State":"exited"}`
		runner := &fakeCompose{ps: []string{jobRunning + "\n" + psWeb, jobFailed + "\n" + psWeb}}
		c := Controller{Compose: runner, Engine: newComposeDocker()}
		checkComposeWaitResult(t, c.waitCompose(t.Context(), "shop-db", t.TempDir(), time.Now(), time.Now().Add(time.Minute), time.Minute),
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
		c := Controller{Compose: &fakeCompose{ps: []string{psWeb0}}, Engine: newComposeDocker()}
		start := time.Now()
		checkComposeWaitResult(t, c.waitCompose(t.Context(), "shop-db", t.TempDir(), start, start.Add(time.Second), time.Second),
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
		checkComposeWaitResult(t, c.waitCompose(t.Context(), "shop-db", t.TempDir(), time.Now(), time.Now().Add(time.Second), time.Second), errComposeUp)
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
	for _, s := range []string{"service job exited with code 1", "service web exited with code -1", "service web is unhealthy", "service web not ready after 120s", "service a.b-c_1 not ready after 1s", string(composeHoldRefusal), "service worker restarted (last exit code unavailable)"} {
		if !PublicMessage(s) {
			t.Errorf("%q is not public", s)
		}
	}
	for _, s := range []string{"service web exited with code 1234", "service web not ready after s", "service web is unhealthy; rm -rf", "service  is unhealthy",
		"service worker restarted (last exit code unavailable) detail", "service -worker restarted (last exit code unavailable)"} {
		if PublicMessage(s) {
			t.Errorf("%q is public", s)
		}
	}
}

const (
	workerID           = "bd1d07291e78b59bc4ebd04a5b7059c3c394111ed8f6022c35b2efef27aa70a6"
	psWorker           = `{"ID":"` + workerID + `","Service":"worker","State":"running"}`
	psWorkerFailed     = `{"ID":"` + workerID + `","Service":"worker","State":"exited","ExitCode":3}`
	psWorkerRestarting = `{"ID":"` + workerID + `","Service":"worker","State":"restarting","ExitCode":0}`
)

type composeWaitDocker struct {
	*composeDocker
	inspect func(string) (engine.ContainerSnapshot, error)
	history func(string, time.Time, time.Time) (int, bool, error)
}

func (d *composeWaitDocker) InspectContainer(_ context.Context, id string) (engine.ContainerSnapshot, error) {
	return d.inspect(id)
}

func (d *composeWaitDocker) LastContainerExitCode(_ context.Context, id string, since, until time.Time) (int, bool, error) {
	if d.history == nil {
		return 0, false, errors.New("history unavailable")
	}
	return d.history(id, since, until)
}

func TestComposeFinalInspectSeesOrdinaryExit(t *testing.T) {
	for _, tt := range []struct{ name, ps string }{
		{"inspect sees exit at boundary", psWorker},
		{"ps exit survives newer running state", psWorkerFailed},
	} {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				zero, three := 0, 3
				start := time.Now()
				docker := &composeWaitDocker{composeDocker: newComposeDocker(), inspect: func(id string) (engine.ContainerSnapshot, error) {
					if id != workerID {
						t.Errorf("inspecting %q, want %q", id, workerID)
					}
					if tt.ps == psWorker && time.Since(start) >= rolloutMonitor {
						return engine.ContainerSnapshot{ID: id, State: "exited", RestartCount: &zero, ExitCode: &three}, nil
					}
					return engine.ContainerSnapshot{ID: id, State: "running", Running: true, Health: "none", RestartCount: &zero}, nil
				}}
				c := Controller{Compose: &fakeCompose{ps: []string{tt.ps}}, Engine: docker}
				checkComposeWaitResult(t, c.waitCompose(t.Context(), "shop-db", t.TempDir(), start, start.Add(time.Minute), time.Minute),
					Refusal("service worker exited with code 3"))
				wantElapsed := time.Duration(0)
				if tt.ps == psWorker {
					wantElapsed = rolloutMonitor
				}
				if got := time.Since(start); got != wantElapsed {
					t.Errorf("verdict after %v, want %v", got, wantElapsed)
				}
			})
		})
	}
}

func TestComposeRestartingFailsOnFirstPoll(t *testing.T) {
	zero, three := 0, 3
	for _, tt := range []struct {
		name, ps, state string
		code            *int
		history         bool
		want            Refusal
	}{
		{"known code", psWorkerRestarting, "restarting", &three, false, "service worker exited with code 3"},
		{"known zero", psWorkerRestarting, "restarting", &zero, false, "service worker exited with code 0"},
		{"omitted inspect code", psWorkerRestarting, "restarting", nil, true, "service worker exited with code 17"},
		{"null inspect code", psWorker, "restarting", nil, true, "service worker exited with code 17"},
		{"running after ps saw restarting", psWorkerRestarting, "running", &zero, true, "service worker exited with code 17"},
		{"unavailable history", psWorkerRestarting, "restarting", nil, false, "service worker restarted (last exit code unavailable)"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				start := time.Now()
				upStarted := start.Add(-time.Second)
				historyCalls := 0
				docker := &composeWaitDocker{composeDocker: newComposeDocker(), inspect: func(id string) (engine.ContainerSnapshot, error) {
					return engine.ContainerSnapshot{ID: id, State: tt.state, Running: true, Health: "none", ExitCode: tt.code, RestartCount: &zero}, nil
				}, history: func(id string, since, until time.Time) (int, bool, error) {
					historyCalls++
					if id != workerID || !since.Equal(upStarted) || !until.Equal(start) {
						t.Errorf("history request for %q from %v to %v, want worker and current up window", id, since, until)
					}
					if tt.history {
						return 17, true, nil
					}
					return 0, false, errors.New("history unavailable")
				}}
				runner := &fakeCompose{ps: []string{tt.ps}}
				c := Controller{Compose: runner, Engine: docker}
				checkComposeWaitResult(t, c.waitCompose(t.Context(), "shop-db", t.TempDir(), upStarted, upStarted.Add(time.Minute), time.Minute), tt.want)
				if elapsed, polls := time.Since(start), runner.psRuns(); elapsed != 0 || polls != 1 {
					t.Errorf("restart verdict after %v and %d polls, want one immediate poll", elapsed, polls)
				}
				wantCalls := 0
				if tt.code == nil || tt.state == "running" {
					wantCalls = 1
				}
				if historyCalls != wantCalls {
					t.Errorf("history queried %d times, want %d", historyCalls, wantCalls)
				}
			})
		})
	}
}

func TestComposeRestartSinceUp(t *testing.T) {
	for _, tt := range []struct {
		name          string
		state         string
		start, finish time.Duration
		want          error
		wantHistory   bool
	}{
		{"running after restart", "running", 2 * time.Second, time.Second, Refusal("service worker exited with code 17"), true},
		{"retained running container", "running", -time.Second, -2 * time.Second, nil, false},
		{"retained exit after up", "exited", -time.Second, time.Second, nil, false},
		{"exited after restart", "exited", time.Second, 2 * time.Second, Refusal("service worker exited with code 0"), false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				start := time.Now()
				upStarted := start.Add(-3 * time.Second)
				zero, two := 0, 2
				finished := upStarted.Add(tt.finish)
				historyCalled := false
				docker := &composeWaitDocker{composeDocker: newComposeDocker(), inspect: func(id string) (engine.ContainerSnapshot, error) {
					return engine.ContainerSnapshot{ID: id, State: tt.state, Running: tt.state == "running", Health: "none", ExitCode: &zero,
						RestartCount: &two, StartedAt: upStarted.Add(tt.start).Format(time.RFC3339Nano), FinishedAt: &finished}, nil
				}, history: func(id string, since, until time.Time) (int, bool, error) {
					historyCalled = true
					if !since.Equal(upStarted) || !until.Equal(start) {
						t.Errorf("history from %v to %v, want %v to %v", since, until, upStarted, start)
					}
					code, found := map[string]int{workerID: 17, "other-worker-replica": 42}[id]
					return code, found, nil
				}}
				c := Controller{Compose: &fakeCompose{ps: []string{psWorker}}, Engine: docker}
				checkComposeWaitResult(t, c.waitCompose(t.Context(), "shop-db", t.TempDir(), upStarted, upStarted.Add(time.Minute), time.Minute), tt.want)
				if historyCalled != tt.wantHistory {
					t.Errorf("history queried: %v, want %v", historyCalled, tt.wantHistory)
				}
			})
		})
	}
}

func TestComposeIncompleteRestartObservation(t *testing.T) {
	t.Run("unavailable inspect restarts hold", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			paceWith(t, time.Second, time.Hour, 0)
			zero := 0
			start := time.Now()
			docker := &composeWaitDocker{composeDocker: newComposeDocker(), inspect: func(id string) (engine.ContainerSnapshot, error) {
				if time.Since(start) == time.Second {
					return engine.ContainerSnapshot{}, errors.New("inspect unavailable")
				}
				return engine.ContainerSnapshot{ID: id, State: "running", Running: true, Health: "none", RestartCount: &zero}, nil
			}}
			c := Controller{Compose: &fakeCompose{ps: []string{psWorker}}, Engine: docker}
			checkComposeWaitResult(t, c.waitCompose(t.Context(), "shop-db", t.TempDir(), start, start.Add(time.Minute), time.Minute), nil)
			if got, want := time.Since(start), rolloutMonitor+2*time.Second; got != want {
				t.Errorf("confirmed readiness after %v, want %v after inspect gap", got, want)
			}
		})
	})
	t.Run("sibling failure wins", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			docker := &composeWaitDocker{composeDocker: newComposeDocker(), inspect: func(string) (engine.ContainerSnapshot, error) {
				return engine.ContainerSnapshot{}, errors.New("inspect unavailable")
			}}
			const siblingFailed = `{"ID":"sibling","Service":"z","State":"exited","ExitCode":3}`
			c := Controller{Compose: &fakeCompose{ps: []string{psWorker + "\n" + siblingFailed}}, Engine: docker}
			checkComposeWaitResult(t, c.waitCompose(t.Context(), "shop-db", t.TempDir(), time.Now(), time.Now().Add(time.Minute), time.Minute),
				Refusal("service z exited with code 3"))
		})
	})
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
	dir, upStarted := t.TempDir(), time.Now()
	deadline := upStarted.Add(time.Minute)
	done := goWait(t, t.Context(), func(ctx context.Context) error {
		return c.waitCompose(ctx, "shop-db", dir, upStarted, deadline, time.Minute)
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
		const running = psWorker
		const failed = `{"ID":"` + workerID + `","Service":"worker","State":"exited","ExitCode":1}`
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
				c := Controller{Compose: &fakeCompose{ps: []string{tt.ps}}, Engine: newComposeDocker()}
				checkComposeWaitResult(t, c.waitCompose(t.Context(), "shop-db", t.TempDir(), time.Now(), time.Now().Add(time.Second), time.Second), composeHoldRefusal)
			})
		})
	}
	t.Run("up consumed budget", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			runner := &fakeCompose{}
			c := Controller{Compose: runner, Engine: newComposeDocker()}
			checkComposeWaitResult(t, c.waitCompose(t.Context(), "shop-db", t.TempDir(), time.Now().Add(-time.Second), time.Now(), time.Second), errComposeUp)
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
			c := Controller{Compose: runner, Engine: newComposeDocker()}
			checkComposeWaitResult(t, c.waitCompose(t.Context(), "shop-db", t.TempDir(), deadline.Add(-rolloutMonitor-time.Second), deadline, rolloutMonitor+time.Second), composeHoldRefusal)
			if !readExpired {
				t.Fatal("the deploy deadline did not bound the confirmation read")
			}
		})
	})
}
