// Package monitor runs the agent's health checks on their intervals and records what they find. A check reports ok,
// warning, critical or unknown. The stored state of a check changes only after it has reported a non-ok result the
// configured number of times in a row. A failing state recovers after the check's configured number of consecutive ok
// results. An alert goes out when the stored state changes, with reminders while a check keeps failing. Unknown never alerts.
package monitor

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
	"unicode/utf8"

	"berthy/internal/state"
)

// The states a check reports. The stored state uses the same words.
const (
	OK       = "ok"
	Warning  = "warning"
	Critical = "critical"
	Unknown  = "unknown"
)

const (
	tickEvery      = time.Second
	rebuildEvery   = time.Minute
	rebuildRetry   = 5 * time.Second
	rebuildTimeout = 20 * time.Second
	runTimeout     = 30 * time.Second
	notifyTimeout  = 15 * time.Second
	// retryAfter is the least time between two attempts to send an alert after one has failed.
	retryAfter = 30 * time.Second
	// maxMessage bounds the text of one alert, below Telegram's limit.
	maxMessage = 4000
	// maxDetail bounds the detail kept for one result.
	maxDetail = 512
	// defaultInterval is used when a check reports no interval.
	defaultInterval = 30 * time.Second
	// kickGap is the least time between two early rebuilds. A kick that comes sooner waits for it, and is merged with the
	// others that wait. An early rebuild is run once more when the gap ends, because Swarm records a failed task a moment
	// after its container dies, so the first read can be a step ahead of it.
	kickGap = 5 * time.Second
)

// Result is one run of a check.
type Result struct{ State, Detail string }

// Check is one scheduled condition.
type Check interface {
	// Name identifies the check, for example "probe:api-public" or "disk:/".
	Name() string
	Interval() time.Duration
	// Failures is how many non-ok results in a row change the stored state. Unknown results count too, but never alert.
	Failures() int
	// Successes is how many consecutive ok results recover a warning or critical state.
	Successes() int
	Run(ctx context.Context) Result
}

// Monitor runs the checks that Checks returns and records their states in Store. Its fields are set before Run.
type Monitor struct {
	Env    string
	Server string
	Store  *state.Store
	// Checks returns the checks to run. It is called every minute. A nil result means the configuration could not be
	// read: the current checks keep running and the call is retried soon.
	Checks func(ctx context.Context) []Check
	// Notifier returns where alerts go, and how often a failing check is reminded about. A nil Notifier, or one that
	// returns nil, means no alerts. An error means an alert could not be prepared; it is retried on a later run.
	Notifier func(ctx context.Context) (Notifier, time.Duration, error)
	// Now is the clock. Nil means time.Now.
	Now func() time.Time
	// Log receives operational messages. They never contain secrets.
	Log func(string)

	// mu guards trackers, nextRebuild and lastLog, and every tracker field. Writes to Store that must not race a
	// removal of a check happen under mu. Logging and network calls never do.
	mu          sync.Mutex
	trackers    map[string]*tracker
	nextRebuild time.Time
	lastLog     string
	// pending is what Kick has asked for and the monitor has not started; lastEarly is when the last early rebuild ran, and
	// follow is run again at followAt, once, to cover what the first read could not see yet.
	pending, follow kickSet
	lastEarly       time.Time
	followAt        time.Time
	wake            chan struct{}
}

// kickSet is the checks a kick wants run early: all of them, or those whose name starts with one of the prefixes.
type kickSet struct {
	any, all bool
	prefixes map[string]bool
}

func (k *kickSet) add(prefixes []string) {
	k.any = true
	if len(prefixes) == 0 {
		k.all = true
	}
	for _, p := range prefixes {
		if k.prefixes == nil {
			k.prefixes = map[string]bool{}
		}
		k.prefixes[p] = true
	}
}

func (k *kickSet) merge(o kickSet) {
	if !o.any {
		return
	}
	k.any = true
	k.all = k.all || o.all
	for p := range o.prefixes {
		if k.prefixes == nil {
			k.prefixes = map[string]bool{}
		}
		k.prefixes[p] = true
	}
}

func (k kickSet) has(name string) bool {
	if k.all {
		return true
	}
	for p := range k.prefixes {
		if len(name) >= len(p) && name[:len(p)] == p {
			return true
		}
	}
	return false
}

