package engine

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"
)

// fakeStream is one connection of the fake Docker: Next gives what the test sends, and fails with the error the test ends
// it with.
type fakeStream struct {
	items  chan Event
	end    chan error
	closed chan struct{}
	once   sync.Once
}

func newFakeStream() *fakeStream {
	return &fakeStream{items: make(chan Event, 64), end: make(chan error, 1), closed: make(chan struct{})}
}

func (s *fakeStream) Next() (Event, error) {
	select {
	case ev := <-s.items:
		return ev, nil
	case err := <-s.end:
		return Event{}, err
	case <-s.closed:
		return Event{}, io.EOF
	}
}

func (s *fakeStream) Close() error {
	s.once.Do(func() { close(s.closed) })
	return nil
}

// fakeEventDocker hands out streams in order, and records the since of each connection. A connection that finds no stream queued
// fails, as a Docker that is down does.
type fakeEventDocker struct {
	mu      sync.Mutex
	streams []*fakeStream
	failN   int // the next failN connections fail
	sinces  []time.Time
	opened  chan struct{}
	gate    chan struct{} // when set, a connection waits until it is closed
}

func newFakeEventDocker(streams ...*fakeStream) *fakeEventDocker {
	return &fakeEventDocker{streams: streams, opened: make(chan struct{}, 64)}
}

func (f *fakeEventDocker) open(_ context.Context, since time.Time) (EventStream, error) {
	if f.gate != nil {
		<-f.gate
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sinces = append(f.sinces, since)
	if f.failN > 0 {
		f.failN--
		return nil, errors.New("Docker unavailable")
	}
	if len(f.streams) == 0 {
		return nil, errors.New("Docker unavailable")
	}
	s := f.streams[0]
	f.streams = f.streams[1:]
	f.opened <- struct{}{}
	return s, nil
}

func (f *fakeEventDocker) sinceOf(i int) time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sinces[i]
}

// hubFor returns a hub on the fake, whose sleeps are recorded instead of waited for, and a function that runs it until the
// test ends.
func hubFor(t *testing.T, f *fakeEventDocker, keep func(Event) bool) (*EventHub, *[]time.Duration) {
	t.Helper()
	var mu sync.Mutex
	var slept []time.Duration
	h := &EventHub{open: f.open, keep: keep, now: func() time.Time { return time.Unix(1000, 0) }}
	h.sleep = func(ctx context.Context, d time.Duration) {
		mu.Lock()
		slept = append(slept, d)
		mu.Unlock()
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); h.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	return h, &slept
}

func recv(t *testing.T, ch <-chan Event) Event {
	t.Helper()
	select {
	case ev := <-ch:
		return ev
	case <-time.After(2 * time.Second):
		t.Fatal("no event arrived")
		return Event{}
	}
}

func none(t *testing.T, ch <-chan Event) {
	t.Helper()
	select {
	case ev := <-ch:
		t.Fatalf("unexpected event %+v", ev)
	case <-time.After(50 * time.Millisecond):
	}
}

func died(id string) Event {
	return Event{Type: "container", Action: "die", ID: id, Time: time.Unix(2000, 0)}
}

func TestHubGivesEachSubscriberTheEventsItAsksFor(t *testing.T) {
	s := newFakeStream()
	h, _ := hubFor(t, newFakeEventDocker(s), func(Event) bool { return true })
	dies, stopDies := h.Watch(func(ev Event) bool { return ev.Action == "die" })
	defer stopDies()
	all, stopAll := h.Watch(func(Event) bool { return true })
	defer stopAll()

	s.items <- Event{Type: "container", Action: "oom", ID: "a", Time: time.Unix(2000, 0)}
	s.items <- died("b")
	if ev := recv(t, dies); ev.ID != "b" {
		t.Fatalf("the die subscriber got %+v", ev)
	}
	if ev := recv(t, all); ev.ID != "a" {
		t.Fatalf("the first event for all was %+v", ev)
	}
	if ev := recv(t, all); ev.ID != "b" {
		t.Fatalf("the second event for all was %+v", ev)
	}
	none(t, dies)
}

func TestHubDropsWhatTheEnvironmentDoesNotOwn(t *testing.T) {
	s := newFakeStream()
	h, _ := hubFor(t, newFakeEventDocker(s), func(ev Event) bool { return ev.Attributes["berthy.environment"] == "lab" })
	all, stop := h.Watch(func(Event) bool { return true })
	defer stop()
	s.items <- Event{Type: "container", Action: "die", ID: "other", Attributes: map[string]string{"berthy.environment": "prod"}}
	s.items <- Event{Type: "container", Action: "die", ID: "mine", Attributes: map[string]string{"berthy.environment": "lab"}}
	if ev := recv(t, all); ev.ID != "mine" {
		t.Fatalf("got %+v", ev)
	}
}

func TestHubStopsGivingEventsToACancelledSubscriber(t *testing.T) {
	s := newFakeStream()
	h, _ := hubFor(t, newFakeEventDocker(s), func(Event) bool { return true })
	ch, stop := h.Watch(func(Event) bool { return true })
	stop()
	stop() // twice is fine
	s.items <- died("a")
	none(t, ch)
}

