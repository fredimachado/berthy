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

func TestNullInTheBaseStillRefusesTheWholeFile(t *testing.T) {
	dir := writeFiles(t, map[string]string{"berthy.yml": overlayBase + "telemetry: null\n"})
	_, err := Load(dir, "prod")
	var ps Problems
	if !errors.As(err, &ps) || len(ps) != 1 {
		t.Fatalf("got %v", err)
	}
}
