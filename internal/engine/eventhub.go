package engine

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// subscriberBuffer is how many events a subscriber may hold unread. An event that finds the buffer full is dropped: the
	// subscriber already has a wake-up pending, and the periodic read it makes covers what was lost.
	subscriberBuffer = 16
	// eventBackoffMax is the longest wait between two attempts to reach Docker's event feed.
	eventBackoffMax = 30 * time.Second
	// eventStableAfter is how long a stream must stay open before a drop counts as a first failure again, not one more of a
	// run of them.
	eventStableAfter = 10 * time.Second
)

// ResyncType and ResyncAction name the event the hub sends to every subscriber once the feed is back after a drop: events
// may have been missed, so what the subscriber knows is to be read again.
const (
	ResyncType   = "berthy"
	ResyncAction = "resync"
)

// Resync reports whether the event is the hub's resync.
func (ev Event) Resync() bool { return ev.Type == ResyncType && ev.Action == ResyncAction }

// EventFilterWatched is what the hub asks Docker for: the container events and service events that change what the agent
// judges. Everything else, such as the health checks' exec events, never leaves Docker.
var EventFilterWatched = EventFilter{
	Types:   []string{"container", "service"},
	Actions: []string{"die", "oom", "health_status", "update", "remove"},
}

// EventHub reads Docker's event feed on one goroutine, Run, and gives each event to the subscribers that asked for it. It
// holds no state about containers: an event is only a reason to look again, and a lost event costs time, never
// correctness. After a dropped connection it asks Docker again from the time of the last event it saw.
type EventHub struct {
	open func(ctx context.Context, since time.Time) (EventStream, error)
	// keep says whether an event is one of Berthy's. Events it refuses are not given to anyone.
	keep  func(Event) bool
	now   func() time.Time
	sleep func(ctx context.Context, d time.Duration)

	live atomic.Bool
	mu   sync.Mutex
	subs map[*subscription]struct{}
}

type subscription struct {
	match func(Event) bool
	ch    chan Event
}

// NewEventHub returns the hub of e, and makes it the one e.Watch and e.EventsLive use. It is set before anything watches. It
// serves every hosted environment of the agent, so a container event is kept when it carries an environment label of any
// environment, or when it belongs to a Compose project, which Berthy does not label. A service event names no labels, so every
// service event is kept. The hub only wakes readers early: each reader still decides what is its own.
func NewEventHub(e *Engine) *EventHub {
	h := &EventHub{
		open: func(ctx context.Context, since time.Time) (EventStream, error) {
			return e.Events(ctx, since, EventFilterWatched)
		},
		keep: func(ev Event) bool {
			return ev.Type != "container" || ev.Attributes["berthy.environment"] != "" || ev.Attributes["com.docker.compose.project"] != ""
		},
	}
	e.hub = h
	return h
}

// Run reads the feed until ctx ends, reconnecting after each drop.
func (h *EventHub) Run(ctx context.Context) {
	var last time.Time
	fails := 0
	resync := false
	for ctx.Err() == nil {
		stream, err := h.open(ctx, last)
		if err != nil {
			resync = true
			fails++
			h.pause(ctx, eventBackoff(fails))
			continue
		}
		connected := h.clock()
		if last.IsZero() {
			// A stream that drops before any event comes must still ask from the time it was opened.
			last = connected
		}
		h.live.Store(true)
		if resync {
			h.publish(Event{Type: ResyncType, Action: ResyncAction, Time: connected}, true)
		}
		// Next does not watch ctx on every stream, so a goroutine closes the stream when ctx ends.
		over := make(chan struct{})
		go func() {
			select {
			case <-ctx.Done():
				stream.Close()
			case <-over:
			}
		}()
		for {
			ev, err := stream.Next()
			if err != nil {
				break
			}
			if ev.Time.After(last) {
				last = ev.Time
			}
			if h.keep == nil || h.keep(ev) {
				h.publish(ev, false)
			}
		}
		close(over)
		h.live.Store(false)
		stream.Close()
		if ctx.Err() != nil {
			return
		}
		resync = true
		if h.clock().Sub(connected) >= eventStableAfter {
			fails = 0
		}
		fails++
		h.pause(ctx, eventBackoff(fails))
	}
}

func (h *EventHub) clock() time.Time {
	if h.now != nil {
		return h.now()
	}
	return time.Now()
}

func (h *EventHub) pause(ctx context.Context, d time.Duration) {
	if h.sleep != nil {
		h.sleep(ctx, d)
		return
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// eventBackoff is the wait after the n-th failure in a row (n from 1): a second, doubling, up to eventBackoffMax.
func eventBackoff(n int) time.Duration {
	d := time.Second
	for i := 1; i < n && d < eventBackoffMax; i++ {
		d *= 2
	}
	return min(d, eventBackoffMax)
}

// publish gives ev to every subscriber that wants it, without waiting for any. A resync goes to all of them.
func (h *EventHub) publish(ev Event, everyone bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for s := range h.subs {
		if !everyone && !s.match(ev) {
			continue
		}
		select {
		case s.ch <- ev:
		default:
		}
	}
}

// Live reports whether the feed is open now.
func (h *EventHub) Live() bool { return h != nil && h.live.Load() }

// Watch subscribes to the events match accepts, and the resync. The channel is never closed. stop ends the subscription and
// may be called more than once. The subscriber must read promptly or lose events: a full buffer drops them.
func (h *EventHub) Watch(match func(Event) bool) (<-chan Event, func()) {
	s := &subscription{match: match, ch: make(chan Event, subscriberBuffer)}
	h.mu.Lock()
	if h.subs == nil {
		h.subs = map[*subscription]struct{}{}
	}
	h.subs[s] = struct{}{}
	h.mu.Unlock()
	return s.ch, func() {
		h.mu.Lock()
		delete(h.subs, s)
		h.mu.Unlock()
	}
}

// Watch subscribes to Docker's events through the engine's hub. An engine without a hub gives a channel that never fires, so
// the caller falls back on polling.
func (e *Engine) Watch(match func(Event) bool) (<-chan Event, func()) {
	if e == nil || e.hub == nil {
		return nil, func() {}
	}
	return e.hub.Watch(match)
}

// EventsLive reports whether the engine's event feed is open now.
func (e *Engine) EventsLive() bool { return e != nil && e.hub.Live() }
