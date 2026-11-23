package manifest

import (
	"errors"
	"strings"
	"testing"
)

// overlayBase is a valid manifest with an env and two secret entries. The comments say the line each key is on.
const overlayBase = `project: demo
network: demo
apps:
  api:
    kind: image
    image: ghcr.io/acme/api
    env:
      plain:
        LOG_LEVEL: info
        LEGACY_MODE: "1"
      secret:
        - JWT_SIGNING_KEY
        - DATABASE_URL: DB
    roles:
      web:
        port: 8080
`

// loadWithWarnings is LoadDetailed for the tests that read a valid manifest's warnings.
func loadWithWarnings(dir, env string) (*Manifest, Problems, error) {
	l, err := LoadDetailed(dir, env)
	if err != nil {
		return nil, nil, err
	}
	return l.Manifest, l.Warnings, nil
}

func TestOverlayNullRemovesAKeyOfThatEnvironmentOnly(t *testing.T) {
	dir := writeFiles(t, map[string]string{
		"berthy.yml":      overlayBase,
		"berthy.prod.yml": "apps:\n  api:\n    env:\n      plain:\n        LEGACY_MODE: null\n",
	})
	m, warnings, err := loadWithWarnings(dir, "prod")
	if err != nil {
		t.Fatal(err)
	}
	if plain := m.Apps["api"].Env.Plain; plain["LOG_LEVEL"] != "info" || len(plain) != 1 || len(warnings) != 0 {
		t.Fatalf("plain %v, warnings %+v", plain, warnings)
	}
	dev, err := Load(dir, "dev")
	if err != nil {
		t.Fatal(err)
	}
	if dev.Apps["api"].Env.Plain["LEGACY_MODE"] != "1" {
		t.Fatalf("dev lost a value that only prod removes: %v", dev.Apps["api"].Env.Plain)
	}
}

func TestOverlayNullRemovesAWholeMapping(t *testing.T) {
	dir := writeFiles(t, map[string]string{
		"berthy.yml":      overlayBase,
		"berthy.prod.yml": "apps:\n  api:\n    env: null\n",
	})
	m, err := Load(dir, "prod")
	if err != nil {
		t.Fatal(err)
	}
	if env := m.Apps["api"].Env; len(env.Plain) != 0 || len(env.Secret) != 0 {
		t.Fatalf("env not removed: %+v", env)
	}
}

func TestOverlayNullOfAKeyTheBaseDoesNotSetWarnsAndChangesNothing(t *testing.T) {
	dir := writeFiles(t, map[string]string{
		"berthy.yml":      overlayBase,
		"berthy.prod.yml": "apps:\n  api:\n    env:\n      plain:\n        NOT_SET: null\n",
	})
	m, warnings, err := loadWithWarnings(dir, "prod")
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 1 || warnings[0].Path != "apps.api.env.plain.NOT_SET" || placedAt(warnings[0]) != [3]any{"berthy.prod.yml", 5, 9} ||
		warnings[0].Message != "null removes nothing; berthy.yml does not set it" {
		t.Fatalf("warnings %+v", warnings)
	}
	if len(m.Apps["api"].Env.Plain) != 2 {
		t.Fatalf("plain %v", m.Apps["api"].Env.Plain)
	}
}

func TestOverlayNullInANewSubtreeIsDroppedWithAWarning(t *testing.T) {
	dir := writeFiles(t, map[string]string{
		"berthy.yml":      overlayBase,
		"berthy.prod.yml": "apps:\n  api:\n    roles:\n      web:\n        health:\n          test: [CMD, curl]\n          retries: null\n",
	})
	m, warnings, err := loadWithWarnings(dir, "prod")
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 1 || warnings[0].Path != "apps.api.roles.web.health.retries" || placedAt(warnings[0]) != [3]any{"berthy.prod.yml", 7, 11} {
		t.Fatalf("warnings %+v", warnings)
	}
	// The null is dropped, so the merged manifest is the one a file without that line gives.
	without := writeFiles(t, map[string]string{
		"berthy.yml":      overlayBase,
		"berthy.prod.yml": "apps:\n  api:\n    roles:\n      web:\n        health:\n          test: [CMD, curl]\n",
	})
	want, err := Load(without, "prod")
	if err != nil {
		t.Fatal(err)
	}
	if got, exp := m.Apps["api"].Roles["web"].Health, want.Apps["api"].Roles["web"].Health; *got.Retries != *exp.Retries || got.Interval != exp.Interval {
		t.Fatalf("health %+v, want %+v", got, exp)
	}
}

