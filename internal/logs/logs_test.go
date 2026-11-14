package logs

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"regexp"
	"strings"
	"testing"
	"time"
)

type fakeSource struct {
	data map[string]string
	tty  bool
}

func (f fakeSource) Open(_ context.Context, id string, _ OpenOptions) (io.ReadCloser, bool, error) {
	return io.NopCloser(strings.NewReader(f.data[id])), f.tty, nil
}
func frame(stream byte, s string) string {
	var b bytes.Buffer
	h := make([]byte, 8)
	h[0] = stream
	binary.BigEndian.PutUint32(h[4:], uint32(len(s)))
	b.Write(h)
	b.WriteString(s)
	return b.String()
}
func TestTailMultiplexAndFilter(t *testing.T) {
	f := fakeSource{data: map[string]string{"a": frame(1, "2026-01-01T00:00:02Z keep\n") + frame(2, "2026-01-01T00:00:03Z skip\n"), "b": frame(2, "2026-01-01T00:00:01Z keep\n")}}
	got, err := Tail(context.Background(), f, []Identity{{ContainerID: "a"}, {ContainerID: "b"}}, Options{Tail: 10, Grep: regexp.MustCompile("keep")})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ContainerID != "b" || got[0].Stream != "stderr" || got[1].Message != "keep" {
		t.Fatalf("%+v", got)
	}
}
func TestTTYAndMalformed(t *testing.T) {
	f := fakeSource{tty: true, data: map[string]string{"a": "2026-01-01T00:00:01Z hi\n"}}
	got, err := Tail(context.Background(), f, []Identity{{ContainerID: "a"}}, Options{Tail: 1, Since: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)})
	if err != nil || len(got) != 1 || got[0].Stream != "stdout" {
		t.Fatalf("%+v %v", got, err)
	}
	f.tty = false
	f.data["a"] = frame(1, "x")[:9]
	if _, err = Tail(context.Background(), f, []Identity{{ContainerID: "a"}}, Options{Tail: 1}); err == nil {
		t.Fatal("expected malformed stream error")
	}
}

type blockingSource struct{ closed chan struct{} }
type blockingReader struct{ closed chan struct{} }

func (b blockingReader) Read([]byte) (int, error) { <-b.closed; return 0, io.EOF }
func (b blockingReader) Close() error {
	select {
	case <-b.closed:
	default:
		close(b.closed)
	}
	return nil
}
func (b blockingSource) Open(context.Context, string, OpenOptions) (io.ReadCloser, bool, error) {
	return blockingReader{b.closed}, true, nil
}
func TestSplitFramesAndInvalidLines(t *testing.T) {
	f := fakeSource{data: map[string]string{"a": frame(1, "2026-01-01T00:00:") + frame(2, "2026-01-01T00:00:02Z err\n") + frame(1, "01Z out\n")}}
	got, err := Tail(context.Background(), f, []Identity{{ContainerID: "a"}}, Options{Tail: 2})
	if err != nil || len(got) != 2 || got[0].Message != "out" || got[1].Stream != "stderr" {
		t.Fatalf("%+v %v", got, err)
	}
	f.data["a"] = frame(1, "no timestamp\n")
	if _, err = Tail(context.Background(), f, []Identity{{ContainerID: "a"}}, Options{Tail: 1}); err == nil {
		t.Fatal("expected invalid timestamp")
	}
}
func TestFinalLineWithoutNewline(t *testing.T) {
	f := fakeSource{tty: true, data: map[string]string{"a": "2026-01-01T00:00:01Z final"}}
	got, err := Tail(context.Background(), f, []Identity{{ContainerID: "a"}}, Options{Tail: 1})
	if err != nil || len(got) != 1 || got[0].Message != "final" {
		t.Fatalf("%+v %v", got, err)
	}
}

type failingSource struct{}

func (failingSource) Open(context.Context, string, OpenOptions) (io.ReadCloser, bool, error) {
	return nil, false, errors.New("driver unavailable")
}
func TestSourceErrorAndTruncatedFrame(t *testing.T) {
	_, err := Tail(context.Background(), failingSource{}, []Identity{{ContainerID: "a"}}, Options{Tail: 1})
	if err == nil || !strings.Contains(err.Error(), "driver unavailable") {
		t.Fatalf("%v", err)
	}
	broken := frame(1, "2026-01-01T00:00:01Z hello\n")
	broken = broken[:len(broken)-1]
	_, err = Tail(context.Background(), fakeSource{data: map[string]string{"a": broken}}, []Identity{{ContainerID: "a"}}, Options{Tail: 1})
	if err == nil || !strings.Contains(err.Error(), "truncated log frame") {
		t.Fatalf("%v", err)
	}
}
func TestTailBoundsEncodedJSON(t *testing.T) {
	line := "2026-01-01T00:00:01Z " + strings.Repeat("\x00", 200000) + "\n"
	var data strings.Builder
	for i := 0; i < 8; i++ {
		data.WriteString(frame(1, line))
	}
	f := fakeSource{data: map[string]string{"a": data.String()}}
	if _, err := Tail(context.Background(), f, []Identity{{ContainerID: "a"}}, Options{Tail: 8}); err == nil {
		t.Fatal("escaped JSON exceeds limit")
	}
}
