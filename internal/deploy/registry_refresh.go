package deploy

import (
	"berthy/internal/engine"
	"berthy/internal/state"
	"context"
	"errors"
	"sort"
	"time"
)

// RegistryRefresh says what happened to one service when a registry password was replaced.
type RegistryRefresh struct {
	Name    string `json:"name"`
	Updated bool   `json:"updated"`
	Err     string `json:"error,omitempty"` // set when Updated is false
}

// registryTarget is a bound service that pulls from a registry with a credential Berthy manages. fail is set instead of live
// and auth when the service is ours but Berthy cannot read it or build its credential.
type registryTarget struct {
	b    state.Binding
	live engine.ServiceSnapshot
	auth *engine.RegistryAuth
	fail string
}

const registryRefreshTimeout = 30 * time.Second

// registryTargets finds, sorted by service name, the bound Swarm services that pull from host. It is the one place that
// decides which services a replaced password concerns. A service Berthy cannot read is listed only when its app's own image
// in the applied manifest is on host, so a problem with an app of another registry is not reported here.
func (c *Controller) registryTargets(ctx context.Context, host string) ([]registryTarget, error) {
	if c.Store == nil {
		return nil, nil
	}
	bindings, err := c.Store.Bindings(ctx, "")
	if err != nil {
		return nil, err
	}
	sort.SliceStable(bindings, func(i, j int) bool { return bindings[i].Name < bindings[j].Name })
	var out []registryTarget
	for _, b := range bindings {
		m, compose, err := c.appliedCompose(ctx, b.App)
		if err != nil || compose {
			continue
		}
		if c.Engine == nil {
			return nil, errors.New("Docker unavailable")
		}
		live, err := c.Engine.InspectService(ctx, b.Name)
		if err != nil || live.ID != b.ServiceID {
			if h, e := imageHost(m.Apps[b.App].Image); e == nil && h == host {
				out = append(out, registryTarget{b: b, fail: "bound service unavailable"})
			}
			continue
		}
		auth, err := c.operationAuth(ctx, b.App, live.Spec)
		if err != nil {
			if h, e := serviceImageHost(live.Spec); e == nil && h == host {
				out = append(out, registryTarget{b: b, fail: "registry credential unavailable"})
			}
			continue
		}
		if auth != nil && auth.ServerAddress == host {
			out = append(out, registryTarget{b: b, live: live, auth: auth})
		}
	}
	return out, nil
}

// RegistryServices lists, sorted, the bound services that pull from host with a credential Berthy manages. It changes nothing.
func (c *Controller) RegistryServices(ctx context.Context, host string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, registryRefreshTimeout)
	defer cancel()
	targets, err := c.registryTargets(ctx, host)
	var names []string
	for _, t := range targets {
		names = append(names, t.b.Name)
	}
	return names, err
}

// RefreshRegistryAuth gives every bound service that pulls from host the password now in the store. It sends Docker the
// service's own current spec with the new credential; Swarm keeps the credential with the service and hands it to the node
// that starts a task. The same spec does not restart tasks. It is safe to repeat: a second run sends the same credential
// again. One service's failure never stops the others.
func (c *Controller) RefreshRegistryAuth(ctx context.Context, host string) ([]RegistryRefresh, error) {
	ctx, cancel := context.WithTimeout(ctx, registryRefreshTimeout)
	defer cancel()
	targets, err := c.registryTargets(ctx, host)
	if err != nil {
		return nil, err
	}
	var out []RegistryRefresh
	for _, t := range targets {
		r := RegistryRefresh{Name: t.b.Name, Err: t.fail}
		if t.fail == "" {
			r.Updated = c.refreshService(ctx, t, host)
			if !r.Updated {
				r.Err = "service update failed"
			}
		}
		out = append(out, r)
	}
	return out, nil
}

// refreshService sends the service's spec with its credential. When Docker rejects it, another writer such as a deploy may
// have changed the service meanwhile, so it reads the service again and tries once more with the version and spec it has now.
func (c *Controller) refreshService(ctx context.Context, t registryTarget, host string) bool {
	if c.Engine.UpdateService(ctx, t.live.ID, t.live.Version, t.live.Spec, t.auth) == nil {
		return true
	}
	live, err := c.Engine.InspectService(ctx, t.b.Name)
	if err != nil || live.ID != t.b.ServiceID {
		return false
	}
	auth, err := c.operationAuth(ctx, t.b.App, live.Spec)
	if err != nil || auth == nil || auth.ServerAddress != host {
		return false
	}
	return c.Engine.UpdateService(ctx, live.ID, live.Version, live.Spec, auth) == nil
}
