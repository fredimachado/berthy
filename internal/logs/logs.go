// Package logs reads bounded Docker container log streams.
package logs

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"time"
)

const TruncationMarker = " …[truncated]"

const (
	MaxSources    = 64
	MaxTail       = 1000
	MaxFrameBytes = 1 << 20
	MaxLineBytes  = 256 << 10
	MaxTailBytes  = 8 << 20
)

type Identity struct {
	Role        string `json:"role"`
	Replica     string `json:"replica"`
	ContainerID string `json:"container_id"`
}
type Record struct {
	Identity
	Timestamp time.Time `json:"timestamp"`
	Stream    string    `json:"stream"`
	Message   string    `json:"message"`
	Truncated bool      `json:"-"`
}
type OpenOptions struct {
	Tail   int
	Since  time.Time
	Follow bool
}
type Options struct {
	Tail  int
	Since time.Time
	Grep  *regexp.Regexp
}
type Source interface {
	Open(context.Context, string, OpenOptions) (io.ReadCloser, bool, error)
}

func validate(ids []Identity, opts Options) error {
	if len(ids) > MaxSources {
		return fmt.Errorf("too many log sources (max %d)", MaxSources)
	}
	if opts.Tail < 0 || opts.Tail > MaxTail {
		return fmt.Errorf("tail must be between 0 and %d", MaxTail)
	}
	for _, id := range ids {
		if id.ContainerID == "" {
			return errors.New("empty container ID")
		}
	}
	return nil
}

// Tail reads each source's requested tail and returns a bounded chronological merge.
func Tail(ctx context.Context, source Source, ids []Identity, opts Options) ([]Record, error) {
	var out []Record
	size := 2
	err := readAll(ctx, source, ids, opts, false, func(r Record) error {
		encoded, err := json.Marshal(r)
		if err != nil {
			return err
		}
		size += len(encoded) + 1
		if size > MaxTailBytes {
			return errors.New("log tail exceeds response limit")
		}
		out = append(out, r)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Timestamp.Before(out[j].Timestamp) })
	return out, nil
}

// Follow delivers records as they arrive. The callback is called serially.
func Follow(ctx context.Context, source Source, ids []Identity, opts Options, emit func(Record) error) error {
	return readAll(ctx, source, ids, opts, true, emit)
}

type result struct {
	record Record
	err    error
	done   bool
}

func readAll(ctx context.Context, source Source, ids []Identity, opts Options, follow bool, emit func(Record) error) error {
	if err := validate(ids, opts); err != nil {
		return err
	}
	if source == nil || emit == nil {
		return errors.New("missing log source or callback")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	ch := make(chan result)
	for _, id := range ids {
		go func(id Identity) {
			send := func(v result) bool {
				select {
				case ch <- v:
					return true
				case <-ctx.Done():
					return false
				}
			}
			stream, tty, err := source.Open(ctx, id.ContainerID, OpenOptions{Tail: opts.Tail, Since: opts.Since, Follow: follow})
			if err != nil {
				send(result{err: fmt.Errorf("logs for %s: %w", id.ContainerID, err), done: true})
				return
			}
			if stream == nil {
				send(result{err: fmt.Errorf("logs for %s: empty stream", id.ContainerID), done: true})
				return
			}
			closed := make(chan struct{})
			go func() {
				select {
				case <-ctx.Done():
					stream.Close() //nolint:errcheck // Cancellation closes the stream only to unblock the reader.
				case <-closed:
				}
			}()
			err = decode(stream, tty, id, opts, func(r Record) error {
				if !send(result{record: r}) {
					return ctx.Err()
				}
				return nil
			})
			close(closed)
			stream.Close() //nolint:errcheck // The read result already determines whether log collection succeeded.
			if err != nil {
				err = fmt.Errorf("logs for %s: %w", id.ContainerID, err)
			}
			send(result{err: err, done: true})
		}(id)
	}
	for remaining := len(ids); remaining > 0; {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case v := <-ch:
			if v.done {
				remaining--
				if v.err != nil {
					return v.err
				}
			} else if err := emit(v.record); err != nil {
				return err
			}
		}
	}
	return nil
}

type pendingLine struct {
	prefix    []byte
	truncated bool
}

func decode(r io.Reader, tty bool, id Identity, opts Options, emit func(Record) error) error {
	pending := map[byte]*pendingLine{1: {}, 2: {}}
	consume := func(stream byte, data []byte) error {
		p := pending[stream]
		for len(data) > 0 {
			i := bytes.IndexByte(data, '\n')
			if i < 0 {
				i = len(data)
			}
			keep := min(i, MaxLineBytes-len(p.prefix))
			p.prefix = append(p.prefix, data[:keep]...)
			p.truncated = p.truncated || keep < i
			if i == len(data) {
				break
			}
			line := string(p.prefix)
			truncated := p.truncated
			*p = pendingLine{}
			data = data[i+1:]
			at := strings.IndexByte(line, ' ')
			if at < 0 {
				return errors.New("log line missing timestamp")
			}
			ts, err := time.Parse(time.RFC3339Nano, line[:at])
			if err != nil {
				return fmt.Errorf("invalid log timestamp: %w", err)
			}
			msg := line[at+1:]
			if truncated {
				msg += TruncationMarker
			}
			if (!opts.Since.IsZero() && ts.Before(opts.Since)) || (opts.Grep != nil && !opts.Grep.MatchString(msg)) {
				continue
			}
			name := "stdout"
			if stream == 2 {
				name = "stderr"
			}
			if err := emit(Record{Identity: id, Timestamp: ts, Stream: name, Message: msg, Truncated: truncated}); err != nil {
				return err
			}
		}
		return nil
	}
	br := bufio.NewReader(r)
	if tty {
		buf := make([]byte, 32<<10)
		for {
			n, err := br.Read(buf)
			if n > 0 {
				if e := consume(1, buf[:n]); e != nil {
					return e
				}
			}
			if err != nil {
				if err != io.EOF {
					return err
				}
				break
			}
		}
	} else {
		h := make([]byte, 8)
		for {
			_, err := io.ReadFull(br, h)
			if err == io.EOF {
				break
			}
			if err != nil {
				return fmt.Errorf("truncated log frame header: %w", err)
			}
			if (h[0] != 1 && h[0] != 2) || h[1] != 0 || h[2] != 0 || h[3] != 0 {
				return errors.New("invalid log frame header")
			}
			n := binary.BigEndian.Uint32(h[4:])
			if n > MaxFrameBytes {
				return errors.New("log frame exceeds limit")
			}
			data, err := io.ReadAll(io.LimitReader(br, int64(n)))
			if err != nil {
				return fmt.Errorf("truncated log frame: %w", err)
			}
			if len(data) != int(n) {
				return fmt.Errorf("truncated log frame: %w", io.ErrUnexpectedEOF)
			}
			p := pending[h[0]]
			if at := bytes.IndexByte(p.prefix, ' '); at >= 0 && bytes.HasPrefix(data, p.prefix[:at+1]) {
				data = data[at+1:]
			}
			if err = consume(h[0], data); err != nil {
				return err
			}
		}
	}
	for _, stream := range []byte{1, 2} {
		if len(pending[stream].prefix) > 0 {
			if err := consume(stream, []byte{'\n'}); err != nil {
				return err
			}
		}
	}
	return nil
}
