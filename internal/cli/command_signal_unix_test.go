//go:build unix

package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"testing"
)

func TestCommandContextSignalsUnix(t *testing.T) {
	const childCase = "BERTHY_TEST_COMMAND_SIGNAL"
	if mode := os.Getenv(childCase); mode != "" {
		signals := map[string]syscall.Signal{"INT": syscall.SIGINT, "TERM": syscall.SIGTERM, "HUP": syscall.SIGHUP, "second TERM": syscall.SIGTERM}
		sig := signals[mode]
		signal.Reset(syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
		ctx, stop := commandContext()
		if err := syscall.Kill(os.Getpid(), sig); err != nil {
			t.Fatal(err)
		}
		<-ctx.Done()
		if ctx.Err() != context.Canceled || context.Cause(ctx) != (commandSignal{sig}) {
			t.Fatalf("context error %v, cause %v, want signal %s", ctx.Err(), context.Cause(ctx), sig)
		}
		if mode == "second TERM" {
			// Leave stop uncalled so only the receiver can restore the disposition.
			fmt.Fprintln(os.Stdout, "canceled")
			_, _ = io.Copy(io.Discard, os.Stdin)
			t.Fatal("second SIGTERM did not terminate the child")
		}
		if !stop() || !stop() {
			t.Fatal("stop did not consistently report interruption")
		}
		return
	}

	for _, mode := range []string{"INT", "TERM", "HUP", "second TERM"} {
		t.Run(mode, func(t *testing.T) {
			child := exec.Command(os.Args[0], "-test.run=^TestCommandContextSignalsUnix$")
			child.Env = append(os.Environ(), childCase+"="+mode)
			if mode != "second TERM" {
				if out, err := child.CombinedOutput(); err != nil {
					t.Fatalf("child: %v\n%s", err, out)
				}
				return
			}

			stdin, err := child.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			defer stdin.Close()
			stdout, err := child.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			defer stdout.Close()
			if err := child.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() {
				if child.ProcessState == nil {
					_ = child.Process.Kill()
					_ = child.Wait()
				}
			}()
			line, err := bufio.NewReader(stdout).ReadString('\n')
			if err != nil || line != "canceled\n" {
				t.Fatalf("child did not report cancellation: %q, %v", line, err)
			}
			if err := child.Process.Signal(syscall.SIGTERM); err != nil {
				t.Fatal(err)
			}
			err = child.Wait()
			status := child.ProcessState.Sys().(syscall.WaitStatus)
			if !status.Signaled() || status.Signal() != syscall.SIGTERM {
				t.Fatalf("second SIGTERM: wait status %v (%v)", status, err)
			}
		})
	}
}
