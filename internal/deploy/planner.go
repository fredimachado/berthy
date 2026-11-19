package deploy

import (
	"berthy/internal/state"
	"context"
	"encoding/json"
	"errors"
	"sort"

	"berthy/internal/engine"
	"berthy/internal/manifest"
	"github.com/distribution/reference"
	digest "github.com/opencontainers/go-digest"
)

type preparedPlan struct {
	Request   Request
	Image     string
	NetworkID string
	Auth      *engine.RegistryAuth
	Env       []string
	// Identity is the BERTHY_* values of the app's containers; Env stays the app's own variables.
	Identity identity
	// LogDriver is the daemon's default log driver, which decides the rotation of a role that sets no logging.
	LogDriver string
}

// identity is what an image app's container knows about itself, as the BERTHY_* variables. It holds only values that
// change with the image or never, so a deploy that keeps the version gives its containers the same variables.
type identity struct {
	Environment, Project, App, Version, Digest string
}

// env returns the BERTHY_* variables of a container of role, sorted. role is "" for a one-off container, which belongs to
// the app rather than a role; BERTHY_ROLE is then set and empty.
func (i identity) env(role string) []string {
	vars := []string{
		"BERTHY_APP=" + i.App,
		"BERTHY_ENVIRONMENT=" + i.Environment,
		"BERTHY_IMAGE_DIGEST=" + i.Digest,
		"BERTHY_PROJECT=" + i.Project,
		"BERTHY_ROLE=" + role,
		"BERTHY_VERSION=" + i.Version,
	}
	sort.Strings(vars)
	return vars
}

// containerEnv is the full environment of a container of role: the app's own variables and the BERTHY_* ones, sorted.
func (p preparedPlan) containerEnv(role string) []string {
	env := append(append([]string(nil), p.Env...), p.Identity.env(role)...)
	sort.Strings(env)
	return env
}

// logDriverEngine reads the daemon's default log driver. An engine without it gets no default rotation.
type logDriverEngine interface {
	LoggingDriver(context.Context) (string, error)
}

// daemonLogDriver is the daemon's default log driver, or "" when the engine cannot tell. A failed read is an error: a
// deploy must not silently drop the rotation.
func (c *Controller) daemonLogDriver(ctx context.Context) (string, error) {
	le, ok := c.Engine.(logDriverEngine)
	if !ok {
		return "", nil
	}
	d, err := le.LoggingDriver(ctx)
	if err != nil {
		return "", errors.New("Docker log driver unavailable")
	}
	return d, nil
}

// requestedTag returns the tag of a resolved image reference ("ghcr.io/x/y:v1.0.2" -> "v1.0.2"), or "" when the reference has
// none, as a digest has none. The tag is a label only: the digest is what runs.
func requestedTag(image string) string {
	ref, err := reference.ParseNormalizedNamed(image)
	if err != nil {
		return ""
	}
	if t, ok := ref.(reference.NamedTagged); ok {
		return t.Tag()
	}
	return ""
}

