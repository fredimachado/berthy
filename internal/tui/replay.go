package tui

import (
	"hash/fnv"
	"time"

	"berthy/internal/logs"
)

// replayWindow is how far before its newest line a reconnection asks the agent to start again. Lines of different containers
// arrive slightly out of order, so the new stream must go back a little to leave no gap.
const replayWindow = time.Second

// lineKey identifies a log line: when, where, which stream and what. The message is hashed, so keys stay small.
type lineKey struct {
	at      int64
	replica string
	role    string
	stream  string
	sum     uint64
}

func keyOf(r logs.Record) lineKey {
	h := fnv.New64a()
	_, _ = h.Write([]byte(r.Message))
	return lineKey{at: r.Timestamp.UnixNano(), replica: r.Replica, role: r.Role, stream: r.Stream, sum: h.Sum64()}
}

// replayFilter keeps a log line from being shown twice when a stream is opened again. It never drops a line of the first
// connection, whatever its time: containers interleave, and two lines can have the same time. After a reconnection the agent
// replays the lines since the floor; the filter skips those it has already delivered (counting identical lines, so a repeated
// line is shown as many times as it was written) and those older than the floor, and lets every other line through.
type replayFilter struct {
	seen     map[lineKey]int
	newest   time.Time
	floor    time.Time
	replayed map[lineKey]int
	// filtering is true on a connection that follows a drop.
	filtering bool
}

func newReplayFilter() *replayFilter { return &replayFilter{seen: map[lineKey]int{}} }

// reconnect starts the filtering of a new connection and returns the time to ask the agent for, or the zero time before any
// line was seen.
func (f *replayFilter) reconnect() time.Time {
	if f.newest.IsZero() {
		f.filtering = false
		return time.Time{}
	}
	f.filtering = true
	f.floor = f.newest.Add(-replayWindow)
	f.replayed = map[lineKey]int{}
	return f.floor
}

// accept reports whether a record is new, and notes it.
func (f *replayFilter) accept(r logs.Record) bool {
	k := keyOf(r)
	if f.filtering {
		if r.Timestamp.Before(f.floor) {
			return false
		}
		if f.replayed[k] < f.seen[k] {
			f.replayed[k]++
			return false
		}
	}
	f.seen[k]++
	if f.filtering {
		f.replayed[k]++
	}
	if r.Timestamp.After(f.newest) {
		f.newest = r.Timestamp
	}
	f.prune()
	return true
}

// prune forgets the keys that no reconnection can ask about: those older than the window before the newest line.
func (f *replayFilter) prune() {
	if len(f.seen) < 4096 {
		return
	}
	cutoff := f.newest.Add(-2 * replayWindow).UnixNano()
	for k := range f.seen {
		if k.at < cutoff {
			delete(f.seen, k)
		}
	}
}
