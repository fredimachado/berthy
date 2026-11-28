package agent

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"berthy/internal/deploy"
	"berthy/internal/engine"
	"berthy/internal/manifest"
	"berthy/internal/monitor"
	"berthy/internal/state"
)

const (
	shopRelease = `{"project":"shop","network":"shop","apps":{"api":{"kind":"image","image":"ghcr.io/acme/api","roles":{"web":{}}},"db":{"kind":"compose","file":"compose.yml"}}}`
	oomEvent    = `{"Type":"container","Action":"oom","Actor":{"ID":"c1","Attributes":{"com.docker.compose.project":"shop-lab-db","com.docker.compose.service":"db","name":"shop-lab-db-db-1"}},"scope":"local","time":1795061398,"timeNano":1795061398000000000}`
	dieEvent    = `{"Type":"container","Action":"die","Actor":{"ID":"c1","Attributes":{"com.docker.compose.project":"shop-lab-db","exitCode":"137"}},"scope":"local","time":1795061399,"timeNano":1795061399000000000}`
)

func shopMonitoring(t *testing.T, d *downFake) (*Monitoring, *state.Store) {
	t.Helper()
	s := openIngressStore(t)
	recordBoundRelease(t, s, "rel-shop", shopRelease, []state.Binding{{App: "api", Role: "web", Name: "shop-api-web", ServiceID: "svc-1"}})
	return NewMonitoring(monitoringConfig(""), d.eng, nil, &deploy.Controller{Store: s, Environment: "lab"}, func(string) {}), s
}

// runHub runs the engine's event hub until the test ends and waits until the feed is open.
func runHub(t *testing.T, e *engine.Engine) {
	t.Helper()
	hub := engine.NewEventHub(e)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); hub.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	waitUntil(t, "the event feed to open", e.EventsLive)
}

func TestEventsMapToTheChecksTheyWake(t *testing.T) {
	cases := []struct {
		name string
		ev   engine.Event
		want []string
	}{
		{"a container dies", engine.Event{Type: "container", Action: "die"}, []string{"down:", "restarts:"}},
		{"a container runs out of memory", engine.Event{Type: "container", Action: "oom"}, []string{"down:", "restarts:"}},
		{"a container changes health", engine.Event{Type: "container", Action: "health_status", Detail: "unhealthy"}, []string{"down:", "restarts:"}},
		{"a service is updated", engine.Event{Type: "service", Action: "update"}, []string{"down:", "drift:", "restarts:"}},
		{"a service is removed", engine.Event{Type: "service", Action: "remove"}, []string{"down:", "drift:", "restarts:"}},
		{"the feed came back", engine.Event{Type: engine.ResyncType, Action: engine.ResyncAction}, []string{"down:", "drift:", "restarts:"}},
		{"a container starts", engine.Event{Type: "container", Action: "start"}, []string{"down:", "restarts:"}},
		{"a container restarts", engine.Event{Type: "container", Action: "restart"}, []string{"down:", "restarts:"}},
	}
	for _, c := range cases {
		got := eventPrefixes(c.ev)
		slices.Sort(got)
		if !slices.Equal(got, c.want) {
			t.Errorf("%s: wakes %v, want %v", c.name, got, c.want)
		}
	}
}

func TestInventoryIsReadWhenDueNotAtEveryRebuild(t *testing.T) {
	d := newDownFake(t)
	w, _ := shopMonitoring(t, d)
	now := time.Now()
	w.now = func() time.Time { return now }
	m := &manifest.Manifest{Project: "shop", Apps: map[string]manifest.App{"api": {Kind: "image"}, "db": {Kind: "compose"}}}
	rebuild := func(after time.Duration) []string {
		t.Helper()
		now = now.Add(after)
		var names []string
		for _, c := range w.appChecks(context.Background(), m) {
			names = append(names, c.Name())
		}
		slices.Sort(names)
		return names
	}
	first := rebuild(0)
	if d.containerReads() != 1 {
		t.Fatalf("the first rebuild read the inventory %d times", d.containerReads())
	}
	// Without an event feed the inventory is read every minute, as before.
	rebuild(30 * time.Second)
	if d.containerReads() != 1 {
		t.Fatalf("a rebuild 30s later read the inventory again: %d", d.containerReads())
	}
	rebuild(30 * time.Second)
	if d.containerReads() != 2 {
		t.Fatalf("without events the inventory was not read after a minute: %d", d.containerReads())
	}

	// With the feed open it is read every few minutes; the rebuilds between keep the same checks.
	runHub(t, d.eng)
	for i := 0; i < 5; i++ {
		if got := rebuild(30 * time.Second); !slices.Equal(got, first) {
			t.Fatalf("the checks changed between reads: %v, want %v", got, first)
		}
	}
	if d.containerReads() != 2 {
		t.Fatalf("with the feed open the inventory was read after %d reads", d.containerReads())
	}
	rebuild(30 * time.Second)
	if d.containerReads() != 3 {
		t.Fatalf("the inventory was not read after %v: %d reads", monitor.InventoryEvery, d.containerReads())
	}
	// A wake reads it at once, once.
	w.readNow.Store(true)
	rebuild(time.Second)
	rebuild(time.Second)
	if d.containerReads() != 4 {
		t.Fatalf("a wake gave %d reads, want 4", d.containerReads())
	}
}

