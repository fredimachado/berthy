package manifest

import (
	"encoding/json"
	"errors"
	"slices"
	"testing"
)

// uncheckedManifest decodes JSON into a Manifest without validating it, as Check is given one.
func uncheckedManifest(t *testing.T, data []byte) *Manifest {
	t.Helper()
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	return &m
}

func TestCheckReportsEveryProblem(t *testing.T) {
	m := uncheckedManifest(t, brokenManifest(t,
		"apps.api.roles.web.port", float64(70000),
		"backups.data.destination", "nope"))
	got := m.Check()
	want := Problems{
		{Path: "apps.api.roles.web.port", Message: "app api role web: invalid settings"},
		{Path: "backups.data.destination", Message: "backup data: unknown destination"},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
	err := m.Validate()
	if err == nil || err.Error() != "app api role web: invalid settings" {
		t.Fatalf("Validate: %v", err)
	}
	var ps Problems
	if !errors.As(err, &ps) || len(ps) != 2 {
		t.Fatalf("Validate does not carry the problems: %#v", err)
	}
}

func TestAValidManifestHasNoProblems(t *testing.T) {
	m := uncheckedManifest(t, brokenManifest(t))
	if got := m.Check(); len(got) != 0 {
		t.Fatalf("got %+v", got)
	}
	if err := m.Validate(); err != nil {
		t.Fatal(err)
	}
}

// Every mistake of the pinned cases says where it is, and the decoder gives the same paths as Check.
func TestEveryProblemHasAPath(t *testing.T) {
	for _, c := range firstMessageCases() {
		t.Run(c.name, func(t *testing.T) {
			_, err := DecodeJSON(brokenManifest(t, c.edits...))
			var ps Problems
			if !errors.As(err, &ps) || len(ps) == 0 {
				t.Fatalf("not Problems: %#v", err)
			}
			for _, p := range ps {
				if p.Path == "" {
					t.Errorf("no path: %+v", p)
				}
			}
		})
	}
}

// A mistake is reported where it is, once: what depends on a broken value is not checked again.
func TestCheckDoesNotCascade(t *testing.T) {
	cases := []struct {
		name  string
		edits []any
		paths []string
	}{
		{"a bad kind", []any{"apps.api.kind", "vm", "apps.api.roles.web.hostnames", []any{"a.example.com"}}, []string{"apps.api.kind"}},
		{"an app that depends on itself", []any{"apps.api.depends_on", []any{"api"}}, []string{"apps.api.depends_on"}},
		{"an unknown dependency", []any{"apps.api.depends_on", []any{"nope"}}, []string{"apps.api.depends_on"}},
		{"a backup of an unknown app", []any{"backups.data.app", "nope"}, []string{"backups.data.app"}},
		{"a job of an unknown app", []any{"jobs.seed.app", "nope"}, []string{"jobs.seed.app"}},
		{"a probe of nothing", []any{"probes", `json:{"p":{}}`}, []string{"probes.p"}},
		{"no apps", []any{"apps", "json:{}", "jobs", nil, "backups", nil}, []string{"apps"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var got []string
			for _, p := range uncheckedManifest(t, brokenManifest(t, c.edits...)).Check() {
				got = append(got, p.Path)
			}
			if !slices.Equal(got, c.paths) {
				t.Fatalf("got %v, want %v", got, c.paths)
			}
		})
	}
}

func TestCheckFindsSeveralMistakesOfOneApp(t *testing.T) {
	m := uncheckedManifest(t, brokenManifest(t,
		"apps.api.project_name", "Bad Name",
		"apps.api.deploy.deploy_timeout", "soon",
		"apps.api.deploy.retain", float64(0),
		"apps.api.roles.web.port", float64(70000),
		"apps.api.roles.web.replicas", float64(0)))
	var got []string
	for _, p := range m.Check() {
		got = append(got, p.Path)
	}
	want := []string{"apps.api.project_name", "apps.api.deploy.deploy_timeout", "apps.api.deploy.retain", "apps.api.roles.web.replicas", "apps.api.roles.web.port"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestProblemString(t *testing.T) {
	cases := []struct {
		p    Problem
		want string
	}{
		{Problem{Path: "apps.api.roles.web.port", Message: "app api role web: invalid settings", File: "berthy.prod.yml", Line: 12, Column: 7}, "berthy.prod.yml:12:7: apps.api.roles.web.port: app api role web: invalid settings"},
		{Problem{Path: "apps", Message: "at least one app is required", File: "berthy.yml"}, "berthy.yml: apps: at least one app is required"},
		{Problem{Path: "apps.api.depends_on", Message: "apps.api.depends_on: unknown app \"x\"", File: "berthy.yml", Line: 5, Column: 3}, "berthy.yml:5:3: apps.api.depends_on: unknown app \"x\""},
		{Problem{Path: "alerts.telegram.chat_id", Message: "alerts.telegram: chat_id must be a numeric chat ID or a @channel name", File: "berthy.yml", Line: 5, Column: 3}, "berthy.yml:5:3: alerts.telegram: chat_id must be a numeric chat ID or a @channel name"},
		{Problem{Message: "invalid JSON manifest"}, "invalid JSON manifest"},
		{Problem{Path: "wat", Message: "unknown field \"wat\"", File: "berthy.yml", Line: 3}, "berthy.yml:3: wat: unknown field \"wat\""},
	}
	for _, c := range cases {
		if got := c.p.String(); got != c.want {
			t.Errorf("got %q, want %q", got, c.want)
		}
	}
}
