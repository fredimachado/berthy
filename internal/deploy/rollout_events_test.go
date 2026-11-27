package deploy

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"berthy/internal/engine"
	"berthy/internal/manifest"
	"berthy/internal/state"
)

func readRolloutStatuses(s *state.Store, opID string) ([][2]string, error) {
	events, err := s.EventsAfter(context.Background(), opID, 0, 100)
	if err != nil {
		return nil, err
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
			return nil, err
		}
		out = append(out, [2]string{data.Role, data.Status})
	}
	return out, nil
}

// rolloutStatuses returns the ordered role/status pairs from persisted rollout events.
func rolloutStatuses(t *testing.T, s *state.Store, opID string) [][2]string {
	t.Helper()
	out, err := readRolloutStatuses(s, opID)
	if err != nil {
		t.Fatal(err)
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

// apiRoleManifest is webManifest with role "api" so app and role ids differ.
func apiRoleManifest(timeout string) manifest.Manifest {
	m := webManifest(timeout, nil)
	app := m.Apps["web"]
	app.Roles = map[string]manifest.Role{"api": app.Roles["web"]}
	m.Apps["web"] = app
	return m
}

// gatedUpdateFake blocks update health until released, and records whether started was
// already durable when UpdateService ran. After-Drain snapshots alone would still pass if
// started were appended after Docker mutation; the mutate-entry check would not.
type gatedUpdateFake struct {
	*positiveEngine
	store *state.Store
	role  string

	mutated     chan struct{}
	verifyHold  chan struct{}
	allowVerify chan struct{}
	hold        atomic.Bool
	held        atomic.Bool

	mutateErr atomic.Value // string
}

func (f *gatedUpdateFake) UpdateService(ctx context.Context, id string, version uint64, spec json.RawMessage, auth *engine.RegistryAuth) error {
	op, err := f.store.Active(ctx)
	if err != nil {
		f.mutateErr.Store("active: " + err.Error())
		return err
	}
	got, err := readRolloutStatuses(f.store, op.ID)
	if err != nil {
		f.mutateErr.Store("events: " + err.Error())
		return err
	}
	started, ready := false, false
	for _, p := range got {
		if p[0] == f.role && p[1] == "started" {
			started = true
		}
		if p[0] == f.role && p[1] == "ready" {
			ready = true
		}
		if p[0] != f.role {
			f.mutateErr.Store("wrong role " + p[0])
		}
	}
	if !started || ready {
		f.mutateErr.Store("at mutate: started/ready mismatch")
	}
	select {
	case f.mutated <- struct{}{}:
	default:
	}
	return f.positiveEngine.UpdateService(ctx, id, version, spec, auth)
}

func (f *gatedUpdateFake) InspectContainerHealth(ctx context.Context, id string) (string, error) {
	if f.hold.Load() && f.held.CompareAndSwap(false, true) {
		select {
		case f.verifyHold <- struct{}{}:
		default:
		}
		<-f.allowVerify
	}
	return f.positiveEngine.InspectContainerHealth(ctx, id)
}

func TestRolloutEventsStartedBeforeUpdateAndReadyAfterVerify(t *testing.T) {
	s := openStore(t)
	f := &gatedUpdateFake{
		positiveEngine: &positiveEngine{},
		store:          s,
		role:           "api",
		mutated:        make(chan struct{}, 1),
		verifyHold:     make(chan struct{}, 1),
		allowVerify:    make(chan struct{}),
	}
	c := &Controller{rolloutHold: zeroTestRolloutHold(), Engine: f, Store: s, Environment: "test"}
	m := apiRoleManifest(testDeployTimeout)
	first := admitAndDrain(t, c, Request{
		RequestID: "first",
		App:       "web",
		Version:   "sha256:" + strings.Repeat("a", 64),
		Manifest:  m,
	})
	if first.State != "succeeded" {
		t.Fatalf("first deploy %s %q", first.State, first.Err)
	}
	if got := rolloutStatuses(t, s, first.ID); len(got) != 2 || got[0] != [2]string{"api", "started"} || got[1] != [2]string{"api", "ready"} {
		t.Fatalf("create milestones %v", got)
	}

	f.hold.Store(true)
	released := false
	defer func() {
		if !released {
			close(f.allowVerify)
		}
	}()

	ctx := context.Background()
	op, _, err := admit(t, c, ctx, Actor{ID: "admin"}, Request{
		RequestID: "second",
		App:       "web",
		Version:   "sha256:" + strings.Repeat("b", 64),
		Manifest:  m,
	})
	if err != nil {
		t.Fatal(err)
	}
	drained := make(chan error, 1)
	go func() { drained <- c.Drain(ctx) }()
	releaseJoin := func(msg string) {
		if !released {
			close(f.allowVerify)
			released = true
		}
		err := <-drained
		if msg != "" {
			t.Fatalf("%s (drain=%v)", msg, err)
		}
		if err != nil {
			t.Fatal(err)
		}
	}

	select {
	case <-f.mutated:
	case <-f.verifyHold:
		releaseJoin("verify hold before update mutation")
		return
	case err := <-drained:
		t.Fatalf("drain finished before update mutation: %v", err)
	}
	if msg, _ := f.mutateErr.Load().(string); msg != "" {
		releaseJoin(msg)
		return
	}
	if got := rolloutStatuses(t, s, op.ID); len(got) != 1 || got[0] != [2]string{"api", "started"} {
		releaseJoin(fmt.Sprintf("after mutate entry %v", got))
		return
	}

	select {
	case <-f.verifyHold:
	case err := <-drained:
		t.Fatalf("drain finished before verify hold: %v", err)
	}
	if got := rolloutStatuses(t, s, op.ID); len(got) != 1 || got[0][1] != "started" {
		releaseJoin(fmt.Sprintf("during verify gate %v", got))
		return
	}
	close(f.allowVerify)
	released = true
	if err := <-drained; err != nil {
		t.Fatal(err)
	}
	got, err := s.Operation(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != "succeeded" {
		t.Fatalf("update %s %q", got.State, got.Err)
	}
	statuses := rolloutStatuses(t, s, op.ID)
	if len(statuses) != 2 || statuses[0] != [2]string{"api", "started"} || statuses[1] != [2]string{"api", "ready"} {
		t.Fatalf("final milestones %v", statuses)
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
