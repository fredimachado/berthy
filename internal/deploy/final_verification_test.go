package deploy

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"berthy/internal/engine"
	"berthy/internal/manifest"
	"berthy/internal/state"
)

type finalCheckEngine struct {
	*budgetEngine
	store                                 *state.Store
	oneOff                                *diagFake
	fault                                 string
	roles                                 int
	armed, triggered, foreign             bool
	reads, foreignReads, foreignMutations int
}

func (f *finalCheckEngine) allVerified(ctx context.Context) bool {
	if f.triggered {
		return false
	}
	op, err := f.store.Active(ctx)
	if err != nil || op.RequestID != "candidate" {
		return false
	}
	intents, err := f.store.Intents(ctx, op.ID)
	if err != nil {
		return false
	}
	n := 0
	for _, i := range intents {
		if i.Kind == "create" || i.Kind == "update" {
			if i.Phase != "verified" {
				return false
			}
			n++
		}
	}
	if n != f.roles {
		return false
	}
	f.armed = true
	return true
}

func (f *finalCheckEngine) InspectService(ctx context.Context, ref string) (engine.ServiceSnapshot, error) {
	if f.triggered && f.foreign {
		f.foreignReads++
		return engine.ServiceSnapshot{ID: "foreign", Spec: json.RawMessage(`{"Name":"foreign"}`)}, nil
	}
	if f.fault == "inspect" && f.allVerified(ctx) {
		f.triggered = true
		return engine.ServiceSnapshot{}, errors.New("injected final inspection failure")
	}
	return f.budgetEngine.InspectService(ctx, ref)
}

func (f *finalCheckEngine) ServiceTasks(ctx context.Context, id string) ([]engine.ServiceTask, error) {
	if (f.fault == "health" || f.fault == "drain") && f.allVerified(ctx) {
		f.reads++
		if f.fault == "health" && f.reads == 1 || f.fault == "drain" && f.reads == 2 {
			f.triggered = true
			return nil, errors.New("injected final task read failure")
		}
	}
	return f.budgetEngine.ServiceTasks(ctx, id)
}

func (f *finalCheckEngine) CreateService(ctx context.Context, spec json.RawMessage, auth *engine.RegistryAuth) (string, error) {
	if f.triggered && f.foreign {
		f.foreignMutations++
	}
	return f.budgetEngine.CreateService(ctx, spec, auth)
}

func (f *finalCheckEngine) UpdateService(ctx context.Context, id string, version uint64, spec json.RawMessage, auth *engine.RegistryAuth) error {
	if f.triggered && f.foreign {
		f.foreignMutations++
	}
	return f.budgetEngine.UpdateService(ctx, id, version, spec, auth)
}

func (f *finalCheckEngine) DeleteService(ctx context.Context, id string) error {
	if f.triggered && f.foreign {
		f.foreignMutations++
	}
	return f.budgetEngine.DeleteService(ctx, id)
}

func (f *finalCheckEngine) CreateContainer(ctx context.Context, name string, spec json.RawMessage) (string, error) {
	return f.oneOff.CreateContainer(ctx, name, spec)
}
func (f *finalCheckEngine) StartContainer(ctx context.Context, id string) error {
	return f.oneOff.StartContainer(ctx, id)
}
func (f *finalCheckEngine) WaitContainer(ctx context.Context, id string) (int, error) {
	return f.oneOff.WaitContainer(ctx, id)
}
func (f *finalCheckEngine) ContainerLogs(ctx context.Context, id string) ([]byte, error) {
	return f.oneOff.ContainerLogs(ctx, id)
}
func (f *finalCheckEngine) InspectContainer(ctx context.Context, id string) (engine.ContainerSnapshot, error) {
	return f.oneOff.InspectContainer(ctx, id)
}
func (f *finalCheckEngine) StopContainer(ctx context.Context, id string, seconds int) error {
	return f.oneOff.StopContainer(ctx, id, seconds)
}

func finalFixture(t *testing.T, fault string, roles int) (*Controller, *state.Store, *finalCheckEngine) {
	t.Helper()
	s := openStore(t)
	f := &finalCheckEngine{budgetEngine: &budgetEngine{boundFake: &boundFake{}}, store: s, fault: fault, roles: roles, oneOff: newDiagFake()}
	c := &Controller{Store: s, Engine: f, Environment: "test", rolloutHold: zeroTestRolloutHold()}
	t.Cleanup(func() {
		if err := c.Drain(context.Background()); err != nil {
			t.Error(err)
		}
	})
	return c, s, f
}

