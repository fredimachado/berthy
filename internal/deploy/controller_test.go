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
	"slices"
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
	t.Cleanup(func() { store.Close() })
	e := &positiveEngine{}
	c := Controller{Engine: e, Store: store, Environment: "test"}
	one := 1
	req := Request{RequestID: "one", App: "web", Version: "sha256:" + strings.Repeat("a", 64), Manifest: manifest.Manifest{Project: "proj", Network: "overlay", Apps: map[string]manifest.App{"web": {Kind: "image", Image: "example/web", Deploy: manifest.Deploy{DeployTimeout: testDeployTimeout, DrainTimeout: "1s"}, Roles: map[string]manifest.Role{"web": {Replicas: &one, Health: &manifest.Health{Test: []string{"CMD", "true"}}}}}}}}
	op, _, err := admit(t, &c, context.Background(), Actor{ID: "actor"}, req)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(testWait)
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
	t.Cleanup(func() { store.Close() })
	e := &positiveEngine{unhealthy: true}
	c := Controller{Engine: e, Store: store, Environment: "test"}
	one := 1
	req := Request{RequestID: "bad", App: "web", Version: "sha256:" + strings.Repeat("a", 64), Manifest: manifest.Manifest{Project: "proj", Network: "overlay", Apps: map[string]manifest.App{"web": {Kind: "image", Image: "example/web", Deploy: manifest.Deploy{DeployTimeout: "100ms", DrainTimeout: "1s"}, Roles: map[string]manifest.Role{"web": {Replicas: &one, Health: &manifest.Health{Test: []string{"CMD", "true"}}}}}}}}
	op, _, err := admit(t, &c, context.Background(), Actor{ID: "actor"}, req)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(testWait)
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
	t.Cleanup(func() { s.Close() })
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
	t.Cleanup(func() { s.Close() })
	e := &positiveEngine{}
	c := Controller{Engine: e, Store: s, Environment: "test"}
	one := 1
	req := Request{RequestID: "first", App: "web", Version: "sha256:" + strings.Repeat("a", 64), Manifest: manifest.Manifest{Project: "proj", Network: "overlay", Apps: map[string]manifest.App{"web": {Kind: "image", Image: "example/web", Deploy: manifest.Deploy{DeployTimeout: testDeployTimeout, DrainTimeout: "1s"}, Roles: map[string]manifest.Role{"web": {Replicas: &one, Health: &manifest.Health{Test: []string{"CMD", "true"}}}}}}}}
	for _, id := range []string{"first", "second"} {
		req.RequestID = id
		if id == "second" {
			req.Version = "sha256:" + strings.Repeat("b", 64)
		}
		op, _, err := admit(t, &c, context.Background(), Actor{ID: "actor"}, req)
		if err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(testWait)
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

func TestDeployRestoresManifestReplicasAfterScaleToZero(t *testing.T) {
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
	t.Cleanup(func() { s.Close() })
	e := &positiveEngine{}
	c := Controller{Engine: e, Store: s, Environment: "test"}
	// The role sets no replicas, so the manifest count is the default of one.
	req := Request{App: "web", Manifest: manifest.Manifest{Project: "proj", Network: "overlay", Apps: map[string]manifest.App{"web": {Kind: "image", Image: "example/web", Deploy: manifest.Deploy{DeployTimeout: testDeployTimeout, DrainTimeout: "1s"}, Roles: map[string]manifest.Role{"web": {Health: &manifest.Health{Test: []string{"CMD", "true"}}}}}}}}
	deployVersion := func(id, digit string) {
		t.Helper()
		req.RequestID = id
		req.Version = "sha256:" + strings.Repeat(digit, 64)
		op, _, err := admit(t, &c, context.Background(), Actor{ID: "actor"}, req)
		if err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(testWait)
		for {
			got, err := s.Operation(context.Background(), op.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.State != "running" {
				if got.State != "succeeded" {
					t.Fatalf("%s: %s %s", id, got.State, got.Err)
				}
				return
			}
			if time.Now().After(deadline) {
				t.Fatal("timed out")
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	// engineService decodes the service spec the engine holds.
	engineService := func() (int, string) {
		e.mu.Lock()
		defer e.mu.Unlock()
		var live struct {
			Mode struct {
				Replicated struct{ Replicas int }
			}
			TaskTemplate struct {
				ContainerSpec struct{ Image string }
			}
		}
		if err := json.Unmarshal(e.spec, &live); err != nil {
			t.Fatal(err)
		}
		return live.Mode.Replicated.Replicas, live.TaskTemplate.ContainerSpec.Image
	}
	// scaleToZero does what berthy scale --replicas 0 does to the service: it sets the replicas the engine holds to zero.
	scaleToZero := func() {
		e.mu.Lock()
		defer e.mu.Unlock()
		var body map[string]any
		if err := json.Unmarshal(e.spec, &body); err != nil {
			t.Fatal(err)
		}
		body["Mode"].(map[string]any)["Replicated"].(map[string]any)["Replicas"] = float64(0)
		spec, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		e.spec = spec
	}
	deployVersion("first", "a")
	scaleToZero()
	if replicas, _ := engineService(); replicas != 0 {
		t.Fatalf("scaled service has %d replicas, want 0", replicas)
	}
	deployVersion("second", "b")
	replicas, image := engineService()
	if replicas != 1 {
		t.Fatalf("redeploy left %d replicas, want the manifest count 1", replicas)
	}
	if !strings.Contains(image, strings.Repeat("b", 64)) {
		t.Fatalf("redeploy image %q is not the second version", image)
	}
}

// stoppedServiceEngine models Swarm for a service stopped at zero replicas: an update of such a service never reports an
// update status, because there was no running task for Swarm to replace.
type stoppedServiceEngine struct {
	*positiveEngine
	mu       sync.Mutex
	noStatus bool
}

func (e *stoppedServiceEngine) UpdateService(ctx context.Context, id string, version uint64, spec json.RawMessage, auth *engine.RegistryAuth) error {
	e.positiveEngine.mu.Lock()
	var current struct {
		Mode struct{ Replicated *struct{ Replicas int } }
	}
	stopped := json.Unmarshal(e.positiveEngine.spec, &current) == nil && current.Mode.Replicated != nil && current.Mode.Replicated.Replicas == 0
	e.positiveEngine.mu.Unlock()
	e.mu.Lock()
	e.noStatus = stopped
	e.mu.Unlock()
	return e.positiveEngine.UpdateService(ctx, id, version, spec, auth)
}

func (e *stoppedServiceEngine) InspectService(ctx context.Context, name string) (engine.ServiceSnapshot, error) {
	live, err := e.positiveEngine.InspectService(ctx, name)
	if err != nil {
		return live, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.noStatus {
		live.UpdateStatus = engine.UpdateStatus{}
	}
	return live, nil
}

func TestDeployOverStoppedRoleSucceeds(t *testing.T) {
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
	t.Cleanup(func() { s.Close() })
	e := &stoppedServiceEngine{positiveEngine: &positiveEngine{}}
	c := Controller{Engine: e, Store: s, Environment: "test"}
	// The role sets no replicas, so the manifest count is the default of one.
	req := Request{App: "web", Manifest: manifest.Manifest{Project: "proj", Network: "overlay", Apps: map[string]manifest.App{"web": {Kind: "image", Image: "example/web", Deploy: manifest.Deploy{DeployTimeout: testDeployTimeout, DrainTimeout: "1s"}, Roles: map[string]manifest.Role{"web": {Health: &manifest.Health{Test: []string{"CMD", "true"}}}}}}}}
	deployVersion := func(id, digit string) {
		t.Helper()
		req.RequestID = id
		req.Version = "sha256:" + strings.Repeat(digit, 64)
		op, _, err := admit(t, &c, context.Background(), Actor{ID: "actor"}, req)
		if err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(testWait)
		for {
			got, err := s.Operation(context.Background(), op.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.State != "running" {
				if got.State != "succeeded" {
					t.Fatalf("%s: %s %s", id, got.State, got.Err)
				}
				return
			}
			if time.Now().After(deadline) {
				t.Fatal("timed out")
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	// scaleToZero does what berthy scale --replicas 0 does to the service: it sets the replicas the engine holds to zero.
	scaleToZero := func() {
		e.positiveEngine.mu.Lock()
		defer e.positiveEngine.mu.Unlock()
		var body map[string]any
		if err := json.Unmarshal(e.positiveEngine.spec, &body); err != nil {
			t.Fatal(err)
		}
		body["Mode"].(map[string]any)["Replicated"].(map[string]any)["Replicas"] = float64(0)
		spec, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		e.positiveEngine.spec = spec
	}
	deployVersion("first", "a")
	scaleToZero()
	deployVersion("second", "b")
	e.positiveEngine.mu.Lock()
	defer e.positiveEngine.mu.Unlock()
	var live struct {
		Mode struct {
			Replicated struct{ Replicas int }
		}
		TaskTemplate struct {
			ContainerSpec struct{ Image string }
		}
	}
	if err := json.Unmarshal(e.positiveEngine.spec, &live); err != nil {
		t.Fatal(err)
	}
	if live.Mode.Replicated.Replicas != 1 {
		t.Fatalf("redeploy left %d replicas, want the manifest count 1", live.Mode.Replicated.Replicas)
	}
	if !strings.Contains(live.TaskTemplate.ContainerSpec.Image, strings.Repeat("b", 64)) {
		t.Fatalf("redeploy image %q is not the second version", live.TaskTemplate.ContainerSpec.Image)
	}
}

func TestRecoverBarrierAcceptsStoppedServiceUpdate(t *testing.T) {
	stopped := json.RawMessage(`{"Name":"svc","Labels":{"berthy.operation":"op"},"Mode":{"Replicated":{"Replicas":0}},"TaskTemplate":{"ContainerSpec":{"Image":"example/web"}}}`)
	desired := json.RawMessage(`{"Name":"svc","Labels":{"berthy.operation":"op"},"Mode":{"Replicated":{"Replicas":1}},"TaskTemplate":{"ContainerSpec":{"Image":"example/web"}}}`)
	receipt, err := json.Marshal(struct {
		ID   string          `json:"id"`
		Spec json.RawMessage `json:"spec"`
	}{"id", desired})
	if err != nil {
		t.Fatal(err)
	}
	// Swarm reports no update status for a service stopped at zero replicas, so the barrier must not wait for one.
	e := &stoppedServiceEngine{positiveEngine: &positiveEngine{spec: desired}, noStatus: true}
	c := Controller{Engine: e}
	intent := state.Intent{Ordinal: 1, Kind: "update", Name: "svc", ServiceID: "id", Generation: "op", Before: stopped, Desired: desired, Phase: "verified", Receipt: receipt}
	barrier := state.Intent{Desired: json.RawMessage(`{"Release":{"App":"web"}}`)}
	if _, err = c.recoverBarrier(context.Background(), []state.Intent{intent}, barrier); err != nil {
		t.Fatalf("barrier over a stopped service's update: %v", err)
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
	t.Cleanup(func() { s.Close() })
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
	t.Cleanup(func() { s.Close() })
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
	t.Cleanup(func() { s.Close() })
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
	spec, err := serviceSpec("test", "proj", "web", "web", (&manifest.Manifest{Project: "proj"}).ServiceName("test", "web", "web"), "prior", "net", digest, func(v string) string { return (&manifest.Manifest{Project: "proj"}).VolumeName("test", v) }, nil, role, "start-first", "1s", nil, "")
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
	t.Cleanup(func() { s.Close() })
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
	t.Cleanup(func() { s.Close() })
	digest := "docker.io/example/web@sha256:" + strings.Repeat("a", 64)
	one := 1
	role := manifest.Role{Service: "existing", Replicas: &one, Health: &manifest.Health{Test: []string{"CMD", "true"}}}
	spec, err := serviceSpec("test", "proj", "web", "web", (&manifest.Manifest{Project: "proj"}).ServiceName("test", "web", "web"), "prior", "net", digest, func(v string) string { return (&manifest.Manifest{Project: "proj"}).VolumeName("test", v) }, nil, role, "start-first", "1s", nil, "")
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
	op, _, err := admit(t, &c, context.Background(), Actor{ID: "admin", Admin: true}, req)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(testWait)
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
	t.Cleanup(func() { s.Close() })
	digest := "docker.io/example/web@sha256:" + strings.Repeat("a", 64)
	one := 1
	role := manifest.Role{Service: "existing", Replicas: &one, Health: &manifest.Health{Test: []string{"CMD", "true"}}}
	spec, err := serviceSpec("test", "proj", "web", "web", (&manifest.Manifest{Project: "proj"}).ServiceName("test", "web", "web"), "prior", "net", digest, func(v string) string { return (&manifest.Manifest{Project: "proj"}).VolumeName("test", v) }, nil, role, "start-first", "1s", nil, "")
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
	m := manifest.Manifest{Project: "proj", Network: "overlay", Apps: map[string]manifest.App{"web": {Kind: "image", Image: "example/web", Deploy: manifest.Deploy{DeployTimeout: testDeployTimeout, DrainTimeout: "1s"}, Roles: map[string]manifest.Role{"web": role}}}}
	for _, req := range []Request{{RequestID: "adopt", Adopt: true, App: "web", Manifest: m}, {RequestID: "deploy", App: "web", Version: "sha256:" + strings.Repeat("b", 64), Manifest: m}} {
		op, _, err := admit(t, &c, context.Background(), Actor{ID: "admin", Admin: true}, req)
		if err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(testWait)
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

func TestSchemaRevertRequiresKnownTargetBeforeRollout(t *testing.T) {
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
	t.Cleanup(func() { s.Close() })
	c := Controller{Store: s}
	m := manifest.Manifest{Project: "p", Network: "n", Apps: map[string]manifest.App{"web": {Kind: "image", Image: "example/web", Roles: map[string]manifest.Role{"web": {}}}}}
	b, _ := json.Marshal(m)
	for i := 0; i < 2; i++ {
		op, _, err := s.Admit(context.Background(), state.Admission{RequestID: strconv.Itoa(i), ActorID: "a", Kind: "deploy", App: "web", PublicRequest: json.RawMessage(`{}`)})
		if err != nil {
			t.Fatal(err)
		}
		if err = s.Finish(context.Background(), op.ID, state.Outcome{State: "succeeded", Release: &state.Release{App: "web", Digest: "sha256:" + strings.Repeat("a", 64), Manifest: b}}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.schemaRevertPlan(context.Background(), Request{App: "web", RevertSchema: true}); err == nil {
		t.Fatal("unknown target accepted")
	}
}

// admit is c.Admit, and the test waits for the operation's goroutine before its cleanups close the store, however the test
// ends: one that fails while an operation runs would otherwise close the store under it, and the goroutine's panic would hide
// the failure. A cleanup runs before the ones registered earlier, as the store's close, which openStore registered.
func admit(t *testing.T, c *Controller, ctx context.Context, actor Actor, req Request) (state.Operation, bool, error) {
	t.Helper()
	op, created, err := c.Admit(ctx, actor, req)
	if created {
		t.Cleanup(func() {
			waitCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := c.Drain(waitCtx); err != nil {
				t.Errorf("an operation is still running at the end of the test: %v", err)
			}
		})
	}
	return op, created, err
}

func openStore(t *testing.T) *state.Store {
	t.Helper()
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
	t.Cleanup(func() { s.Close() })
	return s
}

func TestRecoveryKeepsRecordedSchemaStates(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	op, _, err := s.Admit(ctx, state.Admission{RequestID: "r", ActorID: "a", Kind: "deploy", App: "web", PublicRequest: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Finish(ctx, op.ID, state.Outcome{State: "recovery_required", Err: "held", SchemaBefore: "v1", SchemaAfter: "v2"}); err != nil {
		t.Fatal(err)
	}
	c := Controller{Engine: &positiveEngine{}, Store: s}
	if err = c.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := s.Operation(ctx, op.ID)
	if err != nil || got.State != "interrupted" || got.SchemaBefore != "v1" || got.SchemaAfter != "v2" {
		t.Fatalf("operation %+v: %v", got, err)
	}
}

// absentContainers reports every one-off container as already removed.
type absentContainers struct{ *hookEngine }

func (absentContainers) InspectContainer(context.Context, string) (engine.ContainerSnapshot, error) {
	return engine.ContainerSnapshot{}, &engine.StatusError{Code: 404}
}

func TestRecoveryRestoresSchemaFromOneOffReceipt(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	op, _, err := s.Admit(ctx, state.Admission{RequestID: "r", ActorID: "a", Kind: "deploy", App: "web", PublicRequest: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RecordIntent(ctx, op.ID, state.Intent{Ordinal: 10000, Kind: "oneoff", Role: "schema_before", Name: "berthy-probe", Desired: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if err = s.MarkStep(ctx, op.ID, 10000, "finished", json.RawMessage(`{"id":"container","exit":0,"version":"v1"}`)); err != nil {
		t.Fatal(err)
	}
	c := Controller{Engine: absentContainers{&hookEngine{}}, Store: s}
	if err = c.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := s.Operation(ctx, op.ID)
	if err != nil || got.State != "interrupted" || got.SchemaBefore != "v1" {
		t.Fatalf("operation %+v: %v", got, err)
	}
}

func TestRecoveryPendingDoesNotRepeatAudit(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	op, _, err := s.Admit(ctx, state.Admission{RequestID: "r", ActorID: "a", Kind: "deploy", App: "web", PublicRequest: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RecordIntent(ctx, op.ID, state.Intent{Kind: "update", Name: "svc", ServiceID: "id", Before: json.RawMessage(`{"Name":"old"}`), Desired: json.RawMessage(`{"Name":"new"}`)}); err != nil {
		t.Fatal(err)
	}
	c := Controller{Engine: &positiveEngine{spec: json.RawMessage(`{"Name":"foreign"}`)}, Store: s}
	if err = c.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	before, err := s.AuditLog(ctx, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Recover(ctx); !errors.Is(err, ErrRecoveryPending) {
		t.Fatalf("second recovery: %v", err)
	}
	after, err := s.AuditLog(ctx, 1000)
	if err != nil || len(after) != len(before) {
		t.Fatalf("audit grew from %d to %d: %v", len(before), len(after), err)
	}
}

// boundFake answers service lookups by name or ID, as Docker does, and records the services it creates.
type boundFake struct {
	mu       sync.Mutex
	services []engine.ServiceSnapshot
	failures map[string]error // returned by InspectService for the keyed name or ID
	created  []json.RawMessage
}

func (f *boundFake) add(id string, spec json.RawMessage) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.services = append(f.services, engine.ServiceSnapshot{ID: id, Spec: spec, UpdateStatus: engine.UpdateStatus{State: "completed"}})
}

func (f *boundFake) createdSpecs() []json.RawMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]json.RawMessage(nil), f.created...)
}

func specName(spec json.RawMessage) string {
	var s struct{ Name string }
	_ = json.Unmarshal(spec, &s)
	return s.Name
}

func (f *boundFake) InspectNetwork(context.Context, string) (engine.Network, error) {
	return engine.Network{ID: "net", Driver: "overlay", Scope: "swarm", Attachable: true}, nil
}
func (f *boundFake) ResolveImage(context.Context, string, *engine.RegistryAuth) (string, error) {
	return "", errors.New("unexpected resolution")
}
func (f *boundFake) PullImage(context.Context, string, *engine.RegistryAuth) error { return nil }
func (f *boundFake) InspectImage(context.Context, string) (engine.Image, error) {
	return engine.Image{ID: "image"}, nil
}
func (f *boundFake) InspectService(_ context.Context, ref string) (engine.ServiceSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failures[ref]; err != nil {
		return engine.ServiceSnapshot{}, err
	}
	for _, s := range f.services {
		if s.ID == ref || specName(s.Spec) == ref {
			return s, nil
		}
	}
	return engine.ServiceSnapshot{}, &engine.StatusError{Code: 404}
}
func (f *boundFake) CreateService(_ context.Context, spec json.RawMessage, _ *engine.RegistryAuth) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.services {
		if specName(s.Spec) == specName(spec) {
			return "", &engine.StatusError{Code: 409}
		}
	}
	f.created = append(f.created, append(json.RawMessage(nil), spec...))
	id := "new-" + strconv.Itoa(len(f.created))
	f.services = append(f.services, engine.ServiceSnapshot{ID: id, Spec: append(json.RawMessage(nil), spec...), UpdateStatus: engine.UpdateStatus{State: "completed"}})
	return id, nil
}
func (f *boundFake) UpdateService(context.Context, string, uint64, json.RawMessage, *engine.RegistryAuth) error {
	return errors.New("unexpected update")
}
func (f *boundFake) DeleteService(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, s := range f.services {
		if s.ID == id {
			f.services = append(f.services[:i], f.services[i+1:]...)
			return nil
		}
	}
	return &engine.StatusError{Code: 404}
}
func (f *boundFake) ServiceTasks(_ context.Context, id string) ([]engine.ServiceTask, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.services {
		if s.ID != id {
			continue
		}
		var task struct {
			TaskTemplate struct {
				ContainerSpec struct{ Labels map[string]string }
			}
		}
		if err := json.Unmarshal(s.Spec, &task); err != nil {
			return nil, err
		}
		return []engine.ServiceTask{{ServiceID: id, ContainerID: "c-" + id, State: "running", DesiredState: "running", Slot: 1, Labels: task.TaskTemplate.ContainerSpec.Labels}}, nil
	}
	return nil, nil
}
func (f *boundFake) InspectContainerHealth(context.Context, string) (string, error) {
	return "healthy", nil
}

// seedRelease records a succeeded deploy of app web at digest that binds the given services.
func seedRelease(t *testing.T, s *state.Store, requestID, digest string, bindings ...state.Binding) {
	t.Helper()
	seedTaggedRelease(t, s, requestID, digest, "", bindings...)
}

// seedTaggedRelease records a succeeded deploy of app web at digest, asked for by tag ("" for a digest deploy), that binds the given services.
func seedTaggedRelease(t *testing.T, s *state.Store, requestID, digest, tag string, bindings ...state.Binding) {
	t.Helper()
	ctx := context.Background()
	op, _, err := s.Admit(ctx, state.Admission{RequestID: requestID, ActorID: "admin", Kind: "deploy", App: "web", PublicRequest: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(dockerManifest(nil))
	if err = s.Finish(ctx, op.ID, state.Outcome{State: "succeeded", Release: &state.Release{App: "web", Digest: digest, Tag: tag, Manifest: raw, Bindings: bindings}}); err != nil {
		t.Fatal(err)
	}
	// Later operations must start after this one, so the timestamps order them.
	time.Sleep(3 * time.Millisecond)
}

// boundBinding is the binding of the web role of app web to a service.
func boundBinding(name, serviceID string) state.Binding {
	return state.Binding{App: "web", Role: "web", Name: name, ServiceID: serviceID}
}

// runBound admits a deploy or rollback of app web and returns the operation once it has finished.
func runBound(t *testing.T, s *state.Store, c *Controller, req Request) state.Operation {
	t.Helper()
	op, _, err := admit(t, c, context.Background(), Actor{ID: "admin"}, req)
	if err != nil {
		t.Fatal(err)
	}
	return waitOperation(t, s, op.ID)
}

func TestBoundServiceGoneIsRecreatedUnderBoundName(t *testing.T) {
	s := openStore(t)
	seedRelease(t, s, "prior", "sha256:"+strings.Repeat("a", 64), boundBinding("legacy-web", "old-id"))
	f := &boundFake{}
	c := &Controller{Engine: f, Store: s, Environment: "test"}
	op := runBound(t, s, c, Request{RequestID: "recreate", App: "web", Version: "sha256:" + strings.Repeat("b", 64), Manifest: dockerManifest(nil)})
	if op.State != "succeeded" {
		t.Fatalf("state %s: %s", op.State, op.Err)
	}
	created := f.createdSpecs()
	if len(created) != 1 || specName(created[0]) != "legacy-web" {
		t.Fatalf("%d services created", len(created))
	}
	intents, err := s.Intents(context.Background(), op.ID)
	if err != nil {
		t.Fatal(err)
	}
	var creates []state.Intent
	for _, i := range intents {
		if i.Kind == "create" {
			creates = append(creates, i)
		}
	}
	if len(creates) != 1 || creates[0].Role != "web" || creates[0].Name != "legacy-web" {
		t.Fatalf("create intents %+v", creates)
	}
	bindings, err := s.Bindings(context.Background(), "web")
	if err != nil {
		t.Fatal(err)
	}
	if len(bindings) != 1 || bindings[0] != boundBinding("legacy-web", "new-1") {
		t.Fatalf("bindings %+v", bindings)
	}
}

func TestBoundServiceRenamedOutsideBerthyIsRefused(t *testing.T) {
	s := openStore(t)
	seedRelease(t, s, "prior", "sha256:"+strings.Repeat("a", 64), boundBinding("legacy-web", "old-id"))
	f := &boundFake{}
	f.add("old-id", json.RawMessage(`{"Name":"renamed-web"}`))
	c := &Controller{Engine: f, Store: s, Environment: "test"}
	op := runBound(t, s, c, Request{RequestID: "renamed", App: "web", Version: "sha256:" + strings.Repeat("b", 64), Manifest: dockerManifest(nil)})
	if op.State != "failed" || op.Err != "bound service unavailable" {
		t.Fatalf("state %s: %s", op.State, op.Err)
	}
	if n := len(f.createdSpecs()); n != 0 {
		t.Fatalf("%d services created", n)
	}
}

func TestBoundNameWithDifferentIDIsRefused(t *testing.T) {
	s := openStore(t)
	seedRelease(t, s, "prior", "sha256:"+strings.Repeat("a", 64), boundBinding("legacy-web", "old-id"))
	f := &boundFake{}
	f.add("other-id", json.RawMessage(`{"Name":"legacy-web"}`))
	c := &Controller{Engine: f, Store: s, Environment: "test"}
	op := runBound(t, s, c, Request{RequestID: "replaced", App: "web", Version: "sha256:" + strings.Repeat("b", 64), Manifest: dockerManifest(nil)})
	if op.State != "failed" || op.Err != "bound service unavailable" {
		t.Fatalf("state %s: %s", op.State, op.Err)
	}
	if n := len(f.createdSpecs()); n != 0 {
		t.Fatalf("%d services created", n)
	}
}

func TestBoundNameLookupErrorIsRefused(t *testing.T) {
	s := openStore(t)
	seedRelease(t, s, "prior", "sha256:"+strings.Repeat("a", 64), boundBinding("legacy-web", "old-id"))
	// The recorded ID is absent too, so only the failed name lookup stands between this deploy and a recreate.
	f := &boundFake{failures: map[string]error{"legacy-web": &engine.StatusError{Code: 500}}}
	c := &Controller{Engine: f, Store: s, Environment: "test"}
	op := runBound(t, s, c, Request{RequestID: "unreachable", App: "web", Version: "sha256:" + strings.Repeat("b", 64), Manifest: dockerManifest(nil)})
	if op.State != "failed" || op.Err != "bound service unavailable" {
		t.Fatalf("state %s: %s", op.State, op.Err)
	}
	if n := len(f.createdSpecs()); n != 0 {
		t.Fatalf("%d services created", n)
	}
}

func TestRollbackRecreatesBoundServiceGone(t *testing.T) {
	s := openStore(t)
	seedRelease(t, s, "older", "sha256:"+strings.Repeat("a", 64))
	seedRelease(t, s, "current", "sha256:"+strings.Repeat("b", 64), boundBinding("legacy-web", "old-id"))
	f := &boundFake{}
	c := &Controller{Engine: f, Store: s, Environment: "test"}
	op := runBound(t, s, c, Request{RequestID: "rollback", App: "web", Kind: "rollback"})
	if op.State != "succeeded" {
		t.Fatalf("state %s: %s", op.State, op.Err)
	}
	created := f.createdSpecs()
	if len(created) != 1 || specName(created[0]) != "legacy-web" || !strings.Contains(string(created[0]), strings.Repeat("a", 64)) {
		t.Fatalf("%d services created", len(created))
	}
	bindings, err := s.Bindings(context.Background(), "web")
	if err != nil {
		t.Fatal(err)
	}
	if len(bindings) != 1 || bindings[0] != boundBinding("legacy-web", "new-1") {
		t.Fatalf("bindings %+v", bindings)
	}
}

func TestRecoveryRemovesUnobservedRecreateByBoundName(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	op, _, err := s.Admit(ctx, state.Admission{RequestID: "recreate", ActorID: "admin", Kind: "deploy", App: "web", PublicRequest: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	spec := json.RawMessage(`{"Name":"legacy-web","Labels":{"berthy.operation":"` + op.ID + `"},"TaskTemplate":{"ContainerSpec":{"Labels":{"berthy.operation":"` + op.ID + `"}}}}`)
	if err = s.RecordIntent(ctx, op.ID, state.Intent{Kind: "create", Role: "web", Name: "legacy-web", Generation: op.ID, Desired: spec}); err != nil {
		t.Fatal(err)
	}
	// Docker created the service, but the operation never journaled its result.
	f := &boundFake{}
	f.add("new-1", spec)
	c := &Controller{Engine: f, Store: s, Environment: "test"}
	if err = c.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := s.Operation(ctx, op.ID)
	if err != nil || got.State != "interrupted" {
		t.Fatalf("state %s: %v", got.State, err)
	}
	var status *engine.StatusError
	if _, err = f.InspectService(ctx, "legacy-web"); !errors.As(err, &status) || status.Code != 404 {
		t.Fatalf("recreated service still exists: %v", err)
	}
}

func TestAdoptedServiceWithDockerStoredEmptySettingsIsAdoptedAndUpdated(t *testing.T) {
	s := openStore(t)
	digest := "docker.io/example/web@sha256:" + strings.Repeat("a", 64)
	one := 1
	role := manifest.Role{Service: "existing", Replicas: &one, Health: &manifest.Health{Test: []string{"CMD", "true"}}}
	spec, err := serviceSpec("test", "proj", "web", "web", (&manifest.Manifest{Project: "proj"}).ServiceName("test", "web", "web"), "prior", "net", digest, func(v string) string { return (&manifest.Manifest{Project: "proj"}).VolumeName("test", v) }, nil, role, "start-first", "1s", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	_ = json.Unmarshal(spec, &body)
	body["Name"] = "existing"
	delete(body, "Labels")
	container := body["TaskTemplate"].(map[string]any)["ContainerSpec"].(map[string]any)
	delete(container, "Labels")
	// What docker service create stores for the settings it does not use.
	container["DNSConfig"], container["Hosts"], container["Init"] = map[string]any{}, []any{}, false
	spec, _ = json.Marshal(body)
	e := &positiveEngine{spec: spec}
	c := Controller{Engine: e, Store: s, Environment: "test"}
	m := manifest.Manifest{Project: "proj", Network: "overlay", Apps: map[string]manifest.App{"web": {Kind: "image", Image: "example/web", Deploy: manifest.Deploy{DeployTimeout: testDeployTimeout, DrainTimeout: "1s"}, Roles: map[string]manifest.Role{"web": role}}}}
	for _, req := range []Request{{RequestID: "adopt", Adopt: true, App: "web", Manifest: m}, {RequestID: "deploy", App: "web", Version: "sha256:" + strings.Repeat("b", 64), Manifest: m}} {
		op, _, err := admit(t, &c, context.Background(), Actor{ID: "admin", Admin: true}, req)
		if err != nil {
			t.Fatal(err)
		}
		if got := waitOperation(t, s, op.ID); got.State != "succeeded" {
			t.Fatalf("%s: %s %s", req.RequestID, got.State, got.Err)
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if !strings.Contains(string(e.spec), strings.Repeat("b", 64)) {
		t.Fatal("adopted service was not updated")
	}
}

// tagResolver is an engine whose registry resolves every tag to one digest. The rest of the engine is the one it wraps.
type tagResolver struct {
	Engine
	digest string
}

func (r tagResolver) ResolveImage(context.Context, string, *engine.RegistryAuth) (string, error) {
	return r.digest, nil
}

func TestRequestedTag(t *testing.T) {
	for _, tc := range []struct{ image, tag string }{
		{"ghcr.io/acme/api:v1.0.2", "v1.0.2"},
		{"ghcr.io/acme/api@sha256:" + strings.Repeat("a", 64), ""},
		{"docker.io/cloudflare/cloudflared:2026.11.0", "2026.11.0"},
		{"localhost:5000/a/b:1", "1"},
	} {
		if got := requestedTag(tc.image); got != tc.tag {
			t.Errorf("requestedTag(%q) = %q, want %q", tc.image, got, tc.tag)
		}
	}
}

func TestDeployByTagRecordsTheTag(t *testing.T) {
	s := openStore(t)
	f := &boundFake{}
	c := &Controller{Engine: tagResolver{Engine: f, digest: "sha256:" + strings.Repeat("c", 64)}, Store: s, Environment: "test"}
	op := runBound(t, s, c, Request{RequestID: "by-tag", App: "web", Version: "v1.0.2", Manifest: dockerManifest(nil)})
	if op.State != "succeeded" {
		t.Fatalf("state %s: %s", op.State, op.Err)
	}
	release, err := s.Applied(context.Background(), "web")
	if err != nil || release.Tag != "v1.0.2" || !strings.HasSuffix(release.Digest, "@sha256:"+strings.Repeat("c", 64)) {
		t.Fatalf("release %s tag %q: %v", release.Digest, release.Tag, err)
	}
}

func TestDeployByDigestRecordsNoTag(t *testing.T) {
	s := openStore(t)
	c := &Controller{Engine: &boundFake{}, Store: s, Environment: "test"}
	op := runBound(t, s, c, Request{RequestID: "by-digest", App: "web", Version: "sha256:" + strings.Repeat("d", 64), Manifest: dockerManifest(nil)})
	if op.State != "succeeded" {
		t.Fatalf("state %s: %s", op.State, op.Err)
	}
	release, err := s.Applied(context.Background(), "web")
	if err != nil || release.Tag != "" {
		t.Fatalf("digest deploy recorded tag %q: %v", release.Tag, err)
	}
}

func TestRollbackRecordsTheTagOfTheReleaseItReturnsTo(t *testing.T) {
	s := openStore(t)
	seedTaggedRelease(t, s, "older", "sha256:"+strings.Repeat("a", 64), "v1.0.1")
	seedTaggedRelease(t, s, "current", "sha256:"+strings.Repeat("b", 64), "v1.0.2", boundBinding("legacy-web", "old-id"))
	f := &boundFake{}
	c := &Controller{Engine: f, Store: s, Environment: "test"}
	op := runBound(t, s, c, Request{RequestID: "rollback", App: "web", Kind: "rollback"})
	if op.State != "succeeded" {
		t.Fatalf("state %s: %s", op.State, op.Err)
	}
	release, err := s.Applied(context.Background(), "web")
	if err != nil || release.Tag != "v1.0.1" || !strings.Contains(release.Digest, strings.Repeat("a", 64)) {
		t.Fatalf("rollback release %s tag %q: %v", release.Digest, release.Tag, err)
	}
}

func TestDryRunPlanCarriesTheTag(t *testing.T) {
	s := openStore(t)
	c := &Controller{Engine: tagResolver{Engine: &boundFake{}, digest: "sha256:" + strings.Repeat("c", 64)}, Store: s, Environment: "test"}
	plan, err := c.Plan(context.Background(), Actor{ID: "admin", Admin: true}, Request{RequestID: "plan", App: "web", Version: "v1.0.2", DryRun: true, Manifest: dockerManifest(nil)})
	if err != nil || plan.Tag != "v1.0.2" {
		t.Fatalf("plan tag %q: %v", plan.Tag, err)
	}
	raw, err := json.Marshal(plan)
	if err != nil || !strings.Contains(string(raw), `"tag":"v1.0.2"`) {
		t.Fatalf("plan JSON %s: %v", raw, err)
	}
}

func TestIngressConnectorRecordsItsTagWhenGivenByTag(t *testing.T) {
	f := newIngressFixture(t)
	f.ctrl.Engine = tagResolver{Engine: f.docker, digest: "sha256:" + strings.Repeat("e", 64)}
	m := ingressManifestImage(t, "cloudflare/cloudflared:2026.11.0", "")
	// The dry run shows the tag the connector release will record.
	plan, err := f.ctrl.Plan(context.Background(), Actor{ID: "admin", Admin: true}, Request{Kind: "ingress", Manifest: m, DryRun: true})
	if err != nil || plan.Tag != "2026.11.0" {
		t.Fatalf("ingress plan tag %q: %v", plan.Tag, err)
	}
	f.applyIngress("apply-tag", m)
	release, err := f.store.Applied(context.Background(), manifest.ConnectorAppName)
	if err != nil || release.Tag != "2026.11.0" {
		t.Fatalf("connector release tag %q: %v", release.Tag, err)
	}
}

func TestADeployOfAnotherProjectsManifestIsRefused(t *testing.T) {
	s := openStore(t)
	f := &boundFake{}
	c := &Controller{Engine: f, Store: s, Environment: "test", Project: "proj"}
	op := runBound(t, s, c, Request{RequestID: "first", App: "web", Version: "sha256:" + strings.Repeat("a", 64), Manifest: dockerManifest(nil)})
	if op.State != "succeeded" {
		t.Fatalf("state %s: %s", op.State, op.Err)
	}
	other := dockerManifest(nil)
	other.Project = "other"
	req := Request{RequestID: "second", App: "web", Version: "sha256:" + strings.Repeat("b", 64), Manifest: other}
	const want = "environment test runs project proj; this manifest is for project other"
	ctx := context.Background()
	if _, _, err := c.Admit(ctx, Actor{ID: "admin"}, req); err == nil || err.Error() != want {
		t.Fatalf("admit: %v", err)
	}
	dry := req
	dry.DryRun = true
	if _, err := c.Plan(ctx, Actor{ID: "admin"}, dry); err == nil || err.Error() != want {
		t.Fatalf("dry run: %v", err)
	}
	if _, err := c.ApplySettings(ctx, Actor{ID: "admin"}, ApplyRequest{Manifest: other}); err == nil || err.Error() != want {
		t.Fatalf("apply: %v", err)
	}
	if hist, _ := s.History(ctx, "web"); len(hist) != 1 {
		t.Fatalf("a refused deploy recorded a release: %d releases", len(hist))
	}
	if st, _, _ := s.Settings(ctx); st.Project != "proj" {
		t.Fatalf("a refused deploy changed the project: %+v", st)
	}
}

func TestAnEnvironmentKeepsItsProjectAfterEveryAppIsRemoved(t *testing.T) {
	s := openStore(t)
	c := &Controller{Engine: &boundFake{}, Store: s, Environment: "test", Project: "proj"}
	op := runBound(t, s, c, Request{RequestID: "first", App: "web", Version: "sha256:" + strings.Repeat("a", 64), Manifest: dockerManifest(nil)})
	if op.State != "succeeded" {
		t.Fatalf("state %s: %s", op.State, op.Err)
	}
	ctx := context.Background()
	rm, _, err := s.Admit(ctx, state.Admission{RequestID: "rm", ActorID: "admin", Kind: "remove", PublicRequest: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ForgetApp(ctx, rm.ID, "web"); err != nil {
		t.Fatal(err)
	}
	if err = s.Finish(ctx, rm.ID, state.Outcome{State: "succeeded"}); err != nil {
		t.Fatal(err)
	}
	other := dockerManifest(nil)
	other.Project = "other"
	const want = "environment test runs project proj; this manifest is for project other"
	req := Request{RequestID: "second", App: "web", Version: "sha256:" + strings.Repeat("b", 64), Manifest: other}
	if _, _, err = c.Admit(ctx, Actor{ID: "admin"}, req); err == nil || err.Error() != want {
		t.Fatalf("a deploy of another project with no app applied: %v", err)
	}
	if _, err = c.ApplySettings(ctx, Actor{ID: "admin"}, ApplyRequest{Manifest: other}); err == nil || err.Error() != want {
		t.Fatalf("an apply of another project with no app applied: %v", err)
	}
	if st, _, _ := s.Settings(ctx); st.Project != "proj" {
		t.Fatalf("the project changed: %+v", st)
	}
}

func TestAdoptionRefusesAnExistingUnscopedVolume(t *testing.T) {
	digest := "docker.io/example/web@sha256:" + strings.Repeat("a", 64)
	one := 1
	role := manifest.Role{Service: "existing", Replicas: &one, Health: &manifest.Health{Test: []string{"CMD", "true"}}, Volumes: []string{"data:/data"}}
	spec, err := serviceSpec("test", "proj", "web", "web", "existing", "prior", "net", digest, func(v string) string { return v }, nil, role, "start-first", "1s", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	c := Controller{Engine: &positiveEngine{spec: spec}, Environment: "test"}
	req := Request{Adopt: true, App: "web", Manifest: manifest.Manifest{Project: "proj", Network: "overlay", Apps: map[string]manifest.App{"web": {Kind: "image", Image: "example/web", Roles: map[string]manifest.Role{"web": role}}}}}
	_, _, err = c.planAdoption(context.Background(), req)
	if err == nil || err.Error() != "adoption would change volume data; align the manifest first" {
		t.Fatalf("adoption: %v", err)
	}
	if !PublicMessage(err.Error()) {
		t.Fatalf("refusal is not public: %v", err)
	}
	spec, err = serviceSpec("test", "proj", "web", "web", "existing", "prior", "net", digest, func(v string) string { return req.Manifest.VolumeName("test", v) }, nil, role, "start-first", "1s", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	c.Engine = &positiveEngine{spec: spec}
	if _, _, err = c.planAdoption(context.Background(), req); err != nil {
		t.Fatalf("matching scoped volume refused: %v", err)
	}
}

func TestApplyDryRunComparesResolvedNetworks(t *testing.T) {
	for _, tc := range []struct {
		old  string
		want string
	}{{"old", "network: old -> shop-prod"}, {"shop-prod", ""}} {
		t.Run(tc.old, func(t *testing.T) {
			c := &Controller{Store: openStore(t), Project: "shop", Environment: "prod"}
			m := dockerManifest(nil)
			m.Project = "shop"
			m.Network = tc.old
			ctx := context.Background()
			actor := Actor{ID: "admin"}
			if _, err := c.ApplySettings(ctx, actor, ApplyRequest{Manifest: m}); err != nil {
				t.Fatal(err)
			}
			m.Network = ""
			result, err := c.ApplySettings(ctx, actor, ApplyRequest{Manifest: m, DryRun: true})
			if err != nil {
				t.Fatal(err)
			}
			if tc.want == "" && len(result.Changes) != 0 {
				t.Fatalf("changes %v", result.Changes)
			}
			if tc.want != "" && !slices.Contains(result.Changes, tc.want) {
				t.Fatalf("changes %v, want %q", result.Changes, tc.want)
			}
		})
	}
}

func TestFirstApplyShowsTheDefaultNetwork(t *testing.T) {
	c := &Controller{Store: openStore(t), Project: "shop", Environment: "prod"}
	m := dockerManifest(nil)
	m.Project = "shop"
	m.Network = ""
	got, err := c.ApplySettings(context.Background(), Actor{ID: "admin"}, ApplyRequest{Manifest: m, DryRun: true})
	if err != nil || !slices.Contains(got.Changes, "network:  -> shop-prod") {
		t.Fatalf("changes %v: %v", got.Changes, err)
	}
}

func TestPlanRejectsAnAdoptedNameThatClashesWithAnotherRoleAlias(t *testing.T) {
	s := openStore(t)
	seedDeployed(t, s, "web", state.Binding{App: "web", Role: "web", Name: "api-web", ServiceID: "adopted"})
	c := &Controller{Store: s, Project: "shop", Environment: "prod"}
	m := dockerManifest(nil)
	m.Project = "shop"
	m.Apps["api"] = m.Apps["web"]
	_, err := c.Plan(context.Background(), Actor{ID: "admin", Admin: true}, Request{App: "api", Version: "v1", Manifest: m, DryRun: true})
	if err == nil || !strings.Contains(err.Error(), "api-web") || !strings.Contains(err.Error(), "api/web") || !strings.Contains(err.Error(), "web/web") {
		t.Fatalf("plan error: %v", err)
	}
	if !PublicMessage(err.Error()) {
		t.Fatalf("refusal is not public: %v", err)
	}
}

func TestDeployReportsAnAdoptedNetworkNameClash(t *testing.T) {
	s := openStore(t)
	seedDeployed(t, s, "web", state.Binding{App: "web", Role: "web", Name: "api-web", ServiceID: "adopted"})
	c := &Controller{Store: s, Project: "shop", Environment: "prod"}
	m := dockerManifest(nil)
	m.Project = "shop"
	m.Apps["api"] = m.Apps["web"]
	op, _, err := admit(t, c, context.Background(), Actor{ID: "admin", Admin: true}, Request{RequestID: "dns-clash", App: "api", Version: "v1", Manifest: m})
	if err != nil {
		t.Fatal(err)
	}
	got := waitOperation(t, s, op.ID)
	if got.State != "failed" || got.Err != "network name api-web is shared by roles api/web and web/web" {
		t.Fatalf("operation: %+v", got)
	}
}

func TestIngressPlanRejectsConflictingConnectorDNSNames(t *testing.T) {
	m, err := manifest.DecodeJSON([]byte(`{"project":"shop","ingress":{"cloudflare":{"account_id":"acct","api_token_secret":"CF_TOKEN","image":"cloudflare/cloudflared:2026.11.0"}},"apps":{"shop-prod-ingress":{"kind":"image","image":"alpine","roles":{"connector":{}}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	c := &Controller{Store: openStore(t), Project: "shop", Environment: "prod"}
	_, err = c.Plan(context.Background(), Actor{ID: "admin", Admin: true}, Request{Kind: "ingress", Manifest: *m, DryRun: true})
	if err == nil || !strings.Contains(err.Error(), "shop-prod-ingress-connector") {
		t.Fatalf("plan error: %v", err)
	}
}

func TestIngressApplyReportsConflictingConnectorDNSNames(t *testing.T) {
	m, err := manifest.DecodeJSON([]byte(`{"project":"shop","ingress":{"cloudflare":{"account_id":"acct","api_token_secret":"CF_TOKEN","image":"cloudflare/cloudflared:2026.11.0"}},"apps":{"shop-prod-ingress":{"kind":"image","image":"alpine","roles":{"connector":{}}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	s := openStore(t)
	c := &Controller{Store: s, Project: "shop", Environment: "prod"}
	op, _, err := admit(t, c, context.Background(), Actor{ID: "admin", Admin: true}, Request{RequestID: "ingress-dns", Kind: "ingress", Manifest: *m})
	if err != nil {
		t.Fatal(err)
	}
	got := waitOperation(t, s, op.ID)
	if got.State != "failed" || !strings.Contains(got.Err, "shop-prod-ingress-connector") {
		t.Fatalf("operation: %+v", got)
	}
}

func TestPlanChecksTheBindingOfAnAppNowDeclaredCompose(t *testing.T) {
	s := openStore(t)
	seedDeployed(t, s, "web", state.Binding{App: "web", Role: "web", Name: "api-web", ServiceID: "adopted"})
	c := &Controller{Store: s, Project: "shop", Environment: "prod"}
	m := dockerManifest(nil)
	m.Project = "shop"
	m.Apps["api"] = m.Apps["web"]
	m.Apps["web"] = manifest.App{Kind: "compose", File: "compose.yml"}
	_, err := c.Plan(context.Background(), Actor{ID: "admin", Admin: true}, Request{App: "api", Version: "v1", Manifest: m, DryRun: true})
	if err == nil || !strings.Contains(err.Error(), "api-web") {
		t.Fatalf("plan error: %v", err)
	}
}
