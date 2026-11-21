package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"berthy/internal/manifest"
	"berthy/internal/state"
)

const (
	templateText   = "postgres://todo:${POSTGRES_PASSWORD}@todo-db:5432/todo"
	templateFilled = "postgres://todo:pg-s3cret@todo-db:5432/todo"
)

// templateManifest is the test app web with a plain MODE and DATABASE_URL built from POSTGRES_PASSWORD.
func templateManifest() manifest.Manifest {
	m := dockerManifest(map[string]string{"MODE": "fast"})
	app := m.Apps["web"]
	app.Env.Template = map[string]string{"DATABASE_URL": templateText}
	m.Apps["web"] = app
	return m
}

func putSecret(t *testing.T, s *state.Store, name, value string) {
	t.Helper()
	if err := s.PutSecret(context.Background(), state.Actor{ID: "admin"}, state.SecretInfo{Name: name}, []byte(value)); err != nil {
		t.Fatal(err)
	}
}

// TestImageDeployGivesTheContainerTheFilledTemplate: the service gets the filled DATABASE_URL, and the release keeps the
// template, never the filled value or the password.
func TestImageDeployGivesTheContainerTheFilledTemplate(t *testing.T) {
	s := openStore(t)
	putSecret(t, s, "POSTGRES_PASSWORD", "pg-s3cret")
	f := &boundFake{}
	c := &Controller{Engine: f, Store: s, Environment: "test"}
	op := runBound(t, s, c, Request{RequestID: "tpl", App: "web", Version: "sha256:" + strings.Repeat("b", 64), Manifest: templateManifest()})
	if op.State != "succeeded" {
		t.Fatalf("state %s: %s", op.State, op.Err)
	}
	created := f.createdSpecs()
	if len(created) != 1 {
		t.Fatalf("%d services created", len(created))
	}
	if env := specEnv(t, created[0]); !slices.Contains(env, "DATABASE_URL="+templateFilled) || !slices.Contains(env, "MODE=fast") {
		t.Fatalf("service env %q", env)
	}
	release, err := s.Applied(context.Background(), "web")
	if err != nil {
		t.Fatal(err)
	}
	stored := string(release.Manifest)
	var text string
	if b, err := json.Marshal(templateText); err == nil {
		text = string(b)
	}
	if !strings.Contains(stored, text) || strings.Contains(stored, "pg-s3cret") {
		t.Fatalf("the release does not keep the template alone: %s", stored)
	}
}

// TestAMissingTemplateSecretStopsTheDeployBeforeAnyChange: the plan and the deploy refuse, name the secret, and no service is
// created.
func TestAMissingTemplateSecretStopsTheDeployBeforeAnyChange(t *testing.T) {
	s := openStore(t)
	f := &boundFake{}
	c := &Controller{Engine: f, Store: s, Environment: "test"}
	req := Request{RequestID: "missing", App: "web", Version: "sha256:" + strings.Repeat("b", 64), Manifest: templateManifest()}
	want := "secret POSTGRES_PASSWORD is not set"
	_, err := c.Plan(context.Background(), Actor{ID: "admin", Admin: true}, req)
	var r Refusal
	if !errors.As(err, &r) || string(r) != want || !PublicMessage(want) {
		t.Fatalf("plan: %v", err)
	}
	op := runBound(t, s, c, req)
	if op.State != "failed" || op.Err != want {
		t.Fatalf("deploy: %s %q", op.State, op.Err)
	}
	if n := len(f.createdSpecs()); n != 0 {
		t.Fatalf("%d services created", n)
	}
}

// TestAStoreFailureIsNotAMissingSecret: only a secret that has no row is "not set". A store that fails otherwise gives a plain
// error, which the deploy reports as an internal failure and not as a missing secret.
func TestAStoreFailureIsNotAMissingSecret(t *testing.T) {
	s := openStore(t)
	c := &Controller{Engine: &boundFake{}, Store: s, Environment: "test"}
	if _, err := c.workloadSecret(context.Background(), "ABSENT"); err == nil || err.Error() != "secret ABSENT is not set" {
		t.Fatalf("absent secret: %v", err)
	}
	putSecret(t, s, "PRESENT", "v")
	// A cancelled context makes the store fail for a reason that is not a missing row.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.workloadSecret(ctx, "PRESENT")
	var r Refusal
	if err == nil || errors.As(err, &r) {
		t.Fatalf("a failing store gave %v", err)
	}
	if _, err = c.resolveEnv(ctx, manifest.Env{Template: map[string]string{"X": "${PRESENT}"}}); err == nil || errors.As(err, &r) {
		t.Fatalf("resolveEnv with a failing store gave %v", err)
	}
}

