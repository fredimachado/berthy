package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"berthy/internal/cloudflare"
	"berthy/internal/engine"
	"berthy/internal/state"
)

// destroyDocker is the Docker of a destroy test: the services, volumes and Compose containers of the other fakes, plus a
// container and image inventory that removals change. Docker refuses an image that a container still uses.
type destroyDocker struct {
	*volumeDocker
	bound      *boundFake
	imu        sync.Mutex
	containers []engine.InventoryContainer
	images     []engine.InventoryImage
	project    []engine.ComposeContainer
	// laggards maps a service name to the task containers Docker removes shortly after the service is deleted.
	laggards map[string][]string
}

func (d *destroyDocker) DeleteService(ctx context.Context, id string) error {
	name := ""
	if live, err := d.volumeDocker.InspectService(ctx, id); err == nil {
		name = specName(live.Spec)
	}
	if err := d.volumeDocker.DeleteService(ctx, id); err != nil {
		return err
	}
	d.imu.Lock()
	tasks := d.laggards[name]
	d.imu.Unlock()
	for _, task := range tasks {
		time.AfterFunc(150*time.Millisecond, func() { _ = d.RemoveContainer(context.Background(), task) })
	}
	return nil
}

func (d *destroyDocker) InventoryContainers(context.Context) ([]engine.InventoryContainer, error) {
	d.imu.Lock()
	defer d.imu.Unlock()
	return append([]engine.InventoryContainer(nil), d.containers...), nil
}

func (d *destroyDocker) InventoryImages(context.Context) ([]engine.InventoryImage, error) {
	d.imu.Lock()
	defer d.imu.Unlock()
	return append([]engine.InventoryImage(nil), d.images...), nil
}

func (d *destroyDocker) InventoryServices(ctx context.Context) ([]engine.InventoryService, error) {
	d.bound.mu.Lock()
	defer d.bound.mu.Unlock()
	var out []engine.InventoryService
	for _, s := range d.bound.services {
		out = append(out, engine.InventoryService{ID: s.ID, Spec: s.Spec})
	}
	return out, nil
}

func (d *destroyDocker) RemoveContainer(_ context.Context, id string) error {
	d.imu.Lock()
	defer d.imu.Unlock()
	for i, c := range d.containers {
		if c.ID == id {
			d.containers = append(d.containers[:i], d.containers[i+1:]...)
			d.calls.add("container remove " + id)
			return nil
		}
	}
	return &engine.StatusError{Code: 404}
}

// removeImage removes the image a reference or ID names, unless a container uses it.
func (d *destroyDocker) removeImage(ref string) error {
	d.imu.Lock()
	defer d.imu.Unlock()
	for i, img := range d.images {
		if img.ID != ref && !slices.Contains(img.RepoTags, ref) && !slices.Contains(img.RepoDigests, ref) {
			continue
		}
		for _, c := range d.containers {
			if c.ImageID == img.ID {
				return &engine.StatusError{Code: 409}
			}
		}
		d.images = append(d.images[:i], d.images[i+1:]...)
		d.calls.add("image remove " + ref)
		return nil
	}
	return &engine.StatusError{Code: 404}
}

func (d *destroyDocker) RemoveUnusedImage(_ context.Context, ref string) error {
	return d.removeImage(ref)
}
func (d *destroyDocker) RemoveImageRef(_ context.Context, ref string) error {
	return d.removeImage(ref)
}

func (d *destroyDocker) InspectImage(_ context.Context, ref string) (engine.Image, error) {
	d.imu.Lock()
	defer d.imu.Unlock()
	for _, img := range d.images {
		if img.ID == ref || slices.Contains(img.RepoTags, ref) || slices.Contains(img.RepoDigests, ref) {
			return engine.Image{ID: img.ID}, nil
		}
	}
	return engine.Image{ID: "image"}, nil
}

func (d *destroyDocker) ComposeContainers(context.Context, string) ([]engine.ComposeContainer, error) {
	d.imu.Lock()
	defer d.imu.Unlock()
	return append([]engine.ComposeContainer(nil), d.project...), nil
}

func (d *destroyDocker) InspectVolume(context.Context, string) (string, map[string]string, error) {
	return "local", nil, nil
}

func (d *destroyDocker) ProjectVolumes(context.Context, string) ([]string, error) {
	return []string{"shop-db_data"}, nil
}

const (
	helperTag   = "berthy-helper:aabbccddeeff"
	acquiredRef = "ghcr.io/acme/api@sha256:" + "1111111111111111111111111111111111111111111111111111111111111111"
	heldRef     = "ghcr.io/acme/held@sha256:" + "2222222222222222222222222222222222222222222222222222222222222222"
)

// destroyFixture is one environment with everything Berthy makes in it: managed ingress with a tunnel, an image app with a
// hostname and a volume, a stateful Compose app, the probe helper, a finished archive helper, one-off containers, and
// images Berthy pulled. A few things are not Berthy's, and must survive.
type destroyFixture struct {
	*ingressFixture
	docker   *destroyDocker
	runner   *fakeCompose
	tunnelID string
}

