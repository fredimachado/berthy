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
	"reflect"
	"testing"
)

const refreshHost = "registry.example"

type refreshUpdate struct {
	ID      string
	Version uint64
	Spec    json.RawMessage
	Auth    engine.RegistryAuth
}

// refreshEngine is a Docker with some services. A service listed in failures rejects that many updates, and each rejection
// moves its version on, as another writer would.
type refreshEngine struct {
	Engine
	services map[string]*engine.ServiceSnapshot // by name
	failures map[string]int
	updates  []refreshUpdate
}

func (f *refreshEngine) InspectService(_ context.Context, name string) (engine.ServiceSnapshot, error) {
	s, ok := f.services[name]
	if !ok {
		return engine.ServiceSnapshot{}, errors.New("no such service")
	}
	return *s, nil
}

func (f *refreshEngine) UpdateService(_ context.Context, id string, version uint64, spec json.RawMessage, auth *engine.RegistryAuth) error {
	for name, s := range f.services {
		if s.ID != id {
			continue
		}
		if f.failures[name] != 0 {
			f.failures[name]--
			s.Version++
			return errors.New("update rejected")
		}
		if version != s.Version {
			return errors.New("out of sequence")
		}
		u := refreshUpdate{ID: id, Version: version, Spec: spec}
		if auth != nil {
			u.Auth = *auth
		}
		f.updates = append(f.updates, u)
		return nil
	}
	return errors.New("no such service")
}

func liveSpec(name, image string) json.RawMessage {
	return json.RawMessage(`{"Name":"` + name + `","TaskTemplate":{"ContainerSpec":{"Image":"` + image + `"}},"Mode":{"Replicated":{"Replicas":1}}}`)
}

type refreshFixture struct {
	t      *testing.T
	s      *state.Store
	engine *refreshEngine
	c      *Controller
	n      int
}

