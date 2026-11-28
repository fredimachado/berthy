package monitor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"berthy/internal/state"
)

// openStore returns an empty encrypted state store for environment lab.
func openStore(t *testing.T) *state.Store {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(dir, "key")
	if err := os.WriteFile(key, make([]byte, 32), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := state.Open(dir, key, "lab")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// scripted is a check whose next result the test sets.
type scripted struct {
	name     string
	interval time.Duration
	failures int
	mu       sync.Mutex
	res      Result
	panics   bool
}

func (c *scripted) Name() string            { return c.name }
func (c *scripted) Interval() time.Duration { return c.interval }
func (c *scripted) Failures() int           { return c.failures }
func (c *scripted) Successes() int          { return 1 }
func (c *scripted) set(st, detail string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.res = Result{State: st, Detail: detail}
}
func (c *scripted) Run(context.Context) Result {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.panics {
		panic("broken check")
	}
	return c.res
}

// recorder is a notifier that keeps what it was asked to send. down makes every send fail.
type recorder struct {
	mu       sync.Mutex
	sent     []string
	alerts   []Alert
	attempts int
	down     bool
	// partial makes every send reach the recorder and still report a failure of another channel, as Notifiers does.
	partial bool
}

func (n *recorder) Notify(_ context.Context, a Alert) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.attempts++
	if n.down {
		return ErrTelegramUnavailable
	}
	n.sent = append(n.sent, a.Text)
	n.alerts = append(n.alerts, a)
	if n.partial {
		return &DeliveryError{Err: errors.New("webhook: webhook unavailable"), Delivered: 1}
	}
	return nil
}

func (n *recorder) messages() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]string(nil), n.sent...)
}

// rig drives a monitor with a fake clock. Each step runs every due check synchronously.
type rig struct {
	t        *testing.T
	m        *Monitor
	store    *state.Store
	now      time.Time
	checks   []Check
	notifier *recorder
	reminder time.Duration
	nilNote  bool
	logs     []string
}

func newRig(t *testing.T, store *state.Store) *rig {
	t.Helper()
	r := &rig{t: t, store: store, now: time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC), notifier: &recorder{}}
	r.attach()
	return r
}

// attach builds a monitor on the rig's store. A second call models a restart of the agent.
func (r *rig) attach() {
	r.m = &Monitor{
		Env:   "lab",
		Store: r.store,
		Now:   func() time.Time { return r.now },
		Checks: func(context.Context) []Check {
			return append([]Check(nil), r.checks...)
		},
		Notifier: func(context.Context) (Notifier, time.Duration, error) {
			if r.nilNote {
				return nil, 0, nil
			}
			return r.notifier, r.reminder, nil
		},
		Log: func(s string) { r.logs = append(r.logs, s) },
	}
}

// step moves the clock forward and runs what is due.
func (r *rig) step(d time.Duration) {
	r.now = r.now.Add(d)
	r.m.tick(context.Background(), r.now, func(run func()) { run() })
}

func (r *rig) stored(name string) state.CheckState {
	r.t.Helper()
	checks, err := r.store.Checks(context.Background())
	if err != nil {
		r.t.Fatal(err)
	}
	for _, c := range checks {
		if c.Name == name {
			return c
		}
	}
	r.t.Fatalf("no stored state for %s", name)
	return state.CheckState{}
}

func expectMessages(t *testing.T, got []string, want ...string) {
	t.Helper()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("messages:\n got: %q\nwant: %q", got, want)
	}
}

