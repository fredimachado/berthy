package agent

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"time"

	"berthy/internal/api"
	"berthy/internal/deploy"
	"berthy/internal/engine"
	"berthy/internal/state"
)

// doctorNetwork returns the shared network the doctor checks: the configured one, else the network of the environment's
// settings. It returns false when neither is known, as on a fresh agent before its first deploy.
func doctorNetwork(ctx context.Context, c Config, deployment *deploy.Controller) (string, bool) {
	if c.Network != "" {
		return c.Network, true
	}
	if deployment == nil || deployment.Store == nil {
		return "", false
	}
	m, err := settingsManifest(ctx, deployment.Store)
	if err != nil {
		return "", false
	}
	return m.NetworkName(c.Environment), true
}

// networkCheck reports whether the shared network exists as an attachable overlay of the swarm scope, which is the kind the
// apps join. Its detail is the name, and what is wrong with the network when it is not OK.
func networkCheck(ctx context.Context, e *engine.Engine, name string) api.Check {
	c := api.Check{Name: "network", Critical: true, Detail: name}
	if e == nil {
		c.Detail = name + ": unavailable"
		return c
	}
	n, err := e.InspectNetwork(ctx, name)
	var status *engine.StatusError
	switch {
	case errors.As(err, &status) && status.Code == http.StatusNotFound:
		c.Detail = name + ": missing"
	case err != nil:
		c.Detail = name + ": unavailable"
	case n.Driver != "overlay" || n.Scope != "swarm" || !n.Attachable:
		c.Detail = name + ": not an attachable overlay"
	default:
		c.OK = true
	}
	return c
}

// stateKeyCheck reports that a state key from before the last rotation is still on the server. It is not a failure: the key goes
// when the next state snapshot succeeds. There is no check for a state that holds only its one key.
func stateKeyCheck(s *state.Store) (api.Check, bool) {
	if !s.HasPreviousKey() {
		return api.Check{}, false
	}
	return api.Check{Name: "state_key", OK: true, Detail: "previous key kept until the next state snapshot succeeds"}, true
}

// lockCheck reports the deploy lock: free, or held by its holder for its age, with the reason it was taken. A held lock is a
// state, not a failure, so the check is never critical. It never shows a token.
func lockCheck(ctx context.Context, s *state.Store, now time.Time) api.Check {
	c := api.Check{Name: "lock", OK: true, Detail: "free"}
	l, err := s.Lock(ctx)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		c.Detail = "unavailable"
	default:
		c.Detail = "held by " + l.Holder + " for " + api.HumanAge(now.Sub(l.Acquired))
		if l.Reason != "" {
			c.Detail += ": " + l.Reason
		}
	}
	return c
}
