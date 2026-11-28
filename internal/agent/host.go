package agent

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"

	"berthy/internal/api"
	"berthy/internal/deploy"
	"berthy/internal/scope"
	"berthy/internal/state"
)

// Host is the hosted environments of one agent and the router of their API.
type Host struct {
	cluster *Cluster
	// runtimes is by ID and never changes after Open, so requests read it without a lock. order is the order of the file.
	runtimes map[scope.ID]*Runtime
	order    []*Runtime
	ids      []scope.ID
	failed   map[scope.ID]string
	failures map[scope.ID]http.Handler
	log      func(string)
	// Frozen by Start before any ordinary worker or HTTP handler reads it.
	startupRows []api.Check
	claimsMu    sync.Mutex
	leases      map[scope.ID]map[state.Claim]int
}

// Open opens the environments of a validated file and wires what they share. Only stores with verified ownership and
// readable claims supply peer evidence. Nothing runs and nothing is served yet.
func Open(sh Shared, f File) (*Host, error) {
	cluster, err := OpenCluster(sh, f)
	if err != nil {
		return nil, err
	}
	h := &Host{
		cluster:  cluster,
		runtimes: map[scope.ID]*Runtime{}, failed: map[scope.ID]string{}, failures: map[scope.ID]http.Handler{},
		log: sh.Log, leases: map[scope.ID]map[state.Claim]int{},
	}
	ctx := context.Background()
	seen := map[state.Claim]scope.ID{}
	for _, c := range f.Configs() {
		id := c.ID()
		h.ids = append(h.ids, id)
		c.startupChecks = h.startupChecks
		c.clusterChecks = cluster.ChannelChecks
		c.clusterCheckRows = cluster.CheckRows
		r, err := OpenRuntime(sh, c)
		if err != nil {
			h.fail(c, nil, stateOpenReason(err), err)
			continue
		}
		store := r.deployment.Store
		if err = store.Claim(ctx, c.Project); err != nil {
			reason := stateOpenReason(err)
			var owner *state.OwnerError
			if errors.As(err, &owner) {
				reason = StartupStateOwner
			}
			r.Close()
			h.fail(c, nil, reason, err)
			continue
		}
		claims, err := store.Claims(ctx)
		if err != nil {
			r.Close()
			h.fail(c, nil, StartupStateClaims, err)
			continue
		}
		h.order = append(h.order, r)
		h.runtimes[id] = r
		for _, claim := range claims {
			if other, ok := seen[claim]; ok {
				h.Close()
				return nil, fmt.Errorf("environments %s and %s both hold %s %s: the agent does not start until one of them gives it up", other, id, claim.Kind, claim.Name)
			}
			seen[claim] = id
		}
	}
	for _, r := range h.order {
		id := r.Config.ID()
		r.hosted = h.ids
		r.deployment.Network = r.Config.Network
		r.deployment.Reserve = func(ctx context.Context, claims []state.Claim) ([]state.Claim, error) {
			return h.reserveContext(ctx, id, claims)
		}
		r.deployment.FinishReservation = func(ctx context.Context, leased, added, eligible []state.Claim) error {
			return h.finishConfirmed(ctx, id, leased, added, eligible, r.deployment.AbsentClaims)
		}
		r.deployment.Foreign = func(ctx context.Context) (deploy.Foreign, error) {
			h.claimsMu.Lock()
			defer h.claimsMu.Unlock()
			return h.foreign(ctx, id)
		}
		r.peerClaims = func(ctx context.Context) (map[state.Claim]bool, error) {
			h.claimsMu.Lock()
			defer h.claimsMu.Unlock()
			return h.foreignClaims(ctx, id)
		}
		if r.exporter != nil {
			r.exporter.Keep = r.keep
		}
		if r.Config.Network == "" {
			continue
		}
		claim := state.Claim{Kind: state.ClaimNetwork, Name: r.Config.Network}
		owner, held := seen[claim]
		if held && owner != id {
			h.fail(r.Config, r.deployment.Store, StartupNetwork, fmt.Errorf("network %q is held by %s", claim.Name, owner))
			continue
		}
		if !held {
			// Existing claims stay in force. A new one waits for a startup with complete peer evidence.
			if len(h.runtimes) != len(h.ids) {
				continue
			}
			if err := r.deployment.Store.AddClaims(ctx, []state.Claim{claim}); err != nil {
				h.fail(r.Config, r.deployment.Store, StartupNetwork, err)
				continue
			}
			seen[claim] = id
		}
		h.leases[id] = map[state.Claim]int{claim: 1}
	}
	return h, nil
}

