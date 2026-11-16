package deploy

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"sort"
	"time"

	"berthy/internal/engine"
	"berthy/internal/manifest"
	"berthy/internal/state"
)

// RemovePlan is what removing an app deletes and what it leaves in place. Building it changes nothing.
type RemovePlan struct {
	App         string          `json:"app"`
	Kind        string          `json:"kind,omitempty"`         // image | compose; empty when the app is already removed
	Services    []string        `json:"services,omitempty"`     // Swarm services to delete, or the Compose services that run
	Project     string          `json:"project,omitempty"`      // Compose project to bring down
	Volumes     []string        `json:"volumes,omitempty"`      // volumes to delete; only with --volumes
	KeptVolumes []string        `json:"kept_volumes,omitempty"` // named volumes left in place
	Ingress     []IngressChange `json:"ingress,omitempty"`      // route and record removals, and releases of adopted ones
	Blockers    []string        `json:"blockers,omitempty"`
}

const (
	removeServiceKind  = "remove_service"
	composeDownKind    = "compose_down"
	composeDownOrdinal = 100
)

const (
	errRemoveAdmin       Refusal = "removing an app requires administrator access"
	errRemoveApp         Refusal = "app name required"
	errRemoveConnector   Refusal = "use ingress apply without an ingress block to remove managed ingress"
	errRemoveNotDeployed Refusal = "app is not deployed"
	errRemoveInterrupted Refusal = "remove interrupted; run remove again"
	errRemovePreflight   Refusal = "remove preflight failed"
	errRemoveOwnership   Refusal = "bound service ownership changed"
	errRemoveService     Refusal = "service removal failed"
	errRemoveVolume      Refusal = "volume removal failed"
	errRemoveCompose     Refusal = "compose down failed"
	errRemoveState       Refusal = "app state removal failed"
	errRemoveJournal     Refusal = "remove journal failed"
	errDockerUnavailable Refusal = "Docker unavailable"
)

// Waits of a removal. They are variables so tests can shorten them.
var (
	// removeServiceWait is how long a removal waits for a deleted service to disappear.
	removeServiceWait = time.Minute
	// removeVolumeWait is how long a removal waits for the containers of a deleted service to let go of a volume before it
	// reports the volume as in use.
	removeVolumeWait = 20 * time.Second
	// composeDownLimit bounds one `compose down`.
	composeDownLimit = 3 * time.Minute
)

// volumeEngine is the part of the Docker API a removal uses to delete volumes. The Engine of a Controller may implement it.
type volumeEngine interface {
	VolumeExists(context.Context, string) (bool, error)
	RemoveVolume(context.Context, string) error
}

// projectVolumeEngine lists the volumes Compose created for a project.
type projectVolumeEngine interface {
	ProjectVolumes(context.Context, string) ([]string, error)
}

// removal is everything a removal needs to know, read once: the plan, and what the run acts on.
type removal struct {
	plan   RemovePlan
	gone   bool // the app has no applied release and was deployed once, so it is already removed
	app    manifest.App
	m      manifest.Manifest
	live   []liveBinding // image apps: the bound services that still exist
	routes []state.IngressRoute
}

type liveBinding struct {
	b    state.Binding
	live engine.ServiceSnapshot
}

// removeCheck is what a removal decides from state alone: the caller is an administrator, the app is not the connector,
// and the app is deployed, or was and is already removed.
func (c *Controller) removeCheck(ctx context.Context, actor Actor, req Request) (state.Release, bool, error) {
	switch {
	case !actor.Admin:
		return state.Release{}, false, errRemoveAdmin
	case req.App == "":
		return state.Release{}, false, errRemoveApp
	case req.App == manifest.ConnectorAppName:
		return state.Release{}, false, errRemoveConnector
	case c.Store == nil:
		return state.Release{}, false, errors.New("state unavailable")
	}
	rel, err := c.Store.Applied(ctx, req.App)
	if err == nil {
		return rel, false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return state.Release{}, false, err
	}
	history, err := c.Store.History(ctx, req.App)
	if err != nil {
		return state.Release{}, false, err
	}
	if len(history) == 0 {
		return state.Release{}, false, errRemoveNotDeployed
	}
	return state.Release{}, true, nil
}

