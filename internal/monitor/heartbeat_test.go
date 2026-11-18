package monitor

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// beatPath stands for the credential that a heartbeat URL usually holds.
const beatPath = "/ping/s3cr3t-beat-token"

// beatServer is a heartbeat endpoint that counts the requests it gets. status is the HTTP status it answers with.
type beatServer struct {
	srv     *httptest.Server
	status  atomic.Int32
	mu      sync.Mutex
	methods []string
	paths   []string
	agents  []string
	bodies  int
	release chan struct{} // when not nil, a request waits until it is closed
}

func newBeatServer(t *testing.T) *beatServer {
	t.Helper()
	b := &beatServer{}
	b.status.Store(http.StatusOK)
	b.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b.mu.Lock()
		b.methods = append(b.methods, r.Method)
		b.paths = append(b.paths, r.URL.Path)
		b.agents = append(b.agents, r.UserAgent())
		if r.ContentLength > 0 {
			b.bodies++
		}
		release := b.release
		b.mu.Unlock()
		if release != nil {
			select {
			case <-release:
			case <-r.Context().Done():
			}
		}
		w.WriteHeader(int(b.status.Load()))
	}))
	t.Cleanup(b.srv.Close)
	return b
}

func (b *beatServer) url() string { return b.srv.URL + beatPath }

func (b *beatServer) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.methods)
}

// newHeartbeat returns a heartbeat that pings url while alive is true, on the clock *now.
func newHeartbeat(url string, alive *atomic.Bool, now *time.Time) (*Heartbeat, Check) {
	h := &Heartbeat{
		Alive: func(context.Context) bool { return alive.Load() },
		Now:   func() time.Time { return *now },
	}
	return h, h.Check(10*time.Second, func(context.Context) (string, error) { return url, nil })
}

func TestHeartbeatSendsOneGETAndShowsWhenItWasAccepted(t *testing.T) {
	b := newBeatServer(t)
	now := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	var alive atomic.Bool
	alive.Store(true)
	_, check := newHeartbeat(b.url(), &alive, &now)
	if check.Name() != "heartbeat" || check.Interval() != 10*time.Second || check.Failures() != 1 {
		t.Fatalf("check %s %v %d", check.Name(), check.Interval(), check.Failures())
	}
	got := check.Run(context.Background())
	if got.State != OK || got.Detail != "last ping accepted at 2026-11-19T10:00:00Z" {
		t.Fatalf("result %+v", got)
	}
	if b.count() != 1 || b.methods[0] != http.MethodGet || b.paths[0] != beatPath || b.bodies != 0 || b.agents[0] != "berthy-heartbeat" {
		t.Fatalf("requests %v %v %v", b.methods, b.paths, b.agents)
	}
}

func TestHeartbeatIsUnknownWhenThePingIsNotAcceptedAndNeverAnythingWorse(t *testing.T) {
	b := newBeatServer(t)
	now := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	var alive atomic.Bool
	alive.Store(true)
	_, check := newHeartbeat(b.url(), &alive, &now)

	// Nothing was ever accepted.
	b.status.Store(http.StatusServiceUnavailable)
	if got := check.Run(context.Background()); got.State != Unknown || got.Detail != "ping not accepted: HTTP 503; no ping accepted yet" {
		t.Fatalf("first failure %+v", got)
	}
	b.status.Store(http.StatusOK)
	if got := check.Run(context.Background()); got.State != OK {
		t.Fatalf("accepted %+v", got)
	}
	// A redirect is not an accepted ping.
	now = now.Add(time.Minute)
	b.status.Store(http.StatusFound)
	if got := check.Run(context.Background()); got.State != Unknown || got.Detail != "ping not accepted: HTTP 302; last ping accepted at 2026-11-19T10:00:00Z" {
		t.Fatalf("redirect %+v", got)
	}
	// The endpoint goes away.
	b.srv.Close()
	got := check.Run(context.Background())
	if got.State != Unknown || got.Detail != "ping failed: unreachable; last ping accepted at 2026-11-19T10:00:00Z" {
		t.Fatalf("unreachable %+v", got)
	}
	for _, leak := range []string{"s3cr3t", "127.0.0.1"} {
		if strings.Contains(got.Detail, leak) {
			t.Fatalf("the detail leaks %q: %q", leak, got.Detail)
		}
	}
}

