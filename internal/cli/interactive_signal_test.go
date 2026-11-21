//go:build unix

package cli

import (
	"bytes"
	"context"
	"io"
	"net/url"
	"reflect"
	"syscall"
	"testing"
	"time"

	"github.com/charmbracelet/x/term"
	"github.com/creack/pty"

	"berthy/internal/agentclient"
	"berthy/internal/session"
)

// A terminated CLI must give the person's terminal back as it found it, and exit as a shell reports the signal.
func TestTerminationWhileTheTerminalIsRawRestoresItAndExitsWithTheSignal(t *testing.T) {
	for name, tc := range map[string]struct {
		sig  syscall.Signal
		code int
	}{"SIGTERM": {syscall.SIGTERM, 143}, "SIGHUP": {syscall.SIGHUP, 129}} {
		t.Run(name, func(t *testing.T) {
			ptmx, tty, err := pty.Open()
			if err != nil {
				t.Skipf("no pseudo-terminal here: %v", err)
			}
			defer ptmx.Close()
			defer tty.Close()
			hold := make(chan struct{})
			a := newSessionAgent(t, func(io.Writer) { <-hold })
			defer close(hold)
			exited := make(chan int, 1)
			old := exitProcess
			exitProcess = func(code int) { exited <- code }
			defer func() { exitProcess = old }()
			before, err := term.GetState(tty.Fd())
			if err != nil {
				t.Fatal(err)
			}
			u, _ := url.Parse(a.URL)
			target := &agentclient.Target{Env: "dev", Base: u, Prefix: "/v1/projects/demo/environments/dev/", Token: "t"}
			done := make(chan struct{})
			go func() {
				defer close(done)
				runInteractive(context.Background(), target, session.Request{Kind: "exec", App: "web", Command: []string{"sh"}}, tty, tty, &bytes.Buffer{})
			}()
			// The session must be over, and its read of the terminal returned, before the test takes the terminal and the stub away.
			defer func() {
				<-done
				_, _ = ptmx.WriteString("\n")
				time.Sleep(100 * time.Millisecond)
			}()
			raw := false
			for i := 0; i < 300 && !raw; i++ {
				if now, err := term.GetState(tty.Fd()); err == nil && !reflect.DeepEqual(now, before) {
					raw = true
				}
				time.Sleep(10 * time.Millisecond)
			}
			if !raw {
				t.Fatal("the terminal never went raw")
			}
			time.Sleep(50 * time.Millisecond) // the handler is registered before the first frame is read
			if err = syscall.Kill(syscall.Getpid(), tc.sig); err != nil {
				t.Fatal(err)
			}
			select {
			case code := <-exited:
				if code != tc.code {
					t.Errorf("exit code %d, want %d", code, tc.code)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("the CLI did not exit")
			}
			if now, err := term.GetState(tty.Fd()); err != nil || !reflect.DeepEqual(now, before) {
				t.Errorf("the terminal was left raw: %v", err)
			}
		})
	}
}
