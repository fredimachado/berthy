package deploy

import (
	"context"
	"errors"
	"strings"
	"testing"

	"berthy/internal/state"
)

func keepDigest(c string) string { return "sha256:" + strings.Repeat(c, 64) }

// TestKeepVersionDeploysTheAppliedDigestAndTagWithTheNewManifest: a keep_version deploy runs the digest the app runs now,
// records its tag, and applies the manifest it was sent.
func TestKeepVersionDeploysTheAppliedDigestAndTagWithTheNewManifest(t *testing.T) {
	s := openStore(t)
	seedTaggedRelease(t, s, "older", keepDigest("a"), "v1.0.1")
	seedTaggedRelease(t, s, "current", keepDigest("b"), "v1.0.2")
	c := &Controller{Engine: &boundFake{}, Store: s, Environment: "test"}
	m := dockerManifest(map[string]string{"GREETING": "changed"})

	plan, err := c.Plan(context.Background(), Actor{ID: "admin", Admin: true}, Request{RequestID: "plan", App: "web", KeepVersion: true, DryRun: true, Manifest: m})
	if err != nil || !strings.HasSuffix(plan.Image, "@"+keepDigest("b")) || plan.Tag != "v1.0.2" {
		t.Fatalf("plan image %q tag %q: %v", plan.Image, plan.Tag, err)
	}

	op := runBound(t, s, c, Request{RequestID: "keep", App: "web", KeepVersion: true, Manifest: m})
	if op.State != "succeeded" {
		t.Fatalf("state %s: %s", op.State, op.Err)
	}
	release, err := s.Applied(context.Background(), "web")
	if err != nil || release.Tag != "v1.0.2" || !strings.HasSuffix(release.Digest, "@"+keepDigest("b")) {
		t.Fatalf("release %s tag %q: %v", release.Digest, release.Tag, err)
	}
	if !strings.Contains(string(release.Manifest), `"GREETING":"changed"`) {
		t.Fatalf("the new manifest was not applied: %s", release.Manifest)
	}
	if release.ID == 0 || release.OperationID != op.ID {
		t.Fatalf("no new release was recorded: %+v", release)
	}
}

func TestKeepVersionKeepsADigestDeployWithoutATag(t *testing.T) {
	s := openStore(t)
	seedRelease(t, s, "current", keepDigest("b"))
	c := &Controller{Engine: &boundFake{}, Store: s, Environment: "test"}
	op := runBound(t, s, c, Request{RequestID: "keep", App: "web", KeepVersion: true, Manifest: dockerManifest(nil)})
	if op.State != "succeeded" {
		t.Fatalf("state %s: %s", op.State, op.Err)
	}
	release, err := s.Applied(context.Background(), "web")
	if err != nil || release.Tag != "" || !strings.HasSuffix(release.Digest, "@"+keepDigest("b")) {
		t.Fatalf("release %s tag %q: %v", release.Digest, release.Tag, err)
	}
}

func TestKeepVersionRefusals(t *testing.T) {
	s := openStore(t)
	c := &Controller{Engine: &boundFake{}, Store: s, Environment: "test"}
	ctx := context.Background()
	actor := Actor{ID: "admin", Admin: true}
	for _, tc := range []struct {
		name string
		req  Request
		want string
	}{
		{"never deployed", Request{RequestID: "a", App: "web", KeepVersion: true, Manifest: dockerManifest(nil)}, "app web is not deployed yet: give --version web=REF"},
		{"with a version", Request{RequestID: "b", App: "web", KeepVersion: true, Version: "v1", Manifest: dockerManifest(nil)}, "keep_version and version are exclusive"},
		{"compose app", Request{RequestID: "c", App: "db", KeepVersion: true, Manifest: composeApp(false), ComposeFile: "services: {}\n"}, "keep_version applies to an image app"},
		{"other kind", Request{RequestID: "d", App: "web", Kind: "restart", KeepVersion: true, Manifest: dockerManifest(nil)}, "keep_version applies to a deploy"},
		{"adoption", Request{RequestID: "e", App: "web", Adopt: true, KeepVersion: true, Manifest: dockerManifest(nil)}, "keep_version applies to a deploy"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var refusal Refusal
			_, _, err := c.Admit(ctx, actor, tc.req)
			if !errors.As(err, &refusal) || string(refusal) != tc.want {
				t.Fatalf("Admit error %v, want %q", err, tc.want)
			}
			tc.req.DryRun = true
			_, err = c.Plan(ctx, actor, tc.req)
			if !errors.As(err, &refusal) || string(refusal) != tc.want {
				t.Fatalf("Plan error %v, want %q", err, tc.want)
			}
		})
	}
}

func TestKeepVersionIsAllowedOnAProtectedEnvironment(t *testing.T) {
	s := openStore(t)
	seedTaggedRelease(t, s, "current", keepDigest("b"), "v1.0.2")
	c := &Controller{Engine: &boundFake{}, Store: s, Environment: "test", Protected: true}
	op := runBound(t, s, c, Request{RequestID: "keep", App: "web", KeepVersion: true, Manifest: dockerManifest(nil)})
	if op.State != "succeeded" {
		t.Fatalf("state %s: %s", op.State, op.Err)
	}
}

// TestKeepVersionStillRefusesAnImageOfAnotherRepository: the applied digest is checked against the manifest's image like any
// version.
func TestKeepVersionStillRefusesAnImageOfAnotherRepository(t *testing.T) {
	s := openStore(t)
	seedRelease(t, s, "current", "other/repo@"+keepDigest("b"))
	c := &Controller{Engine: &boundFake{}, Store: s, Environment: "test"}
	_, err := c.Plan(context.Background(), Actor{ID: "admin", Admin: true}, Request{RequestID: "plan", App: "web", KeepVersion: true, DryRun: true, Manifest: dockerManifest(nil)})
	if err == nil || !strings.Contains(err.Error(), "image repository does not match app") {
		t.Fatalf("error %v", err)
	}
}

// TestKeepVersionIsPartOfTheRequestHash: the same request id with keep_version is the same request, and with a version in
// its place a different one.
func TestKeepVersionIsPartOfTheRequestHash(t *testing.T) {
	s := openStore(t)
	seedRelease(t, s, "current", keepDigest("b"))
	c := &Controller{Engine: &boundFake{}, Store: s, Environment: "test"}
	ctx := context.Background()
	keep := Request{RequestID: "same", App: "web", KeepVersion: true, Manifest: dockerManifest(nil)}
	op := runBound(t, s, c, keep)
	if op.State != "succeeded" {
		t.Fatalf("state %s: %s", op.State, op.Err)
	}
	again, created, err := c.Admit(ctx, Actor{ID: "admin"}, keep)
	if err != nil || created || again.ID != op.ID {
		t.Fatalf("a retry of the same request: op %s created %v: %v", again.ID, created, err)
	}
	explicit := keep
	explicit.KeepVersion = false
	explicit.Version = keepDigest("b")
	if _, _, err = c.Admit(ctx, Actor{ID: "admin"}, explicit); !errors.Is(err, state.ErrConflict) {
		t.Fatalf("the same id with a version: %v", err)
	}
}
