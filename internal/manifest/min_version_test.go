package manifest

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"berthy/internal/version"
)

// minVersionYAML is a manifest with one image app, with a min_version line for minVersion. An empty minVersion writes no line.
func minVersionYAML(minVersion string) string {
	line := ""
	if minVersion != "" {
		line = "min_version: " + minVersion + "\n"
	}
	return "project: demo\nnetwork: demo\n" + line + "apps:\n  api:\n    kind: image\n    image: ghcr.io/acme/api\n    roles:\n      web: {}\n"
}

func loadYAML(t *testing.T, content string) (*Manifest, error) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "berthy.yml"), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return Load(dir, "prod")
}

// stampVersion sets the CLI's version for the test and restores it afterwards.
func stampVersion(t *testing.T, v string) {
	t.Helper()
	saved := version.Version
	version.Version = v
	t.Cleanup(func() { version.Version = saved })
}

func TestMinVersionIsReadFromTheManifest(t *testing.T) {
	m, err := loadYAML(t, minVersionYAML("0.9.0"))
	if err != nil {
		t.Fatal(err)
	}
	if m.MinVersion != "0.9.0" {
		t.Fatalf("MinVersion %q", m.MinVersion)
	}
}

func TestMinVersionFormatIsChecked(t *testing.T) {
	for _, v := range []string{"0.9.0", "v0.9.0", "1.0.0-rc.1", `"0.9.0"`} {
		if _, err := loadYAML(t, minVersionYAML(v)); err != nil {
			t.Errorf("min_version %s: %v", v, err)
		}
	}
	for _, v := range []string{"latest", "0.9.x", "0.9.0.1", "^0.9.0", "v", "0.9.0-"} {
		_, err := loadYAML(t, minVersionYAML(v))
		if err == nil || !strings.Contains(err.Error(), "invalid min_version") || !strings.Contains(err.Error(), "use a version such as 0.9.0") {
			t.Errorf("min_version %s: error %v", v, err)
		}
	}
}

func TestMinVersionMissingMarshalsWithoutTheKey(t *testing.T) {
	m, err := loadYAML(t, minVersionYAML(""))
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "min_version") {
		t.Fatalf("a manifest without min_version marshals with the key: %s", b)
	}
}

func TestCheckVersionRefusesAManifestAboveTheRunningRelease(t *testing.T) {
	for _, tc := range []struct {
		name, minVersion, running string
		refused                   bool
	}{
		{"equal", "0.9.0", "v0.9.0", false},
		{"newer running", "0.9.0", "v1.0.0", false},
		{"older running", "0.9.0", "v0.8.2", true},
		{"pre-release below", "1.0.0", "v1.0.0-rc.1", true},
		{"development build", "99.0.0", "dev", false},
		{"development build with commit", "99.0.0", "dev (abc1234)", false},
		{"no min_version", "", "v0.1.0", false},
	} {
		m := Manifest{MinVersion: tc.minVersion}
		err := m.CheckVersion(tc.running)
		var old *TooOld
		if tc.refused != errors.As(err, &old) {
			t.Errorf("%s: err %v", tc.name, err)
			continue
		}
		if tc.refused && (old.Need != tc.minVersion || old.Have != tc.running) {
			t.Errorf("%s: TooOld %+v", tc.name, old)
		}
	}
}

func TestLoadRefusesAManifestTheCLIIsTooOldFor(t *testing.T) {
	stampVersion(t, "v0.9.0")
	_, err := loadYAML(t, minVersionYAML("99.0.0"))
	want := "this manifest needs Berthy 99.0.0 or newer; this CLI is v0.9.0"
	if err == nil || err.Error() != want {
		t.Fatalf("error %v, want %q", err, want)
	}
}

func TestLoadAcceptsAManifestTheCLIMeets(t *testing.T) {
	stampVersion(t, "v0.9.0")
	if _, err := loadYAML(t, minVersionYAML("0.9.0")); err != nil {
		t.Fatal(err)
	}
}

func TestLoadSkipsTheVersionCheckInADevelopmentBuild(t *testing.T) {
	stampVersion(t, "dev")
	if _, err := loadYAML(t, minVersionYAML("99.0.0")); err != nil {
		t.Fatal(err)
	}
}
