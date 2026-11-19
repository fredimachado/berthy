package deploy

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"berthy/internal/engine"
	"berthy/internal/session"
)

// maxSessions is the most interactive sessions an agent holds at once.
const maxSessions = 16

// sessionStopGrace is the seconds the container of a run session gets to stop on SIGTERM when its session is closed: the
// person has left, so there is no point in waiting as long as a run that timed out.
const sessionStopGrace = 3

// ErrTooManySessions is the refusal of a session while maxSessions are open.
var ErrTooManySessions = errors.New("too many interactive sessions")

// errSessionTarget is the refusal of an exec session whose container cannot be chosen, as the exec operation's own.
const errSessionTarget Refusal = "exec target unavailable"

// errSessionAdmin is the refusal of a session for a person who is not an administrator.
const errSessionAdmin Refusal = "administrator required"

// errSessionsStopped is the refusal of a session once the agent has begun to stop.
const errSessionsStopped Refusal = "the agent is stopping"

// execSessionEngine is what an exec session needs of Docker beyond choosing the container.
type execSessionEngine interface {
	StartExecSession(ctx context.Context, containerID string, command []string, tty bool) (*engine.Stream, string, error)
	ResizeExec(ctx context.Context, execID string, rows, cols uint16) error
	ExecExitCode(ctx context.Context, execID string) (int, error)
}

// runSessionEngine is what a run session needs of Docker: the one-off container calls, and the attach and resize.
type runSessionEngine interface {
	runEngine
	AttachSession(ctx context.Context, containerID string) (*engine.Stream, error)
	ResizeContainer(ctx context.Context, containerID string, rows, cols uint16) error
}

// orphanEngine is what removing the containers of earlier run sessions needs of Docker.
type orphanEngine interface {
	InventoryContainers(context.Context) ([]engine.InventoryContainer, error)
	RemoveContainer(context.Context, string) error
}

// sessionSet counts the sessions that are being opened or are open, and lists the open ones. Its zero value is ready. A session
// is listed only once it is wholly open, so closing the listed ones never meets one that is half made.
type sessionSet struct {
	mu      sync.Mutex
	used    int // slots taken: sessions being opened and sessions open
	open    map[*Session]struct{}
	stopped bool // the agent is stopping: no session is admitted any more
	holds   int  // handlers that have not yet finished with their session, the record of its end included
}

// hold counts a handler that is serving a session request, and returns the function that says it is done.
func (set *sessionSet) hold() func() {
	set.mu.Lock()
	set.holds++
	set.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			set.mu.Lock()
			set.holds--
			set.mu.Unlock()
		})
	}
}

func (set *sessionSet) held() int {
	set.mu.Lock()
	defer set.mu.Unlock()
	return set.holds
}

// reserve takes a slot for a session that is about to be opened.
func (set *sessionSet) reserve() error {
	set.mu.Lock()
	defer set.mu.Unlock()
	if set.stopped {
		return errSessionsStopped
	}
	if set.used >= maxSessions {
		return ErrTooManySessions
	}
	set.used++
	return nil
}

// unreserve gives back the slot of a session that could not be opened.
func (set *sessionSet) unreserve() {
	set.mu.Lock()
	defer set.mu.Unlock()
	set.used--
}

// publish lists a session that is wholly open. It refuses once the agent is stopping, which closes every session it lists.
func (set *sessionSet) publish(s *Session) error {
	set.mu.Lock()
	defer set.mu.Unlock()
	if set.stopped {
		return errSessionsStopped
	}
	if set.open == nil {
		set.open = map[*Session]struct{}{}
	}
	set.open[s] = struct{}{}
	return nil
}

// remove gives back the slot of a session that has been closed.
func (set *sessionSet) remove(s *Session) {
	set.mu.Lock()
	defer set.mu.Unlock()
	delete(set.open, s)
	set.used--
}

func (set *sessionSet) all() []*Session {
	set.mu.Lock()
	defer set.mu.Unlock()
	out := make([]*Session, 0, len(set.open))
	for s := range set.open {
		out = append(out, s)
	}
	return out
}

// Session is an interactive command the agent runs for a person: an exec in a running container, or a run in a new
// container of the applied release. It is not an operation: it takes no lock and is not journaled.
type Session struct {
	Kind, App string
	// Target is where the command runs, as in ExecPlan: the Swarm service, the Compose project and service, or the image of a run.
	Target string
	TTY    bool

	c      *Controller
	ctx    context.Context // outlives the request that opened the session
	cancel context.CancelFunc
	stream *engine.Stream
	// resize, wait and release are the parts of a session that differ between an exec and a run.
	resize  func(context.Context, session.Size) error
	wait    func(context.Context) (int, error)
	release func(context.Context) error

	closeOnce sync.Once
	closeErr  error
}

