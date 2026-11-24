package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"berthy/internal/manifest"
	"berthy/internal/state"
)

func TestProtectedPinSurvivesPlanAdmitAndRollout(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(dir, "key")
	if err := os.WriteFile(key, make([]byte, 32), 0600); err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(dir, key, "test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	e := &positiveEngine{}
	c := Controller{Engine: e, Store: store, Environment: "test", Protected: true}
	t.Cleanup(func() {
		if err := c.Drain(ctx); err != nil {
			t.Error(err)
		}
	})
	one := 1
	digest := "sha256:" + strings.Repeat("a", 64)
	zero := int64(0)
	req := Request{RequestID: "pin", App: "web", Version: digest, VersionTag: "v1", ExpectedApplied: &zero, Manifest: manifest.Manifest{Project: "proj", Network: "overlay", Apps: map[string]manifest.App{"web": {Kind: "image", Image: "example/web", Deploy: manifest.Deploy{DeployTimeout: testDeployTimeout, DrainTimeout: "1s"}, Roles: map[string]manifest.Role{"web": {Replicas: &one, Health: &manifest.Health{Test: []string{"CMD", "true"}}}}}}}}
	plan, err := c.Plan(ctx, Actor{ID: "a"}, req)
	if err != nil || plan.Tag != "v1" || !strings.HasSuffix(plan.Image, "@"+digest) {
		t.Fatalf("plan: %+v %v", plan, err)
	}
	op, fresh, err := c.Admit(ctx, Actor{ID: "a"}, req)
	if err != nil || !fresh {
		t.Fatalf("admit: %v %v", fresh, err)
	}
	if err := c.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := store.Operation(ctx, op.ID)
	if err != nil || got.State != "succeeded" {
		t.Fatalf("operation: %+v %v", got, err)
	}
	applied, err := store.Applied(ctx, "web")
	if err != nil || applied.Tag != "v1" || applied.Digest != plan.Image {
		t.Fatalf("applied: %+v %v", applied, err)
	}
	history, err := store.History(ctx, "web")
	if err != nil || len(history) != 1 || history[0].Tag != "v1" {
		t.Fatalf("history: %+v %v", history, err)
	}
	var spec struct {
		TaskTemplate struct {
			ContainerSpec struct {
				Image string
				Env   []string
			}
		}
	}
	if err := json.Unmarshal(e.spec, &spec); err != nil {
		t.Fatal(err)
	}
	if spec.TaskTemplate.ContainerSpec.Image != plan.Image || !strings.Contains(strings.Join(spec.TaskTemplate.ContainerSpec.Env, "\n"), "BERTHY_VERSION=v1") {
		t.Fatalf("spec: %+v", spec)
	}
	replay, fresh, err := c.Admit(ctx, Actor{ID: "a"}, req)
	if err != nil || fresh || replay.ID != op.ID {
		t.Fatalf("replay: %+v %v %v", replay, fresh, err)
	}
	req.RequestID = "second"
	req.ExpectedApplied = &applied.ID
	req.VersionTag = "v2"
	plan2, err := c.Plan(ctx, Actor{ID: "a"}, req)
	if err != nil || plan2.Tag != "v2" || plan2.Image != plan.Image || plan2.Diff == nil || !slices.Contains(plan2.Diff.Changes, DiffChange{Area: "image", Change: "tag v1 -> v2"}) {
		t.Fatalf("tag-only plan: %+v %v", plan2, err)
	}
	_, fresh, err = c.Admit(ctx, Actor{ID: "a"}, req)
	if err != nil || !fresh {
		t.Fatalf("second admit: %v %v", fresh, err)
	}
	if err := c.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	applied2, err := store.Applied(ctx, "web")
	if err != nil || applied2.Tag != "v2" || applied2.Digest != applied.Digest {
		t.Fatalf("second applied: %+v %v", applied2, err)
	}
	if err := json.Unmarshal(e.spec, &spec); err != nil {
		t.Fatal(err)
	}
	if spec.TaskTemplate.ContainerSpec.Image != plan.Image || !slices.Contains(spec.TaskTemplate.ContainerSpec.Env, "BERTHY_IMAGE_DIGEST="+digest) || !slices.Contains(spec.TaskTemplate.ContainerSpec.Env, "BERTHY_VERSION=v2") {
		t.Fatalf("second spec: %+v", spec)
	}
	req.ExpectedApplied = &zero
	req.RequestID = "stale"
	if _, _, err := c.Admit(ctx, Actor{ID: "a"}, req); !errors.Is(err, state.ErrConflict) {
		t.Fatalf("stale: %v", err)
	}
	req.RequestID = "tag"
	req.Version = "latest"
	req.VersionTag = ""
	req.ExpectedApplied = nil
	if _, _, err := c.Admit(ctx, Actor{ID: "a"}, req); err == nil {
		t.Fatal("protected tag admitted")
	}
	if _, err := store.Active(ctx); err == nil {
		t.Fatal("refused request acquired lock")
	}
}

func TestPinnedLabelReachesHookIdentity(t *testing.T) {
	c, f, id, _ := hookFixture(t)
	f.Engine = dockerSpecs{&positiveEngine{}}
	m := dockerManifest(nil)
	app := m.Apps["web"]
	app.Hooks.PreDeploy = []manifest.Hook{{Run: []string{"true"}}}
	m.Apps["web"] = app
	digest := "sha256:" + strings.Repeat("a", 64)
	req := Request{App: "web", Version: digest, VersionTag: "v1", Manifest: m}
	plan, err := c.Plan(context.Background(), Actor{}, req)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := c.prepareExecution(context.Background(), req, plan)
	if err != nil {
		t.Fatal(err)
	}
	f.exits = []int{0}
	if _, _, err := c.runPre(context.Background(), id, prepared); err != nil {
		t.Fatal(err)
	}
	if len(f.specs) != 1 {
		t.Fatalf("hook specs: %d", len(f.specs))
	}
	var spec struct {
		Image string
		Env   []string
	}
	if err := json.Unmarshal(f.specs[0], &spec); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"BERTHY_VERSION=v1", "BERTHY_IMAGE_DIGEST=" + digest, "BERTHY_ROLE="} {
		if !slices.Contains(spec.Env, value) {
			t.Fatalf("hook environment %v lacks %s", spec.Env, value)
		}
	}
	if spec.Image != plan.Image || !strings.HasSuffix(spec.Image, "@"+digest) {
		t.Fatalf("hook image: %s", spec.Image)
	}
}