// removalFor reads what removing req.App would do. Anything that refuses the removal is a blocker of its plan. It changes
// nothing.
func (c *Controller) removalFor(ctx context.Context, actor Actor, req Request) (*removal, error) {
	rel, gone, err := c.removeCheck(ctx, actor, req)
	if err != nil {
		return nil, err
	}
	r := &removal{plan: RemovePlan{App: req.App}, gone: gone}
	if gone {
		return r, nil
	}
	if json.Unmarshal(rel.Manifest, &r.m) != nil {
		return nil, errRemovePreflight
	}
	app, ok := r.m.Apps[req.App]
	if !ok {
		return nil, errRemovePreflight
	}
	r.app = app
	switch app.Kind {
	case "image":
		err = c.removalImage(ctx, req, r)
	case "compose":
		err = c.removalCompose(ctx, actor, req, r)
	default:
		err = errRemovePreflight
	}
	if err != nil {
		return nil, err
	}
	r.plan.Blockers = slices.Compact(r.plan.Blockers)
	return r, nil
}

// removalImage plans the removal of an image app: the bound services that still exist, the volumes its roles name, and its
// routes and records.
func (c *Controller) removalImage(ctx context.Context, req Request, r *removal) error {
	r.plan.Kind = "image"
	if c.Engine == nil {
		return errDockerUnavailable
	}
	bindings, err := c.Store.Bindings(ctx, req.App)
	if err != nil {
		return err
	}
	for _, b := range bindings {
		live, err := c.Engine.InspectService(ctx, b.Name)
		if serviceMissing(err) {
			// The name is free. The service is gone only if its ID is gone too; a service that was renamed is not Berthy's to delete.
			gone, e := c.boundServiceGone(ctx, b, err)
			if e != nil {
				return errors.New("bound service unavailable")
			}
			if !gone {
				r.plan.Blockers = append(r.plan.Blockers, string(errRemoveOwnership))
			}
			continue
		}
		if err != nil {
			return errors.New("bound service unavailable")
		}
		if live.ID != b.ServiceID || !ownedBinding(live.Spec, c.Environment, b) {
			r.plan.Blockers = append(r.plan.Blockers, string(errRemoveOwnership))
			continue
		}
		r.live = append(r.live, liveBinding{b: b, live: live})
		r.plan.Services = append(r.plan.Services, b.Name)
	}
	volumes, err := c.existingVolumes(ctx, declaredVolumes(r.app))
	if err != nil {
		return err
	}
	if req.Volumes {
		r.plan.Volumes = volumes
	} else {
		r.plan.KeptVolumes = volumes
	}
	if r.routes, err = c.Store.IngressRoutes(ctx, req.App); err != nil {
		return err
	}
	if len(r.routes) == 0 {
		return nil
	}
	sy, err := c.publishSync(ctx, manifest.Manifest{}, "", false)
	if err == nil {
		var ip IngressPlan
		if ip, _, _, err = sy.plan(ctx, nil, ownedBy(req.App), false); err == nil {
			r.plan.Ingress = ip.Changes
			r.plan.Blockers = append(r.plan.Blockers, ip.Conflicts...)
		}
	}
	var refusal Refusal
	if errors.As(err, &refusal) {
		// A route that Cloudflare cannot be asked about stops the removal before anything changes.
		r.plan.Blockers = append(r.plan.Blockers, string(refusal))
		return nil
	}
	return err
}

// ownedBinding applies the deploy path's rule for a bound service: the name and ID already match the binding, and any
// berthy.environment, berthy.app or berthy.role label the service carries, at the service or at its tasks, must name this
// environment, app and role. A label that is absent is fine, since a service adopted with --adopt has none until it is
// deployed to.
func ownedBinding(spec json.RawMessage, environment string, b state.Binding) bool {
	var v struct {
		Labels       map[string]string
		TaskTemplate struct {
			ContainerSpec struct{ Labels map[string]string }
		}
	}
	if json.Unmarshal(spec, &v) != nil {
		return false
	}
	want := map[string]string{"berthy.environment": environment, "berthy.app": b.App, "berthy.role": b.Role}
	for _, labels := range []map[string]string{v.Labels, v.TaskTemplate.ContainerSpec.Labels} {
		for key, value := range want {
			if existing, ok := labels[key]; ok && existing != value {
				return false
			}
		}
	}
	return true
}

