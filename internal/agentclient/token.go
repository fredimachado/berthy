package agentclient

import (
	"context"
	"errors"
	"fmt"
	"time"

	"berthy/internal/credstore"
	"berthy/internal/scope"
)

type TokenSource string

const (
	TokenFromEnv   TokenSource = "env"
	TokenFromStore TokenSource = "store"
)

// TokenStore is the process credential store. Its default never accesses the OS store.
var TokenStore credstore.Store = credstore.None()

// StoreWait bounds each credential store call.
var StoreWait = 30 * time.Second

// Tokens returns the credential store with bounded calls.
func Tokens(ctx context.Context) credstore.Store {
	return credstore.Bounded(ctx, TokenStore, StoreWait)
}

// StoreKey identifies the token for one configuration entry.
func StoreKey(project, env string, e Environment) credstore.Key {
	return credstore.Key{URL: e.URL, Project: project, Environment: env, Variable: e.TokenEnv}
}

// StoreUsable reports whether an entry can address an unambiguous store key.
func StoreUsable(project, env string, e Environment) bool {
	return scope.ValidProject(project) && scope.ValidEnvironment(env) && ValidTokenEnv(e.TokenEnv)
}

// FindToken reads the variable first, then the store when provided.
func FindToken(getenv func(string) string, store credstore.Store, project, env string, e Environment) (string, TokenSource, error) {
	if token := getenv(e.TokenEnv); token != "" {
		return token, TokenFromEnv, nil
	}
	prefix := fmt.Sprintf("no token for %s/%s: its token variable is empty", project, env)
	if store == nil {
		return "", "", errors.New(prefix + " (berthy context list names it)")
	}
	token, err := store.Get(StoreKey(project, env, e))
	if err == nil {
		return token, TokenFromStore, nil
	}
	if errors.Is(err, credstore.ErrNotFound) {
		return "", "", fmt.Errorf("%s and the %s has none saved for it; run berthy setup --new-token, or set the variable (berthy context list names it)", prefix, store.Name())
	}
	return "", "", errors.New(prefix + " and this machine has no credential store; set the variable (berthy context list names it)")
}
