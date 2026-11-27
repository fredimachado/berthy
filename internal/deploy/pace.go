package deploy

import (
	"context"
	"time"

	"berthy/internal/engine"
)

// The pace of the waits of a rollout, which read Docker to see whether a service is healthy, updated or drained. An event of
// Docker wakes a wait at once; the polls decide, so a wait that hears nothing still ends, only later.
var (
	// pollFast is the interval between polls when nothing wakes the wait: no event feed, or soon after an event.
	pollFast = 500 * time.Millisecond
	// pollSlow is the interval while the event feed is open: it is the fallback for an event that does not come.
	pollSlow = 5 * time.Second
	// pollSettle is how long after an event the wait stays on pollFast. An event can come a moment before Swarm records the
	// state it announces, such as a task that runs only once its container is healthy.
	pollSettle = 3 * time.Second
)

// eventSource is what an engine that hears Docker's events offers a wait. Fakes and engines without a feed do not have it,
// and their waits poll at pollFast.
type eventSource interface {
	Watch(match func(engine.Event) bool) (<-chan engine.Event, func())
	EventsLive() bool
}

// pacer paces the polls of one wait on one service.
type pacer struct {
	src       eventSource
	events    <-chan engine.Event
	stop      func()
	lastEvent time.Time
}

// serviceEvents matches the events a wait on the Swarm service with this ID cares about: the resync, an event of one of its
// containers, and an event of the service itself.
func serviceEvents(id string) func(engine.Event) bool {
	return func(ev engine.Event) bool {
		return ev.Resync() || ev.Attributes["com.docker.swarm.service.id"] == id || ev.Type == "service" && ev.ID == id
	}
}

// projectEvents matches the events a wait on a Compose project cares about: the resync and an event of one of its containers.
func projectEvents(project string) func(engine.Event) bool {
	return func(ev engine.Event) bool {
		return ev.Resync() || ev.Attributes["com.docker.compose.project"] == project
	}
}

// newPacer returns the pacer of a wait. e is the engine the wait reads; the pacer listens to the events of it that match
// when it offers them. The caller closes the pacer.
func newPacer(e any, match func(engine.Event) bool) *pacer {
	p := &pacer{stop: func() {}}
	if src, ok := e.(eventSource); ok {
		p.src = src
		p.events, p.stop = src.Watch(match)
	}
	return p
}

func (p *pacer) close() { p.stop() }

// wait blocks until the next poll is due: an event of the service, or the interval. It reports false when ctx has ended.
func (p *pacer) wait(ctx context.Context) bool {
	return p.waitUntil(ctx, time.Time{})
}

func (p *pacer) waitUntil(ctx context.Context, next time.Time) bool {
	every := pollFast
	if p.src != nil && p.src.EventsLive() && time.Since(p.lastEvent) >= pollSettle {
		every = pollSlow
	}
	if !next.IsZero() && time.Until(next) < every {
		every = max(0, time.Until(next))
	}
	timer := time.NewTimer(every)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-p.events:
		p.lastEvent = time.Now()
		// A burst of events is one poll.
		for drained := false; !drained; {
			select {
			case <-p.events:
			default:
				drained = true
			}
		}
		return true
	case <-timer.C:
		return true
	}
}