func newRefreshFixture(t *testing.T) *refreshFixture {
	t.Helper()
	dir := t.TempDir()
	os.Chmod(dir, 0700)
	key := filepath.Join(dir, "key")
	if err := os.WriteFile(key, make([]byte, 32), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := state.Open(dir, key, "stage")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	f := &refreshFixture{t: t, s: s, engine: &refreshEngine{services: map[string]*engine.ServiceSnapshot{}, failures: map[string]int{}}}
	f.c = &Controller{Store: s, Engine: f.engine, Environment: "stage"}
	return f
}

func (f *refreshFixture) password(secret, registry, value string) {
	f.t.Helper()
	if err := f.s.PutSecret(context.Background(), state.Actor{ID: "admin"}, state.SecretInfo{Name: secret, Registry: registry}, []byte(value)); err != nil {
		f.t.Fatal(err)
	}
}

// app applies an image app of one role whose manifest lists registries, bound to a service that runs image.
func (f *refreshFixture) app(app, image string, registries map[string]manifest.Registry) string {
	f.t.Helper()
	ctx := context.Background()
	f.n++
	m, err := json.Marshal(manifest.Manifest{Project: "p", Network: "n", Registries: registries, Apps: map[string]manifest.App{app: {Kind: "image", Image: image}}})
	if err != nil {
		f.t.Fatal(err)
	}
	name := app + "-web"
	id := "id-" + app
	op, _, err := f.s.Admit(ctx, state.Admission{RequestID: "deploy-" + app, ActorID: "a", Kind: "deploy", App: app, PublicRequest: []byte(`{}`)})
	if err != nil {
		f.t.Fatal(err)
	}
	if err = f.s.Finish(ctx, op.ID, state.Outcome{State: "succeeded", Release: &state.Release{App: app, Manifest: m, Bindings: []state.Binding{{App: app, Role: "web", Name: name, ServiceID: id}}}}); err != nil {
		f.t.Fatal(err)
	}
	f.engine.services[name] = &engine.ServiceSnapshot{ID: id, Version: uint64(10 + f.n), Spec: liveSpec(name, image)}
	return name
}

func ghcr() map[string]manifest.Registry {
	return map[string]manifest.Registry{refreshHost: {Username: "robot", PasswordSecret: "REGISTRY_TOKEN"}}
}

func TestRefreshRegistryAuthGivesTheNewPasswordToServicesOfThatRegistry(t *testing.T) {
	f := newRefreshFixture(t)
	f.password("REGISTRY_TOKEN", refreshHost, "old")
	name := f.app("web", refreshHost+"/acme/web:1", ghcr())
	f.password("REGISTRY_TOKEN", refreshHost, "new")
	live := *f.engine.services[name]

	got, err := f.c.RefreshRegistryAuth(context.Background(), refreshHost)
	if err != nil {
		t.Fatal(err)
	}
	if want := []RegistryRefresh{{Name: name, Updated: true}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("result %+v, want %+v", got, want)
	}
	if len(f.engine.updates) != 1 {
		t.Fatalf("%d updates", len(f.engine.updates))
	}
	u := f.engine.updates[0]
	wantAuth := engine.RegistryAuth{Username: "robot", Password: "new", ServerAddress: refreshHost}
	if u.ID != live.ID || u.Version != live.Version || string(u.Spec) != string(live.Spec) || u.Auth != wantAuth {
		t.Fatalf("update %+v", u)
	}
}

func TestRefreshRegistryAuthLeavesOtherServicesAlone(t *testing.T) {
	f := newRefreshFixture(t)
	f.password("REGISTRY_TOKEN", refreshHost, "new")
	f.app("web", refreshHost+"/acme/web:1", ghcr())
	f.app("other", "other.example/acme/other:1", map[string]manifest.Registry{"other.example": {Username: "u", PasswordSecret: "REGISTRY_TOKEN"}})
	f.app("unlisted", "unlisted.example/acme/x:1", ghcr())
	f.app("hub", "acme/hub:1", nil)

	got, err := f.c.RefreshRegistryAuth(context.Background(), refreshHost)
	if err != nil {
		t.Fatal(err)
	}
	if want := []RegistryRefresh{{Name: "web-web", Updated: true}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("result %+v, want %+v", got, want)
	}
	if len(f.engine.updates) != 1 || f.engine.updates[0].ID != "id-web" {
		t.Fatalf("updates %+v", f.engine.updates)
	}
}

func TestRefreshRegistryAuthRetriesOnceWhenTheServiceChangedMeanwhile(t *testing.T) {
	f := newRefreshFixture(t)
	f.password("REGISTRY_TOKEN", refreshHost, "new")
	name := f.app("web", refreshHost+"/acme/web:1", ghcr())
	f.engine.failures[name] = 1
	before := f.engine.services[name].Version

	got, err := f.c.RefreshRegistryAuth(context.Background(), refreshHost)
	if err != nil {
		t.Fatal(err)
	}
	if want := []RegistryRefresh{{Name: name, Updated: true}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("result %+v, want %+v", got, want)
	}
	if len(f.engine.updates) != 1 || f.engine.updates[0].Version != before+1 || f.engine.updates[0].Auth.Password != "new" {
		t.Fatalf("updates %+v", f.engine.updates)
	}
}

func TestRefreshRegistryAuthReportsAServiceItCouldNotUpdateAndGoesOn(t *testing.T) {
	f := newRefreshFixture(t)
	f.password("REGISTRY_TOKEN", refreshHost, "new")
	f.app("api", refreshHost+"/acme/api:1", ghcr())
	f.app("web", refreshHost+"/acme/web:1", ghcr())
	f.engine.failures["api-web"] = -1

	got, err := f.c.RefreshRegistryAuth(context.Background(), refreshHost)
	if err != nil {
		t.Fatal(err)
	}
	want := []RegistryRefresh{{Name: "api-web", Err: "service update failed"}, {Name: "web-web", Updated: true}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("result %+v, want %+v", got, want)
	}
	if len(f.engine.updates) != 1 || f.engine.updates[0].ID != "id-web" {
		t.Fatalf("updates %+v", f.engine.updates)
	}
}

func TestRefreshRegistryAuthReportsABoundServiceThatIsGone(t *testing.T) {
	f := newRefreshFixture(t)
	f.password("REGISTRY_TOKEN", refreshHost, "new")
	f.app("web", refreshHost+"/acme/web:1", ghcr())
	f.app("api", refreshHost+"/acme/api:1", ghcr())
	f.app("other", "other.example/x:1", map[string]manifest.Registry{"other.example": {Username: "u", PasswordSecret: "REGISTRY_TOKEN"}})
	// web is gone, api is another service under the bound name, and other is gone but not ours to report.
	delete(f.engine.services, "web-web")
	f.engine.services["api-web"].ID = "replaced"
	delete(f.engine.services, "other-web")

	got, err := f.c.RefreshRegistryAuth(context.Background(), refreshHost)
	if err != nil {
		t.Fatal(err)
	}
	want := []RegistryRefresh{{Name: "api-web", Err: "bound service unavailable"}, {Name: "web-web", Err: "bound service unavailable"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("result %+v, want %+v", got, want)
	}
	if len(f.engine.updates) != 0 {
		t.Fatalf("updates %+v", f.engine.updates)
	}
}

func TestRefreshRegistryAuthDoesNotReportAGoneServiceOfAnotherRegistry(t *testing.T) {
	f := newRefreshFixture(t)
	f.password("REGISTRY_TOKEN", refreshHost, "new")
	// The manifest lists the host for the whole project, but these apps pull from elsewhere.
	f.app("elsewhere", "other.example/acme/x:1", ghcr())
	f.app("hub", "acme/hub:1", ghcr())
	delete(f.engine.services, "elsewhere-web")
	f.engine.services["hub-web"].ID = "replaced"

	got, err := f.c.RefreshRegistryAuth(context.Background(), refreshHost)
	if err != nil || len(got) != 0 {
		t.Fatalf("refresh: %+v %v", got, err)
	}
	names, err := f.c.RegistryServices(context.Background(), refreshHost)
	if err != nil || len(names) != 0 {
		t.Fatalf("list: %v %v", names, err)
	}
}

func TestRefreshRegistryAuthReportsAMissingCredentialWithoutLeakingPasswords(t *testing.T) {
	f := newRefreshFixture(t)
	f.password("REGISTRY_TOKEN", refreshHost, "new")
	f.app("web", refreshHost+"/acme/web:1", ghcr())
	// The secret turns into a plain secret, as it once could by a set without a registry.
	f.password("REGISTRY_TOKEN", "", "plain-password")

	got, err := f.c.RefreshRegistryAuth(context.Background(), refreshHost)
	if err != nil {
		t.Fatal(err)
	}
	if want := []RegistryRefresh{{Name: "web-web", Err: "registry credential unavailable"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("result %+v, want %+v", got, want)
	}
}

func TestRegistryServicesListsWithoutChangingAnything(t *testing.T) {
	f := newRefreshFixture(t)
	f.password("REGISTRY_TOKEN", refreshHost, "new")
	f.app("web", refreshHost+"/acme/web:1", ghcr())
	f.app("api", refreshHost+"/acme/api:1", ghcr())
	f.app("hub", "acme/hub:1", nil)

	got, err := f.c.RegistryServices(context.Background(), refreshHost)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"api-web", "web-web"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("services %v, want %v", got, want)
	}
	if len(f.engine.updates) != 0 {
		t.Fatalf("updates %+v", f.engine.updates)
	}
}

func TestRefreshRegistryAuthWithNoBindingsNeedsNoDocker(t *testing.T) {
	f := newRefreshFixture(t)
	f.c.Engine = nil
	got, err := f.c.RefreshRegistryAuth(context.Background(), refreshHost)
	if got != nil || err != nil {
		t.Fatalf("refresh: %v %v", got, err)
	}
	names, err := f.c.RegistryServices(context.Background(), refreshHost)
	if names != nil || err != nil {
		t.Fatalf("list: %v %v", names, err)
	}
}

func TestRefreshRegistryAuthSaysWhenDockerIsUnavailable(t *testing.T) {
	f := newRefreshFixture(t)
	f.password("REGISTRY_TOKEN", refreshHost, "new")
	f.app("web", refreshHost+"/acme/web:1", ghcr())
	f.c.Engine = nil
	if _, err := f.c.RefreshRegistryAuth(context.Background(), refreshHost); err == nil || err.Error() != "Docker unavailable" {
		t.Fatalf("error %v", err)
	}
}
