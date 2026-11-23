// Package credstore keeps the CLI's agent tokens in the credential store of the operating system.
package credstore

import (
	"errors"
	"fmt"
	"strings"
)

// Key names one stored token: the agent it is for, the hosted environment, and the token variable of the configuration
// entry it was saved for. Two entries for one target with different variables (a person's and a script's) never share one.
type Key struct {
	URL         string // the agent's URL as the CLI configuration holds it
	Project     string
	Environment string
	Variable    string // the entry's token_env
}

// account is the name the store files the token under: "todo/dev@http://10.0.0.5:7710#BERTHY_TODO_DEV_TOKEN".
// A trailing slash of URL is dropped.
func (k Key) account() string {
	return fmt.Sprintf("%s/%s@%s#%s", k.Project, k.Environment, strings.TrimRight(k.URL, "/"), k.Variable)
}

var ErrNotFound = errors.New("no stored token")

// ErrUnavailable wraps every failure of the store itself: no store, a locked or refusing store, a call that was given up.
var ErrUnavailable = errors.New("no credential store on this machine")

// ErrGivenUp is what a bounded call answers when it stopped waiting. It wraps ErrUnavailable. The call may still finish.
var ErrGivenUp = fmt.Errorf("%w: the store did not answer", ErrUnavailable)

// Store is a credential store that holds agent tokens.
type Store interface {
	Name() string                         // "Windows Credential Manager", "macOS Keychain", "Secret Service"
	Get(Key) (string, error)              // ErrNotFound, or an error that wraps ErrUnavailable
	Set(Key, string) error                // an error that wraps ErrUnavailable
	Delete(Key) (removed bool, err error) // (false, nil) when there was none
}

// Service is the service name the berthy command files its tokens under.
const Service = "berthy"

// StoreEnv names the variable that turns the credential store off: "none" means no store.
const StoreEnv = "BERTHY_TOKEN_STORE"

// FromEnvironment is the store the berthy command uses: None when $BERTHY_TOKEN_STORE is "none", OS(Service) when it is
// empty, and an error for any other value, so a misspelt "none" does not turn the store on.
func FromEnvironment(getenv func(string) string) (Store, error) {
	switch v := getenv(StoreEnv); v {
	case "none":
		return None(), nil
	case "":
		return OS(Service), nil
	default:
		return nil, fmt.Errorf("%s=%q: want empty or \"none\"", StoreEnv, v)
	}
}

// None returns a store that answers ErrUnavailable for every call. Name is "credential store".
func None() Store { return none{} }

type none struct{}

func (none) Name() string { return "credential store" }

func (none) Get(Key) (string, error) { return "", ErrUnavailable }

func (none) Set(Key, string) error { return ErrUnavailable }

func (none) Delete(Key) (bool, error) { return false, ErrUnavailable }
