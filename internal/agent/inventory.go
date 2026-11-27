package agent

import (
	"context"
	"errors"
	"slices"
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
	onlyOwned := false
	if d != nil && d.Foreign != nil {
		foreign, err := d.Foreign(ctx)
		if err != nil {
			if d.Store == nil {
				return nil, err
			}
			onlyOwned = true
		} else {
			apps = slices.DeleteFunc(apps, func(app api.App) bool { return foreign.Claims[inventoryClaim(app)] })
		}
	}
	if d == nil || d.Store == nil {
		return apps, nil
	}
	bindings, err := d.Store.Bindings(ctx, "")
	if err != nil {
		return nil, err
	}
	listedIDs := make(map[string]string, len(bindings))
	for _, app := range apps {
		if app.Ref.Kind == "swarm" {
			listedIDs[app.Ref.Name] = app.ServiceID
		}
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
		if live.ID == listedIDs[b.Name] {
			ids[b.Name] = live.ID
		}
	}
	if onlyOwned {
		claims, err := d.Store.Claims(ctx)
		if err != nil {
			return nil, err
		}
		owned := make(map[state.Claim]bool, len(claims)+len(bindings))
		for _, claim := range claims {
			owned[claim] = true
		}
		// A binding requires an ID match, even when the service name is also claimed.
		for _, b := range bindings {
			owned[state.Claim{Kind: state.ClaimService, Name: b.Name}] = ids[b.Name] == b.ServiceID && b.ServiceID != ""
		}
		apps = slices.DeleteFunc(apps, func(app api.App) bool { return !owned[inventoryClaim(app)] })
	}
	return logicalInventory(apps, bindings, ids), nil
}

func inventoryClaim(app api.App) state.Claim {
	claim := state.Claim{Name: app.Ref.Name}
	switch app.Ref.Kind {
	case "swarm":
		claim.Kind = state.ClaimService
	case "compose":
		claim.Kind = state.ClaimComposeProject
	}
	return claim
}
