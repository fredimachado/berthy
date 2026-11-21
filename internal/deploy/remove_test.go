package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"berthy/internal/cloudflare"
	"berthy/internal/engine"
	"berthy/internal/manifest"
	"berthy/internal/state"
)

// volumeDocker is the Docker fake of a removal test: the services of the ingress fixture, plus named volumes. A volume
// whose state is "busy" is held by a container, so Docker refuses to remove it.
type volumeDocker struct {
	*loggedEngine
	mu       sync.Mutex
	volumes  map[string]string // name -> "" when free, "busy" when a container holds it
	removed  []string
	onDelete func(id string)
}

func (d *volumeDocker) DeleteService(ctx context.Context, id string) error {
	if d.onDelete != nil {
		d.onDelete(id)
	}
	d.loggedEngine.calls.add("service delete " + id)
	return d.loggedEngine.DeleteService(ctx, id)
}

func (d *volumeDocker) VolumeExists(_ context.Context, name string) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, ok := d.volumes[name]
	return ok, nil
}

func (d *volumeDocker) RemoveVolume(_ context.Context, name string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	state, ok := d.volumes[name]
	switch {
	case !ok:
		return &engine.StatusError{Code: 404}
	case state == "busy":
		return &engine.StatusError{Code: 409}
	}
	delete(d.volumes, name)
	d.removed = append(d.removed, name)
	d.loggedEngine.calls.add("volume remove " + name)
	return nil
}

// removeApps is the apps block of a two-role image app whose web role publishes the given hostnames and whose worker
// role keeps its data in the named volume demo-test_data.
func removeApps(hostnames ...string) string {
	return `"apps":{"api":{"kind":"image","image":"ghcr.io/acme/api","roles":{"web":` + webRole(hostnames...) + `,"worker":{"health":{"test":["CMD","true"]},"volumes":["data:/data"]}}}}`
}

// removeFixture is an environment with managed ingress applied and api deployed with two roles. api.example.com has a
// record Berthy created; b.example.com had a matching record before Berthy came, which it adopted.
type removeFixture struct {
	*ingressFixture
	docker   *volumeDocker
	m        manifest.Manifest
	tunnelID string
	adopted  string // the adopted record's ID
}

func newRemoveFixture(t *testing.T) *removeFixture {
	t.Helper()
	previous := removeVolumeWait
	removeVolumeWait = 50 * time.Millisecond
	t.Cleanup(func() { removeVolumeWait = previous })
	f := newIngressFixture(t)
	docker := &volumeDocker{loggedEngine: f.docker, volumes: map[string]string{"demo-test_data": ""}}
	f.ctrl.Engine = docker
	m := withIngress(t, removeApps("api.example.com", "b.example.com"))
	f.applyIngress("apply", m)
	tunnelID := f.tunnel().ID
	adopted := f.cf.AddRecord(f.zone, cloudflare.DNSRecord{Type: "CNAME", Name: "b.example.com", Content: cloudflare.TunnelTarget(tunnelID), Proxied: true, Comment: teamComment})
	if op := f.deploy("deploy", "api", apiFirst, m); op.State != "succeeded" {
		t.Fatalf("deploy %s: %s", op.State, op.Err)
	}
	return &removeFixture{ingressFixture: f, docker: docker, m: m, tunnelID: tunnelID, adopted: adopted}
}

func (f *removeFixture) remove(requestID string, mutate func(*Request)) state.Operation {
	f.t.Helper()
	req := Request{RequestID: requestID, Kind: "remove", App: "api"}
	if mutate != nil {
		mutate(&req)
	}
	return f.run(req)
}

func (f *removeFixture) services() []string {
	var names []string
	for _, n := range []string{"demo-test-api-web", "demo-test-api-worker"} {
		if _, err := f.docker.InspectService(context.Background(), n); err == nil {
			names = append(names, n)
		}
	}
	return names
}

// steps lists the step texts an operation reported.
func steps(t *testing.T, s *state.Store, opID string) []string {
	t.Helper()
	events, err := s.EventsAfter(context.Background(), opID, 0, 200)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, ev := range events {
		var data struct {
			Step string `json:"step"`
		}
		if ev.Kind == "step" && json.Unmarshal(ev.Data, &data) == nil && data.Step != "" {
			out = append(out, data.Step)
		}
	}
	return out
}

func hasStep(list []string, want string) bool {
	return slices.ContainsFunc(list, func(s string) bool { return strings.Contains(s, want) })
}

