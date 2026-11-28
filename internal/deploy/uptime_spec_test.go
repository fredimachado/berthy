package deploy

import (
	"context"
	"encoding/json"
	"testing"

	"berthy/internal/manifest"
	"berthy/internal/state"
)

func uptimeManifest(threshold, budget string) manifest.Manifest {
	m := dockerManifest(nil)
	app := m.Apps["web"]
	app.Deploy.DeployTimeout = budget
	role := app.Roles["web"]
	role.Health = &manifest.Health{Uptime: &threshold}
	app.Roles["web"] = role
	m.Apps["web"] = app
	return m
}

func runUptimeRequest(t *testing.T, c *Controller, req Request) state.Operation {
	t.Helper()
	ctx := context.Background()
	op, _, err := c.Admit(ctx, Actor{ID: "admin", Admin: true}, req)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	op, err = c.Store.Operation(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	return op
}

func seedUptimeRelease(t *testing.T, s *state.Store, m manifest.Manifest, bindings ...state.Binding) state.Operation {
	t.Helper()
	ctx := context.Background()
	prior, _, err := s.Admit(ctx, state.Admission{RequestID: "prior", ActorID: "admin", Kind: "deploy", App: "web", PublicRequest: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Finish(ctx, prior.ID, state.Outcome{State: "succeeded", Release: &state.Release{App: "web", Digest: "docker.io/example/web@" + digestOfLetter("a"), Manifest: raw, Bindings: bindings}}); err != nil {
		t.Fatal(err)
	}
	return prior
}

func TestUptimeRoleDisablesDockerHealthcheck(t *testing.T) {
	role := uptimeManifest("20s", "1m").Apps["web"].Roles["web"]
	for _, tc := range []struct {
		name        string
		imageHealth json.RawMessage
		volumes     []string
		mounts      string
	}{
		{name: "no image check"},
		{name: "image check", imageHealth: json.RawMessage(`{"Test":["CMD","false"]}`)},
		{name: "named volume", volumes: []string{"data:/.berthy"}, mounts: `[{"Type":"volume","Source":"data","Target":"/.berthy"}]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			role.Volumes = tc.volumes
			spec, err := serviceSpec("test", "proj", "web", "worker", "service", "operation", "net", "image", func(s string) string { return s }, nil, role, "start-first", "1s", tc.imageHealth, "")
			if err != nil {
				t.Fatal(err)
			}
			var stored struct {
				TaskTemplate struct {
					ContainerSpec struct{ Mounts, Healthcheck json.RawMessage }
				}
			}
			if err := json.Unmarshal(storedAsDocker(spec), &stored); err != nil {
				t.Fatal(err)
			}
			container := stored.TaskTemplate.ContainerSpec
			if !sameSpec(container.Healthcheck, json.RawMessage(`{"Test":["NONE"]}`)) || (tc.mounts == "" && len(container.Mounts) != 0) || (tc.mounts != "" && !sameSpec(container.Mounts, json.RawMessage(tc.mounts))) {
				t.Fatalf("stored health %s, mounts %s, want disabled health and %s", container.Healthcheck, container.Mounts, tc.mounts)
			}
		})
	}
}
