package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"berthy/internal/scope"
	"berthy/internal/state"
)

func TestHasAppliedAppsDispatchValidatesArguments(t *testing.T) {
	for _, args := range [][]string{
		nil,
		{"Alpha", "dev", "/var/lib/berthy/state"},
		{"alpha", "DEV", "/var/lib/berthy/state"},
		{strings.Repeat("a", 55), "development", "/var/lib/berthy/state"},
		{"alpha", "dev", "C:/state"},
		{"alpha", "dev", "/var/lib/../lib/state"},
		{"alpha", "dev", "/var/lib/berthy/state\x00"},
	} {
		var out, diag bytes.Buffer
		code := Run(append([]string{"agent", "has-applied-apps"}, args...), &out, &diag)
		if code != 2 || out.Len() != 0 || !strings.HasPrefix(diag.String(), "has-applied-apps: ") {
			t.Fatalf("args %q: exit %d, stdout %q, stderr %q", args, code, out.String(), diag.String())
		}
	}
}

func TestAppliedAppsProbeIsBoundedAndFailsClosed(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the probe runs on the Linux server")
	}
	ctx := context.Background()
	id := scope.ID{Project: "alpha", Environment: "dev"}
	check := func(dir, want string, wantCode int) {
		t.Helper()
		entries := func() []string {
			t.Helper()
			files, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			var names []string
			for _, file := range files {
				names = append(names, file.Name())
			}
			return names
		}
		before := entries()
		var out, diag bytes.Buffer
		code := printAppliedApps(ctx, dir, id, &out, &diag)
		if code != wantCode || out.String() != want ||
			(code != 0 && diag.String() != "has-applied-apps: could not read applied apps\n") ||
			(code == 0 && diag.Len() != 0) {
			t.Fatalf("probe exit %d, stdout %q, stderr %q", code, out.String(), diag.String())
		}
		if after := entries(); !reflect.DeepEqual(before, after) {
			t.Fatalf("probe changed directory entries: %v -> %v", before, after)
		}
	}
	newStateDir := func() string {
		t.Helper()
		dir := filepath.Join(t.TempDir(), "state")
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	dir := newStateDir()
	check(dir, "", 1)
	if err := os.WriteFile(filepath.Join(dir, "state.db"), []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	check(dir, "", 1)

	dir = newStateDir()
	key := filepath.Join(dir, "key")
	if err := os.WriteFile(key, make([]byte, 32), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := state.Open(dir, key, id.Environment)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	check(dir, "", 1)
	if err = s.Claim(ctx, id.Project); err != nil {
		t.Fatal(err)
	}
	check(dir, "empty\n", 0)
	for i := range 64 {
		app := fmt.Sprintf("app-%d", i)
		op, _, err := s.Admit(ctx, state.Admission{RequestID: app, ActorID: "a", Kind: "deploy", App: app, PublicRequest: []byte(`{}`)})
		if err != nil {
			t.Fatal(err)
		}
		if err = s.Finish(ctx, op.ID, state.Outcome{State: "succeeded", Release: &state.Release{App: app, Digest: "d", Manifest: []byte(`{}`)}}); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			check(dir, "applied\n", 0)
		}
	}
	check(dir, "applied\n", 0)
}
