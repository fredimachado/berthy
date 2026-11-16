package agentclient

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

type event struct{ name, data string }

func readAll(t *testing.T, ctx context.Context, input string, stopAfter int) ([]event, error) {
	t.Helper()
	var got []event
	err := ReadEvents(ctx, strings.NewReader(input), func(name, data string) error {
		got = append(got, event{name, data})
		if stopAfter > 0 && len(got) == stopAfter {
			return ErrStopEvents
		}
		return nil
	})
	return got, err
}

func TestReadEventsParsesEvents(t *testing.T) {
	input := ": keepalive\n\nevent: log\ndata: {\"a\":1}\n\nevent: stats\ndata: {\"b\":\ndata: 2}\n\ndata: {\"c\":3}\n\n"
	got, err := readAll(t, context.Background(), input, 0)
	if !errors.Is(err, ErrStreamEnded) {
		t.Fatalf("err %v", err)
	}
	want := []event{{"log", `{"a":1}`}, {"stats", "{\"b\":\n2}"}, {"", `{"c":3}`}}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("event %d: %v, want %v", i, got[i], want[i])
		}
	}
}

func TestReadEventsStopAndErrors(t *testing.T) {
	if got, err := readAll(t, context.Background(), "event: done\ndata: {\"complete\":true}\n\nevent: log\ndata: {}\n\n", 1); err != nil || len(got) != 1 {
		t.Fatalf("stop: %v %v", got, err)
	}
	for _, bad := range []string{"data: nope\n\n", "data: [1]\n\n", "data: {\"a\":\n\n"} {
		if _, err := readAll(t, context.Background(), bad, 0); !errors.Is(err, ErrInvalidEvent) {
			t.Errorf("%q: %v", bad, err)
		}
	}
	boom := errors.New("boom")
	err := ReadEvents(context.Background(), strings.NewReader("data: {}\n\n"), func(string, string) error { return boom })
	if !errors.Is(err, boom) {
		t.Fatalf("handler error lost: %v", err)
	}
}

func TestReadEventsContextAndBodyErrors(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := readAll(t, ctx, "", 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled: %v", err)
	}
	pr, pw := io.Pipe()
	_ = pw.CloseWithError(errors.New("reset"))
	if err := ReadEvents(context.Background(), pr, func(string, string) error { return nil }); !errors.Is(err, ErrStreamRead) {
		t.Fatalf("body error: %v", err)
	}
}

func TestReadEventsTooBig(t *testing.T) {
	line := "data: " + strings.Repeat("x", 1<<20) + "\n"
	input := strings.Repeat(line, 17)
	if _, err := readAll(t, context.Background(), input, 0); !errors.Is(err, ErrInvalidEvent) {
		t.Fatalf("%v", err)
	}
}
