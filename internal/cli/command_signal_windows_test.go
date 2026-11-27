//go:build windows

package cli

import (
	"context"
	"os"
	"slices"
	"syscall"
	"testing"
)

func TestCommandContextWindows(t *testing.T) {
	want := []os.Signal{os.Interrupt, syscall.SIGTERM}
	if !slices.Equal(terminationSignals, want) {
		t.Errorf("signals %v, want %v", terminationSignals, want)
	}
	ctx, stop := commandContext()
	if stop() || stop() || ctx.Err() != context.Canceled || context.Cause(ctx) != context.Canceled {
		t.Errorf("normal stop: error %v, cause %v", ctx.Err(), context.Cause(ctx))
	}
	signalCtx, cancel := context.WithCancelCause(context.Background())
	cancel(commandSignal{syscall.SIGTERM})
	if got := credentialSignalStatus(signalCtx); got != 143 {
		t.Errorf("SIGTERM exit %d, want 143", got)
	}
}
