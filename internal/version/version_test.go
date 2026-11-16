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
