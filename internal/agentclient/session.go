package agentclient

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"time"

	"berthy/internal/session"
)

// OpenSession asks the agent of t for an interactive session and returns the connection once the agent switches protocols,
// with the reader that holds whatever followed the answer: read the frames from it, and write them to the connection. The
// caller closes the connection. A refusal is returned as an error carrying Message(status, body).
//
// The agent takes the connection over, so the session dials it directly: an http.Client would time the whole exchange out.
// Ending ctx before the answer drops the connection.
func OpenSession(ctx context.Context, t *Target, req session.Request) (net.Conn, *bufio.Reader, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, nil, err
	}
	u := *t.Base
	u.Path = t.Prefix + "sessions"
	httpReq, err := http.NewRequestWithContext(ctx, "POST", u.String(), bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}
	httpReq.Header.Set("Authorization", "Bearer "+t.Token)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Connection", "Upgrade")
	httpReq.Header.Set("Upgrade", session.Upgrade)
	address := u.Host
	if u.Port() == "" {
		address = net.JoinHostPort(u.Hostname(), "80")
	}
	dialer := net.Dialer{Timeout: 5 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, nil, err
	}
	// Ending ctx unblocks the handshake; once it is done the connection is the caller's.
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	fail := func(err error) (net.Conn, *bufio.Reader, error) {
		stop()
		conn.Close()
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		return nil, nil, err
	}
	if err = httpReq.Write(conn); err != nil {
		return fail(err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, httpReq)
	if err != nil {
		return fail(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		if resp.StatusCode == http.StatusNotFound {
			resp = olderAgent(ctx, NewHTTPClient(), httpReq.URL, t.Token, resp)
		}
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return fail(errors.New(Message(resp.StatusCode, data)))
	}
	if !stop() {
		// The context ended while the answer was read, and the connection is closed.
		conn.Close()
		return nil, nil, ctx.Err()
	}
	return conn, br, nil
}
