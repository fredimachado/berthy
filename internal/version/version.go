// Package version holds the version of this Berthy build. A release build stamps Version at link time; any other build
// keeps "dev".
package version

import (
	"runtime/debug"
	"strings"
)

// Version is set at build time: go build -ldflags "-X berthy/internal/version.Version=v1.0.0". Unset, it is "dev".
var Version = "dev"

// String is the version to show: Version, and for a dev build the commit it was built from when the build recorded one
// ("dev (8649d6e)", "dev (8649d6e, modified)").
func String() string {
	info, ok := debug.ReadBuildInfo()
	return describe(Version, info, ok)
}

// Release is true for a build stamped with a release version.
func Release() bool { return IsRelease(Version) }

// IsRelease is true for a release version: one that starts with "v" and is not "dev".
func IsRelease(v string) bool { return strings.HasPrefix(v, "v") && v != "dev" }

// describe is String for the given version and build information. ok is false when the build has none.
func describe(v string, info *debug.BuildInfo, ok bool) string {
	if IsRelease(v) {
		return v
	}
	if v == "" {
		v = "dev"
	}
	if !ok || info == nil {
		return v
	}
	var revision string
	var modified bool
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			revision = s.Value
		case "vcs.modified":
			modified = s.Value == "true"
		}
	}
	if revision == "" {
		return v
	}
	if len(revision) > 7 {
		revision = revision[:7]
	}
	if modified {
		return v + " (" + revision + ", modified)"
	}
	return v + " (" + revision + ")"
}
