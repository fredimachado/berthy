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
	return engine.ServiceSnapshot{ID: "id", Spec: e.spec}, nil
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
	var s struct{ Labels map[string]string }
	_ = json.Unmarshal(e.spec, &s)
	return []engine.ServiceTask{{ServiceID: "id", ContainerID: "c", State: "running", DesiredState: "running", Slot: 1, Labels: s.Labels}}, nil
}
func (e *positiveEngine) InspectContainerHealth(context.Context, string) (string, error) {
	if e.unhealthy {
		return "starting", nil
	}
	return "healthy", nil
}

func TestUnhealthyFirstReleaseRemovesCreatedService(t *testing.T) {
	dir := t.TempDir()
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
