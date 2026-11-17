package jobs

import (
	"context"
	"errors"
	"testing"
	"time"

	"berthy/internal/api"
	"berthy/internal/engine"
	"berthy/internal/manifest"
	"berthy/internal/state"
)

type fakeDocker struct {
	node       string
	nodeErr    error
	tasks      map[string][]engine.ServiceTask
	containers []engine.ComposeContainer
	project    string
}

func (f fakeDocker) NodeID(context.Context) (string, error) { return f.node, f.nodeErr }
func (f fakeDocker) ServiceTasks(_ context.Context, id string) ([]engine.ServiceTask, error) {
	return f.tasks[id], nil
}
func (f fakeDocker) ComposeContainers(_ context.Context, project string) ([]engine.ComposeContainer, error) {
	if f.project != "" && project != f.project {
		return nil, errors.New("wrong project " + project)
	}
	return f.containers, nil
}

func targetManifest() manifest.Manifest {
	return manifest.Manifest{
		Project: "demo",
		Apps: map[string]manifest.App{
			"api":  {Kind: "image", Roles: map[string]manifest.Role{"web": {}, "worker": {}}},
			"solo": {Kind: "image", Roles: map[string]manifest.Role{"main": {}}},
			"db":   {Kind: "compose"},
		},
	}
}