func TestOverlayNullOfARequiredKeyIsReportedAtTheOverlayLine(t *testing.T) {
	ps := loadProblems(t, map[string]string{
		"berthy.yml":      overlayBase,
		"berthy.prod.yml": "apps:\n  api:\n    image: null\n",
	})
	if len(ps) != 1 || ps[0].Path != "apps.api.image" || placedAt(ps[0]) != [3]any{"berthy.prod.yml", 3, 5} {
		t.Fatalf("got %+v", []Problem(ps))
	}
}

func TestOverlayEmptyValueIsRefusedBecauseItWouldRemoveAKeyByAccident(t *testing.T) {
	ps := loadProblems(t, map[string]string{
		"berthy.yml":      overlayBase,
		"berthy.prod.yml": "apps:\n  api:\n    env:\n      plain:\n        LEGACY_MODE:\n",
	})
	if len(ps) != 1 || placedAt(ps[0]) != [3]any{"berthy.prod.yml", 5, 21} ||
		!strings.Contains(ps[0].Message, "write null to remove a key") {
		t.Fatalf("got %+v", []Problem(ps))
	}
}

func TestOverlayListThatDropsBaseEntriesWarnsAndStillLoads(t *testing.T) {
	dir := writeFiles(t, map[string]string{
		"berthy.yml":      overlayBase,
		"berthy.prod.yml": "apps:\n  api:\n    env:\n      secret:\n        - JWT_SIGNING_KEY\n",
	})
	m, warnings, err := loadWithWarnings(dir, "prod")
	if err != nil {
		t.Fatalf("a dropped entry is a warning, not a problem: %v", err)
	}
	if len(warnings) != 1 || warnings[0].Path != "apps.api.env.secret" || placedAt(warnings[0]) != [3]any{"berthy.prod.yml", 4, 7} ||
		warnings[0].Message != "replaces the base list and drops DATABASE_URL" {
		t.Fatalf("warnings %+v", warnings)
	}
	if len(m.Apps["api"].Env.Secret) != 1 {
		t.Fatalf("secret %+v", m.Apps["api"].Env.Secret)
	}
}

func TestOverlayListNamesEveryDroppedEntryInItsOrder(t *testing.T) {
	dir := writeFiles(t, map[string]string{
		"berthy.yml":      overlayBase,
		"berthy.prod.yml": "apps:\n  api:\n    env:\n      secret:\n        - OTHER\n",
	})
	_, warnings, err := loadWithWarnings(dir, "prod")
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 1 || warnings[0].Message != "replaces the base list and drops JWT_SIGNING_KEY, DATABASE_URL" {
		t.Fatalf("warnings %+v", warnings)
	}
}

// A command line is positional, so an overlay that rewrites one replaces all of it on purpose: a role's command, a health check's
// test and a migration command give no note. An entry of any other list still gets one.
func TestOverlayRewritingACommandLineDoesNotWarn(t *testing.T) {
	base := strings.NewReplacer(
		"    roles:\n", "    migrations:\n      version: [app, version]\n      apply: [up]\n    roles:\n",
		"        port: 8080\n", "        port: 8080\n        command: [serve, --port, \"8080\"]\n        health:\n          test: [CMD, curl]\n",
	).Replace(overlayBase)
	dir := writeFiles(t, map[string]string{
		"berthy.yml": base,
		"berthy.prod.yml": "apps:\n  api:\n    migrations:\n      apply: [up, --all]\n    roles:\n      web:\n" +
			"        command: [run, --fast]\n        health:\n          test: [CMD, \"true\"]\n",
	})
	m, warnings, err := loadWithWarnings(dir, "prod")
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 0 {
		t.Fatalf("a command line is replaced without a note, got %+v", warnings)
	}
	if got := strings.Join(m.Apps["api"].Roles["web"].Command, " "); got != "run --fast" {
		t.Fatalf("command %q", got)
	}
	if got := strings.Join(m.Apps["api"].Migrations.Apply, " "); got != "up --all" {
		t.Fatalf("apply %q", got)
	}
}