func stateOpenReason(err error) startupFailureReason {
	switch {
	case errors.Is(err, state.ErrWrongKey):
		return StartupWrongKey
	case errors.Is(err, os.ErrPermission):
		return StartupStatePermission
	default:
		return StartupStateOpen
	}
}

func (h *Host) fail(c Config, store *state.Store, reason startupFailureReason, cause error) {
	id := c.ID()
	public := reason.String()
	h.failed[id] = public
	h.failures[id] = failedEnvironmentHandler(c, store, public)
	if h.log != nil {
		detail := strings.NewReplacer("\r", "\\r", "\n", "\\n").Replace(cause.Error())
		h.log(fmt.Sprintf("%s: %s%s%s", id, public, StartupDiagnosticSeparator, detail))
	}
}

func (h *Host) foreign(ctx context.Context, id scope.ID) (deploy.Foreign, error) {
	claims, err := h.foreignClaims(ctx, id)
	if err != nil {
		return deploy.Foreign{}, err
	}
	f := deploy.Foreign{Claims: claims, Images: map[string]bool{}}
	for _, r := range h.order {
		if r.Config.ID() == id {
			continue
		}
		ingress, err := r.deployment.Store.Ingress(ctx)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return deploy.Foreign{}, err
		}
		if err == nil {
			f.Ingress = append(f.Ingress, ingress)
		}
		kept, err := r.deployment.Store.KeptTunnel(ctx)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return deploy.Foreign{}, err
		}
		if err == nil {
			f.Ingress = append(f.Ingress, kept)
		}
		images, err := r.deployment.RetainedImages(ctx)
		if err != nil {
			return deploy.Foreign{}, err
		}
		for ref := range images {
			f.Images[ref] = true
		}
	}
	return f, nil
}

func (h *Host) foreignClaims(ctx context.Context, id scope.ID) (map[state.Claim]bool, error) {
	claims := map[state.Claim]bool{}
	for _, peer := range h.ids {
		if peer == id {
			continue
		}
		r := h.runtimes[peer]
		if r == nil {
			return nil, fmt.Errorf("state claims unavailable for %s", peer)
		}
		other, err := r.deployment.Store.Claims(ctx)
		if err != nil {
			return nil, err
		}
		for _, claim := range other {
			claims[claim] = true
		}
	}
	return claims, nil
}

func (h *Host) reserve(id scope.ID, claims []state.Claim) ([]state.Claim, error) {
	return h.reserveContext(context.Background(), id, claims)
}
func (h *Host) reserveContext(ctx context.Context, id scope.ID, claims []state.Claim) ([]state.Claim, error) {
	if len(claims) == 0 {
		return nil, nil
	}
	h.claimsMu.Lock()
	defer h.claimsMu.Unlock()
	foreign, err := h.foreignClaims(ctx, id)
	if err != nil {
		return nil, err
	}
	for _, c := range claims {
		if foreign[c] {
			return nil, deploy.ClaimConflict{Claim: c}
		}
	}
	added, err := h.runtimes[id].deployment.Store.AddClaimsNew(ctx, claims)
	if err != nil {
		return nil, err
	}
	if h.leases[id] == nil {
		h.leases[id] = map[state.Claim]int{}
	}
	seen := map[state.Claim]bool{}
	for _, claim := range claims {
		if !seen[claim] {
			h.leases[id][claim]++
			seen[claim] = true
		}
	}
	return added, nil
}

