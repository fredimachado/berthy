package credstore_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"berthy/internal/credstore"
	"berthy/internal/credstore/credtest"
)

func TestBoundedGivesUpWhenTheContextEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	block := make(chan struct{})
	m := credtest.NewMemory()
	m.Block = block
	m.OnCall = cancel
	s := credstore.Bounded(ctx, m, time.Hour)
	key := credstore.Key{URL: "http://10.0.0.5:7710", Project: "todo", Environment: "dev", Variable: "BERTHY_TODO_DEV_TOKEN"}
	err := s.Set(key, "secret")
	if !errors.Is(err, credstore.ErrGivenUp) || !errors.Is(err, credstore.ErrUnavailable) {
		t.Fatalf("Set: %v", err)
	}
	t.Cleanup(func() {
		close(block)
		m.Wait()
	})
}

func TestBoundedGivesUpAfterTheWait(t *testing.T) {
	block := make(chan struct{})
	started := make(chan struct{})
	m := credtest.NewMemory()
	m.Block = block
	// OnCall runs after inflight is counted, so Wait in cleanup cannot return before the blocked call exists.
	m.OnCall = func() { close(started) }
	s := credstore.Bounded(context.Background(), m, time.Millisecond)
	key := credstore.Key{URL: "http://10.0.0.5:7710", Project: "todo", Environment: "dev", Variable: "BERTHY_TODO_DEV_TOKEN"}
	errc := make(chan error, 1)
	go func() { errc <- s.Set(key, "secret") }()
	<-started
	err := <-errc
	if !errors.Is(err, credstore.ErrGivenUp) || !errors.Is(err, credstore.ErrUnavailable) {
		t.Fatalf("Set: %v", err)
	}
	t.Cleanup(func() {
		close(block)
		m.Wait()
	})
}

func TestBoundedPassesAnAnswerThrough(t *testing.T) {
	m := credtest.NewMemory()
	s := credstore.Bounded(context.Background(), m, time.Hour)
	key := credstore.Key{URL: "http://10.0.0.5:7710", Project: "todo", Environment: "dev", Variable: "BERTHY_TODO_DEV_TOKEN"}
	if err := s.Set(key, "secret"); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(key)
	if err != nil || got != "secret" {
		t.Fatalf("Get = %q, %v", got, err)
	}
	removed, err := s.Delete(key)
	if err != nil || !removed {
		t.Fatalf("Delete = %v, %v", removed, err)
	}
	if s.Name() != m.Name() {
		t.Fatalf("Name = %q, want %q", s.Name(), m.Name())
	}
}
