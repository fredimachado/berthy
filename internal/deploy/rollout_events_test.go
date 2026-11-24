package deploy

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"berthy/internal/manifest"
	"berthy/internal/state"
)

// rolloutStatuses returns the ordered role/status pairs from persisted rollout events.
func rolloutStatuses(t *testing.T, s *state.Store, opID string) [][2]string {
	t.Helper()
	events, err := s.EventsAfter(context.Background(), opID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var out [][2]string
	for _, ev := range events {
		if ev.Kind != "rollout" {
			continue
		}
		var data struct {
			Role   string `json:"role"`
			Status string `json:"status"`
		}
		if err := json.Unmarshal(ev.Data, &data); err != nil {
			t.Fatalf("rollout data: %v", err)
		}
		out = append(out, [2]string{data.Role, data.Status})
	}
	return out
}

func TestRolloutEventsMarkStartedAndReadyForSuccessfulRole(t *testing.T) {
	s := openStore(t)
	c := &Controller{Engine: &positiveEngine{}, Store: s, Environment: "test"}
	one := 1
	req := Request{
		RequestID: "ok",
		App:       "web",
		Version:   "sha256:" + strings.Repeat("a", 64),
		Manifest: manifest.Manifest{
			Project: "proj", Network: "overlay",
			Apps: map[string]manifest.App{"web": {
				Kind: "image", Image: "example/web",
				Deploy: manifest.Deploy{DeployTimeout: testDeployTimeout, DrainTimeout: "1s"},
				Roles:  map[string]manifest.Role{"web": {Replicas: &one, Health: &manifest.Health{Test: []string{"CMD", "true"}}}},
			}},
		},
	}
	op, _, err := admit(t, c, context.Background(), Actor{ID: "admin"}, req)
	if err != nil {
		t.Fatal(err)
	}
	got := waitOperation(t, s, op.ID)
	if got.State != "succeeded" {
		t.Fatalf("operation %s %q", got.State, got.Err)
	}
	statuses := rolloutStatuses(t, s, op.ID)
	want := [][2]string{{"web", "started"}, {"web", "ready"}}
	if len(statuses) != len(want) {
		t.Fatalf("rollout events %v", statuses)
	}
	for i := range want {
		if statuses[i] != want[i] {
			t.Fatalf("rollout events %v", statuses)
		}
	}
	events, err := s.EventsAfter(context.Background(), op.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	last := events[len(events)-1]
	if last.Kind != "succeeded" {
		t.Fatalf("final event %s before rollout ready: %s", last.Kind, last.Data)
	}
	var readyAt, finalAt int
	for i, ev := range events {
		if ev.Kind == "rollout" {
			var data struct {
				Status string `json:"status"`
			}
			_ = json.Unmarshal(ev.Data, &data)
			if data.Status == "ready" {
				readyAt = i
			}
		}
		if ev.Kind == "succeeded" {
			finalAt = i
		}
	}
	if readyAt == 0 || finalAt <= readyAt {
		t.Fatalf("ready must precede final: ready@%d final@%d events=%v", readyAt, finalAt, statuses)
	}
}

func TestRolloutEventsOmitReadyWhenRoleFails(t *testing.T) {
	s := openStore(t)
	c := &Controller{Engine: &positiveEngine{unhealthy: true}, Store: s, Environment: "test"}
	one := 1
	req := Request{
		RequestID: "bad",
		App:       "web",
		Version:   "sha256:" + strings.Repeat("a", 64),
		Manifest: manifest.Manifest{
			Project: "proj", Network: "overlay",
			Apps: map[string]manifest.App{"web": {
				Kind: "image", Image: "example/web",
				Deploy: manifest.Deploy{DeployTimeout: "100ms", DrainTimeout: "1s"},
				Roles:  map[string]manifest.Role{"web": {Replicas: &one, Health: &manifest.Health{Test: []string{"CMD", "true"}}}},
			}},
		},
	}
	op, _, err := admit(t, c, context.Background(), Actor{ID: "admin"}, req)
	if err != nil {
		t.Fatal(err)
	}
	got := waitOperation(t, s, op.ID)
	if got.State != "failed" {
		t.Fatalf("operation %s %q", got.State, got.Err)
	}
	statuses := rolloutStatuses(t, s, op.ID)
	want := [][2]string{{"web", "started"}}
	if len(statuses) != len(want) || statuses[0] != want[0] {
		t.Fatalf("rollout events %v, want started only", statuses)
	}
}
