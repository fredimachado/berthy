package version

import (
	"runtime/debug"
	"strings"
	"testing"
)

func buildInfo(revision string, modified string) *debug.BuildInfo {
	info := &debug.BuildInfo{}
	if revision != "" {
		info.Settings = append(info.Settings, debug.BuildSetting{Key: "vcs.revision", Value: revision})
	}
	if modified != "" {
		info.Settings = append(info.Settings, debug.BuildSetting{Key: "vcs.modified", Value: modified})
	}
	return info
}

func TestDevBuildSaysDevWithoutBuildInfo(t *testing.T) {
	if got := describe("dev", nil, false); got != "dev" {
		t.Fatalf("got %q", got)
	}
}

func TestDevBuildShowsTheCommitItWasBuiltFrom(t *testing.T) {
	long := "8649d6e1b2c3d4e5f60718293a4b5c6d7e8f9012"
	for _, tc := range []struct {
		name string
		info *debug.BuildInfo
		want string
	}{
		{"revision", buildInfo(long, "false"), "dev (8649d6e)"},
		{"modified", buildInfo(long, "true"), "dev (8649d6e, modified)"},
		{"no modified setting", buildInfo(long, ""), "dev (8649d6e)"},
		{"short revision", buildInfo("abc", ""), "dev (abc)"},
		{"no revision", buildInfo("", "true"), "dev"},
	} {
		if got := describe("dev", tc.info, true); got != tc.want {
			t.Errorf("%s: got %q want %q", tc.name, got, tc.want)
		}
	}
}

func TestReleaseVersionIsShownAsStamped(t *testing.T) {
	if got := describe("v1.0.0", buildInfo("8649d6e1b2c3", "true"), true); got != "v1.0.0" {
		t.Fatalf("got %q", got)
	}
}

func TestStringIsNeverEmpty(t *testing.T) {
	if got := String(); !strings.HasPrefix(got, "dev") {
		t.Fatalf("unstamped build says %q", got)
	}
}

func TestReleaseNeedsAVersionStartingWithV(t *testing.T) {
	for v, want := range map[string]bool{"v1.0.0": true, "v0.0.0-test": true, "dev": false, "": false, "1.0.0": false, "dev (8649d6e)": false} {
		if got := IsRelease(v); got != want {
			t.Errorf("IsRelease(%q) = %v, want %v", v, got, want)
		}
	}
	if Release() {
		t.Fatal("the unstamped test build is a release")
	}
	saved := Version
	defer func() { Version = saved }()
	Version = "v1.2.3"
	if !Release() {
		t.Fatal("a build stamped v1.2.3 is not a release")
	}
	if got := String(); got != "v1.2.3" {
		t.Fatalf("stamped String() = %q", got)
	}
}

func TestCompareOrdersReleaseVersions(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want int
	}{
		{"0.9.0", "v0.9.0", 0},
		{"v0.9.0", "0.9.0", 0},
		{"v0.8.2", "v0.9.0", -1},
		{"v0.9.0", "v0.8.2", 1},
		{"v1.0.0-rc.1", "v1.0.0", -1},
		{"v1.0.0", "v1.0.0-rc.1", 1},
		{"v1.0.0-rc.2", "v1.0.0-rc.10", -1},
		{"v1.0.0-alpha", "v1.0.0-1", 1},
		{"v1.10.0", "v1.9.0", 1},
		{"v10.0.0", "v9.99.99", 1},
		{"v1.0.0-rc.1", "v1.0.0-rc.1", 0},
		{"v1.3.0+build.1", "v1.2.3", 1},
		{"v1.3.0+a", "v1.3.0+b", 0},
		{"v1.0.0-rc.1+build.1", "v1.0.0", -1},
	} {
		got, ok := Compare(tc.a, tc.b)
		if !ok || got != tc.want {
			t.Errorf("Compare(%q, %q) = %d, %v; want %d, true", tc.a, tc.b, got, ok, tc.want)
		}
	}
}

func TestCompareRefusesWhatIsNotAReleaseVersion(t *testing.T) {
	for _, v := range []string{"dev", "dev (abc1234)", "", "v1", "v1.2", "vx.y.z", "v1.2.3.4"} {
		if _, ok := Compare(v, "v0.9.0"); ok {
			t.Errorf("Compare(%q, v0.9.0) is ok; want not ok", v)
		}
		if _, ok := Compare("v0.9.0", v); ok {
			t.Errorf("Compare(v0.9.0, %q) is ok; want not ok", v)
		}
	}
}

func TestSatisfiesComparesARunningReleaseWithAMinimum(t *testing.T) {
	if ok, known := Satisfies("v0.9.0", "0.9.0"); !known || !ok {
		t.Errorf("v0.9.0 satisfies 0.9.0: got %v, %v", ok, known)
	}
	if ok, known := Satisfies("v0.8.2", "0.9.0"); !known || ok {
		t.Errorf("v0.8.2 satisfies 0.9.0: got %v, %v", ok, known)
	}
	if _, known := Satisfies("dev (abc1234)", "0.9.0"); known {
		t.Errorf("a development build is known to satisfy a minimum")
	}
}
