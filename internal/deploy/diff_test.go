package deploy

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"berthy/internal/manifest"
)

const (
	oldDigest = "ghcr.io/acme/api@sha256:41ab1c2d3e4f5a6b7c8d9e0f1a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d"
	newDigest = "ghcr.io/acme/api@sha256:9f2c0d1e2f3a4b5c6d7e8f9a0b1c2d3e4f5a6b7c8d9e0f1a2b3c4d5e6f708192"
)

// apiManifest declares one image app, api, with a web role, and edit changes it.
func apiManifest(edit func(*manifest.Manifest, *manifest.App)) manifest.Manifest {
	two := 2
	m := manifest.Manifest{Project: "proj", Network: "overlay", Apps: map[string]manifest.App{}}
	app := manifest.App{
		Kind:  "image",
		Image: "ghcr.io/acme/api",
		Env:   manifest.Env{Plain: map[string]string{"LEGACY_MODE": "on", "LOG_LEVEL": "info"}},
		Roles: map[string]manifest.Role{"web": {Replicas: &two, Port: 8080, Health: &manifest.Health{Test: []string{"CMD", "true"}, StartPeriod: "30s"}}},
	}
	if edit != nil {
		edit(&m, &app)
	}
	m.Apps["api"] = app
	return m
}

func side(m manifest.Manifest, image, tag string) *ReleaseSide {
	return &ReleaseSide{Manifest: m, Image: image, Tag: tag}
}

func changesOf(d Diff) []string {
	var out []string
	for _, c := range d.Changes {
		out = append(out, c.Area+": "+c.Change)
	}
	return out
}

func TestReleaseDiffListsExactlyTheImageTheReplicasAndTheVariable(t *testing.T) {
	prev := side(apiManifest(nil), oldDigest, "v1.4.2")
	next := side(apiManifest(func(_ *manifest.Manifest, a *manifest.App) {
		three := 3
		r := a.Roles["web"]
		r.Replicas = &three
		a.Roles["web"] = r
		a.Env.Plain = map[string]string{"LEGACY_MODE": "on", "LOG_LEVEL": "info", "FEATURE_X": "1"}
	}), newDigest, "v1.4.3")
	d := ReleaseDiff("api", prev, next)
	want := []string{
		"image: sha256:41ab1c2d3e4f… -> sha256:9f2c0d1e2f3a…   (v1.4.2 -> v1.4.3)",
		"web: replicas 2 -> 3",
		"env: + FEATURE_X (plain)",
	}
	if got := changesOf(d); !reflect.DeepEqual(got, want) {
		t.Fatalf("changes\n got %q\nwant %q", got, want)
	}
	if d.New || d.Same {
		t.Fatalf("diff %+v", d)
	}
	lines := d.Lines()
	wantLines := []string{
		"image    sha256:41ab1c2d3e4f… -> sha256:9f2c0d1e2f3a…   (v1.4.2 -> v1.4.3)",
		"web      replicas 2 -> 3",
		"env      + FEATURE_X (plain)",
	}
	if !reflect.DeepEqual(lines, wantLines) {
		t.Fatalf("lines\n got %q\nwant %q", lines, wantLines)
	}
}

func TestReleaseDiffOfAFirstDeployIsANewApp(t *testing.T) {
	d := ReleaseDiff("api", nil, side(apiManifest(nil), newDigest, "v1"))
	if !d.New || d.Same || len(d.Changes) != 0 {
		t.Fatalf("diff %+v", d)
	}
	if got := d.Lines(); !reflect.DeepEqual(got, []string{"new app"}) {
		t.Fatalf("lines %q", got)
	}
}

func TestReleaseDiffOfTheSameReleaseSaysNothingChanges(t *testing.T) {
	d := ReleaseDiff("api", side(apiManifest(nil), oldDigest, "v1"), side(apiManifest(nil), oldDigest, "v1"))
	if !d.Same || d.New || len(d.Changes) != 0 {
		t.Fatalf("diff %+v", d)
	}
	if got := d.Lines(); !reflect.DeepEqual(got, []string{"no changes: the same release would be recorded"}) {
		t.Fatalf("lines %q", got)
	}
}

