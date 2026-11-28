package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"berthy/internal/engine"
	"berthy/internal/manifest"
	"berthy/internal/state"
	"github.com/distribution/reference"
)

type deployRun struct {
	opID     string
	rollback bool
	prepared preparedPlan
	work     []roleWork
	ing      *ingressPublish
	budget   time.Duration
	drain    time.Duration
	deadline time.Time
	bindings []state.Binding
	outcome  state.Outcome

	predecessor *manifest.App
}

func (c *Controller) runAdoption(ctx context.Context, op state.Operation, req Request, rollback bool) state.Outcome {
	outcome := state.Outcome{State: "failed", Err: "deployment preflight failed"}
	plan, bindings, e := c.planAdoption(ctx, req)
	if e != nil {
		outcome.Err = "adoption preflight failed"
		return outcome
	}
	raw, e := json.Marshal(req.Manifest)
	if e != nil {
		return outcome
	}
	ing, err := c.publishCheck(ctx, op.ID, req.Manifest, req.App, appRouteWants(req.Manifest, c.Environment, req.App, bindings), req.ReplaceExisting)
	if err != nil {
		outcome.Err = ingressFailure(err, errIngressPublish)
		return outcome
	}
	settings, e := releaseSettings(req, rollback)
	if e != nil {
		return outcome
	}
	outcome = state.Outcome{State: "succeeded", Protected: req.Manifest.Protected, Release: &state.Release{App: req.App, Digest: plan.Image, Commit: req.Commit, Modified: req.Modified, Manifest: raw, Bindings: bindings}, Settings: settings}
	if ing == nil {
		return outcome
	}
	// Adoption starts no service, so its routes follow its bindings at once. The barrier keeps the release if the
	// process stops during the publish, as it does for a deploy.
	barrier, e := json.Marshal(outcome)
	if e != nil || c.cross(ctx, op.ID, state.Intent{Ordinal: 9999, Kind: "barrier", Desired: barrier, Deadline: time.Now()}, reasonSwitched) != nil {
		outcome = state.Outcome{State: "failed", Err: "verification barrier unavailable"}
		return outcome
	}
	if err = c.publishIngress(ctx, op.ID, ing, req.ReplaceExisting); err != nil {
		outcome.State = "failed"
		outcome.Err = ingressFailure(err, errIngressPublish)
	}
	return outcome
}

func (c *Controller) prepareDeployRoles(ctx context.Context, d *deployRun, req Request, p Plan) bool {
	prepared, err := c.prepareExecution(ctx, req, p)
	if err != nil {
		return false
	}
	d.prepared = prepared
	req = prepared.Request
	img, msg, err := c.ensureImage(ctx, prepared)
	if err != nil {
		d.outcome.Err = msg
		return false
	}
	bindings, err := c.Store.Bindings(ctx, req.App)
	if err != nil {
		return false
	}
	byRole := map[string]state.Binding{}
	for _, b := range bindings {
		byRole[b.Role] = b
	}
	app := req.Manifest.Apps[req.App]
	for role := range byRole {
		if _, ok := app.Roles[role]; !ok {
			d.outcome.Err = "removing managed roles unavailable"
			return false
		}
	}
	budget, err := deployStartupBudget(app)
	if err != nil {
		return false
	}
	drain, err := time.ParseDuration(deployDrainTimeout(app))
	if err != nil {
		return false
	}
	d.budget = budget
	d.drain = drain
	d.work = make([]roleWork, 0, len(p.Roles))
	for ordinal, role := range p.Roles {
		if !c.planRoleWork(ctx, d, img, byRole, ordinal, role) {
			return false
		}
	}
	// The check step of a publishing deploy confirms, before anything changes, that its routes and records can be published.
	wants, err := c.routeWants(ctx, req.Manifest, []string{req.App})
	if err != nil {
		d.outcome.Err = ingressFailure(err, errIngressPublish)
		return false
	}
	ing, err := c.publishCheck(ctx, d.opID, req.Manifest, req.App, wants, req.ReplaceExisting)
	if err != nil {
		d.outcome.Err = ingressFailure(err, errIngressPublish)
		return false
	}
	d.ing = ing
	// A cancel that came during the checks stops the deploy here, before it changes anything.
	if err = cancelledBy(ctx); err != nil {
		return false
	}
	return true
}

