package agent

import (
	"context"
	"fmt"
	"net/http"
	"sync"

	"berthy/internal/deploy"
	"berthy/internal/scope"
	"berthy/internal/state"
)

// Host is the hosted environments of one agent and the router of their API.
type Host struct {
	// runtimes is by ID and never changes after Open, so requests read it without a lock. order is the order of the file.
	runtimes map[scope.ID]*Runtime
	order    []*Runtime
	claimsMu sync.Mutex
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
	seen := map[state.Claim]scope.ID{}
	for _, r := range h.order {
		claims, err := r.deployment.Store.Claims(context.Background())
		if err != nil {
			h.Close()
			return nil, err
		}
		for _, claim := range claims {
			if other, ok := seen[claim]; ok {
				h.Close()
				return nil, fmt.Errorf("environments %s and %s both hold %s %s: the agent does not start until one of them gives it up", other, r.Config.ID(), claim.Kind, claim.Name)
			}
			seen[claim] = r.Config.ID()
		}
	}
	for _, r := range h.order {
		id := r.Config.ID()
		r.deployment.Reserve = func(ctx context.Context, claims []state.Claim) error { return h.reserveContext(ctx, id, claims) }
		r.deployment.Foreign = func(ctx context.Context) (deploy.Foreign, error) {
			h.claimsMu.Lock()
			defer h.claimsMu.Unlock()
			return h.foreign(ctx, id)
		}
		if r.Config.Network != "" {
			if err := h.reserve(id, []state.Claim{{Kind: state.ClaimNetwork, Name: r.Config.Network}}); err != nil {
				h.Close()
				return nil, err
			}
		}
	}
	return h, nil
}

func (h *Host) foreign(ctx context.Context, id scope.ID) (deploy.Foreign, error) {
	f := deploy.Foreign{Claims: map[state.Claim]bool{}, Images: map[string]bool{}}
	for _, r := range h.order {
		if r.Config.ID() == id {
			continue
		}
		claims, err := r.deployment.Store.Claims(ctx)
		if err != nil {
			return deploy.Foreign{}, err
		}
		for _, c := range claims {
			f.Claims[c] = true
		}
	}
	return f, nil
}

func (h *Host) reserve(id scope.ID, claims []state.Claim) error {
	return h.reserveContext(context.Background(), id, claims)
}
func (h *Host) reserveContext(ctx context.Context, id scope.ID, claims []state.Claim) error {
	h.claimsMu.Lock()
	defer h.claimsMu.Unlock()
	f, err := h.foreign(ctx, id)
	if err != nil {
		return err
	}
	for _, c := range claims {
		if f.Claims[c] {
			return deploy.Refusal(fmt.Sprintf("name %s is taken by another project or environment on this server", c.Name))
		}
	}
	return h.runtimes[id].deployment.Store.AddClaims(ctx, claims)
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
