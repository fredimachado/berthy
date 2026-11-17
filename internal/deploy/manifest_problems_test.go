package deploy

import (
	"context"
	"testing"

	"berthy/internal/manifest"
)

// A manifest with several problems is refused with the first of them, in the words the agent has always used.
func TestPlanRefusesAnInvalidManifestWithItsFirstProblem(t *testing.T) {
	m := dockerManifest(nil)
	app := m.Apps["web"]
	role := app.Roles["web"]
	role.Port = 70000
	app.Roles["web"] = role
	m.Backups = map[string]manifest.Backup{"data": {App: "web", Volume: "data", Destination: "nowhere"}}
	c := &Controller{Engine: &boundFake{}, Store: openStore(t), Environment: "test"}
	_, err := c.Plan(context.Background(), Actor{ID: "admin", Admin: true}, Request{RequestID: "plan", App: "web", Version: "v1", DryRun: true, Manifest: m})
	if err == nil || err.Error() != "app web role web: invalid settings" {
		t.Fatalf("got %v", err)
	}
}