func newDestroyFixture(t *testing.T, adoptRecord bool) *destroyFixture {
	t.Helper()
	removeVolumeWait = 50 * time.Millisecond
	destroyTunnelWait = 200 * time.Millisecond
	t.Cleanup(func() { removeVolumeWait, destroyTunnelWait = 20*time.Second, time.Minute })
	f := newIngressFixture(t)
	f.ctrl.Project = "demo"
	vd := &volumeDocker{loggedEngine: f.docker, volumes: map[string]string{"demo-test_data": ""}}
	d := &destroyDocker{volumeDocker: vd, bound: f.bound}
	runner := &fakeCompose{config: configOne, hashes: hashOne}
	runner.onDown = func(engine.ComposeRun) { d.imu.Lock(); d.project = nil; d.imu.Unlock() }
	f.ctrl.Engine, f.ctrl.Compose, f.ctrl.StateDir = d, runner, t.TempDir()
	m := withIngress(t, removeApps("api.example.com", "b.example.com"))
	f.applyIngress("apply", m)
	tunnelID := f.tunnel().ID
	if adoptRecord {
		f.cf.AddRecord(f.zone, cloudflare.DNSRecord{Type: "CNAME", Name: "b.example.com", Content: cloudflare.TunnelTarget(tunnelID), Proxied: true, Comment: teamComment})
	}
	if op := f.deploy("deploy-api", "api", apiFirst, m); op.State != "succeeded" {
		t.Fatalf("deploy api %s: %s", op.State, op.Err)
	}
	db := composeApp(true)
	db.Project = m.Project // one environment runs one project
	if op := f.run(composeReq("deploy-db", composeFileOne, db)); op.State != "succeeded" {
		t.Fatalf("deploy db %s: %s", op.State, op.Err)
	}
	ctx := context.Background()
	acquire, _, err := f.store.Admit(ctx, state.Admission{RequestID: "acquire", ActorID: "admin", Kind: "deploy", App: "api", PublicRequest: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range []state.AcquiredImage{{Ref: acquiredRef, ID: "id-acquired"}, {Ref: heldRef, ID: "id-held"}} {
		if err = f.store.RecordImageAcquired(ctx, x.Ref, x.ID); err != nil {
			t.Fatal(err)
		}
	}
	if err = f.store.Finish(ctx, acquire.ID, state.Outcome{State: "succeeded"}); err != nil {
		t.Fatal(err)
	}
	mine := func(extra map[string]string) map[string]string {
		labels := map[string]string{"berthy.project": "demo", "berthy.environment": ingressEnv}
		for k, v := range extra {
			labels[k] = v
		}
		return labels
	}
	d.images = []engine.InventoryImage{
		{ID: "id-helper", RepoTags: []string{helperTag}},
		{ID: "id-stale-helper", RepoTags: []string{"berthy-helper:bbbbbbbbbbbb"}},
		{ID: "id-acquired", RepoDigests: []string{acquiredRef}},
		{ID: "id-held", RepoDigests: []string{heldRef}},
		{ID: "id-foreign", RepoTags: []string{"nginx:alpine"}},
	}
	d.containers = []engine.InventoryContainer{
		{ID: "probe-helper", Names: []string{"/berthy-helper-test"}, ImageID: "id-helper", State: "running", Labels: mine(map[string]string{"berthy.helper": "probe"})},
		{ID: "archive-done", Names: []string{"/archive-1"}, ImageID: "id-foreign", State: "exited", Labels: mine(map[string]string{"berthy.helper": "archive"})},
		{ID: "restore-running", Names: []string{"/restore-1"}, ImageID: "id-helper", State: "running", Labels: mine(map[string]string{"berthy.helper": "restore"})},
		{ID: "other-env-helper", Names: []string{"/other"}, ImageID: "id-foreign", State: "exited", Labels: map[string]string{"berthy.environment": "elsewhere", "berthy.helper": "archive"}},
		{ID: "hook-done", Names: []string{"/hook-1"}, ImageID: "id-foreign", State: "exited", Labels: mine(map[string]string{"berthy.oneoff": "pre_hook", "berthy.operation": "op1"})},
		{ID: "hook-running", Names: []string{"/hook-2"}, ImageID: "id-foreign", State: "running", Labels: mine(map[string]string{"berthy.oneoff": "migration_apply", "berthy.operation": "op2"})},
		{ID: "other-env-hook", Names: []string{"/hook-3"}, ImageID: "id-foreign", State: "exited", Labels: map[string]string{"berthy.environment": "elsewhere", "berthy.oneoff": "pre_hook"}},
		{ID: "foreign", Names: []string{"/nginx"}, ImageID: "id-held", State: "running", Labels: map[string]string{}},
	}
	d.project = []engine.ComposeContainer{{ID: "c1", Service: "database", State: "running"}}
	f.calls.reset()
	return &destroyFixture{ingressFixture: f, docker: d, runner: runner, tunnelID: tunnelID}
}

func (f *destroyFixture) destroy(requestID string, mutate func(*Request)) state.Operation {
	f.t.Helper()
	req := Request{RequestID: requestID, Kind: "destroy", Confirm: f.ctrl.id().String(), AllowRecreate: true}
	if mutate != nil {
		mutate(&req)
	}
	return f.run(req)
}

func (f *destroyFixture) plan(mutate func(*Request)) *DestroyPlan {
	f.t.Helper()
	req := Request{Kind: "destroy", Confirm: f.ctrl.id().String(), AllowRecreate: true, DryRun: true}
	if mutate != nil {
		mutate(&req)
	}
	p, err := f.ctrl.Plan(context.Background(), Actor{ID: "admin", Admin: true}, req)
	if err != nil {
		f.t.Fatal(err)
	}
	if p.Destroy == nil {
		f.t.Fatal("no destroy plan")
	}
	return p.Destroy
}

func (f *destroyFixture) containerIDs() []string {
	f.docker.imu.Lock()
	defer f.docker.imu.Unlock()
	var ids []string
	for _, c := range f.docker.containers {
		ids = append(ids, c.ID)
	}
	return ids
}

func (f *destroyFixture) imageIDs() []string {
	f.docker.imu.Lock()
	defer f.docker.imu.Unlock()
	var ids []string
	for _, i := range f.docker.images {
		ids = append(ids, i.ID)
	}
	return ids
}

func (f *destroyFixture) allServices() []string {
	f.docker.bound.mu.Lock()
	defer f.docker.bound.mu.Unlock()
	var names []string
	for _, s := range f.docker.bound.services {
		names = append(names, specName(s.Spec))
	}
	return names
}

func TestDestroyRemovesWhatBerthyMadeInOrderAndKeepsTheRest(t *testing.T) {
	f := newDestroyFixture(t, true)
	ctx := context.Background()
	if err := f.store.PutSecret(ctx, state.Actor{ID: "admin"}, state.SecretInfo{Name: "KEPT_SECRET"}, []byte("v")); err != nil {
		t.Fatal(err)
	}
	foreignRecord := f.cf.AddRecord(f.zone, cloudflare.DNSRecord{Type: "A", Name: "other.example.com", Content: "203.0.113.9"})
	op := f.destroy("destroy", nil)
	if op.State != "succeeded" || op.Kind != "destroy" {
		t.Fatalf("destroy %s: %s", op.State, op.Err)
	}
	if got := f.allServices(); len(got) != 0 {
		t.Fatalf("services left: %v", got)
	}
	if got := f.containerIDs(); !reflect.DeepEqual(got, []string{"restore-running", "other-env-helper", "hook-running", "other-env-hook", "foreign"}) {
		t.Fatalf("containers left: %v", got)
	}
	if got := f.imageIDs(); !reflect.DeepEqual(got, []string{"id-helper", "id-stale-helper", "id-held", "id-foreign"}) {
		// Every helper image stays, the unused one too: it belongs to the agent, which every hosted environment shares, and
		// another environment's backup may be about to use it. The held image stays while a foreign container uses it.
		t.Fatalf("images left: %v", got)
	}
	if got := describeRules(f.tunnelConfig(f.tunnelID)); !reflect.DeepEqual(got, []string{"||http_status:404"}) {
		t.Fatalf("rules %v", got)
	}
	records := f.cf.Records(f.zone)
	var names []string
	for _, r := range records {
		names = append(names, r.Name+"|"+r.Comment)
	}
	slices.Sort(names)
	if !reflect.DeepEqual(names, []string{"b.example.com|" + teamComment, "other.example.com|"}) {
		t.Fatalf("records %v", names)
	}
	if records[0].ID == "" || foreignRecord == "" {
		t.Fatal("setup")
	}
	if _, err := f.store.Ingress(ctx); err == nil {
		t.Fatal("managed ingress state is still stored")
	}
	if applied, err := f.store.AppliedApps(ctx); err != nil || len(applied) != 0 {
		t.Fatalf("applied %v %v", applied, err)
	}
	if secrets, err := f.store.Secrets(ctx); err != nil || len(secrets) < 2 {
		t.Fatalf("secrets must stay: %v %v", secrets, err)
	}
	if h, err := f.store.History(ctx, ""); err != nil || len(h) < 3 {
		t.Fatalf("history must stay: %v %v", h, err)
	}
	if len(f.docker.removed) != 0 || f.docker.volumes["demo-test_data"] != "" {
		t.Fatalf("volumes are kept without --volumes: removed %v", f.docker.removed)
	}
	if runs := downRuns(f.runner); len(runs) != 1 || !reflect.DeepEqual(runs[0].Args, []string{"down", "--remove-orphans"}) {
		t.Fatalf("down runs %+v", runs)
	}
	// Apps first, then the connector, then containers, then images.
	calls := f.calls.all()
	lastService, firstContainer, lastContainer, firstImage := -1, -1, -1, -1
	for i, c := range calls {
		switch {
		case strings.HasPrefix(c, "service delete"):
			lastService = i
		case strings.HasPrefix(c, "container remove"):
			if firstContainer < 0 {
				firstContainer = i
			}
			lastContainer = i
		case strings.HasPrefix(c, "image remove"):
			if firstImage < 0 {
				firstImage = i
			}
		}
	}
	if lastService < 0 || firstContainer < lastService || firstImage < lastContainer {
		t.Fatalf("order of removals: %v", calls)
	}
	got := steps(t, f.store, op.ID)
	for _, want := range []string{"api: deleted service demo-test-api-web", "db: brought down Compose project demo-test-db", "api: kept volume demo-test_data", "removed container berthy-helper-test", "removed container archive-1", "kept container restore-1: running", "removed container hook-1", "kept container hook-2: running", "removed image " + acquiredRef} {
		if !hasStep(got, want) {
			t.Errorf("no step %q in %v", want, got)
		}
	}
	if f.connectorMissing() == false {
		t.Fatal("the connector is still there")
	}
}

// withoutImageMethods is the fixture's Docker with the two image methods shadowed by ones of another signature, so it no longer
// has them as the engine interfaces name them.
type withoutImageMethods struct{ *destroyDocker }

func (withoutImageMethods) InventoryImages()   {}
func (withoutImageMethods) RemoveUnusedImage() {}

// A destroy needs container inventory and removal only; an engine without image methods still gets its containers cleaned up.
func TestDestroyCleansContainersOfAnEngineWithoutImageMethods(t *testing.T) {
	f := newDestroyFixture(t, true)
	f.ctrl.Engine = withoutImageMethods{f.docker}
	op := f.destroy("destroy", nil)
	if op.State != "succeeded" {
		t.Fatalf("destroy %s: %s", op.State, op.Err)
	}
	if got := f.containerIDs(); !reflect.DeepEqual(got, []string{"restore-running", "other-env-helper", "hook-running", "other-env-hook", "foreign"}) {
		t.Fatalf("containers left: %v", got)
	}
	if got := f.imageIDs(); len(got) != 5 {
		t.Fatalf("an engine without image methods lost images: %v", got)
	}
}

func TestDestroyDryRunListsEverythingAndChangesNothing(t *testing.T) {
	f := newDestroyFixture(t, true)
	before := f.cf.Snapshot()
	p := f.plan(func(r *Request) { r.Tunnel = true })
	if len(p.Blockers) != 0 {
		t.Fatalf("blockers %v", p.Blockers)
	}
	var apps []string
	for _, a := range p.Apps {
		apps = append(apps, a.App)
	}
	if !reflect.DeepEqual(apps, []string{"api", "db"}) {
		t.Fatalf("apps %v", apps)
	}
	if !reflect.DeepEqual(p.Helpers, []string{"berthy-helper-test", "archive-1"}) || !reflect.DeepEqual(p.OneOffs, []string{"hook-1"}) {
		t.Fatalf("helpers %v one-offs %v", p.Helpers, p.OneOffs)
	}
	if !reflect.DeepEqual(p.Images, []string{acquiredRef}) {
		t.Fatalf("images %v", p.Images)
	}
	if !p.RemoveConnector {
		t.Fatal("the plan does not say the connector goes")
	}
	if p.Tunnel == nil || p.Tunnel.Name != ingressTunnel || p.Tunnel.ID != f.tunnelID {
		t.Fatalf("tunnel %+v", p.Tunnel)
	}
	if got := f.allServices(); len(got) != 3 {
		t.Fatalf("a dry run deleted services: %v", got)
	}
	if len(f.containerIDs()) != 8 || len(f.imageIDs()) != 5 {
		t.Fatal("a dry run removed containers or images")
	}
	if !reflect.DeepEqual(f.cf.Snapshot(), before) {
		t.Fatal("a dry run changed Cloudflare")
	}
	if len(downRuns(f.runner)) != 0 {
		t.Fatal("a dry run brought a project down")
	}
	if _, err := f.store.Ingress(context.Background()); err != nil {
		t.Fatalf("a dry run forgot the ingress: %v", err)
	}
}

func TestDestroyRefusesWholeWhenAStatefulComposeAppNeedsAllowRecreate(t *testing.T) {
	f := newDestroyFixture(t, true)
	p := f.plan(func(r *Request) { r.AllowRecreate = false })
	if !reflect.DeepEqual(p.Blockers, []string{"removing stateful app db needs --allow-recreate"}) {
		t.Fatalf("blockers %v", p.Blockers)
	}
	before := f.cf.Snapshot()
	op := f.destroy("refused", func(r *Request) { r.AllowRecreate = false })
	if op.State != "failed" || op.Err != "removing stateful app db needs --allow-recreate" {
		t.Fatalf("destroy %s: %q", op.State, op.Err)
	}
	if got := f.allServices(); len(got) != 3 {
		t.Fatalf("nothing may be removed: %v", got)
	}
	if len(f.containerIDs()) != 8 || len(f.imageIDs()) != 5 || len(downRuns(f.runner)) != 0 || !reflect.DeepEqual(f.cf.Snapshot(), before) {
		t.Fatal("something was removed")
	}
	if applied, _ := f.store.AppliedApps(context.Background()); len(applied) != 3 {
		t.Fatalf("applied %v", applied)
	}
}

func TestDestroyDeletesVolumesOnlyWithTheFlag(t *testing.T) {
	f := newDestroyFixture(t, true)
	op := f.destroy("volumes", func(r *Request) { r.Volumes = true })
	if op.State != "succeeded" {
		t.Fatalf("destroy %s: %s", op.State, op.Err)
	}
	if !reflect.DeepEqual(f.docker.removed, []string{"demo-test_data"}) {
		t.Fatalf("removed volumes %v", f.docker.removed)
	}
	if runs := downRuns(f.runner); len(runs) != 1 || !reflect.DeepEqual(runs[0].Args, []string{"down", "--remove-orphans", "--volumes"}) {
		t.Fatalf("down runs %+v", runs)
	}
}

func TestDestroyNeedsAnAdministratorAndTheEnvironmentName(t *testing.T) {
	f := newDestroyFixture(t, true)
	ctx := context.Background()
	confirmOK := f.ctrl.id().String()
	req := Request{RequestID: "d", Kind: "destroy", Confirm: confirmOK}
	if _, _, err := admit(t, f.ctrl, ctx, Actor{ID: "ci"}, req); !errors.Is(err, errDestroyAdmin) {
		t.Fatalf("admit as deploy: %v", err)
	}
	if _, err := f.ctrl.Plan(ctx, Actor{ID: "ci"}, Request{Kind: "destroy", Confirm: confirmOK, DryRun: true}); !errors.Is(err, errDestroyAdmin) {
		t.Fatalf("plan as deploy: %v", err)
	}
	for _, confirm := range []string{"", "other", "TEST", confirmOK + " "} {
		req.Confirm = confirm
		if _, _, err := admit(t, f.ctrl, ctx, Actor{ID: "admin", Admin: true}, req); !errors.Is(err, errDestroyConfirm) {
			t.Fatalf("admit with %q: %v", confirm, err)
		}
		if _, err := f.ctrl.Plan(ctx, Actor{ID: "admin", Admin: true}, Request{Kind: "destroy", Confirm: confirm, DryRun: true}); !errors.Is(err, errDestroyConfirm) {
			t.Fatalf("plan with %q: %v", confirm, err)
		}
	}
	if got := f.allServices(); len(got) != 3 {
		t.Fatalf("services %v", got)
	}
}

// TestDestroyNeedsProjectAndEnvironmentToConfirm: the confirmation is project/environment; the environment alone is refused
// and nothing is removed.
func TestDestroyNeedsProjectAndEnvironmentToConfirm(t *testing.T) {
	f := newDestroyFixture(t, true)
	ctx := context.Background()
	before := f.allServices()
	containers, images := f.containerIDs(), f.imageIDs()
	req := Request{RequestID: "d", Kind: "destroy", Confirm: ingressEnv, AllowRecreate: true}
	if _, _, err := admit(t, f.ctrl, ctx, Actor{ID: "admin", Admin: true}, req); !errors.Is(err, errDestroyConfirm) {
		t.Fatalf("admit with environment alone: %v", err)
	}
	if _, err := f.ctrl.Plan(ctx, Actor{ID: "admin", Admin: true}, Request{Kind: "destroy", Confirm: ingressEnv, AllowRecreate: true, DryRun: true}); !errors.Is(err, errDestroyConfirm) {
		t.Fatalf("plan with environment alone: %v", err)
	}
	if !reflect.DeepEqual(f.allServices(), before) || !reflect.DeepEqual(f.containerIDs(), containers) || !reflect.DeepEqual(f.imageIDs(), images) {
		t.Fatal("nothing must be removed when confirmation names only the environment")
	}
}

func TestDestroyTwiceIsHarmless(t *testing.T) {
	f := newDestroyFixture(t, true)
	if op := f.destroy("first", nil); op.State != "succeeded" {
		t.Fatalf("first %s: %s", op.State, op.Err)
	}
	before := f.cf.Snapshot()
	containers, images := f.containerIDs(), f.imageIDs()
	f.calls.reset()
	op := f.destroy("second", nil)
	if op.State != "succeeded" {
		t.Fatalf("second %s: %s", op.State, op.Err)
	}
	if !reflect.DeepEqual(f.cf.Snapshot(), before) || !reflect.DeepEqual(f.containerIDs(), containers) || !reflect.DeepEqual(f.imageIDs(), images) || f.calls.count("service delete") != 0 {
		t.Fatalf("a second destroy changed something: %v", f.calls.all())
	}
}

func TestDestroyLeavesTheHelperAndOneOffsOfOtherEnvironments(t *testing.T) {
	c := Controller{Project: "shop", Environment: "test"}
	for _, kind := range []string{"berthy.helper", "berthy.oneoff"} {
		for _, labels := range []map[string]string{
			{"berthy.project": "blog", "berthy.environment": "test", kind: "archive"},
			{"berthy.project": "shop", "berthy.environment": "prod", kind: "archive"},
		} {
			if name, _ := c.destroyable(engine.InventoryContainer{ID: "foreign", Names: []string{"/foreign"}, Labels: labels}); name != "" {
				t.Errorf("foreign %s accepted for removal: %v", kind, labels)
			}
		}
	}
}

func TestDestroyDeletesACleanTunnel(t *testing.T) {
	f := newDestroyFixture(t, false)
	p := f.plan(func(r *Request) { r.Tunnel = true })
	if p.Tunnel == nil || !p.Tunnel.Delete || len(p.Tunnel.Reasons) != 0 {
		t.Fatalf("plan %+v", p.Tunnel)
	}
	op := f.destroy("tunnel", func(r *Request) { r.Tunnel = true })
	if op.State != "succeeded" {
		t.Fatalf("destroy %s: %s", op.State, op.Err)
	}
	if f.tunnel() != nil {
		t.Fatal("the clean tunnel is still there")
	}
	if !hasStep(steps(t, f.store, op.ID), "deleted tunnel "+ingressTunnel) {
		t.Fatalf("steps %v", steps(t, f.store, op.ID))
	}
	if secrets, _ := f.store.Secrets(context.Background()); len(secrets) == 0 {
		t.Fatal("the API token secret must stay")
	}
}

func TestDestroyKeepsATunnelThatIsNotClean(t *testing.T) {
	cases := []struct {
		name   string
		seed   func(f *destroyFixture)
		reason string
	}{
		{
			name: "a foreign rule",
			seed: func(f *destroyFixture) {
				cfg := f.tunnelConfig(f.tunnelID)
				cfg.Ingress = append([]cloudflare.Rule{rule("foreign.example.com", "http://foreign:80")}, cfg.Ingress...)
				raw, err := json.Marshal(cfg)
				if err != nil {
					f.t.Fatal(err)
				}
				f.cf.SetConfig(f.tunnelID, raw)
			},
			reason: "a rule for foreign.example.com remains",
		},
		{
			name: "a record in another zone",
			seed: func(f *destroyFixture) {
				zone := f.cf.AddZone("example.net")
				f.cf.AddRecord(zone, cloudflare.DNSRecord{Type: "CNAME", Name: "www.example.net", Content: cloudflare.TunnelTarget(f.tunnelID), Proxied: true})
			},
			reason: "a DNS record for www.example.net points at the tunnel",
		},
		{
			name: "an adopted record",
			seed: func(f *destroyFixture) {
				f.cf.AddRecord(f.zone, cloudflare.DNSRecord{Type: "CNAME", Name: "kept.example.com", Content: cloudflare.TunnelTarget(f.tunnelID), Proxied: true})
			},
			reason: "a DNS record for kept.example.com points at the tunnel",
		},
		{
			name: "active connections",
			seed: func(f *destroyFixture) {
				f.cf.SetTunnelStatus(f.tunnelID, "healthy", 2)
			},
			reason: "the tunnel still has 2 active connections",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newDestroyFixture(t, false)
			tc.seed(f)
			p := f.plan(func(r *Request) { r.Tunnel = true })
			if tc.name != "active connections" {
				if p.Tunnel == nil || p.Tunnel.Delete || !reflect.DeepEqual(p.Tunnel.Reasons, []string{tc.reason}) {
					t.Fatalf("plan %+v", p.Tunnel)
				}
			}
			op := f.destroy("tunnel", func(r *Request) { r.Tunnel = true })
			if op.State != "succeeded" {
				t.Fatalf("destroy %s: %s", op.State, op.Err)
			}
			if f.tunnel() == nil {
				t.Fatal("the tunnel was deleted")
			}
			if !hasStep(steps(t, f.store, op.ID), "kept tunnel "+ingressTunnel+": "+tc.reason) {
				t.Fatalf("steps %v", steps(t, f.store, op.ID))
			}
			// Everything else is gone all the same.
			if got := f.allServices(); len(got) != 0 {
				t.Fatalf("services %v", got)
			}
		})
	}
}

func TestDestroyTunnelWithoutManagedIngressReportsNoManagedTunnel(t *testing.T) {
	f := newIngressFixture(t)
	f.ctrl.Project = "demo"
	f.ctrl.Engine = &destroyDocker{volumeDocker: &volumeDocker{loggedEngine: f.docker, volumes: map[string]string{}}, bound: f.bound}
	confirm := f.ctrl.id().String()
	p, err := f.ctrl.Plan(context.Background(), Actor{ID: "admin", Admin: true}, Request{Kind: "destroy", Confirm: confirm, Tunnel: true, DryRun: true})
	if err != nil || p.Destroy == nil || p.Destroy.Tunnel == nil || p.Destroy.Tunnel.Delete || !reflect.DeepEqual(p.Destroy.Tunnel.Reasons, []string{"no managed tunnel"}) {
		t.Fatalf("plan %+v %v", p.Destroy, err)
	}
	op := f.run(Request{RequestID: "d", Kind: "destroy", Confirm: confirm, Tunnel: true})
	if op.State != "succeeded" || !hasStep(steps(t, f.store, op.ID), "no managed tunnel") {
		t.Fatalf("destroy %s: %s %v", op.State, op.Err, steps(t, f.store, op.ID))
	}
}

func TestRecoveryFinishesAnInterruptedDestroy(t *testing.T) {
	f := newDestroyFixture(t, true)
	ctx := context.Background()
	web, worker := idOf2(t, f, "demo-test-api-web"), idOf2(t, f, "demo-test-api-worker")
	live, _ := f.docker.InspectService(ctx, "demo-test-api-web")
	op, _, err := f.store.Admit(ctx, state.Admission{RequestID: "crashed", ActorID: "admin", Kind: "destroy", PublicRequest: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	for i, b := range []struct{ role, name, id string }{{"web", "demo-test-api-web", web}, {"worker", "demo-test-api-worker", worker}} {
		if err = f.store.RecordIntent(ctx, op.ID, state.Intent{Ordinal: i + 1, Kind: removeServiceKind, Role: b.role, Name: b.name, ServiceID: b.id, Before: live.Spec, Desired: json.RawMessage(`{}`)}); err != nil {
			t.Fatal(err)
		}
	}
	if err = f.docker.DeleteService(ctx, web); err != nil {
		t.Fatal(err)
	}
	if err = f.ctrl.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := f.store.Operation(ctx, op.ID)
	if err != nil || got.State != "interrupted" || got.Err != "destroy interrupted; run destroy again" {
		t.Fatalf("operation %+v %v", got, err)
	}
	if _, err = f.docker.InspectService(ctx, "demo-test-api-worker"); !serviceMissing(err) {
		t.Fatalf("the half-done service deletion was not finished: %v", err)
	}
	if again := f.destroy("again", nil); again.State != "succeeded" {
		t.Fatalf("destroy again %s: %s", again.State, again.Err)
	}
}

func idOf2(t *testing.T, f *destroyFixture, name string) string {
	t.Helper()
	live, err := f.docker.InspectService(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	return live.ID
}

func TestALaterDestroyDeletesATunnelThatWasKept(t *testing.T) {
	f := newDestroyFixture(t, false)
	cfg := f.tunnelConfig(f.tunnelID)
	cfg.Ingress = append([]cloudflare.Rule{rule("foreign.example.com", "http://foreign:80")}, cfg.Ingress...)
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	f.cf.SetConfig(f.tunnelID, raw)
	first := f.destroy("first", func(r *Request) { r.Tunnel = true })
	if first.State != "succeeded" || f.tunnel() == nil {
		t.Fatalf("first %s: %s", first.State, first.Err)
	}
	if got := describeRules(f.tunnelConfig(f.tunnelID)); !reflect.DeepEqual(got, []string{"foreign.example.com||http://foreign:80", "||http_status:404"}) {
		t.Fatalf("the foreign rule must stay: %v", got)
	}
	// The plan of a later destroy knows the tunnel although managed ingress is gone.
	if p := f.plan(func(r *Request) { r.Tunnel = true }); p.Tunnel == nil || p.Tunnel.Delete || p.Tunnel.ID != f.tunnelID || len(p.Tunnel.Reasons) != 1 {
		t.Fatalf("plan %+v", p.Tunnel)
	}
	// Someone removes the foreign rule.
	raw, _ = json.Marshal(rulesOf(rule("", "http_status:404")))
	f.cf.SetConfig(f.tunnelID, raw)
	second := f.destroy("second", func(r *Request) { r.Tunnel = true })
	if second.State != "succeeded" || f.tunnel() != nil {
		t.Fatalf("second %s: %s", second.State, second.Err)
	}
	if third := f.destroy("third", func(r *Request) { r.Tunnel = true }); !hasStep(steps(t, f.store, third.ID), "no managed tunnel") {
		t.Fatalf("a deleted tunnel is not remembered: %v", steps(t, f.store, third.ID))
	}
}

func TestDestroyWithoutTheFlagLeavesTheTunnelAlone(t *testing.T) {
	f := newDestroyFixture(t, false)
	if op := f.destroy("plain", nil); op.State != "succeeded" {
		t.Fatalf("destroy %s: %s", op.State, op.Err)
	}
	if f.tunnel() == nil {
		t.Fatal("the tunnel was deleted without --tunnel")
	}
}

// A deleted service's task containers go a moment after the service. The images they use must not be skipped for that.
func TestDestroyWaitsForTheTaskContainersOfDeletedServicesBeforeRemovingImages(t *testing.T) {
	f := newDestroyFixture(t, false)
	removeVolumeWait = 5 * time.Second
	f.docker.imu.Lock()
	f.docker.containers = append(f.docker.containers, engine.InventoryContainer{ID: "task-api", ImageID: "id-acquired", State: "running", Labels: map[string]string{"com.docker.swarm.service.name": "demo-test-api-web"}})
	f.docker.laggards = map[string][]string{"demo-test-api-web": {"task-api"}}
	f.docker.imu.Unlock()
	op := f.destroy("lag", nil)
	if op.State != "succeeded" {
		t.Fatalf("destroy %s: %s", op.State, op.Err)
	}
	if slices.Contains(f.imageIDs(), "id-acquired") {
		t.Fatalf("the image of a deleted service's task was kept: %v", f.imageIDs())
	}
}

func TestDestroyKeepsAnImageAnotherEnvironmentRetains(t *testing.T) {
	f := newDestroyFixture(t, false)
	f.ctrl.Foreign = func(context.Context) (Foreign, error) {
		return Foreign{Images: map[string]bool{acquiredRef: true}}, nil
	}
	op := f.destroy("destroy-retained", nil)
	if op.State != "succeeded" {
		t.Fatalf("destroy: %+v", op)
	}
	if !slices.Contains(f.imageIDs(), "id-acquired") {
		t.Fatalf("retained image removed: %v", f.imageIDs())
	}
}

func TestDestroyReadsForeignImagesBeforeRemovingAnything(t *testing.T) {
	for _, afterPlan := range []bool{false, true} {
		t.Run(fmt.Sprint(afterPlan), func(t *testing.T) {
			base := newIngressFixture(t)
			base.ctrl.Project = "demo"
			d := &destroyDocker{volumeDocker: &volumeDocker{loggedEngine: base.docker, volumes: map[string]string{}}, bound: base.bound}
			d.containers = []engine.InventoryContainer{{ID: "helper", Names: []string{"helper"}, ImageID: "image", State: "exited", Labels: map[string]string{"berthy.project": "demo", "berthy.environment": ingressEnv, "berthy.helper": "probe"}}}
			base.ctrl.Engine = d
			base.calls.reset()
			reads := 0
			base.ctrl.Foreign = func(context.Context) (Foreign, error) {
				reads++
				if reads > 1 {
					return Foreign{}, errors.New("peer unavailable")
				}
				return Foreign{Images: map[string]bool{}}, nil
			}
			req := Request{RequestID: "destroy", Kind: "destroy", Confirm: base.ctrl.id().String()}
			if afterPlan {
				if _, err := base.ctrl.Plan(context.Background(), Actor{ID: "admin", Admin: true}, req); err != nil {
					t.Fatal(err)
				}
			}
			op := base.run(req)
			if afterPlan {
				if op.State != "failed" || op.Err != "release history unavailable" || base.calls.count("container remove") != 0 {
					t.Fatalf("outcome %+v, calls %v", op, base.calls.all())
				}
			} else if op.State != "succeeded" || reads != 1 || base.calls.count("container remove") != 1 {
				t.Fatalf("outcome %+v reads %d calls %v", op, reads, base.calls.all())
			}
		})
	}
}

func TestKeptTunnelCannotBeTakenOrDeletedByPeer(t *testing.T) {
	f := newDestroyFixture(t, false)
	if op := f.destroy("keep-tunnel", nil); op.State != "succeeded" {
		t.Fatalf("destroy: %+v", op)
	}
	kept, err := f.store.KeptTunnel(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	peer := *f.ctrl
	peer.Foreign = func(context.Context) (Foreign, error) { return Foreign{Ingress: []state.IngressState{kept}}, nil }
	if _, err := peer.ingressTargetFor(context.Background(), ingressManifest(t, "")); err == nil || err.Error() != string(errTunnelForeign) {
		t.Fatalf("peer ingress: %v", err)
	}
	f.ctrl.Foreign = func(context.Context) (Foreign, error) { return Foreign{Ingress: []state.IngressState{kept}}, nil }
	plan := f.plan(func(r *Request) { r.Tunnel = true })
	if plan.Tunnel == nil || plan.Tunnel.Delete || !slices.Contains(plan.Tunnel.Reasons, string(errTunnelForeign)) {
		t.Fatalf("destroy plan: %+v", plan.Tunnel)
	}
	op := f.destroy("refuse-peer-tunnel", func(r *Request) { r.Tunnel = true })
	if f.tunnel() == nil {
		t.Fatalf("destroy deleted peer tunnel: %+v", op)
	}
}
