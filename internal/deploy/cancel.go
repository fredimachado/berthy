package deploy

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"

	"berthy/internal/state"
)

// Cancelling an operation.
//
// An operation can be cancelled while it can still be undone cleanly, and not after. What decides is its point of no return, one
// per operation, which the operation records in the state in the same transaction as the step that crosses it
// (state.Intent.NoReturn). Before it, a cancel wins: the operation stops where it is and ends as failed, or as rolled_back when
// a rollout had to be taken back. After it, a cancel is refused with the reason the operation recorded.
//
// Where the point is, by kind:
//
//	deploy, rollback   the step that records the verified release, just before the routes are published and the post-deploy
//	                   commands run. A Compose deploy: the step that journals the file it is about to bring up.
//	run                none. A run can be cancelled until it has ended.
//	any other kind     they cannot be cancelled (restore, remove, destroy, exec, restart, scale, prune, ingress).
//
// A migration is no point of no return, since the deploy that runs it is still undone cleanly by a rollout that fails after it.
// But the migration itself cannot be stopped without leaving the schema half changed, so a cancel is refused while one runs.
//
// The controller owns the cancel, as it owns the goroutine of every operation it admits. Each running operation has one opRun.
// Its mutex is the only place where "a cancel was accepted" and "the operation crossed" are decided, so they cannot both happen.

// Cancelled is why a cancelled operation stopped. It is the cause of the context that its waits end with, and of the error a
// step returns when the cancel arrived before it began.
type Cancelled struct{ By string }

func (e *Cancelled) Error() string { return "cancelled by " + e.By }

// CancelRefused is the answer to a cancel that cannot be accepted. Its text says why, and is public.
type CancelRefused string

func (e CancelRefused) Error() string { return string(e) }

// CancelPlan is what a cancel does, or would do: the operation, and whether the cancel is accepted. A cancel that is not
// accepted says why.
type CancelPlan struct {
	OperationID string `json:"operation_id"`
	Kind        string `json:"kind"`
	App         string `json:"app,omitempty"`
	Accepted    bool   `json:"accepted"`
	Reason      string `json:"reason,omitempty"`
}

// The reasons a cancel is refused, apart from the kind of the operation and the one its point of no return records.
const (
	reasonMigration  = "a migration is running"
	reasonRevert     = "a schema revert is running"
	reasonEnding     = "the operation is ending"
	reasonNotRunning = "the operation is not running on this agent"
	// reasonSwitched is recorded with the barrier of a deploy: from then on the release is verified, and the routes and the
	// post-deploy commands follow.
	reasonSwitched = "the deploy already switched traffic"
	reasonCompose  = "the Compose project is being changed"
)

// opRun is the cancel state of one running operation. Every field is guarded by mu.
type opRun struct {
	mu sync.Mutex
	// by is who cancelled the operation, empty until a cancel is accepted.
	by string
	// busy is the reason a cancel is refused while a step that cannot be stopped runs, such as a migration. It is empty between
	// such steps.
	busy string
	// sealed is set when the operation has decided how it ends, so a cancel that comes after it is refused.
	sealed bool
	// ctx ends, with a *Cancelled as its cause, when a cancel is accepted.
	ctx  context.Context
	stop context.CancelCauseFunc
}

type runKey struct{}

// runFrom is the cancel state of the operation whose goroutine runs with ctx. It is nil for a context that is not one's.
func runFrom(ctx context.Context) *opRun {
	r, _ := ctx.Value(runKey{}).(*opRun)
	return r
}

// register makes the cancel state of an operation Admit has started, and returns the context its goroutine runs with. The
// context never ends: a cancel ends only the waits a step makes interruptible, so the operation can still write what it did.
func (c *Controller) register(opID string) context.Context {
	rctx, stop := context.WithCancelCause(context.Background())
	r := &opRun{ctx: rctx, stop: stop}
	c.runsMu.Lock()
	if c.runs == nil {
		c.runs = map[string]*opRun{}
	}
	c.runs[opID] = r
	c.runsMu.Unlock()
	return context.WithValue(context.Background(), runKey{}, r)
}

// unregister forgets an operation whose goroutine is about to end.
func (c *Controller) unregister(opID string) {
	c.runsMu.Lock()
	r := c.runs[opID]
	delete(c.runs, opID)
	c.runsMu.Unlock()
	if r != nil {
		r.stop(nil)
	}
}

// interruptible returns a context that ends when ctx does, and when a cancel of the operation is accepted, with the *Cancelled
// as its cause. A step puts its waiting calls on it: the pull, a command's container, the wait for health. A context that is
// not an operation's is returned as it is.
func interruptible(ctx context.Context) (context.Context, context.CancelFunc) {
	r := runFrom(ctx)
	if r == nil {
		return ctx, func() {}
	}
	child, cancel := context.WithCancelCause(ctx)
	stop := context.AfterFunc(r.ctx, func() { cancel(context.Cause(r.ctx)) })
	return child, func() {
		stop()
		cancel(context.Canceled)
	}
}

// cancelledBy returns the *Cancelled of the operation if a cancel was accepted, and nil otherwise. A step calls it before it
// begins, so a cancel that came between two steps stops the operation at the second.
func cancelledBy(ctx context.Context) error {
	r := runFrom(ctx)
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.by != "" {
		return &Cancelled{By: r.by}
	}
	return nil
}