// finish drops leases and releases confirmed-absent targets only while no operation uses them.
func (h *Host) finish(ctx context.Context, id scope.ID, leased, candidates, eligible []state.Claim) error {
	return h.finishConfirmed(ctx, id, leased, candidates, eligible, nil)
}

func (h *Host) finishConfirmed(ctx context.Context, id scope.ID, leased, candidates, eligible []state.Claim, confirm func(context.Context, []state.Claim) []state.Claim) error {
	h.claimsMu.Lock()
	defer h.claimsMu.Unlock()
	seen := map[state.Claim]bool{}
	for _, claim := range leased {
		if seen[claim] {
			continue
		}
		seen[claim] = true
		if h.leases[id][claim] == 0 {
			return fmt.Errorf("claim has no lease")
		}
		h.leases[id][claim]--
		if h.leases[id][claim] == 0 {
			delete(h.leases[id], claim)
		}
	}
	var release []state.Claim
	for _, claim := range eligible {
		if slices.Contains(candidates, claim) && h.leases[id][claim] == 0 {
			release = append(release, claim)
		}
	}
	if confirm != nil && len(release) > 0 {
		release = confirm(ctx, release)
	}
	return h.runtimes[id].deployment.Store.ReleaseClaims(ctx, release)
}

// Start settles every startup result before any ordinary workers or requests can observe it.
func (h *Host) Start(ctx context.Context) error {
	type recovered struct {
		r   *Runtime
		ctx context.Context
	}
	var ready []recovered
	for _, r := range h.order {
		if err := ctx.Err(); err != nil {
			return err
		}
		if h.failed[r.Config.ID()] != "" {
			continue
		}
		runtimeCtx, cancel := context.WithCancel(ctx)
		r.cancel = cancel
		if err := r.recoverForStart(runtimeCtx); err != nil {
			r.stop()
			r.waitReports()
			if ctx.Err() != nil {
				return ctx.Err()
			}
			h.fail(r.Config, r.deployment.Store, StartupRecovery, err)
			continue
		}
		ready = append(ready, recovered{r, runtimeCtx})
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	h.startupRows = make([]api.Check, 0, len(h.ids))
	for _, id := range h.ids {
		reason, failed := h.failed[id]
		h.startupRows = append(h.startupRows, api.Check{Name: "hosted:" + id.String(), OK: !failed, Detail: reason})
	}
	for _, entry := range ready {
		entry.r.startWorkers(entry.ctx)
	}
	return nil
}

func (h *Host) startupChecks() []api.Check { return h.startupRows }

// ServeHTTP sends a request to the environment its URL names. A request for any other path is refused by refuseOutside, the
// same answer a runtime gives, so an environment that does not exist is not told apart from a bad token.
func (h *Host) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/healthz" {
		w.Header().Set("Content-Type", "application/json")
		serveHealth(w, r)
		return
	}
	if id, _, ok := environmentPath(r.URL.Path); ok {
		if failed := h.failures[id]; failed != nil {
			failed.ServeHTTP(w, r)
			return
		}
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

// Close closes the environments in reverse order. It may follow a failed Open or Start. Every exporter (and every other
// reader of peer claims) is stopped and drained before any store closes, so one runtime's final export cannot read a
// store another runtime has already closed.
func (h *Host) Close() {
	for i := len(h.order) - 1; i >= 0; i-- {
		h.order[i].stop()
	}
	for i := len(h.order) - 1; i >= 0; i-- {
		h.order[i].wait()
	}
	for i := len(h.order) - 1; i >= 0; i-- {
		h.order[i].closeStore()
	}
	h.cluster.Close()
}
