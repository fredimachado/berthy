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

// TestManagedLogSecretsRedactAMappedVariable: a variable that takes a secret's value is redacted from the logs of its
// containers, rotated or not. The container carries the variable's name, not the secret's.
func TestManagedLogSecretsRedactAMappedVariable(t *testing.T) {
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
	op, _, err := s.Admit(ctx, state.Admission{RequestID: "r", ActorID: "a", Kind: "deploy", App: "api", PublicRequest: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	m := []byte(`{"project":"demo","network":"demo","apps":{"api":{"kind":"image","image":"alpine","roles":{"web":{}},"env":{"secret":[{"DATABASE_URL":"API_DB"}]}}}}`)
	if err = s.Finish(ctx, op.ID, state.Outcome{State: "succeeded", Release: &state.Release{App: "api", Manifest: m, Bindings: []state.Binding{{App: "api", Role: "web", Name: "demo-api-web", ServiceID: "service1"}}}}); err != nil {
		t.Fatal(err)
	}
	if err = s.PutSecret(ctx, state.Actor{ID: "admin"}, state.SecretInfo{Name: "API_DB"}, []byte("new-value")); err != nil {
		t.Fatal(err)
	}
	d := &deploy.Controller{Store: s}
	app := api.App{Ref: api.AppRef{Kind: "swarm", Name: "demo-api-web"}}
	source := fakeManagedLogs{service: engine.ServiceSnapshot{ID: "service1", Spec: []byte(`{}`)}, env: []string{"DATABASE_URL=old-value"}}
	secret, err := managedLogSecrets(ctx, d, source, app, []logs.Identity{{ContainerID: "c1"}})
	if err != nil {
		t.Fatal(err)
	}
	redacted := redactRecords([]logs.Record{{Message: "old-value new-value"}}, secret, nil)
	if strings.Contains(redacted[0].Message, "value") {
		t.Fatalf("a mapped value leaked: %q", redacted[0].Message)
	}
}
