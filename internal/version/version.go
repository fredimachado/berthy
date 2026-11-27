// Package version holds the version of this Berthy build. A release build stamps Version at link time; any other build
// keeps "dev".
package version

import (
	"regexp"
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

// releasePattern is a release version: an optional "v", three numbers, an optional pre-release, and an optional
// build-metadata suffix (+...) that semver ignores for precedence.
var releasePattern = regexp.MustCompile(`^v?([0-9]+)\.([0-9]+)\.([0-9]+)(?:-([0-9A-Za-z.-]+))?(?:\+[0-9A-Za-z.-]+)?$`)

// Compare compares two release versions, such as "v0.9.0", "0.9.0" or "v1.0.0-rc.1", by semantic versioning: the numbers of
// the core in turn, then a pre-release before its release. Pre-release identifiers compare as numbers when both are numbers,
// and as text otherwise. ok is false when either is not a release version, including "dev" and "dev (abc1234)"; the
// comparison is then meaningless and callers skip the check.
func Compare(a, b string) (cmp int, ok bool) {
	va, okA := parseRelease(a)
	vb, okB := parseRelease(b)
	if !okA || !okB {
		return 0, false
	}
	for i := range va.core {
		if c := compareNumbers(va.core[i], vb.core[i]); c != 0 {
			return c, true
		}
	}
	return comparePre(va.pre, vb.pre), true
}

// Satisfies reports whether running is at least minimum. ok is false when running or minimum is not a release version, such as
// a development build; callers skip the check then.
func Satisfies(running, minimum string) (satisfied, ok bool) {
	c, ok := Compare(running, minimum)
	return ok && c >= 0, ok
}

// release is the parsed form of a release version: the numbers of its core, and its pre-release, empty for a release.
type release struct {
	core [3]string
	pre  string
}

func parseRelease(v string) (release, bool) {
	m := releasePattern.FindStringSubmatch(v)
	if m == nil {
		return release{}, false
	}
	return release{core: [3]string{m[1], m[2], m[3]}, pre: m[4]}, true
}

// compareNumbers compares two decimal numbers of any length, without the bounds of an integer type.
func compareNumbers(a, b string) int {
	a, b = strings.TrimLeft(a, "0"), strings.TrimLeft(b, "0")
	if len(a) != len(b) {
		if len(a) < len(b) {
			return -1
		}
		return 1
	}
	return strings.Compare(a, b)
}

// comparePre compares two pre-release strings. An empty one is a release, which comes after every pre-release of its version.
func comparePre(a, b string) int {
	switch {
	case a == b:
		return 0
	case a == "":
		return 1
	case b == "":
		return -1
	}
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) && i < len(bs); i++ {
		x, y := as[i], bs[i]
		xn, yn := isNumber(x), isNumber(y)
		var c int
		switch {
		case xn && yn:
			c = compareNumbers(x, y)
		case xn:
			c = -1
		case yn:
			c = 1
		default:
			c = strings.Compare(x, y)
		}
		if c != 0 {
			return c
		}
	}
	switch {
	case len(as) < len(bs):
		return -1
	case len(as) > len(bs):
		return 1
	}
	return 0
}

// isNumber reports whether s is a non-empty run of decimal digits.
func isNumber(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

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
