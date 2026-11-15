package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"berthy/internal/api"
	"berthy/internal/deploy"
	"berthy/internal/engine"
	"berthy/internal/logs"
	"berthy/internal/state"
)

// Logs of the connector must redact its tunnel token, so the stored connector release has to decode.
func TestConnectorLogsRedactTheTunnelToken(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(dir, "key")
	if err := os.WriteFile(key, make([]byte, 32), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := state.Open(dir, key, "lab")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	op, _, err := s.Admit(ctx, state.Admission{RequestID: "connector", ActorID: "a", Kind: "deploy", App: "ingress", PublicRequest: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	stored := `{"project":"demo","network":"demo","apps":{"ingress":{"kind":"image","image":"docker.io/cloudflare/cloudflared","roles":{"connector":{"replicas":1}},"env":{"secret":["TUNNEL_TOKEN"]}}}}`
	release := &state.Release{App: "ingress", Digest: "docker.io/cloudflare/cloudflared@sha256:" + strings.Repeat("d", 64), Manifest: []byte(stored), Bindings: []state.Binding{{App: "ingress", Role: "connector", Name: "demo-ingress-connector", ServiceID: "service1"}}}
	if err = s.Finish(ctx, op.ID, state.Outcome{State: "succeeded", Release: release}); err != nil {
		t.Fatal(err)
	}
	if err = s.PutSecret(ctx, state.Actor{ID: "admin"}, state.SecretInfo{Name: "TUNNEL_TOKEN"}, []byte("token-value")); err != nil {
		t.Fatal(err)
	}
	d := &deploy.Controller{Store: s}
	app := api.App{Ref: api.AppRef{Kind: "swarm", Name: "demo-ingress-connector"}}
	source := fakeManagedLogs{service: engine.ServiceSnapshot{ID: "service1", Spec: []byte(`{}`)}, env: []string{"TUNNEL_TOKEN=old-value"}}
	secrets, err := managedLogSecrets(ctx, d, source, app, []logs.Identity{{ContainerID: "c1"}})
	if err != nil {
		t.Fatalf("connector logs refused: %v", err)
	}
	joined := strings.Join(secrets, " ")
	if !strings.Contains(joined, "token-value") || !strings.Contains(joined, "old-value") {
		t.Fatalf("token values not redacted: %v", secrets)
	}
}
