package agentclient

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"testing/synctest"

	"berthy/internal/credstore"
	"berthy/internal/credstore/credtest"
)

func TestFindTokenPrefersTheVariable(t *testing.T) {
	s := credtest.NewMemory()
	e := Environment{URL: "http://host", TokenEnv: "TOK"}
	_ = s.Set(StoreKey("todo", "dev", e), "stored")
	v, src, err := FindToken(func(string) string { return "env" }, s, "todo", "dev", e)
	if v != "env" || src != TokenFromEnv || err != nil || s.Calls() != 1 {
		t.Fatalf("%q %q %v calls %d", v, src, err, s.Calls())
	}
}
func TestFindTokenFallsBackToTheStore(t *testing.T) {
	s := credtest.NewMemory()
	e := Environment{URL: "http://host", TokenEnv: "TOK"}
	_ = s.Set(StoreKey("todo", "dev", e), "stored")
	v, src, err := FindToken(func(string) string { return "" }, s, "todo", "dev", e)
	if v != "stored" || src != TokenFromStore || err != nil {
		t.Fatalf("%q %q %v", v, src, err)
	}
}
func TestFindTokenWithANilStoreReadsTheVariableOnly(t *testing.T) {
	v, src, err := FindToken(func(string) string { return "env" }, nil, "todo", "dev", Environment{})
	if v != "env" || src != TokenFromEnv || err != nil {
		t.Fatalf("%q %q %v", v, src, err)
	}
}
func TestFindTokenWithNeitherSaysWhy(t *testing.T) {
	for _, tc := range []struct {
		s    credstore.Store
		want string
	}{
		{nil, "no token for todo/dev: its token variable is empty (berthy context list names it)"},
		{credtest.NewMemory(), "no token for todo/dev: its token variable is empty and the test store has none saved for it; run berthy setup --new-token, or set the variable (berthy context list names it)"},
		{credstore.None(), "no token for todo/dev: its token variable is empty and this machine has no credential store; set the variable (berthy context list names it)"},
	} {
		_, _, err := FindToken(func(string) string { return "" }, tc.s, "todo", "dev", Environment{})
		if err == nil || err.Error() != tc.want {
			t.Errorf("got %v want %q", err, tc.want)
		}
	}
}
func TestResolveUsesTheStoredToken(t *testing.T) {
	t.Setenv("BERTHY_TEST_TOKEN", "")
	s := credtest.NewMemory()
	old := TokenStore
	TokenStore = s
	t.Cleanup(func() { TokenStore = old })
	_ = s.Set(StoreKey("demo", "dev", Environment{URL: "http://127.0.0.1:9000/base/", TokenEnv: "BERTHY_TEST_TOKEN"}), "stored")
	target, err := Resolve(writeConfig(t, cfgDev), Selector{Environment: "dev"})
	if err != nil || target.Token != "stored" {
		t.Fatalf("%+v %v", target, err)
	}
}

func TestResolveContextCancelsBlockedStore(t *testing.T) {
	t.Setenv("BERTHY_TEST_TOKEN", "")
	path := writeConfig(t, cfgDev)
	synctest.Test(t, func(t *testing.T) {
		store := credtest.NewMemory()
		entered, release := make(chan struct{}), make(chan struct{})
		store.OnCall, store.Block = func() { close(entered) }, release
		old := TokenStore
		TokenStore = store
		ctx, cancel := context.WithCancel(context.Background())
		result := make(chan error, 1)
		done := make(chan struct{})
		go func() {
			defer close(done)
			_, err := ResolveContext(ctx, path, Selector{Environment: "dev"})
			result <- err
		}()
		defer func() {
			cancel()
			close(release)
			<-done
			store.Wait()
			TokenStore = old
		}()
		select {
		case <-entered:
		case err := <-result:
			t.Fatalf("resolve returned before entering the store: %v", err)
		}
		cancel()
		synctest.Wait()
		select {
		case err := <-result:
			if err == nil {
				t.Fatal("resolve succeeded after cancellation")
			}
		default:
			t.Fatal("resolve did not return while the credential store was blocked")
		}
	})
}
func TestTheDefaultTokenStoreIsNone(t *testing.T) {
	if TokenStore != credstore.None() {
		t.Fatalf("default token store is %T, want the disabled store", TokenStore)
	}
}

func TestResolveRefusesAmbiguousTokenVariableBeforeStore(t *testing.T) {
	s := credtest.NewMemory()
	good := Environment{URL: "http://host#X", TokenEnv: "Y"}
	bad := Environment{URL: "http://host", TokenEnv: "X#Y"}
	if StoreKey("demo", "dev", good).Account() != StoreKey("demo", "dev", bad).Account() {
		t.Fatal("fixture keys do not collide")
	}
	if err := s.Set(StoreKey("demo", "dev", good), "admin"); err != nil {
		t.Fatal(err)
	}
	old := TokenStore
	TokenStore = s
	t.Cleanup(func() { TokenStore = old })
	calls := s.Calls()
	_, err := Resolve(writeConfig(t, `{"projects":{"demo":{"environments":{"dev":{"url":"http://host","token_env":"X#Y"}}}}}`), Selector{Environment: "dev"})
	if err == nil || err.Error() != "no token for demo/dev: its token variable is empty (berthy context list names it)" || s.Calls() != calls {
		t.Fatalf("error %v, store calls %d", err, s.Calls()-calls)
	}
}

func TestResolveRejectsInvalidScopeBeforeStore(t *testing.T) {
	for _, tc := range []struct{ project, env string }{{"bad@name", "dev"}, {"bad/name", "dev"}, {"demo", "bad@env"}, {"demo", "bad/env"}, {strings.Repeat("a", 64), "dev"}} {
		t.Run(tc.project+"_"+tc.env, func(t *testing.T) {
			s := credtest.NewMemory()
			old := TokenStore
			TokenStore = s
			t.Cleanup(func() { TokenStore = old })
			t.Setenv("SCOPE_TOKEN", "")
			cfg := writeConfig(t, fmt.Sprintf(`{"projects":{%q:{"environments":{%q:{"url":"http://host","token_env":"SCOPE_TOKEN"}}}}}`, tc.project, tc.env))
			_, err := Resolve(cfg, Selector{Project: tc.project, Environment: tc.env})
			if err == nil || err.Error() != "unknown environment or missing token_env" || s.Calls() != 0 {
				t.Fatalf("error %v, calls %d", err, s.Calls())
			}
		})
	}
}
func TestResolveAllowsUnusualVariableWithoutStore(t *testing.T) {
	s := credtest.NewMemory()
	old := TokenStore
	TokenStore = s
	t.Cleanup(func() { TokenStore = old })
	cfg := writeConfig(t, `{"projects":{"demo":{"environments":{"dev":{"url":"http://host","token_env":"BERTHY.TOKEN"}}}}}`)
	t.Setenv("BERTHY.TOKEN", "from-variable")
	target, err := Resolve(cfg, Selector{Environment: "dev"})
	if err != nil || target.Token != "from-variable" || s.Calls() != 0 {
		t.Fatalf("target %+v error %v calls %d", target, err, s.Calls())
	}
	t.Setenv("BERTHY.TOKEN", "")
	_, err = Resolve(cfg, Selector{Environment: "dev"})
	want := "no token for demo/dev: its token variable is empty (berthy context list names it)"
	if err == nil || err.Error() != want || s.Calls() != 0 {
		t.Fatalf("error %v calls %d", err, s.Calls())
	}
}
