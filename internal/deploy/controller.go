package deploy

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"

	"berthy/internal/engine"
	"berthy/internal/manifest"
	"berthy/internal/state"
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
}
type Actor struct {
	ID, Name, Remote string
	Admin            bool
}
type Plan struct {
	App      string   `json:"app"`
	Image    string   `json:"image"`
	Roles    []string `json:"roles"`
	Blockers []string `json:"blockers,omitempty"`
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
}

// Plan performs no durable or Docker mutations.
func (c *Controller) Plan(ctx context.Context, actor Actor, req Request) (Plan, error) {
	if req.Adopt {
		if !actor.Admin {
			return Plan{}, errors.New("adoption requires administrator access")
		}
		plan, _, err := c.planAdoption(ctx, req)
		return plan, err
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
	if req.Kind == "" || req.Kind == "deploy" {
		if err := req.Manifest.Validate(); err != nil {
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
		App     string `json:"app"`
		Version string `json:"version"`
		Commit  string `json:"commit"`
		Adopt   bool   `json:"adopt"`
	}{req.App, req.Version, req.Commit, req.Adopt})
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
	if kind == "deploy" || kind == "rollback" {
		go c.execute(context.Background(), op, actor, req)
	} else {
		go func() {
			outcome := c.runOperation(context.Background(), op, actor, req)
			_ = c.Store.Finish(context.Background(), op.ID, outcome)
		}()
	}
	return op, true, nil
}
func (c *Controller) Recover(ctx context.Context) error {
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
	hasHooks := false
	var barrier *state.Intent
	for _, i := range intents {
		if i.Kind == "oneoff" {
			hasHooks = true
		} else if i.Kind == "barrier" {
			copyIntent := i
			barrier = &copyIntent
		} else if i.Kind == "exec" && i.Phase == "finished" {
		} else if i.Kind != "create" && i.Kind != "update" {
			return c.Store.Finish(ctx, op.ID, state.Outcome{State: "recovery_required", Err: "unsupported recovery step"})
		}
	}
	if hasHooks {
		if err = c.recoverHooks(ctx, op.ID); err != nil {
			return c.Store.Finish(ctx, op.ID, state.Outcome{State: "recovery_required", Err: "one-off recovery required"})
		}
	}
	if barrier != nil {
		out, err := c.recoverBarrier(ctx, intents, *barrier)
		if err != nil {
			return c.Store.Finish(ctx, op.ID, state.Outcome{State: "recovery_required", Err: "verified release requires inspection"})
		}
		return c.Store.Finish(ctx, op.ID, out)
	}
	if err = c.compensateRollout(ctx, op.ID, op.App); err != nil {
		return c.Store.Finish(ctx, op.ID, state.Outcome{State: "recovery_required", Err: "service recovery required"})
	}
	return c.Store.Finish(ctx, op.ID, state.Outcome{State: "interrupted", Err: "operation interrupted and cleaned up"})
}

func (c *Controller) rollbackRequest(ctx context.Context, req Request) (Request, error) {
	if c.Store == nil || req.App == "" {
		return Request{}, errors.New("rollback history unavailable")
	}
	if req.RevertSchema {
		return Request{}, errors.New("schema revert unavailable")
	}
	current, err := c.Store.Applied(ctx, req.App)
	if err != nil {
		return Request{}, errors.New("applied release unavailable")
	}
	history, err := c.Store.History(ctx, req.App)
	if err != nil {
		return Request{}, err
	}
	var chosen *state.Release
	for idx := range history {
		r := &history[idx]
		if req.TargetRelease != 0 {
			if r.ID == req.TargetRelease {
				chosen = r
				break
			}
		} else if r.ID < current.ID {
			chosen = r
			break
		}
	}
	if chosen == nil {
		return Request{}, errors.New("rollback release unavailable")
	}
	var m manifest.Manifest
	if json.Unmarshal(chosen.Manifest, &m) != nil {
		return Request{}, errors.New("saved release invalid")
	}
	req.Manifest = m
	req.Version = chosen.Digest
	req.Commit = chosen.Commit
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