func TestRemoveDeletesServicesRoutesAndStateAndKeepsVolumesAndHistory(t *testing.T) {
	f := newRemoveFixture(t)
	ctx := context.Background()
	if got := f.services(); !reflect.DeepEqual(got, []string{"demo-test-api-web", "demo-test-api-worker"}) {
		t.Fatalf("services before %v", got)
	}
	op := f.remove("remove", nil)
	if op.State != "succeeded" || op.Kind != "remove" || op.App != "api" {
		t.Fatalf("remove %s: %s", op.State, op.Err)
	}
	if got := f.services(); len(got) != 0 {
		t.Fatalf("services left: %v", got)
	}
	if got := describeRules(f.tunnelConfig(f.tunnelID)); !reflect.DeepEqual(got, []string{"||http_status:404"}) {
		// b.example.com had no rule before Berthy, so Berthy created both rules and both go.
		t.Fatalf("rules %v", got)
	}
	records := f.cf.Records(f.zone)
	if len(records) != 1 || records[0].ID != f.adopted || records[0].Comment != teamComment {
		t.Fatalf("the created record goes and the adopted one stays as it was: %+v", records)
	}
	if routes, err := f.store.IngressRoutes(ctx, ""); err != nil || len(routes) != 0 {
		t.Fatalf("routes %+v %v", routes, err)
	}
	if _, err := f.store.Applied(ctx, "api"); err == nil {
		t.Fatal("api is still applied")
	}
	if b, err := f.store.Bindings(ctx, "api"); err != nil || len(b) != 0 {
		t.Fatalf("bindings %+v %v", b, err)
	}
	if h, err := f.store.History(ctx, "api"); err != nil || len(h) != 1 {
		t.Fatalf("history is kept: %+v %v", h, err)
	}
	if len(f.docker.removed) != 0 || f.docker.volumes["demo-test_data"] != "" {
		t.Fatalf("volumes are deleted only on request: removed %v", f.docker.removed)
	}
	got := steps(t, f.store, op.ID)
	for _, want := range []string{"deleted service demo-test-api-web", "deleted service demo-test-api-worker", "kept volume demo-test_data"} {
		if !hasStep(got, want) {
			t.Fatalf("no step %q in %v", want, got)
		}
	}
	if f.docker.calls.index("service delete") < 0 {
		t.Fatalf("calls %v", f.docker.calls.all())
	}
}

