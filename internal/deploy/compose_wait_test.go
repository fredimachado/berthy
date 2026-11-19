package deploy

import (
	"context"
	"strings"
	"sync"
	"testing"
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
}

// composeOutcome deploys composeFileOne with the polls of ps the test gives and returns the finished operation.
func composeOutcome(t *testing.T, runner *fakeCompose, timeout string) (*Controller, string, string) {
	t.Helper()
	paceWith(t, 5*time.Millisecond, 5*time.Millisecond, 0)
	c, s := composeController(t, runner, newComposeDocker())
	m := composeApp(false)
	if timeout != "" {
		app := m.Apps["db"]
		app.Deploy.DeployTimeout = timeout
		m.Apps["db"] = app
	}
	op, _, err := admit(t, c, context.Background(), Actor{ID: "ci"}, composeReq("wait", composeFileOne, m))
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
	_, state, err := composeOutcome(t, runner, "")
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
	c, state, err := composeOutcome(t, runner, "")
	if state != "failed" || err != "service job exited with code 1" {
		t.Fatalf("deploy %s: %q", state, err)
	}
	if rel, e := c.Store.Applied(context.Background(), "db"); e == nil {
		t.Fatalf("release recorded for a failed deploy: %+v", rel)
	}
}

func TestComposeDeployFailsWhenUnhealthy(t *testing.T) {
	runner := &fakeCompose{config: configOne, hashes: hashOne, ps: []string{psWebU}}
	_, state, err := composeOutcome(t, runner, "")
	if state != "failed" || err != "service web is unhealthy" {
		t.Fatalf("deploy %s: %q", state, err)
	}
}

func TestComposeDeployWaitsForStartingService(t *testing.T) {
	runner := &fakeCompose{config: configOne, hashes: hashOne, ps: []string{psWeb0, psWeb}}
	_, state, err := composeOutcome(t, runner, "")
	if state != "succeeded" {
		t.Fatalf("deploy %s: %s", state, err)
	}
	if n := runner.psRuns(); n < 2 {
		t.Fatalf("%d ps polls, want at least 2", n)
	}
}

func TestComposeDeployTimesOut(t *testing.T) {
	runner := &fakeCompose{config: configOne, hashes: hashOne, ps: []string{psWeb0}}
	start := time.Now()
	_, state, err := composeOutcome(t, runner, "1s")
	if state != "failed" || err != "service web not ready after 1s" {
		t.Fatalf("deploy %s: %q", state, err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("the wait took %v for a 1s budget", took)
	}
}

func TestComposeDeployRetriesAPSThatFails(t *testing.T) {
	runner := &fakeCompose{config: configOne, hashes: hashOne, ps: []string{psWeb}, psFails: 2}
	_, state, err := composeOutcome(t, runner, "")
	if state != "succeeded" || runner.psRuns() != 3 {
		t.Fatalf("deploy %s: %q after %d polls", state, err, runner.psRuns())
	}
}

func TestComposeDeployFailsWhenPSFailsUntilTheDeadline(t *testing.T) {
	runner := &fakeCompose{config: configOne, hashes: hashOne, ps: []string{psWeb}, psFails: 1 << 30}
	_, state, err := composeOutcome(t, runner, "1s")
	if state != "failed" || err != string(errComposeUp) {
		t.Fatalf("deploy %s: %q", state, err)
	}
}

func TestComposeDeployFailsWhenPSOutputIsGarbage(t *testing.T) {
	runner := &fakeCompose{config: configOne, hashes: hashOne, ps: []string{"not json"}}
	_, state, err := composeOutcome(t, runner, "")
	if state != "failed" || err != string(errComposeUp) {
		t.Fatalf("deploy %s: %q", state, err)
	}
}

func TestComposeWaitMessagesArePublic(t *testing.T) {
	for _, s := range []string{"service job exited with code 1", "service web exited with code -1", "service web is unhealthy", "service web not ready after 120s", "service a.b-c_1 not ready after 1s"} {
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

func TestComposeWaitEndsAtAHealthEventOfTheProject(t *testing.T) {
	paceWith(t, time.Hour, time.Hour, 0)
	runner := &fakeCompose{config: configOne, hashes: hashOne, ps: []string{psWeb0, psWeb}}
	c, _ := composeController(t, runner, newComposeDocker())
	docker := &evComposeDocker{composeDocker: newComposeDocker(), events: make(chan engine.Event, 4)}
	c.Engine = docker
	done := make(chan error, 1)
	go func() {
		done <- c.waitCompose(context.Background(), "shop-db", t.TempDir(), time.Now().Add(time.Minute), time.Minute)
	}()
	deadline := time.After(2 * time.Second)
	for runner.psRuns() < 1 || docker.matcher() == nil {
		select {
		case <-deadline:
			t.Fatal("the wait did not poll")
		case <-time.After(time.Millisecond):
		}
	}
	ev := engine.Event{Type: "container", Action: "health_status", Attributes: map[string]string{"com.docker.compose.project": "shop-db"}}
	if !docker.matcher()(ev) {
		t.Fatal("the wait ignores a health event of its project")
	}
	docker.events <- ev
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the event did not wake the wait")
	}
}
