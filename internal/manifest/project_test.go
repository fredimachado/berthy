package manifest

import (
	"path/filepath"
	"testing"
)

func TestProjectNameReadsTheBaseFileOnly(t *testing.T) {
	dir := writeFiles(t, map[string]string{
		"berthy.yml":      "project: demo\nnetwork: demo\napps: nonsense\n",
		"berthy.prod.yml": "project: other\n",
	})
	name, ok, err := ProjectName(dir)
	if name != "demo" || !ok || err != nil {
		t.Fatalf("ProjectName = %q, %v, %v", name, ok, err)
	}
}

func TestProjectNameIsAbsentWithoutAManifest(t *testing.T) {
	name, ok, err := ProjectName(t.TempDir())
	if name != "" || ok || err != nil {
		t.Fatalf("ProjectName = %q, %v, %v", name, ok, err)
	}
	name, ok, err = ProjectName(filepath.Join(t.TempDir(), "missing"))
	if name != "" || ok || err != nil {
		t.Fatalf("a missing directory: ProjectName = %q, %v, %v", name, ok, err)
	}
}

func TestProjectNameRefusesAnInvalidProject(t *testing.T) {
	for name, body := range map[string]string{
		"missing":    "network: demo\n",
		"invalid":    "project: Not Valid\n",
		"not text":   "project: [a]\n",
		"unreadable": "project: [\n",
	} {
		dir := writeFiles(t, map[string]string{"berthy.yml": body})
		got, ok, err := ProjectName(dir)
		if err == nil || !ok || got != "" {
			t.Errorf("%s: ProjectName = %q, %v, %v", name, got, ok, err)
		}
	}
}
