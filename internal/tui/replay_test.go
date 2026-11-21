package tui

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"berthy/internal/logs"

	tea "charm.land/bubbletea/v2"
)

func rec(at time.Time, replica, stream, msg string) logs.Record {
	return logs.Record{Identity: logs.Identity{Role: "web", Replica: replica}, Timestamp: at, Stream: stream, Message: msg}
}

func TestFirstConnectionNeverDropsALine(t *testing.T) {
	f := newReplayFilter()
	f.reconnect()
	t0 := testNow
	// Two replicas interleaved with out-of-order times, equal times, and a line written twice.
	in := []logs.Record{
		rec(t0.Add(5*time.Second), "a", "stdout", "a newest"),
		rec(t0.Add(4*time.Second), "b", "stdout", "b older than a's newest"),
		rec(t0.Add(4*time.Second), "b", "stdout", "b same time, other text"),
		rec(t0.Add(4*time.Second), "b", "stdout", "b same time, other text"),
		rec(t0.Add(time.Second), "b", "stderr", "b much older"),
		rec(t0.Add(5*time.Second), "a", "stdout", "a newest"),
	}
	for i, r := range in {
		if !f.accept(r) {
			t.Errorf("line %d (%s) was dropped on the first connection", i, r.Message)
		}
	}
}

func TestReconnectReplayHasNoDuplicatesAndNoGaps(t *testing.T) {
	f := newReplayFilter()
	f.reconnect()
	t0 := testNow
	first := []logs.Record{
		rec(t0, "a", "stdout", "one"),
		rec(t0.Add(2*time.Second), "b", "stdout", "two"),
		rec(t0.Add(2*time.Second), "a", "stdout", "three"),
		rec(t0.Add(2*time.Second), "a", "stdout", "three"),
		rec(t0.Add(3*time.Second), "a", "stdout", "four"),
	}
	for _, r := range first {
		f.accept(r)
	}
	floor := f.reconnect()
	if !floor.Equal(t0.Add(2 * time.Second)) {
		t.Fatalf("floor %v, want the newest line minus one second", floor)
	}
	replay := []logs.Record{
		rec(t0, "a", "stdout", "one"),                               // older than the floor: skipped
		rec(t0.Add(2*time.Second), "b", "stdout", "two"),            // seen
		rec(t0.Add(2500*time.Millisecond), "b", "stdout", "missed"), // written during the outage: a gap to fill
		rec(t0.Add(2*time.Second), "a", "stdout", "three"),          // seen twice
		rec(t0.Add(2*time.Second), "a", "stdout", "three"),
		rec(t0.Add(2*time.Second), "a", "stdout", "three"), // a third copy: new
		rec(t0.Add(3*time.Second), "a", "stdout", "four"),  // seen
		rec(t0.Add(4*time.Second), "a", "stdout", "five"),  // new
	}
	var got []string
	for _, r := range replay {
		if f.accept(r) {
			got = append(got, r.Message)
		}
	}
	if strings.Join(got, ",") != "missed,three,five" {
		t.Fatalf("delivered %v, want [missed three five]", got)
	}
	// A line already delivered on this connection is not new again on the next one.
	f.reconnect()
	if f.accept(rec(t0.Add(4*time.Second), "a", "stdout", "five")) {
		t.Fatal("a line delivered before the second drop was shown again")
	}
}

func TestReplayFilterPrunesOldKeys(t *testing.T) {
	f := newReplayFilter()
	f.reconnect()
	for i := 0; i < 6000; i++ {
		f.accept(rec(testNow.Add(time.Duration(i)*time.Second), "a", "stdout", fmt.Sprint(i)))
	}
	if len(f.seen) > 4200 {
		t.Fatalf("%d keys kept", len(f.seen))
	}
}

// A fake agent whose first connection delivers interleaved lines and drops, and whose second one replays and adds two.
func TestStreamShowsInterleavedLinesAndFillsTheGapOnReconnect(t *testing.T) {
	line := func(sec float64, replica, msg string) string {
		ts := testNow.Add(time.Duration(sec * float64(time.Second))).Format(time.RFC3339Nano)
		return fmt.Sprintf("event: log\ndata: {\"role\":\"web\",\"replica\":%q,\"timestamp\":%q,\"stream\":\"stdout\",\"message\":%q}\n\n", replica, ts, msg)
	}
	var mu sync.Mutex
	n := 0
	c, _ := agentServer(t, map[string]http.HandlerFunc{
		"/v1/projects/demo/environments/prod/apps/api/logs": func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			n++
			conn := n
			mu.Unlock()
			w.Header().Set("Content-Type", "text/event-stream")
			if conn == 1 {
				fmt.Fprint(w, line(5, "a", "a5")+line(4, "b", "b4 older than a5")+line(4, "b", "b4 same time")+line(4, "b", "b4 same time")+line(2, "b", "b2"))
				return
			}
			fmt.Fprint(w, line(2, "b", "b2")+line(4, "b", "b4 same time")+line(4, "b", "b4 same time")+line(4.5, "a", "a4.5 missed")+
				line(5, "a", "a5")+line(4, "b", "b4 older than a5")+line(6, "a", "a6 new")+line(5.5, "b", "b5.5 new"))
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := make(chan tea.Msg, 64)
	go c.streamLogs(ctx, "api", 1, channelSender(ctx, ch))
	var got []string
	collect(t, ch, func(m tea.Msg) bool {
		if l, ok := m.(logMsg); ok {
			got = append(got, l.rec.Message)
			return l.rec.Message == "b5.5 new"
		}
		return false
	})
	want := "a5,b4 older than a5,b4 same time,b4 same time,b2,a4.5 missed,a6 new,b5.5 new"
	if strings.Join(got, ",") != want {
		t.Fatalf("shown:\n  %v\nwant:\n  %v", got, strings.Split(want, ","))
	}
}
