package agentclient

import (
	"berthy/internal/credstore"
	"berthy/internal/credstore/credtest"
	"testing"
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
func TestTheDefaultTokenStoreIsNone(t *testing.T) {
	if _, err := TokenStore.Get(credstore.Key{}); err != credstore.ErrUnavailable {
		t.Fatalf("%v", err)
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
	if err == nil || err.Error() != "unknown environment or missing token_env" || s.Calls() != calls {
		t.Fatalf("error %v, store calls %d", err, s.Calls()-calls)
	}
}
