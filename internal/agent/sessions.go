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
	"berthy/internal/state"
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
func serveSession(w http.ResponseWriter, r *http.Request, d *deploy.Controller, gate *approvalGate, actor deploy.Actor) {
	ctx := r.Context()
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
	var req session.Request
	r.Body = http.MaxBytesReader(w, r.Body, maxSessionRequest)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil || dec.Decode(&struct{}{}) != io.EOF {
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
	sa := state.Actor{ID: actor.ID, Name: actor.Name, Remote: actor.Remote}
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
	outcome := bridge(ctx, s, conn, brw.Reader)
	_ = conn.Close()
	err = closeSession()
	if err != nil {
		outcome += " (cleanup failed)"
	}
	// The session is over and the client has been told; a failure to record its end has nobody left to be reported to.
	_ = d.Store.RecordAudit(context.WithoutCancel(ctx), sa, action, "ended: "+outcome)
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
// it ended, for the audit log: "exit N", "client disconnected", or the failure the client was told.
func bridge(ctx context.Context, s *deploy.Session, conn net.Conn, in *bufio.Reader) string {
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
	go func() {
		for {
			kind, payload, err := session.ReadFrame(in)
			if err != nil {
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
				leave()
				return
			}
		}
	}()
	stdout, stderr := watched{out.Output(session.Stdout), &gone, leave}, watched{out.Output(session.Stderr), &gone, leave}
	if s.TTY {
		_, _ = io.Copy(stdout, stream)
	} else {
		_ = engine.Demux(stream, stdout, stderr)
	}
	switch {
	case gone.Load():
		return "client disconnected"
	case s.Closed():
		_ = out.Frame(session.Failure, []byte(errAgentStopping))
		return errAgentStopping
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
		return message
	}
	status, _ := json.Marshal(session.ExitStatus{Code: code})
	_ = out.Frame(session.Exit, status)
	return "exit " + strconv.Itoa(code)
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
