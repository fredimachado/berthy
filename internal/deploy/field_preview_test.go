package deploy

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
	"testing/synctest"

	"berthy/internal/engine"
	"berthy/internal/state"
)

func finishFieldPreviewRelease(t *testing.T, store *state.Store, id, digest string, req Request) int64 {
	t.Helper()
	ctx := t.Context()
	raw, err := json.Marshal(req.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	op, _, err := store.Admit(ctx, state.Admission{RequestID: id, ActorID: "ci", Kind: "deploy", App: req.App, PublicRequest: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	rel := &state.Release{App: req.App, Digest: digest, Manifest: raw}
	if err := store.Finish(ctx, op.ID, state.Outcome{State: "succeeded", Release: rel}); err != nil {
		t.Fatal(err)
	}
	applied, err := store.Applied(ctx, req.App)
	if err != nil {
		t.Fatal(err)
	}
	return applied.ID
}

func drainFieldPreviewController(t *testing.T, c *Controller) {
	t.Helper()
	if err := c.Drain(context.Background()); err != nil {
		t.Error(err)
	}
}

func TestFieldPreviewAdmissionRejectsChangedBase(t *testing.T) {
	imageDigest := "docker.io/example/web@" + digestOfLetter("a")
	for _, name := range []string{"image", "first image", "kept version", "compose", "first compose"} {
		t.Run(name, func(t *testing.T) {
			var c *Controller
			req := Request{RequestID: "preview", App: "web", Version: imageDigest, Manifest: dockerManifest(nil)}
			digest := imageDigest
			switch name {
			case "compose", "first compose":
				runner := &fakeCompose{config: configOne, hashes: hashOne}
				c, _ = composeController(t, runner, newComposeDocker())
				req = composeReq("preview", composeFileOne, composeApp(false))
				digest = digestOf(composeFileOne)
			default:
				c = &Controller{Store: openStore(t), Engine: &positiveEngine{}, Environment: "test", rolloutHold: zeroTestRolloutHold()}
			}
			t.Cleanup(func() { drainFieldPreviewController(t, c) })
			ctx := t.Context()
			actor := Actor{ID: "ci", Admin: true}
			var initial int64
			if name != "first image" && name != "first compose" {
				initial = finishFieldPreviewRelease(t, c.Store, "initial", digest, req)
			}
			if name == "kept version" {
				req.Version, req.KeepDigest, req.KeepVersion = "", digest, true
			}
			req.DryRun = true
			plan, err := c.Plan(ctx, actor, req)
			if err != nil || plan.DiffBase == nil || *plan.DiffBase != initial {
				t.Fatalf("preview base: %v, want %d: %v", plan.DiffBase, initial, err)
			}
			req.ExpectedApplied = plan.DiffBase
			if _, err := c.Plan(ctx, actor, req); err != nil {
				t.Errorf("matching guard rejected: %v", err)
			}
			// A later release of the same digest still has a new identity.
			current := finishFieldPreviewRelease(t, c.Store, "later", digest, req)
			if current == initial {
				t.Fatal("the sequential release retained the initial ID")
			}
			req.DryRun = false
			if op, fresh, err := c.Admit(ctx, actor, req); fresh || !errors.Is(err, state.ErrConflict) {
				t.Errorf("stale guard: operation %q, fresh %v, error %v, want conflict", op.ID, fresh, err)
			}
			if _, err := c.Store.AdmissionForRequest(ctx, req.RequestID, actor.ID, [32]byte{}); !errors.Is(err, sql.ErrNoRows) {
				t.Errorf("stale guard left an admission: %v", err)
			}
		})
	}
}

type movingPreviewTag struct {
	*positiveEngine
	digest      string
	resolutions int
}

func (e *movingPreviewTag) ResolveImage(context.Context, string, *engine.RegistryAuth) (string, error) {
	e.resolutions++
	return e.digest, nil
}

func TestFieldPreviewAdmissionUsesFrozenSelection(t *testing.T) {
	for _, mode := range []string{"moving tag", "tagged digest", "kept version", "compose"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx := t.Context()
				actor := Actor{ID: "ci", Admin: true}
				moving := &movingPreviewTag{positiveEngine: &positiveEngine{}, digest: digestOfLetter("a")}
				var c *Controller
				var runner *fakeCompose
				req := Request{RequestID: "preview", App: "web", Version: "v1", Manifest: dockerManifest(nil)}
				if mode == "compose" {
					runner = &fakeCompose{config: configOne, hashes: hashOne}
					c, _ = composeController(t, runner, newComposeDocker())
					req = composeReq("preview", composeFileTwo, composeApp(false))
				} else {
					c = &Controller{Store: openStore(t), Engine: moving, Environment: "test", rolloutHold: zeroTestRolloutHold()}
				}
				t.Cleanup(func() { drainFieldPreviewController(t, c) })
				wantImage := "docker.io/example/web@" + moving.digest
				switch mode {
				case "compose":
					finishFieldPreviewRelease(t, c.Store, "initial", digestOf(composeFileOne), req)
					wantImage = digestOf(composeFileTwo)
				case "kept version":
					finishFieldPreviewRelease(t, c.Store, "initial", wantImage, req)
					req.Version, req.KeepDigest, req.KeepVersion = "", wantImage, true
				case "tagged digest":
					wantImage = "docker.io/example/web:v1@" + moving.digest
					req.Version = wantImage
				}
				req.DryRun = true
				plan, err := c.Plan(ctx, actor, req)
				if err != nil || plan.DiffBase == nil {
					t.Fatalf("preview has no base: %v", err)
				}
				if mode != "compose" && plan.Image != wantImage {
					t.Fatalf("planned image %q, want %q", plan.Image, wantImage)
				}
				if mode == "moving tag" {
					if plan.Tag != "v1" {
						t.Fatalf("planned tag %q, want v1", plan.Tag)
					}
					req.Version, req.VersionTag = plan.Image, plan.Tag
					moving.digest = digestOfLetter("b")
				}
				req.ExpectedApplied, req.DryRun = plan.DiffBase, false
				op, fresh, err := c.Admit(ctx, actor, req)
				if err != nil || !fresh {
					t.Fatalf("guarded admit: fresh %v, error %v", fresh, err)
				}
				drainFieldPreviewController(t, c)
				outcome, err := c.Store.Operation(ctx, op.ID)
				if err != nil || outcome.State != "succeeded" {
					t.Fatalf("operation state %q, reason %q: %v", outcome.State, outcome.Err, err)
				}
				applied, err := c.Store.Applied(ctx, req.App)
				if err != nil || applied.Digest != wantImage || applied.Tag != plan.Tag {
					t.Fatalf("applied digest/label %q %q, want %q %q: %v", applied.Digest, applied.Tag, wantImage, plan.Tag, err)
				}
				if runner != nil {
					if runner.upFile != composeFileTwo {
						t.Fatal("Compose did not use the captured bytes")
					}
				} else {
					var spec struct {
						TaskTemplate struct{ ContainerSpec struct{ Image string } }
					}
					if err := json.Unmarshal(moving.spec, &spec); err != nil || spec.TaskTemplate.ContainerSpec.Image != wantImage {
						t.Fatalf("service image %q, want %q: %v", spec.TaskTemplate.ContainerSpec.Image, wantImage, err)
					}
				}
				if mode == "moving tag" && moving.resolutions != 1 {
					t.Fatalf("resolved floating tag %d times", moving.resolutions)
				}
				replay, fresh, err := c.Admit(ctx, actor, req)
				if err != nil || fresh || replay.ID != op.ID {
					t.Fatalf("replay: %q, fresh %v, error %v, want %q", replay.ID, fresh, err, op.ID)
				}
			})
		})
	}
}