// Kick asks for the checks whose name starts with one of prefixes, or with none given every check, to be read again and run
// now instead of at their interval. It only wakes them: what a check decides is unchanged, and the periodic runs stay the
// safety net, so a kick that is lost costs time. Kicks that come within kickGap of the last early rebuild are merged and run
// when the gap ends. Kick never blocks.
func (m *Monitor) Kick(prefixes ...string) {
	m.mu.Lock()
	m.pending.add(prefixes)
	ch := m.wakeLocked()
	m.mu.Unlock()
	select {
	case ch <- struct{}{}:
	default:
	}
}

func (m *Monitor) wakeLocked() chan struct{} {
	if m.wake == nil {
		m.wake = make(chan struct{}, 1)
	}
	return m.wake
}

// takeKick returns the checks to run early at now, if an early rebuild is due. A kick starts one at once and schedules a
// second one kickGap later; kicks inside the gap wait for its end.
func (m *Monitor) takeKick(now time.Time) (kickSet, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.lastEarly.IsZero() && now.Sub(m.lastEarly) < kickGap {
		return kickSet{}, false
	}
	var set kickSet
	followDue := !m.followAt.IsZero() && !now.Before(m.followAt)
	switch {
	case m.pending.any:
		set.merge(m.pending)
		m.pending = kickSet{}
		if followDue {
			set.merge(m.follow)
		}
		m.follow, m.followAt = set, now.Add(kickGap)
	case followDue:
		set = m.follow
		m.follow, m.followAt = kickSet{}, time.Time{}
	default:
		return kickSet{}, false
	}
	m.lastEarly = now
	return set, true
}

// tracker is the in-memory side of one check: the stored state it last recorded, and the bookkeeping for its runs.
type tracker struct {
	name     string
	check    Check
	cur      state.CheckState
	dirty    bool // cur has changed and has not been stored yet
	streak   int  // non-ok results in a row
	okStreak int  // consecutive ok results while warning or critical
	running  bool
	last     time.Time // when the last run started
	failedAt time.Time // when the last alert attempt failed; zero after one succeeds
	logged   string    // the last message logged for this check
}

// Run rebuilds the checks when due, starts each check that is due, and returns once ctx is done and every run has ended.
func (m *Monitor) Run(ctx context.Context) {
	var wg sync.WaitGroup
	defer wg.Wait()
	ticker := time.NewTicker(tickEvery)
	defer ticker.Stop()
	for {
		m.tick(ctx, m.now(), func(run func()) {
			wg.Add(1)
			go func() {
				defer wg.Done()
				run()
			}()
		})
		m.mu.Lock()
		wake := m.wakeLocked()
		m.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-wake:
		}
	}
}

// tick rebuilds the checks if a rebuild is due, then hands each due check to start, which must run the function it is
// given. A check that is still running is never handed out again.
func (m *Monitor) tick(ctx context.Context, now time.Time, start func(func())) {
	early, wanted := m.takeKick(now)
	m.mu.Lock()
	rebuild := wanted || !now.Before(m.nextRebuild)
	m.mu.Unlock()
	if rebuild {
		m.rebuild(ctx, now)
	}
	if wanted {
		m.markDue(early)
	}
	for _, t := range m.due(now) {
		t := t
		start(func() { m.runCheck(ctx, t) })
	}
}

// markDue makes the checks of set due at the next due call, as if they had never run.
func (m *Monitor) markDue(set kickSet) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, t := range m.trackers {
		if set.has(t.name) {
			t.last = time.Time{}
		}
	}
}

