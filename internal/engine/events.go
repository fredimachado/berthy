package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Event is one thing Docker reported through GET /events.
type Event struct {
	// Type is the kind of object: "container" or "service". The hub also uses "berthy" for its own resync event.
	Type string
	// Action is what happened, such as "die", "oom", "health_status", "update" or "remove".
	Action string
	// Detail is what Docker adds after the action, such as "healthy" for "health_status: healthy". It is empty otherwise.
	Detail string
	// ID is the ID of the object.
	ID string
	// Attributes are Docker's attributes of the event: for a container its name, image, labels and, for a die, the exit code.
	Attributes map[string]string
	Time       time.Time
}

// EventFilter says which events Docker is asked to send. An empty list asks for all.
type EventFilter struct{ Types, Actions []string }

// EventStream is one open connection to Docker's event feed. Next waits for the next event and fails when the connection
// ends. Close ends it. Next is called by one goroutine at a time; Close may be called from any.
type EventStream interface {
	Next() (Event, error)
	Close() error
}

// Events opens Docker's event feed. since, when set, asks Docker to replay the events from that time first. It returns once
// Docker has accepted the request. The stream has no timeout of its own: it lasts until ctx ends, Close is called, or the
// connection drops.
func (e *Engine) Events(ctx context.Context, since time.Time, f EventFilter) (EventStream, error) {
	q := url.Values{}
	if !since.IsZero() {
		q.Set("since", fmt.Sprintf("%d.%09d", since.Unix(), since.Nanosecond()))
	}
	filters := map[string][]string{}
	if len(f.Types) > 0 {
		filters["type"] = f.Types
	}
	if len(f.Actions) > 0 {
		filters["event"] = f.Actions
	}
	if len(filters) > 0 {
		b, err := json.Marshal(filters)
		if err != nil {
			return nil, err
		}
		q.Set("filters", string(b))
	}
	path := "http://docker/v1.44/events"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, "GET", path, nil)
	if err != nil {
		return nil, err
	}
	// The engine's client times a request out after seconds, which would cut the stream: this one shares its transport only.
	client := http.Client{Transport: e.client.Transport}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("Docker unavailable: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, &StatusError{Code: resp.StatusCode}
	}
	return &eventStream{body: resp.Body, dec: json.NewDecoder(resp.Body)}, nil
}

type eventStream struct {
	body io.ReadCloser
	dec  *json.Decoder
}

func (s *eventStream) Close() error { return s.body.Close() }

func (s *eventStream) Next() (Event, error) {
	var raw struct {
		Type, Action string
		Actor        struct {
			ID         string
			Attributes map[string]string
		}
		TimeNano int64 `json:"timeNano"`
		Time     int64 `json:"time"`
	}
	if err := s.dec.Decode(&raw); err != nil {
		return Event{}, err
	}
	ev := Event{Type: raw.Type, ID: raw.Actor.ID, Attributes: raw.Actor.Attributes}
	ev.Action, ev.Detail, _ = strings.Cut(raw.Action, ": ")
	if raw.TimeNano != 0 {
		ev.Time = time.Unix(0, raw.TimeNano)
	} else {
		ev.Time = time.Unix(raw.Time, 0)
	}
	return ev, nil
}
