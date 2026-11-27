// Package session is the wire format of an interactive session between the CLI and the agent. The CLI sends a JSON Request
// in an HTTP request that asks to switch to Upgrade; once the agent answers 101 Switching Protocols, both sides exchange
// frames on the connection until the agent sends Exit or Failure.
package session

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"
)

// Upgrade is the protocol named in the Upgrade header of a session request.
const Upgrade = "berthy-session/1"

// Kind is the type of a frame.
type Kind byte

const (
	Stdin      Kind = 1 // client to agent: bytes for the command's standard input
	Resize     Kind = 2 // client to agent: the terminal's new size, a Size
	CloseStdin Kind = 3 // client to agent: the input has ended
	Stdout     Kind = 4 // agent to client: output; with a terminal, all of the output
	Stderr     Kind = 5 // agent to client: error output, only without a terminal
	Exit       Kind = 6 // agent to client: the command ended, an ExitStatus; the last frame
	Failure    Kind = 7 // agent to client: the session could not start or broke, a short public message; the last frame
)

func (k Kind) valid() bool { return k >= Stdin && k <= Failure }

// MaxPayload is the largest payload of one frame.
const MaxPayload = 64 << 10

// headerSize is the kind and the 4-byte payload length that start a frame.
const headerSize = 5

// WriteFrame writes one frame: the kind, the payload's length as 4 bytes big-endian, and the payload. It writes the frame
// with one call to w.Write, or none when the frame is refused.
func WriteFrame(w io.Writer, k Kind, payload []byte) error {
	if !k.valid() {
		return fmt.Errorf("unknown session frame kind %d", k)
	}
	if len(payload) > MaxPayload {
		return fmt.Errorf("session frame payload of %d bytes is over the limit", len(payload))
	}
	buf := make([]byte, headerSize+len(payload))
	buf[0] = byte(k)
	binary.BigEndian.PutUint32(buf[1:], uint32(len(payload)))
	copy(buf[headerSize:], payload)
	_, err := w.Write(buf)
	return err
}

// ReadFrame reads one frame. An unknown kind or a payload over MaxPayload is an error. A stream that ends between frames
// is io.EOF; one that ends inside a frame is io.ErrUnexpectedEOF.
func ReadFrame(r io.Reader) (Kind, []byte, error) {
	var hdr [headerSize]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	k := Kind(hdr[0])
	if !k.valid() {
		return 0, nil, fmt.Errorf("unknown session frame kind %d", k)
	}
	n := binary.BigEndian.Uint32(hdr[1:])
	if n > MaxPayload {
		return 0, nil, fmt.Errorf("session frame payload of %d bytes is over the limit", n)
	}
	payload, err := io.ReadAll(io.LimitReader(r, int64(n)))
	if err != nil {
		return 0, nil, err
	}
	if len(payload) != int(n) {
		return 0, nil, io.ErrUnexpectedEOF
	}
	return k, payload, nil
}

// Writer writes frames from several goroutines, one frame at a time.
type Writer struct {
	mu sync.Mutex
	w  io.Writer
}

func NewWriter(w io.Writer) *Writer { return &Writer{w: w} }

// Frame writes one frame.
func (w *Writer) Frame(k Kind, payload []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return WriteFrame(w.w, k, payload)
}

// Output returns an io.Writer that sends what is written to it as frames of kind k, split at MaxPayload.
func (w *Writer) Output(k Kind) io.Writer { return output{w, k} }

type output struct {
	w *Writer
	k Kind
}

func (o output) Write(p []byte) (int, error) {
	sent := 0
	for len(p) > 0 {
		n := min(len(p), MaxPayload)
		if err := o.w.Frame(o.k, p[:n]); err != nil {
			return sent, err
		}
		sent += n
		p = p[n:]
	}
	return sent, nil
}

// Size is a terminal size.
type Size struct{ Rows, Cols uint16 }

// maxSide is the largest number of rows or columns a terminal may have.
const maxSide = 1000

func (s Size) check() error {
	if s.Rows < 1 || s.Rows > maxSide || s.Cols < 1 || s.Cols > maxSide {
		return fmt.Errorf("terminal size %dx%d is out of range", s.Rows, s.Cols)
	}
	return nil
}

// Encode is the payload of a Resize frame: rows then columns, 2 bytes each, big-endian.
func (s Size) Encode() []byte {
	return binary.BigEndian.AppendUint16(binary.BigEndian.AppendUint16(nil, s.Rows), s.Cols)
}

// DecodeSize parses the payload of a Resize frame. A size with no rows or no columns, or over 1000 of either, is refused.
func DecodeSize(b []byte) (Size, error) {
	if len(b) != 4 {
		return Size{}, errors.New("invalid terminal size")
	}
	s := Size{binary.BigEndian.Uint16(b), binary.BigEndian.Uint16(b[2:])}
	return s, s.check()
}

// ExitStatus is the payload of an Exit frame, as JSON {"code": N}.
type ExitStatus struct {
	Code int `json:"code"`
}

// Request is the JSON body of a session request.
type Request struct {
	Kind    string   `json:"kind"` // "exec" or "run"
	App     string   `json:"app"`
	Role    string   `json:"role,omitempty"`
	Service string   `json:"service,omitempty"` // a Compose app's service, for exec
	Command []string `json:"command"`
	TTY     bool     `json:"tty"`
	Size    *Size    `json:"size,omitempty"`    // the terminal's size when TTY is set
	Timeout string   `json:"timeout,omitempty"` // run only
}

// Check validates a request where it enters: kind exec or run, an app, a command with a program and no NUL byte, a size only
// with TTY, rows and columns from 1 to 1000, role and service not both, service only for exec, timeout only for run.
func (r Request) Check() error {
	if r.Kind != "exec" && r.Kind != "run" {
		return errors.New("kind must be exec or run")
	}
	if r.App == "" {
		return errors.New("app is required")
	}
	if len(r.Command) == 0 || r.Command[0] == "" {
		return errors.New("command is required")
	}
	for _, a := range r.Command {
		if strings.ContainsRune(a, 0) {
			return errors.New("command must not contain a NUL byte")
		}
	}
	if r.Size != nil {
		if !r.TTY {
			return errors.New("size needs tty")
		}
		if err := r.Size.check(); err != nil {
			return err
		}
	}
	if r.Role != "" && r.Service != "" {
		return errors.New("role and service are exclusive")
	}
	if r.Service != "" && r.Kind != "exec" {
		return errors.New("service is only for exec")
	}
	if r.Timeout != "" && r.Kind != "run" {
		return errors.New("timeout is only for run")
	}
	return nil
}

// endTime bounds how long End waits for the peer to close. It is a variable so a test can shorten it.
var endTime = 5 * time.Second

// End closes the connection of the side that wrote the last frame, Exit or Failure. Closing a socket that still holds
// unread data resets the connection, and the peer, which may still be sending input, can then lose the last frame before it
// reads it. So End half-closes the connection, keeps reading what the peer sends until it closes (the peer does as soon as
// it has read the last frame) or endTime has passed, and only then closes. peerDone is closed when the goroutine that reads
// the peer's frames from conn has stopped; End does not read conn itself.
func End(conn net.Conn, peerDone <-chan struct{}) {
	defer conn.Close()
	if c, ok := conn.(interface{ CloseWrite() error }); ok {
		_ = c.CloseWrite()
	}
	_ = conn.SetReadDeadline(time.Now().Add(endTime))
	timer := time.NewTimer(endTime)
	defer timer.Stop()
	select {
	case <-peerDone:
	case <-timer.C:
	}
}