// due marks and returns the checks that should start at now.
func (m *Monitor) due(now time.Time) []*tracker {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*tracker
	for _, t := range m.trackers {
		interval := t.check.Interval()
		if interval <= 0 {
			interval = defaultInterval
		}
		if t.running || !t.last.IsZero() && now.Sub(t.last) < interval {
			continue
		}
		t.running = true
		t.last = now
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

// rebuild replaces the checks with the set Checks returns. A check that is no longer returned loses its stored state
// without an alert. A check that is new starts unknown, so it shows a state before its first run.
func (m *Monitor) rebuild(ctx context.Context, now time.Time) {
	rctx, cancel := context.WithTimeout(ctx, rebuildTimeout)
	checks := m.Checks(rctx)
	cancel()
	if checks == nil {
		m.retryRebuild(now)
		return
	}
	stored, err := m.Store.Checks(ctx)
	if err != nil {
		m.logRebuild("check states unavailable: " + err.Error())
		m.retryRebuild(now)
		return
	}
	byName := make(map[string]state.CheckState, len(stored))
	for _, s := range stored {
		byName[s.Name] = s
	}
	want := make(map[string]Check, len(checks))
	var duplicates []string
	for _, c := range checks {
		if c == nil {
			continue
		}
		if _, dup := want[c.Name()]; dup {
			duplicates = append(duplicates, c.Name())
			continue
		}
		want[c.Name()] = c
	}
	var removeErrs []string
	m.mu.Lock()
	if m.trackers == nil {
		m.trackers = map[string]*tracker{}
	}
	for name := range m.trackers {
		if _, ok := want[name]; !ok {
			delete(m.trackers, name)
		}
	}
	for name, c := range want {
		if t, ok := m.trackers[name]; ok {
			t.check = c
			continue
		}
		cur, found := byName[name]
		if !found {
			cur = state.CheckState{Name: name, State: Unknown, Detail: "not checked yet", Since: now}
		}
		m.trackers[name] = &tracker{name: name, check: c, cur: cur, dirty: !found}
	}
	for _, s := range stored {
		if _, ok := want[s.Name]; !ok {
			if err := m.Store.DeleteCheck(ctx, s.Name); err != nil {
				removeErrs = append(removeErrs, err.Error())
			}
		}
	}
	m.nextRebuild = now.Add(rebuildEvery)
	m.mu.Unlock()
	for _, name := range duplicates {
		m.logRebuild("duplicate check " + name + " ignored")
	}
	for _, reason := range removeErrs {
		m.logRebuild("removing a check state failed: " + reason)
	}
}

func (m *Monitor) retryRebuild(now time.Time) {
	m.mu.Lock()
	m.nextRebuild = now.Add(rebuildRetry)
	m.mu.Unlock()
}

// runCheck runs one check once, records the result, and sends the alert or reminder the new state calls for.
func (m *Monitor) runCheck(ctx context.Context, t *tracker) {
	defer func() {
		m.mu.Lock()
		t.running = false
		m.mu.Unlock()
	}()
	m.mu.Lock()
	check := t.check
	m.mu.Unlock()
	res := m.execute(ctx, check)
	now := m.now()
	if m.apply(ctx, t, res, now) {
		m.notify(ctx, t, now)
	}
}

// execute runs a check with a deadline. A check that panics reports unknown instead of stopping the monitor.
func (m *Monitor) execute(ctx context.Context, c Check) (res Result) {
	runCtx, cancel := context.WithTimeout(ctx, runTimeout)
	defer cancel()
	defer func() {
		if r := recover(); r != nil {
			m.logRebuild(c.Name() + " check failed unexpectedly")
			res = Result{State: Unknown, Detail: "check failed"}
		}
	}()
	return c.Run(runCtx)
}

// apply moves the stored state of t toward the result. It reports false when t was removed while its check ran.
//
// A warning or critical state recovers after the check's success limit; other states become ok at once. Any non-ok
// result, unknown included, counts toward the failure limit. Before either limit the state stays as it was.
func (m *Monitor) apply(ctx context.Context, t *tracker, res Result, now time.Time) bool {
	res = normalize(res)
	m.mu.Lock()
	if m.trackers[t.name] != t {
		m.mu.Unlock()
		return false
	}
	prev := t.cur
	next := prev
	next.Name = t.name
	if res.State == OK {
		t.streak = 0
		if prev.State == Warning || prev.State == Critical {
			t.okStreak++
		}
		if (prev.State != Warning && prev.State != Critical) || t.okStreak >= t.check.Successes() {
			next.State = OK
			t.okStreak = 0
		}
	} else {
		t.okStreak = 0
		t.streak++
		if t.streak >= t.check.Failures() {
			next.State = res.State
		}
	}
	if next.State == res.State {
		next.Detail = res.Detail
	}
	if next.State != prev.State {
		next.Since = now
	}
	changed := next.State != prev.State || next.Detail != prev.Detail || !next.Since.Equal(prev.Since)
	t.cur = next
	var storeErr error
	if changed || t.dirty {
		t.dirty = true
		if storeErr = m.Store.PutCheck(ctx, next); storeErr == nil {
			t.dirty = false
		}
	}
	m.mu.Unlock()
	if storeErr != nil {
		m.logOnce(t, "check state not stored for "+t.name+": "+storeErr.Error())
	}
	return true
}

// notify sends the alert that the stored state calls for, or a reminder while a notified check keeps failing. A failed
// send is retried on a later run, but no sooner than retryAfter after the failure. The notified state is recorded only
// once the send succeeds.
func (m *Monitor) notify(ctx context.Context, t *tracker, now time.Time) {
	if m.Notifier == nil {
		return
	}
	m.mu.Lock()
	cur := t.cur
	failedAt := t.failedAt
	m.mu.Unlock()
	failing := cur.State == Warning || cur.State == Critical
	alert := cur.State != "" && cur.State != Unknown && cur.State != cur.NotifiedState && !(cur.State == OK && (cur.NotifiedState == "" || cur.NotifiedState == OK))
	if !alert && !(failing && cur.NotifiedState == cur.State) {
		return
	}
	if !failedAt.IsZero() && now.Sub(failedAt) < retryAfter {
		return
	}
	nctx, cancel := context.WithTimeout(ctx, notifyTimeout)
	defer cancel()
	n, reminder, err := m.Notifier(nctx)
	if err != nil {
		m.alertFailed(t, now, err.Error())
		return
	}
	if n == nil {
		return
	}
	reminding := !alert
	if reminding && (reminder <= 0 || now.Sub(cur.Notified) < reminder) {
		return
	}
	var partial *DeliveryError
	err = n.Notify(nctx, alertOf(m.Env, m.Server, t.name, cur, reminding))
	if err != nil && !errors.As(err, &partial) {
		m.alertFailed(t, now, err.Error())
		return
	}
	m.mu.Lock()
	t.failedAt = time.Time{}
	t.logged = ""
	var storeErr error
	if m.trackers[t.name] == t {
		t.cur.NotifiedState = cur.State
		t.cur.Notified = now
		t.dirty = true
		if storeErr = m.Store.PutCheck(ctx, t.cur); storeErr == nil {
			t.dirty = false
		}
	}
	m.mu.Unlock()
	if storeErr != nil {
		m.logOnce(t, "alert state not stored for "+t.name+": "+storeErr.Error())
	}
	// An alert that some channel took is delivered, and is not sent again to the channels that took it. The channels that
	// refused it are named once.
	if partial != nil {
		m.logOnce(t, "alert for "+t.name+" not delivered to every channel: "+partial.Error())
	}
}

// alertFailed records that an alert could not be sent, so the next attempt waits retryAfter.
func (m *Monitor) alertFailed(t *tracker, now time.Time, reason string) {
	m.mu.Lock()
	t.failedAt = now
	m.mu.Unlock()
	m.logOnce(t, "alert not sent for "+t.name+": "+reason)
}

// Snapshot returns the stored state of every check, sorted by name.
func (m *Monitor) Snapshot(ctx context.Context) ([]state.CheckState, error) {
	return m.Store.Checks(ctx)
}

func (m *Monitor) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

// logOnce logs msg for t unless it repeats the message last logged for t.
func (m *Monitor) logOnce(t *tracker, msg string) {
	m.mu.Lock()
	repeat := t.logged == msg
	t.logged = msg
	m.mu.Unlock()
	if !repeat {
		m.emit(msg)
	}
}

// logRebuild logs a message that belongs to the monitor rather than to one check, once until it changes.
func (m *Monitor) logRebuild(msg string) {
	m.mu.Lock()
	repeat := m.lastLog == msg
	m.lastLog = msg
	m.mu.Unlock()
	if !repeat {
		m.emit(msg)
	}
}

func (m *Monitor) emit(msg string) {
	if m.Log != nil {
		m.Log(msg)
	}
}

// normalize keeps the result within the known states and a bounded detail.
func normalize(r Result) Result {
	switch r.State {
	case OK, Warning, Critical, Unknown:
	default:
		r = Result{State: Unknown, Detail: "invalid result"}
	}
	r.Detail = truncate(r.Detail, maxDetail)
	return r
}

// alertOf is the alert for a stored state of a check. reminding selects the text sent again while a check keeps failing.
func alertOf(env, server, name string, c state.CheckState, reminding bool) Alert {
	a := Alert{Environment: env, Server: server, Kind: "check", Name: name, State: c.State, Detail: c.Detail, Since: c.Since}
	a.Text = message(a.Subject(), name, c, reminding)
	return a
}

// message is the alert for a stored state. reminding selects the text sent again while a check keeps failing.
func message(subject, name string, c state.CheckState, reminding bool) string {
	var text string
	switch {
	case c.State == OK:
		text = fmt.Sprintf("✅ %s %s recovered", subject, name)
	case reminding:
		text = fmt.Sprintf("🔁 still failing: %s %s: %s", subject, name, c.Detail)
	case c.State == Critical:
		text = fmt.Sprintf("🔴 %s %s: %s", subject, name, c.Detail)
	default:
		text = fmt.Sprintf("🟠 %s %s: %s", subject, name, c.Detail)
	}
	return truncate(text, maxMessage)
}

// truncate shortens s to at most n bytes, on a rune boundary, marking the cut with "...".
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n - 3
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "..."
}
