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
	"berthy/internal/version"
)

const agentRefusal = "this manifest needs Berthy 99.0.0 or newer; this agent runs v0.8.0: upgrade it with berthy setup"

// stampAgent sets the agent's release version for the test and restores it afterwards.
func stampAgent(t *testing.T, v string) {
	t.Helper()
	saved := version.Version
	version.Version = v
	t.Cleanup(func() { version.Version = saved })
}

// newerManifest is the docker manifest of web, with a min_version no release in these tests meets.
func newerManifest() manifest.Manifest {
	m := dockerManifest(nil)
	m.MinVersion = "99.0.0"
	return m
}

// needsBerthy reports whether err is a version refusal of either kind.
func needsBerthy(err error) bool {
	return err != nil && strings.Contains(err.Error(), "needs Berthy")
}

func TestAgentRefusesAManifestNewerThanItsRelease(t *testing.T) {
	stampAgent(t, "v0.8.0")
	s := openStore(t)
	c := &Controller{Engine: &boundFake{}, Store: s, Environment: "test"}
	ctx := context.Background()
	actor := Actor{ID: "admin", Admin: true}

	_, err := c.Plan(ctx, actor, Request{RequestID: "plan", App: "web", Version: "v1", DryRun: true, Manifest: newerManifest()})
	var refusal Refusal
	if !errors.As(err, &refusal) || string(refusal) != agentRefusal {
		t.Errorf("deploy plan: %v", err)
	}
	op, _, err := c.Admit(ctx, actor, Request{RequestID: "admit", App: "web", Version: "v1", Manifest: newerManifest()})
	if !errors.As(err, &refusal) || string(refusal) != agentRefusal {
		t.Errorf("deploy admit: %v", err)
	}
	if err == nil {
		waitOperation(t, s, op.ID)
	}
	_, err = c.Plan(ctx, actor, Request{RequestID: "ingress-plan", Kind: "ingress", DryRun: true, Manifest: newerManifest()})
	if !errors.As(err, &refusal) || string(refusal) != agentRefusal {
		t.Errorf("ingress plan: %v", err)
	}
	_, _, err = c.Admit(ctx, actor, Request{RequestID: "ingress-admit", Kind: "ingress", Manifest: newerManifest()})
	if !errors.As(err, &refusal) || string(refusal) != agentRefusal {
		t.Errorf("ingress admit: %v", err)
	}
}

func TestAgentSkipsTheVersionCheckInADevelopmentBuild(t *testing.T) {
	stampAgent(t, "dev")
	s := openStore(t)
	c := &Controller{Engine: &boundFake{}, Store: s, Environment: "test"}
	ctx := context.Background()
	actor := Actor{ID: "admin", Admin: true}

	_, err := c.Plan(ctx, actor, Request{RequestID: "plan", App: "web", Version: "v1", DryRun: true, Manifest: newerManifest()})
	if needsBerthy(err) {
		t.Errorf("deploy plan: %v", err)
	}
	op, _, err := c.Admit(ctx, actor, Request{RequestID: "admit", App: "web", Version: "v1", Manifest: newerManifest()})
	if needsBerthy(err) {
		t.Errorf("deploy admit: %v", err)
	}
	if err == nil {
		waitOperation(t, s, op.ID)
	}
	_, err = c.Plan(ctx, actor, Request{RequestID: "ingress-plan", Kind: "ingress", DryRun: true, Manifest: newerManifest()})
	if needsBerthy(err) {
		t.Errorf("ingress plan: %v", err)
	}
}

// seedStoredRelease records a succeeded deploy of web at digest whose stored manifest is m.
func seedStoredRelease(t *testing.T, s *state.Store, requestID, digest string, m manifest.Manifest) {
	t.Helper()
	ctx := context.Background()
	op, _, err := s.Admit(ctx, state.Admission{RequestID: requestID, ActorID: "admin", Kind: "deploy", App: "web", PublicRequest: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(m)
	if err = s.Finish(ctx, op.ID, state.Outcome{State: "succeeded", Release: &state.Release{App: "web", Digest: digest, Manifest: raw}}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(3 * time.Millisecond)
}

// TestRollbackDoesNotCheckTheStoredManifest: a rollback runs a manifest the agent stored earlier, so the request's
// manifest is not what the version check reads.
func TestRollbackDoesNotCheckTheStoredManifest(t *testing.T) {
	stampAgent(t, "v0.8.0")
	s := openStore(t)
	seedStoredRelease(t, s, "older", keepDigest("a"), newerManifest())
	seedStoredRelease(t, s, "current", keepDigest("b"), dockerManifest(nil))
	c := &Controller{Engine: &boundFake{}, Store: s, Environment: "test"}

	if _, err := c.Plan(context.Background(), Actor{ID: "admin", Admin: true}, Request{RequestID: "back", App: "web", Kind: "rollback", DryRun: true}); err != nil {
		t.Fatalf("rollback plan: %v", err)
	}
}
