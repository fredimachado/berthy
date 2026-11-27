//go:build unix

package cli

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"reflect"
	"runtime"
	"syscall"
	"testing"

	"github.com/charmbracelet/x/term"
	"github.com/creack/pty"
	"golang.org/x/sys/unix"
)

func TestReadHiddenRestoresModeOnSignal(t *testing.T) {
	const childMode = "BERTHY_TEST_HIDDEN_INPUT"
	if mode := os.Getenv(childMode); mode != "" {
		ctx, stop := commandContext()
		defer stop()
		data, err := readHidden(ctx, os.Stdin.Fd())
		switch mode {
		case "editing":
			if err != nil || string(data) != "abc" {
				t.Fatal("hidden line did not preserve canonical editing and carriage return")
			}
		case "EOF":
			if !errors.Is(err, io.EOF) || len(data) != 0 {
				t.Fatal("hidden line did not report terminal EOF")
			}
		default:
			t.Fatal("interrupted hidden read returned to its caller")
		}
		return
	}

	for _, tc := range []struct {
		name string
		sig  syscall.Signal
		code int
	}{
		{"SIGINT", syscall.SIGINT, 130},
		{"SIGTERM", syscall.SIGTERM, 143},
		{"SIGHUP", syscall.SIGHUP, 129},
		{"editing", 0, 0},
		{"EOF", 0, 0},
	} {
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
			if before.Lflag&unix.ECHO == 0 {
				t.Fatal("fixture terminal started without echo")
			}
			defer term.Restore(tty.Fd(), before)
			child := exec.Command(os.Args[0], "-test.run=^TestReadHiddenRestoresModeOnSignal$")
			child.Env = append(os.Environ(), childMode+"="+tc.name)
			child.Stdin, child.Stdout, child.Stderr = tty, os.Stdout, tty
			if err := child.Start(); err != nil {
				t.Fatal(err)
			}
			done := make(chan struct{})
			var waitErr error
			go func() {
				waitErr = child.Wait()
				close(done)
			}()
			defer func() {
				_ = child.Process.Kill()
				<-done
			}()
			for {
				now, err := term.GetState(tty.Fd())
				if err != nil {
					t.Fatal(err)
				}
				if now.Lflag&unix.ECHO == 0 {
					if now.Lflag&(unix.ICANON|unix.ISIG) != unix.ICANON|unix.ISIG {
						t.Error("hidden mode disabled canonical input or signals")
					}
					break
				}
				select {
				case <-done:
					t.Fatalf("child exited before entering hidden mode: %v", waitErr)
				default:
					runtime.Gosched()
				}
			}
			switch {
			case tc.sig != 0:
				err = child.Process.Signal(tc.sig)
			case tc.name == "EOF":
				_, err = master.Write([]byte{before.Cc[unix.VEOF]})
			default:
				_, err = master.Write([]byte{'a', 'b', 'x', before.Cc[unix.VERASE], 'c', '\r'})
			}
			if err != nil {
				t.Fatal(err)
			}
			<-done
			if code := child.ProcessState.ExitCode(); code != tc.code {
				t.Errorf("native exit %d, want %d (%v)", code, tc.code, waitErr)
			}
			after, err := term.GetState(tty.Fd())
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Errorf("hidden terminal mode was not restored: %v", err)
			}
		})
	}
}
