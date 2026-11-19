package deploy

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"berthy/internal/manifest"
)

const driftImage = "docker.io/example/web@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// driftDesired is the spec of a role at a release: two replicas, two variables.
func driftDesired(t *testing.T, role manifest.Role) json.RawMessage {
	t.Helper()
	spec, err := serviceSpec("test", "proj", "api", "web", "op-deploy", "net", driftImage, []string{"DATABASE_URL=postgres://secret", "MODE=fast"}, role, "start-first", "10s", nil, "json-file")
	if err != nil {
		t.Fatal(err)
	}
	return spec
}

func driftRole() manifest.Role {
	two := 2
	return manifest.Role{Replicas: &two, Health: &manifest.Health{Test: []string{"CMD", "true"}}}
}

// edited is the service as a person changed it with Docker: edit gets the decoded spec and its container spec.
func edited(t *testing.T, spec json.RawMessage, edit func(spec, container map[string]any)) json.RawMessage {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal(spec, &v); err != nil {
		t.Fatal(err)
	}
	task := v["TaskTemplate"].(map[string]any)
	edit(v, task["ContainerSpec"].(map[string]any))
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestRoleDriftIsEmptyForTheSpecTheReleaseProduces(t *testing.T) {
	want := driftDesired(t, driftRole())
	// Docker stores the spec with its own defaults, and every restart sets another operation label and the counter.
	live := withDockerDefaults(storedAsDocker(edited(t, want, func(spec, container map[string]any) {
		spec["Labels"].(map[string]any)["berthy.operation"] = "op-restart"
		container["Labels"].(map[string]any)["berthy.operation"] = "op-restart"
		spec["TaskTemplate"].(map[string]any)["ForceUpdate"] = 3
	})))
	lines, scaled := roleDrift("api/web", live, want, false)
	if len(lines) != 0 || scaled != "" {
		t.Fatalf("lines %q scaled %q", lines, scaled)
	}
}

func TestRoleDriftNamesAReplicaCountChangedByHand(t *testing.T) {
	want := driftDesired(t, driftRole())
	live := edited(t, want, func(spec, _ map[string]any) {
		spec["Mode"] = map[string]any{"Replicated": map[string]any{"Replicas": 3}}
	})
	lines, scaled := roleDrift("api/web", live, want, false)
	if !reflect.DeepEqual(lines, []string{"api/web: replicas 3, release says 2"}) || scaled != "" {
		t.Fatalf("lines %q scaled %q", lines, scaled)
	}
}

func TestRoleDriftNamesAnEnvironmentVariableWithoutItsValue(t *testing.T) {
	want := driftDesired(t, driftRole())
	live := edited(t, want, func(_, container map[string]any) {
		container["Env"] = []any{"DATABASE_URL=postgres://other-secret", "MODE=fast", "EXTRA=hand-value"}
	})
	lines, _ := roleDrift("api/web", live, want, false)
	got := strings.Join(lines, "\n")
	if got != "api/web: env DATABASE_URL, EXTRA differ" {
		t.Fatalf("lines %q", lines)
	}
	for _, secret := range []string{"secret", "hand-value", "fast"} {
		if strings.Contains(got, secret) {
			t.Fatalf("a value is shown: %q", lines)
		}
	}
}

func TestRoleDriftNamesAUserAddedByHandAndOneTheReleaseSets(t *testing.T) {
	want := driftDesired(t, driftRole())
	live := edited(t, want, func(_, container map[string]any) { container["User"] = "1000" })
	lines, _ := roleDrift("api/web", live, want, false)
	if !reflect.DeepEqual(lines, []string{`api/web: user "1000", release says none`}) {
		t.Fatalf("lines %q", lines)
	}
	role := driftRole()
	role.User = "1000"
	lines, _ = roleDrift("api/web", want, driftDesired(t, role), false)
	if !reflect.DeepEqual(lines, []string{`api/web: user none, release says "1000"`}) {
		t.Fatalf("lines %q", lines)
	}
}

func TestRoleDriftShowsAnImageByItsDigest(t *testing.T) {
	want := driftDesired(t, driftRole())
	live := edited(t, want, func(_, container map[string]any) {
		container["Image"] = "docker.io/example/web:v2@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	})
	lines, _ := roleDrift("api/web", live, want, false)
	line := "api/web: image docker.io/example/web:v2@sha256:bbbbbbbbbbbb…, release says docker.io/example/web@sha256:aaaaaaaaaaaa…"
	if !reflect.DeepEqual(lines, []string{line}) {
		t.Fatalf("lines %q", lines)
	}
}

func TestRoleDriftReportsAScaledRoleAsScaled(t *testing.T) {
	want := driftDesired(t, driftRole())
	live := edited(t, want, func(spec, _ map[string]any) {
		spec["Mode"] = map[string]any{"Replicated": map[string]any{"Replicas": 3}}
	})
	lines, scaled := roleDrift("api/web", live, want, true)
	if len(lines) != 0 || scaled != "api/web: scaled to 3 with berthy scale" {
		t.Fatalf("lines %q scaled %q", lines, scaled)
	}
	// Scaling explains the replica count only. A change of anything else is still drift.
	live = edited(t, live, func(_, container map[string]any) { container["User"] = "1000" })
	lines, scaled = roleDrift("api/web", live, want, true)
	if !reflect.DeepEqual(lines, []string{`api/web: user "1000", release says none`}) || scaled == "" {
		t.Fatalf("lines %q scaled %q", lines, scaled)
	}
}

func TestRoleDriftIgnoresAScaleThatLeftTheReleaseCount(t *testing.T) {
	want := driftDesired(t, driftRole())
	lines, scaled := roleDrift("api/web", want, want, true)
	if len(lines) != 0 || scaled != "" {
		t.Fatalf("lines %q scaled %q", lines, scaled)
	}
}