// OpenSession checks the request and starts the command, so a refusal is known before the connection is taken over. The
// caller closes the session.
func (c *Controller) OpenSession(ctx context.Context, actor Actor, req session.Request) (*Session, error) {
	if !actor.Admin {
		return nil, errSessionAdmin
	}
	if err := req.Check(); err != nil {
		return nil, Refusal(err.Error())
	}
	s := &Session{Kind: req.Kind, App: req.App, TTY: req.TTY, c: c}
	if err := c.sessions.reserve(); err != nil {
		return nil, err
	}
	s.ctx, s.cancel = context.WithCancel(context.WithoutCancel(ctx))
	var err error
	if req.Kind == "exec" {
		err = c.openExecSession(ctx, s, req)
	} else {
		err = c.openRunSession(ctx, s, req)
	}
	if err != nil {
		s.cancel()
		c.sessions.unreserve()
		return nil, err
	}
	if err = c.sessions.publish(s); err != nil {
		// The agent began to stop while the session was opened: it is closed here, as CloseSessions would have.
		closeCtx, cancel := captureContext(ctx)
		defer cancel()
		_ = s.Close(closeCtx)
		return nil, err
	}
	return s, nil
}

// publicOr is err when it is a refusal, which says what to do, and otherwise the refusal fallback.
func publicOr(err error, fallback Refusal) error {
	var r Refusal
	if errors.As(err, &r) {
		return r
	}
	return fallback
}

func (c *Controller) openExecSession(ctx context.Context, s *Session, req session.Request) error {
	e, ok := c.Engine.(execSessionEngine)
	if !ok {
		return errors.New("interactive sessions unavailable")
	}
	container, target, err := c.execTarget(ctx, Request{App: req.App, Role: req.Role, Service: req.Service, Command: req.Command})
	if err != nil {
		return publicOr(err, errSessionTarget)
	}
	stream, execID, err := e.StartExecSession(s.ctx, container, req.Command, req.TTY)
	if err != nil {
		return errors.New("interactive session unavailable")
	}
	s.Target, s.stream = target, stream
	s.resize = func(ctx context.Context, size session.Size) error {
		return e.ResizeExec(ctx, execID, size.Rows, size.Cols)
	}
	s.wait = func(ctx context.Context) (int, error) {
		// Docker records the exit code a moment after the output ends.
		for range 50 {
			code, err := e.ExecExitCode(ctx, execID)
			if err == nil {
				return code, nil
			}
			select {
			case <-ctx.Done():
				return 0, ctx.Err()
			case <-time.After(100 * time.Millisecond):
			}
		}
		return 0, errors.New("exit status unavailable")
	}
	s.release = func(context.Context) error { return nil }
	s.setSize(ctx, req)
	return nil
}

// setSize gives the terminal its first size. A terminal that cannot be sized still works at Docker's default size, so a
// failure here ends nothing.
func (s *Session) setSize(ctx context.Context, req session.Request) {
	if req.TTY && req.Size != nil {
		_ = s.resize(ctx, *req.Size)
	}
}

