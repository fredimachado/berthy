package logs

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"reflect"
	"regexp"
	"runtime"
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

func TestLogFrameMissingEntirePayload(t *testing.T) {
	for _, stream := range []byte{1, 3} {
		wire := frame(stream, "x")[:8]
		err := decode(strings.NewReader(wire), false, Identity{}, Options{}, func(Record) error { return nil })
		if !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Errorf("stream %d missing payload: %v", stream, err)
		}
	}
}

func TestLogFrameAllocationTracksInput(t *testing.T) {
	for _, size := range []int{0, 64, 4096, MaxFrameBytes} {
		wire := []byte(frame(1, strings.Repeat("x", size)))
		for _, input := range [][]byte{wire, wire[:8]} {
			r := bytes.NewReader(input)
			runtime.GC()
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			_ = decode(r, false, Identity{}, Options{}, func(Record) error { return nil })
			runtime.ReadMemStats(&after)
			allocated := after.TotalAlloc - before.TotalAlloc
			limit := uint64(16384 + 12*len(input))
			if allocated > limit {
				t.Errorf("input %d declared %d: allocated %d, limit %d", len(input), size, allocated, limit)
			}
		}
	}
}

func FuzzMultiplexedLogs(f *testing.F) {
	for _, wire := range []string{
		"", frame(1, ""), frame(1, "2026-01-01T00:00:01Z hello\n"),
		frame(1, "2026-01-01T00:00:") + frame(2, "2026-01-01T00:00:02Z err\n") + frame(1, "01Z out\n"),
		frame(1, "2026-01-01T00:00:01Z final"), frame(1, "x")[:8],
		string([]byte{3, 0, 0, 0, 0, 0, 0, 0}), string([]byte{1, 1, 0, 0, 0, 0, 0, 0}),
		string([]byte{1, 0, 0, 0, 0, 16, 0, 1}),
	} {
		f.Add([]byte(wire), uint32(8))
	}
	f.Fuzz(func(t *testing.T, wire []byte, cut uint32) {
		id := Identity{Role: "web", Replica: "2", ContainerID: "container"}
		emit := func(Record) error { return nil }
		_ = decode(bytes.NewReader(wire), false, id, Options{}, emit)
		message := hex.EncodeToString(wire[:min(len(wire), 128)])
		line := "2026-01-01T00:00:01Z " + message
		split := int(cut % uint32(len(line)+1))
		other := "2026-01-01T00:00:02Z other\n"
		combined := frame(1, "") + frame(1, line[:split]) + frame(2, other) + frame(1, line[split:])
		var got []Record
		err := decode(strings.NewReader(combined), false, id, Options{}, func(r Record) error { got = append(got, r); return nil })
		want := []Record{
			{Identity: id, Timestamp: time.Date(2026, 1, 1, 0, 0, 2, 0, time.UTC), Stream: "stderr", Message: "other"},
			{Identity: id, Timestamp: time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC), Stream: "stdout", Message: message},
		}
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("records: %+v, error: %v", got, err)
		}
		valid := frame(1, "2026-01-01T00:00:01Z hello\n")
		n := int(cut % uint32(len(valid)+1))
		err = decode(strings.NewReader(valid[:n]), false, id, Options{}, emit)
		if n == 0 || n == len(valid) {
			if err != nil {
				t.Fatalf("complete stream: %v", err)
			}
		} else if !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("cut %d: %v", n, err)
		}
		for _, bad := range [][]byte{{4, 0, 0, 0, 0, 0, 0, 0}, {1, 1, 0, 0, 0, 0, 0, 0}, {1, 0, 0, 0, 0, 16, 0, 1}} {
			if err := decode(bytes.NewReader(bad), false, id, Options{}, emit); err == nil {
				t.Fatal("invalid header accepted")
			}
		}
	})
}

