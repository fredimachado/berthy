package scope_test

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"berthy/internal/manifest"
	"berthy/internal/scope"
)

func TestOwnsNeedsTheProjectAndTheEnvironment(t *testing.T) {
	id := scope.ID{Project: "demo", Environment: "prod"}
	if !id.Owns(id.Labels()) {
		t.Fatal("an ID does not own its own labels")
	}
	cases := []struct {
		name   string
		id     scope.ID
		labels map[string]string
		want   bool
	}{
		{"same environment and another project", scope.ID{Project: "demo", Environment: "prod"}, map[string]string{scope.LabelProject: "other", scope.LabelEnvironment: "prod"}, false},
		{"absent project label with a set project", scope.ID{Project: "demo", Environment: "prod"}, map[string]string{scope.LabelEnvironment: "prod"}, false},
		{"absent project label with an empty project", scope.ID{Project: "", Environment: "prod"}, map[string]string{scope.LabelEnvironment: "prod"}, true},
	}
	for _, c := range cases {
		if got := c.id.Owns(c.labels); got != c.want {
			t.Errorf("%s: Owns = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestSlugDiffersWhereTheNamesSplitDifferently(t *testing.T) {
	left := scope.ID{Project: "a-b", Environment: "c"}
	right := scope.ID{Project: "a", Environment: "b-c"}
	if left.Slug() == right.Slug() {
		t.Fatalf("slug %q is the same for {a-b, c} and {a, b-c}", left.Slug())
	}
	if left.Slug() != "a-b.c" || right.Slug() != "a.b-c" {
		t.Fatalf("slugs %q and %q", left.Slug(), right.Slug())
	}
}

func TestParseIsTheInverseOfString(t *testing.T) {
	id := scope.ID{Project: "demo", Environment: "prod"}
	got, err := scope.Parse(id.String())
	if err != nil || got != id {
		t.Fatalf("Parse(%q) = %+v, %v", id.String(), got, err)
	}
	for _, s := range []string{"prod", "a/b/c", "A/prod"} {
		if _, err := scope.Parse(s); err == nil {
			t.Errorf("Parse(%q) accepted", s)
		}
	}
}

func TestAPIPathEscapesNothingAValidIDCanHold(t *testing.T) {
	id := scope.ID{Project: "a-b_c", Environment: "d-e"}
	want := "/v1/projects/a-b_c/environments/d-e/"
	if got := id.APIPath(); got != want {
		t.Fatalf("APIPath = %q, want %q", got, want)
	}
}

func TestValidProjectIsTheManifestRule(t *testing.T) {
	cases := []struct {
		name string
		ok   bool
	}{
		{"demo", true},
		{"a_b", true},
		{"a-1", true},
		{"prod_1", true},
		{strings.Repeat("a", 63), true},
		{"", false},
		{"-prod", false},
		{"Demo", false},
		{strings.Repeat("a", 64), false},
	}
	for _, c := range cases {
		if got := scope.ValidProject(c.name); got != c.ok {
			t.Errorf("ValidProject(%q) = %v, want %v", c.name, got, c.ok)
		}
		err := loadProject(t, c.name)
		if (err == nil) != c.ok {
			t.Errorf("Load project %q: ValidProject=%v err=%v", c.name, scope.ValidProject(c.name), err)
		}
	}
}

func TestValidEnvironmentAtItsBoundaries(t *testing.T) {
	for _, name := range []string{"prod", strings.Repeat("a", 63), "a-1"} {
		if !scope.ValidEnvironment(name) {
			t.Errorf("ValidEnvironment(%q) = false", name)
		}
	}
	for _, name := range []string{"-prod", "prod_1", strings.Repeat("a", 64), ""} {
		if scope.ValidEnvironment(name) {
			t.Errorf("ValidEnvironment(%q) = true", name)
		}
	}
}

func loadProject(t *testing.T, project string) error {
	t.Helper()
	dir := t.TempDir()
	text := "project: " + strconv.Quote(project) + "\nnetwork: demo\napps:\n  api:\n    kind: image\n    image: ghcr.io/acme/api\n    roles:\n      web: {}\n"
	if err := os.WriteFile(filepath.Join(dir, "berthy.yml"), []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := manifest.Load(dir, "prod")
	return err
}
