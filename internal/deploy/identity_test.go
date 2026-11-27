package deploy

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// specEnv returns the environment of the container of a service spec.
func specEnv(t *testing.T, spec json.RawMessage) []string {
	t.Helper()
	var s struct {
		TaskTemplate struct {
			ContainerSpec struct{ Env []string }
		}
	}
	if err := json.Unmarshal(spec, &s); err != nil {
		t.Fatal(err)
	}
	return s.TaskTemplate.ContainerSpec.Env
}

func TestIdentityEnvOfARoleIsSortedAndComplete(t *testing.T) {
	id := identity{Environment: "stage", Project: "shop", App: "web", Version: "v1", Digest: "sha256:abc"}
	want := []string{"BERTHY_APP=web", "BERTHY_ENVIRONMENT=stage", "BERTHY_IMAGE_DIGEST=sha256:abc", "BERTHY_PROJECT=shop", "BERTHY_ROLE=main", "BERTHY_VERSION=v1"}
	if got := id.env("main"); !reflect.DeepEqual(got, want) {
		t.Fatalf("env(main) = %q, want %q", got, want)
	}
}

func TestIdentityEnvOfAOneOffHasAnEmptyRoleAndVersion(t *testing.T) {
	id := identity{Environment: "stage", Project: "shop", App: "web", Digest: "sha256:abc"}
	want := []string{"BERTHY_APP=web", "BERTHY_ENVIRONMENT=stage", "BERTHY_IMAGE_DIGEST=sha256:abc", "BERTHY_PROJECT=shop", "BERTHY_ROLE=", "BERTHY_VERSION="}
	if got := id.env(""); !reflect.DeepEqual(got, want) {
		t.Fatalf("env(\"\") = %q, want %q", got, want)
	}
}

func TestContainerEnvIsTheAppsVariablesAndTheIdentitySorted(t *testing.T) {
	p := preparedPlan{
		Env:      []string{"MODE=fast", "TOKEN=secret"},
		Identity: identity{Environment: "stage", Project: "shop", App: "web", Version: "v1", Digest: "sha256:abc"},
	}
	want := []string{"BERTHY_APP=web", "BERTHY_ENVIRONMENT=stage", "BERTHY_IMAGE_DIGEST=sha256:abc", "BERTHY_PROJECT=shop", "BERTHY_ROLE=web", "BERTHY_VERSION=v1", "MODE=fast", "TOKEN=secret"}
	if got := p.containerEnv("web"); !reflect.DeepEqual(got, want) {
		t.Fatalf("containerEnv = %q, want %q", got, want)
	}
	if !reflect.DeepEqual(p.Env, []string{"MODE=fast", "TOKEN=secret"}) {
		t.Fatalf("Env changed to %q", p.Env)
	}
}

// TestTagDeployGivesItsServiceTheIdentity: a deploy by tag runs the digest the tag resolves to, and the service's
// containers carry that tag and digest next to the app's own variables.
func TestTagDeployGivesItsServiceTheIdentity(t *testing.T) {
	s := openStore(t)
	f := &boundFake{}
	digest := "sha256:" + strings.Repeat("c", 64)
	c := &Controller{rolloutHold: zeroTestRolloutHold(), Engine: tagResolver{Engine: f, digest: digest}, Store: s, Environment: "test"}
	op := runBound(t, s, c, Request{RequestID: "tag", App: "web", Version: "v1.0.2", Manifest: dockerManifest(map[string]string{"MODE": "fast"})})
	if op.State != "succeeded" {
		t.Fatalf("state %s: %s", op.State, op.Err)
	}
	created := f.createdSpecs()
	if len(created) != 1 {
		t.Fatalf("%d services created", len(created))
	}
	want := []string{"BERTHY_APP=web", "BERTHY_ENVIRONMENT=test", "BERTHY_IMAGE_DIGEST=" + digest, "BERTHY_PROJECT=proj", "BERTHY_ROLE=web", "BERTHY_VERSION=v1.0.2", "MODE=fast"}
	if got := specEnv(t, created[0]); !reflect.DeepEqual(got, want) {
		t.Fatalf("service env %q, want %q", got, want)
	}
}