func TestLongLineKeepsPrefixAndNextRecord(t *testing.T) {
	stamp := "2026-01-01T00:00:01Z "
	line := strings.Repeat("abc", 100*1024)
	id := Identity{Role: "web", Replica: "2", ContainerID: "a"}
	want := []Record{
		{Identity: id, Timestamp: time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC), Stream: "stdout", Message: "before"},
		{Identity: id, Timestamp: time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC), Stream: "stdout", Message: line[:MaxLineBytes-len(stamp)] + TruncationMarker, Truncated: true},
		{Identity: id, Timestamp: time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC), Stream: "stdout", Message: "after"},
	}
	for _, tc := range []struct {
		name, wire string
		tty        bool
	}{
		{"single frame", frame(1, stamp+"before\n"+stamp+line+"\n"+stamp+"after\n"), false},
		{"continuations", frame(1, stamp+"before\n") + frame(1, stamp+line[:100000]) + frame(1, stamp+line[100000:200000]) + frame(1, stamp+line[200000:]+"\n") + frame(1, stamp+"after\n"), false},
		{"tty", stamp + "before\n" + stamp + line + "\n" + stamp + "after\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := fakeSource{tty: tc.tty, data: map[string]string{"a": tc.wire}}
			for _, follow := range []bool{false, true} {
				var got []Record
				var err error
				if follow {
					err = Follow(context.Background(), f, []Identity{id}, Options{}, func(r Record) error { got = append(got, r); return nil })
				} else {
					got, err = Tail(context.Background(), f, []Identity{id}, Options{})
				}
				if err != nil {
					t.Errorf("follow=%v: %v", follow, err)
					continue
				}
				if !reflect.DeepEqual(got, want) {
					t.Errorf("follow=%v: records differ", follow)
				}
			}
		})
	}
}

func TestLongLineStateBelongsToItsStream(t *testing.T) {
	stamp := "2026-01-01T00:00:01Z "
	long := strings.Repeat("x", MaxLineBytes+10)
	exact := strings.Repeat("y", MaxLineBytes-len(stamp))
	wire := frame(1, stamp+long) + frame(2, stamp+"error\n") + frame(1, stamp+"discard\n") + frame(1, stamp+exact[:100]) + frame(1, stamp+exact[100:]+"\n")
	var got []Record
	err := decode(strings.NewReader(wire), false, Identity{}, Options{}, func(r Record) error { got = append(got, r); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].Stream != "stderr" || got[0].Message != "error" || got[1].Message != long[:MaxLineBytes-len(stamp)]+TruncationMarker || !got[1].Truncated || got[2].Message != exact || got[2].Truncated {
		t.Fatal("stream state or boundary incorrect")
	}
}

func TestDaemonErrorIsARecordForOneSource(t *testing.T) {
	stamp := "2026-01-01T00:00:01Z "
	ids := []Identity{{Role: "web", Replica: "1", ContainerID: "a"}, {Role: "worker", Replica: "2", ContainerID: "b"}}
	f := fakeSource{data: map[string]string{
		"a": frame(1, stamp+"before\n") + frame(3, "log reader failed\r\n") + frame(1, stamp+"ignored\n"),
		"b": frame(1, stamp+"healthy\n"),
	}}
	for _, follow := range []bool{false, true} {
		var got []Record
		var err error
		if follow {
			err = Follow(context.Background(), f, ids, Options{}, func(r Record) error { got = append(got, r); return nil })
		} else {
			got, err = Tail(context.Background(), f, ids, Options{})
		}
		if err != nil {
			t.Errorf("follow=%v: %v", follow, err)
			continue
		}
		if len(got) != 3 {
			t.Errorf("follow=%v: got %d records", follow, len(got))
			continue
		}
		seen := map[string]bool{}
		for _, r := range got {
			switch r.Message {
			case "before":
				seen["before"] = r.Identity == ids[0] && r.Stream == "stdout"
			case "healthy":
				seen["healthy"] = r.Identity == ids[1] && r.Stream == "stdout"
			case "Docker daemon error: log reader failed":
				seen["error"] = r.Identity == ids[0] && r.Stream == "stderr" && !r.Timestamp.IsZero() && r.Timestamp.Location() == time.UTC
			}
		}
		if !seen["before"] || !seen["healthy"] || !seen["error"] {
			t.Errorf("follow=%v: %+v", follow, got)
		}
	}
}

func TestDaemonErrorIsBoundedAndFiltered(t *testing.T) {
	const label = "Docker daemon error: "
	body := strings.Repeat("x", MaxLineBytes) + "\n"
	wire := frame(3, body)
	var rows []Record
	err := decode(strings.NewReader(wire), false, Identity{}, Options{}, func(r Record) error { rows = append(rows, r); return nil })
	if err != nil || len(rows) != 1 || !rows[0].Truncated || rows[0].Message != label+body[:MaxLineBytes-len(label)]+TruncationMarker {
		t.Fatalf("bounded record: count=%d error=%v", len(rows), err)
	}
	rows = nil
	err = decode(strings.NewReader(wire), false, Identity{}, Options{Grep: regexp.MustCompile("no-match")}, func(r Record) error { rows = append(rows, r); return nil })
	if err != nil || len(rows) != 0 {
		t.Fatalf("filtered record: count=%d error=%v", len(rows), err)
	}
}