func TestHeartbeatTimesOutAndSaysSo(t *testing.T) {
	b := newBeatServer(t)
	b.release = make(chan struct{})
	defer close(b.release)
	now := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	var alive atomic.Bool
	alive.Store(true)
	h, _ := newHeartbeat(b.url(), &alive, &now)
	h.Timeout = 50 * time.Millisecond
	check := h.Check(10*time.Second, func(context.Context) (string, error) { return b.url(), nil })
	started := time.Now()
	got := check.Run(context.Background())
	if got.State != Unknown || got.Detail != "ping failed: timeout; no ping accepted yet" || time.Since(started) > 5*time.Second {
		t.Fatalf("result %+v after %v", got, time.Since(started))
	}
}

func TestHeartbeatSendsNothingWhileDockerDoesNotAnswer(t *testing.T) {
	b := newBeatServer(t)
	now := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	var alive atomic.Bool
	_, check := newHeartbeat(b.url(), &alive, &now)
	got := check.Run(context.Background())
	if got.State != Unknown || got.Detail != "Docker unavailable: no ping sent; no ping accepted yet" || b.count() != 0 {
		t.Fatalf("result %+v, %d requests", got, b.count())
	}
	alive.Store(true)
	if got = check.Run(context.Background()); got.State != OK || b.count() != 1 {
		t.Fatalf("result %+v, %d requests", got, b.count())
	}
}

func TestHeartbeatWithoutAUsableURLSendsNothing(t *testing.T) {
	b := newBeatServer(t)
	now := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	var alive atomic.Bool
	alive.Store(true)
	h := &Heartbeat{Alive: func(context.Context) bool { return alive.Load() }, Now: func() time.Time { return now }}
	check := h.Check(time.Minute, func(context.Context) (string, error) {
		return "", errors.New("heartbeat url secret missing")
	})
	if got := check.Run(context.Background()); got.State != Unknown || got.Detail != "heartbeat url secret missing; no ping accepted yet" || b.count() != 0 {
		t.Fatalf("result %+v", got)
	}
}

func TestHeartbeatNeverAlertsAndNeverBecomesAWarningOrCritical(t *testing.T) {
	b := newBeatServer(t)
	r := newRig(t, openStore(t))
	var alive atomic.Bool
	alive.Store(true)
	h := &Heartbeat{Alive: func(context.Context) bool { return alive.Load() }, Now: func() time.Time { return r.now }}
	r.checks = []Check{h.Check(10*time.Second, func(context.Context) (string, error) { return b.url(), nil })}

	r.step(0)
	if got := r.stored("heartbeat"); got.State != OK || got.Detail != "last ping accepted at 2026-11-16T10:00:00Z" {
		t.Fatalf("first ping: %+v", got)
	}
	// One GET for each interval, and none in between.
	r.step(5 * time.Second)
	if b.count() != 1 {
		t.Fatalf("a ping before the interval: %d", b.count())
	}
	r.step(5 * time.Second)
	r.step(10 * time.Second)
	if b.count() != 3 {
		t.Fatalf("pings after 20 seconds: %d", b.count())
	}

	// The endpoint refuses, then Docker stops answering: unknown, and never a message.
	b.status.Store(http.StatusInternalServerError)
	for i := 0; i < 10; i++ {
		r.step(10 * time.Second)
		if st := r.stored("heartbeat").State; st != Unknown {
			t.Fatalf("state %q while the endpoint refuses", st)
		}
	}
	alive.Store(false)
	pings := b.count()
	for i := 0; i < 10; i++ {
		r.step(10 * time.Second)
	}
	if b.count() != pings {
		t.Fatalf("pings while Docker does not answer: %d", b.count()-pings)
	}
	b.status.Store(http.StatusOK)
	alive.Store(true)
	r.step(10 * time.Second)
	if got := r.stored("heartbeat"); got.State != OK {
		t.Fatalf("recovery: %+v", got)
	}
	expectMessages(t, r.notifier.messages())
	if r.notifier.attempts != 0 {
		t.Fatalf("the monitor tried to alert %d times", r.notifier.attempts)
	}
}