func TestThresholdEscalationAndRecovery(t *testing.T) {
	r := newRig(t, openStore(t))
	c := &scripted{name: "probe:site", interval: 30 * time.Second, failures: 3}
	r.checks = []Check{c}
	c.set(Warning, "HTTP 429")
	r.step(0)
	r.step(30 * time.Second)
	if got := r.stored("probe:site"); got.State != Unknown || got.Detail != "not checked yet" {
		t.Fatalf("state changed before the threshold: %+v", got)
	}
	r.step(30 * time.Second)
	if got := r.stored("probe:site"); got.State != Warning || got.NotifiedState != Warning || got.Detail != "HTTP 429" {
		t.Fatalf("threshold reached but state is %+v", got)
	}
	expectMessages(t, r.notifier.messages(), "🟠 lab probe:site: HTTP 429")

	// A critical result in the same streak takes the state at once: the streak has already met the threshold.
	c.set(Critical, "HTTP 503")
	r.step(30 * time.Second)
	expectMessages(t, r.notifier.messages(), "🟠 lab probe:site: HTTP 429", "🔴 lab probe:site: HTTP 503")
	r.step(30 * time.Second)
	expectMessages(t, r.notifier.messages(), "🟠 lab probe:site: HTTP 429", "🔴 lab probe:site: HTTP 503")

	c.set(OK, "HTTP 200")
	r.step(30 * time.Second)
	if got := r.stored("probe:site"); got.State != OK || got.NotifiedState != OK {
		t.Fatalf("recovery not recorded: %+v", got)
	}
	expectMessages(t, r.notifier.messages(), "🟠 lab probe:site: HTTP 429", "🔴 lab probe:site: HTTP 503", "✅ lab probe:site recovered")

	// After recovery a new failure needs the whole threshold again.
	c.set(Critical, "timeout")
	r.step(30 * time.Second)
	r.step(30 * time.Second)
	if got := r.stored("probe:site"); got.State != OK {
		t.Fatalf("failure below the threshold after recovery changed the state: %+v", got)
	}
	r.step(30 * time.Second)
	expectMessages(t, r.notifier.messages(), "🟠 lab probe:site: HTTP 429", "🔴 lab probe:site: HTTP 503", "✅ lab probe:site recovered", "🔴 lab probe:site: timeout")
}

func TestWarningsBelowThresholdNeverAlert(t *testing.T) {
	r := newRig(t, openStore(t))
	c := &scripted{name: "probe:flaky", interval: 30 * time.Second, failures: 3}
	r.checks = []Check{c}
	c.set(Warning, "HTTP 429")
	r.step(0)
	r.step(30 * time.Second)
	c.set(OK, "HTTP 200")
	r.step(30 * time.Second)
	r.step(30 * time.Second)
	got := r.stored("probe:flaky")
	if got.State != OK || got.NotifiedState != "" {
		t.Fatalf("short failure recorded as failing: %+v", got)
	}
	expectMessages(t, r.notifier.messages())
}

func TestRecoveryIsNotAlertedWhenNeverAlerted(t *testing.T) {
	r := newRig(t, openStore(t))
	c := &scripted{name: "memory", interval: 30 * time.Second, failures: 1}
	r.checks = []Check{c}
	c.set(OK, "80.0% available")
	r.step(0)
	r.step(30 * time.Second)
	expectMessages(t, r.notifier.messages())
	if got := r.stored("memory"); got.State != OK || got.NotifiedState != "" {
		t.Fatalf("ok state recorded as notified: %+v", got)
	}
}

func TestUnknownNeverAlerts(t *testing.T) {
	r := newRig(t, openStore(t))
	c := &scripted{name: "tunnel", interval: 30 * time.Second, failures: 1}
	r.checks = []Check{c}
	c.set(Unknown, "Cloudflare unavailable")
	r.step(0)
	r.step(30 * time.Second)
	if got := r.stored("tunnel"); got.State != Unknown || got.NotifiedState != "" {
		t.Fatalf("unknown recorded: %+v", got)
	}
	c.set(Critical, "tunnel down")
	r.step(30 * time.Second)
	expectMessages(t, r.notifier.messages(), "🔴 lab tunnel: tunnel down")
	c.set(Unknown, "Cloudflare unavailable")
	r.step(30 * time.Second)
	expectMessages(t, r.notifier.messages(), "🔴 lab tunnel: tunnel down")
	if got := r.stored("tunnel"); got.State != Unknown || got.NotifiedState != Critical {
		t.Fatalf("unknown after a notified failure: %+v", got)
	}
	c.set(OK, "tunnel healthy")
	r.step(30 * time.Second)
	expectMessages(t, r.notifier.messages(), "🔴 lab tunnel: tunnel down", "✅ lab tunnel recovered")
}

