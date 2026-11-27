package cli

import (
	"context"
	"os"
	"os/signal"
	"sync"
	"syscall"
)

type commandSignal struct{ syscall.Signal }

func (s commandSignal) Error() string { return s.Signal.String() }

// commandContext unregisters before publishing cancellation so another signal
// can end a command that is still cleaning up.
func commandContext() (context.Context, func() bool) {
	ctx, cancel := context.WithCancelCause(context.Background())
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, terminationSignals...)
	unregister := sync.OnceFunc(func() { signal.Stop(signals) })
	stopping, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		var received os.Signal
		select {
		case received = <-signals:
			unregister()
		case <-stopping:
			// Delivery has stopped, but a signal already queued still counts.
			select {
			case received = <-signals:
			default:
			}
		}
		if sig, ok := received.(syscall.Signal); ok {
			cancel(commandSignal{sig})
			return
		}
		cancel(nil)
	}()
	stop := sync.OnceValue(func() bool {
		unregister()
		close(stopping)
		<-done
		_, interrupted := context.Cause(ctx).(commandSignal)
		return interrupted
	})
	return ctx, stop
}

func credentialSignalStatus(ctx context.Context) int {
	if s, ok := context.Cause(ctx).(commandSignal); ok {
		return 128 + int(s.Signal)
	}
	return 130
}
