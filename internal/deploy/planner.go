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

func (c *Controller) prepareExecution(ctx context.Context, req Request, p Plan) (preparedPlan, error) {
	n, err := c.Engine.InspectNetwork(ctx, req.Manifest.Network)
	if err != nil || n.Driver != "overlay" || n.Scope != "swarm" || !n.Attachable {
		return preparedPlan{}, errors.New("overlay network unavailable")
	}
	out := preparedPlan{Request: req, Image: p.Image, NetworkID: n.ID}
	named, err := reference.ParseNormalizedNamed(p.Image)
	if err != nil {
		return out, err
	}
	host := reference.Domain(named)
	if reg, ok := req.Manifest.Registries[host]; ok {
		info, value, e := c.Store.Secret(ctx, reg.PasswordSecret)
		if e != nil || info.Registry != host {
			return out, errors.New("registry credential unavailable")
		}
		out.Auth = &engine.RegistryAuth{Username: reg.Username, Password: string(value), ServerAddress: host}
	}
	app := req.Manifest.Apps[req.App]
	for k, v := range app.Env.Plain {
		out.Env = append(out.Env, k+"="+v)
	}
	for _, k := range app.Env.Secret {
		info, value, e := c.Store.Secret(ctx, k)
		if e != nil || info.Registry != "" {
			return out, errors.New("workload secret unavailable")
		}
		out.Env = append(out.Env, k+"="+string(value))
	}
	sort.Strings(out.Env)
	return out, nil
}
