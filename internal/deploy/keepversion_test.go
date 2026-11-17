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
		{"keep_digest alone", Request{RequestID: "f", App: "web", KeepDigest: keepDigest("a"), Manifest: dockerManifest(nil)}, "keep_digest applies with keep_version"},
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
	checked := keep
	applied, err := s.Applied(ctx, "web")
	if err != nil {
		t.Fatal(err)
	}
	checked.KeepDigest = applied.Digest
	if _, _, err = c.Admit(ctx, Actor{ID: "admin"}, checked); !errors.Is(err, state.ErrConflict) {
		t.Fatalf("the same id with a keep_digest: %v", err)
	}
}

// TestKeepVersionWithTheAppliedDigestDeploysIt: a keep_digest equal to the digest the app runs changes nothing.
func TestKeepVersionWithTheAppliedDigestDeploysIt(t *testing.T) {
	s := openStore(t)
	seedTaggedRelease(t, s, "current", keepDigest("b"), "v1.0.2")
	c := &Controller{Engine: &boundFake{}, Store: s, Environment: "test"}
	m := dockerManifest(nil)
	plan, err := c.Plan(context.Background(), Actor{ID: "admin", Admin: true}, Request{RequestID: "plan", App: "web", KeepVersion: true, KeepDigest: keepDigest("b"), DryRun: true, Manifest: m})
	if err != nil || !strings.HasSuffix(plan.Image, "@"+keepDigest("b")) {
		t.Fatalf("plan image %q: %v", plan.Image, err)
	}
	op := runBound(t, s, c, Request{RequestID: "keep", App: "web", KeepVersion: true, KeepDigest: keepDigest("b"), Manifest: m})
	if op.State != "succeeded" {
		t.Fatalf("state %s: %s", op.State, op.Err)
	}
	release, err := s.Applied(context.Background(), "web")
	if err != nil || release.Tag != "v1.0.2" || !strings.HasSuffix(release.Digest, "@"+keepDigest("b")) || release.OperationID != op.ID {
		t.Fatalf("release %+v: %v", release, err)
	}
}

const keepDigestChanged = "app web changed since its version was read: it runs sha256:bbbbbbbbbbbb now, not sha256:aaaaaaaaaaaa; deploy it again"

// TestKeepVersionRefusesADigestThatChanged: the client read version a, another deploy made it b, and the request is refused
// at once, with nothing deployed.
func TestKeepVersionRefusesADigestThatChanged(t *testing.T) {
	s := openStore(t)
	c := &Controller{Engine: &boundFake{}, Store: s, Environment: "test"}
	m := dockerManifest(nil)
	// What a client reads is the applied release's digest, with its repository.
	seedRelease(t, s, "first", "example/web@"+keepDigest("a"))
	seedRelease(t, s, "second", "example/web@"+keepDigest("b"))
	applied, err := s.Applied(context.Background(), "web")
	if err != nil {
		t.Fatal(err)
	}
	seen := "example/web@" + keepDigest("a")
	req := Request{RequestID: "keep", App: "web", KeepVersion: true, KeepDigest: seen, Manifest: m}
	var refusal Refusal
	if _, _, err = c.Admit(context.Background(), Actor{ID: "admin"}, req); !errors.As(err, &refusal) || string(refusal) != keepDigestChanged {
		t.Fatalf("Admit error %v, want %q", err, keepDigestChanged)
	}
	req.DryRun = true
	if _, err = c.Plan(context.Background(), Actor{ID: "admin"}, req); !errors.As(err, &refusal) || string(refusal) != keepDigestChanged {
		t.Fatalf("Plan error %v, want %q", err, keepDigestChanged)
	}
	if after, err := s.Applied(context.Background(), "web"); err != nil || after.ID != applied.ID {
		t.Fatalf("a release was recorded: %+v: %v", after, err)
	}
}

// TestKeepVersionRefusesAtExecuteWhenTheAppChangedAfterStart: the digest was current when the request was admitted, and
// another deploy landed before it ran. The operation fails with what changed, not with the generic preflight text.
func TestKeepVersionRefusesAtExecuteWhenTheAppChangedAfterStart(t *testing.T) {
	s := openStore(t)
	seedRelease(t, s, "first", keepDigest("a"))
	seedRelease(t, s, "second", keepDigest("b"))
	op, _, err := s.Admit(context.Background(), state.Admission{RequestID: "keep", ActorID: "admin", Kind: "deploy", App: "web", PublicRequest: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	c := &Controller{Engine: &boundFake{}, Store: s, Environment: "test"}
	c.execute(context.Background(), op, Actor{ID: "admin"}, Request{RequestID: "keep", App: "web", KeepVersion: true, KeepDigest: keepDigest("a"), Manifest: dockerManifest(nil)})
	got, err := s.Operation(context.Background(), op.ID)
	if err != nil || got.State != "failed" || got.Err != keepDigestChanged {
		t.Fatalf("operation %+v: %v", got, err)
	}
	if !PublicMessage(got.Err) {
		t.Fatalf("%q would not be shown to a client", got.Err)
	}
}

func TestShortDigestKeepsSha256AndTwelveHexCharacters(t *testing.T) {
	for in, want := range map[string]string{
		keepDigest("a"): "sha256:aaaaaaaaaaaa",
		"docker.io/library/nginx@" + keepDigest("b"): "sha256:bbbbbbbbbbbb",
		"1.4.2":        "1.4.2",
		"sha256:short": "sha256:short",
	} {
		if got := ShortDigest(in); got != want {
			t.Errorf("ShortDigest(%q) = %q, want %q", in, got, want)
		}
	}
}
