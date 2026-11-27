package cli

import (
	"context"
	"errors"
	"io"
	"syscall"
	"testing"
)

func TestSecretPipeEOFStillUsesSignalExit(t *testing.T) {
	for _, reader := range []string{"set", "import"} {
		for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGHUP} {
			t.Run(reader+"/"+sig.String(), func(t *testing.T) {
				withStdinFile(t, "FIXTURE=value\n")
				ctx, cancel := context.WithCancelCause(context.Background())
				defer cancel(nil)
				oldAfterFunc, oldExit := pipeSignalAfterFunc, exitProcess
				t.Cleanup(func() { pipeSignalAfterFunc, exitProcess = oldAfterFunc, oldExit })
				// Model EOF winning stop after the cause is published and before
				// cancellation reaches the registered callback.
				pipeSignalAfterFunc = func(_ context.Context, _ func()) func() bool {
					return func() bool {
						cancel(commandSignal{sig})
						return true
					}
				}
				var codes []int
				exitProcess = func(code int) { codes = append(codes, code) }

				if reader == "set" {
					value, _, code := secretValue(ctx, commandFlags{}, io.Discard, nil, "")
					if value != "" || code == 0 {
						t.Error("secret set accepted input after cancellation")
					}
				} else {
					input, err := readSecretImport(ctx, false)
					if !errors.Is(err, context.Canceled) || len(input.Secrets) != 0 {
						t.Error("secret import accepted input after cancellation")
					}
				}
				want := 128 + int(sig)
				if len(codes) != 1 || codes[0] != want {
					t.Errorf("signal exits %v, want [%d]", codes, want)
				}
			})
		}
	}
}
