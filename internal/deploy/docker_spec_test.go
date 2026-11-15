package deploy

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"berthy/internal/engine"
	"berthy/internal/manifest"
	"berthy/internal/state"
)

// storedAsDocker returns a service spec as Docker keeps it: null fields are not stored.
func storedAsDocker(s json.RawMessage) json.RawMessage {
	var v any
	dec := json.NewDecoder(bytes.NewReader(s))
	dec.UseNumber()
	if dec.Decode(&v) != nil {
		return s
	}
	out, _ := json.Marshal(dropNulls(v))
	return out
}

// dockerSpecs is positiveEngine with service specs stored the way Docker stores them.
type dockerSpecs struct{ *positiveEngine }

func (e dockerSpecs) CreateService(ctx context.Context, spec json.RawMessage, auth *engine.RegistryAuth) (string, error) {
	return e.positiveEngine.CreateService(ctx, storedAsDocker(spec), auth)
}

func (e dockerSpecs) UpdateService(ctx context.Context, id string, version uint64, spec json.RawMessage, auth *engine.RegistryAuth) error {
	return e.positiveEngine.UpdateService(ctx, id, version, storedAsDocker(spec), auth)
}

// dockerManifest declares one web role with the given plain environment, which may be empty.
func dockerManifest(plain map[string]string) manifest.Manifest {
	one := 1
	return manifest.Manifest{Project: "proj", Network: "overlay", Apps: map[string]manifest.App{"web": {Kind: "image", Image: "example/web", Env: manifest.Env{Plain: plain}, Deploy: manifest.Deploy{DeployTimeout: "2s", DrainTimeout: "1s"}, Roles: map[string]manifest.Role{"web": {Replicas: &one, Health: &manifest.Health{Test: []string{"CMD", "true"}}}}}}}
}

// waitOperation polls until the operation leaves the running state.
func waitOperation(t *testing.T, s *state.Store, id string) state.Operation {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got, err := s.Operation(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if got.State != "running" {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatal("operation did not finish")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestDockerStoredSpecDeploysAppWithoutEnv(t *testing.T) {
	s := openStore(t)
	c := Controller{Engine: dockerSpecs{&positiveEngine{}}, Store: s, Environment: "test"}
	op, _, err := c.Admit(context.Background(), Actor{ID: "admin"}, Request{RequestID: "first", App: "web", Version: "sha256:" + strings.Repeat("a", 64), Manifest: dockerManifest(nil)})
	if err != nil {
		t.Fatal(err)
	}
	if got := waitOperation(t, s, op.ID); got.State != "succeeded" {
		t.Fatalf("state %s: %s", got.State, got.Err)
	}
	if _, err = s.Applied(context.Background(), "web"); err != nil {
		t.Fatalf("no release recorded: %v", err)
	}
}

func TestDockerStoredSpecUpdatesToAppWithoutEnv(t *testing.T) {
	s := openStore(t)
	e := dockerSpecs{&positiveEngine{}}
	c := Controller{Engine: e, Store: s, Environment: "test"}
	for _, req := range []Request{
		{RequestID: "first", App: "web", Version: "sha256:" + strings.Repeat("a", 64), Manifest: dockerManifest(map[string]string{"MODE": "fast"})},
		{RequestID: "second", App: "web", Version: "sha256:" + strings.Repeat("b", 64), Manifest: dockerManifest(nil)},
	} {
		op, _, err := c.Admit(context.Background(), Actor{ID: "admin"}, req)
		if err != nil {
			t.Fatal(err)
		}
		if got := waitOperation(t, s, op.ID); got.State != "succeeded" {
			t.Fatalf("%s: %s %s", req.RequestID, got.State, got.Err)
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	var live struct {
		TaskTemplate struct {
			ContainerSpec map[string]json.RawMessage
		}
	}
	if err := json.Unmarshal(e.spec, &live); err != nil {
		t.Fatal(err)
	}
	if _, ok := live.TaskTemplate.ContainerSpec["Env"]; ok {
		t.Fatalf("env kept after update: %s", e.spec)
	}
}

func TestRecoveryAcceptsJournaledNullEnv(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	prior, _, err := s.Admit(ctx, state.Admission{RequestID: "prior", ActorID: "a", Kind: "deploy", App: "web", PublicRequest: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Finish(ctx, prior.ID, state.Outcome{State: "succeeded", Release: &state.Release{App: "web", Digest: "docker.io/example/web@sha256:" + strings.Repeat("a", 64), Manifest: json.RawMessage(`{}`)}}); err != nil {
		t.Fatal(err)
	}
	op, _, err := s.Admit(ctx, state.Admission{RequestID: "update", ActorID: "a", Kind: "deploy", App: "web", PublicRequest: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	before := json.RawMessage(`{"Name":"svc","Labels":{"berthy.operation":"old"},"TaskTemplate":{"ContainerSpec":{"Image":"docker.io/example/web@sha256:` + strings.Repeat("a", 64) + `","Labels":{"berthy.operation":"old"}}},"Mode":{"Replicated":{"Replicas":1}}}`)
	desired := json.RawMessage(`{"Name":"svc","Labels":{"berthy.operation":"new"},"TaskTemplate":{"ContainerSpec":{"Image":"docker.io/example/web@sha256:` + strings.Repeat("b", 64) + `","Env":null,"Labels":{"berthy.operation":"new"}}},"Mode":{"Replicated":{"Replicas":1}}}`)
	if err = s.RecordIntent(ctx, op.ID, state.Intent{Kind: "update", Name: "svc", ServiceID: "id", Generation: "new", Before: before, Desired: desired}); err != nil {
		t.Fatal(err)
	}
	// Swarm applied the journaled update, and Docker stored it without the null field.
	e := dockerSpecs{&positiveEngine{spec: storedAsDocker(desired)}}
	c := Controller{Engine: e, Store: s}
	if err = c.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := s.Operation(ctx, op.ID)
	if err != nil || got.State != "interrupted" {
		t.Fatalf("operation %+v: %v", got, err)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if !sameSpec(e.spec, before) {
		t.Fatalf("service not restored: %s", e.spec)
	}
}

func TestSpecComparisonIgnoresNullMapFields(t *testing.T) {
	if !sameSpec(json.RawMessage(`{"a":null,"b":1}`), json.RawMessage(`{"b":1}`)) {
		t.Fatal("null map field changes the comparison")
	}
	if sameSpec(json.RawMessage(`[null]`), json.RawMessage(`[]`)) {
		t.Fatal("null array element is ignored")
	}
	if !matchesCreatedSpec(json.RawMessage(`{"Name":"svc","TaskTemplate":{"ContainerSpec":{"Env":null}}}`), json.RawMessage(`{"Name":"svc","TaskTemplate":{"ContainerSpec":{}}}`)) {
		t.Fatal("created spec with a null env is rejected")
	}
}

func TestServiceSpecOmitsEmptyEnv(t *testing.T) {
	b, err := serviceSpec("env", "proj", "app", "web", "op", "network", "image", nil, manifest.Role{Health: &manifest.Health{Test: []string{"CMD", "true"}}}, "10s", nil)
	if err != nil || strings.Contains(string(b), `"Env"`) {
		t.Fatalf("spec %s: %v", b, err)
	}
}
