package agent

import (
	"berthy/internal/api"
	"berthy/internal/state"
	"testing"
)

func TestLogicalInventoryPreservesForeignAndGroupsBindings(t *testing.T) {
	apps := []api.App{{Ref: api.AppRef{Kind: "swarm", Name: "legacy-web"}, Roles: []api.Role{{Name: "legacy-web", Running: 2}}}, {Ref: api.AppRef{Kind: "swarm", Name: "worker-svc"}, Roles: []api.Role{{Name: "worker-svc", Running: 1}}}, {Ref: api.AppRef{Kind: "swarm", Name: "foreign"}, Roles: []api.Role{{Name: "foreign"}}}, {Ref: api.AppRef{Kind: "compose", Name: "other"}}}
	bindings := []state.Binding{{App: "api", Role: "web", Name: "legacy-web", ServiceID: "a"}, {App: "api", Role: "worker", Name: "worker-svc", ServiceID: "b"}}
	out := logicalInventory(apps, bindings, map[string]string{"legacy-web": "a", "worker-svc": "b"})
	if len(out) != 3 {
		t.Fatalf("apps %v", out)
	}
	var image api.App
	foreign := false
	for _, a := range out {
		if a.Ref.Kind == "image" && a.Ref.Name == "api" {
			image = a
		}
		if a.Ref.Name == "foreign" {
			foreign = true
		}
	}
	if !foreign || len(image.Roles) != 2 || image.Roles[0].Name != "web" || image.Roles[0].Running != 2 || image.Roles[1].Name != "worker" {
		t.Fatalf("image %v foreign %v", image, foreign)
	}
}
func TestLogicalInventoryNeverBindsMismatchedServiceID(t *testing.T) {
	apps := []api.App{{Ref: api.AppRef{Kind: "swarm", Name: "svc"}, Roles: []api.Role{{Name: "svc"}}}}
	bindings := []state.Binding{{App: "app", Role: "web", Name: "svc", ServiceID: "expected"}}
	out := logicalInventory(apps, bindings, map[string]string{"svc": "foreign"})
	for _, a := range out {
		if a.Ref.Kind == "image" && len(a.Roles) > 0 && len(a.Roles[0].Replicas) > 0 {
			t.Fatal("bound foreign service")
		}
	}
	if len(out) != 2 {
		t.Fatalf("foreign inventory lost: %v", out)
	}
}