func (c *Controller) openRunSession(ctx context.Context, s *Session, req session.Request) error {
	e, ok := c.Engine.(runSessionEngine)
	if !ok {
		return errors.New("interactive sessions unavailable")
	}
	t, p, err := c.prepareRun(ctx, Request{App: req.App, Role: req.Role, Command: req.Command, Timeout: req.Timeout})
	if err != nil {
		return publicOr(err, errRunPrepare)
	}
	limit := t.Timeout
	if req.Timeout == "" {
		limit = 0 // runTarget gives the default hour; an interactive run ends when the person leaves
	}
	var random [12]byte
	if _, err = rand.Read(random[:]); err != nil {
		return errRunPrepare
	}
	id := hex.EncodeToString(random[:])
	labels := map[string]string{"berthy.environment": c.Environment, "berthy.session": id, "berthy.oneoff": "session"}
	spec, err := p.oneOffSpec(req.Command, t.Role, t.Limits, t.Volumes, labels, &terminal{TTY: req.TTY})
	if err != nil {
		return errRunPrepare
	}
	container, err := e.CreateContainer(ctx, "berthy-session-"+id, spec)
	if err != nil || container == "" {
		return errors.New("interactive session unavailable")
	}
	var exited atomic.Bool
	remove := func(ctx context.Context) error {
		if !exited.Load() {
			// The person left while the command ran. It may ignore SIGTERM, so Docker kills it after the grace; a failure to
			// stop is not final, because the removal below kills it as well.
			_ = e.StopContainer(ctx, container, sessionStopGrace)
		}
		err := e.RemoveContainer(ctx, container)
		var status *engine.StatusError
		if errors.As(err, &status) && status.Code == 404 {
			return nil
		}
		return err
	}
	fail := func(err error) error {
		rmCtx, cancel := captureContext(ctx)
		defer cancel()
		_ = e.RemoveContainer(rmCtx, container)
		return err
	}
	// The attach comes before the start, so the first output is not lost.
	stream, err := e.AttachSession(s.ctx, container)
	if err != nil {
		return fail(errors.New("interactive session unavailable"))
	}
	if err = e.StartContainer(ctx, container); err != nil {
		stream.Close()
		return fail(errors.New("interactive session unavailable"))
	}
	s.Target, s.stream = t.Release.Digest, stream
	s.resize = func(ctx context.Context, size session.Size) error {
		return e.ResizeContainer(ctx, container, size.Rows, size.Cols)
	}
	var timedOut atomic.Bool
	var timer *time.Timer
	if limit > 0 {
		timer = time.AfterFunc(limit, func() {
			timedOut.Store(true)
			stopCtx, cancel := captureContext(s.ctx)
			defer cancel()
			_ = e.StopContainer(stopCtx, container, runStopGrace)
		})
	}
	s.wait = func(ctx context.Context) (int, error) {
		code, err := e.WaitContainer(ctx, container)
		if err != nil {
			return 0, err
		}
		exited.Store(true)
		if timedOut.Load() {
			return 0, Refusal("run timed out after " + limit.String())
		}
		return code, nil
	}
	s.release = func(ctx context.Context) error {
		if timer != nil {
			timer.Stop()
		}
		return remove(ctx)
	}
	s.setSize(ctx, req)
	return nil
}

// Stream is the command's two-way stream: raw with a terminal, Docker-framed without one (use engine.Demux).
func (s *Session) Stream() *engine.Stream { return s.stream }

// Closed reports whether the session has been closed, by its owner or by the agent stopping.
func (s *Session) Closed() bool { return s.ctx.Err() != nil }

// Resize gives the command's terminal a new size. A session without a terminal has none to resize.
func (s *Session) Resize(ctx context.Context, size session.Size) error {
	if !s.TTY {
		return nil
	}
	return s.resize(ctx, size)
}

// Wait returns the command's exit code once its output has ended. A run past its timeout is stopped and reported as a
// refusal that says so.
func (s *Session) Wait(ctx context.Context) (int, error) { return s.wait(ctx) }

// Close ends the session: it drops the stream, and for a run it stops and removes the container. An exec's command may keep
// running in its container, as with docker exec. It is idempotent.
func (s *Session) Close(ctx context.Context) error {
	s.closeOnce.Do(func() {
		s.cancel()
		s.stream.Close()
		s.closeErr = s.release(ctx)
		s.c.sessions.remove(s)
	})
	return s.closeErr
}

// TrackSession is called by whoever serves a session request, before it opens the session, and the function it returns once
// it has nothing left to do for it, such as recording its end. CloseSessions waits for every such handler, so the end of a session
// that the agent's stopping closed is recorded before the agent exits.
func (c *Controller) TrackSession() (done func()) { return c.sessions.hold() }

// CloseSessions closes every open session, for the agent's shutdown, and admits no more: the containers of run sessions are stopped and removed
// before it returns, or when ctx ends.
func (c *Controller) CloseSessions(ctx context.Context) {
	c.sessions.mu.Lock()
	c.sessions.stopped = true
	c.sessions.mu.Unlock()
	var wg sync.WaitGroup
	for _, s := range c.sessions.all() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = s.Close(ctx)
		}()
	}
	wg.Wait()
	for c.sessions.held() > 0 && ctx.Err() == nil {
		time.Sleep(10 * time.Millisecond)
	}
}

// RemoveOrphanSessions removes the containers of run sessions and of job runs an earlier agent process left behind: a running
// agent owns neither of an earlier process, whatever state its container is in. Containers of other environments are left alone.
func (c *Controller) RemoveOrphanSessions(ctx context.Context) error {
	e, ok := c.Engine.(orphanEngine)
	if !ok {
		return errors.New("session cleanup unavailable")
	}
	containers, err := e.InventoryContainers(ctx)
	if err != nil {
		return errors.New("session cleanup unavailable")
	}
	var failed error
	for _, ct := range containers {
		_, session := ct.Labels["berthy.session"]
		if !session && !commandKinds[ct.Labels["berthy.oneoff"]] || ct.Labels["berthy.environment"] != c.Environment {
			continue
		}
		if err = e.RemoveContainer(ctx, ct.ID); err != nil {
			failed = errors.New("session container removal failed")
		}
	}
	return failed
}