func TestReleaseDiffIgnoresWhatOtherAppsAndTheEnvironmentHold(t *testing.T) {
	prev := apiManifest(nil)
	next := apiManifest(func(m *manifest.Manifest, _ *manifest.App) {
		m.Network = "other"
		m.Peers = map[string]string{"b": "https://b"}
		m.Apps["worker"] = manifest.App{Kind: "image", Image: "x/y"}
		m.Jobs = map[string]manifest.Job{"other": {App: "worker", Command: []string{"x"}}}
	})
	if d := ReleaseDiff("api", side(prev, oldDigest, "v1"), side(next, oldDigest, "v1")); !d.Same {
		t.Fatalf("diff %+v", d)
	}
}

func TestReleaseDiffComparesAfterDefaults(t *testing.T) {
	// A release stored before a default existed, and a request that has the default, run the same thing.
	prev := apiManifest(nil)
	next := apiManifest(func(_ *manifest.Manifest, a *manifest.App) {
		a.Deploy.DeployTimeout = "120s"
		a.Deploy.DrainTimeout = "30s"
	})
	if d := ReleaseDiff("api", side(prev, oldDigest, "v1"), side(next, oldDigest, "v1")); !d.Same {
		t.Fatalf("diff %+v", d)
	}
}

func TestReleaseDiffNamesSecretsButNeverTheirValues(t *testing.T) {
	prev := apiManifest(func(_ *manifest.Manifest, a *manifest.App) {
		a.Env.Secret = []manifest.SecretVar{{Var: "DATABASE_URL", Secret: "DATABASE_URL"}, {Var: "OLD_KEY", Secret: "OLD_KEY"}}
	})
	next := apiManifest(func(_ *manifest.Manifest, a *manifest.App) {
		a.Env.Secret = []manifest.SecretVar{{Var: "DATABASE_URL", Secret: "API_DATABASE_URL"}, {Var: "NEW_KEY", Secret: "NEW_KEY"}}
	})
	d := ReleaseDiff("api", side(prev, oldDigest, "v1"), side(next, oldDigest, "v1"))
	want := []string{
		"env: ~ DATABASE_URL now from secret API_DATABASE_URL",
		"env: + NEW_KEY (secret)",
		"env: - OLD_KEY (secret)",
	}
	if got := changesOf(d); !reflect.DeepEqual(got, want) {
		t.Fatalf("changes\n got %q\nwant %q", got, want)
	}
}

func TestReleaseDiffShowsTemplatesByNameWithTheirText(t *testing.T) {
	prev := apiManifest(func(_ *manifest.Manifest, a *manifest.App) {
		a.Env.Template = map[string]string{"DSN": "postgres://app:${DB_PASSWORD}@db/app", "GONE": "${A}"}
	})
	next := apiManifest(func(_ *manifest.Manifest, a *manifest.App) {
		a.Env.Template = map[string]string{"DSN": "postgres://app:${DB_PASSWORD}@db/app2", "ADDED": "${B}"}
	})
	d := ReleaseDiff("api", side(prev, oldDigest, "v1"), side(next, oldDigest, "v1"))
	want := []string{
		`env: + ADDED (template)`,
		`env: ~ DSN (template) "postgres://app:${DB_PASSWORD}@db/app" -> "postgres://app:${DB_PASSWORD}@db/app2"`,
		`env: - GONE (template)`,
	}
	if got := changesOf(d); !reflect.DeepEqual(got, want) {
		t.Fatalf("changes\n got %q\nwant %q", got, want)
	}
}

func TestReleaseDiffNeverCarriesASecretValueOfTheStore(t *testing.T) {
	const value = "hunter2-very-secret"
	prev := apiManifest(func(_ *manifest.Manifest, a *manifest.App) {
		a.Env.Secret = []manifest.SecretVar{{Var: "DB_PASSWORD", Secret: "DB_PASSWORD"}}
	})
	next := apiManifest(func(_ *manifest.Manifest, a *manifest.App) {
		a.Env.Secret = []manifest.SecretVar{{Var: "DB_PASSWORD", Secret: "DB_PASSWORD_V2"}, {Var: "TOKEN", Secret: "TOKEN"}}
		a.Env.Template = map[string]string{"DSN": "u:${DB_PASSWORD_V2}@h"}
	})
	d := ReleaseDiff("api", side(prev, oldDigest, "v1"), side(next, newDigest, "v2"))
	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	// The diff is built from the manifests alone, so no value of the secret store can reach it: its text holds only names.
	for _, text := range append(d.Lines(), string(raw)) {
		if strings.Contains(text, value) {
			t.Fatalf("a secret value is in %q", text)
		}
	}
	if len(d.Changes) == 0 {
		t.Fatal("expected changes")
	}
}

