package deploy

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"berthy/internal/engine"
	"berthy/internal/manifest"
	"berthy/internal/state"
	"github.com/distribution/reference"
)

type Request struct {
	App           string            `json:"app"`
	Version       string            `json:"version"`
	Commit        string            `json:"commit"`
	RequestID     string            `json:"request_id"`
	Manifest      manifest.Manifest `json:"manifest"`
	DryRun        bool              `json:"dry_run,omitempty"`
	Adopt         bool              `json:"adopt,omitempty"`
	Kind          string            `json:"kind,omitempty"`
	TargetRelease int64             `json:"target_release,omitempty"`
	Role          string            `json:"role,omitempty"`
	Replicas      *int              `json:"replicas,omitempty"`
	RevertSchema  bool              `json:"revert_schema,omitempty"`
	Command       []string          `json:"command,omitempty"`
	// ReplaceExisting takes over routes and DNS records that Berthy did not create. It needs administrator access.
	ReplaceExisting bool `json:"replace_existing,omitempty"`
	// ComposeFile is the Compose file of a Compose app deploy. The CLI reads it from the manifest directory.
	ComposeFile string `json:"compose_file,omitempty"`
	// AllowRecreate lets a Compose deploy or rollback recreate or remove services of a stateful app, and lets a removal take
	// down a stateful Compose app. It needs administrator access.
	AllowRecreate bool `json:"allow_recreate,omitempty"`
	// Volumes makes a removal delete the app's volumes too. Without it every volume stays.
	Volumes bool `json:"volumes,omitempty"`
}
type Actor struct {
	ID, Name, Remote string
	Admin            bool
}
type Plan struct {
	App          string               `json:"app"`
	Image        string               `json:"image"`
	Roles        []string             `json:"roles"`
	Blockers     []string             `json:"blockers,omitempty"`
	SchemaRevert *SchemaRevertSummary `json:"schema_revert,omitempty"`
	Ingress      *IngressPlan         `json:"ingress,omitempty"`
	Compose      *ComposePlan         `json:"compose,omitempty"`
	Remove       *RemovePlan          `json:"remove,omitempty"`
}
type SchemaRevertSummary struct {
	SourceOperation string `json:"source_operation"`
	SourceImage     string `json:"source_image"`
	From            string `json:"from,omitempty"`
	Target          string `json:"target"`
	RollbackRelease int64  `json:"rollback_release,omitempty"`
}
type Engine interface {
	InspectService(context.Context, string) (engine.ServiceSnapshot, error)
	CreateService(context.Context, json.RawMessage, *engine.RegistryAuth) (string, error)
	UpdateService(context.Context, string, uint64, json.RawMessage, *engine.RegistryAuth) error
	DeleteService(context.Context, string) error
	InspectNetwork(context.Context, string) (engine.Network, error)
	ResolveImage(context.Context, string, *engine.RegistryAuth) (string, error)
	PullImage(context.Context, string, *engine.RegistryAuth) error
	InspectImage(context.Context, string) (engine.Image, error)
	ServiceTasks(context.Context, string) ([]engine.ServiceTask, error)
	InspectContainerHealth(context.Context, string) (string, error)
}
type Controller struct {
	Engine      Engine
	Store       *state.Store
	Environment string
	Protected   bool
	// CloudflareURL is the Cloudflare API base URL. Empty means the production API.
	CloudflareURL string
	// Compose runs the Compose CLI for Compose apps. StateDir holds their working directories under compose/.
	Compose  ComposeRunner
	StateDir string
	// recoverMu serializes Recover and ResolveRecovery so a resolve never lands during a recovery attempt's Docker changes.
	recoverMu sync.Mutex
}

// ErrRecoveryPending means an operation is still recovery_required; retries record nothing new.
var ErrRecoveryPending = errors.New("recovery still required")

