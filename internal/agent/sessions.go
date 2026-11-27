package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"berthy/internal/deploy"
	"berthy/internal/engine"
	"berthy/internal/session"
)

const (
	// maxSessionRequest is the largest JSON body of a session request.
	maxSessionRequest = 64 << 10
	// sessionCloseTime bounds how long ending a session may take: stopping and removing a run's container.
	sessionCloseTime = 30 * time.Second
	// sessionWaitTime bounds how long the exit status of a command whose output has ended may take to arrive.
	sessionWaitTime = 30 * time.Second
	// maxAuditCommand is how much of a session's command the audit log keeps.
	maxAuditCommand = 200
)

// errAgentStopping is what a session still open at the agent's shutdown is told.
const errAgentStopping = "the agent is stopping"

// serveSession is POST sessions: an interactive exec or run. The request asks to switch to the berthy-session protocol; once
// the session has started the agent answers 101 and takes the connection over, and the two sides exchange session frames
// until the command ends (see package session). A session is not an operation: it takes no lock and is not journaled, and an
// agent token on a protected environment is refused because a session cannot wait for an approval.
func (rt *routeRuntime) serveSession(call routeCall, read readRouteBody[session.Request]) {
	w, r, actor := call.w, call.r, call.actor
	d, gate, ctx := rt.d, rt.gate, r.Context()
	if actor.Agent {
		protected, err := gate.protected(ctx)
		if err != nil {
			writeError(w, 503, "state_unavailable", "state unavailable")
			return
		}
		if protected {
			writeError(w, 403, "forbidden", "an interactive session cannot be held for approval; use a person's token")
			return
		}
	}
	if !strings.EqualFold(r.Header.Get("Upgrade"), session.Upgrade) || !headerHasToken(r.Header, "Connection", "upgrade") {
		writeError(w, 400, "bad_request", "session requires an upgrade")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxSessionRequest) //nolint:forbidigo // Session requests use a smaller bound before shared decoding.
	req, err := read()
	if err != nil {
		writeError(w, 400, "bad_request", "invalid request")
		return
	}
	if err := req.Check(); err != nil {
		writeError(w, 400, "bad_request", err.Error())
		return
	}
	redact, ok := d.Redactor(ctx)
	if !ok {
		writeError(w, 503, "state_unavailable", "state unavailable")
		return
	}
	done := d.TrackSession()
	defer done()
	s, err := d.OpenSession(ctx, actor, req)
	switch {
	case errors.Is(err, deploy.ErrTooManySessions):
		writeError(w, 429, "too_many_sessions", err.Error())
		return
	case err != nil:
		operationError(w, err)
		return
	}
	sa := call.stateActor()
	action := sessionAction(req)
	closeSession := func() error {
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sessionCloseTime)
		defer cancel()
		return s.Close(closeCtx)
	}
	command := cleanText(redact(strings.Join(req.Command, " ")), maxAuditCommand)
	if err = d.Store.RecordAudit(ctx, sa, action, "started: "+command); err != nil {
		_ = closeSession()
		writeError(w, 503, "state_unavailable", "state unavailable")
		return
	}
	conn, brw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		_ = closeSession()
		_ = d.Store.RecordAudit(ctx, sa, action, "ended: the connection could not be taken over")
		writeError(w, 500, "internal", "session unavailable")
		return
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Time{})
	if _, err = io.WriteString(conn, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: "+session.Upgrade+"\r\n\r\n"); err != nil {
		_ = closeSession()
		_ = d.Store.RecordAudit(ctx, sa, action, "ended: client disconnected")
		return
	}
	outcome, end := bridge(ctx, s, conn, brw.Reader)
	err = closeSession()
	if err != nil {
		outcome += " (cleanup failed)"
	}
	// The session is over and the client has been told; a failure to record its end has nobody left to be reported to.
	_ = d.Store.RecordAudit(context.WithoutCancel(ctx), sa, action, "ended: "+outcome)
	end()
}

// sessionAction is the audit action of a session: session exec api/web, or session run api/web.
func sessionAction(req session.Request) string {
	target := req.App
	if req.Role != "" {
		target += "/" + req.Role
	} else if req.Service != "" {
		target += "/" + req.Service
	}
	return "session " + req.Kind + " " + cleanText(target, 120)
}

// headerHasToken reports whether a comma-separated header has the token, whatever its case.
func headerHasToken(h http.Header, name, token string) bool {
	for _, v := range h.Values(name) {
		for _, t := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(t), token) {
				return true
			}
		}
	}
	return false
}

// bridge carries the frames between the client and the session until the command ends or the client leaves, and returns how
// it ended, for the audit log: "exit N", "client disconnected", or the failure the client was told. It also returns what ends the
// connection, to run once the session has been cleaned up: after a last frame it waits for the client to have read it.
func bridge(ctx context.Context, s *deploy.Session, conn net.Conn, in *bufio.Reader) (string, func()) {
	out := session.NewWriter(conn)
	stream := s.Stream()
	var gone atomic.Bool
	// leave is how the client's departure, whether it closed the connection or stopped taking output, ends the session.
	leave := func() {
		gone.Store(true)
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sessionCloseTime)
		defer cancel()
		_ = s.Close(closeCtx)
	}
	// clientDone is closed when the client's frames are no longer read, whether it left or sent what ends the session.
	clientDone := make(chan struct{})
	go func() {
		for {
			kind, payload, err := session.ReadFrame(in)
			if err != nil {
				close(clientDone)
				leave()
				return
			}
			switch kind {
			case session.Stdin:
				// The command may have closed its input; its end is seen on the output side.
				_, _ = stream.Write(payload)
			case session.Resize:
				// A size that is out of range, or a terminal that cannot be resized, leaves the terminal as it is.
				if size, err := session.DecodeSize(payload); err == nil {
					_ = s.Resize(ctx, size)
				}
			case session.CloseStdin:
				_ = stream.CloseWrite()
			default:
				close(clientDone)
				leave()
				return
			}
		}
	}()
	// After the last frame, Exit or Failure, end closes the connection once the client has read it (see session.End). The
	// caller runs it after it has cleaned up, so the cleanup does not wait for the client.
	end := func() { session.End(conn, clientDone) }
	stdout, stderr := watched{out.Output(session.Stdout), &gone, leave}, watched{out.Output(session.Stderr), &gone, leave}
	if s.TTY {
		_, _ = io.Copy(stdout, stream)
	} else {
		_ = engine.Demux(stream, stdout, stderr)
	}
	switch {
	case gone.Load():
		_ = conn.Close()
		return "client disconnected", func() {}
	case s.Closed():
		_ = out.Frame(session.Failure, []byte(errAgentStopping))
		return errAgentStopping, end
	}
	waitCtx, cancel := context.WithTimeout(ctx, sessionWaitTime)
	defer cancel()
	code, err := s.Wait(waitCtx)
	if err != nil {
		message := "the session ended unexpectedly"
		var refusal deploy.Refusal
		if errors.As(err, &refusal) {
			message = string(refusal)
		}
		_ = out.Frame(session.Failure, []byte(message))
		return message, end
	}
	status, _ := json.Marshal(session.ExitStatus{Code: code})
	_ = out.Frame(session.Exit, status)
	return "exit " + strconv.Itoa(code), end
}

// watched is a writer to the client that ends the session when the client cannot take what is written.
type watched struct {
	w     io.Writer
	gone  *atomic.Bool
	leave func()
}

func (w watched) Write(p []byte) (int, error) {
	n, err := w.w.Write(p)
	if err != nil && !w.gone.Load() {
		w.leave()
	}
	return n, err
}
