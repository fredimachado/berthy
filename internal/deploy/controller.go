package deploy

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"berthy/internal/engine"
	"berthy/internal/manifest"
	"berthy/internal/state"
	"berthy/internal/version"
	"github.com/distribution/reference"
)

type Request struct {
	App     string `json:"app"`
	Version string `json:"version"`
	Commit  string `json:"commit"`
	// Modified marks a deploy from a checkout whose manifest or Compose files have local changes, so Commit does not hold
	// what is deployed. A protected environment refuses it.
	Modified      bool              `json:"modified,omitempty"`
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
	// Service names the Compose service an exec runs in. It may be left out when the app has one service.
	Service string `json:"service,omitempty"`
	// Timeout is the time limit of a run, such as "30m". Empty means the default.
	Timeout string `json:"timeout,omitempty"`
	// ReplaceExisting takes over routes and DNS records that Berthy did not create. It needs administrator access.
	ReplaceExisting bool `json:"replace_existing,omitempty"`
	// ComposeFile is the Compose file of a Compose app deploy. The CLI reads it from the manifest directory.
	ComposeFile string `json:"compose_file,omitempty"`
	// AllowRecreate lets a Compose deploy or rollback recreate or remove services of a stateful app, and lets a removal take
	// down a stateful Compose app. It needs administrator access.
	AllowRecreate bool `json:"allow_recreate,omitempty"`
	// Volumes makes a removal or a destroy delete the volumes of the apps too. Without it every volume stays.
	Volumes bool `json:"volumes,omitempty"`
	// Confirm is the environment name a destroy must repeat, so it cannot be sent by accident. Tunnel makes a destroy delete
	// the Cloudflare tunnel too, when it is clean.
	Confirm string `json:"confirm,omitempty"`
	Tunnel  bool   `json:"tunnel,omitempty"`
	// RestoreKey is the object key of the backup a restore reads, and RestoreInto the name of the new volume it fills.
	RestoreKey  string `json:"restore_key,omitempty"`
	RestoreInto string `json:"restore_into,omitempty"`
	// KeepVersion deploys the version the app runs now: the digest and tag of its applied release. It is for an image app and
	// excludes Version. The new manifest is applied as in any deploy.
	KeepVersion bool `json:"keep_version,omitempty"`
	// KeepDigest is the digest the client saw applied when it asked to keep the version. With KeepVersion the agent refuses the
	// deploy when the app's applied release has another digest by the time it resolves the version. Empty: no check.
	KeepDigest string `json:"keep_digest,omitempty"`
	// recordTag is the tag to record when the version is a digest taken from an earlier release: the tag of the release a
	// rollback returns to, or of the applied release a keep_version deploy keeps. It is unexported, so a client cannot send
	// it, and it is not in the request hash.
	recordTag string
}
type Actor struct {
	ID, Name, Remote string
	Admin            bool
	// Agent marks an AI agent's token. On a protected environment its changes are held until a person approves them.
	Agent bool
}
type Plan struct {
	App   string `json:"app"`
	Image string `json:"image"`
	// Tag is the tag the deploy asked for, shown in the plan and recorded with the release. Empty for a digest.
	Tag          string               `json:"tag,omitempty"`
	Roles        []string             `json:"roles"`
	Blockers     []string             `json:"blockers,omitempty"`
	SchemaRevert *SchemaRevertSummary `json:"schema_revert,omitempty"`
	Ingress      *IngressPlan         `json:"ingress,omitempty"`
	Compose      *ComposePlan         `json:"compose,omitempty"`
	Remove       *RemovePlan          `json:"remove,omitempty"`
	Destroy      *DestroyPlan         `json:"destroy,omitempty"`
	Restore      *RestorePlan         `json:"restore,omitempty"`
	// Command is what an exec would run, so the person who approves it sees the command and not only the app.
	Command []string `json:"command,omitempty"`
	// Exec says where an exec runs: the Swarm service of the role, or the Compose project and service. The container itself
	// is chosen when the command runs, so the plan stays the same between a hold and its approval.
	Exec *ExecPlan `json:"exec,omitempty"`
	// Changes lists what the next deploy will change on an adopted service, one line per difference.
	Changes []string `json:"changes,omitempty"`
}
type ExecPlan struct {
	// Target is the Swarm service of the role for an image app, and the Compose project and service, such as shop-db/postgres,
	// for a Compose app.
	Target string `json:"target"`
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
	// Restorer reads backups from object storage for a restore. Nil means restore is unavailable.
	Restorer VolumeRestorer
	// Backups runs the volume backups of `backup:` hooks. Nil means a deploy with such a hook fails at it.
	Backups BackupRunner
	// recoverMu serializes Recover and ResolveRecovery so a resolve never lands during a recovery attempt's Docker changes.
	recoverMu sync.Mutex
	// sessions are the interactive sessions that are open.
	sessions sessionSet
}