func TestHubNeverBlocksOnASubscriberThatDoesNotRead(t *testing.T) {
	s := newFakeStream()
	h, _ := hubFor(t, newFakeEventDocker(s), func(Event) bool { return true })
	slow, stopSlow := h.Watch(func(Event) bool { return true })
	defer stopSlow()
	fast, stopFast := h.Watch(func(Event) bool { return true })
	defer stopFast()
	// More events than a subscriber buffers: the slow one loses some, the fast one is not held up by it.
	total := subscriberBuffer * 3
	got := make(chan int, 1)
	go func() {
		n := 0
		for range fast {
			n++
			if n == total {
				break
			}
		}
		got <- n
	}()
	for i := 0; i < total; i++ {
		s.items <- died("x")
		if i%8 == 7 {
			time.Sleep(time.Millisecond)
		}
	}
	select {
	case n := <-got:
		if n != total {
			t.Fatalf("the fast subscriber got %d of %d", n, total)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the hub was held up by a subscriber that does not read")
	}
	if n := len(slow); n > subscriberBuffer {
		t.Fatalf("the slow subscriber holds %d events, its buffer is %d", n, subscriberBuffer)
	}
}

func TestHubReconnectsFromTheLastEventAndTellsSubscribersToReadAgain(t *testing.T) {
	first, second := newFakeStream(), newFakeStream()
	f := newFakeEventDocker(first, second)
	h, slept := hubFor(t, f, func(Event) bool { return true })
	ch, stop := h.Watch(func(ev Event) bool { return ev.Action == "die" })
	defer stop()

	first.items <- died("a")
	if ev := recv(t, ch); ev.ID != "a" {
		t.Fatal(ev)
	}
	if !h.Live() {
		t.Fatal("the hub is not live while the stream is open")
	}
	first.end <- errors.New("connection reset")
	if ev := recv(t, ch); !ev.Resync() {
		t.Fatalf("after the reconnect the subscriber got %+v, want a resync", ev)
	}
	<-f.opened
	<-f.opened
	if got := f.sinceOf(1); !got.Equal(time.Unix(2000, 0)) {
		t.Fatalf("the second connection asked since %v, want the time of the last event", got)
	}
	if got := f.sinceOf(0); !got.IsZero() && !got.Equal(time.Unix(1000, 0)) {
		t.Fatalf("the first connection asked since %v", got)
	}
	second.items <- died("b")
	if ev := recv(t, ch); ev.ID != "b" {
		t.Fatalf("the events after the reconnect were not delivered: %+v", ev)
	}
	if len(*slept) != 1 || (*slept)[0] != eventBackoff(1) {
		t.Fatalf("slept %v before the reconnect", *slept)
	}
}

func TestHubReconnectsFromTheConnectTimeWhenNoEventCameBeforeTheDrop(t *testing.T) {
	first, second := newFakeStream(), newFakeStream()
	f := newFakeEventDocker(first, second)
	hubFor(t, f, func(Event) bool { return true })
	<-f.opened
	first.end <- io.ErrUnexpectedEOF
	<-f.opened
	if got := f.sinceOf(1); !got.Equal(time.Unix(1000, 0)) {
		t.Fatalf("since %v: a quiet stream that drops must not lose the gap", got)
	}
}

func TestHubBacksOffWhileDockerCannotBeReached(t *testing.T) {
	s := newFakeStream()
	f := newFakeEventDocker(s)
	f.failN = 6
	f.gate = make(chan struct{})
	h, slept := hubFor(t, f, func(Event) bool { return true })
	ch, stop := h.Watch(func(Event) bool { return true })
	defer stop()
	close(f.gate)
	<-f.opened
	if ev := recv(t, ch); !ev.Resync() {
		t.Fatalf("got %+v", ev)
	}
	if !h.Live() {
		t.Fatal("not live after the connection came back")
	}
	want := []time.Duration{eventBackoff(1), eventBackoff(2), eventBackoff(3), eventBackoff(4), eventBackoff(5), eventBackoff(6)}
	if len(*slept) != len(want) {
		t.Fatalf("slept %v, want %v", *slept, want)
	}
	for i := range want {
		if (*slept)[i] != want[i] {
			t.Fatalf("sleep %d was %v, want %v", i, (*slept)[i], want[i])
		}
	}
	for i := 1; i < len(want); i++ {
		if want[i] < want[i-1] {
			t.Fatalf("backoff shrinks: %v", want)
		}
	}
	if eventBackoff(100) != eventBackoffMax {
		t.Fatalf("backoff is not capped: %v", eventBackoff(100))
	}
}

func TestHubIsNotLiveWhileTheStreamIsDown(t *testing.T) {
	s := newFakeStream()
	f := newFakeEventDocker(s)
	h, _ := hubFor(t, f, func(Event) bool { return true })
	<-f.opened
	s.end <- io.EOF
	deadline := time.After(2 * time.Second)
	for h.Live() {
		select {
		case <-deadline:
			t.Fatal("still live after the stream ended")
		case <-time.After(time.Millisecond):
		}
	}
}

func TestEngineWithoutAHubWatchesNothing(t *testing.T) {
	e := &Engine{}
	ch, stop := e.Watch(func(Event) bool { return true })
	stop()
	if e.EventsLive() {
		t.Fatal("an engine without a hub is live")
	}
	select {
	case ev, ok := <-ch:
		t.Fatalf("event %+v %v", ev, ok)
	case <-time.After(10 * time.Millisecond):
	}
}
