package deploy

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"

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
	if _, err := c.Plan(ctx, actor, req); err != nil {
		return state.Operation{}, false, err
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
		App, Version, Commit string
		Adopt                bool
	}{req.App, req.Version, req.Commit, req.Adopt})
	if err != nil {
		return state.Operation{}, false, err
	}
	if c.Store == nil {
		return state.Operation{}, false, errors.New("state unavailable")
	}
	op, created, err := c.Store.Admit(ctx, state.Admission{RequestID: req.RequestID, ActorID: actor.ID, ActorName: actor.Name, Remote: actor.Remote, Kind: "deploy", App: req.App, Commit: req.Commit, RequestHash: sum, PublicRequest: public})
	if err != nil || !created {
		return op, created, err
	}
	go c.execute(context.Background(), op, actor, req)
	return op, true, nil
}
func (c *Controller) execute(ctx context.Context, op state.Operation, actor Actor, req Request) {
	// Re-check mutable prerequisites after acquiring operation ownership.
	_, err := c.Plan(ctx, actor, req)
	if err == nil {
		err = errors.New("deployment execution unavailable")
	}
	_ = c.Store.Finish(ctx, op.ID, state.Outcome{State: "failed", Err: fmt.Sprint(err)})
}
func (c *Controller) Recover(ctx context.Context) error {
	if c.Store == nil {
		return errors.New("state unavailable")
	}
	op, err := c.Store.Active(ctx)
	if err != nil {
		return err
	}
	intents, err := c.Store.Intents(ctx, op.ID)
	if err != nil {
		return err
	}
	if len(intents) > 0 {
		return c.Store.Finish(ctx, op.ID, state.Outcome{State: "recovery_required", Err: "deployment requires recovery"})
	}
	return c.Store.Finish(ctx, op.ID, state.Outcome{State: "failed", Err: "interrupted before rollout"})
}
