package manifest

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// positionBase is a valid manifest. The comments say the line each interesting key is on.
const positionBase = `project: demo
network: demo
apps:
  api:
    kind: image
    image: ghcr.io/acme/api
    roles:
      web:
        port: 8080
destinations:
  s3:
    type: s3
    bucket: my-bucket
    endpoint: https://s3.example.com
    access_key_secret: AK
    secret_key_secret: SK
backups:
  data:
    app: api
    volume: data
    destination: s3
`

func writeFiles(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func loadProblems(t *testing.T, files map[string]string) Problems {
	t.Helper()
	_, err := Load(writeFiles(t, files), "prod")
	var ps Problems
	if !errors.As(err, &ps) {
		t.Fatalf("want Problems, got %#v", err)
	}
	return ps
}

// at is a Problem's place, for a short comparison.
func placedAt(p Problem) [3]any { return [3]any{p.File, p.Line, p.Column} }

func TestLoadPlacesEachProblemInItsFile(t *testing.T) {
	ps := loadProblems(t, map[string]string{
		"berthy.yml":      strings.Replace(positionBase, "8080", "70000", 1),
		"berthy.prod.yml": "backups:\n  data:\n    destination: nope\n",
	})
	if len(ps) != 2 {
		t.Fatalf("got %+v", []Problem(ps))
	}
	if ps[0].Path != "apps.api.roles.web.port" || placedAt(ps[0]) != [3]any{"berthy.yml", 9, 9} {
		t.Errorf("port: %+v", ps[0])
	}
	if ps[1].Path != "backups.data.destination" || placedAt(ps[1]) != [3]any{"berthy.prod.yml", 3, 5} {
		t.Errorf("destination: %+v", ps[1])
	}
}

func TestLoadAttributesAKeySetInBothFilesToTheOverlay(t *testing.T) {
	ps := loadProblems(t, map[string]string{
		"berthy.yml":      positionBase,
		"berthy.prod.yml": "apps:\n  api:\n    roles:\n      web:\n        port: 70001\n",
	})
	if len(ps) != 1 || placedAt(ps[0]) != [3]any{"berthy.prod.yml", 5, 9} {
		t.Fatalf("got %+v", []Problem(ps))
	}
	// And the overlay's good value hides the base's bad one: the problem is gone.
	if _, err := Load(writeFiles(t, map[string]string{
		"berthy.yml":      strings.Replace(positionBase, "8080", "70000", 1),
		"berthy.prod.yml": "apps:\n  api:\n    roles:\n      web:\n        port: 80\n",
	}), "prod"); err != nil {
		t.Fatal(err)
	}
}

func TestLoadReportsEveryUnknownKeyWithItsLine(t *testing.T) {
	ps := loadProblems(t, map[string]string{
		"berthy.yml":      positionBase + "wat: 1\n",
		"berthy.prod.yml": "apps:\n  api:\n    nope: 1\n",
	})
	if len(ps) != 2 {
		t.Fatalf("got %+v", []Problem(ps))
	}
	if ps[0].Message != `unknown field "nope"` || ps[0].Path != "apps.api.nope" || placedAt(ps[0]) != [3]any{"berthy.prod.yml", 3, 5} {
		t.Errorf("nope: %+v", ps[0])
	}
	if ps[1].Message != `unknown field "wat"` || placedAt(ps[1]) != [3]any{"berthy.yml", 22, 1} {
		t.Errorf("wat: %+v", ps[1])
	}
}

func TestLoadStillChecksTheManifestWhenKeysAreUnknown(t *testing.T) {
	ps := loadProblems(t, map[string]string{
		"berthy.yml":      strings.Replace(positionBase, "8080", "70000", 1) + "wat: 1\n",
		"berthy.prod.yml": "backups:\n  data:\n    destination: nope\n",
	})
	var got []string
	for _, p := range ps {
		got = append(got, p.Path)
	}
	if strings.Join(got, " ") != "wat apps.api.roles.web.port backups.data.destination" {
		t.Fatalf("got %v", got)
	}
}

func TestLoadReportsATypeErrorWithItsLine(t *testing.T) {
	ps := loadProblems(t, map[string]string{
		"berthy.yml": strings.Replace(positionBase, "port: 8080", "replicas: \"two\"", 1),
	})
	if len(ps) != 1 {
		t.Fatalf("got %+v", []Problem(ps))
	}
	p := ps[0]
	if p.Path != "apps.api.roles.web.replicas" || placedAt(p) != [3]any{"berthy.yml", 9, 9} ||
		p.Message != "json: cannot unmarshal string into Go struct field Role.apps.roles.replicas of type int" {
		t.Fatalf("got %+v", p)
	}
}

func TestLoadReportsEveryTypeErrorAndSkipsTheChecks(t *testing.T) {
	ps := loadProblems(t, map[string]string{
		"berthy.yml":      strings.Replace(positionBase, "port: 8080", "replicas: \"two\"\n        port: \"80\"", 1),
		"berthy.prod.yml": "network: 5\nbackups:\n  data:\n    app: nope\n",
	})
	want := Problems{
		{Path: "apps.api.roles.web.port", File: "berthy.yml", Line: 10, Column: 9},
		{Path: "apps.api.roles.web.replicas", File: "berthy.yml", Line: 9, Column: 9},
		{Path: "network", File: "berthy.prod.yml", Line: 1, Column: 1},
	}
	if len(ps) != len(want) {
		t.Fatalf("got %+v", []Problem(ps))
	}
	for i, w := range want {
		g := ps[i]
		if g.Path != w.Path || g.File != w.File || g.Line != w.Line || g.Column != w.Column {
			t.Errorf("problem %d: got %+v, want %+v", i, g, w)
		}
	}
}

func TestLoadPointsAtTheNearestKeyForAMissingOne(t *testing.T) {
	ps := loadProblems(t, map[string]string{
		"berthy.yml": strings.Replace(positionBase, "    volume: data\n", "", 1),
	})
	if len(ps) != 1 || ps[0].Path != "backups.data.volume" || placedAt(ps[0]) != [3]any{"berthy.yml", 18, 3} {
		t.Fatalf("got %+v", []Problem(ps))
	}
}

func TestLoadNamesTheBaseWhenTheWholeIsAtFault(t *testing.T) {
	ps := loadProblems(t, map[string]string{"berthy.yml": "project: demo\nnetwork: demo\n"})
	if len(ps) != 1 || ps[0].Message != "at least one app is required" || placedAt(ps[0]) != [3]any{"berthy.yml", 0, 0} {
		t.Fatalf("got %+v", []Problem(ps))
	}
}

func TestLoadPlacesParseProblems(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]string
		want  [3]any
		msg   string
	}{
		{"a duplicate key", map[string]string{"berthy.yml": positionBase + "project: other\n"}, [3]any{"berthy.yml", 22, 1}, `invalid or duplicate key "project"`},
		{"null in the base", map[string]string{"berthy.yml": positionBase + "telemetry: null\n"}, [3]any{"berthy.yml", 22, 12}, "aliases, anchors and null are not allowed"},
		{"null in an overlay list", map[string]string{"berthy.yml": positionBase, "berthy.prod.yml": "apps:\n  api:\n    env:\n      secret:\n        - JWT_SIGNING_KEY\n        - null\n"}, [3]any{"berthy.prod.yml", 6, 11}, "aliases, anchors and null are not allowed"},
		{"an alias", map[string]string{"berthy.yml": positionBase + "jobs: &j {}\n"}, [3]any{"berthy.yml", 22, 7}, "aliases, anchors and null are not allowed"},
		{"a syntax error", map[string]string{"berthy.yml": "project: [\n"}, [3]any{"berthy.yml", 0, 0}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ps := loadProblems(t, c.files)
			if len(ps) != 1 || placedAt(ps[0]) != c.want || !strings.Contains(ps[0].Message, c.msg) {
				t.Fatalf("got %+v", []Problem(ps))
			}
		})
	}
}

