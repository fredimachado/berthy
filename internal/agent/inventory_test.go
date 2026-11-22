package agent

import (
	"berthy/internal/api"
	"berthy/internal/deploy"
	"berthy/internal/engine"
	"berthy/internal/state"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
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

func TestInventoryLeavesOutWhatOtherEnvironmentsHold(t *testing.T) {
	e := inventoryDocker(t)
	d := &deploy.Controller{Foreign: func(context.Context) (deploy.Foreign, error) {
		return deploy.Foreign{Claims: map[state.Claim]bool{{Kind: state.ClaimService, Name: "foreign"}: true, {Kind: state.ClaimComposeProject, Name: "foreign-compose"}: true}}, nil
	}}
	apps, err := appInventory(context.Background(), e, d)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range apps {
		if a.Ref.Name == "foreign" || a.Ref.Name == "foreign-compose" {
			t.Fatalf("foreign app visible: %+v", apps)
		}
	}
	if len(apps) != 2 {
		t.Fatalf("unclaimed apps lost: %+v", apps)
	}
}

func TestInventoryFailsWhenTheOtherClaimsCannotBeRead(t *testing.T) {
	d := &deploy.Controller{Foreign: func(context.Context) (deploy.Foreign, error) { return deploy.Foreign{}, errors.New("unavailable") }}
	if apps, err := appInventory(context.Background(), inventoryDocker(t), d); err == nil {
		t.Fatalf("partial inventory: %+v", apps)
	}
}

func inventoryDocker(t *testing.T) *engine.Engine {
	t.Helper()
	dir, err := os.MkdirTemp("", "bty-inventory-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Skipf("unix socket: %v", err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/images/json"):
			fmt.Fprint(w, `[]`)
		case strings.HasSuffix(r.URL.Path, "/containers/json"):
			fmt.Fprint(w, `[{"Id":"a","Labels":{"com.docker.compose.project":"foreign-compose","com.docker.compose.service":"db"}},{"Id":"b","Labels":{"com.docker.compose.project":"unclaimed-compose","com.docker.compose.service":"db"}}]`)
		case strings.HasSuffix(r.URL.Path, "/services"):
			fmt.Fprint(w, `[{"ID":"a","Spec":{"Name":"foreign"}},{"ID":"b","Spec":{"Name":"unclaimed"}}]`)
		case strings.HasSuffix(r.URL.Path, "/tasks"):
			fmt.Fprint(w, `[]`)
		default:
			w.WriteHeader(404)
		}
	})}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })
	return engine.New(sock)
}