func TestReminderCadenceAndDisable(t *testing.T) {
	r := newRig(t, openStore(t))
	c := &scripted{name: "disk:/", interval: 30 * time.Second, failures: 1}
	r.checks = []Check{c}
	r.reminder = time.Hour
	c.set(Critical, "96.0% used")
	r.step(0)
	expectMessages(t, r.notifier.messages(), "🔴 lab disk:/: 96.0% used")
	for i := 0; i < 119; i++ { // 59.5 minutes after the alert
		r.step(30 * time.Second)
	}
	expectMessages(t, r.notifier.messages(), "🔴 lab disk:/: 96.0% used")
	r.step(30 * time.Second) // one hour after the alert
	expectMessages(t, r.notifier.messages(), "🔴 lab disk:/: 96.0% used", "🔁 still failing: lab disk:/: 96.0% used")
	for i := 0; i < 119; i++ {
		r.step(30 * time.Second)
	}
	expectMessages(t, r.notifier.messages(), "🔴 lab disk:/: 96.0% used", "🔁 still failing: lab disk:/: 96.0% used")
	r.step(30 * time.Second) // one hour after the first reminder
	expectMessages(t, r.notifier.messages(), "🔴 lab disk:/: 96.0% used", "🔁 still failing: lab disk:/: 96.0% used", "🔁 still failing: lab disk:/: 96.0% used")

	r.reminder = 0
	before := len(r.notifier.messages())
	for i := 0; i < 600; i++ {
		r.step(30 * time.Second)
	}
	if after := len(r.notifier.messages()); after != before {
		t.Fatalf("reminders sent with reminder 0: %d more", after-before)
	}
}

func TestFailedSendIsRetriedAndStateAdvances(t *testing.T) {
	r := newRig(t, openStore(t))
	c := &scripted{name: "probe:api", interval: time.Second, failures: 1}
	r.checks = []Check{c}
	r.notifier.down = true
	c.set(Critical, "connection refused")
	r.step(0)
	if got := r.stored("probe:api"); got.State != Critical || got.NotifiedState != "" {
		t.Fatalf("state or notified state after a failed send: %+v", got)
	}
	if r.notifier.attempts != 1 {
		t.Fatalf("attempts after the first failure: %d", r.notifier.attempts)
	}
	for i := 0; i < 29; i++ {
		r.step(time.Second)
	}
	if r.notifier.attempts != 1 {
		t.Fatalf("retry before 30 seconds: %d attempts", r.notifier.attempts)
	}
	r.step(time.Second)
	if r.notifier.attempts != 2 {
		t.Fatalf("no retry at 30 seconds: %d attempts", r.notifier.attempts)
	}
	r.notifier.down = false
	for i := 0; i < 29; i++ {
		r.step(time.Second)
	}
	if r.notifier.attempts != 2 {
		t.Fatalf("retry before the next 30 seconds: %d attempts", r.notifier.attempts)
	}
	r.step(time.Second)
	expectMessages(t, r.notifier.messages(), "🔴 lab probe:api: connection refused")
	if got := r.stored("probe:api"); got.NotifiedState != Critical {
		t.Fatalf("delivered alert not recorded: %+v", got)
	}
	r.step(time.Second)
	if r.notifier.attempts != 3 {
		t.Fatalf("a successful alert was retried: %d attempts", r.notifier.attempts)
	}
	if len(r.logs) != 1 || !strings.Contains(r.logs[0], "probe:api") {
		t.Fatalf("log lines: %q", r.logs)
	}
}

func TestAlertCarriesTheCheckAsFields(t *testing.T) {
	r := newRig(t, openStore(t))
	r.reminder = time.Hour
	c := &scripted{name: "probe:site", interval: 30 * time.Second, failures: 1}
	r.checks = []Check{c}
	c.set(Critical, "HTTP 503")
	r.step(0)
	failedAt := r.now
	r.step(time.Hour)
	c.set(OK, "HTTP 200")
	r.step(30 * time.Second)
	if len(r.notifier.alerts) != 3 {
		t.Fatalf("alerts: %+v", r.notifier.alerts)
	}
	first, reminder, recovered := r.notifier.alerts[0], r.notifier.alerts[1], r.notifier.alerts[2]
	if first != (Alert{Environment: "lab", Kind: "check", Name: "probe:site", State: Critical, Detail: "HTTP 503", Since: failedAt, Text: "🔴 lab probe:site: HTTP 503"}) {
		t.Fatalf("first alert %+v", first)
	}
	if reminder.State != Critical || reminder.Kind != "check" || reminder.Since != failedAt || reminder.Text != "🔁 still failing: lab probe:site: HTTP 503" {
		t.Fatalf("reminder %+v", reminder)
	}
	if recovered.State != OK || recovered.Detail != "HTTP 200" || recovered.Text != "✅ lab probe:site recovered" {
		t.Fatalf("recovery %+v", recovered)
	}
}

