package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
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
	obs := &recordingObserver{}
	c := Controller{Engine: e, Store: store, Environment: "test", Protected: true, Observer: obs}
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
	intents, err := store.Intents(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	foundBarrier := false
	for _, intent := range intents {
		if intent.Kind != "barrier" {
			continue
		}
		foundBarrier = true
		var outcome state.Outcome
		if err := json.Unmarshal(intent.Desired, &outcome); err != nil {
			t.Fatal(err)
		}
		if outcome.Release == nil || outcome.Release.Digest != plan.Image || outcome.Release.Tag != "v1" {
			t.Fatalf("barrier release: %+v", outcome.Release)
		}
	}
	if !foundBarrier {
		t.Fatal("verification barrier missing")
	}
	starts := obs.starts()
	if len(starts) != 1 || starts[0].OperationID != op.ID || starts[0].Version != "v1" || starts[0].Digest != digest {
		t.Fatalf("observer starts: %+v", starts)
	}
	obs.mu.Lock()
	ends := append([]OperationEvent(nil), obs.ended...)
	obs.mu.Unlock()
	if len(ends) != 1 || ends[0].OperationID != op.ID || ends[0].Version != "v1" || ends[0].Digest != digest {
		t.Fatalf("observer ends: %+v", ends)
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

func TestPinnedReplayAfterConcurrentCompletion(t *testing.T) {
	ctx := context.Background()
	store := openStore(t)
	obs := &recordingObserver{}
	c := &Controller{Store: store, Engine: &positiveEngine{}, Environment: "test", Protected: true, Observer: obs}
	t.Cleanup(func() {
		if err := c.Drain(ctx); err != nil {
			t.Error(err)
		}
	})
	type gateKey struct{}
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	c.Foreign = func(ctx context.Context) (Foreign, error) {
		if ctx.Value(gateKey{}) != nil {
			close(entered)
			<-release
		}
		return Foreign{}, nil
	}
	zero := int64(0)
	req := Request{RequestID: "same", App: "web", Version: "sha256:" + strings.Repeat("a", 64), VersionTag: "v1", ExpectedApplied: &zero, Manifest: dockerManifest(nil)}
	actor := Actor{ID: "actor"}
	type result struct {
		op    state.Operation
		fresh bool
		err   error
	}
	done := make(chan result, 1)
	go func() {
		op, fresh, err := c.Admit(context.WithValue(ctx, gateKey{}, true), actor, req)
		done <- result{op, fresh, err}
	}()
	<-entered
	original, fresh, err := c.Admit(ctx, actor, req)
	if err != nil || !fresh {
		t.Fatalf("original: %v %v", fresh, err)
	}
	if err := c.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	applied, err := store.Applied(ctx, "web")
	if err != nil || applied.ID == 0 {
		t.Fatalf("applied: %+v %v", applied, err)
	}
	once.Do(func() { close(release) })
	retry := <-done
	if retry.err != nil || retry.fresh || retry.op.ID != original.ID {
		t.Fatalf("retry: %+v, original %s", retry, original.ID)
	}
	if starts := obs.starts(); len(starts) != 1 {
		t.Fatalf("operation starts: %+v", starts)
	}
	releases, err := store.History(ctx, "web")
	if err != nil || len(releases) != 1 {
		t.Fatalf("releases: %+v %v", releases, err)
	}
	if _, err := c.Plan(ctx, actor, req); !errors.Is(err, state.ErrConflict) {
		t.Fatalf("stale plan: %v", err)
	}
}

func TestFailedPinnedOperationHistoryKeepsLabel(t *testing.T) {
	ctx := context.Background()
	store := openStore(t)
	obs := &recordingObserver{}
	c := &Controller{Store: store, Environment: "test", Observer: obs}
	t.Cleanup(func() {
		if err := c.Drain(ctx); err != nil {
			t.Error(err)
		}
	})
	digest := "sha256:" + strings.Repeat("a", 64)
	req := Request{RequestID: "failed-pin", App: "web", Version: digest, VersionTag: "v1", Manifest: dockerManifest(nil)}
	op, fresh, err := c.Admit(ctx, Actor{ID: "actor"}, req)
	if err != nil || !fresh {
		t.Fatalf("admission: %v %v", fresh, err)
	}
	if err := c.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := store.Operation(ctx, op.ID)
	if err != nil || got.State != "failed" {
		t.Fatalf("operation: %+v %v", got, err)
	}
	rows, err := store.DeployHistory(ctx, "web")
	if err != nil || len(rows) != 1 || rows[0].OperationID != op.ID || rows[0].Tag != "v1" {
		t.Fatalf("history: %+v %v", rows, err)
	}
	obs.mu.Lock()
	ends := append([]OperationEvent(nil), obs.ended...)
	obs.mu.Unlock()
	if len(ends) != 1 || ends[0].Version != "v1" || ends[0].Digest != digest {
		t.Fatalf("failed observer: %+v", ends)
	}
}