// ErrRecoveryPending means an operation is still recovery_required; retries record nothing new.
var ErrRecoveryPending = errors.New("recovery still required")

// checkRequestManifest refuses a manifest whose min_version is above this agent's release. A development build runs any
// manifest. The refusal says what to do, so it is public.
func checkRequestManifest(m manifest.Manifest) error {
	var old *manifest.TooOld
	if err := m.CheckVersion(version.Version); errors.As(err, &old) {
		return Refusal(fmt.Sprintf("this manifest needs Berthy %s or newer; this agent runs %s: upgrade it with berthy setup", old.Need, old.Have))
	}
	return nil
}

// Plan performs no durable or Docker mutations. It checks the manifest a client sent. A rollback's manifest is the stored
// one, so it is not checked, and neither is anything plan calls itself.
func (c *Controller) Plan(ctx context.Context, actor Actor, req Request) (Plan, error) {
	if req.Kind != "rollback" {
		if err := checkRequestManifest(req.Manifest); err != nil {
			return Plan{}, err
		}
	}
	return c.plan(ctx, actor, req)
}

func (c *Controller) plan(ctx context.Context, actor Actor, req Request) (Plan, error) {
	if err := c.refuseLocalChanges(ctx, req); err != nil {
		return Plan{}, err
	}
	if req.ReplaceExisting && !actor.Admin {
		return Plan{}, errReplaceAdmin
	}
	if req.AllowRecreate && !actor.Admin {
		return Plan{}, errAllowRecreateAdmin
	}
	if req.KeepVersion || req.KeepDigest != "" {
		resolved, err := c.resolveKeepVersion(ctx, req)
		if err != nil {
			return Plan{}, err
		}
		return c.plan(ctx, actor, resolved)
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
		return c.plan(ctx, actor, resolved)
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
	p, err := c.prepare(ctx, req, image)
	if err != nil {
		return Plan{}, err
	}
	if req.recordTag != "" {
		// The digest of an earlier release has no tag of its own, so the deploy records the one that release had.
		p.Tag = req.recordTag
	}
	return p, nil
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
	if req.Kind != "rollback" {
		if err := checkRequestManifest(req.Manifest); err != nil {
			return state.Operation{}, false, err
		}
	}
	if req.KeepVersion || req.KeepDigest != "" {
		// What state alone can refuse is refused now, so the caller sees it at once instead of in a failed operation.
		if _, err := c.resolveKeepVersion(ctx, req); err != nil {
			return state.Operation{}, false, err
		}
	}
	// A rollback deploys the release it returns to, so that release is what the local-changes rule judges.
	planned := req
	if req.Kind == "rollback" && !req.RevertSchema {
		if resolved, err := c.rollbackRequest(ctx, req); err == nil {
			planned = resolved
		}
	}
	if err := c.refuseLocalChanges(ctx, planned); err != nil {
		return state.Operation{}, false, err
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
	if req.Kind == "destroy" {
		if err := c.destroyCheck(actor, req); err != nil {
			return state.Operation{}, false, err
		}
	}
	if req.Kind == restoreKind {
		// What the backup and the volume alone can refuse is refused now, so the caller sees it at once.
		if _, err := c.restoreCheck(ctx, actor, req); err != nil {
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
	if c.Store != nil && req.Kind == "scale" {
		_, compose, err := c.appliedCompose(ctx, req.App)
		if err != nil {
			return state.Operation{}, false, err
		}
		if compose {
			return state.Operation{}, false, errScaleCompose
		}
	}
	if req.Kind == "exec" {
		// A flag the app's kind does not take is refused now, so the caller sees it at once instead of in a failed operation.
		if _, _, err := c.execKind(ctx, req); err != nil {
			return state.Operation{}, false, err
		}
	}
	if req.Kind == "run" {
		// What state alone can refuse is refused now, so the caller sees it at once instead of in a failed operation.
		if _, err := c.runTarget(ctx, req); err != nil {
			return state.Operation{}, false, err
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
		Modified        bool   `json:"modified,omitempty"`
		Adopt           bool   `json:"adopt"`
		RevertSchema    bool   `json:"revert_schema,omitempty"`
		TargetRelease   int64  `json:"target_release,omitempty"`
		ReplaceExisting bool   `json:"replace_existing,omitempty"`
		AllowRecreate   bool   `json:"allow_recreate,omitempty"`
		Volumes         bool   `json:"volumes,omitempty"`
		Tunnel          bool   `json:"tunnel,omitempty"`
		RestoreKey      string `json:"restore_key,omitempty"`
		RestoreInto     string `json:"restore_into,omitempty"`
		KeepVersion     bool   `json:"keep_version,omitempty"`
		KeepDigest      string `json:"keep_digest,omitempty"`
	}{req.App, req.Version, req.Commit, req.Modified, req.Adopt, req.RevertSchema, req.TargetRelease, req.ReplaceExisting, req.AllowRecreate, req.Volumes, req.Tunnel, req.RestoreKey, req.RestoreInto, req.KeepVersion, req.KeepDigest})
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
	op, created, err := c.Store.Admit(ctx, state.Admission{RequestID: req.RequestID, ActorID: actor.ID, ActorName: actor.Name, Remote: actor.Remote, Kind: kind, App: req.App, Commit: req.Commit, Modified: req.Modified, RequestHash: sum, PublicRequest: public})
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
	if (op.Kind == "remove" || op.Kind == "destroy") && op.State == "running" {
		return c.recoverRemove(ctx, op, intents)
	}
	if op.Kind == restoreKind && op.State == "running" {
		return c.recoverRestore(ctx, op, intents)
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
	req.recordTag = chosen.Tag
	req.Commit = chosen.Commit
	req.Modified = chosen.Modified
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

// errLocalChanges refuses a deploy from a checkout with local changes on a protected environment. The CLI names the files
// before it sends anything, so the agent says only this.
const errLocalChanges Refusal = "the manifest has local changes: commit them first"

// refuseLocalChanges refuses a request that comes from a checkout with local changes when its environment is protected: by the
// agent's configuration, by the request's manifest, or by an earlier protected deploy.
func (c *Controller) refuseLocalChanges(ctx context.Context, req Request) error {
	if !req.Modified {
		return nil
	}
	protected := c.Protected || req.Manifest.Protected
	if !protected && c.Store != nil {
		sticky, err := c.Store.Protected(ctx)
		if err != nil {
			return err
		}
		protected = sticky
	}
	if protected {
		return errLocalChanges
	}
	return nil
}

const (
	errKeepVersionKind    Refusal = "keep_version applies to a deploy"
	errKeepVersionExclude Refusal = "keep_version and version are exclusive"
	errKeepVersionImage   Refusal = "keep_version applies to an image app"
	errKeepDigestAlone    Refusal = "keep_digest applies with keep_version"
)

// keepChangedMessage matches the refusal of a keep_version deploy whose app runs another digest than the client read. It names
// the app and two shortened digests, so it is public like the fixed refusals.
var keepChangedMessage = regexp.MustCompile(`^app [a-z0-9][a-z0-9_-]{0,62} changed since its version was read: it runs sha256:[0-9a-f]{12} now, not sha256:[0-9a-f]{12}; deploy it again$`)

// ShortDigest shortens an image digest, with or without its repository, to sha256: and its first 12 hex characters, for a text
// a person reads. Anything else, such as a tag, is returned as it is.
func ShortDigest(v string) string {
	if _, digest, found := strings.Cut(v, "@"); found {
		v = digest
	}
	if hex, ok := strings.CutPrefix(v, "sha256:"); ok && len(hex) > 12 {
		return "sha256:" + hex[:12]
	}
	return v
}

// resolveKeepVersion turns a keep_version deploy into one of the version the app runs now: the digest of its applied release,
// and that release's tag to record. With a keep_digest it refuses the deploy when the app runs another digest, so a
// deploy that landed after the client read the version is not undone by this one. A request without keep_version is returned as
// it is. The client's own request, not this one, is what the request hash covers.
func (c *Controller) resolveKeepVersion(ctx context.Context, req Request) (Request, error) {
	if !req.KeepVersion {
		if req.KeepDigest != "" {
			return Request{}, errKeepDigestAlone
		}
		return req, nil
	}
	switch {
	case req.Kind != "" && req.Kind != "deploy", req.Adopt:
		return Request{}, errKeepVersionKind
	case req.Version != "":
		return Request{}, errKeepVersionExclude
	case req.Manifest.Apps[req.App].Kind != "image":
		return Request{}, errKeepVersionImage
	case c.Store == nil:
		return Request{}, errors.New("applied release unavailable")
	}
	applied, err := c.Store.Applied(ctx, req.App)
	if errors.Is(err, sql.ErrNoRows) {
		return Request{}, Refusal("app " + req.App + " is not deployed yet: give --version " + req.App + "=REF")
	}
	if err != nil {
		return Request{}, errors.New("applied release unavailable")
	}
	if req.KeepDigest != "" && applied.Digest != req.KeepDigest {
		return Request{}, Refusal("app " + req.App + " changed since its version was read: it runs " + ShortDigest(applied.Digest) + " now, not " + ShortDigest(req.KeepDigest) + "; deploy it again")
	}
	req.Version = applied.Digest
	req.recordTag = applied.Tag
	req.KeepVersion = false
	req.KeepDigest = ""
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
		if plan, err = c.plan(ctx, actor, resolved); err != nil {
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