func TestReleaseDiffChangedPlainValueIsShown(t *testing.T) {
	next := apiManifest(func(_ *manifest.Manifest, a *manifest.App) {
		a.Env.Plain = map[string]string{"LOG_LEVEL": "debug"}
	})
	d := ReleaseDiff("api", side(apiManifest(nil), oldDigest, "v1"), side(next, oldDigest, "v1"))
	want := []string{"env: - LEGACY_MODE (plain)", `env: ~ LOG_LEVEL (plain) "info" -> "debug"`}
	if got := changesOf(d); !reflect.DeepEqual(got, want) {
		t.Fatalf("changes\n got %q\nwant %q", got, want)
	}
}

func TestReleaseDiffRoleSettings(t *testing.T) {
	one := 1
	next := apiManifest(func(_ *manifest.Manifest, a *manifest.App) {
		r := a.Roles["web"]
		r.Command = []string{"serve", "--fast"}
		r.User = "app"
		r.Port = 9090
		r.Health.StartPeriod = "60s"
		r.Volumes = []string{"uploads:/data"}
		r.Resources = &manifest.Resources{Memory: "512m"}
		r.Hostnames = []string{"api.example.com"}
		r.Logging = &manifest.Logging{Driver: "json-file", Options: map[string]string{"max-size": "10m"}}
		a.Roles["web"] = r
		a.Roles["worker"] = manifest.Role{Replicas: &one}
		a.Deploy.Order = "stop-first"
		a.Deploy.DeployTimeout = "300s"
		off := false
		a.Deploy.Verify = &off
	})
	d := ReleaseDiff("api", side(apiManifest(nil), oldDigest, "v1"), side(next, oldDigest, "v1"))
	want := []string{
		`roles: + worker`,
		`web: command none -> ["serve","--fast"]`,
		`web: user none -> app`,
		`web: port 8080 -> 9090`,
		`web: health.start_period 30s -> 60s`,
		`web: + volume uploads:/data`,
		`web: resources.memory none -> 512m`,
		`web: logging.driver none -> json-file`,
		`web: logging.options.max-size none -> 10m`,
		`deploy: deploy_timeout 120s -> 300s`,
		`deploy: order auto -> stop-first`,
		`deploy: verify true -> false`,
		`ingress: + api.example.com`,
	}
	if got := changesOf(d); !reflect.DeepEqual(got, want) {
		t.Fatalf("changes\n got %q\nwant %q", got, want)
	}
}

func TestReleaseDiffRemovedRoleAndHealthCheck(t *testing.T) {
	one := 1
	prev := apiManifest(func(_ *manifest.Manifest, a *manifest.App) {
		a.Roles["worker"] = manifest.Role{Replicas: &one}
		r := a.Roles["web"]
		r.Hostnames = []string{"api.example.com"}
		a.Roles["web"] = r
	})
	next := apiManifest(func(_ *manifest.Manifest, a *manifest.App) {
		r := a.Roles["web"]
		r.Health = nil
		a.Roles["web"] = r
	})
	d := ReleaseDiff("api", side(prev, oldDigest, "v1"), side(next, oldDigest, "v1"))
	want := []string{
		`roles: - worker`,
		`web: - health`,
		`ingress: - api.example.com`,
	}
	if got := changesOf(d); !reflect.DeepEqual(got, want) {
		t.Fatalf("changes\n got %q\nwant %q", got, want)
	}
}