func TestAFailedReadIsTriedAgainAtTheNextRebuild(t *testing.T) {
	d := newDownFake(t)
	w, _ := shopMonitoring(t, d)
	now := time.Now()
	w.now = func() time.Time { return now }
	m := &manifest.Manifest{Project: "shop", Apps: map[string]manifest.App{"api": {Kind: "image"}, "db": {Kind: "compose"}}}
	runHub(t, d.eng)
	w.appChecks(context.Background(), m)
	d.setDown(true)
	now = now.Add(monitor.InventoryEvery)
	w.appChecks(context.Background(), m)
	failed := d.containerReads()
	now = now.Add(time.Second)
	w.appChecks(context.Background(), m)
	if d.containerReads() != failed+1 {
		t.Fatalf("the read that failed was not tried again at the next rebuild: %d reads, %d before", d.containerReads(), failed)
	}
}

func TestAnOOMEventRaisesTheConditionWithoutWaitingForAPoll(t *testing.T) {
	d := newDownFake(t)
	w, s := shopMonitoring(t, d)
	runHub(t, d.eng)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); w.Run(ctx) }()
	defer func() { cancel(); <-done }()

	const check = "restarts:shop-lab-db/db"
	stored := func() state.CheckState {
		checks, _ := s.Checks(context.Background())
		for _, c := range checks {
			if c.Name == check {
				return c
			}
		}
		return state.CheckState{}
	}
	// The restart watch answers unknown until one RestartWindow has passed, so the first inventory is not ok.
	waitUntil(t, "the restart check to see inventory", func() bool {
		got := stored()
		return got.State == monitor.Unknown && got.Detail == "watching restarts since agent start"
	})

	d.setOOM(true)
	start := time.Now()
	d.push(oomEvent)
	waitUntil(t, "the restart check to report the out-of-memory kill", func() bool { return stored().State == monitor.Warning })
	if took := time.Since(start); took > 4*time.Second {
		t.Fatalf("the condition took %v to show; the periodic read is a minute away", took)
	}
	if got := stored().Detail; !strings.Contains(got, "OOM killed") {
		t.Fatalf("detail %q", got)
	}
}

func TestADieEventAsksForAReadOfTheInventory(t *testing.T) {
	d := newDownFake(t)
	w, _ := shopMonitoring(t, d)
	runHub(t, d.eng)
	ch, stop := d.eng.Watch(func(engine.Event) bool { return true })
	defer stop()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); w.followEvents(ctx, ch) }()
	defer func() { cancel(); <-done }()
	d.push(dieEvent)
	waitUntil(t, "the die event to ask for a read", w.readNow.Load)
}

type eventWakeCheck struct{ ran chan struct{} }

func (c eventWakeCheck) Name() string            { return "down:shop-lab-db/db" }
func (c eventWakeCheck) Interval() time.Duration { return time.Hour }
func (c eventWakeCheck) Failures() int           { return 1 }
func (c eventWakeCheck) Successes() int          { return 1 }
func (c eventWakeCheck) Run(context.Context) monitor.Result {
	c.ran <- struct{}{}
	return monitor.Result{State: monitor.OK}
}

func TestContainerStartsWakeDownCheckThroughHub(t *testing.T) {
	for _, action := range []string{"start", "restart"} {
		t.Run(action, func(t *testing.T) {
			d := newDownFake(t)
			w, s := shopMonitoring(t, d)
			runHub(t, d.eng)
			filters := <-d.filters
			if !slices.Contains(filters["event"], action) {
				t.Fatalf("Docker subscription omits %s: %v", action, filters)
			}
			ran := make(chan struct{}, 2)
			w.mon = &monitor.Monitor{Env: "lab", Store: s, Checks: func(context.Context) []monitor.Check { return []monitor.Check{eventWakeCheck{ran}} }}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() { defer close(done); w.Run(ctx) }()
			defer func() { cancel(); <-done }()
			select {
			case <-ran:
			case <-time.After(30 * time.Second):
				t.Fatal("initial check did not run")
			}
			d.push(`{"Type":"container","Action":"` + action + `","Actor":{"ID":"c1","Attributes":{"com.docker.compose.project":"shop-lab-db"}}}`)
			select {
			case <-ran:
			case <-time.After(30 * time.Second):
				t.Fatalf("%s did not wake the down check", action)
			}
		})
	}
}
