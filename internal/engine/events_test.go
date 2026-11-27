package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// eventsServer answers /events with the lines it is given, one JSON object each, then holds the stream open until the test
// closes it or the client leaves. It records the query of each request.
func eventsServer(t *testing.T, lines []string, hold bool) (*Engine, func() []url.Values) {
	t.Helper()
	var mu sync.Mutex
	var queries []url.Values
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1.44/events" {
			t.Errorf("path %s", r.URL.Path)
		}
		mu.Lock()
		queries = append(queries, r.URL.Query())
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		for _, l := range lines {
			io.WriteString(w, l+"\n")
			w.(http.Flusher).Flush()
		}
		if hold {
			<-r.Context().Done()
		}
	}))
	t.Cleanup(s.Close)
	return &Engine{client: &http.Client{Transport: rewriteTransport{base: s.URL}, Timeout: time.Second}}, func() []url.Values {
		mu.Lock()
		defer mu.Unlock()
		return queries
	}
}

const dieLine = `{"Type":"container","Action":"die","Actor":{"ID":"c1","Attributes":{"berthy.environment":"lab","exitCode":"137","name":"web.1.x"}},"scope":"local","time":1795061398,"timeNano":1795061398648214300}`
const healthLine = `{"Type":"container","Action":"health_status: healthy","Actor":{"ID":"c2","Attributes":{"berthy.environment":"lab"}},"scope":"local","time":1795061399,"timeNano":1795061399000000007}`
const updateLine = `{"Type":"service","Action":"update","Actor":{"ID":"svc","Attributes":{"name":"shop-api-web","replicas.new":"2","replicas.old":"1"}},"scope":"swarm","time":1795061400,"timeNano":1795061400000000000}`

