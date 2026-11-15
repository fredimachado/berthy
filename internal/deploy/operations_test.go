package deploy

import (
	"berthy/internal/engine"
	"berthy/internal/state"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRuntimeChangePreservesSpecAndSetsGeneration(t *testing.T) {
	original := json.RawMessage(`{"Name":"app-web","Labels":{"berthy.app":"app","custom":"keep"},"TaskTemplate":{"ForceUpdate":7,"ContainerSpec":{"Image":"image@sha256:aa","Env":["TOKEN=hidden"],"Labels":{"berthy.app":"app"}}},"Mode":{"Replicated":{"Replicas":2}}}`)
	binding := state.Binding{App: "app", Role: "web", Name: "app-web", ServiceID: "id"}
	desired, n, err := runtimeSpec(original, binding, "stage", "op", "restart", nil)
	if err != nil || n != 2 {
		t.Fatalf("restart: %d %v", n, err)
	}
	var v map[string]any
	if json.Unmarshal(desired, &v) != nil {
		t.Fatal("invalid spec")
	}
	task := v["TaskTemplate"].(map[string]any)
	if task["ForceUpdate"] != float64(8) || task["ContainerSpec"].(map[string]any)["Labels"].(map[string]any)["berthy.operation"] != "op" || v["Labels"].(map[string]any)["custom"] != "keep" {
		t.Fatalf("lost values: %s", desired)
	}
	count := 3
	desired, n, err = runtimeSpec(original, binding, "stage", "op", "scale", &count)
	if err != nil || n != 3 || !json.Valid(desired) {
		t.Fatalf("scale %d %v", n, err)
	}
}
func TestRuntimeRejectsForeignIdentityAndBadCount(t *testing.T) {
	b := state.Binding{App: "app", Role: "web", Name: "name"}
	spec := json.RawMessage(`{"Name":"other","TaskTemplate":{"ContainerSpec":{}},"Mode":{"Replicated":{"Replicas":1}}}`)
	n := 0
	if _, _, err := runtimeSpec(spec, b, "stage", "op", "scale", &n); err == nil {
		t.Fatal("accepted invalid service/count")
	}
}

type runtimeEngine struct {
	Engine
	store   *state.Store
	op      string
	spec    json.RawMessage
	updates int
	fail    bool
}

func (f *runtimeEngine) InspectService(context.Context, string) (engine.ServiceSnapshot, error) {
	return engine.ServiceSnapshot{ID: "id", Version: 9, Spec: f.spec}, nil
}
func (f *runtimeEngine) UpdateService(ctx context.Context, id string, v uint64, spec json.RawMessage, auth *engine.RegistryAuth) error {
	intents, err := f.store.Intents(ctx, f.op)
	if err != nil || len(intents) != 1 || string(intents[0].Before) != string(f.spec) {
		return errors.New("update without durable preimage")
	}
	f.updates++
	if f.fail {
		return errors.New("lost response")
	}
	f.spec = spec
	return nil
}
func (f *runtimeEngine) CreateService(context.Context, json.RawMessage, *engine.RegistryAuth) (string, error) {
	return "", errors.New("unexpected create")
}
func (f *runtimeEngine) DeleteService(context.Context, string) error {
	return errors.New("unexpected delete")
}
func (f *runtimeEngine) ServiceTasks(context.Context, string) ([]engine.ServiceTask, error) {
	return []engine.ServiceTask{{ServiceID: "id", DesiredState: "running", State: "running", Slot: 1, ContainerID: "container", Labels: map[string]string{"berthy.operation": f.op}}}, nil
}
func (f *runtimeEngine) InspectContainerHealth(context.Context, string) (string, error) {
	return "healthy", nil
}
func TestRestartJournalAndRecoveryLock(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint(fail), func(t *testing.T) {
			dir := t.TempDir()
			os.Chmod(dir, 0700)
			key := filepath.Join(dir, "key")
			os.WriteFile(key, make([]byte, 32), 0600)
			s, err := state.Open(dir, key, "stage")
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			ctx := context.Background()
			first, _, err := s.Admit(ctx, state.Admission{RequestID: "first", ActorID: "a", Kind: "deploy", App: "app", PublicRequest: []byte(`{}`)})
			if err != nil {
				t.Fatal(err)
			}
			if err = s.Finish(ctx, first.ID, state.Outcome{State: "succeeded", Release: &state.Release{App: "app", Manifest: []byte(`{}`), Bindings: []state.Binding{{App: "app", Role: "web", Name: "app-web", ServiceID: "id"}}}}); err != nil {
				t.Fatal(err)
			}
			op, _, err := s.Admit(ctx, state.Admission{RequestID: "restart", ActorID: "a", Kind: "restart", App: "app", PublicRequest: []byte(`{}`)})
			if err != nil {
				t.Fatal(err)
			}
			f := &runtimeEngine{store: s, op: op.ID, fail: fail, spec: json.RawMessage(`{"Name":"app-web","TaskTemplate":{"ContainerSpec":{"Image":"example/app:tag"}},"Mode":{"Replicated":{"Replicas":1}}}`)}
			c := Controller{Store: s, Engine: f, Environment: "stage"}
			out := c.runOperation(ctx, op, Actor{}, Request{Kind: "restart", App: "app"})
			want := "succeeded"
			if fail {
				want = "recovery_required"
			}
			if out.State != want || f.updates != 1 {
				t.Fatalf("%s updates=%d", out.State, f.updates)
			}
			if err = s.Finish(ctx, op.ID, out); err != nil {
				t.Fatal(err)
			}
			_, err = s.Active(ctx)
			if fail && err != nil {
				t.Fatal("recovery lock released")
			}
			if !fail && err == nil {
				t.Fatal("completed lock remains")
			}
		})
	}
}
func TestRedactExecOutputBeforeTruncation(t *testing.T) {
	text := strings.Repeat("x", 65530) + "supersecret"
	got := redactOutput([]byte(text), [][]byte{[]byte("supersecret")})
	if strings.Contains(got, "super") || len(got) > 65536 {
		t.Fatal("secret or oversized output")
	}
}
func TestPruneKeepsReferencedImages(t *testing.T) {
	owned := []state.AcquiredImage{{Ref: "old", ID: "id-old"}, {Ref: "keep", ID: "id-keep"}, {Ref: "foreign", ID: "id-container"}}
	got := pruneCandidates(owned, map[string]bool{"keep": true}, map[string]bool{"id-container": true})
	if len(got) != 1 || got[0].Ref != "old" {
		t.Fatalf("unexpected candidates: %v", got)
	}
}
func (f *runtimeEngine) InspectContainer(context.Context, string) (engine.ContainerSnapshot, error) {
	return engine.ContainerSnapshot{ID: "container", Running: true, Labels: map[string]string{"berthy.operation": f.op}}, nil
}
func (f *runtimeEngine) Exec(context.Context, string, []string) (engine.ExecResult, error) {
	return engine.ExecResult{Output: []byte("token=secret-value"), ExitCode: 7}, nil
}
func TestExecStoresRedactedOutputPrivately(t *testing.T) {
	dir := t.TempDir()
	os.Chmod(dir, 0700)
	key := filepath.Join(dir, "key")
	os.WriteFile(key, make([]byte, 32), 0600)
	s, err := state.Open(dir, key, "stage")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	if err = s.PutSecret(ctx, state.Actor{ID: "a"}, state.SecretInfo{Name: "TOKEN"}, []byte("secret-value")); err != nil {
		t.Fatal(err)
	}
	first, _, err := s.Admit(ctx, state.Admission{RequestID: "first", ActorID: "a", Kind: "deploy", App: "app", PublicRequest: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Finish(ctx, first.ID, state.Outcome{State: "succeeded", Release: &state.Release{App: "app", Manifest: []byte(`{}`), Bindings: []state.Binding{{App: "app", Role: "web", Name: "app-web", ServiceID: "id"}}}}); err != nil {
		t.Fatal(err)
	}
	op, _, err := s.Admit(ctx, state.Admission{RequestID: "exec", ActorID: "a", Kind: "exec", App: "app", PublicRequest: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	spec := fmt.Sprintf(`{"Name":"app-web","TaskTemplate":{"ContainerSpec":{"Labels":{"berthy.operation":%q}}}}`, op.ID)
	f := &runtimeEngine{store: s, op: op.ID, spec: []byte(spec)}
	c := Controller{Store: s, Engine: f, Environment: "stage"}
	out := c.runExec(ctx, op, Actor{Admin: true}, Request{App: "app", Command: []string{"echo", "one arg"}})
	if out.State != "failed" {
		t.Fatalf("outcome %v", out)
	}
	steps, err := s.Intents(ctx, op.ID)
	if err != nil || len(steps) != 1 || strings.Contains(string(steps[0].Receipt), "secret-value") || !strings.Contains(string(steps[0].Receipt), "[redacted]") {
		t.Fatalf("receipt %v %v", steps, err)
	}
	events, err := s.EventsAfter(ctx, op.ID, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if strings.Contains(string(e.Data), "token=") {
			t.Fatal("output in public event")
		}
	}
}

type pruneEngine struct {
	*runtimeEngine
	images         []engine.InventoryImage
	containers     []engine.InventoryContainer
	services       []engine.InventoryService
	removed        []string
	inventoryError bool
}

func (f *pruneEngine) InventoryImages(context.Context) ([]engine.InventoryImage, error) {
	if f.inventoryError {
		return nil, errors.New("unavailable")
	}
	return f.images, nil
}
func (f *pruneEngine) InventoryContainers(context.Context) ([]engine.InventoryContainer, error) {
	return f.containers, nil
}
func (f *pruneEngine) InventoryServices(context.Context) ([]engine.InventoryService, error) {
	return f.services, nil
}
func (f *pruneEngine) InspectImage(_ context.Context, ref string) (engine.Image, error) {
	for _, im := range f.images {
		for _, r := range im.RepoDigests {
			if r == ref {
				return engine.Image{ID: im.ID}, nil
			}
		}
	}
	return engine.Image{}, errors.New("missing")
}
func (f *pruneEngine) RemoveImageRef(_ context.Context, ref string) error {
	f.removed = append(f.removed, ref)
	return nil
}
func TestPruneProtectsContainersAndRetainedReleases(t *testing.T) {
	dir := t.TempDir()
	os.Chmod(dir, 0700)
	key := filepath.Join(dir, "key")
	os.WriteFile(key, make([]byte, 32), 0600)
	s, err := state.Open(dir, key, "stage")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	op, _, err := s.Admit(ctx, state.Admission{RequestID: "a", ActorID: "a", Kind: "deploy", App: "app", PublicRequest: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range []state.AcquiredImage{{Ref: "old", ID: "id-old"}, {Ref: "used", ID: "id-used"}, {Ref: "current", ID: "id-current"}} {
		if err = s.RecordImageAcquired(ctx, x.Ref, x.ID); err != nil {
			t.Fatal(err)
		}
	}
	if err = s.Finish(ctx, op.ID, state.Outcome{State: "succeeded", Release: &state.Release{App: "app", Digest: "current", Manifest: []byte(`{"apps":{"app":{"deploy":{"retain":1}}}}`)}}); err != nil {
		t.Fatal(err)
	}
	op, _, err = s.Admit(ctx, state.Admission{RequestID: "p", ActorID: "a", Kind: "prune", PublicRequest: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	f := &pruneEngine{runtimeEngine: &runtimeEngine{}, images: []engine.InventoryImage{{ID: "id-old", RepoDigests: []string{"old"}}, {ID: "id-used", RepoDigests: []string{"used"}}, {ID: "id-current", RepoDigests: []string{"current"}}}, containers: []engine.InventoryContainer{{ImageID: "id-used"}}}
	c := Controller{Store: s, Engine: f}
	out := c.runOperation(ctx, op, Actor{Admin: true}, Request{Kind: "prune"})
	if out.State != "succeeded" || len(f.removed) != 1 || f.removed[0] != "old" {
		t.Fatalf("outcome %v removed %v", out, f.removed)
	}
}
