package deploy

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"sync"
	"testing"

	"berthy/internal/engine"
	"berthy/internal/manifest"
	"berthy/internal/state"
)

// recordingEngine is a Docker-like engine that keeps every spec it was asked to create or update.
type recordingEngine struct {
	dockerSpecs
	mu2      sync.Mutex
	creates  []json.RawMessage
	updates  []json.RawMessage
	failures int
}

func newRecordingEngine() *recordingEngine {
	return &recordingEngine{dockerSpecs: dockerSpecs{&positiveEngine{}}}
}

func (e *recordingEngine) CreateService(ctx context.Context, spec json.RawMessage, auth *engine.RegistryAuth) (string, error) {
	e.mu2.Lock()
	e.creates = append(e.creates, append(json.RawMessage(nil), spec...))
	e.mu2.Unlock()
	return e.dockerSpecs.CreateService(ctx, spec, auth)
}

func (e *recordingEngine) UpdateService(ctx context.Context, id string, version uint64, spec json.RawMessage, auth *engine.RegistryAuth) error {
	e.mu2.Lock()
	e.updates = append(e.updates, append(json.RawMessage(nil), spec...))
	e.mu2.Unlock()
	return e.dockerSpecs.UpdateService(ctx, id, version, spec, auth)
}

func (e *recordingEngine) sent() (creates, updates []json.RawMessage) {
	e.mu2.Lock()
	defer e.mu2.Unlock()
	return append([]json.RawMessage(nil), e.creates...), append([]json.RawMessage(nil), e.updates...)
}

// deploySucceeds deploys the web app at the digest of the letter v and returns the ID of the operation.
func deploySucceeds(t *testing.T, c *Controller, s *state.Store, id string, v string, m manifest.Manifest) string {
	t.Helper()
	op := deployWeb(t, c, s, id, v, m)
	if op.State != "succeeded" {
		t.Fatalf("%s: %s %s", id, op.State, op.Err)
	}
	return op.ID
}

// releaseManifest is a manifest whose one role sets everything the release is made of.
func releaseManifest() manifest.Manifest {
	m := dockerManifest(map[string]string{"MODE": "fast"})
	app := m.Apps["web"]
	role := app.Roles["web"]
	role.User = "1000"
	role.Entrypoint = []string{"/bin/app"}
	role.Command = []string{"serve"}
	role.Resources = &manifest.Resources{Memory: "64M"}
	app.Roles["web"] = role
	m.Apps["web"] = app
	return m
}

func normalizeOperation(spec json.RawMessage, op string) string {
	return strings.ReplaceAll(string(spec), op, "OP")
}

func TestUpdateSendsTheSameSpecAsCreate(t *testing.T) {
	m := releaseManifest()

	// The update: a first deploy of another version creates the service, then the release deploys over it.
	s := openStore(t)
	updating := newRecordingEngine()
	c := Controller{Engine: updating, Store: s, Environment: "test"}
	deploySucceeds(t, &c, s, "first", "a", dockerManifest(nil))
	op := deploySucceeds(t, &c, s, "second", "b", m)
	_, updates := updating.sent()
	if len(updates) != 1 {
		t.Fatalf("updates %d", len(updates))
	}

	// The create: the same release on an empty daemon.
	s2 := openStore(t)
	creating := newRecordingEngine()
	c2 := Controller{Engine: creating, Store: s2, Environment: "test"}
	op2 := deploySucceeds(t, &c2, s2, "only", "b", m)
	creates, _ := creating.sent()
	if len(creates) != 1 {
		t.Fatalf("creates %d", len(creates))
	}

	if got, want := normalizeOperation(updates[0], op), normalizeOperation(creates[0], op2); got != want {
		t.Fatalf("the update sends\n%s\nbut a create of the release sends\n%s", got, want)
	}
}

func TestUpdateDropsAFieldChangedByHand(t *testing.T) {
	s := openStore(t)
	e := newRecordingEngine()
	c := Controller{Engine: e, Store: s, Environment: "test"}
	deploySucceeds(t, &c, s, "first", "a", dockerManifest(nil))

	// Someone ran docker service update --user 1000 --hostname handmade.
	e.positiveEngine.mu.Lock()
	var spec map[string]any
	if err := json.Unmarshal(e.positiveEngine.spec, &spec); err != nil {
		t.Fatal(err)
	}
	container := spec["TaskTemplate"].(map[string]any)["ContainerSpec"].(map[string]any)
	container["User"], container["Hostname"] = "1000", "handmade"
	e.positiveEngine.spec, _ = json.Marshal(spec)
	e.positiveEngine.mu.Unlock()

	deploySucceeds(t, &c, s, "second", "b", dockerManifest(nil))
	e.positiveEngine.mu.Lock()
	defer e.positiveEngine.mu.Unlock()
	if strings.Contains(string(e.positiveEngine.spec), `"User"`) || strings.Contains(string(e.positiveEngine.spec), `"Hostname"`) {
		t.Fatalf("a field changed by hand survived the deploy: %s", e.positiveEngine.spec)
	}
}

func TestServiceSpecSetsUserAndEntrypoint(t *testing.T) {
	role := manifest.Role{User: "1000:1000", Entrypoint: []string{"/bin/app", "--serve"}, Command: []string{"-v"}, Health: &manifest.Health{Test: []string{"CMD", "true"}}}
	b, err := serviceSpec("env", "proj", "app", "web", "op", "network", "image", nil, role, "10s", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		TaskTemplate struct {
			ContainerSpec struct {
				User          string
				Command, Args []string
			}
		}
	}
	if err = json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	c := v.TaskTemplate.ContainerSpec
	if c.User != "1000:1000" || !reflect.DeepEqual(c.Command, []string{"/bin/app", "--serve"}) || !reflect.DeepEqual(c.Args, []string{"-v"}) {
		t.Fatalf("container spec %+v", c)
	}
	// A role that sets neither leaves both to the image.
	b, err = serviceSpec("env", "proj", "app", "web", "op", "network", "image", nil, manifest.Role{Health: role.Health}, "10s", nil, "")
	if err != nil || strings.Contains(string(b), `"User"`) || strings.Contains(string(b), `"Command"`) {
		t.Fatalf("spec %s: %v", b, err)
	}
}