// Plan performs no durable or Docker mutations.
func (c *Controller) Plan(ctx context.Context, actor Actor, req Request) (Plan, error) {
	if req.ReplaceExisting && !actor.Admin {
		return Plan{}, errReplaceAdmin
	}
	if req.AllowRecreate && !actor.Admin {
		return Plan{}, errAllowRecreateAdmin
	}
	if req.Kind == "ingress" {
		return c.planIngress(ctx, actor, req)
	}
	if req.Adopt {
		if !actor.Admin {
			return Plan{}, errors.New("adoption requires administrator access")
		}
		if composeRequest(req) {
			return c.planCompose(ctx, actor, req)
		}
		plan, bindings, err := c.planAdoption(ctx, req)
		if err != nil {
			return Plan{}, err
		}
		if req.DryRun {
			// The routes an adoption publishes follow its bindings, which the plan does not yet record.
			if err = c.dryRunPublish(ctx, &plan, req.Manifest, req.App, appRouteWants(req.Manifest, req.App, bindings), req.ReplaceExisting); err != nil {
				return Plan{}, err
			}
		}
		return plan, nil
	}
	if req.Kind == "rollback" && req.RevertSchema {
		return c.planSchemaRevert(ctx, actor, req)
	}
	if req.Kind == "rollback" {
		resolved, err := c.rollbackRequest(ctx, req)
		if err != nil {
			return Plan{}, err
		}
		resolved.Kind = "deploy"
		return c.Plan(ctx, actor, resolved)
	}
	if req.Kind != "" && req.Kind != "deploy" && req.Kind != "rollback" {
		return c.planOperation(ctx, actor, req)
	}
	if err := req.Manifest.Validate(); err != nil {
		return Plan{}, err
	}
	if composeRequest(req) {
		return c.planCompose(ctx, actor, req)
	}
	if err := checkComposeRequest(req); err != nil {
		return Plan{}, err
	}
	protected := c.Protected
	if c.Store != nil {
		sticky, err := c.Store.Protected(ctx)
		if err != nil {
			return Plan{}, err
		}
		protected = protected || sticky
	}
	image, err := req.Manifest.CheckImageDeploy(req.App, req.Version, protected)
	if err != nil {
		return Plan{}, err
	}
	if req.Adopt && !actor.Admin {
		return Plan{}, errors.New("adoption requires administrator access")
	}
	if req.Kind != "" && req.Kind != "deploy" {
		return Plan{}, errors.New("unsupported operation")
	}
	return c.prepare(ctx, req, image)
}

// hasHostnames reports whether the app declares a public hostname on any role.
func hasHostnames(app manifest.App) bool {
	for _, role := range app.Roles {
		if len(role.Hostnames) > 0 {
			return true
		}
	}
	return false
}