// TestDigestDeployHasAnEmptyVersion: a digest deploy asks for no tag, so BERTHY_VERSION is empty.
func TestDigestDeployHasAnEmptyVersion(t *testing.T) {
	s := openStore(t)
	f := &boundFake{}
	digest := "sha256:" + strings.Repeat("d", 64)
	c := &Controller{rolloutHold: zeroTestRolloutHold(), Engine: f, Store: s, Environment: "test"}
	op := runBound(t, s, c, Request{RequestID: "digest", App: "web", Version: digest, Manifest: dockerManifest(nil)})
	if op.State != "succeeded" {
		t.Fatalf("state %s: %s", op.State, op.Err)
	}
	created := f.createdSpecs()
	if len(created) != 1 {
		t.Fatalf("%d services created", len(created))
	}
	want := []string{"BERTHY_APP=web", "BERTHY_ENVIRONMENT=test", "BERTHY_IMAGE_DIGEST=" + digest, "BERTHY_PROJECT=proj", "BERTHY_ROLE=web", "BERTHY_VERSION="}
	if got := specEnv(t, created[0]); !reflect.DeepEqual(got, want) {
		t.Fatalf("service env %q, want %q", got, want)
	}
}

// TestRollbackGivesTheIdentityOfTheReleaseItReturnsTo: a rollback runs the tag and digest of that release.
func TestRollbackGivesTheIdentityOfTheReleaseItReturnsTo(t *testing.T) {
	s := openStore(t)
	seedTaggedRelease(t, s, "older", "sha256:"+strings.Repeat("a", 64), "v1.0.1")
	seedTaggedRelease(t, s, "current", "sha256:"+strings.Repeat("b", 64), "v1.0.2", boundBinding("legacy-web", "old-id"))
	f := &boundFake{}
	c := &Controller{rolloutHold: zeroTestRolloutHold(), Engine: f, Store: s, Environment: "test"}
	op := runBound(t, s, c, Request{RequestID: "rollback", App: "web", Kind: "rollback"})
	if op.State != "succeeded" {
		t.Fatalf("state %s: %s", op.State, op.Err)
	}
	created := f.createdSpecs()
	if len(created) != 1 {
		t.Fatalf("%d services created", len(created))
	}
	env := specEnv(t, created[0])
	for _, want := range []string{"BERTHY_VERSION=v1.0.1", "BERTHY_IMAGE_DIGEST=sha256:" + strings.Repeat("a", 64)} {
		if !slices.Contains(env, want) {
			t.Fatalf("rollback env %q lacks %s", env, want)
		}
	}
}

// TestDeploysThatKeepTheVersionGiveTheSameEnvironment: a keep_version deploy of the running release leaves the containers'
// environment as it was, whatever request shape keeps the version.
func TestDeploysThatKeepTheVersionGiveTheSameEnvironment(t *testing.T) {
	digest := "sha256:" + strings.Repeat("c", 64)
	for name, withDigest := range map[string]bool{"keep": false, "keep with its digest": true} {
		t.Run(name, func(t *testing.T) {
			s := openStore(t)
			e := &positiveEngine{}
			c := &Controller{rolloutHold: zeroTestRolloutHold(), Engine: tagResolver{Engine: dockerSpecs{e}, digest: digest}, Store: s, Environment: "test"}
			first := runBound(t, s, c, Request{RequestID: "tag", App: "web", Version: "v1.0.2", Manifest: dockerManifest(nil)})
			if first.State != "succeeded" {
				t.Fatalf("first deploy %s: %s", first.State, first.Err)
			}
			before := liveEnv(t, e)
			if !slices.Contains(before, "BERTHY_VERSION=v1.0.2") {
				t.Fatalf("first deploy environment %q lacks its version", before)
			}
			applied, err := s.Applied(context.Background(), "web")
			if err != nil {
				t.Fatal(err)
			}
			keep := Request{RequestID: "keep", App: "web", KeepVersion: true, Manifest: dockerManifest(nil)}
			if withDigest {
				keep.KeepDigest = applied.Digest
			}
			if op := runBound(t, s, c, keep); op.State != "succeeded" {
				t.Fatalf("keep deploy %s: %s", op.State, op.Err)
			}
			if after := liveEnv(t, e); !reflect.DeepEqual(before, after) {
				t.Fatalf("environment changed by a deploy that keeps the version:\n before %q\n after  %q", before, after)
			}
		})
	}
}

// TestExecutionRefusesAnImageWithoutADigest: an image with no digest has no BERTHY_IMAGE_DIGEST to give, so it is refused
// rather than run with an empty one.
func TestExecutionRefusesAnImageWithoutADigest(t *testing.T) {
	c := &Controller{Engine: &boundFake{}, Environment: "test"}
	if _, err := c.prepareExecution(context.Background(), Request{App: "web", Manifest: dockerManifest(nil)}, Plan{Image: "example/web:v1.0.2"}); err == nil {
		t.Fatal("an image without a digest was prepared")
	}
}

// liveEnv returns the environment of the service the engine holds.
func liveEnv(t *testing.T, e *positiveEngine) []string {
	t.Helper()
	e.mu.Lock()
	defer e.mu.Unlock()
	return specEnv(t, e.spec)
}