// removalCompose plans the removal of a Compose app: the project to bring down, the services that run, and its volumes.
func (c *Controller) removalCompose(ctx context.Context, actor Actor, req Request, r *removal) error {
	r.plan.Kind = "compose"
	r.plan.Project = r.m.ComposeProject(req.App)
	if err := c.composeReady(); err != nil {
		return err
	}
	if r.app.Stateful && !(req.AllowRecreate && actor.Admin) {
		r.plan.Blockers = append(r.plan.Blockers, fmt.Sprintf("removing stateful app %s needs --allow-recreate", req.App))
	}
	containers, err := c.Engine.(composeEngine).ComposeContainers(ctx, r.plan.Project)
	if err != nil {
		return errComposePlan
	}
	seen := map[string]bool{}
	for _, ctr := range containers {
		if !ctr.OneOff && ctr.Service != "" && !seen[ctr.Service] {
			seen[ctr.Service] = true
			r.plan.Services = append(r.plan.Services, ctr.Service)
		}
	}
	sort.Strings(r.plan.Services)
	var volumes []string
	if pe, ok := c.Engine.(projectVolumeEngine); ok {
		if volumes, err = pe.ProjectVolumes(ctx, r.plan.Project); err != nil {
			return errComposePlan
		}
	}
	if req.Volumes {
		r.plan.Volumes = volumes
	} else {
		r.plan.KeptVolumes = volumes
	}
	return nil
}

// declaredVolumes lists the named volumes an image app's roles mount, once each, in name order. Only these belong to the
// app; no other volume is ever listed.
func declaredVolumes(app manifest.App) []string {
	seen := map[string]bool{}
	for _, role := range app.Roles {
		for _, v := range role.Volumes {
			if m, err := manifest.ParseMount(v); err == nil {
				seen[m.Source] = true
			}
		}
	}
	return sortedKeys(seen)
}

// existingVolumes keeps the named volumes Docker holds. An engine that cannot say keeps them all.
func (c *Controller) existingVolumes(ctx context.Context, names []string) ([]string, error) {
	ve, ok := c.Engine.(volumeEngine)
	if !ok {
		return names, nil
	}
	var out []string
	for _, n := range names {
		found, err := ve.VolumeExists(ctx, n)
		if err != nil {
			return nil, errors.New("volume inventory unavailable")
		}
		if found {
			out = append(out, n)
		}
	}
	return out, nil
}

// planRemove describes removing an app without changing anything.
func (c *Controller) planRemove(ctx context.Context, actor Actor, req Request) (Plan, error) {
	r, err := c.removalFor(ctx, actor, req)
	if err != nil {
		return Plan{}, err
	}
	return Plan{App: req.App, Roles: []string{}, Remove: &r.plan}, nil
}

// removeFailure is the outcome of a removal that stopped: a refusal keeps its text, anything else gets fallback.
func removeFailure(err error, fallback Refusal) state.Outcome {
	var r Refusal
	if errors.As(err, &r) {
		return state.Outcome{State: "failed", Err: string(r)}
	}
	return state.Outcome{State: "failed", Err: string(fallback)}
}

// runRemove deletes what Berthy created for an app. Its routes and records go first, so nothing is sent to services that
// are about to disappear; then its services, or its Compose project; then its volumes when asked; and last the state that
// says the app exists. Every step can be repeated, so an interrupted removal is finished by running it again.
func (c *Controller) runRemove(ctx context.Context, op state.Operation, actor Actor, req Request) state.Outcome {
	step := func(text string) {
		if data, err := json.Marshal(map[string]string{"step": text}); err == nil {
			_ = c.Store.AppendEvent(ctx, op.ID, state.Event{Kind: "step", Data: data})
		}
	}
	r, err := c.removalFor(ctx, actor, req)
	if err != nil {
		return removeFailure(err, errRemovePreflight)
	}
	for _, blocker := range r.plan.Blockers {
		_ = c.Store.AppendEvent(ctx, op.ID, state.Event{Kind: "blocked", Data: reasonEvent(blocker)})
	}
	if len(r.plan.Blockers) > 0 {
		return removeFailure(Refusal(r.plan.Blockers[0]), errRemovePreflight)
	}
	if r.gone {
		step("app " + req.App + " is already removed")
		return state.Outcome{State: "succeeded"}
	}
	if len(r.routes) > 0 {
		if err = c.removeRoutes(ctx, op.ID, req.App, step); err != nil {
			return state.Outcome{State: "failed", Err: ingressFailure(err, errIngressPublish)}
		}
	}
	switch r.plan.Kind {
	case "image":
		if err = c.removeServices(ctx, op.ID, r, step); err != nil {
			return removeFailure(err, errRemoveService)
		}
		if req.Volumes {
			for _, name := range r.plan.Volumes {
				if err = c.removeVolume(ctx, name, step); err != nil {
					return removeFailure(err, errRemoveVolume)
				}
			}
		}
	case "compose":
		if err = c.removeProject(ctx, op.ID, r.plan.Project, req.Volumes); err != nil {
			return removeFailure(err, errRemoveCompose)
		}
		step("brought down Compose project " + r.plan.Project)
		for _, name := range r.plan.Volumes {
			step("removed volume " + name)
		}
	}
	for _, name := range r.plan.KeptVolumes {
		step("kept volume " + name)
	}
	if err = c.Store.ForgetIngressRoutes(ctx, op.ID, req.App); err != nil {
		return state.Outcome{State: "failed", Err: string(errRemoveState)}
	}
	if err = c.Store.ForgetApp(ctx, op.ID, req.App); err != nil {
		return state.Outcome{State: "failed", Err: string(errRemoveState)}
	}
	return state.Outcome{State: "succeeded"}
}