func TestLoadReportsTheProblemsOfBothFilesTogether(t *testing.T) {
	ps := loadProblems(t, map[string]string{
		"berthy.yml":      positionBase + "project: other\n",
		"berthy.prod.yml": "protected: true\nprotected: false\n",
	})
	if len(ps) != 2 || ps[0].File != "berthy.yml" || ps[1].File != "berthy.prod.yml" {
		t.Fatalf("got %+v", []Problem(ps))
	}
}

func TestLoadErrorIsTheFirstMessage(t *testing.T) {
	_, err := Load(writeFiles(t, map[string]string{
		"berthy.yml":      strings.Replace(positionBase, "8080", "70000", 1),
		"berthy.prod.yml": "backups:\n  data:\n    destination: nope\n",
	}), "prod")
	if err == nil || err.Error() != "app api role web: invalid settings" {
		t.Fatalf("got %v", err)
	}
}

func TestKeysWithDotsAreLocated(t *testing.T) {
	ps := loadProblems(t, map[string]string{
		"berthy.yml": positionBase + "registries:\n  ghcr.io:\n    username: \"\"\n    password_secret: P\n",
	})
	if len(ps) != 1 || ps[0].Path != "registries.ghcr.io.username" || placedAt(ps[0]) != [3]any{"berthy.yml", 24, 5} {
		t.Fatalf("got %+v", []Problem(ps))
	}
}

func TestLoadPlacesAnEntryOfAList(t *testing.T) {
	ps := loadProblems(t, map[string]string{
		"berthy.yml":      positionBase + "aliases:\n  up: deploy\n",
		"berthy.prod.yml": "apps:\n  api:\n    env:\n      secret:\n        - A\n        - 1x\n",
	})
	if len(ps) != 1 || ps[0].Path != "apps.api.env.secret.1" || placedAt(ps[0]) != [3]any{"berthy.prod.yml", 6, 11} {
		t.Fatalf("got %+v", []Problem(ps))
	}
}