// isCancelled reports whether err is, or wraps, the cancel of an operation.
func isCancelled(err error) bool {
	var c *Cancelled
	return errors.As(err, &c)
}

// cross records an intent that is the operation's point of no return, with reason, unless a cancel was accepted first, in which
// case nothing is recorded and the *Cancelled is returned. Accepting the cancel and recording the step are both done under the
// operation's mutex, so exactly one of them happens first.
func (c *Controller) cross(ctx context.Context, opID string, i state.Intent, reason string) error {
	i.NoReturn = reason
	r := runFrom(ctx)
	if r == nil {
		return c.Store.RecordIntent(ctx, opID, i)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.by != "" {
		return &Cancelled{By: r.by}
	}
	return c.Store.RecordIntent(ctx, opID, i)
}

// enter begins a step that a cancel is refused during, with the reason it gives. The returned function ends the step. When a
// cancel was accepted before, the step does not begin and the *Cancelled is returned.
func (c *Controller) enter(ctx context.Context, reason string) (func(), error) {
	r := runFrom(ctx)
	if r == nil {
		return func() {}, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.by != "" {
		return nil, &Cancelled{By: r.by}
	}
	r.busy = reason
	return func() {
		r.mu.Lock()
		r.busy = ""
		r.mu.Unlock()
	}, nil
}

// conclude settles how an operation ends. After it, a cancel is refused. When a cancel was accepted, the operation ends as the
// cancel says: an outcome that succeeded did so without crossing its point of no return, and ends as failed, with none of what it
// would have recorded; any other keeps its state and gets the reason. An operation that is not cancellable, such as a recovery,
// ends as it is.
func conclude(ctx context.Context, o state.Outcome) state.Outcome {
	r := runFrom(ctx)
	if r == nil {
		return o
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.by == "" {
		r.sealed = true
		return o
	}
	if o.State == "succeeded" {
		o.State, o.Release, o.Settings, o.Protected, o.Event = "failed", nil, nil, false, nil
	}
	if o.State == "failed" || o.State == "rolled_back" {
		o.Err = errCancelled
	}
	o.Reason = (&Cancelled{By: r.by}).Error()
	return o
}

// errCancelled is the failure text of an operation that was cancelled. Its reason names who cancelled it.
const errCancelled = "operation cancelled"

// cancelRefusal is why an operation of this kind is never cancelled, or "" for a kind that can be.
func cancelRefusal(kind string) string {
	switch kind {
	case "deploy", "rollback", "run":
		return ""
	}
	article := "a "
	if strings.ContainsRune("aeiou", rune(kind[0])) {
		article = "an "
	}
	return article + kind + " cannot be cancelled"
}

// cancelName is the name of the actor in the reason of a cancelled operation: printable ASCII only, since the reason is public.
func cancelName(name string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r > 0x7e {
			return '?'
		}
		return r
	}, name)
}

// Cancel stops the running operation id when it can still be undone cleanly, and refuses when it cannot. A dry run says what
// the answer would be and changes nothing. An accepted cancel returns at once: the operation stops, takes back what it changed
// and ends as failed or rolled_back, with the reason "cancelled by <actor>". The cancel, accepted or refused, is audited.
func (c *Controller) Cancel(ctx context.Context, actor Actor, id string, dryRun bool) (CancelPlan, error) {
	if c.Store == nil {
		return CancelPlan{}, errors.New("state unavailable")
	}
	op, err := c.Store.Operation(ctx, id)
	if err != nil {
		return CancelPlan{}, err
	}
	plan := CancelPlan{OperationID: op.ID, Kind: op.Kind, App: op.App}
	audit := func(outcome string) {
		if !dryRun {
			_ = c.Store.RecordAudit(ctx, state.Actor{ID: actor.ID, Name: actor.Name, Remote: actor.Remote}, "cancel:"+op.ID, outcome)
		}
	}
	refuse := func(reason string) (CancelPlan, error) {
		plan.Reason = reason
		if dryRun {
			return plan, nil
		}
		audit("refused")
		return plan, CancelRefused(reason)
	}
	if reason := cancelRefusal(op.Kind); reason != "" {
		return refuse(reason)
	}
	c.runsMu.Lock()
	r := c.runs[id]
	c.runsMu.Unlock()
	if r == nil {
		if op.State != "running" {
			return refuse("the operation already ended: " + op.State)
		}
		return refuse(reasonNotRunning)
	}
	// The state is read again under the mutex of the operation: crossing the point of no return writes it under the same one.
	r.mu.Lock()
	defer r.mu.Unlock()
	cur, err := c.Store.Operation(ctx, id)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return CancelPlan{}, err
	case err != nil:
		return CancelPlan{}, err
	case cur.State != "running":
		return refuse("the operation already ended: " + cur.State)
	case cur.NoReturn != "":
		return refuse(cur.NoReturn)
	case r.busy != "":
		return refuse(r.busy)
	case r.sealed:
		return refuse(reasonEnding)
	}
	plan.Accepted = true
	if dryRun || r.by != "" {
		return plan, nil
	}
	r.by = cancelName(actor.Name)
	if r.by == "" {
		r.by = cancelName(actor.ID)
	}
	r.stop(&Cancelled{By: r.by})
	audit("accepted")
	return plan, nil
}
