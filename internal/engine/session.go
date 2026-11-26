package engine

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sync"
)

// Stream is a two-way Docker stream taken over from HTTP. Read returns the output: raw bytes with a terminal, Docker's framed
// stdout and stderr without one (see Demux). Write sends input. CloseWrite ends the input and keeps the output. Close drops the
// connection, and so does the end of the context it was opened with.
type Stream struct {
	ctx       context.Context
	conn      net.Conn
	br        *bufio.Reader
	stop      func() bool
	closeOnce sync.Once
	closeErr  error
}

// NewStream wraps a connection that is already open, such as one a test made, as a Stream.
func NewStream(ctx context.Context, conn net.Conn) *Stream {
	return &Stream{ctx: ctx, conn: conn, br: bufio.NewReader(conn), stop: func() bool { return false }}
}

// Read returns the output. Ending the context makes it fail with the context's error.
func (s *Stream) Read(p []byte) (int, error) {
	n, err := s.br.Read(p)
	return n, s.ended(err)
}

// Write sends input. Ending the context makes it fail with the context's error.
func (s *Stream) Write(p []byte) (int, error) {
	n, err := s.conn.Write(p)
	return n, s.ended(err)
}

// ended reports the end of the context in place of the "closed connection" it caused.
func (s *Stream) ended(err error) error {
	if err != nil && s.ctx.Err() != nil {
		return s.ctx.Err()
	}
	return err
}

// CloseWrite ends the input and leaves the output readable.
func (s *Stream) CloseWrite() error {
	if c, ok := s.conn.(interface{ CloseWrite() error }); ok {
		return c.CloseWrite()
	}
	return errors.New("connection cannot be half-closed")
}

// Close drops the connection. It may be called more than once, and after the context has ended.
func (s *Stream) Close() error {
	s.closeOnce.Do(func() {
		s.stop()
		if err := s.conn.Close(); !errors.Is(err, net.ErrClosed) {
			s.closeErr = err
		}
	})
	return s.closeErr
}

// upgrade sends a request with Connection: Upgrade and Upgrade: tcp on a new connection to the Docker socket and returns the
// connection once Docker answers 101, with the reader that holds whatever followed the answer, and the function that ends the
// watch on ctx (call it before closing the connection yourself). path is the part after the API version, with its query. A
// non-nil body is JSON. Docker switches the socket to a raw stream, so this uses the socket directly and not the HTTP client.
//
// Ending ctx drops the connection, which unblocks the handshake and every later read and write. Every error closes it.
func (e *Engine) upgrade(ctx context.Context, method, path string, body []byte) (net.Conn, *bufio.Reader, func() bool, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://docker/v1.44"+path, rd)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("invalid Docker request")
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "tcp")
	conn, err := e.dial(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return nil, nil, nil, ctx.Err()
		}
		return nil, nil, nil, fmt.Errorf("Docker unavailable")
	}
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	fail := func(err error) (net.Conn, *bufio.Reader, func() bool, error) {
		stop()
		conn.Close()
		if ctx.Err() != nil {
			return nil, nil, nil, ctx.Err()
		}
		return nil, nil, nil, err
	}
	if err = req.Write(conn); err != nil {
		return fail(fmt.Errorf("Docker unavailable"))
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		return fail(fmt.Errorf("Docker unavailable"))
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return fail(fmt.Errorf("invalid Docker attach response"))
		}
		return fail(&StatusError{Code: resp.StatusCode})
	}
	return conn, br, stop, nil
}

// stream opens a Stream on an upgraded request.
func (e *Engine) stream(ctx context.Context, method, path string, body []byte) (*Stream, error) {
	conn, br, stop, err := e.upgrade(ctx, method, path, body)
	if err != nil {
		return nil, err
	}
	return &Stream{ctx: ctx, conn: conn, br: br, stop: stop}, nil
}

// StartExecSession creates an exec in the container with its stdin attached, and a terminal when tty is set, starts it on a
// connection taken over from HTTP, and returns the stream and the exec's id.
func (e *Engine) StartExecSession(ctx context.Context, containerID string, command []string, tty bool) (*Stream, string, error) {
	execID, err := e.createExec(ctx, containerID, command, true, tty)
	if err != nil {
		return nil, "", err
	}
	s, err := e.stream(ctx, "POST", "/exec/"+execID+"/start", []byte(`{"Detach":false,"Tty":`+strconv.FormatBool(tty)+`}`))
	if err != nil {
		return nil, "", err
	}
	return s, execID, nil
}

// AttachSession attaches to a created container's stdin, stdout and stderr before it starts. The container must have been
// created with OpenStdin and StdinOnce, and Tty when a terminal is wanted.
func (e *Engine) AttachSession(ctx context.Context, containerID string) (*Stream, error) {
	p, err := pathID(containerID)
	if err != nil {
		return nil, err
	}
	q := url.Values{"stream": {"1"}, "stdin": {"1"}, "stdout": {"1"}, "stderr": {"1"}}
	return e.stream(ctx, "POST", "/containers/"+p+"/attach?"+q.Encode(), nil)
}

// ResizeExec sets the terminal size of an exec that has one.
func (e *Engine) ResizeExec(ctx context.Context, execID string, rows, cols uint16) error {
	p, err := pathID(execID)
	if err != nil {
		return err
	}
	return e.jsonRequest(ctx, "POST", "/exec/"+p+"/resize?"+resizeQuery(rows, cols), nil, nil, nil)
}

// ResizeContainer sets the terminal size of a container that has one.
func (e *Engine) ResizeContainer(ctx context.Context, containerID string, rows, cols uint16) error {
	p, err := pathID(containerID)
	if err != nil {
		return err
	}
	return e.jsonRequest(ctx, "POST", "/containers/"+p+"/resize?"+resizeQuery(rows, cols), nil, nil, nil)
}

func resizeQuery(rows, cols uint16) string {
	return url.Values{"h": {strconv.Itoa(int(rows))}, "w": {strconv.Itoa(int(cols))}}.Encode()
}

// Demux copies Docker's framed stream to stdout and stderr until the stream ends. It shares the frame parsing with
// dockerStream. A malformed stream is an error, and so is a failing writer, with the writer's own error.
func Demux(r io.Reader, stdout, stderr io.Writer) error { return demux(r, stdout, stderr, -1) }