func (c *Controller) planRoleWork(ctx context.Context, d *deployRun, img engine.Image, byRole map[string]state.Binding, ordinal int, role string) bool {
	req := d.prepared.Request
	app := req.Manifest.Apps[req.App]
	r := app.Roles[role]
	uptime, e := r.Health.UptimeThreshold()
	if e != nil {
		d.outcome.Err = "role preflight failed"
		return false
	}
	replicas := 1
	if r.Replicas != nil {
		replicas = *r.Replicas
	}
	var recoveryAllowance time.Duration
	if len(d.work) > 0 {
		recoveryAllowance = d.work[len(d.work)-1].recoveryAllowance
	}
	recoveryAllowance, e = addStartupAllowance(recoveryAllowance, serviceCleanupAllowance)
	if e != nil {
		return false
	}
	spec, e := c.releaseRoleSpec(d.prepared, img, role, d.opID)
	if e != nil {
		d.outcome.Err = "role preflight failed"
		return false
	}
	name := req.Manifest.ServiceName(c.Environment, req.App, role)
	intent := state.Intent{Ordinal: ordinal, Kind: "create", Role: role, Name: name, Generation: d.opID, Desired: spec}
	if b, ok := byRole[role]; ok {
		name = b.Name
		// The release's spec goes to the bound service under its bound name. An adopted role keeps the name of the
		// service it adopted, which is not the generated one.
		if spec, e = renamed(spec, name); e != nil {
			return false
		}
		live, e := c.Engine.InspectService(ctx, name)
		if e != nil {
			gone, ge := c.boundServiceGone(ctx, b, e)
			if ge != nil || !gone {
				d.outcome.Err = "bound service unavailable"
				return false
			}
			// Recreate the role under its bound name; the release re-binds it to the new service.
			intent = state.Intent{Ordinal: ordinal, Kind: "create", Role: role, Name: name, Generation: d.opID, Desired: spec}
		} else {
			if live.ID != b.ServiceID {
				d.outcome.Err = "bound service unavailable"
				return false
			}
			var ownership struct {
				Labels       map[string]string
				TaskTemplate struct {
					ContainerSpec struct {
						Labels map[string]string
						Image  string
					}
				}
			}
			if json.Unmarshal(live.Spec, &ownership) != nil {
				d.outcome.Err = "invalid bound service"
				return false
			}
			for _, labels := range []map[string]string{ownership.Labels, ownership.TaskTemplate.ContainerSpec.Labels} {
				for key, value := range map[string]string{"berthy.environment": c.Environment, "berthy.project": req.Manifest.Project, "berthy.app": req.App, "berthy.role": role} {
					if existing, ok := labels[key]; ok && existing != value {
						d.outcome.Err = "bound service ownership changed"
						return false
					}
				}
			}
			var previous struct {
				replacementSpec
				TaskTemplate struct {
					ContainerSpec struct {
						Labels map[string]string
						Image  string
					}
				}
			}
			if json.Unmarshal(live.Spec, &previous) != nil || previous.Mode.Replicated == nil || previous.Mode.Replicated.Replicas < 0 {
				d.outcome.Err = "prior service health cannot be verified"
				return false
			}
			if previous.Mode.Replicated.Replicas > 0 {
				if d.predecessor == nil {
					release, err := c.Store.Applied(ctx, req.App)
					var applied manifest.Manifest
					if err != nil || json.Unmarshal(release.Manifest, &applied) != nil {
						return false
					}
					priorApp := applied.Apps[req.App]
					d.predecessor = &priorApp
				}
				startup, err := replacementBudget(*d.predecessor, replicas, previous.Mode.Replicated.Replicas, previous.parallelism())
				if err != nil {
					return false
				}
				recoveryAllowance, err = addStartupAllowance(recoveryAllowance, startup)
				if err != nil {
					return false
				}
			}
			if previous.TaskTemplate.ContainerSpec.Labels["berthy.operation"] == "" {
				ref, e := reference.ParseNormalizedNamed(previous.TaskTemplate.ContainerSpec.Image)
				if e != nil {
					d.outcome.Err = "prior image identity unavailable"
					return false
				}
				if _, ok := ref.(reference.Canonical); !ok {
					d.outcome.Err = "prior image is not pinned"
					return false
				}
				if healthyImageOnce(ctx, c.Engine, live.ID, previous.TaskTemplate.ContainerSpec.Image, previous.Mode.Replicated.Replicas) != nil {
					d.outcome.Err = "prior service health cannot be verified"
					return false
				}
			}
			if _, e = c.operationAuth(ctx, req.App, live.Spec); e != nil {
				d.outcome.Err = "prior image credential unavailable"
				return false
			}
			// The update sends the release's spec, as a create does. Nothing in it comes from the live spec: whatever the
			// live one has beyond the release is replaced.
			intent = state.Intent{Ordinal: ordinal, Kind: "update", Role: role, Name: name, ServiceID: live.ID, SeenVersion: live.Version, Generation: d.opID, Before: live.Spec, Desired: spec}
		}
	} else {
		_, e = c.Engine.InspectService(ctx, name)
		var status *engine.StatusError
		if e == nil || !errors.As(e, &status) || status.Code != 404 {
			d.outcome.Err = "service name is unavailable"
			return false
		}
	}
	intent.Uptime = uptime
	d.work = append(d.work, roleWork{intent: intent, replicas: replicas, recoveryAllowance: recoveryAllowance})
	return true
}

