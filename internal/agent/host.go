package agent

import (
	"context"
	"fmt"
	"net/http"

	"berthy/internal/scope"
)

// Host is the hosted environments of one agent and the router of their API.
type Host struct {
	// runtimes is by ID and never changes after Open, so requests read it without a lock. order is the order of the file.
	runtimes map[scope.ID]*Runtime
	order    []*Runtime
}

// Open opens every hosted environment of the file and wires what they share. When one fails it closes those already opened
// and returns the error. Nothing runs and nothing is served yet.
//
// The order is the rule later steps rely on: every state is opened and claimed, then what the environments share is set on
// every runtime as plain fields, and only then may Start run a worker. Nothing is written to a runtime after Open returns.
func Open(sh Shared, f File) (*Host, error) {
	h := &Host{runtimes: map[scope.ID]*Runtime{}}
	for _, c := range f.Configs() {
		r, err := OpenRuntime(sh, c)
		if err != nil {
			h.Close()
			return nil, err
		}
		h.order = append(h.order, r)
		h.runtimes[c.ID()] = r
	}
	for _, r := range h.order {
		if err := r.deployment.Store.Claim(context.Background(), r.Config.Project); err != nil {
			h.Close()
			return nil, fmt.Errorf("state in %s: %w", r.Config.StateDir, err)
		}
	}
	return h, nil
}

// Start starts every hosted environment. The caller serves the Host only after it returns. When one fails to start, the
// caller still closes the Host.
func (h *Host) Start(ctx context.Context) error {
	for _, r := range h.order {
		if err := r.Start(ctx); err != nil {
			return fmt.Errorf("%s: %w", r.Config.ID(), err)
		}
	}
	return nil
}

// ServeHTTP sends a request to the environment its URL names. A request for any other path is refused by refuseOutside, the
// same answer a runtime gives, so an environment that does not exist is not told apart from a bad token.
func (h *Host) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/healthz" {
		w.Header().Set("Content-Type", "application/json")
		serveHealth(w, r)
		return
	}
	if id, _, ok := environmentPath(r.URL.Path); ok {
		if rt := h.runtimes[id]; rt != nil {
			rt.Handler.ServeHTTP(w, r)
			return
		}
	}
	refuseOutside(w, r.URL.Path)
}

// CloseSessions closes the open interactive sessions of every environment.
func (h *Host) CloseSessions(ctx context.Context) {
	for _, r := range h.order {
		r.CloseSessions(ctx)
	}
}

// Close closes the environments in reverse order. It may follow a failed Open or Start.
func (h *Host) Close() {
	for i := len(h.order) - 1; i >= 0; i-- {
		h.order[i].Close()
	}
}