// Admit durably claims the environment before an agent-owned worker starts.
func (c *Controller) Admit(ctx context.Context, actor Actor, req Request) (state.Operation, bool, error) {
	if req.DryRun {
		return state.Operation{}, false, errors.New("use Plan for dry run")
	}
	if actor.ID == "" || req.RequestID == "" {
		return state.Operation{}, false, errors.New("actor and request ID required")
	}
	if req.Adopt && !actor.Admin {
		return state.Operation{}, false, errors.New("adoption requires administrator access")
	}
	if req.RevertSchema && !actor.Admin {
		return state.Operation{}, false, errors.New("schema revert requires administrator access")
	}
	if req.ReplaceExisting && !actor.Admin {
		return state.Operation{}, false, errReplaceAdmin
	}
	if req.AllowRecreate && !actor.Admin {
		return state.Operation{}, false, errAllowRecreateAdmin
	}
	if req.Kind == "ingress" {
		req.App = manifest.ConnectorAppName
		if err := req.Manifest.Validate(); err != nil {
			return state.Operation{}, false, err
		}
	}
	if req.Kind == "remove" {
		// What state alone can refuse is refused now, so the caller sees it at once instead of in a failed operation.
		if _, _, err := c.removeCheck(ctx, actor, req); err != nil {
			return state.Operation{}, false, err
		}
	}
	if req.Kind == "" || req.Kind == "deploy" {
		if err := req.Manifest.Validate(); err != nil {
			return state.Operation{}, false, err
		}
		if err := checkComposeRequest(req); err != nil {
			return state.Operation{}, false, err
		}
	}
	if c.Store != nil && (req.Kind == "scale" || req.Kind == "exec") {
		_, compose, err := c.appliedCompose(ctx, req.App)
		if err != nil {
			return state.Operation{}, false, err
		}
		if compose && req.Kind == "scale" {
			return state.Operation{}, false, errScaleCompose
		}
		if compose {
			return state.Operation{}, false, errExecCompose
		}
	}
	copyReq := req
	copyReq.RequestID = ""
	copyReq.DryRun = false
	b, err := json.Marshal(copyReq)
	if err != nil {
		return state.Operation{}, false, err
	}
	sum := sha256.Sum256(b)
	public, err := json.Marshal(struct {
		App             string `json:"app"`
		Version         string `json:"version"`
		Commit          string `json:"commit"`
		Adopt           bool   `json:"adopt"`
		RevertSchema    bool   `json:"revert_schema,omitempty"`
		TargetRelease   int64  `json:"target_release,omitempty"`
		ReplaceExisting bool   `json:"replace_existing,omitempty"`
		AllowRecreate   bool   `json:"allow_recreate,omitempty"`
		Volumes         bool   `json:"volumes,omitempty"`
	}{req.App, req.Version, req.Commit, req.Adopt, req.RevertSchema, req.TargetRelease, req.ReplaceExisting, req.AllowRecreate, req.Volumes})
	if err != nil {
		return state.Operation{}, false, err
	}
	if c.Store == nil {
		return state.Operation{}, false, errors.New("state unavailable")
	}
	kind := req.Kind
	if kind == "" {
		kind = "deploy"
	}
	op, created, err := c.Store.Admit(ctx, state.Admission{RequestID: req.RequestID, ActorID: actor.ID, ActorName: actor.Name, Remote: actor.Remote, Kind: kind, App: req.App, Commit: req.Commit, RequestHash: sum, PublicRequest: public})
	if err != nil || !created {
		return op, created, err
	}
	switch kind {
	case "deploy", "rollback":
		go c.execute(context.Background(), op, actor, req)
	case "ingress":
		go func() {
			outcome := c.runIngress(context.Background(), op, actor, req)
			_ = c.Store.Finish(context.Background(), op.ID, outcome)
		}()
	default:
		go func() {
			outcome := c.runOperation(context.Background(), op, actor, req)
			_ = c.Store.Finish(context.Background(), op.ID, outcome)
		}()
	}
	return op, true, nil
}
func (c *Controller) Recover(ctx context.Context) error {
	c.recoverMu.Lock()
	defer c.recoverMu.Unlock()
	if c.Store == nil || c.Engine == nil {
		return errors.New("recovery unavailable")
	}
	op, err := c.Store.Active(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	intents, err := c.Store.Intents(ctx, op.ID)
	if err != nil {
		return err
	}
	if op.Kind == "remove" && op.State == "running" {
		return c.recoverRemove(ctx, op, intents)
	}
	before, after := recordedSchema(op, intents)
	reason := ""
	// A retry of an operation already marked recovery_required must not append another event and audit row.
	required := func(msg string) error {
		if op.State == "recovery_required" {
			return ErrRecoveryPending
		}
		return c.Store.Finish(ctx, op.ID, state.Outcome{State: "recovery_required", Err: msg, SchemaBefore: before, SchemaAfter: after, Reason: reason})
	}
	hasHooks := false
	composeRun := false
	var barrier *state.Intent
	for _, i := range intents {
		if i.Kind == "oneoff" {
			hasHooks = true
		} else if i.Kind == "schema_source" || i.Kind == "compose_source" {
		} else if i.Kind == "compose" {
			composeRun = true
		} else if i.Kind == "barrier" {
			copyIntent := i
			barrier = &copyIntent
		} else if i.Kind == "exec" && i.Phase == "finished" {
		} else if i.Kind != "create" && i.Kind != "update" {
			return required("unsupported recovery step")
		}
	}
	if hasHooks {
		if err = c.recoverHooks(ctx, op.ID); err != nil {
			return required("one-off recovery required")
		}
	}
	if barrier != nil {
		out, err := c.recoverBarrier(ctx, intents, *barrier)
		if err != nil {
			return required("verified release requires inspection")
		}
		if out.SchemaBefore == "" {
			out.SchemaBefore = before
		}
		if out.SchemaAfter == "" {
			out.SchemaAfter = after
		}
		return c.Store.Finish(ctx, op.ID, out)
	}
	if composeRun {
		// Compose has no compensation: the next deploy converges the project, as up is idempotent.
		return c.Store.Finish(ctx, op.ID, state.Outcome{State: "interrupted", Err: string(errComposeInterrupted), SchemaBefore: before, SchemaAfter: after})
	}
	reason = c.captureInterrupted(ctx, op.ID, intents)
	if err = c.compensateRollout(ctx, op.ID, op.App); err != nil {
		return required("service recovery required")
	}
	return c.Store.Finish(ctx, op.ID, state.Outcome{State: "interrupted", Err: "operation interrupted and cleaned up", SchemaBefore: before, SchemaAfter: after, Reason: reason})
}

// recordedSchema returns the schema states of an operation, falling back to the versions its finished one-offs journaled.
func recordedSchema(op state.Operation, intents []state.Intent) (before, after string) {
	before, after = op.SchemaBefore, op.SchemaAfter
	for _, i := range intents {
		if i.Kind != "oneoff" || i.Phase != "finished" {
			continue
		}
		var r struct {
			Version string `json:"version"`
		}
		if json.Unmarshal(i.Receipt, &r) != nil || r.Version == "" {
			continue
		}
		switch i.Role {
		case "schema_before", "revert_before":
			if before == "" {
				before = r.Version
			}
		case "schema_after", "revert_after":
			if after == "" {
				after = r.Version
			}
		}
	}
	return before, after
}

func (c *Controller) rollbackRequest(ctx context.Context, req Request) (Request, error) {
	if c.Store == nil || req.App == "" {
		return Request{}, errors.New("rollback history unavailable")
	}
	if req.RevertSchema {
		return Request{}, errors.New("use schema revert")
	}

	chosen, err := c.selectRollback(ctx, req)
	if err != nil {
		return Request{}, err
	}
	var m manifest.Manifest
	if json.Unmarshal(chosen.Manifest, &m) != nil {
		return Request{}, errors.New("saved release invalid")
	}
	m.Connector = req.App == manifest.ConnectorAppName
	req.Manifest = m
	req.Version = chosen.Digest
	req.Commit = chosen.Commit
	if composeRequest(req) {
		// A Compose release has no image digest. Its deploy journaled the Compose file it ran.
		file, err := c.composeSource(ctx, chosen.OperationID)
		if err != nil {
			return Request{}, err
		}
		req.ComposeFile = file
		req.Version = ""
	}
	return req, nil
}

func (c *Controller) recoverBarrier(ctx context.Context, intents []state.Intent, barrier state.Intent) (state.Outcome, error) {
	var out state.Outcome
	if json.Unmarshal(barrier.Desired, &out) != nil || out.Release == nil {
		return state.Outcome{}, errors.New("invalid verification barrier")
	}
	for _, i := range intents {
		if i.Kind != "create" && i.Kind != "update" {
			continue
		}
		if i.Phase != "verified" {
			return state.Outcome{}, errors.New("unverified service")
		}
		var receipt struct {
			ID   string          `json:"id"`
			Spec json.RawMessage `json:"spec"`
		}
		if json.Unmarshal(i.Receipt, &receipt) != nil || receipt.ID == "" {
			return state.Outcome{}, errors.New("missing service observation")
		}
		live, err := c.Engine.InspectService(ctx, i.Name)
		if err != nil || live.ID != receipt.ID || !sameSpec(live.Spec, receipt.Spec) {
			return state.Outcome{}, errors.New("service changed after verification")
		}
		var spec struct {
			Mode struct{ Replicated *struct{ Replicas int } }
		}
		if json.Unmarshal(live.Spec, &spec) != nil || spec.Mode.Replicated == nil {
			return state.Outcome{}, errors.New("invalid service mode")
		}
		if err = healthyOnce(ctx, c.Engine, live.ID, i.Generation, spec.Mode.Replicated.Replicas); err != nil {
			return state.Outcome{}, err
		}
		if err = waitDrain(ctx, c.Engine, live.ID, i.Generation); err != nil {
			return state.Outcome{}, err
		}
		if i.Kind == "update" && live.UpdateStatus.State != "completed" {
			return state.Outcome{}, errors.New("native update incomplete")
		}
	}
	out.State = "failed"
	out.Event = nil
	out.Err = "post-deploy completion was interrupted"
	return out, nil
}

func (c *Controller) selectRollback(ctx context.Context, req Request) (state.Release, error) {
	current, err := c.Store.Applied(ctx, req.App)
	if err != nil {
		return state.Release{}, errors.New("applied release unavailable")
	}
	history, err := c.Store.History(ctx, req.App)
	if err != nil {
		return state.Release{}, err
	}
	for _, r := range history {
		if req.TargetRelease != 0 && r.ID == req.TargetRelease || req.TargetRelease == 0 && r.ID < current.ID {
			return r, nil
		}
	}
	return state.Release{}, errors.New("rollback release unavailable")
}

// planSchemaRevert describes a schema revert without changing anything.
func (c *Controller) planSchemaRevert(ctx context.Context, actor Actor, req Request) (Plan, error) {
	if !actor.Admin {
		return Plan{}, errAdminRequired
	}
	sr, err := c.schemaRevertPlan(ctx, req)
	if err != nil {
		return Plan{}, err
	}
	plan := Plan{App: req.App, Image: sr.Prepared.Image, Roles: []string{}}
	summary := SchemaRevertSummary{SourceOperation: sr.Source.ID, SourceImage: sr.Prepared.Image, From: sr.Source.SchemaAfter, Target: sr.Target}
	if sr.Image != nil {
		imgReq := req
		imgReq.RevertSchema = false
		imgReq.TargetRelease = sr.Image.ID
		resolved, err := c.rollbackRequest(ctx, imgReq)
		if err != nil {
			return Plan{}, err
		}
		resolved.Kind = "deploy"
		if plan, err = c.Plan(ctx, actor, resolved); err != nil {
			return Plan{}, err
		}
		summary.RollbackRelease = sr.Image.ID
	}
	plan.SchemaRevert = &summary
	return plan, nil
}

type schemaRevert struct {
	Source          state.Operation
	Prepared        preparedPlan
	Target          string
	Revert, Version []string
	Timeout         time.Duration  // the migration's own time limit for each command
	Image           *state.Release // rolled back to first; nil keeps the current image
}

// schemaRevertPlan picks the newest recorded migration, the state to revert to, and the release to roll back to first.
func (c *Controller) schemaRevertPlan(ctx context.Context, req Request) (schemaRevert, error) {
	if c.Store == nil || req.App == "" {
		return schemaRevert{}, errors.New("schema revert unavailable")
	}
	src, err := c.Store.LatestMigration(ctx, req.App)
	if errors.Is(err, sql.ErrNoRows) {
		return schemaRevert{}, errNoMigration
	}
	if err != nil {
		return schemaRevert{}, err
	}
	intents, err := c.Store.Intents(ctx, src.ID)
	if err != nil {
		return schemaRevert{}, err
	}
	var source struct {
		Manifest manifest.Manifest `json:"manifest"`
		Image    string            `json:"image"`
	}
	journaled := false
	for _, i := range intents {
		if i.Kind == "schema_source" {
			journaled = json.Unmarshal(i.Desired, &source) == nil
			break
		}
	}
	if !journaled || source.Manifest.Validate() != nil {
		return schemaRevert{}, errSourceMissing
	}
	app, ok := source.Manifest.Apps[req.App]
	if !ok || app.Migrations == nil || len(app.Migrations.Revert) == 0 || len(app.Migrations.Version) == 0 {
		return schemaRevert{}, errNoRevertCommand
	}
	ref, err := reference.ParseNormalizedNamed(source.Image)
	if err != nil {
		return schemaRevert{}, errSourceImage
	}
	if _, ok := ref.(reference.Canonical); !ok {
		return schemaRevert{}, errSourceNotPinned
	}
	current, err := c.Store.Applied(ctx, req.App)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return schemaRevert{}, errors.New("applied release unavailable")
	}
	hasCurrent := err == nil
	history, err := c.Store.History(ctx, req.App)
	if err != nil {
		return schemaRevert{}, err
	}
	var target string
	var image *state.Release
	if req.TargetRelease != 0 {
		var rel *state.Release
		for i := range history {
			if history[i].ID == req.TargetRelease {
				rel = &history[i]
				break
			}
		}
		if rel == nil || rel.SchemaAfter == "" || !rel.Created.Before(src.Started) {
			return schemaRevert{}, errTargetMissing
		}
		target = rel.SchemaAfter
		if !hasCurrent || current.ID != rel.ID {
			image = rel
		}
	} else {
		target = src.SchemaBefore
		switch {
		case hasCurrent && current.OperationID == src.ID:
			for i := range history {
				if history[i].ID < current.ID {
					image = &history[i]
					break
				}
			}
			if image == nil {
				return schemaRevert{}, errNoEarlierRelease
			}
		case hasCurrent && current.Created.After(src.Started):
			return schemaRevert{}, errNewerRelease
		}
	}
	args := append([]string(nil), app.Migrations.Revert...)
	count := 0
	for i, v := range args {
		if v == "{target}" {
			args[i] = target
			count++
		}
	}
	if count != 1 {
		return schemaRevert{}, errBadRevertCommand
	}
	if c.Engine == nil {
		return schemaRevert{}, errors.New("Docker unavailable")
	}
	p, err := c.prepareExecution(ctx, Request{App: req.App, Manifest: source.Manifest}, Plan{Image: source.Image})
	if err != nil {
		return schemaRevert{}, err
	}
	return schemaRevert{Source: src, Prepared: p, Target: target, Revert: args, Version: app.Migrations.Version, Timeout: app.Migrations.TimeoutDuration(), Image: image}, nil
}