func TestAlertReachingOneChannelIsDeliveredAndTheFailureIsLoggedOnce(t *testing.T) {
	r := newRig(t, openStore(t))
	r.notifier.partial = true
	c := &scripted{name: "probe:api", interval: time.Second, failures: 1}
	r.checks = []Check{c}
	c.set(Critical, "connection refused")
	for i := 0; i < 70; i++ {
		r.step(time.Second)
	}
	if got := r.stored("probe:api"); got.NotifiedState != Critical {
		t.Fatalf("a delivered alert is not recorded: %+v", got)
	}
	if r.notifier.attempts != 1 {
		t.Fatalf("a delivered alert was sent again: %d attempts", r.notifier.attempts)
	}
	if len(r.logs) != 1 || r.logs[0] != "alert for probe:api not delivered to every channel: webhook: webhook unavailable" {
		t.Fatalf("log lines: %q", r.logs)
	}
}

func TestAlertsUnavailableAreLoggedOnce(t *testing.T) {
	r := newRig(t, openStore(t))
	c := &scripted{name: "probe:api", interval: time.Second, failures: 1}
	r.checks = []Check{c}
	r.m.Notifier = func(context.Context) (Notifier, time.Duration, error) {
		return nil, 0, errors.New("telegram bot token secret missing")
	}
	c.set(Critical, "HTTP 500")
	for i := 0; i < 40; i++ {
		r.step(time.Second)
	}
	if got := r.stored("probe:api"); got.State != Critical || got.NotifiedState != "" {
		t.Fatalf("state after unavailable alerts: %+v", got)
	}
	if len(r.logs) != 1 || r.logs[0] != "alert not sent for probe:api: telegram bot token secret missing" {
		t.Fatalf("log lines: %q", r.logs)
	}
}

func TestRestartDoesNotRealertFailingCheck(t *testing.T) {
	store := openStore(t)
	r := newRig(t, store)
	c := &scripted{name: "probe:site", interval: 30 * time.Second, failures: 1}
	r.checks = []Check{c}
	c.set(Critical, "HTTP 503")
	r.step(0)
	expectMessages(t, r.notifier.messages(), "🔴 lab probe:site: HTTP 503")

	// The agent restarts: a new monitor reads the stored state, and the check still fails.
	r.attach()
	r.step(30 * time.Second)
	r.step(30 * time.Second)
	expectMessages(t, r.notifier.messages(), "🔴 lab probe:site: HTTP 503")
	if got := r.stored("probe:site"); got.State != Critical || got.NotifiedState != Critical {
		t.Fatalf("state lost across restart: %+v", got)
	}

	c.set(OK, "HTTP 200")
	r.step(30 * time.Second)
	expectMessages(t, r.notifier.messages(), "🔴 lab probe:site: HTTP 503", "✅ lab probe:site recovered")
}

func TestRemovedCheckIsDeletedWithoutAlert(t *testing.T) {
	r := newRig(t, openStore(t))
	gone := &scripted{name: "probe:old", interval: 30 * time.Second, failures: 1}
	kept := &scripted{name: "probe:new", interval: 30 * time.Second, failures: 1}
	r.checks = []Check{gone, kept}
	gone.set(Critical, "HTTP 500")
	kept.set(OK, "HTTP 200")
	r.step(0)
	expectMessages(t, r.notifier.messages(), "🔴 lab probe:old: HTTP 500")

	r.checks = []Check{kept}
	r.step(time.Minute)
	r.step(30 * time.Second)
	expectMessages(t, r.notifier.messages(), "🔴 lab probe:old: HTTP 500")
	checks, err := r.m.Snapshot(context.Background())
	if err != nil || len(checks) != 1 || checks[0].Name != "probe:new" {
		t.Fatalf("snapshot after removal: %+v %v", checks, err)
	}
}

func TestMissingNotifierRecordsStateWithoutAlerts(t *testing.T) {
	r := newRig(t, openStore(t))
	r.m.Notifier = nil
	c := &scripted{name: "probe:api", interval: time.Second, failures: 1}
	r.checks = []Check{c}
	c.set(Critical, "connection refused")
	r.step(0)
	if got := r.stored("probe:api"); got.State != Critical || got.NotifiedState != "" {
		t.Fatalf("state without a notifier: %+v", got)
	}
}

func TestNoConfiguredNotifierSendsNothing(t *testing.T) {
	r := newRig(t, openStore(t))
	r.nilNote = true
	c := &scripted{name: "probe:api", interval: time.Second, failures: 1}
	r.checks = []Check{c}
	c.set(Critical, "connection refused")
	r.step(0)
	if got := r.stored("probe:api"); got.State != Critical || got.NotifiedState != "" {
		t.Fatalf("unconfigured alerts changed notified state: %+v", got)
	}
	expectMessages(t, r.notifier.messages())
}

