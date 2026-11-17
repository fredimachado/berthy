package engine

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"sync"
)

// stderrKept is how much of a container's stderr Attach keeps.
const stderrKept = 4 << 10

// BoundedBuffer keeps the first limit bytes written to it and drops the rest. Writes never fail, so a chatty stream is
// never stopped by it. It is safe for concurrent use: the stream's reader writes while the caller reads.
type BoundedBuffer struct {
	mu    sync.Mutex
	limit int
	buf   []byte
}

func NewBoundedBuffer(limit int) *BoundedBuffer { return &BoundedBuffer{limit: limit} }

func (b *BoundedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if room := b.limit - len(b.buf); room > 0 {
		b.buf = append(b.buf, p[:min(room, len(p))]...)
	}
	return len(p), nil
}

func (b *BoundedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}

// Attach attaches to a created container, before it starts, without a TTY. It returns the container's stdout with
// Docker's stream framing removed, a writer for its stdin (nil unless stdin is true; Close ends the container's input but
// not its output) and the first 4 KiB of its stderr, which fills as stdout is read. Reading stdout to its end is what
// collects stderr. Closing stdout, or ending ctx, drops the connection.
//
// The attach is a request that Docker answers by switching the socket to the raw stream, so it uses the socket directly
// and not the HTTP client.
func (e *Engine) Attach(ctx context.Context, id string, stdin bool) (io.ReadCloser, io.WriteCloser, *BoundedBuffer, error) {
	p, err := pathID(id)
	if err != nil {
		return nil, nil, nil, err
	}
	q := url.Values{"stream": {"1"}, "stdout": {"1"}, "stderr": {"1"}}
	if stdin {
		q.Set("stdin", "1")
	}
	conn, br, stop, err := e.upgrade(ctx, "POST", "/containers/"+p+"/attach?"+q.Encode(), nil)
	if err != nil {
		return nil, nil, nil, err
	}
	a := &attachedOutput{ctx: ctx, conn: conn, br: br, stop: stop, stderr: NewBoundedBuffer(stderrKept)}
	var in io.WriteCloser
	if stdin {
		in = attachedInput{conn: conn}
	}
	return a, in, a.stderr, nil
}

// attachedOutput reads the stdout frames of an attached container and sends its stderr frames to a bounded buffer.
type attachedOutput struct {
	ctx    context.Context
	conn   net.Conn
	br     *bufio.Reader
	stop   func() bool
	stderr *BoundedBuffer
	remain int64 // payload bytes left in the current stdout frame
	err    error // the end of the stream, once reached
}

func (a *attachedOutput) Read(p []byte) (int, error) {
	if a.err != nil {
		return 0, a.err
	}
	for a.remain == 0 {
		var hdr [8]byte
		if _, err := io.ReadFull(a.br, hdr[:]); err != nil {
			return 0, a.fail(err, true)
		}
		if (hdr[0] != 1 && hdr[0] != 2) || hdr[1] != 0 || hdr[2] != 0 || hdr[3] != 0 {
			return 0, a.fail(errors.New("invalid Docker output stream"), false)
		}
		size := int64(binary.BigEndian.Uint32(hdr[4:]))
		if hdr[0] == 2 {
			if _, err := io.CopyN(a.stderr, a.br, size); err != nil {
				return 0, a.fail(err, false)
			}
			continue
		}
		a.remain = size
	}
	n, err := a.br.Read(p[:min(int64(len(p)), a.remain)])
	a.remain -= int64(n)
	if err != nil && !(err == io.EOF && a.remain > 0) {
		if n > 0 {
			return n, nil
		}
		return 0, a.fail(err, false)
	}
	return n, nil
}

// fail records the end of the stream. A clean end is only the end of the connection between two frames.
func (a *attachedOutput) fail(err error, betweenFrames bool) error {
	switch {
	case a.ctx.Err() != nil:
		err = a.ctx.Err()
	case err == io.EOF && betweenFrames:
	case err == io.EOF || err == io.ErrUnexpectedEOF:
		err = io.ErrUnexpectedEOF
	case errors.Is(err, net.ErrClosed):
		err = io.ErrClosedPipe
	}
	a.err = err
	return err
}

func (a *attachedOutput) Close() error {
	a.stop()
	return a.conn.Close()
}

// attachedInput is the container's stdin: Close ends the input and leaves the output readable.
type attachedInput struct{ conn net.Conn }

func (i attachedInput) Write(p []byte) (int, error) { return i.conn.Write(p) }

func (i attachedInput) Close() error {
	if c, ok := i.conn.(interface{ CloseWrite() error }); ok {
		return c.CloseWrite()
	}
	return errors.New("connection cannot be half-closed")
}

// CreateVolume creates the named volume. It fails with a 409 StatusError when the volume exists: Docker alone would
// answer success for a local volume that is already there, and a caller that fills a new volume must not fill an old one.
func (e *Engine) CreateVolume(ctx context.Context, name string, labels map[string]string) error {
	exists, err := e.VolumeExists(ctx, name)
	if err != nil {
		return err
	}
	if exists {
		return &StatusError{Code: http.StatusConflict}
	}
	body, err := json.Marshal(struct {
		Name   string
		Labels map[string]string `json:",omitempty"`
	}{name, labels})
	if err != nil {
		return err
	}
	return e.jsonRequest(ctx, "POST", "/volumes/create", body, nil, nil)
}

// RemoveVolume removes the named volume. Docker refuses a volume that a container uses.
func (e *Engine) RemoveVolume(ctx context.Context, name string) error {
	p, err := pathID(name)
	if err != nil {
		return err
	}
	return e.jsonRequest(ctx, "DELETE", "/volumes/"+p, nil, nil, nil)
}