func TestReleaseDiffHooksMigrationsJobsAndBackups(t *testing.T) {
	prev := apiManifest(func(m *manifest.Manifest, a *manifest.App) {
		a.Hooks.PostDeploy = []manifest.Hook{{Run: []string{"./notify"}}}
		a.Migrations = &manifest.Migrations{Version: []string{"v"}, Apply: []string{"up"}}
		m.Jobs = map[string]manifest.Job{"nightly": {App: "api", Schedule: "0 2 * * *", Command: []string{"x"}}, "old": {App: "api", Command: []string{"y"}}}
		m.Backups = map[string]manifest.Backup{"data": {App: "api", Volume: "data", Destination: "s3", Schedule: "0 1 * * *"}}
	})
	next := apiManifest(func(m *manifest.Manifest, a *manifest.App) {
		a.Hooks.PreDeploy = []manifest.Hook{{Backup: "main-db"}}
		a.Hooks.PostDeploy = []manifest.Hook{{Run: []string{"./notify"}}}
		a.Migrations = &manifest.Migrations{Version: []string{"v"}, Apply: []string{"up", "--all"}}
		m.Jobs = map[string]manifest.Job{"nightly": {App: "api", Schedule: "0 3 * * *", Command: []string{"x"}}, "fresh": {App: "api", Schedule: "@daily", Command: []string{"z"}}}
		m.Backups = map[string]manifest.Backup{"data": {App: "api", Volume: "data", Destination: "s3", Schedule: "0 1 * * *"}, "logs": {App: "api", Volume: "logs", Destination: "s3"}}
	})
	d := ReleaseDiff("api", side(prev, oldDigest, "v1"), side(next, oldDigest, "v1"))
	want := []string{
		`hooks: + pre_deploy: backup main-db`,
		`migrations: apply ["up"] -> ["up","--all"]`,
		`jobs: + fresh`,
		`jobs: ~ nightly schedule "0 2 * * *" -> "0 3 * * *"`,
		`jobs: - old`,
		`backups: + logs`,
	}
	if got := changesOf(d); !reflect.DeepEqual(got, want) {
		t.Fatalf("changes\n got %q\nwant %q", got, want)
	}
}

func TestReleaseDiffRemovedHookAndMigrations(t *testing.T) {
	prev := apiManifest(func(_ *manifest.Manifest, a *manifest.App) {
		a.Hooks.PreDeploy = []manifest.Hook{{Run: []string{"./a", "b"}}}
		a.Migrations = &manifest.Migrations{Version: []string{"v"}, Apply: []string{"up"}}
	})
	d := ReleaseDiff("api", side(prev, oldDigest, "v1"), side(apiManifest(nil), oldDigest, "v1"))
	want := []string{`hooks: - pre_deploy: run ./a b`, `migrations: - migrations`}
	if got := changesOf(d); !reflect.DeepEqual(got, want) {
		t.Fatalf("changes\n got %q\nwant %q", got, want)
	}
}

func TestReleaseDiffTagOnlyAndRepository(t *testing.T) {
	d := ReleaseDiff("api", side(apiManifest(nil), oldDigest, "v1"), side(apiManifest(nil), oldDigest, "v2"))
	if got := changesOf(d); !reflect.DeepEqual(got, []string{"image: tag v1 -> v2"}) {
		t.Fatalf("changes %q", got)
	}
	next := apiManifest(func(_ *manifest.Manifest, a *manifest.App) { a.Image = "ghcr.io/acme/api2" })
	d = ReleaseDiff("api", side(apiManifest(nil), oldDigest, "v1"), side(next, oldDigest, "v1"))
	if got := changesOf(d); !reflect.DeepEqual(got, []string{"image: repository ghcr.io/acme/api -> ghcr.io/acme/api2"}) {
		t.Fatalf("changes %q", got)
	}
}

func TestReleaseDiffIsDeterministic(t *testing.T) {
	prev := apiManifest(nil)
	next := apiManifest(func(m *manifest.Manifest, a *manifest.App) {
		a.Env.Plain = map[string]string{"A": "1", "B": "2", "C": "3", "D": "4", "E": "5"}
		a.Env.Secret = []manifest.SecretVar{{Var: "S1", Secret: "S1"}, {Var: "S2", Secret: "S2"}}
		m.Jobs = map[string]manifest.Job{"j1": {App: "api", Command: []string{"a"}}, "j2": {App: "api", Command: []string{"b"}}, "j3": {App: "api", Command: []string{"c"}}}
	})
	first, _ := json.Marshal(ReleaseDiff("api", side(prev, oldDigest, "v1"), side(next, newDigest, "v2")))
	for i := 0; i < 30; i++ {
		again, _ := json.Marshal(ReleaseDiff("api", side(prev, oldDigest, "v1"), side(next, newDigest, "v2")))
		if string(again) != string(first) {
			t.Fatalf("run %d differs:\n%s\n%s", i, first, again)
		}
	}
}