func (c *Controller) runDeployPre(ctx context.Context, d *deployRun) bool {
	if d.rollback {
		return true
	}
	req := d.prepared.Request
	app := req.Manifest.Apps[req.App]
	if app.Migrations != nil {
		source, e := json.Marshal(struct {
			Manifest manifest.Manifest `json:"manifest"`
			Image    string            `json:"image"`
		}{req.Manifest, d.prepared.Image})
		if e != nil {
			return false
		}
		if e = c.Store.RecordIntent(ctx, d.opID, state.Intent{Ordinal: 9000, Kind: "schema_source", Desired: source}); e != nil {
			d.outcome.Err = "migration source journal failed"
			return false
		}
	}
	before, after, err := c.runPre(ctx, d.opID, d.prepared)
	d.outcome.SchemaBefore = before
	d.outcome.SchemaAfter = after
	if err != nil {
		if errors.Is(err, ErrOneOffUncertain) {
			d.outcome.State = "recovery_required"
			d.outcome.Err = "one-off outcome uncertain"
		} else {
			d.outcome.Err = hookFailureText(err, "pre")
		}
		d.outcome.Reason = c.recordOneOffFailure(ctx, d.opID, err)
		return false
	}
	return true
}

func (c *Controller) rollDeployRoles(ctx context.Context, d *deployRun) bool {
	req := d.prepared.Request
	app := req.Manifest.Apps[req.App]
	attempts := app.Deploy.StartAttemptLimit()
	hold := c.monitorHold()
	for n, w := range d.work {
		timing := rolloutTiming{window: d.budget, cutoff: time.Now().Add(d.budget), hold: hold, drain: d.drain}
		w.intent.Deadline = timing.cutoff
		rollCtx, stopRoll := interruptible(ctx)
		id := w.intent.ServiceID
		starts := startBudget{limit: attempts, uptime: w.intent.Uptime}
		var e error
		if w.intent.Kind == "create" {
			id, e = createRole(rollCtx, c.Store, c.Engine, d.opID, w.intent, d.prepared.Auth, w.replicas, starts, timing)
		} else {
			e = updateRole(rollCtx, c.Store, c.Engine, d.opID, w.intent, d.prepared.Auth, w.replicas, starts, timing)
		}
		stopRoll()
		if e != nil {
			touched := n + 1
			var before beforeServiceMutation
			if errors.As(e, &before) {
				touched = n
			}
			c.unwindDeploy(ctx, d, e, d.work[:touched])
			return false
		}
		d.bindings = append(d.bindings, state.Binding{App: req.App, Role: w.intent.Role, Name: w.intent.Name, ServiceID: id})
	}
	d.deadline = time.Now().Add(d.budget)
	if hold > 0 {
		d.deadline = d.deadline.Add(rolloutVerificationGrace)
	}
	return true
}

