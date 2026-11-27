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
	c := &Controller{rolloutHold: zeroTestRolloutHold(), Engine: &positiveEngine{}, Store: s, Environment: "test"}
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
