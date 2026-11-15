package state

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestPrivateStateAndManualLock(t *testing.T) {
	s, dir, key := testStore(t)
	ctx := context.Background()
	if err := s.AcquireManual(ctx, Actor{ID: "test"}, "alice", "maintenance"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Admit(ctx, Admission{RequestID: "blocked", ActorID: "a", Kind: "deploy", PublicRequest: []byte(`{}`)}); err == nil {
		t.Fatal("manual lock bypassed")
	}
	if err := s.ReleaseManual(ctx, Actor{ID: "test"}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutSecret(ctx, Actor{ID: "test"}, SecretInfo{Name: "registry", Registry: "registry.example"}, []byte("sensitive-value")); err != nil {
		t.Fatal(err)
	}
	info, value, err := s.Secret(ctx, "registry")
	if err != nil || info.Registry != "registry.example" || string(value) != "sensitive-value" {
		t.Fatalf("secret: %v %q %v", info, value, err)
	}
	s.Close()
	data, err := os.ReadFile(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("sensitive-value")) {
		t.Fatal("secret persisted in plaintext")
	}
	if err = os.Chmod(key, 0644); err != nil {
		t.Fatal(err)
	}
	if strictKeyMode && func() bool { _, err := Open(dir, key, "test"); return err == nil }() {
		t.Fatal("insecure key accepted")
	}
}
