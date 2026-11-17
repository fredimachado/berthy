//go:build unix

package cli

import (
	"context"
	"os"
	"os/signal"
	"syscall"
)

// terminalResizes sends on the channel each time the terminal's size may have changed, until ctx ends. A change that comes
// while one is waiting to be read is not sent again, as the reader looks at the size when it reads.
func terminalResizes(ctx context.Context, _ uintptr) <-chan struct{} {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGWINCH)
	events := make(chan struct{}, 1)
	go func() {
		defer signal.Stop(signals)
		defer close(events)
		for {
			select {
			case <-ctx.Done():
				return
			case <-signals:
				select {
				case events <- struct{}{}:
				default:
				}
			}
		}
	}()
	return events
}

// terminationSignals are the signals that end the CLI while the terminal is raw: a terminated process, or a closed terminal window.
var terminationSignals = []os.Signal{syscall.SIGTERM, syscall.SIGHUP}
