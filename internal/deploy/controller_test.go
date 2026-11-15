package deploy

import (
	"berthy/internal/engine"
	"berthy/internal/manifest"
	"berthy/internal/state"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestProtectedPlanRejectsTagBeforeEngineAccess(t *testing.T) {
	c := Controller{Protected: true}
	_, err := c.Plan(context.Background(), Actor{}, Request{App: "web", Version: "latest", Manifest: manifest.Manifest{Project: "example", Network: "private", Apps: map[string]manifest.App{"web": {Kind: "image", Image: "example/web", Roles: map[string]manifest.Role{"web": {}}}}}})
	if err == nil {
		t.Fatal("protected tag was accepted")
	}
}

func TestAdmitCreatesHealthyFirstRelease(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(dir, "key")
	if err := os.WriteFile(key, make([]byte, 32), 0600); err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(dir, key, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	e := &positiveEngine{}
	c := Controller{Engine: e, Store: store, Environment: "test"}
	one := 1
	req := Request{RequestID: "one", App: "web", Version: "sha256:" + strings.Repeat("a", 64), Manifest: manifest.Manifest{Project: "proj", Network: "overlay", Apps: map[string]manifest.App{"web": {Kind: "image", Image: "example/web", Deploy: manifest.Deploy{DeployTimeout: "1s", DrainTimeout: "1s"}, Roles: map[string]manifest.Role{"web": {Replicas: &one, Health: &manifest.Health{Test: []string{"CMD", "true"}}}}}}}}
	op, _, err := c.Admit(context.Background(), Actor{ID: "actor"}, req)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		got, err := store.Operation(context.Background(), op.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.State != "running" {
			if got.State != "succeeded" {
				t.Fatalf("state %s: %s", got.State, got.Err)
			}
			return
		}
		time.Sleep(time.Millisecond * 10)
	}
	t.Fatal("operation did not finish")
}

type positiveEngine struct {
	mu        sync.Mutex
	spec      json.RawMessage
	unhealthy bool
}

func (e *positiveEngine) InspectNetwork(context.Context, string) (engine.Network, error) {
	return engine.Network{ID: "net", Driver: "overlay", Scope: "swarm", Attachable: true}, nil
}
func (e *positiveEngine) ResolveImage(context.Context, string, *engine.RegistryAuth) (string, error) {
	return "", errors.New("unexpected resolution")
}
func (e *positiveEngine) PullImage(context.Context, string, *engine.RegistryAuth) error { return nil }
func (e *positiveEngine) InspectImage(context.Context, string) (engine.Image, error) {
	return engine.Image{ID: "image"}, nil
}
func (e *positiveEngine) InspectService(context.Context, string) (engine.ServiceSnapshot, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.spec == nil {
		return engine.ServiceSnapshot{}, &engine.StatusError{Code: 404}
	}
	return engine.ServiceSnapshot{ID: "id", Spec: e.spec, UpdateStatus: engine.UpdateStatus{State: "completed"}}, nil
}
func (e *positiveEngine) CreateService(_ context.Context, s json.RawMessage, _ *engine.RegistryAuth) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.spec = append(json.RawMessage(nil), s...)
	return "id", nil
}
func (e *positiveEngine) UpdateService(_ context.Context, _ string, _ uint64, spec json.RawMessage, _ *engine.RegistryAuth) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.spec = append(json.RawMessage(nil), spec...)
	return nil
}
func (e *positiveEngine) DeleteService(context.Context, string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.spec = nil
	return nil
}
func (e *positiveEngine) ServiceTasks(context.Context, string) ([]engine.ServiceTask, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	var s struct {
		Labels       map[string]string
		TaskTemplate struct{ ContainerSpec struct{ Image string } }
	}
	_ = json.Unmarshal(e.spec, &s)
	return []engine.ServiceTask{{ServiceID: "id", ContainerID: "c", State: "running", DesiredState: "running", Slot: 1, Labels: s.Labels, Image: s.TaskTemplate.ContainerSpec.Image}}, nil
}
func (e *positiveEngine) InspectContainerHealth(context.Context, string) (string, error) {
	if e.unhealthy {
		return "starting", nil
	}
	return "healthy", nil
}

