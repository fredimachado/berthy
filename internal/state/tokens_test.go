package state

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var tokenActorOne = Actor{ID: "admin-id", Name: "alice", Remote: "10.0.0.1:1"}

func tokenRow(name string, n int) Token {
	return Token{Name: name, Digest: strings.Repeat(string(rune('a'+n)), 64), Scope: "deploy", Created: time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)}
}

func TestTokenCreateListAndAuditInOneStep(t *testing.T) {
	s, _, _ := testStore(t)
	ctx := context.Background()
	exp := time.Date(2027, 2, 16, 0, 0, 0, 0, time.UTC)
	ci := tokenRow("ci", 0)
	ci.Expires = &exp
	claude := tokenRow("claude", 1)
	claude.Agent = true
	for _, tok := range []Token{ci, claude} {
		if err := s.CreateToken(ctx, tokenActorOne, tok); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.Tokens(ctx)
	if err != nil || len(got) != 2 {
		t.Fatalf("tokens: %v %v", got, err)
	}
	if got[0].Name != "ci" || got[0].Digest != ci.Digest || got[0].Scope != "deploy" || got[0].Agent || got[0].Expires == nil || !got[0].Expires.Equal(exp) || !got[0].Created.Equal(ci.Created) {
		t.Fatalf("ci: %+v", got[0])
	}
	if got[1].Name != "claude" || !got[1].Agent || got[1].Expires != nil {
		t.Fatalf("claude: %+v", got[1])
	}
	log, err := s.AuditLog(ctx, 10)
	if err != nil || len(log) != 2 || log[1].Action != "token_create:ci" || log[0].Action != "token_create:claude" || log[1].ActorName != "alice" || log[1].ActorID != "admin-id" || log[1].Outcome != "succeeded" {
		t.Fatalf("audit: %+v %v", log, err)
	}
}

func TestTokenNameAndDigestAreUnique(t *testing.T) {
	s, _, _ := testStore(t)
	ctx := context.Background()
	if err := s.CreateToken(ctx, tokenActorOne, tokenRow("ci", 0)); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateToken(ctx, tokenActorOne, tokenRow("ci", 1)); !errors.Is(err, ErrConflict) {
		t.Fatalf("same name: %v", err)
	}
	same := tokenRow("other", 0)
	if err := s.CreateToken(ctx, tokenActorOne, same); !errors.Is(err, ErrConflict) {
		t.Fatalf("same digest: %v", err)
	}
	if got, _ := s.Tokens(ctx); len(got) != 1 {
		t.Fatalf("a refused create left %d tokens", len(got))
	}
	if log, _ := s.AuditLog(ctx, 10); len(log) != 1 {
		t.Fatalf("a refused create was audited: %+v", log)
	}
	if err := s.CreateToken(ctx, Actor{}, tokenRow("x", 2)); err == nil {
		t.Fatal("a create without an actor was accepted")
	}
}

func TestTokenRevokeRemovesItsRowAndItsUseAndAudits(t *testing.T) {
	s, _, _ := testStore(t)
	ctx := context.Background()
	ci, claude := tokenRow("ci", 0), tokenRow("claude", 1)
	for _, tok := range []Token{ci, claude} {
		if err := s.CreateToken(ctx, tokenActorOne, tok); err != nil {
			t.Fatal(err)
		}
	}
	at := time.Date(2026, 11, 18, 11, 0, 0, 0, time.UTC)
	if err := s.TouchTokens(ctx, map[string]time.Time{ci.Digest: at, claude.Digest: at}); err != nil {
		t.Fatal(err)
	}
	var seen []Token
	if err := s.RevokeToken(ctx, tokenActorOne, "ci", func(remaining []Token) error { seen = remaining; return nil }); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 || seen[0].Name != "claude" {
		t.Fatalf("the guard saw %+v", seen)
	}
	if got, _ := s.Tokens(ctx); len(got) != 1 || got[0].Name != "claude" {
		t.Fatalf("after revoke: %+v", got)
	}
	use, err := s.TokenUse(ctx)
	if err != nil || len(use) != 1 || !use[claude.Digest].Equal(at) {
		t.Fatalf("use after revoke: %v %v", use, err)
	}
	log, _ := s.AuditLog(ctx, 10)
	if len(log) != 3 || log[0].Action != "token_revoke:ci" || log[0].ActorName != "alice" {
		t.Fatalf("audit: %+v", log)
	}
	if err := s.RevokeToken(ctx, tokenActorOne, "ci", nil); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("revoke of an absent token: %v", err)
	}
	if err := s.CreateToken(ctx, tokenActorOne, tokenRow("ci", 3)); err != nil {
		t.Fatalf("the name is free again: %v", err)
	}
}

func TestTokenRevokeStopsWhenTheGuardRefuses(t *testing.T) {
	s, _, _ := testStore(t)
	ctx := context.Background()
	if err := s.CreateToken(ctx, tokenActorOne, tokenRow("ci", 0)); err != nil {
		t.Fatal(err)
	}
	refusal := errors.New("last admin")
	if err := s.RevokeToken(ctx, tokenActorOne, "ci", func([]Token) error { return refusal }); !errors.Is(err, refusal) {
		t.Fatalf("revoke: %v", err)
	}
	if got, _ := s.Tokens(ctx); len(got) != 1 {
		t.Fatalf("a refused revoke removed the token: %+v", got)
	}
	if log, _ := s.AuditLog(ctx, 10); len(log) != 1 {
		t.Fatalf("a refused revoke was audited: %+v", log)
	}
}

func TestTouchTokensKeepsTheLatestUse(t *testing.T) {
	s, _, _ := testStore(t)
	ctx := context.Background()
	first := time.Date(2026, 11, 18, 11, 0, 0, 0, time.UTC)
	second := first.Add(time.Hour)
	digest := strings.Repeat("c", 64)
	if err := s.TouchTokens(ctx, map[string]time.Time{digest: first}); err != nil {
		t.Fatal(err)
	}
	if err := s.TouchTokens(ctx, map[string]time.Time{digest: second}); err != nil {
		t.Fatal(err)
	}
	use, err := s.TokenUse(ctx)
	if err != nil || len(use) != 1 || !use[digest].Equal(second) {
		t.Fatalf("use: %v %v", use, err)
	}
}

func TestTokensAreInTheSnapshot(t *testing.T) {
	ctx := context.Background()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	s := openKeyed(t, dir, key)
	ci := tokenRow("ci", 0)
	if err := s.CreateToken(ctx, tokenActorOne, ci); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 11, 18, 11, 0, 0, 0, time.UTC)
	if err := s.TouchTokens(ctx, map[string]time.Time{ci.Digest: at}); err != nil {
		t.Fatal(err)
	}
	copyDir := filepath.Join(t.TempDir(), "copy")
	if err := os.Mkdir(copyDir, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Snapshot(ctx, filepath.Join(copyDir, "state.db")); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(keyPath, key, 0600); err != nil {
		t.Fatal(err)
	}
	copied, err := Open(copyDir, keyPath, "prod")
	if err != nil {
		t.Fatal(err)
	}
	defer copied.Close()
	got, err := copied.Tokens(ctx)
	if err != nil || len(got) != 1 || got[0].Name != "ci" || got[0].Digest != ci.Digest {
		t.Fatalf("tokens in the snapshot: %+v %v", got, err)
	}
	if use, _ := copied.TokenUse(ctx); !use[ci.Digest].Equal(at) {
		t.Fatalf("use in the snapshot: %v", use)
	}
}