// removeRoutes removes the routes and DNS records Berthy created for the app, and releases the ones it adopted.
func (c *Controller) removeRoutes(ctx context.Context, opID, app string, step func(string)) error {
	sy, err := c.publishSync(ctx, manifest.Manifest{}, opID, false)
	if err != nil {
		return err
	}
	plan, cfg, steps, err := sy.plan(ctx, nil, ownedBy(app), false)
	if err != nil {
		return err
	}
	if len(plan.Conflicts) > 0 {
		c.recordConflicts(ctx, opID, plan.Conflicts)
		return errIngressConflict
	}
	if err = sy.apply(ctx, plan, cfg, steps); err != nil {
		return err
	}
	for _, ch := range plan.Changes {
		text := "route " + ch.Hostname + ": " + ch.Route
		if ch.DNS != "" {
			text += ", DNS " + ch.DNS
		}
		step(text)
	}
	return nil
}

// removeServices deletes the bound services of an image app. Each deletion is journaled first, so a recovery can finish
// it, and each is confirmed gone before the next begins.
func (c *Controller) removeServices(ctx context.Context, opID string, r *removal, step func(string)) error {
	for n, lb := range r.live {
		i := state.Intent{Ordinal: n + 1, Kind: removeServiceKind, Role: lb.b.Role, Name: lb.b.Name, ServiceID: lb.b.ServiceID, SeenVersion: lb.live.Version, Before: lb.live.Spec, Desired: json.RawMessage(`{}`)}
		if err := c.Store.RecordIntent(ctx, opID, i); err != nil {
			return errRemoveJournal
		}
		if err := c.deleteBoundService(ctx, i); err != nil {
			return errRemoveService
		}
		if err := c.Store.MarkStep(ctx, opID, i.Ordinal, "finished", json.RawMessage(`{}`)); err != nil {
			return errRemoveJournal
		}
		step("deleted service " + lb.b.Name)
	}
	return nil
}