func TestEventsDecodesDockerEvents(t *testing.T) {
	e, _ := eventsServer(t, []string{dieLine, healthLine, updateLine}, false)
	st, err := e.Events(context.Background(), time.Time{}, EventFilter{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	die, err := st.Next()
	if err != nil {
		t.Fatal(err)
	}
	if die.Type != "container" || die.Action != "die" || die.ID != "c1" || die.Attributes["exitCode"] != "137" || die.Attributes["berthy.environment"] != "lab" {
		t.Fatalf("die event %+v", die)
	}
	if !die.Time.Equal(time.Unix(0, 1795061398648214300)) {
		t.Fatalf("die time %v", die.Time)
	}
	health, err := st.Next()
	if err != nil {
		t.Fatal(err)
	}
	if health.Action != "health_status" || health.Detail != "healthy" || health.Time.Nanosecond() != 7 {
		t.Fatalf("health event %+v", health)
	}
	svc, err := st.Next()
	if err != nil || svc.Type != "service" || svc.Action != "update" || svc.Attributes["name"] != "shop-api-web" || svc.Detail != "" {
		t.Fatalf("service event %+v %v", svc, err)
	}
	if _, err = st.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("end of stream gave %v, want EOF", err)
	}
}

func TestEventsAsksDockerForTheFilterAndTheTime(t *testing.T) {
	e, queries := eventsServer(t, nil, false)
	since := time.Unix(1795061398, 5)
	f := EventFilter{Types: []string{"container", "service"}, Actions: []string{"die", "oom"}}
	st, err := e.Events(context.Background(), since, f)
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	q := queries()[0]
	if q.Get("since") != "1795061398.000000005" {
		t.Errorf("since %q", q.Get("since"))
	}
	var filters map[string][]string
	if err := json.Unmarshal([]byte(q.Get("filters")), &filters); err != nil {
		t.Fatalf("filters %q: %v", q.Get("filters"), err)
	}
	if len(filters["type"]) != 2 || filters["type"][0] != "container" || len(filters["event"]) != 2 || filters["event"][1] != "oom" {
		t.Errorf("filters %v", filters)
	}
	e2, queries2 := eventsServer(t, nil, false)
	st, err = e2.Events(context.Background(), time.Time{}, EventFilter{})
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	if q := queries2()[0]; q.Has("since") || q.Has("filters") {
		t.Errorf("an empty request carries %v", q)
	}
}

func TestLastContainerExitCode(t *testing.T) {
	const id = "bd1d07291e78b59bc4ebd04a5b7059c3c394111ed8f6022c35b2efef27aa70a6"
	since := time.Unix(1795061398, 5)
	until := time.Unix(1795061400, 9)
	die := func(container, code string, at time.Time) string {
		return fmt.Sprintf(`{"Type":"container","Action":"die","Actor":{"ID":%q,"Attributes":{"exitCode":%q,"com.docker.compose.service":"worker"}},"timeNano":%d}`, container, code, at.UnixNano())
	}
	older := die(id, "3", since)
	latest := die(id, "17", since.Add(time.Second))
	for _, tt := range []struct {
		name       string
		lines      []string
		code       int
		found, bad bool
	}{
		{"latest matching", []string{latest, older, die("other", "42", until), die(id, "43", since.Add(-time.Nanosecond)),
			die(id, "44", until.Add(time.Nanosecond)), strings.Replace(die(id, "45", until), `"die"`, `"start"`, 1)}, 17, true, false},
		{"zero", []string{older, die(id, "0", until)}, 0, true, false},
		{"empty history", nil, 0, false, false},
		{"bad final record", []string{latest, `{"Actor":`}, 0, false, true},
		{"bad last exit code", []string{latest, die(id, "unavailable", until)}, 0, false, false},
		{"out of range", []string{latest, die(id, "1234", until)}, 0, false, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e, queries := eventsServer(t, tt.lines, false)
			e.client.Timeout = 0 // The fixture ends the response after the last record.
			code, found, err := e.LastContainerExitCode(t.Context(), id, since, until)
			if code != tt.code || found != tt.found || (err != nil) != tt.bad {
				t.Errorf("last exit code = %d, %v, %v; want %d, %v, error=%v", code, found, err, tt.code, tt.found, tt.bad)
			}
			qs := queries()
			if len(qs) != 1 || qs[0].Get("since") != "1795061398.000000005" || qs[0].Get("until") != "1795061400.000000009" {
				t.Fatalf("unexpected event history bounds in %v", qs)
			}
			var filters map[string][]string
			if err = json.Unmarshal([]byte(qs[0].Get("filters")), &filters); err != nil {
				t.Fatal(err)
			}
			if len(filters) != 3 || !slices.Equal(filters["type"], []string{"container"}) ||
				!slices.Equal(filters["event"], []string{"die"}) || !slices.Equal(filters["container"], []string{id}) {
				t.Errorf("unexpected filters %v", filters)
			}
		})
	}
}

func TestEventsRefusedWhenDockerAnswersAnError(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusInternalServerError) }))
	defer s.Close()
	e := &Engine{client: &http.Client{Transport: rewriteTransport{base: s.URL}}}
	_, err := e.Events(context.Background(), time.Time{}, EventFilter{})
	var status *StatusError
	if !errors.As(err, &status) || status.Code != 500 {
		t.Fatalf("got %v, want a status error", err)
	}
}

func TestEventsStreamOutlivesTheClientTimeout(t *testing.T) {
	// The engine's client times a request out after seconds. A stream is meant to stay open far longer, so it must not
	// inherit that timeout.
	e, _ := eventsServer(t, []string{dieLine}, true)
	e.client.Timeout = 50 * time.Millisecond
	st, err := e.Events(context.Background(), time.Time{}, EventFilter{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err = st.Next(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)
	done := make(chan error, 1)
	go func() { _, err := st.Next(); done <- err }()
	select {
	case err := <-done:
		t.Fatalf("the stream ended after the client timeout: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestEventsEndsWhenTheContextEnds(t *testing.T) {
	e, _ := eventsServer(t, nil, true)
	ctx, cancel := context.WithCancel(context.Background())
	st, err := e.Events(ctx, time.Time{}, EventFilter{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	done := make(chan error, 1)
	go func() { _, err := st.Next(); done <- err }()
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Next returned an event after cancel")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Next did not return after the context ended")
	}
}