func (c *Controller) prepare(ctx context.Context, req Request, image string) (Plan, error) {
	// The tag is read from the requested image: prepare pins p.Image to its digest below.
	p := Plan{App: req.App, Image: image, Tag: requestedTag(image)}
	app := req.Manifest.Apps[req.App]
	for role := range app.Roles {
		p.Roles = append(p.Roles, role)
	}
	sort.Strings(p.Roles)
	for _, role := range p.Roles {
		if order, reason := app.UpdateOrder(role); order == manifest.StopFirst {
			p.Notes = append(p.Notes, "role "+role+": "+order+", "+reason)
		}
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
	for _, name := range app.Env.SecretNames() {
		if _, err := c.workloadSecret(ctx, name); err != nil {
			return Plan{}, err
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
	if req.DryRun {
		wants, err := c.routeWants(ctx, req.Manifest, []string{req.App})
		if err != nil {
			return Plan{}, err
		}
		if err = c.dryRunPublish(ctx, &p, req.Manifest, req.App, wants, req.ReplaceExisting); err != nil {
			return Plan{}, err
		}
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
	// prepare pins every image, so a reference without a digest is an error rather than an empty BERTHY_IMAGE_DIGEST.
	canonical, ok := named.(reference.Canonical)
	if !ok {
		return out, errors.New("image is not pinned to a digest")
	}
	out.Identity = identity{Environment: c.Environment, Project: req.Manifest.Project, App: req.App, Version: p.Tag, Digest: canonical.Digest().String()}
	host := reference.Domain(named)
	if reg, ok := req.Manifest.Registries[host]; ok {
		info, value, e := c.Store.Secret(ctx, reg.PasswordSecret)
		if e != nil || info.Registry != host {
			return out, errors.New("registry credential unavailable")
		}
		out.Auth = &engine.RegistryAuth{Username: reg.Username, Password: string(value), ServerAddress: host}
	}
	app := req.Manifest.Apps[req.App]
	vars, err := c.resolveEnv(ctx, app.Env)
	if err != nil {
		return out, err
	}
	for _, v := range vars {
		out.Env = append(out.Env, v.Name+"="+v.Value)
	}
	sort.Strings(out.Env)
	logDriver, err := c.daemonLogDriver(ctx)
	if err != nil {
		return out, err
	}
	out.LogDriver = logDriver
	return out, nil
}

// planAdoption inspects existing services without creating, updating, or pulling anything.
func (c *Controller) planAdoption(ctx context.Context, req Request) (Plan, []state.Binding, error) {
	if err := req.Manifest.Validate(); err != nil {
		return Plan{}, nil, err
	}
	app, ok := req.Manifest.Apps[req.App]
	if !ok || app.Kind != "image" {
		return Plan{}, nil, errors.New("image app required")
	}
	if c.Engine == nil {
		return Plan{}, nil, errors.New("Docker unavailable")
	}
	net, err := c.Engine.InspectNetwork(ctx, req.Manifest.Network)
	if err != nil || net.Driver != "overlay" || net.Scope != "swarm" || !net.Attachable {
		return Plan{}, nil, errors.New("overlay network unavailable")
	}
	daemonDriver, err := c.daemonLogDriver(ctx)
	if err != nil {
		return Plan{}, nil, err
	}
	expected, err := reference.ParseNormalizedNamed(app.Image)
	if err != nil {
		return Plan{}, nil, err
	}
	var existing []state.Binding
	if c.Store != nil {
		existing, err = c.Store.Bindings(ctx, "")
		if err != nil {
			return Plan{}, nil, err
		}
	}
	roles := make([]string, 0, len(app.Roles))
	for role := range app.Roles {
		roles = append(roles, role)
	}
	sort.Strings(roles)
	p := Plan{App: req.App, Roles: roles}
	bindings := make([]state.Binding, 0, len(roles))
	for _, role := range roles {
		r := app.Roles[role]
		if r.Service == "" {
			return Plan{}, nil, errors.New("adoption requires a service name for every role")
		}
		live, e := c.Engine.InspectService(ctx, r.Service)
		if e != nil || live.ID == "" {
			return Plan{}, nil, errors.New("adoption service unavailable")
		}
		for _, b := range existing {
			if (b.Name == r.Service || b.ServiceID == live.ID) && (b.App != req.App || b.Role != role) {
				return Plan{}, nil, errors.New("service binding conflict")
			}
		}
		var spec struct {
			Name         string
			TaskTemplate struct {
				ContainerSpec struct {
					Image       string
					Healthcheck json.RawMessage
					Env         []string
				}
			}
			Mode struct{ Replicated *struct{ Replicas int } }
		}
		if json.Unmarshal(live.Spec, &spec) != nil || spec.Name != r.Service || spec.Mode.Replicated == nil || spec.Mode.Replicated.Replicas < 1 {
			return Plan{}, nil, errors.New("incompatible adoption service")
		}
		img, e := reference.ParseNormalizedNamed(spec.TaskTemplate.ContainerSpec.Image)
		if e != nil {
			return Plan{}, nil, errors.New("running image identity unavailable")
		}
		if _, ok := img.(reference.Canonical); !ok || reference.TrimNamed(img).Name() != reference.TrimNamed(expected).Name() {
			return Plan{}, nil, errors.New("running image must be a pinned digest from the configured repository")
		}
		if p.Image != "" && p.Image != img.String() {
			return Plan{}, nil, errors.New("roles run different image digests")
		}
		p.Image = img.String()
		// The release's spec of the role, which the first deploy sends. It runs the image the service runs now, so that the
		// image is not a difference, and takes the service's own values for the secret variables, which are read at deploy.
		order, _ := app.UpdateOrder(role)
		desired, e := serviceSpec(c.Environment, req.Manifest.Project, req.App, role, "adoption", net.ID, spec.TaskTemplate.ContainerSpec.Image, adoptionEnv(app.Env, spec.TaskTemplate.ContainerSpec.Env), r, order, func() string {
			if app.Deploy.DrainTimeout == "" {
				return "30s"
			}
			return app.Deploy.DrainTimeout
		}(), spec.TaskTemplate.ContainerSpec.Healthcheck, daemonDriver)
		if e != nil {
			return Plan{}, nil, errors.New("adoption health configuration unavailable")
		}
		if desired, e = renamed(desired, r.Service); e != nil {
			return Plan{}, nil, errors.New("invalid desired role")
		}
		if e = adoptableService(live.Spec, net.ID); e != nil {
			return Plan{}, nil, errors.New("unsafe or incompatible adoption service")
		}
		p.Changes = append(p.Changes, specChanges(role, live.Spec, desired)...)
		tasks, e := c.Engine.ServiceTasks(ctx, live.ID)
		if e != nil {
			return Plan{}, nil, errors.New("adoption health unavailable")
		}
		slots := map[uint64]bool{}
		for _, t := range tasks {
			if t.ServiceID == live.ID && t.DesiredState == "running" && t.State == "running" && t.Slot > 0 && t.ContainerID != "" {
				health, e := c.Engine.InspectContainerHealth(ctx, t.ContainerID)
				if e == nil && health == "healthy" {
					slots[t.Slot] = true
				}
			}
		}
		if !slotsCover(slots, spec.Mode.Replicated.Replicas) {
			return Plan{}, nil, errors.New("adoption service is not healthy")
		}
		bindings = append(bindings, state.Binding{App: req.App, Role: role, Name: r.Service, ServiceID: live.ID})
	}
	if req.Version != "" {
		selected, e := req.Manifest.CheckImageDeploy(req.App, req.Version, true)
		if e != nil || selected != p.Image {
			return Plan{}, nil, errors.New("adoption selector must match the running digest")
		}
	}
	return p, bindings, nil
}
