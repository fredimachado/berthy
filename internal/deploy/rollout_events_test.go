package deploy

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"berthy/internal/engine"
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

func admitAndDrain(t *testing.T, c *Controller, req Request) state.Operation {
	t.Helper()
	ctx := context.Background()
	op, _, err := admit(t, c, ctx, Actor{ID: "admin"}, req)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := c.Store.Operation(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestRolloutEventsMarkStartedAndReadyForSuccessfulRole(t *testing.T) {
	c, s := diagController(t, newDiagFake())
	got := admitAndDrain(t, c, Request{
		RequestID: "ok",
		App:       "web",
		Version:   "sha256:" + strings.Repeat("a", 64),
		Manifest:  webManifest(testDeployTimeout, nil),
	})
	if got.State != "succeeded" {
		t.Fatalf("operation %s %q", got.State, got.Err)
	}
	statuses := rolloutStatuses(t, s, got.ID)
	want := [][2]string{{"web", "started"}, {"web", "ready"}}
	if len(statuses) != len(want) {
		t.Fatalf("rollout events %v", statuses)
	}
	for i := range want {
		if statuses[i] != want[i] {
			t.Fatalf("rollout events %v", statuses)
		}
	}
	events, err := s.EventsAfter(context.Background(), got.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	readyAt, finalAt := -1, -1
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
	if readyAt < 0 || finalAt < 0 || finalAt <= readyAt {
		t.Fatalf("ready must precede final: ready@%d final@%d events=%v", readyAt, finalAt, statuses)
	}
}

func TestRolloutEventsOmitReadyWhenRoleFails(t *testing.T) {
	f := newDiagFake()
	f.tasksFor = func(generation, _ string) []engine.ServiceTask { return failedTasks(generation, 3) }
	f.diagnose = exitedContainers(3, "starting")
	c, s := diagController(t, f)
	got := admitAndDrain(t, c, Request{
		RequestID: "bad",
		App:       "web",
		Version:   "sha256:" + strings.Repeat("a", 64),
		Manifest:  webManifest(testDeployTimeout, nil),
	})
	if got.State != "failed" {
		t.Fatalf("operation %s %q", got.State, got.Err)
	}
	statuses := rolloutStatuses(t, s, got.ID)
	want := [][2]string{{"web", "started"}}
	if len(statuses) != len(want) || statuses[0] != want[0] {
		t.Fatalf("rollout events %v, want started only", statuses)
	}
}