// unwindDeploy takes the rollout back after a failure or a cancel, with the roles in touched changed.
func (c *Controller) unwindDeploy(ctx context.Context, d *deployRun, cause error, touched []roleWork) {
	req := d.prepared.Request
	app := req.Manifest.Apps[req.App]
	// The evidence is read first: compensation deletes a created service together with its tasks and their logs.
	var gaveUp *errStartAttempts
	errors.As(cause, &gaveUp)
	d.outcome.Reason = addStoppedFirst(c.recordRolloutFailure(ctx, d.opID, d.work, rolloutFallback(cause), gaveUp), app, touched)
	recoveryErr := c.compensateTouchedRoles(d.opID, req.App, touched)
	if recoveryErr != nil {
		for _, message := range compensationMessages(recoveryErr) {
			if c.Log != nil {
				c.Log(message)
			}
			c.storeDiagnostics(context.Background(), d.opID, []Diagnostic{{Kind: "compensation", Error: message, Captured: time.Now().UTC()}}, "")
		}
		d.outcome.State = "recovery_required"
		d.outcome.Err = "rollout recovery required"
		return
	}
	d.outcome.State = "failed"
	for _, done := range touched {
		if done.intent.Kind == "update" {
			d.outcome.State = "rolled_back"
			break
		}
	}
	d.outcome.Err = "rollout failed and was cleaned up"
}

func (c *Controller) compensateTouchedRoles(opID, app string, touched []roleWork) error {
	if len(touched) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), touched[len(touched)-1].recoveryAllowance)
	defer cancel()
	intents, err := c.Store.Intents(ctx, opID)
	if err != nil {
		return errors.New(compensationIntents)
	}
	ordinals := make(map[int]bool, len(touched))
	for _, w := range touched {
		ordinals[w.intent.Ordinal] = true
	}
	intents = slices.DeleteFunc(intents, func(i state.Intent) bool { return !ordinals[i.Ordinal] })
	return c.compensateRollout(ctx, app, intents)
}

func (c *Controller) verifyDeployRoles(ctx, verifyCtx context.Context, d *deployRun) bool {
	unavailable := state.Outcome{
		State: "recovery_required", Err: "verification unavailable",
		SchemaBefore: d.outcome.SchemaBefore, SchemaAfter: d.outcome.SchemaAfter,
	}
	steps, e := c.Store.Intents(verifyCtx, d.opID)
	if e != nil {
		d.outcome = unavailable
		return false
	}
	seen := make(map[int]bool)
	for _, i := range steps {
		if i.Kind != "create" && i.Kind != "update" {
			continue
		}
		if i.Ordinal < 0 || i.Ordinal >= len(d.work) || seen[i.Ordinal] {
			d.outcome = unavailable
			return false
		}
		expected := d.work[i.Ordinal].intent
		if i.Kind != expected.Kind || i.Role != expected.Role || i.Name != expected.Name || i.Generation != expected.Generation || i.ServiceID != expected.ServiceID {
			d.outcome = unavailable
			return false
		}
		seen[i.Ordinal] = true
	}
	if len(seen) != len(d.work) {
		d.outcome = unavailable
		return false
	}
	req := d.prepared.Request
	if e = c.verifyRollout(verifyCtx, steps, req.Manifest.Apps[req.App]); e != nil {
		var failed finalRolloutFailure
		if errors.As(e, &failed) {
			c.unwindDeploy(ctx, d, e, d.work)
			return false
		}
		d.outcome = unavailable
		return false
	}
	return true
}

