package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"berthy/internal/manifest"
	"berthy/internal/state"
)

const localChangesRefusal = "the manifest has local changes: commit them first"

// seedCommitRelease records a succeeded deploy of web of digest from commit, which is flagged when it was deployed from a
// checkout with local changes.
func seedCommitRelease(t *testing.T, s *state.Store, requestID, digest, commit string, modified bool) {
	t.Helper()
	ctx := context.Background()
	op, _, err := s.Admit(ctx, state.Admission{RequestID: requestID, ActorID: "admin", Kind: "deploy", App: "web", Commit: commit, Modified: modified, PublicRequest: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(dockerManifest(nil))
	release := &state.Release{App: "web", Digest: digest, Commit: commit, Modified: modified, Manifest: raw}
	if err = s.Finish(ctx, op.ID, state.Outcome{State: "succeeded", Release: release}); err != nil {
		t.Fatal(err)
	}
	// Later operations must start after this one, so the timestamps order them.
	time.Sleep(3 * time.Millisecond)
}

// TestModifiedDeployIsRefusedOnAProtectedEnvironment: a deploy from a checkout with local changes is refused, before it starts,
// when the environment is protected by the agent, by the manifest, or by an earlier protected deploy.
func TestModifiedDeployIsRefusedOnAProtectedEnvironment(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	actor := Actor{ID: "admin", Admin: true}
	protected := dockerManifest(nil)
	protected.Protected = true
	for _, tc := range []struct {
		name string
		c    *Controller
		m    manifest.Manifest
	}{
		{"by the agent", &Controller{Engine: &boundFake{}, Store: s, Environment: "test", Protected: true}, dockerManifest(nil)},
		{"by the manifest", &Controller{Engine: &boundFake{}, Store: s, Environment: "test"}, protected},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := Request{RequestID: "modified", App: "web", Version: "sha256:" + strings.Repeat("b", 64), Commit: "abc123", Modified: true, Manifest: tc.m}
			var refusal Refusal
			if _, _, err := admit(t, tc.c, ctx, actor, req); !errors.As(err, &refusal) || string(refusal) != localChangesRefusal {
				t.Fatalf("Admit: %v", err)
			}
			req.DryRun = true
			if _, err := tc.c.Plan(ctx, actor, req); !errors.As(err, &refusal) || string(refusal) != localChangesRefusal {
				t.Fatalf("Plan: %v", err)
			}
		})
	}

	// A protected deploy keeps the environment protected, so a later one from local changes is refused too.
	seedCommitRelease(t, s, "clean", "sha256:"+strings.Repeat("a", 64), "abc123", false)
	ok, _, err := s.Admit(ctx, state.Admission{RequestID: "protect", ActorID: "admin", Kind: "deploy", App: "web", PublicRequest: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Finish(ctx, ok.ID, state.Outcome{State: "succeeded", Protected: true, Release: &state.Release{App: "web", Digest: "sha256:" + strings.Repeat("a", 64), Manifest: []byte(`{}`)}}); err != nil {
		t.Fatal(err)
	}
	c := &Controller{Engine: &boundFake{}, Store: s, Environment: "test"}
	var refusal Refusal
	req := Request{RequestID: "sticky", App: "web", Version: "sha256:" + strings.Repeat("b", 64), Modified: true, Manifest: dockerManifest(nil)}
	if _, _, err = admit(t, c, ctx, actor, req); !errors.As(err, &refusal) || string(refusal) != localChangesRefusal {
		t.Fatalf("sticky protection: %v", err)
	}
}

// TestModifiedDeployOnAnUnprotectedEnvironmentIsRecorded: the release and the operation keep the flag and the commit.
func TestModifiedDeployOnAnUnprotectedEnvironmentIsRecorded(t *testing.T) {
	s := openStore(t)
	c := &Controller{Engine: &boundFake{}, Store: s, Environment: "test"}
	op := runBound(t, s, c, Request{RequestID: "modified", App: "web", Version: "sha256:" + strings.Repeat("b", 64), Commit: "abc123", Modified: true, Manifest: dockerManifest(nil)})
	if op.State != "succeeded" || !op.Modified || op.Commit != "abc123" {
		t.Fatalf("operation: %+v %s", op, op.Err)
	}
	release, err := s.Applied(context.Background(), "web")
	if err != nil || !release.Modified || release.Commit != "abc123" {
		t.Fatalf("release: %+v %v", release, err)
	}
}

// TestRollbackCarriesTheFlagOfTheReleaseItReturnsTo: a rollback is a deploy of that release, so it is flagged the same way and
// a protected environment refuses it.
func TestRollbackCarriesTheFlagOfTheReleaseItReturnsTo(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	seedCommitRelease(t, s, "older", keepDigest("a"), "oldsha", true)
	seedCommitRelease(t, s, "newer", keepDigest("b"), "newsha", false)
	c := &Controller{Engine: &boundFake{}, Store: s, Environment: "test"}
	resolved, err := c.rollbackRequest(ctx, Request{App: "web", Kind: "rollback"})
	if err != nil || !resolved.Modified || resolved.Commit != "oldsha" {
		t.Fatalf("resolved rollback: %+v %v", resolved, err)
	}
	protected := &Controller{Engine: &boundFake{}, Store: s, Environment: "test", Protected: true}
	actor := Actor{ID: "admin", Admin: true}
	var refusal Refusal
	if _, _, err = admit(t, protected, ctx, actor, Request{RequestID: "rollback", App: "web", Kind: "rollback"}); !errors.As(err, &refusal) || string(refusal) != localChangesRefusal {
		t.Fatalf("Admit of a rollback to local changes: %v", err)
	}
	if _, err = protected.Plan(ctx, actor, Request{App: "web", Kind: "rollback", DryRun: true}); !errors.As(err, &refusal) || string(refusal) != localChangesRefusal {
		t.Fatalf("Plan of a rollback to local changes: %v", err)
	}
}