func TestUnhealthyFirstReleaseRemovesCreatedService(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(dir, "key")
	if err := os.WriteFile(key, make([]byte, 32), 0600); err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(dir, key, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	e := &positiveEngine{unhealthy: true}
	c := Controller{Engine: e, Store: store, Environment: "test"}
	one := 1
	req := Request{RequestID: "bad", App: "web", Version: "sha256:" + strings.Repeat("a", 64), Manifest: manifest.Manifest{Project: "proj", Network: "overlay", Apps: map[string]manifest.App{"web": {Kind: "image", Image: "example/web", Deploy: manifest.Deploy{DeployTimeout: "100ms", DrainTimeout: "1s"}, Roles: map[string]manifest.Role{"web": {Replicas: &one, Health: &manifest.Health{Test: []string{"CMD", "true"}}}}}}}}
	op, _, err := c.Admit(context.Background(), Actor{ID: "actor"}, req)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		got, err := store.Operation(context.Background(), op.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.State != "running" {
			if got.State != "failed" {
				t.Fatalf("state %s: %s", got.State, got.Err)
			}
			e.mu.Lock()
			defer e.mu.Unlock()
			if e.spec != nil {
				t.Fatal("created service still exists")
			}
			return
		}
		time.Sleep(time.Millisecond * 10)
	}
	t.Fatal("operation did not finish")
}

func TestRecoveryCleansObservedCreate(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(dir, "key")
	if err := os.WriteFile(key, make([]byte, 32), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := state.Open(dir, key, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	op, _, err := s.Admit(context.Background(), state.Admission{RequestID: "r", ActorID: "a", Kind: "deploy", PublicRequest: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	spec := json.RawMessage(`{"Name":"svc"}`)
	if err = s.RecordIntent(context.Background(), op.ID, state.Intent{Kind: "create", Name: "svc", Desired: spec}); err != nil {
		t.Fatal(err)
	}
	if err = s.MarkStep(context.Background(), op.ID, 0, "observed", json.RawMessage(`{"id":"id","spec":{"Name":"svc"}}`)); err != nil {
		t.Fatal(err)
	}
	e := &positiveEngine{spec: spec}
	c := Controller{Engine: e, Store: s}
	if err = c.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, err := s.Operation(context.Background(), op.ID)
	if err != nil || got.State != "interrupted" {
		t.Fatalf("state %s: %v", got.State, err)
	}
}

func TestManagedReleaseUpdatesBoundService(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(dir, "key")
	if err := os.WriteFile(key, make([]byte, 32), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := state.Open(dir, key, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	e := &positiveEngine{}
	c := Controller{Engine: e, Store: s, Environment: "test"}
	one := 1
	req := Request{RequestID: "first", App: "web", Version: "sha256:" + strings.Repeat("a", 64), Manifest: manifest.Manifest{Project: "proj", Network: "overlay", Apps: map[string]manifest.App{"web": {Kind: "image", Image: "example/web", Deploy: manifest.Deploy{DeployTimeout: "1s", DrainTimeout: "1s"}, Roles: map[string]manifest.Role{"web": {Replicas: &one, Health: &manifest.Health{Test: []string{"CMD", "true"}}}}}}}}
	for _, id := range []string{"first", "second"} {
		req.RequestID = id
		if id == "second" {
			req.Version = "sha256:" + strings.Repeat("b", 64)
		}
		op, _, err := c.Admit(context.Background(), Actor{ID: "actor"}, req)
		if err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(2 * time.Second)
		for {
			got, err := s.Operation(context.Background(), op.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.State != "running" {
				if got.State != "succeeded" {
					t.Fatalf("%s: %s %s", id, got.State, got.Err)
				}
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("timed out")
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if !strings.Contains(string(e.spec), strings.Repeat("b", 64)) {
		t.Fatal("updated digest missing")
	}
}

func TestRecoveryKeepsLockOnForeignUpdate(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(dir, "key")
	if err := os.WriteFile(key, make([]byte, 32), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := state.Open(dir, key, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	op, _, err := s.Admit(context.Background(), state.Admission{RequestID: "r", ActorID: "a", Kind: "deploy", App: "web", PublicRequest: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RecordIntent(context.Background(), op.ID, state.Intent{Kind: "update", Name: "svc", ServiceID: "id", Before: json.RawMessage(`{"Name":"old"}`), Desired: json.RawMessage(`{"Name":"new"}`)}); err != nil {
		t.Fatal(err)
	}
	e := &positiveEngine{spec: json.RawMessage(`{"Name":"foreign"}`)}
	c := Controller{Engine: e, Store: s}
	if err = c.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, err := s.Operation(context.Background(), op.ID)
	if err != nil || got.State != "recovery_required" {
		t.Fatalf("state %s: %v", got.State, err)
	}
	lock, err := s.Lock(context.Background())
	if err != nil || lock.OperationID != op.ID {
		t.Fatal("recovery lock released")
	}
}

func TestRollbackSelectsPriorSuccessfulRelease(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(dir, "key")
	if err := os.WriteFile(key, make([]byte, 32), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := state.Open(dir, key, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c := Controller{Store: s}
	app := manifest.Manifest{Project: "p", Network: "n", Apps: map[string]manifest.App{"web": {Kind: "image", Image: "example/web", Roles: map[string]manifest.Role{"web": {}}}}}
	b, _ := json.Marshal(app)
	for i := 0; i < 2; i++ {
		id := strconv.Itoa(i)
		op, _, err := s.Admit(context.Background(), state.Admission{RequestID: id, ActorID: "a", Kind: "deploy", App: "web", PublicRequest: json.RawMessage(`{}`)})
		if err != nil {
			t.Fatal(err)
		}
		err = s.Finish(context.Background(), op.ID, state.Outcome{State: "succeeded", Release: &state.Release{App: "web", Digest: "sha256:" + strings.Repeat(string(rune('a'+i)), 64), Manifest: b}})
		if err != nil {
			t.Fatal(err)
		}
	}
	got, err := c.rollbackRequest(context.Background(), Request{App: "web", Kind: "rollback"})
	if err != nil || got.Version != "sha256:"+strings.Repeat("a", 64) {
		t.Fatalf("rollback %s: %v", got.Version, err)
	}
}

func TestAdoptionRequiresAdminBeforeInspection(t *testing.T) {
	c := Controller{}
	_, err := c.Plan(context.Background(), Actor{}, Request{Adopt: true, App: "web", Manifest: manifest.Manifest{Project: "p", Network: "n", Apps: map[string]manifest.App{"web": {Kind: "image", Image: "example/web", Roles: map[string]manifest.Role{"web": {Service: "existing"}}}}}})
	if err == nil || !strings.Contains(err.Error(), "administrator") {
		t.Fatalf("adoption error: %v", err)
	}
}

func TestRecoveryCleansExactUnacknowledgedCreate(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(dir, "key")
	if err := os.WriteFile(key, make([]byte, 32), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := state.Open(dir, key, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	op, _, err := s.Admit(context.Background(), state.Admission{RequestID: "r", ActorID: "a", Kind: "deploy", PublicRequest: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	spec := json.RawMessage(`{"Name":"svc","Labels":{"berthy.operation":"owned"},"TaskTemplate":{"ContainerSpec":{"Labels":{"berthy.operation":"owned"}}}}`)
	if err = s.RecordIntent(context.Background(), op.ID, state.Intent{Kind: "create", Name: "svc", Generation: "owned", Desired: spec}); err != nil {
		t.Fatal(err)
	}
	e := &positiveEngine{spec: spec}
	c := Controller{Engine: e, Store: s}
	if err = c.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, err := s.Operation(context.Background(), op.ID)
	if err != nil || got.State != "interrupted" {
		t.Fatalf("state %s: %v", got.State, err)
	}
}

func TestAdoptionPlansExistingHealthyPinnedService(t *testing.T) {
	digest := "docker.io/example/web@sha256:" + strings.Repeat("a", 64)
	one := 1
	role := manifest.Role{Service: "existing", Replicas: &one, Health: &manifest.Health{Test: []string{"CMD", "true"}}}
	spec, err := serviceSpec("test", "proj", "web", "web", "prior", "net", digest, nil, role, "1s", nil)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	_ = json.Unmarshal(spec, &body)
	body["Name"] = "existing"
	spec, _ = json.Marshal(body)
	e := &positiveEngine{spec: spec}
	c := Controller{Engine: e, Environment: "test"}
	req := Request{Adopt: true, App: "web", Manifest: manifest.Manifest{Project: "proj", Network: "overlay", Apps: map[string]manifest.App{"web": {Kind: "image", Image: "example/web", Roles: map[string]manifest.Role{"web": role}}}}}
	p, bindings, err := c.planAdoption(context.Background(), req)
	if err != nil || p.Image != digest || len(bindings) != 1 || bindings[0].ServiceID != "id" {
		t.Fatalf("plan %+v bindings %+v: %v", p, bindings, err)
	}
}

func TestRecoveryPreservesVerifiedReleaseAfterPostHookInterruption(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(dir, "key")
	if err := os.WriteFile(key, make([]byte, 32), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := state.Open(dir, key, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	op, _, err := s.Admit(context.Background(), state.Admission{RequestID: "r", ActorID: "a", Kind: "deploy", App: "web", PublicRequest: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	spec := json.RawMessage(`{"Name":"svc","Labels":{"berthy.operation":"owned"},"Mode":{"Replicated":{"Replicas":1}}}`)
	if err = s.RecordIntent(context.Background(), op.ID, state.Intent{Kind: "create", Name: "svc", Generation: "owned", Desired: spec}); err != nil {
		t.Fatal(err)
	}
	if err = s.MarkStep(context.Background(), op.ID, 0, "verified", json.RawMessage(`{"id":"id","spec":{"Name":"svc","Labels":{"berthy.operation":"owned"},"Mode":{"Replicated":{"Replicas":1}}}}`)); err != nil {
		t.Fatal(err)
	}
	out := state.Outcome{State: "succeeded", Release: &state.Release{App: "web", Digest: "digest", Manifest: json.RawMessage(`{}`), Bindings: []state.Binding{{App: "web", Role: "web", Name: "svc", ServiceID: "id"}}}}
	b, _ := json.Marshal(out)
	if err = s.RecordIntent(context.Background(), op.ID, state.Intent{Ordinal: 9999, Kind: "barrier", Desired: b}); err != nil {
		t.Fatal(err)
	}
	e := &positiveEngine{spec: spec}
	c := Controller{Engine: e, Store: s}
	if err = c.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, err := s.Operation(context.Background(), op.ID)
	if err != nil || got.State != "failed" {
		t.Fatalf("state %s: %v", got.State, err)
	}
	if _, err = s.Applied(context.Background(), "web"); err != nil {
		t.Fatal(err)
	}
	if _, err = e.InspectService(context.Background(), "svc"); err != nil {
		t.Fatal("verified service deleted")
	}
}

func TestAdoptionPersistsBindingWithoutServiceWrite(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(dir, "key")
	if err := os.WriteFile(key, make([]byte, 32), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := state.Open(dir, key, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	digest := "docker.io/example/web@sha256:" + strings.Repeat("a", 64)
	one := 1
	role := manifest.Role{Service: "existing", Replicas: &one, Health: &manifest.Health{Test: []string{"CMD", "true"}}}
	spec, err := serviceSpec("test", "proj", "web", "web", "prior", "net", digest, nil, role, "1s", nil)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	_ = json.Unmarshal(spec, &body)
	body["Name"] = "existing"
	spec, _ = json.Marshal(body)
	e := &positiveEngine{spec: append(json.RawMessage(nil), spec...)}
	c := Controller{Engine: e, Store: s, Environment: "test"}
	req := Request{RequestID: "adopt", Adopt: true, App: "web", Manifest: manifest.Manifest{Project: "proj", Network: "overlay", Apps: map[string]manifest.App{"web": {Kind: "image", Image: "example/web", Roles: map[string]manifest.Role{"web": role}}}}}
	op, _, err := c.Admit(context.Background(), Actor{ID: "admin", Admin: true}, req)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		got, err := s.Operation(context.Background(), op.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.State != "running" {
			if got.State != "succeeded" {
				t.Fatalf("%s: %s", got.State, got.Err)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(10 * time.Millisecond)
	}
	b, err := s.Bindings(context.Background(), "web")
	if err != nil || len(b) != 1 || b[0].ServiceID != "id" {
		t.Fatalf("bindings %+v: %v", b, err)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if !sameSpec(e.spec, spec) {
		t.Fatal("adoption changed service")
	}
}

func TestDeployUpdatesUnlabeledAdoptedService(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(dir, "key")
	if err := os.WriteFile(key, make([]byte, 32), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := state.Open(dir, key, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	digest := "docker.io/example/web@sha256:" + strings.Repeat("a", 64)
	one := 1
	role := manifest.Role{Service: "existing", Replicas: &one, Health: &manifest.Health{Test: []string{"CMD", "true"}}}
	spec, err := serviceSpec("test", "proj", "web", "web", "prior", "net", digest, nil, role, "1s", nil)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	_ = json.Unmarshal(spec, &body)
	body["Name"] = "existing"
	delete(body, "Labels")
	task := body["TaskTemplate"].(map[string]any)
	delete(task["ContainerSpec"].(map[string]any), "Labels")
	spec, _ = json.Marshal(body)
	e := &positiveEngine{spec: spec}
	c := Controller{Engine: e, Store: s, Environment: "test"}
	m := manifest.Manifest{Project: "proj", Network: "overlay", Apps: map[string]manifest.App{"web": {Kind: "image", Image: "example/web", Deploy: manifest.Deploy{DeployTimeout: "2s", DrainTimeout: "1s"}, Roles: map[string]manifest.Role{"web": role}}}}
	for _, req := range []Request{{RequestID: "adopt", Adopt: true, App: "web", Manifest: m}, {RequestID: "deploy", App: "web", Version: "sha256:" + strings.Repeat("b", 64), Manifest: m}} {
		op, _, err := c.Admit(context.Background(), Actor{ID: "admin", Admin: true}, req)
		if err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(3 * time.Second)
		for {
			got, err := s.Operation(context.Background(), op.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.State != "running" {
				if got.State != "succeeded" {
					t.Fatalf("%s: %s %s", req.RequestID, got.State, got.Err)
				}
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("timed out")
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if !strings.Contains(string(e.spec), strings.Repeat("b", 64)) {
		t.Fatal("adopted service was not updated")
	}
}