func TestOverlayListOfNamesStillWarnsWhenItDropsAnEntry(t *testing.T) {
	dir := writeFiles(t, map[string]string{
		"berthy.yml":      overlayBase,
		"berthy.prod.yml": "apps:\n  api:\n    env:\n      secret:\n        - JWT_SIGNING_KEY\n",
	})
	_, warnings, err := loadWithWarnings(dir, "prod")
	if err != nil || len(warnings) != 1 || warnings[0].Message != "replaces the base list and drops DATABASE_URL" {
		t.Fatalf("err %v, warnings %+v", err, warnings)
	}
}

func TestOverlayListThatKeepsEveryBaseEntryDoesNotWarn(t *testing.T) {
	dir := writeFiles(t, map[string]string{
		"berthy.yml":      overlayBase,
		"berthy.prod.yml": "apps:\n  api:\n    env:\n      secret:\n        - DATABASE_URL: DB\n        - NEW_KEY\n        - JWT_SIGNING_KEY\n",
	})
	_, warnings, err := loadWithWarnings(dir, "prod")
	if err != nil || len(warnings) != 0 {
		t.Fatalf("err %v, warnings %+v", err, warnings)
	}
}

func TestOverlayRemovesADeclaredSecretWithNull(t *testing.T) {
	base := overlayBase + `secrets:
  JWT_SIGNING_KEY:
    generate:
      format: hex
      bytes: 32
`
	dir := writeFiles(t, map[string]string{
		"berthy.yml":      base,
		"berthy.prod.yml": "secrets:\n  JWT_SIGNING_KEY: null\n",
	})
	m, err := Load(dir, "prod")
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Secrets) != 0 {
		t.Fatalf("secret not removed: %+v", m.Secrets)
	}
	dev, err := Load(dir, "dev")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := dev.Secrets["JWT_SIGNING_KEY"]; !ok {
		t.Fatal("dev lost a declared secret that only prod removes")
	}
}

func TestNullInTheBaseStillRefusesTheWholeFile(t *testing.T) {
	dir := writeFiles(t, map[string]string{"berthy.yml": overlayBase + "telemetry: null\n"})
	_, err := Load(dir, "prod")
	var ps Problems
	if !errors.As(err, &ps) || len(ps) != 1 {
		t.Fatalf("got %v", err)
	}
}

func TestAnOverlayCannotSetTheProject(t *testing.T) {
	const want = "project is set in berthy.yml only: an environment cannot have another project"
	for name, overlay := range map[string]string{
		"another project":  "network: demo\nproject: other\n",
		"the same project": "network: demo\nproject: demo\n",
		"null":             "network: demo\nproject: null\n",
	} {
		ps := loadProblems(t, map[string]string{"berthy.yml": overlayBase, "berthy.prod.yml": overlay})
		if len(ps) != 1 || ps[0].Message != want || placedAt(ps[0]) != [3]any{"berthy.prod.yml", 2, 1} {
			t.Errorf("%s: got %+v", name, []Problem(ps))
		}
	}
	// The refusal is one of the problems of the overlay, not the only one.
	ps := loadProblems(t, map[string]string{"berthy.yml": overlayBase, "berthy.prod.yml": "project: other\nnope: 1\n"})
	if len(ps) != 2 || ps[0].Message != want || ps[1].Message != `unknown field "nope"` {
		t.Errorf("with another problem: got %+v", []Problem(ps))
	}
}
