//go:build windows

package cli

import (
	"context"
	"os"
	"syscall"
	"time"
)

// terminalResizes sends on the channel each time the size of the terminal on fd has changed, until ctx ends. Windows has no
// signal for it, so the size is read every 250 ms.
func terminalResizes(ctx context.Context, fd uintptr) <-chan struct{} {
	events := make(chan struct{}, 1)
	last := terminalSize(fd)
	go func() {
		defer close(events)
		tick := time.NewTicker(250 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				now := terminalSize(fd)
				if now != nil && (last == nil || *now != *last) {
					last = now
					select {
					case events <- struct{}{}:
					default:
					}
				}
			}
		}
	}()
	return events
}

// terminationSignals are the signals that end the CLI while the terminal is raw. Windows has SIGTERM for Notify's purposes only.
var terminationSignals = []os.Signal{syscall.SIGTERM}
