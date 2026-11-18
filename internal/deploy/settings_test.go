package deploy

import (
	"context"
	"strings"
	"testing"

	"berthy/internal/manifest"
	"berthy/internal/state"
)

// settingsChat returns the Telegram chat of the environment's stored settings.
func settingsChat(t *testing.T, s *state.Store) string {
	t.Helper()
	st, ok, err := s.Settings(context.Background())
	if err != nil || !ok {
		t.Fatalf("settings: %v %v", ok, err)
	}
	m, err := manifest.DecodeSettings(st.Manifest)
	if err != nil || m.Alerts == nil || m.Alerts.Telegram == nil {
		t.Fatalf("stored settings: %+v %v", m, err)
	}
	return m.Alerts.Telegram.ChatID
}

func withChat(chat string) manifest.Manifest {
	m := dockerManifest(nil)
	m.Alerts = &manifest.Alerts{Telegram: &manifest.Telegram{BotTokenSecret: "TG", ChatID: chat}}
	return m
}

func TestADeployWritesTheSettingsAndARollbackDoesNot(t *testing.T) {
	s := openStore(t)
	c := &Controller{Engine: &positiveEngine{}, Store: s, Environment: "test"}
	op := runBound(t, s, c, Request{RequestID: "deploy-1", App: "web", Version: "sha256:" + strings.Repeat("b", 64), Commit: "c1", Manifest: withChat("1")})
	if op.State != "succeeded" {
		t.Fatalf("state %s: %s", op.State, op.Err)
	}
	if got := settingsChat(t, s); got != "1" {
		t.Fatalf("chat after the first deploy: %s", got)
	}
	op = runBound(t, s, c, Request{RequestID: "deploy-2", App: "web", Version: "sha256:" + strings.Repeat("c", 64), Commit: "c2", Manifest: withChat("2")})
	if op.State != "succeeded" {
		t.Fatalf("state %s: %s", op.State, op.Err)
	}
	st, _, _ := s.Settings(context.Background())
	if settingsChat(t, s) != "2" || st.Commit != "c2" || st.OperationID != op.ID {
		t.Fatalf("settings after the second deploy: %+v", st)
	}
	op = runBound(t, s, c, Request{RequestID: "rollback", App: "web", Kind: "rollback"})
	if op.State != "succeeded" {
		t.Fatalf("rollback: %s: %s", op.State, op.Err)
	}
	if got := settingsChat(t, s); got != "2" {
		t.Fatalf("a rollback changed the environment's alerts to chat %s", got)
	}
	again, _, _ := s.Settings(context.Background())
	if again.OperationID != st.OperationID || again.Commit != "c2" {
		t.Fatalf("a rollback rewrote the settings: %+v", again)
	}
	// The rollback went back with the app: the applied release is the one of deploy-1.
	applied, err := s.Applied(context.Background(), "web")
	if err != nil || !strings.Contains(applied.Digest, strings.Repeat("b", 64)) {
		t.Fatalf("applied after the rollback: %+v %v", applied, err)
	}
}

func TestADeployOfAnotherProjectIsRefusedBeforeAnythingChanges(t *testing.T) {
	s := openStore(t)
	f := &boundFake{}
	c := &Controller{Engine: f, Store: s, Environment: "test"}
	op := runBound(t, s, c, Request{RequestID: "first", App: "web", Version: "sha256:" + strings.Repeat("a", 64), Manifest: dockerManifest(nil)})
	if op.State != "succeeded" {
		t.Fatalf("state %s: %s", op.State, op.Err)
	}
	other := dockerManifest(nil)
	other.Project = "other"
	req := Request{RequestID: "second", App: "web", Version: "sha256:" + strings.Repeat("b", 64), Manifest: other}
	const want = "environment test runs project proj; this manifest is for project other"
	if _, _, err := c.Admit(context.Background(), Actor{ID: "admin"}, req); err == nil || err.Error() != want {
		t.Fatalf("admit: %v", err)
	}
	dry := req
	dry.DryRun = true
	if _, err := c.Plan(context.Background(), Actor{ID: "admin"}, dry); err == nil || err.Error() != want {
		t.Fatalf("dry run: %v", err)
	}
	if hist, _ := s.History(context.Background(), "web"); len(hist) != 1 {
		t.Fatalf("a refused deploy recorded a release: %d releases", len(hist))
	}
	if st, _, _ := s.Settings(context.Background()); st.Project != "proj" {
		t.Fatalf("a refused deploy changed the project: %+v", st)
	}
}

func TestADeployOfAnotherProjectIsAcceptedWhenNoAppIsApplied(t *testing.T) {
	s := openStore(t)
	f := &boundFake{}
	c := &Controller{Engine: f, Store: s, Environment: "test"}
	op := runBound(t, s, c, Request{RequestID: "first", App: "web", Version: "sha256:" + strings.Repeat("a", 64), Manifest: dockerManifest(nil)})
	if op.State != "succeeded" {
		t.Fatalf("state %s: %s", op.State, op.Err)
	}
	rm, _, err := s.Admit(context.Background(), state.Admission{RequestID: "rm", ActorID: "admin", Kind: "remove", PublicRequest: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ForgetApp(context.Background(), rm.ID, "web"); err != nil {
		t.Fatal(err)
	}
	if err = s.Finish(context.Background(), rm.ID, state.Outcome{State: "succeeded"}); err != nil {
		t.Fatal(err)
	}
	other := dockerManifest(nil)
	other.Project = "other"
	op = runBound(t, s, c, Request{RequestID: "second", App: "web", Version: "sha256:" + strings.Repeat("b", 64), Manifest: other})
	if op.State != "succeeded" {
		t.Fatalf("a new project after every app was removed: %s: %s", op.State, op.Err)
	}
	if st, _, _ := s.Settings(context.Background()); st.Project != "other" {
		t.Fatalf("project: %+v", st)
	}
}
