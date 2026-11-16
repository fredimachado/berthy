package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"runtime"
	"strings"
	"testing"

	"berthy/internal/version"
)

func TestVersionPrintsTheBuildAndThePlatform(t *testing.T) {
	saved := version.Version
	defer func() { version.Version = saved }()
	version.Version = "v1.2.3"
	var out, errout bytes.Buffer
	if code := Run([]string{"version"}, &out, &errout); code != 0 {
		t.Fatalf("exit %d: %s", code, errout.String())
	}
	if want := fmt.Sprintf("berthy v1.2.3 %s/%s\n", runtime.GOOS, runtime.GOARCH); out.String() != want {
		t.Fatalf("output %q, want %q", out.String(), want)
	}
}

func TestVersionJSONSaysWhetherItIsARelease(t *testing.T) {
	saved := version.Version
	defer func() { version.Version = saved }()
	for _, tc := range []struct {
		stamped string
		release bool
	}{{"v1.2.3", true}, {"dev", false}} {
		version.Version = tc.stamped
		var out, errout bytes.Buffer
		if code := Run([]string{"version", "--json"}, &out, &errout); code != 0 {
			t.Fatalf("%s: exit %d: %s", tc.stamped, code, errout.String())
		}
		var v struct {
			Version string `json:"version"`
			Release bool   `json:"release"`
			OS      string `json:"os"`
			Arch    string `json:"arch"`
		}
		if err := json.Unmarshal(out.Bytes(), &v); err != nil {
			t.Fatalf("%s: %v in %q", tc.stamped, err, out.String())
		}
		if v.Version != version.String() || v.Release != tc.release || v.OS != runtime.GOOS || v.Arch != runtime.GOARCH {
			t.Errorf("%s: %+v", tc.stamped, v)
		}
		if !strings.Contains(out.String(), "\n  \"version\"") {
			t.Errorf("%s: JSON is not indented like the other commands: %q", tc.stamped, out.String())
		}
	}
}

func TestVersionRefusesArgumentsAndUnknownFlags(t *testing.T) {
	for _, args := range [][]string{
		{"version", "extra"},
		{"version", "--json", "extra"},
		{"version", "-e", "dev"},
		{"version", "--config", "x.json"},
		{"version", "--verbose"},
	} {
		var out, errout bytes.Buffer
		if code := Run(args, &out, &errout); code != 2 || out.Len() != 0 {
			t.Errorf("%v: exit %d, output %q, stderr %q", args, code, out.String(), errout.String())
		}
	}
}

func TestVersionIsInTheUsage(t *testing.T) {
	var out, errout bytes.Buffer
	if code := Run(nil, &out, &errout); code != 2 || !strings.Contains(errout.String(), "version") {
		t.Fatalf("usage: exit %d %q", code, errout.String())
	}
}