func TestCheckRunsOnceAtATime(t *testing.T) {
	r := newRig(t, openStore(t))
	c := &scripted{name: "probe:slow", interval: time.Second, failures: 1}
	r.checks = []Check{c}
	c.set(OK, "HTTP 200")
	r.step(0) // rebuilds the checks and runs the first pass
	var queued []func()
	start := func(run func()) { queued = append(queued, run) }
	r.now = r.now.Add(time.Minute)
	r.m.tick(context.Background(), r.now, start)
	if len(queued) != 1 {
		t.Fatalf("first due run not started: %d", len(queued))
	}
	r.now = r.now.Add(5 * time.Second)
	r.m.tick(context.Background(), r.now, start)
	if len(queued) != 1 {
		t.Fatalf("a running check was started again: %d", len(queued))
	}
	queued[0]()
	r.now = r.now.Add(time.Second)
	r.m.tick(context.Background(), r.now, start)
	if len(queued) != 2 {
		t.Fatalf("check not started after it finished: %d", len(queued))
	}
}

func TestPanickingCheckReportsUnknown(t *testing.T) {
	r := newRig(t, openStore(t))
	c := &scripted{name: "probe:broken", interval: time.Second, failures: 1, panics: true}
	r.checks = []Check{c}
	r.step(0)
	if got := r.stored("probe:broken"); got.State != Unknown || got.Detail != "check failed" {
		t.Fatalf("panic not reported as unknown: %+v", got)
	}
	expectMessages(t, r.notifier.messages())
}

func TestNilChecksKeepTheCurrentSet(t *testing.T) {
	r := newRig(t, openStore(t))
	c := &scripted{name: "probe:api", interval: time.Second, failures: 1}
	r.checks = []Check{c}
	c.set(OK, "HTTP 200")
	r.step(0)
	r.m.Checks = func(context.Context) []Check { return nil }
	r.step(time.Minute)
	if got := r.stored("probe:api"); got.State != OK {
		t.Fatalf("checks removed on a failed read: %+v", got)
	}
}

func TestSnapshotIsSortedByName(t *testing.T) {
	r := newRig(t, openStore(t))
	r.checks = []Check{
		&scripted{name: "tunnel", interval: time.Minute, failures: 1},
		&scripted{name: "disk:/", interval: time.Minute, failures: 1},
		&scripted{name: "memory", interval: time.Minute, failures: 1},
	}
	r.step(0)
	checks, err := r.m.Snapshot(context.Background())
	if err != nil || len(checks) != 3 || checks[0].Name != "disk:/" || checks[1].Name != "memory" || checks[2].Name != "tunnel" {
		t.Fatalf("snapshot: %+v %v", checks, err)
	}
}

func TestMessagesStayWithinTelegramLimits(t *testing.T) {
	long := strings.Repeat("é", 3000)
	text := message("lab", "probe:"+long, state.CheckState{State: Critical, Detail: long}, false)
	if len(text) > maxMessage || !strings.HasSuffix(text, "...") {
		t.Fatalf("message length %d", len(text))
	}
	if !strings.HasPrefix(text, "🔴 lab probe:") {
		t.Fatalf("message prefix: %q", text[:20])
	}
	if got := message("lab", "disk:/", state.CheckState{State: OK}, false); got != "✅ lab disk:/ recovered" {
		t.Fatalf("recovery text: %q", got)
	}
	if got := message("lab", "disk:/", state.CheckState{State: Warning, Detail: "81.0% used"}, false); got != "🟠 lab disk:/: 81.0% used" {
		t.Fatalf("warning text: %q", got)
	}
}

func TestRunStopsWhenContextEnds(t *testing.T) {
	store := openStore(t)
	c := &scripted{name: "probe:api", interval: time.Second, failures: 1}
	c.set(OK, "HTTP 200")
	m := &Monitor{Env: "lab", Store: store, Checks: func(context.Context) []Check { return []Check{c} }}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() {
		m.Run(ctx)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after the context ended")
	}
	if checks, err := store.Checks(context.Background()); err != nil || len(checks) != 1 || checks[0].State != OK {
		t.Fatalf("check not recorded while running: %+v %v", checks, err)
	}
}
