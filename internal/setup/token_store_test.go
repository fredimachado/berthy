package setup

import (
	"context"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"berthy/internal/agentclient"
	"berthy/internal/credstore"
	"berthy/internal/credstore/credtest"
)

func TestTokenStoreStepDryRunSavesNothing(t *testing.T) {
	store := credtest.NewMemory()
	s := &Session{Opts: Options{Tokens: store}, DryRun: true, dryToken: true}
	status, _, err := tokenStoreStep(context.Background(), s)
	if err != nil || status != StatusWouldChange || store.Calls() != 0 {
		t.Fatalf("status %q, error %v, calls %d", status, err, store.Calls())
	}
}

func TestTokenStoreStepGivesUpWhenInterrupted(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		block := make(chan struct{})
		store := credtest.NewMemory()
		store.Block, store.OnCall = block, cancel
		defer func() { close(block); store.Wait(); cancel() }()
		s := &Session{Opts: Options{Project: "shop", Env: "p5", Tokens: credstore.Bounded(ctx, store, time.Hour)}, Token: "secret", TokenLive: true, TokenEnv: "CUSTOM", cliConfigKnown: true, cliEntry: agentclient.Environment{URL: "http://host", TokenEnv: "CUSTOM"}}
		status, detail, err := tokenStoreStep(ctx, s)
		if err != nil || status != StatusSkipped || s.TokenStored || !strings.Contains(detail, "could not confirm the save in the test store") {
			t.Fatalf("status %s, detail %s, error %v", status, detail, err)
		}
	})
}

func TestAdminTokenOrder(t *testing.T) {
	store := credtest.NewMemory()
	e := agentclient.Environment{URL: "http://host", TokenEnv: "CUSTOM"}
	if err := store.Set(agentclient.StoreKey("shop", "p5", e), "stored"); err != nil {
		t.Fatal(err)
	}
	value := "variable"
	s := &Session{Opts: Options{Project: "shop", Env: "p5", Tokens: store, Getenv: func(string) string { return value }}, Token: "new", TokenLive: true, TokenEnv: "CUSTOM", cliEntry: e, cliConfigKnown: true}
	if s.AdminToken() != "new" {
		t.Fatal("new token not preferred")
	}
	s.TokenLive = false
	if s.AdminToken() != "variable" {
		t.Fatal("variable not preferred")
	}
	value = ""
	if s.AdminToken() != "variable" {
		t.Fatal("the run changed its credential observation")
	}
	s = &Session{Opts: s.Opts, TokenEnv: "CUSTOM", cliEntry: e, cliConfigKnown: true}
	if s.AdminToken() != "stored" {
		t.Fatal("stored token not found")
	}
}
