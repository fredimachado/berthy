//go:build unix

package cli

import (
	"bufio"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"reflect"
	"sync"
	"syscall"
	"testing"

	"github.com/charmbracelet/x/term"
	"github.com/creack/pty"

	"berthy/internal/agentclient"
	"berthy/internal/session"
)

type blockedSessionWriter struct{ gate *os.File }

func (w blockedSessionWriter) Write(p []byte) (int, error) {
	if string(p) == "blocked stderr frame" {
		fmt.Fprintln(os.Stdout, "writer entered")
		_, _ = io.Copy(io.Discard, w.gate)
	}
	return len(p), nil
}

func TestTerminationWhileTheTerminalIsRawRestoresItAndExitsWithTheSignal(t *testing.T) {
	const childAgent = "BERTHY_TEST_RAW_SESSION_AGENT"
	if address := os.Getenv(childAgent); address != "" {
		u, err := url.Parse(address)
		if err != nil {
			t.Fatal(err)
		}
		target := &agentclient.Target{Env: "dev", Base: u, Prefix: "/v1/projects/demo/environments/dev/", Token: "t"}
		gate := os.NewFile(3, "stderr release")
		defer gate.Close()
		ctx, stop := commandContext()
		defer stop()
		code := runInteractive(ctx, target, session.Request{Kind: "exec", App: "web", Command: []string{"sh"}}, os.Stdin, os.Stdin, blockedSessionWriter{gate})
		t.Fatalf("raw session returned %d without exiting on the signal", code)
	}

	for _, tc := range []struct {
		name string
		sig  syscall.Signal
		code int
	}{{"SIGTERM", syscall.SIGTERM, 143}, {"SIGHUP", syscall.SIGHUP, 129}} {
		t.Run(tc.name, func(t *testing.T) {
			master, tty, err := pty.Open()
			if err != nil {
				t.Fatal(err)
			}
			defer master.Close()
			defer tty.Close()
			before, err := term.GetState(tty.Fd())
			if err != nil {
				t.Fatal(err)
			}
			defer term.Restore(tty.Fd(), before)
			gate, release, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer gate.Close()
			hold := make(chan struct{})
			a := newSessionAgent(t, func(w io.Writer) {
				frame(session.Stderr, "blocked stderr frame")(w)
				<-hold
			})
			child := exec.Command(os.Args[0], "-test.run=^TestTerminationWhileTheTerminalIsRawRestoresItAndExitsWithTheSignal$")
			child.Env = append(os.Environ(), childAgent+"="+a.URL)
			child.Stdin, child.Stderr = tty, tty
			child.ExtraFiles = []*os.File{gate}
			releaseFixture := sync.OnceFunc(func() {
				_ = release.Close()
				close(hold)
				_ = master.Close()
			})
			defer func() {
				releaseFixture()
				if child.Process != nil && child.ProcessState == nil {
					_ = child.Process.Kill()
					_ = child.Wait()
				}
			}()
			ready, err := child.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			defer ready.Close()
			if err := child.Start(); err != nil {
				t.Fatal(err)
			}
			line, err := bufio.NewReader(ready).ReadString('\n')
			if err != nil || line != "writer entered\n" {
				t.Fatalf("child did not reach the blocked output write: %q, %v", line, err)
			}
			during, err := term.GetState(tty.Fd())
			if err != nil || reflect.DeepEqual(during, before) {
				t.Fatalf("terminal was not put in raw mode: %v", err)
			}
			if err := child.Process.Signal(tc.sig); err != nil {
				t.Fatal(err)
			}
			// The gate only opens in teardown. Even a stuck write must allow exit.
			waitErr := waitForSignalChild(t, child, releaseFixture, fmt.Sprintf("child did not exit while stderr was blocked after %s; want %d", tc.sig, tc.code))
			if code := child.ProcessState.ExitCode(); code != tc.code {
				t.Errorf("native exit %d while stderr was blocked, want %d (%v)", code, tc.code, waitErr)
			}
			after, err := term.GetState(tty.Fd())
			if err != nil || !reflect.DeepEqual(after, before) {
				t.Errorf("terminal mode not restored while stderr was blocked: %v", err)
			}
		})
	}
}