func bindApp(t *testing.T, s *state.Store, app, role, name, id string) {
	t.Helper()
	ctx := context.Background()
	op, _, err := s.Admit(ctx, state.Admission{RequestID: app + role, ActorID: "a", Kind: "deploy", App: app, PublicRequest: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Finish(ctx, op.ID, state.Outcome{State: "succeeded", Release: &state.Release{App: app, Digest: "sha256:x", Manifest: []byte(`{}`), Bindings: []state.Binding{{App: app, Role: role, Name: name, ServiceID: id}}}}); err != nil {
		t.Fatal(err)
	}
}

func TestImageTargetPicksTheLowestRunningLocalSlot(t *testing.T) {
	store, _, _ := openTestStore(t)
	bindApp(t, store, "api", "web", "demo-api-web", "svc-web")
	docker := fakeDocker{node: "n1", tasks: map[string][]engine.ServiceTask{"svc-web": {
		{ID: "t1", NodeID: "n2", ContainerID: "c-other-node", State: "running", DesiredState: "running", Slot: 1},
		{ID: "t2", NodeID: "n1", ContainerID: "c-starting", State: "starting", DesiredState: "running", Slot: 2},
		{ID: "t3", NodeID: "n1", ContainerID: "c-stopping", State: "running", DesiredState: "shutdown", Slot: 3},
		{ID: "t4", NodeID: "n1", ContainerID: "c-slot5", State: "running", DesiredState: "running", Slot: 5},
		{ID: "t5", NodeID: "n1", ContainerID: "c-slot4", State: "running", DesiredState: "running", Slot: 4},
		{ID: "t6", NodeID: "n1", ContainerID: "", State: "running", DesiredState: "running", Slot: 1},
		{ID: "t7", NodeID: "n1", ContainerID: "c-old", State: "shutdown", DesiredState: "shutdown", Slot: 1},
	}}}
	target := EngineTarget{Store: store, Docker: docker}
	id, err := target.Container(context.Background(), targetManifest(), manifest.Job{App: "api", Role: "web"})
	if err != nil || id != "c-slot4" {
		t.Fatalf("container %q: %v", id, err)
	}
}

func TestImageTargetDefaultsToTheOnlyRole(t *testing.T) {
	store, _, _ := openTestStore(t)
	bindApp(t, store, "solo", "main", "demo-solo-main", "svc-solo")
	docker := fakeDocker{node: "n1", tasks: map[string][]engine.ServiceTask{"svc-solo": {{ID: "t", NodeID: "n1", ContainerID: "c1", State: "running", DesiredState: "running", Slot: 1}}}}
	id, err := EngineTarget{Store: store, Docker: docker}.Container(context.Background(), targetManifest(), manifest.Job{App: "solo"})
	if err != nil || id != "c1" {
		t.Fatalf("container %q: %v", id, err)
	}
}

func TestImageTargetFailuresNameTheTarget(t *testing.T) {
	store, _, _ := openTestStore(t)
	bindApp(t, store, "api", "web", "demo-api-web", "svc-web")
	ctx := context.Background()
	m := targetManifest()
	var te TargetError
	// Every task is on another node.
	other := fakeDocker{node: "n1", tasks: map[string][]engine.ServiceTask{"svc-web": {{ID: "t", NodeID: "n2", ContainerID: "c", State: "running", DesiredState: "running", Slot: 1}}}}
	if _, err := (EngineTarget{Store: store, Docker: other}).Container(ctx, m, manifest.Job{App: "api", Role: "web"}); !errors.As(err, &te) || err.Error() != "no running container for api/web" {
		t.Fatalf("other node: %v", err)
	}
	// The role is not deployed: it has no binding.
	if _, err := (EngineTarget{Store: store, Docker: other}).Container(ctx, m, manifest.Job{App: "api", Role: "worker"}); !errors.As(err, &te) || err.Error() != "no running container for api/worker" {
		t.Fatalf("no binding: %v", err)
	}
	// Docker cannot be asked: the error is not a target error, so its text never reaches the run record.
	broken := fakeDocker{nodeErr: errors.New("boom")}
	if _, err := (EngineTarget{Store: store, Docker: broken}).Container(ctx, m, manifest.Job{App: "api", Role: "web"}); err == nil || errors.As(err, &te) {
		t.Fatalf("docker error: %v", err)
	}
	if _, err := (EngineTarget{Store: store, Docker: other}).Container(ctx, m, manifest.Job{App: "gone"}); err == nil {
		t.Fatal("job of an app that is not in the manifest")
	}
}

func TestComposeTargetPicksTheNamedServicesRunningContainer(t *testing.T) {
	docker := fakeDocker{project: "demo-db", containers: []engine.ComposeContainer{
		{ID: "web-1", Service: "web", State: "running", Number: 1},
		{ID: "pg-stopped", Service: "postgres", State: "exited", Number: 1},
		{ID: "pg-oneoff", Service: "postgres", State: "running", Number: 1, OneOff: true},
		{ID: "pg-3", Service: "postgres", State: "running", Number: 3},
		{ID: "pg-2", Service: "postgres", State: "running", Number: 2},
	}}
	id, err := (EngineTarget{Docker: docker}).Container(context.Background(), targetManifest(), manifest.Job{App: "db", Service: "postgres"})
	if err != nil || id != "pg-2" {
		t.Fatalf("container %q: %v", id, err)
	}
}

func TestComposeTargetWithoutAServiceNeedsExactlyOneRunningService(t *testing.T) {
	ctx := context.Background()
	m := targetManifest()
	var te TargetError
	one := fakeDocker{containers: []engine.ComposeContainer{
		{ID: "pg-1", Service: "postgres", State: "running", Number: 1},
		{ID: "old", Service: "web", State: "exited", Number: 1},
	}}
	if id, err := (EngineTarget{Docker: one}).Container(ctx, m, manifest.Job{App: "db"}); err != nil || id != "pg-1" {
		t.Fatalf("one running service: %q %v", id, err)
	}
	two := fakeDocker{containers: []engine.ComposeContainer{
		{ID: "pg-1", Service: "postgres", State: "running", Number: 1},
		{ID: "web-1", Service: "web", State: "running", Number: 1},
	}}
	if _, err := (EngineTarget{Docker: two}).Container(ctx, m, manifest.Job{App: "db"}); !errors.As(err, &te) || err.Error() != "name the Compose service as service" {
		t.Fatalf("two services: %v", err)
	}
	none := fakeDocker{containers: []engine.ComposeContainer{{ID: "old", Service: "web", State: "exited"}}}
	if _, err := (EngineTarget{Docker: none}).Container(ctx, m, manifest.Job{App: "db"}); !errors.As(err, &te) || err.Error() != "no running container for db" {
		t.Fatalf("none running: %v", err)
	}
	if _, err := (EngineTarget{Docker: none}).Container(ctx, m, manifest.Job{App: "db", Service: "web"}); !errors.As(err, &te) || err.Error() != "no running container for db/web" {
		t.Fatalf("named service not running: %v", err)
	}
}

func TestStatusesListJobsWithNextAndLastRun(t *testing.T) {
	store, _, _ := openTestStore(t)
	ctx := context.Background()
	m := targetManifest()
	m.Jobs = map[string]manifest.Job{
		"tick":   {App: "solo", Schedule: "*/5 * * * *", Timeout: "30s", Command: []string{"date"}},
		"manual": {App: "api", Role: "web", Command: []string{"date"}},
		"dump":   {App: "db", Service: "postgres", Schedule: "30 2 * * *", Command: []string{"date"}},
	}
	run, err := store.StartJobRun(ctx, state.JobRun{Job: "tick", App: "solo", Trigger: "manual", Started: time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	three := 3
	if err = store.FinishJobRun(ctx, run, "failed", &three, "exit status 3"); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 16, 10, 2, 30, 0, time.UTC)
	got, err := Statuses(ctx, store, m, "", now)
	if err != nil || len(got) != 3 {
		t.Fatalf("statuses: %+v %v", got, err)
	}
	dump, manual, tick := got[0], got[1], got[2]
	if dump.Name != "dump" || manual.Name != "manual" || tick.Name != "tick" {
		t.Fatalf("not sorted by name: %v %v %v", dump.Name, manual.Name, tick.Name)
	}
	if tick.App != "solo" || tick.Target != "main" || tick.Schedule != "*/5 * * * *" || tick.Timeout != "30s" || tick.Next == nil || !tick.Next.Equal(time.Date(2026, 11, 16, 10, 5, 0, 0, time.UTC)) {
		t.Fatalf("tick: %+v", tick)
	}
	if tick.Last == nil || tick.Last.Status != "failed" || tick.Last.Detail != "exit status 3" || tick.Last.ExitCode == nil || *tick.Last.ExitCode != 3 || tick.Last.Trigger != "manual" || tick.Last.Finished == nil || tick.Last.Scheduled != nil {
		t.Fatalf("tick last run: %+v", tick.Last)
	}
	if manual.Next != nil || manual.Last != nil || manual.Target != "web" || manual.Schedule != "" || manual.Timeout != "1h0m0s" {
		t.Fatalf("manual: %+v", manual)
	}
	if dump.Target != "postgres" {
		t.Fatalf("dump: %+v", dump)
	}
	one, err := Statuses(ctx, store, m, "solo", now)
	if err != nil || len(one) != 1 || one[0].Name != "tick" {
		t.Fatalf("filtered statuses: %+v %v", one, err)
	}
	var _ []api.JobStatus = one
}

// TestPickComposeContainerChoosesWhatAJobOrAnExecRunsIn: the running, non-one-off container of the named service, or of the
// only service when none is named, the lowest replica first and then the lowest id.
func TestPickComposeContainerChoosesWhatAJobOrAnExecRunsIn(t *testing.T) {
	pg := []engine.ComposeContainer{
		{ID: "pg-3", Service: "postgres", State: "running", Number: 3},
		{ID: "pg-2", Service: "postgres", State: "running", Number: 2},
		{ID: "pg-2b", Service: "postgres", State: "running", Number: 2},
	}
	web := engine.ComposeContainer{ID: "web-1", Service: "web", State: "running", Number: 1}
	oneOff := engine.ComposeContainer{ID: "pg-oneoff", Service: "postgres", State: "running", Number: 1, OneOff: true}
	stopped := engine.ComposeContainer{ID: "pg-stopped", Service: "postgres", State: "exited", Number: 1}
	for _, tc := range []struct {
		name       string
		containers []engine.ComposeContainer
		service    string
		want       string
	}{
		{"named service", append([]engine.ComposeContainer{web, oneOff, stopped}, pg...), "postgres", "pg-2"},
		{"only service", append([]engine.ComposeContainer{oneOff, stopped}, pg...), "", "pg-2"},
		{"lowest id among replicas of one number", []engine.ComposeContainer{{ID: "b", Service: "x", State: "running", Number: 1}, {ID: "a", Service: "x", State: "running", Number: 1}}, "x", "a"},
		{"one-off container is never chosen", []engine.ComposeContainer{oneOff, {ID: "pg-run", Service: "postgres", State: "running", Number: 4}}, "postgres", "pg-run"},
	} {
		got, err := PickComposeContainer(tc.containers, tc.service)
		if err != nil || got != tc.want {
			t.Errorf("%s: container %q, %v; want %q", tc.name, got, err, tc.want)
		}
	}
	if _, err := PickComposeContainer([]engine.ComposeContainer{web, {ID: "pg", Service: "postgres", State: "running", Number: 1}}, ""); !errors.Is(err, ErrSeveralServices) {
		t.Errorf("two services without a name: %v", err)
	}
	if _, err := PickComposeContainer([]engine.ComposeContainer{web, {ID: "pg", Service: "postgres", State: "running", Number: 1}}, "postgres"); err != nil {
		t.Errorf("named service among two: %v", err)
	}
	for _, tc := range []struct {
		name       string
		containers []engine.ComposeContainer
		service    string
	}{
		{"no containers", nil, ""},
		{"only one-off and stopped", []engine.ComposeContainer{oneOff, stopped}, "postgres"},
		{"named service not running", []engine.ComposeContainer{web}, "postgres"},
	} {
		if _, err := PickComposeContainer(tc.containers, tc.service); !errors.Is(err, ErrNoRunningContainer) {
			t.Errorf("%s: %v", tc.name, err)
		}
	}
}