func finalDeploy(t *testing.T, c *Controller, s *state.Store, request, digit string, m manifest.Manifest) state.Operation {
	t.Helper()
	ctx := context.Background()
	op, _, err := c.Admit(ctx, Actor{ID: "a", Admin: true}, Request{RequestID: request, App: "web", Version: digestOfLetter(digit), Manifest: m})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	op, err = s.Operation(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	return op
}

func assertFinalFault(t *testing.T, f *finalCheckEngine, s *state.Store, op state.Operation) {
	t.Helper()
	if !f.armed || !f.triggered {
		t.Fatalf("final %s fault did not run after %d verified roles (armed=%v, triggered=%v, reads=%d)", f.fault, f.roles, f.armed, f.triggered, f.reads)
	}
	intents, err := s.Intents(context.Background(), op.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, i := range intents {
		if i.Kind == "barrier" {
			t.Fatal("barrier recorded after failed verification")
		}
	}
	history, err := s.History(context.Background(), "web")
	if err != nil {
		t.Fatal(err)
	}
	for _, release := range history {
		if release.OperationID == op.ID {
			t.Fatal("candidate release was recorded")
		}
	}
	settings, exists, err := s.Settings(context.Background())
	if err != nil || exists && settings.OperationID == op.ID {
		t.Fatalf("candidate settings: %+v, exists=%v, err=%v", settings, exists, err)
	}
}

func TestFinalRolloutFailureUnwindsVerifiedRoles(t *testing.T) {
	for _, kind := range []string{"create", "update"} {
		for _, fault := range []string{"inspect", "health", "drain"} {
			t.Run(kind+"/"+fault, func(t *testing.T) {
				c, s, f := finalFixture(t, fault, 1)
				m := webManifest(testDeployTimeout, nil)
				var previous state.Release
				var before engine.ServiceSnapshot
				var settings state.Settings
				var settingsExist bool
				if kind == "update" {
					prior := finalDeploy(t, c, s, "prior", "a", m)
					if prior.State != "succeeded" {
						t.Fatalf("prior deployment %s: %s", prior.State, prior.Err)
					}
					var err error
					previous, err = s.Applied(context.Background(), "web")
					if err != nil {
						t.Fatal(err)
					}
					before, err = f.boundFake.InspectService(context.Background(), "proj-test-web-web")
					if err != nil {
						t.Fatal(err)
					}
					settings, settingsExist, err = s.Settings(context.Background())
					if err != nil {
						t.Fatal(err)
					}
				}
				op := finalDeploy(t, c, s, "candidate", "b", m)
				assertFinalFault(t, f, s, op)
				want := "failed"
				if kind == "update" {
					want = "rolled_back"
				}
				if op.State != want {
					t.Fatalf("final %s failure: state %s, want %s (lock remains held)", fault, op.State, want)
				}
				if _, err := s.Lock(context.Background()); !errors.Is(err, sql.ErrNoRows) {
					t.Fatalf("lock after cleanup: %v", err)
				}
				live, err := f.boundFake.InspectService(context.Background(), "proj-test-web-web")
				if kind == "create" {
					var status *engine.StatusError
					if !errors.As(err, &status) || status.Code != 404 {
						t.Fatalf("created service remains: %+v %v", live, err)
					}
				} else {
					if err != nil || live.ID != before.ID || !sameSpec(live.Spec, before.Spec) {
						t.Fatalf("previous service not restored: %+v %v", live, err)
					}
					applied, err := s.Applied(context.Background(), "web")
					if err != nil || applied.ID != previous.ID || applied.Digest != previous.Digest {
						t.Fatalf("previous release: %+v %v", applied, err)
					}
					after, exists, err := s.Settings(context.Background())
					if err != nil || exists != settingsExist || !reflect.DeepEqual(after, settings) {
						t.Fatalf("settings changed: %+v %v %v", after, exists, err)
					}
				}
			})
		}
	}
}

func TestFinalFailureCompensatesEveryVerifiedRole(t *testing.T) {
	c, s, f := finalFixture(t, "inspect", 2)
	m := budgetManifest()
	a := m.Apps["web"]
	delete(a.Roles, "b")
	m.Apps["web"] = a
	if prior := finalDeploy(t, c, s, "prior", "a", m); prior.State != "succeeded" {
		t.Fatalf("prior deployment: %+v", prior)
	}
	before, err := f.boundFake.InspectService(context.Background(), "proj-test-web-a")
	if err != nil {
		t.Fatal(err)
	}
	m = budgetManifest()
	a = m.Apps["web"]
	a.Migrations = &manifest.Migrations{Version: []string{"version"}, Apply: []string{"apply"}}
	m.Apps["web"] = a
	f.oneOff.exits = []int{0, 0, 0}
	f.oneOff.versions = []string{"v1\n", "v2\n"}
	op := finalDeploy(t, c, s, "candidate", "b", m)
	assertFinalFault(t, f, s, op)
	if op.State != "rolled_back" {
		t.Fatalf("mixed rollout state %s, want rolled_back", op.State)
	}
	if op.SchemaBefore != "v1" || op.SchemaAfter != "v2" {
		t.Fatalf("schema observations %q -> %q", op.SchemaBefore, op.SchemaAfter)
	}
	if _, err := s.Lock(context.Background()); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("lock after compensation: %v", err)
	}
	live, err := f.boundFake.InspectService(context.Background(), "proj-test-web-a")
	if err != nil || live.ID != before.ID || !sameSpec(live.Spec, before.Spec) {
		t.Fatalf("update not restored: %+v %v", live, err)
	}
	if _, err := f.boundFake.InspectService(context.Background(), "proj-test-web-b"); err == nil {
		t.Fatal("created role remains")
	} else {
		var status *engine.StatusError
		if !errors.As(err, &status) || status.Code != 404 {
			t.Fatal(err)
		}
	}
}

func TestFinalFailureKeepsLockWhenCompensationCannotConfirmOwnership(t *testing.T) {
	c, s, f := finalFixture(t, "inspect", 1)
	f.foreign = true
	op := finalDeploy(t, c, s, "candidate", "b", webManifest(testDeployTimeout, nil))
	assertFinalFault(t, f, s, op)
	if f.foreignReads < 1 {
		t.Fatal("compensation never inspected the foreign service")
	}
	if f.foreignMutations != 0 {
		t.Fatalf("mutated a foreign service %d times", f.foreignMutations)
	}
	diagnostics := storedDiagnostics(t, s, op.ID)
	found := false
	for _, d := range diagnostics {
		if d.Kind == "compensation" && strings.Contains(d.Error, "compensation:") {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing compensation diagnostic: %+v", diagnostics)
	}
	if op.State != "recovery_required" {
		t.Fatalf("state %s, want recovery_required", op.State)
	}
	lock, err := s.Lock(context.Background())
	if err != nil || lock.OperationID != op.ID {
		t.Fatalf("lock: %+v %v", lock, err)
	}
}
