package deploy

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"berthy/internal/manifest"
	"berthy/internal/state"
)

// mappedManifest is the test app web with a plain MODE and the given env.secret entries.
func mappedManifest(secret ...manifest.SecretVar) manifest.Manifest {
	m := dockerManifest(map[string]string{"MODE": "fast"})
	app := m.Apps["web"]
	app.Env.Secret = secret
	m.Apps["web"] = app
	return m
}

// TestMappedSecretGivesItsVariableTheSecretsValue: an app whose env.secret maps DATABASE_URL to API_DATABASE_URL runs with
// DATABASE_URL set to the secret's value. The secret's name is not a variable of the container.
func TestMappedSecretGivesItsVariableTheSecretsValue(t *testing.T) {
	s := openStore(t)
	if err := s.PutSecret(context.Background(), state.Actor{ID: "admin"}, state.SecretInfo{Name: "API_DATABASE_URL"}, []byte("postgres://api")); err != nil {
		t.Fatal(err)
	}
	f := &boundFake{}
	c := &Controller{Engine: f, Store: s, Environment: "test"}
	m := mappedManifest(manifest.SecretVar{Var: "DATABASE_URL", Secret: "API_DATABASE_URL"})
	op := runBound(t, s, c, Request{RequestID: "mapped", App: "web", Version: "sha256:" + strings.Repeat("b", 64), Manifest: m})
	if op.State != "succeeded" {
		t.Fatalf("state %s: %s", op.State, op.Err)
	}
	created := f.createdSpecs()
	if len(created) != 1 {
		t.Fatalf("%d services created", len(created))
	}
	env := specEnv(t, created[0])
	if !slices.Contains(env, "DATABASE_URL=postgres://api") || !slices.Contains(env, "MODE=fast") {
		t.Fatalf("service env %q", env)
	}
	for _, entry := range env {
		if strings.HasPrefix(entry, "API_DATABASE_URL=") {
			t.Fatalf("the secret's name is a variable of the container: %q", env)
		}
	}
}

// TestMappedSecretMustBeStoredBeforeThePlan: a mapped secret is checked by its own name. Storing the variable's name
// does not satisfy the mapping, and the plan refuses with the workload message until the secret has a value.
func TestMappedSecretMustBeStoredBeforeThePlan(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	c := &Controller{Engine: &boundFake{}, Store: s, Environment: "test"}
	req := Request{App: "web", Version: "sha256:" + strings.Repeat("b", 64), Manifest: mappedManifest(manifest.SecretVar{Var: "DATABASE_URL", Secret: "API_DATABASE_URL"})}
	admin := Actor{ID: "admin", Admin: true}
	if _, err := c.Plan(ctx, admin, req); err == nil || err.Error() != "workload secret unavailable" {
		t.Fatalf("plan without the secret: %v", err)
	}
	if err := s.PutSecret(ctx, state.Actor{ID: "admin"}, state.SecretInfo{Name: "DATABASE_URL"}, []byte("postgres://wrong")); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Plan(ctx, admin, req); err == nil || err.Error() != "workload secret unavailable" {
		t.Fatalf("plan with only the variable's name stored: %v", err)
	}
	if err := s.PutSecret(ctx, state.Actor{ID: "admin"}, state.SecretInfo{Name: "API_DATABASE_URL"}, []byte("postgres://api")); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Plan(ctx, admin, req); err != nil {
		t.Fatalf("plan with the secret stored: %v", err)
	}
}

// TestComposeEnvFileGivesTheMappedVariableTheSecret: a Compose app's .env file names the variable, with the secret's value,
// and does not name the secret.
func TestComposeEnvFileGivesTheMappedVariableTheSecret(t *testing.T) {
	runner := &fakeCompose{}
	c, s := composeController(t, runner, newComposeDocker())
	ctx := context.Background()
	if err := s.PutSecret(ctx, state.Actor{ID: "admin", Name: "admin"}, state.SecretInfo{Name: "WEB_DB"}, []byte("postgres://web")); err != nil {
		t.Fatal(err)
	}
	m := composeApp(false)
	app := m.Apps["db"]
	app.Env = manifest.Env{Secret: []manifest.SecretVar{{Var: "DB_PASS", Secret: "WEB_DB"}}}
	m.Apps["db"] = app
	dir := t.TempDir()
	if _, err := c.stageCompose(ctx, dir, composeReq("mapped", composeFileOne, m)); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, ".env"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(raw)
	want := "BERTHY_APP='db'\nBERTHY_ENVIRONMENT='test'\nBERTHY_PROJECT='shop'\nDB_PASS='postgres://web'\n"
	if got != want {
		t.Fatalf("environment file %q, want %q", got, want)
	}
}

// TestMigrationVersionRejectsAMappedVariable: the variable of a mapping is private, whatever its secret is named, so a
// version that prints its value is refused.
func TestMigrationVersionRejectsAMappedVariable(t *testing.T) {
	c, f, id, p := hookFixture(t)
	a := p.Request.Manifest.Apps["app"]
	a.Migrations = &manifest.Migrations{Version: []string{"version"}, Apply: []string{"apply"}}
	a.Env.Secret = []manifest.SecretVar{{Var: "TOKEN", Secret: "STORED_TOKEN"}}
	p.Request.Manifest.Apps["app"] = a
	// Enough exits and logs for every step of a migration: if the version were accepted, apply would run and the spec count
	// below would show it, instead of the engine running out of answers.
	f.exits = []int{0, 0, 0, 0}
	f.logs = [][]byte{[]byte("secret\n"), []byte("applied\n"), []byte("v2\n"), []byte("v3\n")}
	_, _, err := c.runPre(context.Background(), id, p)
	if err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatal("secret output was accepted or disclosed")
	}
	if len(f.specs) != 1 {
		t.Fatal("apply ran after invalid version")
	}
}
