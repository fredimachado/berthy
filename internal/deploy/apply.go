package deploy

import (
	"context"
	"errors"
	"time"

	"berthy/internal/manifest"
	"berthy/internal/state"
)

// ApplyRequest asks to apply the environment settings of a manifest without deploying an app.
type ApplyRequest struct {
	Manifest manifest.Manifest `json:"manifest"`
	Commit   string            `json:"commit"`
	// Modified marks a manifest from a checkout with local changes, so Commit does not hold what is applied. A protected
	// environment refuses it.
	Modified bool `json:"modified,omitempty"`
	DryRun   bool `json:"dry_run,omitempty"`
}

// ApplyResult lists what the apply changes, or with a dry run would change, in the environment's settings. It is empty when
// the settings are already what the manifest says.
type ApplyResult struct {
	Changes []string `json:"changes"`
}

// ApplySettings applies the environment scope of a manifest: the registries, destinations, state backup, telemetry, peers,
// alerts and probes that name no app, and the rest the manifest sets for the environment. It deploys no app and starts no
// operation. It is refused for a manifest the agent would refuse to deploy, for another project than the one an app runs, and
// with state.ErrConflict while anything holds the environment lock. A dry run changes nothing.
func (c *Controller) ApplySettings(ctx context.Context, actor Actor, req ApplyRequest) (ApplyResult, error) {
	if c.Store == nil {
		return ApplyResult{}, errors.New("state unavailable")
	}
	if actor.ID == "" {
		return ApplyResult{}, errors.New("actor required")
	}
	if err := checkRequestManifest(req.Manifest); err != nil {
		return ApplyResult{}, err
	}
	if err := req.Manifest.Validate(); err != nil {
		return ApplyResult{}, err
	}
	if err := c.refuseLocalChanges(ctx, Request{Manifest: req.Manifest, Modified: req.Modified}); err != nil {
		return ApplyResult{}, err
	}
	if err := c.checkProject(Request{Manifest: req.Manifest}); err != nil {
		return ApplyResult{}, err
	}
	namesReq := Request{Kind: "apply", Manifest: req.Manifest}
	if err := c.checkNames(ctx, namesReq); err != nil {
		return ApplyResult{}, err
	}
	var prev manifest.Manifest
	if st, ok, err := c.Store.Settings(ctx); err != nil {
		return ApplyResult{}, err
	} else if ok {
		if prev, err = manifest.DecodeSettings(st.Manifest); err != nil {
			return ApplyResult{}, err
		}
	}
	result := ApplyResult{Changes: manifest.SettingsChanges(prev, req.Manifest, c.Environment)}
	if result.Changes == nil {
		result.Changes = []string{}
	}
	if req.DryRun {
		return result, nil
	}
	if c.Reserve != nil {
		claims, err := c.reserveNames(ctx, namesReq)
		if err != nil {
			return ApplyResult{}, err
		}
		defer func() {
			settleCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			c.settle(settleCtx, claims)
		}()
	}
	st, err := state.NewSettings(req.Manifest, req.Commit, req.Modified)
	if err != nil {
		return ApplyResult{}, err
	}
	err = c.Store.ApplySettings(ctx, state.Actor{ID: actor.ID, Name: actor.Name, Remote: actor.Remote}, *st, req.Manifest.Protected)
	if err != nil {
		return ApplyResult{}, err
	}
	return result, nil
}