// deleteBoundService deletes the service a journaled intent names, if it is still the one that was bound, and waits until
// Docker no longer knows it. A service that is already gone counts as deleted.
func (c *Controller) deleteBoundService(ctx context.Context, i state.Intent) error {
	live, err := c.Engine.InspectService(ctx, i.Name)
	if serviceMissing(err) {
		return nil
	}
	if err != nil || live.ID != i.ServiceID {
		return errors.New("bound service changed")
	}
	if err = c.Engine.DeleteService(ctx, live.ID); err != nil && !serviceMissing(err) {
		return err
	}
	deadline := time.Now().Add(removeServiceWait)
	for {
		_, err = c.Engine.InspectService(ctx, i.Name)
		if serviceMissing(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if !time.Now().Before(deadline) {
			return errors.New("service still present")
		}
		if sleepContext(ctx, 100*time.Millisecond) != nil {
			return ctx.Err()
		}
	}
}

// sleepContext waits for d or until ctx ends.
func sleepContext(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// removeVolume deletes one named volume. Docker refuses a volume a container holds, and the containers of a service that
// was just deleted take a moment to go, so the removal is tried again for a while. A volume that stays in use is kept and
// reported; it is not an error.
func (c *Controller) removeVolume(ctx context.Context, name string, step func(string)) error {
	ve, ok := c.Engine.(volumeEngine)
	if !ok {
		return errRemoveVolume
	}
	deadline := time.Now().Add(removeVolumeWait)
	for {
		err := ve.RemoveVolume(ctx, name)
		var status *engine.StatusError
		switch {
		case err == nil:
			step("removed volume " + name)
			return nil
		case errors.As(err, &status) && status.Code == 404:
			return nil
		case errors.As(err, &status) && status.Code == 409:
			if !time.Now().Before(deadline) {
				step("kept volume " + name + ": in use by another container")
				return nil
			}
			if sleepContext(ctx, 100*time.Millisecond) != nil {
				return ctx.Err()
			}
		default:
			return errRemoveVolume
		}
	}
}

// removeProject brings a Compose project down and deletes its working directory. It uses the stored project files; when
// they are gone it names the project alone, which Compose resolves by label. The project's volumes are removed only when
// volumes is set. The run is journaled first, so a recovery can repeat it.
func (c *Controller) removeProject(ctx context.Context, opID, project string, volumes bool) error {
	desired, err := json.Marshal(struct {
		Project string `json:"project"`
		Volumes bool   `json:"volumes"`
	}{project, volumes})
	if err != nil {
		return errRemoveJournal
	}
	if err = c.Store.RecordIntent(ctx, opID, state.Intent{Ordinal: composeDownOrdinal, Kind: composeDownKind, Name: project, Desired: desired}); err != nil {
		return errRemoveJournal
	}
	if err = c.Store.MarkStep(ctx, opID, composeDownOrdinal, "issued", json.RawMessage(`{}`)); err != nil {
		return errRemoveJournal
	}
	if err = c.composeDown(ctx, project, volumes); err != nil {
		_ = c.Store.MarkStep(ctx, opID, composeDownOrdinal, "finished", json.RawMessage(`{"ok":false}`))
		return errRemoveCompose
	}
	return c.Store.MarkStep(ctx, opID, composeDownOrdinal, "finished", json.RawMessage(`{"ok":true}`))
}

// composeDown runs `compose down --remove-orphans`, with --volumes when asked, confirms no container of the project is
// left, and deletes the project's working directory.
func (c *Controller) composeDown(ctx context.Context, project string, volumes bool) error {
	if err := c.composeReady(); err != nil {
		return err
	}
	args := []string{"down", "--remove-orphans"}
	if volumes {
		args = append(args, "--volumes")
	}
	run := engine.ComposeRun{Project: project, Dir: c.composeDir(project), Args: args}
	if _, err := os.Stat(run.Dir); err != nil {
		// The project's files are gone. Compose finds a project's containers, networks and volumes by label, so the name is
		// enough; the directory is only a private working directory for the command.
		if !os.IsNotExist(err) {
			return err
		}
		if run.Dir, err = c.newComposeDir(".down-"); err != nil {
			return err
		}
		defer os.RemoveAll(run.Dir)
		run.NoFiles = true
	}
	dctx, cancel := context.WithTimeout(ctx, composeDownLimit)
	defer cancel()
	if _, _, err := c.Compose.Run(dctx, run); err != nil {
		return err
	}
	containers, err := c.Engine.(composeEngine).ComposeContainers(ctx, project)
	if err != nil {
		return err
	}
	if len(containers) > 0 {
		return errors.New("project containers remain")
	}
	return os.RemoveAll(c.composeDir(project))
}

// recoverRemove finishes what an interrupted removal journaled: the service deletions and the Compose down. Whatever it
// cannot finish stays for the next removal, which repeats every step. The operation ends interrupted either way, and the
// app stays applied until a removal completes.
func (c *Controller) recoverRemove(ctx context.Context, op state.Operation, intents []state.Intent) error {
	for _, i := range intents {
		if i.Phase == "finished" {
			continue
		}
		switch i.Kind {
		case removeServiceKind:
			if err := c.deleteBoundService(ctx, i); err == nil {
				_ = c.Store.MarkStep(ctx, op.ID, i.Ordinal, "finished", json.RawMessage(`{}`))
			}
		case composeDownKind:
			var d struct {
				Project string `json:"project"`
				Volumes bool   `json:"volumes"`
			}
			if json.Unmarshal(i.Desired, &d) == nil && d.Project != "" && c.composeDown(ctx, d.Project, d.Volumes) == nil {
				_ = c.Store.MarkStep(ctx, op.ID, i.Ordinal, "finished", json.RawMessage(`{"ok":true}`))
			}
		}
	}
	return c.Store.Finish(ctx, op.ID, state.Outcome{State: "interrupted", Err: string(errRemoveInterrupted)})
}
