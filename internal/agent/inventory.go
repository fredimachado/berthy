package agent

import (
	"context"
	"errors"
	"sort"

	"berthy/internal/api"
	"berthy/internal/deploy"
	"berthy/internal/engine"
	"berthy/internal/state"
)

func logicalInventory(apps []api.App, bindings []state.Binding, ids map[string]string) []api.App {
	grouped := map[string]*api.App{}
	managed := map[string]bool{}
	byName := map[string]api.App{}
	for _, a := range apps {
		if a.Ref.Kind == "swarm" {
			byName[a.Ref.Name] = a
		}
	}
	for _, b := range bindings {
		a := grouped[b.App]
		if a == nil {
			a = &api.App{Ref: api.AppRef{Kind: "image", Name: b.App}, Roles: []api.Role{}}
			grouped[b.App] = a
		}
		role := api.Role{Name: b.Role, Replicas: []api.Replica{}}
		if ids[b.Name] == b.ServiceID && b.ServiceID != "" {
			if source, ok := byName[b.Name]; ok && len(source.Roles) == 1 {
				role = source.Roles[0]
				role.Name = b.Role
				managed[b.Name] = true
			}
		}
		a.Roles = append(a.Roles, role)
	}
	out := make([]api.App, 0, len(apps)+len(grouped))
	for _, a := range apps {
		if a.Ref.Kind != "swarm" || !managed[a.Ref.Name] {
			out = append(out, a)
		}
	}
	for _, a := range grouped {
		sort.Slice(a.Roles, func(i, j int) bool { return a.Roles[i].Name < a.Roles[j].Name })
		out = append(out, *a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ref.Kind+":"+out[i].Ref.Name < out[j].Ref.Kind+":"+out[j].Ref.Name })
	return out
}
func appInventory(ctx context.Context, e *engine.Engine, d *deploy.Controller) ([]api.App, error) {
	apps, err := e.Apps(ctx)
	if err != nil {
		return nil, err
	}
	if d == nil || d.Store == nil {
		return apps, nil
	}
	bindings, err := d.Store.Bindings(ctx, "")
	if err != nil {
		return nil, err
	}
	ids := map[string]string{}
	for _, b := range bindings {
		live, err := e.InspectService(ctx, b.Name)
		if err != nil {
			var status *engine.StatusError
			if errors.As(err, &status) && status.Code == 404 {
				continue
			}
			return nil, err
		}
		ids[b.Name] = live.ID
	}
	return logicalInventory(apps, bindings, ids), nil
}
