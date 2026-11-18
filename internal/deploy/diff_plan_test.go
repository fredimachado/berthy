package deploy

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"berthy/internal/engine"
	"berthy/internal/manifest"
	"berthy/internal/state"
)

func digestOfLetter(c string) string { return "sha256:" + strings.Repeat(c, 64) }

// deployedWeb runs a deploy of app web by digest and returns the controller that ran it.
func deployedWeb(t *testing.T, id, letter string, m manifest.Manifest) *Controller {
	t.Helper()
	s := openStore(t)
	c := &Controller{Engine: &boundFake{}, Store: s, Environment: "test"}
	redeployWeb(t, c, id, letter, m)
	return c
}

func redeployWeb(t *testing.T, c *Controller, id, letter string, m manifest.Manifest) {
	t.Helper()
	if op := runBound(t, c.Store, c, Request{RequestID: id, App: "web", Version: digestOfLetter(letter), Manifest: m}); op.State != "succeeded" {
		t.Fatalf("deploy %s: %s %s", id, op.State, op.Err)
	}
}

// seedManifestRelease records a succeeded deploy of app web at digest that ran manifest m.
func seedManifestRelease(t *testing.T, s *state.Store, requestID, digest string, m manifest.Manifest) {
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

func webWith(plain map[string]string, replicas int) manifest.Manifest {
	m := dockerManifest(plain)
	app := m.Apps["web"]
	r := app.Roles["web"]
	r.Replicas = &replicas
	app.Roles["web"] = r
	m.Apps["web"] = app
	return m
}

func TestDryRunPlanSaysWhatTheDeployChangesAgainstTheAppliedRelease(t *testing.T) {
	c := deployedWeb(t, "first", "a", webWith(nil, 1))
	plan, err := c.Plan(context.Background(), Actor{ID: "admin", Admin: true}, Request{RequestID: "plan", App: "web", Version: digestOfLetter("b"), DryRun: true, Manifest: webWith(map[string]string{"FEATURE_X": "1"}, 2)})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Diff == nil || plan.Diff.New || plan.Diff.Same {
		t.Fatalf("diff %+v", plan.Diff)
	}
	want := []string{
		"image: sha256:" + strings.Repeat("a", 12) + "… -> sha256:" + strings.Repeat("b", 12) + "…",
		"web: replicas 1 -> 2",
		"env: + FEATURE_X (plain)",
	}
	if got := changesOf(*plan.Diff); !reflect.DeepEqual(got, want) {
		t.Fatalf("changes\n got %q\nwant %q", got, want)
	}
}

func TestDryRunPlanOfTheSameReleaseChangesNothing(t *testing.T) {
	c := deployedWeb(t, "first", "a", webWith(nil, 1))
	plan, err := c.Plan(context.Background(), Actor{ID: "admin", Admin: true}, Request{RequestID: "plan", App: "web", Version: digestOfLetter("a"), DryRun: true, Manifest: webWith(nil, 1)})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Diff == nil || !plan.Diff.Same {
		t.Fatalf("diff %+v", plan.Diff)
	}
}

func TestDryRunPlanOfAFirstDeployIsANewApp(t *testing.T) {
	c := &Controller{Engine: &boundFake{}, Store: openStore(t), Environment: "test"}
	plan, err := c.Plan(context.Background(), Actor{ID: "admin", Admin: true}, Request{RequestID: "plan", App: "web", Version: digestOfLetter("a"), DryRun: true, Manifest: webWith(nil, 1)})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Diff == nil || !plan.Diff.New {
		t.Fatalf("diff %+v", plan.Diff)
	}
}

func TestPlanDiffIsTheSameWhenAskedTwice(t *testing.T) {
	c := deployedWeb(t, "first", "a", webWith(nil, 1))
	ask := func() []byte {
		plan, err := c.Plan(context.Background(), Actor{ID: "admin", Admin: true}, Request{RequestID: "plan", App: "web", Version: digestOfLetter("b"), DryRun: true, Manifest: webWith(map[string]string{"A": "1", "B": "2", "C": "3"}, 3)})
		if err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(plan)
		return b
	}
	first := ask()
	for i := 0; i < 10; i++ {
		if again := ask(); string(again) != string(first) {
			t.Fatalf("plan differs:\n%s\n%s", first, again)
		}
	}
}

func TestRollbackPlanComparesTheAppliedReleaseWithTheOneItReturnsTo(t *testing.T) {
	s := openStore(t)
	seedManifestRelease(t, s, "first", digestOfLetter("a"), webWith(map[string]string{"OLD": "1"}, 1))
	seedManifestRelease(t, s, "second", digestOfLetter("b"), webWith(map[string]string{"NEW": "1"}, 1))
	c := &Controller{Engine: &boundFake{}, Store: s, Environment: "test"}
	plan, err := c.Plan(context.Background(), Actor{ID: "admin", Admin: true}, Request{RequestID: "rb", App: "web", Kind: "rollback", DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Diff == nil {
		t.Fatal("no diff")
	}
	want := []string{
		"image: sha256:" + strings.Repeat("b", 12) + "… -> sha256:" + strings.Repeat("a", 12) + "…",
		"env: - NEW (plain)",
		"env: + OLD (plain)",
	}
	if got := changesOf(*plan.Diff); !reflect.DeepEqual(got, want) {
		t.Fatalf("changes\n got %q\nwant %q", got, want)
	}
}

func TestComposePlanDiffListsTheServicesItRecreates(t *testing.T) {
	stale := newComposeDocker()
	runner := &fakeCompose{config: configOne, hashes: hashOne}
	c, s := composeController(t, runner, stale)
	op, _, err := admit(t, c, context.Background(), Actor{ID: "ci"}, composeReq("one", composeFileOne, composeApp(false)))
	if err != nil {
		t.Fatal(err)
	}
	if got := waitComposeOperation(t, s, op.ID); got.State != "succeeded" {
		t.Fatalf("deploy %s %s", got.State, got.Err)
	}
	stale.containers = []engine.ComposeContainer{{ID: "c1", Service: "database", ConfigHash: "hash-old-000000000000"}, {ID: "c2", Service: "retired", ConfigHash: "hash-x-0000000000"}}
	plan, err := c.Plan(context.Background(), Actor{ID: "ci"}, composeReq("dry", composeFileOne, composeApp(false)))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"compose: recreate database", "compose: remove retired"}
	if plan.Diff == nil || plan.Diff.Same || !reflect.DeepEqual(changesOf(*plan.Diff), want) {
		t.Fatalf("diff %+v", plan.Diff)
	}
}

func TestAdoptionPlanHasNoDiff(t *testing.T) {
	// An adoption lists its own changes against the running service.
	c := &Controller{Engine: &boundFake{}, Store: openStore(t), Environment: "test"}
	plan, err := c.Plan(context.Background(), Actor{ID: "admin", Admin: true}, Request{RequestID: "plan", App: "web", Version: digestOfLetter("a"), DryRun: true, Adopt: true, Manifest: webWith(nil, 1)})
	if err == nil && plan.Diff != nil {
		t.Fatalf("diff %+v", plan.Diff)
	}
}