func TestRemoveJournalsEachServiceBeforeDeletingIt(t *testing.T) {
	f := newRemoveFixture(t)
	ctx := context.Background()
	var journaled [][]string
	f.docker.onDelete = func(id string) {
		active, err := f.store.Active(ctx)
		if err != nil {
			t.Errorf("no active operation: %v", err)
			return
		}
		intents, err := f.store.Intents(ctx, active.ID)
		if err != nil {
			t.Errorf("intents: %v", err)
			return
		}
		var ids []string
		for _, i := range intents {
			if i.Kind == "remove_service" && len(i.Before) > 0 {
				ids = append(ids, i.ServiceID)
			}
		}
		journaled = append(journaled, ids)
		if !slices.Contains(ids, id) {
			t.Errorf("service %s deleted before its intent was journaled (%v)", id, ids)
		}
	}
	op := f.remove("journal", nil)
	if op.State != "succeeded" {
		t.Fatalf("remove %s: %s", op.State, op.Err)
	}
	if len(journaled) != 2 {
		t.Fatalf("deletions %v", journaled)
	}
	intents, err := f.store.Intents(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, i := range intents {
		if i.Kind != "remove_service" || i.Phase != "finished" {
			t.Fatalf("intent %+v", i)
		}
		names = append(names, i.Name)
	}
	if !reflect.DeepEqual(names, []string{"demo-test-api-web", "demo-test-api-worker"}) {
		t.Fatalf("intents %v", names)
	}
}

func TestRemoveDryRunListsEverythingAndChangesNothing(t *testing.T) {
	f := newRemoveFixture(t)
	ctx := context.Background()
	before := f.cf.Snapshot()
	plan, err := f.ctrl.Plan(ctx, Actor{ID: "admin", Admin: true}, Request{Kind: "remove", App: "api", DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	r := plan.Remove
	if r == nil || r.App != "api" || r.Kind != "image" || len(r.Blockers) != 0 {
		t.Fatalf("plan %+v", plan)
	}
	if !reflect.DeepEqual(r.Services, []string{"demo-test-api-web", "demo-test-api-worker"}) {
		t.Fatalf("services %v", r.Services)
	}
	if len(r.Volumes) != 0 || !reflect.DeepEqual(r.KeptVolumes, []string{"demo-test_data"}) {
		t.Fatalf("volumes %v kept %v", r.Volumes, r.KeptVolumes)
	}
	want := map[string][2]string{"api.example.com": {"remove", "remove"}, "b.example.com": {"remove", "release"}}
	if len(r.Ingress) != len(want) {
		t.Fatalf("ingress %+v", r.Ingress)
	}
	for _, ch := range r.Ingress {
		if w := want[ch.Hostname]; ch.Route != w[0] || ch.DNS != w[1] {
			t.Fatalf("%s: route %s dns %s, want %v", ch.Hostname, ch.Route, ch.DNS, w)
		}
	}
	withVolumes, err := f.ctrl.Plan(ctx, Actor{ID: "admin", Admin: true}, Request{Kind: "remove", App: "api", Volumes: true, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if v := withVolumes.Remove; !reflect.DeepEqual(v.Volumes, []string{"demo-test_data"}) || len(v.KeptVolumes) != 0 {
		t.Fatalf("with --volumes %+v", v)
	}
	if got := f.services(); len(got) != 2 {
		t.Fatalf("a dry run deleted services: %v", got)
	}
	if !reflect.DeepEqual(f.cf.Snapshot(), before) {
		t.Fatal("a dry run changed Cloudflare")
	}
	if _, err = f.store.Applied(ctx, "api"); err != nil {
		t.Fatalf("a dry run forgot the app: %v", err)
	}
	if routes, err := f.store.IngressRoutes(ctx, "api"); err != nil || len(routes) != 2 {
		t.Fatalf("routes %+v %v", routes, err)
	}
}

func TestRemoveDeletesVolumesOnlyWhenAsked(t *testing.T) {
	f := newRemoveFixture(t)
	op := f.remove("volumes", func(r *Request) { r.Volumes = true })
	if op.State != "succeeded" {
		t.Fatalf("remove %s: %s", op.State, op.Err)
	}
	if !reflect.DeepEqual(f.docker.removed, []string{"demo-test_data"}) {
		t.Fatalf("removed volumes %v", f.docker.removed)
	}
	if !hasStep(steps(t, f.store, op.ID), "removed volume demo-test_data") {
		t.Fatalf("steps %v", steps(t, f.store, op.ID))
	}
	// The volume goes only after the services that mounted it.
	calls := f.docker.calls.all()
	last := -1
	for i, c := range calls {
		if strings.HasPrefix(c, "service delete") {
			last = i
		}
	}
	if vol := f.docker.calls.index("volume remove demo-test_data"); last < 0 || vol < last {
		t.Fatalf("the volume must go after the services: %v", calls)
	}
}

func TestRemoveKeepsAndReportsAVolumeAnotherContainerStillUses(t *testing.T) {
	f := newRemoveFixture(t)
	f.docker.volumes["demo-test_data"] = "busy"
	op := f.remove("busy", func(r *Request) { r.Volumes = true })
	if op.State != "succeeded" {
		t.Fatalf("a busy volume does not fail the removal: %s: %s", op.State, op.Err)
	}
	if len(f.docker.removed) != 0 {
		t.Fatalf("removed %v", f.docker.removed)
	}
	if !hasStep(steps(t, f.store, op.ID), "kept volume demo-test_data: in use by another container") {
		t.Fatalf("steps %v", steps(t, f.store, op.ID))
	}
	if got := f.services(); len(got) != 0 {
		t.Fatalf("services %v", got)
	}
}

func TestRemoveNeverTouchesAVolumeTheAppDoesNotName(t *testing.T) {
	f := newRemoveFixture(t)
	f.docker.volumes["other"] = ""
	if op := f.remove("others", func(r *Request) { r.Volumes = true }); op.State != "succeeded" {
		t.Fatalf("remove %s: %s", op.State, op.Err)
	}
	if _, ok := f.docker.volumes["other"]; !ok || !reflect.DeepEqual(f.docker.removed, []string{"demo-test_data"}) {
		t.Fatalf("removed %v", f.docker.removed)
	}
}

func TestRemoveTwiceIsHarmless(t *testing.T) {
	f := newRemoveFixture(t)
	if op := f.remove("first", nil); op.State != "succeeded" {
		t.Fatalf("first %s: %s", op.State, op.Err)
	}
	before := f.cf.Snapshot()
	f.calls.reset()
	op := f.remove("second", nil)
	if op.State != "succeeded" {
		t.Fatalf("second %s: %s", op.State, op.Err)
	}
	if !reflect.DeepEqual(f.cf.Snapshot(), before) || f.calls.count("service delete") != 0 {
		t.Fatalf("a second removal changed something: %v", f.calls.all())
	}
	if !hasStep(steps(t, f.store, op.ID), "already removed") {
		t.Fatalf("steps %v", steps(t, f.store, op.ID))
	}
}

func TestRemoveOfAnAppThatNeverRanIsRefused(t *testing.T) {
	f := newRemoveFixture(t)
	req := Request{RequestID: "never", Kind: "remove", App: "ghost"}
	if _, _, err := admit(t, f.ctrl, context.Background(), Actor{ID: "admin", Admin: true}, req); !errors.Is(err, errRemoveNotDeployed) {
		t.Fatalf("admit: %v", err)
	}
	req.DryRun = true
	if _, err := f.ctrl.Plan(context.Background(), Actor{ID: "admin", Admin: true}, req); !errors.Is(err, errRemoveNotDeployed) {
		t.Fatalf("plan: %v", err)
	}
}

func TestRemoveRefusesTheConnectorApp(t *testing.T) {
	f := newRemoveFixture(t)
	ctx := context.Background()
	want := Refusal("use ingress apply without an ingress block to remove managed ingress")
	req := Request{RequestID: "connector", Kind: "remove", App: "ingress"}
	if _, _, err := admit(t, f.ctrl, ctx, Actor{ID: "admin", Admin: true}, req); !errors.Is(err, want) {
		t.Fatalf("admit: %v", err)
	}
	req.DryRun = true
	if _, err := f.ctrl.Plan(ctx, Actor{ID: "admin", Admin: true}, req); !errors.Is(err, want) {
		t.Fatalf("plan: %v", err)
	}
	if f.connectorMissing() {
		t.Fatal("the connector was removed")
	}
}

func TestRemoveNeedsAnAdministrator(t *testing.T) {
	f := newRemoveFixture(t)
	ctx := context.Background()
	if _, _, err := admit(t, f.ctrl, ctx, Actor{ID: "ci"}, Request{RequestID: "ci", Kind: "remove", App: "api"}); !errors.Is(err, errRemoveAdmin) {
		t.Fatalf("admit: %v", err)
	}
	if _, err := f.ctrl.Plan(ctx, Actor{ID: "ci"}, Request{Kind: "remove", App: "api", DryRun: true}); !errors.Is(err, errRemoveAdmin) {
		t.Fatalf("plan: %v", err)
	}
	if got := f.services(); len(got) != 2 {
		t.Fatalf("services %v", got)
	}
}

func TestRemoveRefusesAServiceThatIsNotTheBoundOne(t *testing.T) {
	f := newRemoveFixture(t)
	// Someone replaced the service behind the binding.
	f.bound.mu.Lock()
	for i := range f.bound.services {
		if specName(f.bound.services[i].Spec) == "demo-test-api-web" {
			f.bound.services[i].ID = "somebody-elses"
		}
	}
	f.bound.mu.Unlock()
	op := f.remove("foreign", nil)
	if op.State != "failed" || op.Err != "bound service ownership changed" {
		t.Fatalf("remove %s: %s", op.State, op.Err)
	}
	if got := f.services(); len(got) != 2 {
		t.Fatalf("nothing may be deleted when one service is not Berthy's: %v", got)
	}
	if _, err := f.store.Applied(context.Background(), "api"); err != nil {
		t.Fatalf("the app must stay applied: %v", err)
	}
	if routes, _ := f.store.IngressRoutes(context.Background(), "api"); len(routes) != 2 {
		t.Fatalf("routes must stay: %+v", routes)
	}
}

func TestRemoveToleratesAServiceThatIsAlreadyGone(t *testing.T) {
	f := newRemoveFixture(t)
	if err := f.docker.DeleteService(context.Background(), idOf(t, f, "demo-test-api-worker")); err != nil {
		t.Fatal(err)
	}
	op := f.remove("partial", nil)
	if op.State != "succeeded" {
		t.Fatalf("remove %s: %s", op.State, op.Err)
	}
	if got := f.services(); len(got) != 0 {
		t.Fatalf("services %v", got)
	}
}

func idOf(t *testing.T, f *removeFixture, name string) string {
	t.Helper()
	live, err := f.docker.InspectService(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	return live.ID
}

func TestRecoveryFinishesAnInterruptedRemoval(t *testing.T) {
	f := newRemoveFixture(t)
	ctx := context.Background()
	web, worker := idOf(t, f, "demo-test-api-web"), idOf(t, f, "demo-test-api-worker")
	live, err := f.docker.InspectService(ctx, "demo-test-api-web")
	if err != nil {
		t.Fatal(err)
	}
	// The process stopped after journaling both deletions and deleting the first one.
	op, _, err := f.store.Admit(ctx, state.Admission{RequestID: "crashed", ActorID: "admin", Kind: "remove", App: "api", PublicRequest: json.RawMessage(`{"app":"api"}`)})
	if err != nil {
		t.Fatal(err)
	}
	for ordinal, b := range []struct{ role, name, id string }{{"web", "demo-test-api-web", web}, {"worker", "demo-test-api-worker", worker}} {
		i := state.Intent{Ordinal: ordinal + 1, Kind: "remove_service", Role: b.role, Name: b.name, ServiceID: b.id, Before: live.Spec, Desired: json.RawMessage(`{}`)}
		if err = f.store.RecordIntent(ctx, op.ID, i); err != nil {
			t.Fatal(err)
		}
	}
	if err = f.docker.DeleteService(ctx, web); err != nil {
		t.Fatal(err)
	}
	if err = f.ctrl.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if got := f.services(); len(got) != 0 {
		t.Fatalf("recovery must finish the deletion: %v", got)
	}
	got, err := f.store.Operation(ctx, op.ID)
	if err != nil || got.State != "interrupted" || got.Err != "remove interrupted; run remove again" {
		t.Fatalf("operation %+v %v", got, err)
	}
	if _, err = f.store.Active(ctx); err == nil {
		t.Fatal("the lock must be released")
	}
	// The app stays applied, so the operator can run remove again to finish the rest.
	if again := f.remove("again", nil); again.State != "succeeded" {
		t.Fatalf("remove again %s: %s", again.State, again.Err)
	}
	if _, err = f.store.Applied(ctx, "api"); err == nil {
		t.Fatal("api is still applied after the second removal")
	}
}

func TestRecoveryLeavesAServiceItDoesNotRecognise(t *testing.T) {
	f := newRemoveFixture(t)
	ctx := context.Background()
	op, _, err := f.store.Admit(ctx, state.Admission{RequestID: "crashed", ActorID: "admin", Kind: "remove", App: "api", PublicRequest: json.RawMessage(`{"app":"api"}`)})
	if err != nil {
		t.Fatal(err)
	}
	live, _ := f.docker.InspectService(ctx, "demo-test-api-web")
	if err = f.store.RecordIntent(ctx, op.ID, state.Intent{Ordinal: 1, Kind: "remove_service", Role: "web", Name: "demo-test-api-web", ServiceID: "an-older-id", Before: live.Spec, Desired: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if err = f.ctrl.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if got := f.services(); len(got) != 2 {
		t.Fatalf("a service with another ID must be left alone: %v", got)
	}
	if got, _ := f.store.Operation(ctx, op.ID); got.State != "interrupted" || got.Err != "remove interrupted; run remove again" {
		t.Fatalf("operation %+v", got)
	}
}

func TestRemovedAppsImagesBecomePrunable(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	seed := func(requestID, app, digest, id string) {
		op, _, err := s.Admit(ctx, state.Admission{RequestID: requestID, ActorID: "a", Kind: "deploy", App: app, PublicRequest: []byte(`{}`)})
		if err != nil {
			t.Fatal(err)
		}
		if err = s.RecordImageAcquired(ctx, digest, id); err != nil {
			t.Fatal(err)
		}
		if err = s.Finish(ctx, op.ID, state.Outcome{State: "succeeded", Release: &state.Release{App: app, Digest: digest, Manifest: []byte(`{"apps":{"` + app + `":{}}}`)}}); err != nil {
			t.Fatal(err)
		}
	}
	seed("kept", "web", "web-digest", "id-web")
	seed("gone", "api", "api-digest", "id-api")
	op, _, err := s.Admit(ctx, state.Admission{RequestID: "forget", ActorID: "a", Kind: "remove", App: "api", PublicRequest: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ForgetApp(ctx, op.ID, "api"); err != nil {
		t.Fatal(err)
	}
	if err = s.Finish(ctx, op.ID, state.Outcome{State: "succeeded"}); err != nil {
		t.Fatal(err)
	}
	f := &pruneEngine{runtimeEngine: &runtimeEngine{}, images: []engine.InventoryImage{{ID: "id-web", RepoDigests: []string{"web-digest"}}, {ID: "id-api", RepoDigests: []string{"api-digest"}}}}
	c := Controller{Store: s, Engine: f}
	prune, _, err := s.Admit(ctx, state.Admission{RequestID: "prune", ActorID: "a", Kind: "prune", PublicRequest: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	out := c.runOperation(ctx, prune, Actor{Admin: true}, Request{Kind: "prune"})
	if out.State != "succeeded" || !reflect.DeepEqual(f.removed, []string{"api-digest"}) {
		t.Fatalf("the removed app's image is prunable and the deployed app's is not: %v removed %v", out, f.removed)
	}
}

// Compose apps.

// downDocker is the Compose fake's Docker: containers and volumes that the fake runner's down removes.
func composeRemoveFixture(t *testing.T, stateful bool) (*Controller, *state.Store, *fakeCompose, *composeDocker) {
	t.Helper()
	runner := &fakeCompose{config: configOne, hashes: hashOne}
	docker := newComposeDocker(engine.ComposeContainer{ID: "c1", Service: "database", ConfigHash: "hash-0000000000000001", State: "running"})
	docker.projectVolumes = []string{"shop-db_data"}
	runner.onDown = func(r engine.ComposeRun) { docker.containers = nil }
	c, s := composeController(t, runner, docker)
	m := composeApp(stateful)
	op, _, err := admit(t, c, context.Background(), Actor{ID: "ci", Admin: true}, composeReq("deploy", composeFileOne, m))
	if err != nil {
		t.Fatal(err)
	}
	if got := waitComposeOperation(t, s, op.ID); got.State != "succeeded" {
		t.Fatalf("deploy %s: %s", got.State, got.Err)
	}
	return c, s, runner, docker
}

func removeCompose(t *testing.T, c *Controller, s *state.Store, id string, admin bool, mutate func(*Request)) state.Operation {
	t.Helper()
	req := Request{RequestID: id, Kind: "remove", App: "db"}
	if mutate != nil {
		mutate(&req)
	}
	op, _, err := admit(t, c, context.Background(), Actor{ID: "admin", Admin: admin}, req)
	if err != nil {
		t.Fatal(err)
	}
	return waitComposeOperation(t, s, op.ID)
}

func downRuns(r *fakeCompose) []engine.ComposeRun {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []engine.ComposeRun
	for _, run := range r.runs {
		if run.Args[0] == "down" {
			out = append(out, run)
		}
	}
	return out
}

func TestRemoveComposeBringsTheProjectDownAndKeepsItsVolumes(t *testing.T) {
	c, s, runner, _ := composeRemoveFixture(t, false)
	project := "shop-db"
	if _, err := os.Stat(c.composeDir(project)); err != nil {
		t.Fatalf("the project directory is missing before the removal: %v", err)
	}
	op := removeCompose(t, c, s, "remove", true, nil)
	if op.State != "succeeded" {
		t.Fatalf("remove %s: %s", op.State, op.Err)
	}
	runs := downRuns(runner)
	if len(runs) != 1 || !reflect.DeepEqual(runs[0].Args, []string{"down", "--remove-orphans"}) || runs[0].Dir != c.composeDir(project) || runs[0].NoFiles || runs[0].Project != project {
		t.Fatalf("down runs %+v", runs)
	}
	if _, err := os.Stat(c.composeDir(project)); !os.IsNotExist(err) {
		t.Fatalf("the project directory must go: %v", err)
	}
	if _, err := s.Applied(context.Background(), "db"); err == nil {
		t.Fatal("db is still applied")
	}
	if h, _ := s.History(context.Background(), "db"); len(h) != 1 {
		t.Fatalf("history %+v", h)
	}
	if got := steps(t, s, op.ID); !hasStep(got, "brought down Compose project shop-db") || !hasStep(got, "kept volume shop-db_data") {
		t.Fatalf("steps %v", got)
	}
}

func TestRemoveComposeWithVolumesAddsTheFlag(t *testing.T) {
	c, s, runner, _ := composeRemoveFixture(t, false)
	op := removeCompose(t, c, s, "remove", true, func(r *Request) { r.Volumes = true })
	if op.State != "succeeded" {
		t.Fatalf("remove %s: %s", op.State, op.Err)
	}
	runs := downRuns(runner)
	if len(runs) != 1 || !reflect.DeepEqual(runs[0].Args, []string{"down", "--remove-orphans", "--volumes"}) {
		t.Fatalf("down runs %+v", runs)
	}
}

func TestRemoveStatefulComposeNeedsAllowRecreate(t *testing.T) {
	c, s, runner, _ := composeRemoveFixture(t, true)
	op := removeCompose(t, c, s, "refused", true, nil)
	if op.State != "failed" || op.Err != "removing stateful app db needs --allow-recreate" {
		t.Fatalf("remove %s: %q", op.State, op.Err)
	}
	if len(downRuns(runner)) != 0 {
		t.Fatal("compose ran for a refused removal")
	}
	if _, err := os.Stat(c.composeDir("shop-db")); err != nil {
		t.Fatalf("the project directory must stay: %v", err)
	}
	if _, err := s.Applied(context.Background(), "db"); err != nil {
		t.Fatalf("the app must stay applied: %v", err)
	}
	plan, err := c.Plan(context.Background(), Actor{ID: "admin", Admin: true}, Request{Kind: "remove", App: "db", DryRun: true})
	if err != nil || plan.Remove == nil || !reflect.DeepEqual(plan.Remove.Blockers, []string{"removing stateful app db needs --allow-recreate"}) {
		t.Fatalf("plan %+v %v", plan.Remove, err)
	}
	op = removeCompose(t, c, s, "allowed", true, func(r *Request) { r.AllowRecreate = true })
	if op.State != "succeeded" {
		t.Fatalf("remove with --allow-recreate %s: %s", op.State, op.Err)
	}
	if len(downRuns(runner)) != 1 {
		t.Fatalf("down runs %+v", downRuns(runner))
	}
}

func TestRemoveComposeWithoutItsProjectFilesBringsTheProjectDownByName(t *testing.T) {
	c, s, runner, _ := composeRemoveFixture(t, false)
	if err := os.RemoveAll(c.composeDir("shop-db")); err != nil {
		t.Fatal(err)
	}
	op := removeCompose(t, c, s, "remove", true, func(r *Request) { r.Volumes = true })
	if op.State != "succeeded" {
		t.Fatalf("remove %s: %s", op.State, op.Err)
	}
	runs := downRuns(runner)
	if len(runs) != 1 || !runs[0].NoFiles || runs[0].Project != "shop-db" || !reflect.DeepEqual(runs[0].Args, []string{"down", "--remove-orphans", "--volumes"}) {
		t.Fatalf("down runs %+v", runs)
	}
	if _, err := os.Stat(runs[0].Dir); !os.IsNotExist(err) {
		t.Fatalf("the throwaway directory must go: %v", err)
	}
}

func TestRemoveComposePlanListsServicesAndVolumes(t *testing.T) {
	c, _, runner, _ := composeRemoveFixture(t, false)
	plan, err := c.Plan(context.Background(), Actor{ID: "admin", Admin: true}, Request{Kind: "remove", App: "db", Volumes: true, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	r := plan.Remove
	if r == nil || r.Kind != "compose" || r.Project != "shop-db" || !reflect.DeepEqual(r.Services, []string{"database"}) || !reflect.DeepEqual(r.Volumes, []string{"shop-db_data"}) || len(r.KeptVolumes) != 0 || len(r.Blockers) != 0 {
		t.Fatalf("plan %+v", r)
	}
	if len(downRuns(runner)) != 0 {
		t.Fatal("a dry run ran compose down")
	}
}

func TestRemoveComposeFailureKeepsTheAppAndItsFiles(t *testing.T) {
	c, s, runner, _ := composeRemoveFixture(t, false)
	runner.downErr = errors.New("compose could not run")
	op := removeCompose(t, c, s, "remove", true, nil)
	if op.State != "failed" || op.Err != string(errRemoveCompose) {
		t.Fatalf("remove %s: %q", op.State, op.Err)
	}
	if _, err := os.Stat(c.composeDir("shop-db")); err != nil {
		t.Fatalf("the project directory must stay so the removal can be repeated: %v", err)
	}
	if _, err := s.Applied(context.Background(), "db"); err != nil {
		t.Fatalf("the app must stay applied: %v", err)
	}
}

func TestRecoveryRepeatsAnInterruptedComposeDown(t *testing.T) {
	c, s, runner, docker := composeRemoveFixture(t, false)
	ctx := context.Background()
	// The process stopped after journaling the down and before it finished.
	op, _, err := s.Admit(ctx, state.Admission{RequestID: "crashed", ActorID: "admin", Kind: "remove", App: "db", PublicRequest: json.RawMessage(`{"app":"db","volumes":true}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RecordIntent(ctx, op.ID, state.Intent{Ordinal: 1, Kind: composeDownKind, Name: "shop-db", Desired: json.RawMessage(`{"project":"shop-db","volumes":true}`)}); err != nil {
		t.Fatal(err)
	}
	if err = c.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	runs := downRuns(runner)
	if len(runs) != 1 || !reflect.DeepEqual(runs[0].Args, []string{"down", "--remove-orphans", "--volumes"}) || len(docker.containers) != 0 {
		t.Fatalf("recovery must repeat the down: %+v", runs)
	}
	got, err := s.Operation(ctx, op.ID)
	if err != nil || got.State != "interrupted" || got.Err != string(errRemoveInterrupted) {
		t.Fatalf("operation %+v %v", got, err)
	}
	// The app is still applied, so the removal can be run again to finish.
	if _, err = s.Applied(ctx, "db"); err != nil {
		t.Fatalf("applied: %v", err)
	}
}

// setServiceLabels replaces the labels of a fixture service, at the service and at its tasks.
func setServiceLabels(t *testing.T, f *removeFixture, name string, labels map[string]string) {
	t.Helper()
	f.bound.mu.Lock()
	defer f.bound.mu.Unlock()
	for i := range f.bound.services {
		if specName(f.bound.services[i].Spec) != name {
			continue
		}
		var spec map[string]any
		if err := json.Unmarshal(f.bound.services[i].Spec, &spec); err != nil {
			t.Fatal(err)
		}
		spec["Labels"] = labels
		task := spec["TaskTemplate"].(map[string]any)
		container := task["ContainerSpec"].(map[string]any)
		container["Labels"] = labels
		raw, err := json.Marshal(spec)
		if err != nil {
			t.Fatal(err)
		}
		f.bound.services[i].Spec = raw
	}
}

func TestRemoveDeletesAnAdoptedServiceThatCarriesNoLabels(t *testing.T) {
	f := newRemoveFixture(t)
	for _, n := range []string{"demo-test-api-web", "demo-test-api-worker"} {
		setServiceLabels(t, f, n, nil)
	}
	op := f.remove("unlabeled", nil)
	if op.State != "succeeded" {
		t.Fatalf("remove %s: %s", op.State, op.Err)
	}
	if got := f.services(); len(got) != 0 {
		t.Fatalf("services %v", got)
	}
}

func TestRemoveRefusesAServiceWhoseLabelsNameAnotherOwner(t *testing.T) {
	for name, labels := range map[string]map[string]string{
		"another app":         {"berthy.environment": ingressEnv, "berthy.app": "other", "berthy.role": "web"},
		"another environment": {"berthy.environment": "elsewhere", "berthy.app": "api", "berthy.role": "web"},
		"another role":        {"berthy.environment": ingressEnv, "berthy.app": "api", "berthy.role": "worker"},
		"only the app label":  {"berthy.app": "other"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newRemoveFixture(t)
			setServiceLabels(t, f, "demo-test-api-web", labels)
			op := f.remove("foreign-labels", nil)
			if op.State != "failed" || op.Err != "bound service ownership changed" {
				t.Fatalf("remove %s: %s", op.State, op.Err)
			}
			if got := f.services(); len(got) != 2 {
				t.Fatalf("nothing may be deleted: %v", got)
			}
		})
	}
}

func TestRemoveRefusesAServiceLabelledForAnotherProject(t *testing.T) {
	f := newRemoveFixture(t)
	setServiceLabels(t, f, "demo-test-api-web", map[string]string{"berthy.project": "other", "berthy.environment": ingressEnv, "berthy.app": "api", "berthy.role": "web"})
	op := f.remove("foreign-project", nil)
	if op.State != "failed" || op.Err != "bound service ownership changed" || len(f.services()) != 2 {
		t.Fatalf("remove %s: %s; services %v", op.State, op.Err, f.services())
	}
}

func TestRemoveWithVolumesRemovesTheEnvironmentsVolumeOnly(t *testing.T) {
	f := newRemoveFixture(t)
	f.docker.volumes["demo-test_data"] = ""
	f.docker.volumes["demo-prod_data"] = ""
	f.docker.volumes["data"] = ""
	op := f.remove("environment-volumes", func(r *Request) { r.Volumes = true })
	if op.State != "succeeded" {
		t.Fatalf("remove %s: %s", op.State, op.Err)
	}
	if !reflect.DeepEqual(f.docker.removed, []string{"demo-test_data"}) {
		t.Fatalf("removed %v", f.docker.removed)
	}
	if _, ok := f.docker.volumes["demo-prod_data"]; !ok {
		t.Fatal("removed the other environment's volume")
	}
	if _, ok := f.docker.volumes["data"]; !ok {
		t.Fatal("removed the unscoped volume")
	}
}
