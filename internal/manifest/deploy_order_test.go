package manifest

import (
	"errors"
	"strings"
	"testing"
)

func TestUpdateOrderStopsARoleThatWritesAVolume(t *testing.T) {
	cases := []struct {
		name   string
		deploy Deploy
		role   Role
		order  string
		reason string
	}{
		{"no volume", Deploy{}, Role{}, StartFirst, ""},
		{"read-write volume", Deploy{}, Role{Volumes: []string{"uploads:/data"}}, StopFirst, "because it mounts volume uploads"},
		{"explicit read-write mode", Deploy{}, Role{Volumes: []string{"uploads:/data:rw"}}, StopFirst, "because it mounts volume uploads"},
		{"read-only volume", Deploy{}, Role{Volumes: []string{"uploads:/data:ro"}}, StartFirst, ""},
		{"the first read-write volume names the reason", Deploy{}, Role{Volumes: []string{"config:/etc/app:ro", "uploads:/data", "index:/idx"}}, StopFirst, "because it mounts volume uploads"},
		{"deploy.order start-first overrides a volume", Deploy{Order: StartFirst}, Role{Volumes: []string{"uploads:/data"}}, StartFirst, "set by deploy.order"},
		{"deploy.order stop-first overrides no volume", Deploy{Order: StopFirst}, Role{}, StopFirst, "set by deploy.order"},
	}
	for _, c := range cases {
		app := App{Kind: "image", Deploy: c.deploy, Roles: map[string]Role{"web": c.role}}
		order, reason := app.UpdateOrder("web")
		if order != c.order || reason != c.reason {
			t.Errorf("%s: got %q %q, want %q %q", c.name, order, reason, c.order, c.reason)
		}
	}
}

// orderManifest is a valid manifest whose web role mounts uploads read-write. The comments say the line each key is on.
const orderManifest = `project: demo
network: demo
apps:
  api:
    kind: image
    image: ghcr.io/acme/api
    deploy:
      order: start-first
    roles:
      web:
        port: 8080
        volumes:
          - uploads:/data
`

func warningsOf(t *testing.T, base string) Problems {
	t.Helper()
	dir := writeFiles(t, map[string]string{"berthy.yml": base})
	m, warnings, err := LoadWithWarnings(dir, "prod")
	if err != nil {
		t.Fatalf("a manifest with warnings is valid: %v", err)
	}
	if m == nil {
		t.Fatal("no manifest")
	}
	return warnings
}

func TestWarnsAboutStartFirstOverAWritableVolume(t *testing.T) {
	ws := warningsOf(t, orderManifest)
	if len(ws) != 1 {
		t.Fatalf("got %+v", []Problem(ws))
	}
	w := ws[0]
	if w.Path != "apps.api.deploy.order" || w.Message != "start-first lets two versions of role web write volume uploads at once" || placedAt(w) != [3]any{"berthy.yml", 8, 7} {
		t.Fatalf("got %+v", w)
	}
	if got := w.String(); got != "berthy.yml:8:7: apps.api.deploy.order: start-first lets two versions of role web write volume uploads at once" {
		t.Fatalf("printed as %q", got)
	}
}

func TestWarnsAboutReplicasSharingAWritableVolume(t *testing.T) {
	base := strings.Replace(orderManifest, "order: start-first", "order: stop-first", 1)
	base = strings.Replace(base, "        port: 8080\n", "        port: 8080\n        replicas: 3\n", 1)
	ws := warningsOf(t, base)
	if len(ws) != 1 {
		t.Fatalf("got %+v", []Problem(ws))
	}
	w := ws[0]
	if w.Path != "apps.api.roles.web" || w.Message != "3 replicas share volume uploads" || placedAt(w) != [3]any{"berthy.yml", 10, 7} {
		t.Fatalf("got %+v", w)
	}
	if got := w.String(); got != "berthy.yml:10:7: apps.api.roles.web: 3 replicas share volume uploads" {
		t.Fatalf("printed as %q", got)
	}
}

func TestStopFirstOverAWritableVolumeWithOneReplicaHasNoWarning(t *testing.T) {
	base := strings.Replace(orderManifest, "order: start-first", "order: stop-first", 1)
	if ws := warningsOf(t, base); len(ws) != 0 {
		t.Fatalf("got %+v", []Problem(ws))
	}
}

func TestReadOnlyVolumesNeverWarn(t *testing.T) {
	base := strings.Replace(orderManifest, "uploads:/data", "uploads:/data:ro", 1)
	base = strings.Replace(base, "        port: 8080\n", "        port: 8080\n        replicas: 3\n", 1)
	if ws := warningsOf(t, base); len(ws) != 0 {
		t.Fatalf("got %+v", []Problem(ws))
	}
}

func TestWarningsAreNotProblems(t *testing.T) {
	_, err := Load(writeFiles(t, map[string]string{"berthy.yml": orderManifest}), "prod")
	if err != nil {
		var ps Problems
		if errors.As(err, &ps) {
			t.Fatalf("a warning was reported as a problem: %+v", []Problem(ps))
		}
		t.Fatal(err)
	}
}

func TestDeployOrderIsCheckedAgainstTheTwoOrders(t *testing.T) {
	base := strings.Replace(orderManifest, "order: start-first", "order: sometimes", 1)
	ps := loadProblems(t, map[string]string{"berthy.yml": base})
	if len(ps) != 1 || ps[0].Path != "apps.api.deploy.order" || placedAt(ps[0]) != [3]any{"berthy.yml", 8, 7} {
		t.Fatalf("got %+v", []Problem(ps))
	}
}