// TestPublicMessageNamesOnlyAValidSecret: the refusal that names a secret is public for a secret name, and for nothing else.
func TestPublicMessageNamesOnlyAValidSecret(t *testing.T) {
	for _, ok := range []string{"secret A is not set", "secret _db_2 is not set", "secret REG is a registry password, not a workload secret"} {
		if !PublicMessage(ok) {
			t.Errorf("not public: %q", ok)
		}
	}
	for _, bad := range []string{"secret is not set", "secret a b is not set", "secret 1x is not set", "secret A=hunter2 is not set", "secret A is not set\nx"} {
		if PublicMessage(bad) {
			t.Errorf("public: %q", bad)
		}
	}
}

// TestTheFilledTemplateIsRedactedAsAWhole: output that shows the filled value is redacted to one marker, and the password
// alone is redacted too.
func TestTheFilledTemplateIsRedactedAsAWhole(t *testing.T) {
	s := openStore(t)
	putSecret(t, s, "POSTGRES_PASSWORD", "pg-s3cret")
	c := &Controller{Engine: &boundFake{}, Store: s, Environment: "test"}
	if op := runBound(t, s, c, Request{RequestID: "tpl", App: "web", Version: "sha256:" + strings.Repeat("b", 64), Manifest: templateManifest()}); op.State != "succeeded" {
		t.Fatalf("state %s: %s", op.State, op.Err)
	}
	redact, ok := c.Redactor(context.Background())
	if !ok {
		t.Fatal("redactor unavailable")
	}
	if got, want := redact("DATABASE_URL="+templateFilled+" pw=pg-s3cret"), "DATABASE_URL=[redacted] pw=[redacted]"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// TestComposeEnvFileGivesTheFilledTemplate: a Compose app's .env file gets the filled value, quoted like any value, and the
// template text is not written.
func TestComposeEnvFileGivesTheFilledTemplate(t *testing.T) {
	c, s := composeController(t, &fakeCompose{}, newComposeDocker())
	putSecret(t, s, "POSTGRES_PASSWORD", "pg-s3cret")
	m := composeApp(false)
	app := m.Apps["db"]
	app.Env = manifest.Env{Plain: map[string]string{"A_PLAIN": "x"}, Template: map[string]string{"DATABASE_URL": templateText}}
	m.Apps["db"] = app
	dir := t.TempDir()
	if _, err := c.stageCompose(context.Background(), dir, composeReq("tpl", composeFileOne, m)); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, ".env"))
	if err != nil {
		t.Fatal(err)
	}
	want := "BERTHY_APP='db'\nBERTHY_ENVIRONMENT='test'\nBERTHY_NETWORK='berthy'\nBERTHY_PROJECT='shop'\nA_PLAIN='x'\nDATABASE_URL='" + templateFilled + "'\n"
	if string(raw) != want {
		t.Fatalf("environment file %q, want %q", raw, want)
	}
	// A missing secret stops the deploy with a refusal that names it.
	if err = os.Remove(filepath.Join(dir, ".env")); err != nil {
		t.Fatal(err)
	}
	app.Env.Template = map[string]string{"DATABASE_URL": "x${ABSENT}"}
	m.Apps["db"] = app
	_, err = c.stageCompose(context.Background(), t.TempDir(), composeReq("tpl2", composeFileOne, m))
	var r Refusal
	if !errors.As(err, &r) || string(r) != "secret ABSENT is not set" {
		t.Fatalf("missing secret: %v", err)
	}
}

// TestAdoptionTakesATemplateVariablesLiveValue: a secret read only when a deploy runs is not known to an adoption plan, so a
// template variable takes the service's own value, like a secret variable.
func TestAdoptionTakesATemplateVariablesLiveValue(t *testing.T) {
	env := manifest.Env{Plain: map[string]string{"A": "1"}, Template: map[string]string{"DATABASE_URL": templateText}}
	got := adoptionEnv(env, []string{"DATABASE_URL=live", "OTHER=x"})
	if want := []string{"A=1", "DATABASE_URL=live"}; !slices.Equal(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// TestMigrationVersionRejectsATemplateValue: the variable of a template is private, so a version that prints its filled value
// is refused.
func TestMigrationVersionRejectsATemplateValue(t *testing.T) {
	c, f, id, p := hookFixture(t)
	a := p.Request.Manifest.Apps["app"]
	a.Migrations = &manifest.Migrations{Version: []string{"version"}, Apply: []string{"apply"}}
	a.Env.Template = map[string]string{"DSN": "pg://${PW}"}
	p.Request.Manifest.Apps["app"] = a
	p.Env = []string{"DSN=pg://secret"}
	f.exits = []int{0, 0, 0, 0}
	f.logs = [][]byte{[]byte("pg://secret\n"), []byte("applied\n"), []byte("v2\n"), []byte("v3\n")}
	if _, _, err := c.runPre(context.Background(), id, p); err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatal("the filled value was accepted or disclosed")
	}
	if len(f.specs) != 1 {
		t.Fatal("apply ran after an invalid version")
	}
}
