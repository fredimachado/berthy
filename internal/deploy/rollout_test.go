package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"berthy/internal/engine"
	"berthy/internal/manifest"
	"berthy/internal/state"
)

type journalFake struct {
	fail  bool
	saved []state.Intent
}

func (j *journalFake) RecordIntent(_ context.Context, _ string, i state.Intent) error {
	if j.fail {
		return errors.New("disk failed")
	}
	j.saved = append(j.saved, i)
	return nil
}
func (j *journalFake) MarkStep(context.Context, string, int, string, json.RawMessage) error {
	return nil
}

type mutationFake struct {
	creates    int
	live       engine.ServiceSnapshot
	inspectErr error
}

func (m *mutationFake) InspectService(context.Context, string) (engine.ServiceSnapshot, error) {
	return m.live, m.inspectErr
}
func (m *mutationFake) CreateService(context.Context, json.RawMessage, *engine.RegistryAuth) (string, error) {
	m.creates++
	return "created", nil
}
func (m *mutationFake) UpdateService(context.Context, string, uint64, json.RawMessage, *engine.RegistryAuth) error {
	return nil
}
func (m *mutationFake) DeleteService(context.Context, string) error { return nil }
func TestJournalFailurePreventsCreate(t *testing.T) {
	j := &journalFake{fail: true}
	m := &mutationFake{}
	err := applyIntent(context.Background(), j, m, "op", state.Intent{Kind: "create", Name: "svc", Desired: json.RawMessage(`{"Name":"svc"}`)}, nil)
	if err == nil || m.creates != 0 {
		t.Fatalf("journal error %v, creates %d", err, m.creates)
	}
}

func TestUpdateRejectsForeignDriftAfterJournal(t *testing.T) {
	j := &journalFake{}
	m := &mutationFake{live: engine.ServiceSnapshot{ID: "id", Version: 4, Spec: json.RawMessage(`{"Name":"changed"}`)}}
	err := applyIntent(context.Background(), j, m, "op", state.Intent{Kind: "update", Name: "svc", ServiceID: "id", Before: json.RawMessage(`{"Name":"svc"}`), Desired: json.RawMessage(`{"Name":"new"}`)}, nil)
	if err == nil || len(j.saved) != 1 {
		t.Fatalf("drift error %v, intents %d", err, len(j.saved))
	}
}

func TestCompensationRefusesForeignCreate(t *testing.T) {
	m := &mutationFake{live: engine.ServiceSnapshot{ID: "other", Spec: json.RawMessage(`{"Name":"other"}`)}}
	err := compensate(context.Background(), m, state.Intent{Kind: "create", Name: "svc", Desired: json.RawMessage(`{"Name":"owned"}`)}, nil)
	if err == nil {
		t.Fatal("foreign service accepted")
	}
}

func TestNewServiceUsesStartFirstAndPinnedImage(t *testing.T) {
	one := 1
	r := manifest.Role{Replicas: &one, Health: &manifest.Health{Test: []string{"CMD", "true"}}}
	b, err := serviceSpec("env", "proj", "app", "web", "operation", "network", "registry/image@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", nil, r, "10s")
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	if err = json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	update := v["UpdateConfig"].(map[string]any)
	if update["Order"] != "start-first" {
		t.Fatalf("update config: %s", b)
	}
}

type healthFake struct {
	tasks  []engine.ServiceTask
	status string
}

func (h healthFake) ServiceTasks(context.Context, string) ([]engine.ServiceTask, error) {
	return h.tasks, nil
}
func (h healthFake) InspectContainerHealth(context.Context, string) (string, error) {
	return h.status, nil
}
func TestHealthRejectsStartingContainer(t *testing.T) {
	e := healthFake{tasks: []engine.ServiceTask{{ID: "t", ServiceID: "s", ContainerID: "c", State: "running", DesiredState: "running", Slot: 1, Labels: map[string]string{"berthy.operation": "op"}}}, status: "starting"}
	if healthyOnce(context.Background(), e, "s", "op", 1) == nil {
		t.Fatal("starting task counted healthy")
	}
}

func TestHealthRejectsOldGeneration(t *testing.T) {
	e := healthFake{tasks: []engine.ServiceTask{{ID: "t", ServiceID: "s", ContainerID: "c", State: "running", DesiredState: "running", Slot: 1, Labels: map[string]string{"berthy.operation": "old"}}}, status: "healthy"}
	if healthyOnce(context.Background(), e, "s", "new", 1) == nil {
		t.Fatal("old task counted healthy")
	}
}

func TestHealthWaitHonorsDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	if err := waitHealthy(ctx, healthFake{}, "s", "op", 1); err == nil {
		t.Fatal("unhealthy service passed")
	}
}
