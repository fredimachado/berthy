package session

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"runtime"
	"strings"
	"sync"
	"testing"
	"testing/iotest"
)

func TestFramesRoundTripForEveryKind(t *testing.T) {
	for _, k := range []Kind{Stdin, Resize, CloseStdin, Stdout, Stderr, Exit, Failure} {
		for _, payload := range []string{"", "x", "hello\x00world"} {
			var b bytes.Buffer
			if err := WriteFrame(&b, k, []byte(payload)); err != nil {
				t.Fatalf("kind %d: write: %v", k, err)
			}
			gotK, got, err := ReadFrame(&b)
			if err != nil || gotK != k || string(got) != payload {
				t.Errorf("kind %d payload %q: got kind %d payload %q err %v", k, payload, gotK, got, err)
			}
			if b.Len() != 0 {
				t.Errorf("kind %d: %d bytes left over", k, b.Len())
			}
		}
	}
}

func TestFrameLayoutIsKindLengthPayload(t *testing.T) {
	var b bytes.Buffer
	if err := WriteFrame(&b, Stdout, []byte("abc")); err != nil {
		t.Fatal(err)
	}
	if got := b.String(); got != "\x04\x00\x00\x00\x03abc" {
		t.Fatalf("frame %q", got)
	}
}

func TestPayloadLimit(t *testing.T) {
	full := bytes.Repeat([]byte("a"), MaxPayload)
	var b bytes.Buffer
	if err := WriteFrame(&b, Stdin, full); err != nil {
		t.Fatalf("a payload of MaxPayload: %v", err)
	}
	if _, got, err := ReadFrame(&b); err != nil || len(got) != MaxPayload {
		t.Fatalf("read back %d bytes: %v", len(got), err)
	}
	b.Reset()
	if err := WriteFrame(&b, Stdin, append(full, 'a')); err == nil {
		t.Fatal("write accepted MaxPayload+1")
	}
	if b.Len() != 0 {
		t.Fatal("a refused frame was partly written")
	}
	// A header that announces MaxPayload+1 is refused before any payload is read.
	if _, _, err := ReadFrame(bytes.NewReader([]byte{byte(Stdin), 0, 1, 0, 1})); err == nil {
		t.Fatal("read accepted MaxPayload+1")
	}
}

func TestUnknownKindIsRefused(t *testing.T) {
	for _, k := range []Kind{0, 8, 255} {
		if err := WriteFrame(io.Discard, k, nil); err == nil {
			t.Errorf("write accepted kind %d", k)
		}
		if _, _, err := ReadFrame(bytes.NewReader([]byte{byte(k), 0, 0, 0, 0})); err == nil {
			t.Errorf("read accepted kind %d", k)
		}
	}
}

func TestReadFrameFromAReaderThatReturnsOneByteAtATime(t *testing.T) {
	var b bytes.Buffer
	WriteFrame(&b, Stdout, []byte("hello"))
	WriteFrame(&b, Exit, []byte(`{"code":0}`))
	r := iotest.OneByteReader(&b)
	if k, p, err := ReadFrame(r); err != nil || k != Stdout || string(p) != "hello" {
		t.Fatalf("first: %d %q %v", k, p, err)
	}
	if k, p, err := ReadFrame(r); err != nil || k != Exit || string(p) != `{"code":0}` {
		t.Fatalf("second: %d %q %v", k, p, err)
	}
	if _, _, err := ReadFrame(r); err != io.EOF {
		t.Fatalf("end of stream: %v", err)
	}
}

func TestReadFrameTellsAnEndFromACutFrame(t *testing.T) {
	if _, _, err := ReadFrame(bytes.NewReader(nil)); err != io.EOF {
		t.Errorf("empty: %v", err)
	}
	for name, cut := range map[string][]byte{"header": {byte(Stdout), 0, 0}, "payload": {byte(Stdout), 0, 0, 0, 5, 'a'}} {
		if _, _, err := ReadFrame(bytes.NewReader(cut)); !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Errorf("cut %s: %v", name, err)
		}
	}
}

// splitWriter passes each Write on in two halves, with a yield between them: a Writer that does not hold its lock across
// the whole frame would interleave two frames here.
type splitWriter struct{ w io.Writer }

func (s *splitWriter) Write(p []byte) (int, error) {
	h := len(p) / 2
	n, err := s.w.Write(p[:h])
	if err != nil {
		return n, err
	}
	runtime.Gosched()
	m, err := s.w.Write(p[h:])
	return n + m, err
}

