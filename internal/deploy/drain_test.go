package deploy

import (
	"context"
	"strings"
	"testing"
	"time"
)

// holdingObserver blocks every end until release is closed: the operation's goroutine is still running, after the state
// holds its outcome.
type holdingObserver struct {
	recordingObserver
	release chan struct{}
}

func (h *holdingObserver) OperationEnded(ctx context.Context, e OperationEvent) {
	<-h.release
	h.recordingObserver.OperationEnded(ctx, e)
}

// TestDrainReturnsWhenTheOperationsHaveEnded: Drain is how the agent, and a test, learns that no operation goroutine is left to
// touch the store.
func TestDrainReturnsWhenTheOperationsHaveEnded(t *testing.T) {
	s := openStore(t)
	obs := &holdingObserver{release: make(chan struct{})}
	c := &Controller{Engine: &boundFake{}, Store: s, Environment: "prod", Observer: obs}
	op, _, err := c.Admit(context.Background(), Actor{ID: "ci"}, Request{RequestID: "wait", App: "web", Version: "sha256:" + strings.Repeat("a", 64), Manifest: dockerManifest(nil)})
	if err != nil {
		t.Fatal(err)
	}
	if got := waitOperation(t, s, op.ID); got.State != "succeeded" {
		t.Fatalf("state %s: %s", got.State, got.Err)
	}
	short, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err = c.Drain(short); err == nil {
		t.Fatal("Drain returned while the operation's goroutine was still running")
	}
	close(obs.release)
	long, cancelLong := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelLong()
	if err = c.Drain(long); err != nil {
		t.Fatalf("Drain after the operation ended: %v", err)
	}
	if n := len(obs.ended); n != 1 {
		t.Fatalf("the observer heard %d ends before Drain returned, want 1", n)
	}
}

// TestDrainOfAControllerThatAdmittedNothingReturnsAtOnce: the zero value is ready, as the other fields of a Controller are.
func TestDrainOfAControllerThatAdmittedNothingReturnsAtOnce(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := (&Controller{}).Drain(ctx); err != nil {
		t.Fatal(err)
	}
}