func TestReleaseDiffLinesAlignTheAreas(t *testing.T) {
	d := Diff{Changes: []DiffChange{{Area: "image", Change: "x"}, {Area: "migrations", Change: "y"}}}
	want := []string{"image      x", "migrations y"}
	if got := d.Lines(); !reflect.DeepEqual(got, want) {
		t.Fatalf("lines %q", got)
	}
}

func TestDiffWithComposeAddsTheServicesThePlanChanges(t *testing.T) {
	m := manifest.Manifest{Project: "p", Network: "n", Apps: map[string]manifest.App{"shop": {Kind: "compose", File: "compose.yml"}}}
	d := ReleaseDiff("shop", side(m, "sha256:aaaaaaaaaaaaaaaa", ""), side(m, "sha256:aaaaaaaaaaaaaaaa", ""))
	if !d.Same {
		t.Fatalf("diff %+v", d)
	}
	d = d.WithCompose(ComposePlan{Services: []ComposeService{{Name: "web", Action: "recreate"}, {Name: "cache", Action: "keep"}, {Name: "db", Action: "create"}, {Name: "old", Action: "remove"}}})
	want := []string{"compose: recreate web", "compose: create db", "compose: remove old"}
	if d.Same || !reflect.DeepEqual(changesOf(d), want) {
		t.Fatalf("diff %+v changes %q", d, changesOf(d))
	}
	// A keep says nothing, so a plan that only keeps leaves the diff as it was.
	if kept := (Diff{Same: true}).WithCompose(ComposePlan{Services: []ComposeService{{Name: "web", Action: "keep"}}}); !kept.Same {
		t.Fatalf("diff %+v", kept)
	}
	// A new app stays new, whatever its services.
	if fresh := (Diff{New: true}).WithCompose(ComposePlan{Services: []ComposeService{{Name: "web", Action: "create"}}}); !fresh.New || len(fresh.Changes) != 0 {
		t.Fatalf("diff %+v", fresh)
	}
}

func TestReleaseDiffOfAComposeAppNamesTheFileChange(t *testing.T) {
	m := manifest.Manifest{Project: "p", Network: "n", Apps: map[string]manifest.App{"shop": {Kind: "compose", File: "compose.yml"}}}
	d := ReleaseDiff("shop", side(m, "sha256:aaaaaaaaaaaaaaaaaaaa", ""), side(m, "sha256:bbbbbbbbbbbbbbbbbbbb", ""))
	want := []string{"compose: file sha256:aaaaaaaaaaaa… -> sha256:bbbbbbbbbbbb…"}
	if got := changesOf(d); !reflect.DeepEqual(got, want) {
		t.Fatalf("changes %q", got)
	}
}

func TestReleaseDiffRetainsFieldPathsAndSides(t *testing.T) {
	prefix := strings.Repeat("x", 60)
	before := apiManifest(func(_ *manifest.Manifest, a *manifest.App) {
		r := a.Roles["web"]
		r.Logging = &manifest.Logging{Driver: "json-file", Options: map[string]string{"option.name": "old -> older"}}
		a.Roles = map[string]manifest.Role{"env": r}
		a.Env.Plain = map[string]string{"GONE": "remove me", "NOTE": prefix + " -> before"}
	})
	after := apiManifest(func(_ *manifest.Manifest, a *manifest.App) {
		three := 3
		r := before.Apps["api"].Roles["env"]
		r.Replicas = &three
		r.Health = &manifest.Health{Test: r.Health.Test, StartPeriod: "45s"}
		r.Logging = &manifest.Logging{Driver: "json-file", Options: map[string]string{"option.name": "new"}}
		r.Hostnames = []string{"api.example.com"}
		a.Roles = map[string]manifest.Role{"env": r}
		a.Env.Plain = map[string]string{"ADDED": "add me", "NOTE": prefix + " -> after"}
	})
	d := ReleaseDiff("api", side(before, oldDigest, "v1"), side(after, newDigest, "v2"))
	assertDiffJSON(t, d.Fields, `[`+
		`{"path":["image","digest"],"before":"`+pinnedDigest(oldDigest)+`","after":"`+pinnedDigest(newDigest)+`"},`+
		`{"path":["image","tag"],"before":"v1","after":"v2"},`+
		`{"path":["roles","env","replicas"],"before":"2","after":"3"},`+
		`{"path":["roles","env","health","start_period"],"before":"30s","after":"45s"},`+
		`{"path":["roles","env","logging","options","option.name"],"before":"old -\u003e older","after":"new"},`+
		`{"path":["env","ADDED"],"after":"add me"},`+
		`{"path":["env","GONE"],"before":"remove me"},`+
		`{"path":["env","NOTE"],"before":"`+prefix+` -\u003e before","after":"`+prefix+` -\u003e after"},`+
		`{"path":["ingress","api.example.com"],"after":"env"}]`)
}