func TestWriterNeverInterleavesFrames(t *testing.T) {
	var b bytes.Buffer
	w := NewWriter(&splitWriter{w: &b})
	const each = 200
	payload := func(k Kind) string { return strings.Repeat(string(rune('a'+int(k))), 1000) }
	var wg sync.WaitGroup
	for _, k := range []Kind{Stdout, Stderr} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < each; i++ {
				if err := w.Frame(k, []byte(payload(k))); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	wg.Wait()
	counts := map[Kind]int{}
	for {
		k, p, err := ReadFrame(&b)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if string(p) != payload(k) {
			t.Fatalf("a frame of kind %d has another frame's payload", k)
		}
		counts[k]++
	}
	if counts[Stdout] != each || counts[Stderr] != each {
		t.Fatalf("frames %v", counts)
	}
}

func TestOutputSplitsALargeWriteIntoFrames(t *testing.T) {
	var b bytes.Buffer
	out := NewWriter(&b).Output(Stdout)
	data := bytes.Repeat([]byte("z"), 2*MaxPayload+10)
	if n, err := out.Write(data); n != len(data) || err != nil {
		t.Fatalf("write: %d %v", n, err)
	}
	var sizes []int
	var got []byte
	for b.Len() > 0 {
		k, p, err := ReadFrame(&b)
		if err != nil || k != Stdout {
			t.Fatalf("frame: kind %d %v", k, err)
		}
		sizes = append(sizes, len(p))
		got = append(got, p...)
	}
	if len(sizes) != 3 || sizes[0] != MaxPayload || sizes[1] != MaxPayload || sizes[2] != 10 || !bytes.Equal(got, data) {
		t.Fatalf("frame sizes %v", sizes)
	}
	if n, err := out.Write(nil); n != 0 || err != nil || b.Len() != 0 {
		t.Fatalf("an empty write sent a frame: %d %v %d", n, err, b.Len())
	}
}

// failAfter accepts left writes and fails the next.
type failAfter struct{ left int }

func (f *failAfter) Write(p []byte) (int, error) {
	if f.left == 0 {
		return 0, io.ErrClosedPipe
	}
	f.left--
	return len(p), nil
}

func TestOutputReportsTheBytesSentBeforeAFailure(t *testing.T) {
	out := NewWriter(&failAfter{left: 1}).Output(Stderr)
	n, err := out.Write(make([]byte, 2*MaxPayload))
	if err == nil || n != MaxPayload {
		t.Fatalf("write: %d %v", n, err)
	}
}

func TestSizeEncodesRowsThenColumns(t *testing.T) {
	b := Size{Rows: 24, Cols: 300}.Encode()
	if !bytes.Equal(b, []byte{0, 24, 1, 44}) {
		t.Fatalf("encoded %v", b)
	}
	s, err := DecodeSize(b)
	if err != nil || s != (Size{24, 300}) {
		t.Fatalf("decoded %v %v", s, err)
	}
	for name, bad := range map[string][]byte{"short": {0, 24, 0}, "long": {0, 24, 0, 80, 0}, "zero rows": {0, 0, 0, 80}, "zero columns": {0, 24, 0, 0}, "too many rows": {3, 233, 0, 80}} {
		if _, err := DecodeSize(bad); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestRequestCheck(t *testing.T) {
	good := func() Request {
		return Request{Kind: "exec", App: "shop", Command: []string{"sh", "-c", "echo hi"}}
	}
	size := func(r, c uint16) *Size { return &Size{r, c} }
	for name, tc := range map[string]struct {
		edit func(*Request)
		ok   bool
	}{
		"good exec":                   {func(r *Request) {}, true},
		"good exec with role":         {func(r *Request) { r.Role = "web" }, true},
		"good exec with service":      {func(r *Request) { r.Service = "db" }, true},
		"good exec with tty and size": {func(r *Request) { r.TTY = true; r.Size = size(24, 80) }, true},
		"good exec with tty no size":  {func(r *Request) { r.TTY = true }, true},
		"good run":                    {func(r *Request) { r.Kind = "run"; r.Role = "web"; r.Timeout = "5m" }, true},
		"largest size":                {func(r *Request) { r.TTY = true; r.Size = size(1000, 1000) }, true},
		"smallest size":               {func(r *Request) { r.TTY = true; r.Size = size(1, 1) }, true},
		"unknown kind":                {func(r *Request) { r.Kind = "attach" }, false},
		"no kind":                     {func(r *Request) { r.Kind = "" }, false},
		"no app":                      {func(r *Request) { r.App = "" }, false},
		"no command":                  {func(r *Request) { r.Command = nil }, false},
		"empty command":               {func(r *Request) { r.Command = []string{} }, false},
		"empty program":               {func(r *Request) { r.Command = []string{""} }, false},
		"NUL in the program":          {func(r *Request) { r.Command = []string{"s\x00h"} }, false},
		"NUL in an argument":          {func(r *Request) { r.Command = []string{"sh", "a\x00b"} }, false},
		"size without tty":            {func(r *Request) { r.Size = size(24, 80) }, false},
		"role and service":            {func(r *Request) { r.Role = "web"; r.Service = "db" }, false},
		"service with run":            {func(r *Request) { r.Kind = "run"; r.Service = "db" }, false},
		"timeout with exec":           {func(r *Request) { r.Timeout = "5m" }, false},
		"zero rows":                   {func(r *Request) { r.TTY = true; r.Size = size(0, 80) }, false},
		"zero columns":                {func(r *Request) { r.TTY = true; r.Size = size(24, 0) }, false},
		"1001 rows":                   {func(r *Request) { r.TTY = true; r.Size = size(1001, 80) }, false},
		"1001 columns":                {func(r *Request) { r.TTY = true; r.Size = size(24, 1001) }, false},
	} {
		r := good()
		tc.edit(&r)
		if err := r.Check(); (err == nil) != tc.ok {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func FuzzSessionFrame(f *testing.F) {
	var all bytes.Buffer
	for k := Stdin; k <= Failure; k++ {
		var b bytes.Buffer
		if err := WriteFrame(&b, k, []byte{0, 255, 'x'}); err != nil {
			f.Fatal(err)
		}
		f.Add(b.Bytes())
		if err := WriteFrame(&all, k, nil); err != nil {
			f.Fatal(err)
		}
	}
	f.Add(all.Bytes())
	var full bytes.Buffer
	if err := WriteFrame(&full, Stdin, bytes.Repeat([]byte{'a'}, MaxPayload)); err != nil {
		f.Fatal(err)
	}
	f.Add(full.Bytes())
	for _, b := range [][]byte{nil, {4}, {4, 0, 0, 0}, {4, 0, 0, 0, 2, 'a'}, {0, 0, 0, 0, 0}, {8, 0, 0, 0, 0}, {1, 0, 1, 0, 1}} {
		f.Add(b)
	}
	f.Fuzz(func(t *testing.T, wire []byte) {
		for _, fragmented := range []bool{false, true} {
			base := bytes.NewReader(wire)
			var r io.Reader = base
			if fragmented {
				r = iotest.OneByteReader(base)
			}
			for {
				remaining := wire[len(wire)-base.Len():]
				before := base.Len()
				k, payload, err := ReadFrame(r)
				if len(remaining) == 0 {
					if err != io.EOF {
						t.Fatalf("end: %v", err)
					}
					break
				}
				if len(remaining) < headerSize {
					if !errors.Is(err, io.ErrUnexpectedEOF) {
						t.Fatalf("header: %v", err)
					}
					break
				}
				n := binary.BigEndian.Uint32(remaining[1:5])
				if remaining[0] < byte(Stdin) || remaining[0] > byte(Failure) || n > MaxPayload {
					if err == nil {
						t.Fatal("invalid header accepted")
					}
					break
				}
				if uint64(n) > uint64(len(remaining)-headerSize) {
					if !errors.Is(err, io.ErrUnexpectedEOF) {
						t.Fatalf("payload: %v", err)
					}
					break
				}
				size := headerSize + int(n)
				if err != nil || k != Kind(remaining[0]) || !bytes.Equal(payload, remaining[headerSize:size]) || before-base.Len() != size {
					t.Fatalf("frame mismatch: kind %d error %v", k, err)
				}
				var out bytes.Buffer
				if err := WriteFrame(&out, k, payload); err != nil || !bytes.Equal(out.Bytes(), remaining[:size]) {
					t.Fatalf("round trip: %v", err)
				}
			}
		}
	})
}

func TestReadFrameAllocationTracksInput(t *testing.T) {
	for _, size := range []int{0, 64, 4096, MaxPayload} {
		var full bytes.Buffer
		if err := WriteFrame(&full, Stdin, bytes.Repeat([]byte{'x'}, size)); err != nil {
			t.Fatal(err)
		}
		for _, wire := range [][]byte{full.Bytes(), full.Bytes()[:headerSize]} {
			r := bytes.NewReader(wire)
			runtime.GC()
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			_, _, _ = ReadFrame(r)
			runtime.ReadMemStats(&after)
			allocated := after.TotalAlloc - before.TotalAlloc
			limit := uint64(8192 + 8*len(wire))
			if allocated > limit {
				t.Errorf("input %d declared %d: allocated %d, limit %d", len(wire), size, allocated, limit)
			}
		}
	}
}
