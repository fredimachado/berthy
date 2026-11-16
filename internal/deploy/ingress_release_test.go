package deploy

import (
	"context"
	"encoding/json"
	"testing"

	"berthy/internal/engine"
	"berthy/internal/state"
)

// connectorStored is the manifest Berthy stores with each connector release: the connector app, with the tunnel token.
const connectorStored = `{"project":"demo","network":"demo","apps":{"ingress":{"kind":"image","image":"docker.io/cloudflare/cloudflared","roles":{"connector":{"replicas":1}},"env":{"secret":["TUNNEL_TOKEN"]}}}}`

// Removing managed ingress forgets the connector app's applied release, but keeps its history. Prune must still run. The
// connector no longer runs, so none of its releases protects an image.
func TestPruneAfterManagedIngressIsRemoved(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	for _, digest := range []string{"ing-old", "ing-current"} {
		op, _, err := s.Admit(ctx, state.Admission{RequestID: "release-" + digest, ActorID: "a", Kind: "deploy", App: "ingress", PublicRequest: json.RawMessage(`{}`)})
		if err != nil {
			t.Fatal(err)
		}
		if err = s.RecordImageAcquired(ctx, digest, "id-"+digest); err != nil {
			t.Fatal(err)
		}
		release := &state.Release{App: "ingress", Digest: digest, Manifest: json.RawMessage(`{"apps":{"ingress":{"deploy":{"retain":1}}}}`)}
		if err = s.Finish(ctx, op.ID, state.Outcome{State: "succeeded", Release: release}); err != nil {
			t.Fatal(err)
		}
	}
	forget, _, err := s.Admit(ctx, state.Admission{RequestID: "forget", ActorID: "a", Kind: "ingress", App: "ingress", PublicRequest: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ForgetApp(ctx, forget.ID, "ingress"); err != nil {
		t.Fatal(err)
	}
	if err = s.Finish(ctx, forget.ID, state.Outcome{State: "succeeded"}); err != nil {
		t.Fatal(err)
	}

	prune, _, err := s.Admit(ctx, state.Admission{RequestID: "prune", ActorID: "a", Kind: "prune", PublicRequest: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	f := &pruneEngine{runtimeEngine: &runtimeEngine{}, images: []engine.InventoryImage{
		{ID: "id-ing-old", RepoDigests: []string{"ing-old"}},
		{ID: "id-ing-current", RepoDigests: []string{"ing-current"}},
	}}
	c := Controller{Store: s, Engine: f}
	out := c.runOperation(ctx, prune, Actor{Admin: true}, Request{Kind: "prune"})
	if out.State != "succeeded" || len(f.removed) != 2 || f.removed[0] != "ing-current" || f.removed[1] != "ing-old" {
		t.Fatalf("outcome %+v removed %v", out, f.removed)
	}
}

// A rollback of the connector app resolves to a manifest that still validates, because the connector flag comes back
// with the app's name.
func TestRollbackOfTheConnectorValidates(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	for _, digest := range []string{"old-digest", "new-digest"} {
		op, _, err := s.Admit(ctx, state.Admission{RequestID: "release-" + digest, ActorID: "a", Kind: "deploy", App: "ingress", PublicRequest: json.RawMessage(`{}`)})
		if err != nil {
			t.Fatal(err)
		}
		release := &state.Release{App: "ingress", Digest: digest, Manifest: json.RawMessage(connectorStored)}
		if err = s.Finish(ctx, op.ID, state.Outcome{State: "succeeded", Release: release}); err != nil {
			t.Fatal(err)
		}
	}
	c := Controller{Store: s}
	got, err := c.rollbackRequest(ctx, Request{App: "ingress", Kind: "rollback"})
	if err != nil || got.Version != "old-digest" || !got.Manifest.Connector {
		t.Fatalf("rollback %+v: %v", got, err)
	}
	if err = got.Manifest.Validate(); err != nil {
		t.Fatalf("rolled-back connector manifest invalid: %v", err)
	}
}
