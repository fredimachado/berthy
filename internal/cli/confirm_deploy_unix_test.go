//go:build unix

package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/charmbracelet/x/term"
	"github.com/creack/pty"
	"golang.org/x/sys/unix"
)

func TestConfirmDeployTerminal(t *testing.T) {
	for _, tc := range []struct {
		input string
		yes   bool
	}{
		{" yEs \nnext\n", true}, {"Y\n", true}, {"\n", false}, {"no\n", false}, {"yes please\n", false}, {"\x1b[?1000hy\n", false}, {strings.Repeat(" ", 257) + "yes\n", false}, {"y\x03", false},
	} {
		t.Run(tc.input, func(t *testing.T) {
			master, tty, err := pty.Open()
			if err != nil {
				t.Fatal(err)
			}
			defer master.Close()
			defer tty.Close()
			state, err := term.MakeRaw(tty.Fd())
			if err != nil {
				t.Fatal(err)
			}
			defer term.Restore(tty.Fd(), state)
			type result struct {
				ok  bool
				err error
			}
			resultCh := make(chan result, 1)
			go func() { ok, err := confirmDeploy(context.Background(), tty, tty); resultCh <- result{ok, err} }()
			prompt := make([]byte, len("Proceed with this deploy? [y/N] "))
			if _, err := io.ReadFull(master, prompt); err != nil {
				t.Fatal(err)
			}
			if string(prompt) != "Proceed with this deploy? [y/N] " {
				t.Fatalf("prompt %q", prompt)
			}
			if _, err := io.WriteString(master, tc.input); err != nil {
				t.Fatal(err)
			}
			got := <-resultCh
			if got.ok != tc.yes {
				t.Fatalf("ok=%v err=%v", got.ok, got.err)
			}
			if strings.HasSuffix(tc.input, "next\n") {
				fds := []unix.PollFd{{Fd: int32(tty.Fd()), Events: unix.POLLIN}}
				if n, err := unix.Poll(fds, 0); err != nil || n != 1 || fds[0].Revents&unix.POLLIN == 0 {
					t.Fatalf("next line unavailable: n=%d events=%v err=%v", n, fds[0].Revents, err)
				}
				fd := int(tty.Fd())
				if err := unix.SetNonblock(fd, true); err != nil {
					t.Fatal(err)
				}
				var next [5]byte
				if n, err := unix.Read(fd, next[:]); err != nil || n != len(next) || string(next[:]) != "next\n" {
					t.Fatalf("next=%q n=%d err=%v", next, n, err)
				}
			}
		})
	}
}

func TestConfirmDeployCanonicalQuotedEnter(t *testing.T) {
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
	type result struct {
		ok  bool
		err error
	}
	resultCh := make(chan result, 1)
	go func() { ok, err := confirmDeploy(context.Background(), tty, tty); resultCh <- result{ok, err} }()
	prompt := make([]byte, len("Proceed with this deploy? [y/N] "))
	if _, err := io.ReadFull(master, prompt); err != nil {
		t.Fatal(err)
	}
	during, err := term.GetState(tty.Fd())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := master.Write([]byte("y\x16\n")); err != nil {
		t.Fatal(err)
	}
	got := <-resultCh
	if reflect.DeepEqual(before, during) {
		t.Error("prompt displayed before terminal mode changed")
	}
	if got.ok || got.err != nil {
		t.Errorf("quoted Enter: ok=%v err=%v", got.ok, got.err)
	}
	fd := int(master.Fd())
	if err := unix.SetNonblock(fd, true); err != nil {
		t.Fatal(err)
	}
	transcript := make([]byte, len("y\r\n"))
	if n, err := unix.Read(fd, transcript); err != nil || n != len(transcript) || string(transcript) != "y\r\n" {
		t.Errorf("transcript = %q n=%d err=%v", transcript, n, err)
	}
	after, err := term.GetState(tty.Fd())
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Errorf("terminal not restored: %v", err)
	}
}

func TestConfirmDeployCancelAndWriteFailure(t *testing.T) {
	master, tty, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer master.Close()
	defer tty.Close()
	readOnly, err := os.Open(tty.Name())
	if err != nil {
		t.Fatal(err)
	}
	defer readOnly.Close()
	before, err := term.GetState(tty.Fd())
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := confirmDeploy(context.Background(), tty, readOnly); ok || err == nil {
		t.Fatalf("write: %v %v", ok, err)
	}
	after, err := term.GetState(tty.Fd())
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("terminal not restored after write failure: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() { _, err := confirmDeploy(ctx, tty, tty); result <- err }()
	prompt := make([]byte, len("Proceed with this deploy? [y/N] "))
	if _, err := io.ReadFull(master, prompt); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
	after, err = term.GetState(tty.Fd())
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("terminal not restored after cancellation: %v", err)
	}
}