func TestReleaseDiffDistinguishesAbsentAndEmptyLogOption(t *testing.T) {
	release := func(options map[string]string) *ReleaseSide {
		m := apiManifest(func(_ *manifest.Manifest, a *manifest.App) {
			r := a.Roles["web"]
			r.Logging = &manifest.Logging{Driver: "json-file", Options: options}
			a.Roles["web"] = r
		})
		if err := m.Validate(); err != nil {
			t.Fatal(err)
		}
		return side(m, oldDigest, "v1")
	}
	absent, empty := release(nil), release(map[string]string{"option.name": ""})
	for _, tc := range []struct {
		name          string
		before, after *ReleaseSide
		fields        string
	}{
		{"add", absent, empty, `[{"path":["roles","web","logging","options","option.name"],"after":""}]`},
		{"remove", empty, absent, `[{"path":["roles","web","logging","options","option.name"],"before":""}]`},
		{"unchanged", empty, empty, `null`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := ReleaseDiff("api", tc.before, tc.after)
			assertDiffJSON(t, d, `{"same":true}`)
			assertDiffJSON(t, d.Fields, tc.fields)
		})
	}
}

func TestReleaseDiffFieldsUseManifestSecretSources(t *testing.T) {
	release := func(secret, template string) *ReleaseSide {
		m := apiManifest(func(_ *manifest.Manifest, a *manifest.App) {
			a.Env.Secret = []manifest.SecretVar{{Var: "DB_PASSWORD", Secret: secret}}
			a.Env.Template = map[string]string{"DSN": template}
		})
		return side(m, oldDigest, "v1")
	}
	d := ReleaseDiff("api", release("DB_V1", "u:${DB_V1}@db"), release("DB_V2", "u:${DB_V2}@db"))
	// Exact sides exclude any resolved secret value.
	assertDiffJSON(t, d.Fields, `[{"path":["env","DB_PASSWORD"],"before":"from secret DB_V1","after":"from secret DB_V2"},`+
		`{"path":["env","DSN"],"before":"u:${DB_V1}@db","after":"u:${DB_V2}@db"}]`)
}

func TestComposeDiffFieldsKeepServiceActions(t *testing.T) {
	plan := ComposePlan{Services: []ComposeService{
		{Name: "db", Action: "recreate"},
		{Name: "cache", Action: "keep"},
		{Name: "old", Action: "remove"},
		{Name: "web", Action: "create"},
	}}
	d := (Diff{Same: true}).WithCompose(plan)
	assertDiffJSON(t, d, `{"changes":[{"area":"compose","change":"recreate db"},{"area":"compose","change":"remove old"},{"area":"compose","change":"create web"}]}`)
	assertDiffJSON(t, d.Fields, `[{"path":["compose","db"],"note":"recreate db"},{"path":["compose","old"],"note":"remove old"},{"path":["compose","web"],"note":"create web"}]`)

	first := (Diff{New: true}).WithCompose(plan)
	assertDiffJSON(t, first, `{"new":true}`)
	if len(first.Fields) != 0 || !reflect.DeepEqual(first.Lines(), []string{"new app"}) {
		t.Fatalf("first deploy = %+v, lines %q", first, first.Lines())
	}
}

func assertDiffJSON(t *testing.T, value any, want string) {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != want {
		t.Fatalf("JSON\n got %s\nwant %s", raw, want)
	}
}
