package logs

import (
	"context"
	"io"
	"testing"
	"time"
)

type observedSource struct {
	opened chan struct{}
	closed chan struct{}
}

func (b observedSource) Open(context.Context, string, OpenOptions) (io.ReadCloser, bool, error) {
	close(b.opened)
	return blockingReader{b.closed}, true, nil
}
func TestOpenedFollowClosesOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	source := observedSource{make(chan struct{}), make(chan struct{})}
	done := make(chan error, 1)
	go func() {
		done <- Follow(ctx, source, []Identity{{ContainerID: "a"}}, Options{Tail: 1}, func(Record) error { return nil })
	}()
	select {
	case <-source.opened:
	case <-time.After(time.Second):
		t.Fatal("source not opened")
	}
	cancel()
	select {
	case <-source.closed:
	case <-time.After(time.Second):
		t.Fatal("source not closed")
	}
	if err := <-done; err != context.Canceled {
		t.Fatalf("got %v", err)
	}
}
