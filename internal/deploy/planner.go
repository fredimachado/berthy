package deploy

import (
	"context"
	"errors"
	"sort"

	"berthy/internal/engine"
	"github.com/distribution/reference"
	digest "github.com/opencontainers/go-digest"
)

type preparedPlan struct {
	Request   Request
	Image     string
	NetworkID string
	Auth      *engine.RegistryAuth
	Env       []string
}

func (c *Controller) prepare(ctx context.Context, req Request, image string) (Plan, error) {
	p := Plan{App: req.App, Image: image}
	app := req.Manifest.Apps[req.App]
	for role := range app.Roles {
		p.Roles = append(p.Roles, role)
	}
	sort.Strings(p.Roles)
	if len(app.Hooks.PreDeploy) > 0 || len(app.Hooks.PostDeploy) > 0 || app.Migrations != nil {
		return Plan{}, errors.New("hooks and migrations are unavailable")
	}
	if c.Engine == nil {
		return Plan{}, errors.New("Docker unavailable")
	}
	n, err := c.Engine.InspectNetwork(ctx, req.Manifest.Network)
	if err != nil {
		return Plan{}, errors.New("overlay network unavailable")
	}
	if n.Driver != "overlay" || n.Scope != "swarm" || !n.Attachable {
		return Plan{}, errors.New("network must be an attachable swarm overlay")
	}
	named, err := reference.ParseNormalizedNamed(image)
	if err != nil {
		return Plan{}, err
	}
	host := reference.Domain(named)
	var auth *engine.RegistryAuth
	if reg, ok := req.Manifest.Registries[host]; ok {
		if c.Store == nil {
			return Plan{}, errors.New("registry credential unavailable")
		}
		info, value, err := c.Store.Secret(ctx, reg.PasswordSecret)
		if err != nil || info.Registry != host {
			return Plan{}, errors.New("registry credential unavailable for image host")
		}
		auth = &engine.RegistryAuth{Username: reg.Username, Password: string(value), ServerAddress: host}
	}
	for _, key := range app.Env.Secret {
		if c.Store == nil {
			return Plan{}, errors.New("workload secret unavailable")
		}
		info, _, err := c.Store.Secret(ctx, key)
		if err != nil || info.Registry != "" {
			return Plan{}, errors.New("workload secret unavailable")
		}
	}
	if _, ok := named.(reference.Canonical); !ok {
		d, err := c.Engine.ResolveImage(ctx, image, auth)
		if err != nil {
			return Plan{}, errors.New("image resolution failed")
		}
		pinned, err := reference.WithDigest(reference.TrimNamed(named), digest.Digest(d))
		if err != nil {
			return Plan{}, errors.New("invalid resolved image digest")
		}
		p.Image = pinned.String()
	}
	return p, nil
}