func (c *Controller) crossDeployBarrier(ctx, verifyCtx context.Context, d *deployRun, tag string) bool {
	req := d.prepared.Request
	before, after := d.outcome.SchemaBefore, d.outcome.SchemaAfter
	recordingFailure := state.Outcome{
		State: "recovery_required", Err: "release recording failed",
		SchemaBefore: before, SchemaAfter: after,
	}
	manifestJSON, err := json.Marshal(req.Manifest)
	if err != nil {
		d.outcome = recordingFailure
		return false
	}
	settings, err := releaseSettings(req, d.rollback)
	if err != nil {
		d.outcome = recordingFailure
		return false
	}
	candidate := state.Outcome{
		State: "succeeded", SchemaBefore: before, SchemaAfter: after, Protected: req.Manifest.Protected,
		Release: &state.Release{
			App: req.App, Digest: d.prepared.Image, Tag: tag, Commit: req.Commit, Modified: req.Modified,
			Manifest: manifestJSON, Bindings: d.bindings,
		},
		Settings: settings,
	}
	barrier, err := json.Marshal(candidate)
	if err != nil {
		d.outcome = recordingFailure
		return false
	}
	// Recording the barrier is the point of no return of a deploy: the release is verified, and the routes and the post-deploy
	// commands follow. A cancel that was accepted before it takes the rollout back instead.
	if err = c.cross(verifyCtx, d.opID, state.Intent{Ordinal: 9999, Kind: "barrier", Desired: barrier, Deadline: d.deadline}, reasonSwitched); err != nil {
		if isCancelled(err) {
			d.outcome = state.Outcome{SchemaBefore: before, SchemaAfter: after}
			c.unwindDeploy(ctx, d, err, d.work)
			return false
		}
		d.outcome = state.Outcome{State: "recovery_required", Err: "verification barrier unavailable", SchemaBefore: before, SchemaAfter: after}
		return false
	}
	d.outcome = candidate
	return true
}

func (c *Controller) finishDeployRelease(ctx context.Context, d *deployRun) {
	req := d.prepared.Request
	// The publish follows the barrier. A crash during it leaves the operation to recovery, which keeps the release. The
	// next deploy or ingress apply brings any route or record it missed in line.
	publishErr := c.publishIngress(ctx, d.opID, d.ing, req.ReplaceExisting)
	var err error
	if !d.rollback {
		err = c.runPost(ctx, d.opID, d.prepared)
	}
	switch {
	case err != nil && errors.Is(err, ErrOneOffUncertain):
		d.outcome = state.Outcome{
			State: "recovery_required", Err: "post-deploy command outcome uncertain",
			SchemaBefore: d.outcome.SchemaBefore, SchemaAfter: d.outcome.SchemaAfter,
			Reason: c.recordOneOffFailure(ctx, d.opID, err),
		}
	case err != nil:
		d.outcome.State = "failed"
		d.outcome.Err = hookFailureText(err, "post")
		d.outcome.Reason = c.recordOneOffFailure(ctx, d.opID, err)
	case publishErr != nil:
		// The new version runs and its release is recorded. Only its routes are missing.
		d.outcome.State = "failed"
		d.outcome.Err = ingressFailure(publishErr, errIngressPublish)
	}
	if d.outcome.State == "succeeded" {
		// The new version serves and its release is recorded. What the checks find is a warning, never a failure.
		app := req.Manifest.Apps[req.App]
		c.verifyDeploy(ctx, d.opID, req.Manifest.NetworkName(c.Environment), app, d.work, d.ing != nil)
	}
}
