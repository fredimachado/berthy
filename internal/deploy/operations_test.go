package deploy

import (
	"berthy/internal/engine"
	"berthy/internal/scope"
	"berthy/internal/state"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
	zero := 0
	desired, n, err = runtimeSpec(original, binding, "stage", "op", "scale", &zero)
	if err != nil || n != 0 {
		t.Fatalf("scale to zero: %d %v", n, err)
	}
	var stopped map[string]any
	if json.Unmarshal(desired, &stopped) != nil || stopped["Mode"].(map[string]any)["Replicated"].(map[string]any)["Replicas"] != float64(0) {
		t.Fatalf("scale to zero kept the count: %s", desired)
	}
	idle := json.RawMessage(strings.Replace(string(original), `"Replicas":2`, `"Replicas":0`, 1))
	desired, n, err = runtimeSpec(idle, binding, "stage", "op", "restart", nil)
	if err == nil || err.Error() != "role is stopped" {
		t.Fatalf("restart at zero: %d %v", n, err)
	}
	two := 2
	desired, n, err = runtimeSpec(idle, binding, "stage", "op", "scale", &two)
	if err != nil || n != 2 {
		t.Fatalf("scale from zero: %d %v", n, err)
	}
}
func TestRuntimeRejectsForeignIdentityAndBadCount(t *testing.T) {
	b := state.Binding{App: "app", Role: "web", Name: "name"}
	own := json.RawMessage(`{"Name":"name","TaskTemplate":{"ContainerSpec":{}},"Mode":{"Replicated":{"Replicas":1}}}`)
	foreign := json.RawMessage(`{"Name":"other","TaskTemplate":{"ContainerSpec":{}},"Mode":{"Replicated":{"Replicas":1}}}`)
	one, negative, tooMany := 1, -1, 1001
	for _, tc := range []struct {
		name  string
		spec  json.RawMessage
		count *int
		want  string
	}{
		{"foreign identity", foreign, &one, "service identity mismatch"},
		{"negative count", own, &negative, "invalid replica count"},
		{"count above limit", own, &tooMany, "invalid replica count"},
		{"missing count", own, nil, "invalid replica count"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := runtimeSpec(tc.spec, b, "stage", "op", "scale", tc.count); err == nil || err.Error() != tc.want {
				t.Fatalf("want %q, got %v", tc.want, err)
			}
		})
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
			t.Cleanup(func() { s.Close() })
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
func TestScaleToZeroStopsTheRole(t *testing.T) {
	dir := t.TempDir()
	os.Chmod(dir, 0700)
	key := filepath.Join(dir, "key")
	os.WriteFile(key, make([]byte, 32), 0600)
	s, err := state.Open(dir, key, "stage")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	ctx := context.Background()
	first, _, err := s.Admit(ctx, state.Admission{RequestID: "first", ActorID: "a", Kind: "deploy", App: "app", PublicRequest: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Finish(ctx, first.ID, state.Outcome{State: "succeeded", Release: &state.Release{App: "app", Manifest: []byte(`{}`), Bindings: []state.Binding{{App: "app", Role: "web", Name: "app-web", ServiceID: "id"}}}}); err != nil {
		t.Fatal(err)
	}
	op, _, err := s.Admit(ctx, state.Admission{RequestID: "scale", ActorID: "a", Kind: "scale", App: "app", PublicRequest: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	f := &runtimeEngine{store: s, op: op.ID, spec: json.RawMessage(`{"Name":"app-web","TaskTemplate":{"ContainerSpec":{"Image":"example/app:tag"}},"Mode":{"Replicated":{"Replicas":1}}}`)}
	c := Controller{Store: s, Engine: f, Environment: "stage"}
	zero := 0
	out := c.runOperation(ctx, op, Actor{}, Request{Kind: "scale", App: "app", Replicas: &zero})
	if out.State != "succeeded" || f.updates != 1 {
		t.Fatalf("%s %s updates=%d", out.State, out.Err, f.updates)
	}
	var stopped map[string]any
	if json.Unmarshal(f.spec, &stopped) != nil || stopped["Mode"].(map[string]any)["Replicated"].(map[string]any)["Replicas"] != float64(0) {
		t.Fatalf("role not stopped: %s", f.spec)
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
	t.Cleanup(func() { s.Close() })
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
	containersErr  error
	servicesErr    error
}

func (f *pruneEngine) InventoryImages(context.Context) ([]engine.InventoryImage, error) {
	if f.inventoryError {
		return nil, errors.New("unavailable")
	}
	return f.images, nil
}
func (f *pruneEngine) InventoryContainers(context.Context) ([]engine.InventoryContainer, error) {
	if f.containersErr != nil {
		return nil, f.containersErr
	}
	return f.containers, nil
}
func (f *pruneEngine) InventoryServices(context.Context) ([]engine.InventoryService, error) {
	if f.servicesErr != nil {
		return nil, f.servicesErr
	}
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
	t.Cleanup(func() { s.Close() })
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

// TestPruneLeavesTheHelperAlone checks that prune cannot select the overlay helper: its image was never acquired by a
// deploy, so it is not a candidate, and its container is not a journaled hook. Only the acquired image goes.
func TestPruneLeavesTheHelperAlone(t *testing.T) {
	dir := t.TempDir()
	os.Chmod(dir, 0700)
	key := filepath.Join(dir, "key")
	os.WriteFile(key, make([]byte, 32), 0600)
	s, err := state.Open(dir, key, "stage")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	ctx := context.Background()
	deploy, _, err := s.Admit(ctx, state.Admission{RequestID: "a", ActorID: "a", Kind: "deploy", App: "app", PublicRequest: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	// Berthy records an image only while the operation that pulled it runs.
	if err = s.RecordImageAcquired(ctx, "old", "id-old"); err != nil {
		t.Fatal(err)
	}
	if err = s.Finish(ctx, deploy.ID, state.Outcome{State: "succeeded"}); err != nil {
		t.Fatal(err)
	}
	op, _, err := s.Admit(ctx, state.Admission{RequestID: "p", ActorID: "a", Kind: "prune", PublicRequest: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	f := &pruneEngine{runtimeEngine: &runtimeEngine{}, images: []engine.InventoryImage{
		{ID: "id-old", RepoDigests: []string{"old"}},
		{ID: "id-helper", RepoTags: []string{"berthy-helper:7da690fcc280"}},
	}, containers: []engine.InventoryContainer{
		{ID: "helper", ImageID: "id-helper", Labels: map[string]string{"berthy.helper": "probe", "berthy.environment": "stage"}},
	}}
	c := Controller{Store: s, Engine: f, Environment: "stage"}
	out := c.runOperation(ctx, op, Actor{Admin: true}, Request{Kind: "prune"})
	if out.State != "succeeded" || len(f.removed) != 1 || f.removed[0] != "old" {
		t.Fatalf("outcome %v removed %v", out, f.removed)
	}
}

func TestPruneOnlyStoppedMatchingHook(t *testing.T) {
	h := state.CompletedOneOff{ID: "one", OperationID: "op", Role: "pre_hook"}
	good := engine.ContainerSnapshot{ID: "one", Labels: map[string]string{"berthy.environment": "stage", "berthy.operation": "op", "berthy.oneoff": "pre_hook"}}
	if !ownedFinishedHook(good, h, scope.ID{Environment: "stage"}) {
		t.Fatal("owned stopped hook rejected")
	}
	good.Running = true
	if ownedFinishedHook(good, h, scope.ID{Environment: "stage"}) {
		t.Fatal("running hook accepted")
	}
	good.Running = false
	good.ID = "other"
	if ownedFinishedHook(good, h, scope.ID{Environment: "stage"}) {
		t.Fatal("foreign container accepted")
	}
}

func TestAFinishedHookOfAnotherEnvironmentIsNotOurs(t *testing.T) {
	h := state.CompletedOneOff{ID: "one", OperationID: "op", Role: "pre_hook"}
	for _, labels := range []map[string]string{
		{"berthy.project": "blog", "berthy.environment": "stage", "berthy.operation": "op", "berthy.oneoff": "pre_hook"},
		{"berthy.project": "shop", "berthy.environment": "prod", "berthy.operation": "op", "berthy.oneoff": "pre_hook"},
	} {
		if ownedFinishedHook(engine.ContainerSnapshot{ID: "one", Labels: labels}, h, scope.ID{Project: "shop", Environment: "stage"}) {
			t.Errorf("foreign hook accepted: %v", labels)
		}
	}
}

type hookPruneEngine struct {
	*pruneEngine
	container engine.ContainerSnapshot
	deleted   []string
}

func (f *hookPruneEngine) InspectContainer(context.Context, string) (engine.ContainerSnapshot, error) {
	return f.container, nil
}
func (f *hookPruneEngine) RemoveContainer(_ context.Context, id string) error {
	f.deleted = append(f.deleted, id)
	return nil
}
func TestPruneRemovesOnlyJournaledStoppedHook(t *testing.T) {
	dir := t.TempDir()
	os.Chmod(dir, 0700)
	key := filepath.Join(dir, "key")
	os.WriteFile(key, make([]byte, 32), 0600)
	s, err := state.Open(dir, key, "stage")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	ctx := context.Background()
	previous, _, err := s.Admit(ctx, state.Admission{RequestID: "h", ActorID: "a", Kind: "deploy", PublicRequest: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RecordIntent(ctx, previous.ID, state.Intent{Ordinal: 10000, Kind: "oneoff", Role: "pre_hook", Name: "owned", Desired: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if err = s.MarkStep(ctx, previous.ID, 10000, "finished", []byte(`{"id":"hook","exit":0}`)); err != nil {
		t.Fatal(err)
	}
	if err = s.Finish(ctx, previous.ID, state.Outcome{State: "failed"}); err != nil {
		t.Fatal(err)
	}
	op, _, err := s.Admit(ctx, state.Admission{RequestID: "p", ActorID: "a", Kind: "prune", PublicRequest: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	f := &hookPruneEngine{pruneEngine: &pruneEngine{runtimeEngine: &runtimeEngine{}}, container: engine.ContainerSnapshot{ID: "hook", Labels: map[string]string{"berthy.environment": "stage", "berthy.operation": previous.ID, "berthy.oneoff": "pre_hook"}}}
	c := Controller{Store: s, Engine: f, Environment: "stage"}
	out := c.runOperation(ctx, op, Actor{Admin: true}, Request{Kind: "prune"})
	if out.State != "succeeded" || len(f.deleted) != 1 || f.deleted[0] != "hook" {
		t.Fatalf("outcome %v deleted %v", out, f.deleted)
	}
}
func TestWaitWithoutRoleChecksAllBindings(t *testing.T) {
	dir := t.TempDir()
	os.Chmod(dir, 0700)
	key := filepath.Join(dir, "key")
	os.WriteFile(key, make([]byte, 32), 0600)
	s, err := state.Open(dir, key, "stage")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	ctx := context.Background()
	op, _, err := s.Admit(ctx, state.Admission{RequestID: "a", ActorID: "a", Kind: "deploy", App: "app", PublicRequest: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	bindings := []state.Binding{{App: "app", Role: "a", Name: "one", ServiceID: "id"}, {App: "app", Role: "b", Name: "two", ServiceID: "id2"}}
	if err = s.Finish(ctx, op.ID, state.Outcome{State: "succeeded", Release: &state.Release{App: "app", Manifest: []byte(`{}`), Bindings: bindings}}); err != nil {
		t.Fatal(err)
	}
	fake := &runtimeEngine{op: "generation", spec: []byte(`{"Name":"one","TaskTemplate":{"ContainerSpec":{"Labels":{"berthy.operation":"generation"}}},"Mode":{"Replicated":{"Replicas":1}}}`)}
	c := Controller{Store: s, Engine: fake}
	deadline, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	if err = c.Wait(deadline, "app", ""); err == nil || strings.Contains(err.Error(), "role required") {
		t.Fatalf("did not inspect each role: %v", err)
	}
}

// waitFixture is a finished release whose one web role is bound to service "one" with ServiceID "id".
func waitFixture(t *testing.T) *state.Store {
	t.Helper()
	s := openStore(t)
	ctx := context.Background()
	op, _, err := s.Admit(ctx, state.Admission{RequestID: "a", ActorID: "a", Kind: "deploy", App: "app", PublicRequest: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	bindings := []state.Binding{{App: "app", Role: "web", Name: "one", ServiceID: "id"}}
	if err = s.Finish(ctx, op.ID, state.Outcome{State: "succeeded", Release: &state.Release{App: "app", Manifest: []byte(`{}`), Bindings: bindings}}); err != nil {
		t.Fatal(err)
	}
	return s
}

// A role stopped by scale --replicas 0 has no replicas and no berthy.operation label, and is ready at once.
func TestWaitReturnsAtOnceForStoppedRole(t *testing.T) {
	c := Controller{Store: waitFixture(t), Engine: &runtimeEngine{spec: []byte(`{"Name":"one","TaskTemplate":{"ContainerSpec":{}},"Mode":{"Replicated":{"Replicas":0}}}`)}}
	deadline, cancel := context.WithTimeout(context.Background(), testWait)
	defer cancel()
	started := time.Now()
	if err := c.Wait(deadline, "app", ""); err != nil {
		t.Fatalf("stopped role not ready: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Fatalf("stopped role waited %v", elapsed)
	}
}

func TestWaitRefusesMissingOrNegativeReplicas(t *testing.T) {
	for name, spec := range map[string]string{
		"no mode":           `{"Name":"one","TaskTemplate":{"ContainerSpec":{}}}`,
		"negative replicas": `{"Name":"one","TaskTemplate":{"ContainerSpec":{}},"Mode":{"Replicated":{"Replicas":-1}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			c := Controller{Store: waitFixture(t), Engine: &runtimeEngine{spec: []byte(spec)}}
			deadline, cancel := context.WithTimeout(context.Background(), testWait)
			defer cancel()
			if err := c.Wait(deadline, "app", ""); err == nil || err.Error() != "invalid bound service" {
				t.Fatalf("got %v, want invalid bound service", err)
			}
		})
	}
}

func pruneFixture(t *testing.T) (*state.Store, state.Operation) {
	t.Helper()
	s := openStore(t)
	ctx := context.Background()
	deploy, _, err := s.Admit(ctx, state.Admission{RequestID: "a", ActorID: "a", Kind: "deploy", App: "app", PublicRequest: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range []state.AcquiredImage{{Ref: "old", ID: "id-old"}, {Ref: "prev", ID: "id-prev"}, {Ref: "current", ID: "id-current"}} {
		if err = s.RecordImageAcquired(ctx, x.Ref, x.ID); err != nil {
			t.Fatal(err)
		}
	}
	if err = s.Finish(ctx, deploy.ID, state.Outcome{State: "succeeded"}); err != nil {
		t.Fatal(err)
	}
	op, _, err := s.Admit(ctx, state.Admission{RequestID: "p", ActorID: "a", Kind: "prune", PublicRequest: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	return s, op
}

// A service whose previous spec names an image that is gone protects nothing it cannot resolve, and aborts nothing: every
// image a container or a service spec still resolves to stays.
func TestPruneProceedsPastAnUnresolvableServiceImage(t *testing.T) {
	s, op := pruneFixture(t)
	spec := func(image string) json.RawMessage {
		return json.RawMessage(fmt.Sprintf(`{"TaskTemplate":{"ContainerSpec":{"Image":%q}}}`, image))
	}
	f := &pruneEngine{runtimeEngine: &runtimeEngine{}, images: []engine.InventoryImage{
		{ID: "id-old", RepoDigests: []string{"old"}}, {ID: "id-prev", RepoDigests: []string{"prev"}}, {ID: "id-current", RepoDigests: []string{"current"}},
	}, services: []engine.InventoryService{
		{ID: "unmanaged", Spec: spec("current"), PreviousSpec: spec("gone-image@sha256:aaaa")},
		{ID: "gone-current", Spec: spec("also-gone"), PreviousSpec: spec("prev")},
	}}
	c := Controller{Store: s, Engine: f, Environment: "stage"}
	out := c.runOperation(context.Background(), op, Actor{Admin: true}, Request{Kind: "prune"})
	if out.State != "succeeded" || len(f.removed) != 1 || f.removed[0] != "old" {
		t.Fatalf("outcome %+v removed %v: only the unreferenced image may go", out, f.removed)
	}
}

func TestPruneFailureCategoriesAreDistinct(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(*pruneEngine)
		want string
	}{
		{"images", func(f *pruneEngine) { f.inventoryError = true }, "image inventory unavailable"},
		{"containers", func(f *pruneEngine) { f.containersErr = errors.New("boom") }, "container inventory unavailable"},
		{"services", func(f *pruneEngine) { f.servicesErr = errors.New("boom") }, "service inventory unavailable"},
		{"docker down", func(f *pruneEngine) { f.containersErr = errors.New("Docker unavailable: dial unix") }, "Docker unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, op := pruneFixture(t)
			f := &pruneEngine{runtimeEngine: &runtimeEngine{}}
			tc.set(f)
			c := Controller{Store: s, Engine: f, Environment: "stage"}
			out := c.runOperation(context.Background(), op, Actor{Admin: true}, Request{Kind: "prune"})
			if out.State != "failed" || out.Err != tc.want || len(f.removed) != 0 {
				t.Fatalf("outcome %+v removed %v, want %q", out, f.removed, tc.want)
			}
		})
	}
}

func TestExecPlanNamesTheCommandSoAPersonCanReviewIt(t *testing.T) {
	dir := t.TempDir()
	os.Chmod(dir, 0700)
	key := filepath.Join(dir, "key")
	os.WriteFile(key, make([]byte, 32), 0600)
	s, err := state.Open(dir, key, "stage")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	ctx := context.Background()
	first, _, err := s.Admit(ctx, state.Admission{RequestID: "first", ActorID: "a", Kind: "deploy", App: "app", PublicRequest: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Finish(ctx, first.ID, state.Outcome{State: "succeeded", Release: &state.Release{App: "app", Manifest: []byte(`{}`), Bindings: []state.Binding{{App: "app", Role: "web", Name: "app-web", ServiceID: "id"}}}}); err != nil {
		t.Fatal(err)
	}
	spec := fmt.Sprintf(`{"Name":"app-web","TaskTemplate":{"ContainerSpec":{"Labels":{"berthy.operation":%q}}}}`, first.ID)
	c := Controller{Store: s, Engine: &runtimeEngine{store: s, op: first.ID, spec: []byte(spec)}, Environment: "stage"}
	plan, err := c.Plan(ctx, Actor{Admin: true}, Request{Kind: "exec", App: "app", Command: []string{"rm", "-rf", "/data"}})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := json.Marshal(plan.Command); string(got) != `["rm","-rf","/data"]` || plan.App != "app" {
		t.Fatalf("plan %+v", plan)
	}
	if plan.Exec == nil || plan.Exec.Target != "app-web" {
		t.Fatalf("an image app's exec plan names the role's service: %+v", plan.Exec)
	}
}
