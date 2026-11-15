package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
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
	creates       int
	updateVersion uint64
	live          engine.ServiceSnapshot
	inspectErr    error
	tasks         []engine.ServiceTask
}

func (m *mutationFake) InspectService(context.Context, string) (engine.ServiceSnapshot, error) {
	return m.live, m.inspectErr
}
func (m *mutationFake) CreateService(context.Context, json.RawMessage, *engine.RegistryAuth) (string, error) {
	m.creates++
	return "id", nil
}
func (m *mutationFake) UpdateService(_ context.Context, _ string, version uint64, spec json.RawMessage, _ *engine.RegistryAuth) error {
	m.updateVersion = version
	m.live.Spec = spec
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
	b, err := serviceSpec("env", "proj", "app", "web", "operation", "network", "registry/image@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", nil, r, "10s", nil)
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

func TestCreatedSpecRejectsHostPrivileges(t *testing.T) {
	desired := json.RawMessage(`{"Name":"svc","TaskTemplate":{"ContainerSpec":{"Image":"x","Labels":{"berthy.operation":"op"}}}}`)
	live := json.RawMessage(`{"Name":"svc","TaskTemplate":{"ContainerSpec":{"Image":"x","Labels":{"berthy.operation":"op"},"Privileges":{"CredentialSpec":{"File":"evil"}}}}}`)
	if matchesCreatedSpec(desired, live) {
		t.Fatal("host privilege accepted")
	}
}

func TestCreatedSpecAcceptsEngineDefaults(t *testing.T) {
	d := json.RawMessage(`{"Name":"svc","TaskTemplate":{"ContainerSpec":{"Image":"x"}}}`)
	l := json.RawMessage(`{"Name":"svc","TaskTemplate":{"ContainerSpec":{"Image":"x","Isolation":"default"},"Runtime":"container","ForceUpdate":0}}`)
	if !matchesCreatedSpec(d, l) {
		t.Fatal("engine defaults differ")
	}
}

func TestServiceSpecUsesNamedVolumesAndResourceLimits(t *testing.T) {
	one := 1
	r := manifest.Role{Replicas: &one, Health: &manifest.Health{Test: []string{"CMD", "true"}}, Volumes: []string{"data:/data:ro"}, Resources: &manifest.Resources{CPU: "0.5", Memory: "64MiB"}, Port: 8080}
	b, err := serviceSpec("env", "proj", "app", "web", "op", "network", "image", nil, r, "10s", nil)
	if err != nil {
		t.Fatal(err)
	}
	var spec map[string]any
	if err = json.Unmarshal(b, &spec); err != nil {
		t.Fatal(err)
	}
	task := spec["TaskTemplate"].(map[string]any)
	container := task["ContainerSpec"].(map[string]any)
	mount := container["Mounts"].([]any)[0].(map[string]any)
	if mount["Type"] != "volume" || mount["ReadOnly"] != true {
		t.Fatalf("mount: %v", mount)
	}
	limits := task["Resources"].(map[string]any)["Limits"].(map[string]any)
	if limits["NanoCPUs"] != float64(500000000) || limits["MemoryBytes"] != float64(67108864) {
		t.Fatalf("limits: %v", limits)
	}
}

func TestServiceSpecInheritsImageHealth(t *testing.T) {
	b, err := serviceSpec("env", "proj", "app", "web", "op", "network", "image", nil, manifest.Role{}, "10s", json.RawMessage(`{"Test":["CMD","true"],"Interval":1000000000}`))
	if err != nil || !strings.Contains(string(b), `"Healthcheck"`) {
		t.Fatalf("image health: %s %v", b, err)
	}
}

func TestCreateRoleJournalsAndVerifiesHealthyGeneration(t *testing.T) {
	desired := json.RawMessage(`{"Name":"svc","TaskTemplate":{"ContainerSpec":{"Image":"x","Labels":{"berthy.operation":"op"}}}}`)
	m := &mutationFake{live: engine.ServiceSnapshot{ID: "id", Spec: desired}, tasks: []engine.ServiceTask{{ServiceID: "id", ContainerID: "container", State: "running", DesiredState: "running", Slot: 1, Labels: map[string]string{"berthy.operation": "op"}}}}
	j := &journalFake{}
	id, err := createRole(context.Background(), j, m, "op", state.Intent{Kind: "create", Name: "svc", Generation: "op", Desired: desired}, nil, 1)
	if err != nil || id != "id" || len(j.saved) != 1 || m.creates != 1 {
		t.Fatalf("id=%q err=%v intents=%d creates=%d", id, err, len(j.saved), m.creates)
	}
}

func (m *mutationFake) ServiceTasks(context.Context, string) ([]engine.ServiceTask, error) {
	return m.tasks, nil
}
func (m *mutationFake) InspectContainerHealth(context.Context, string) (string, error) {
	return "healthy", nil
}

func TestSpecComparisonDetectsLargeIntegerDrift(t *testing.T) {
	if sameSpec(json.RawMessage(`{"value":9007199254740992}`), json.RawMessage(`{"value":9007199254740993}`)) {
		t.Fatal("integer drift hidden")
	}
}

func TestUpdateRejectsInheritedHostMount(t *testing.T) {
	live := json.RawMessage(`{"Name":"svc","TaskTemplate":{"ContainerSpec":{"Image":"old","Mounts":[{"Type":"bind","Source":"/","Target":"/host"}]},"Networks":[{"Target":"net"}]},"Mode":{"Replicated":{"Replicas":1}}}`)
	desired := json.RawMessage(`{"Name":"svc","TaskTemplate":{"ContainerSpec":{"Image":"new"},"Networks":[{"Target":"net"}]},"Mode":{"Replicated":{"Replicas":1}}}`)
	if _, err := mergeServiceSpec(live, desired, "net"); err == nil {
		t.Fatal("host bind accepted")
	}
}

func TestUpdateRoleUsesCurrentVersion(t *testing.T) {
	before := json.RawMessage(`{"Name":"svc","version":"old"}`)
	desired := json.RawMessage(`{"Name":"svc","version":"new"}`)
	m := &mutationFake{live: engine.ServiceSnapshot{ID: "id", Version: 9, Spec: before}, tasks: []engine.ServiceTask{{ServiceID: "id", ContainerID: "c", State: "running", DesiredState: "running", Slot: 1, Labels: map[string]string{"berthy.operation": "op"}}}}
	j := &journalFake{}
	err := updateRole(context.Background(), j, m, "op", state.Intent{Kind: "update", Name: "svc", ServiceID: "id", Generation: "op", Before: before, Desired: desired}, nil, 1)
	if err != nil || m.updateVersion != 9 {
		t.Fatalf("update version %d: %v", m.updateVersion, err)
	}
}

func TestDrainWaitsForRetiringTask(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	e := healthFake{tasks: []engine.ServiceTask{{ServiceID: "s", State: "running", Labels: map[string]string{"berthy.operation": "old"}}}}
	if waitDrain(ctx, e, "s", "new") == nil {
		t.Fatal("old task still running")
	}
}
