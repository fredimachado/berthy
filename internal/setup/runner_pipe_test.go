package setup

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRunnerProbe(t *testing.T) {
	mode := os.Getenv("BERTHY_PIPE_PROBE")
	if mode == "" {
		return
	}
	addr := os.Getenv("BERTHY_PIPE_ADDR")
	if mode == "holder" {
		c, e := net.Dial("tcp", addr)
		if e != nil {
			os.Exit(2)
		}
		defer c.Close()
		b := make([]byte, 1)
		if _, e = c.Read(b); e == nil {
			c.Write(b)
		}
		return
	}
	if mode == "child" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestRunnerProbe$")
		cmd.Env = append(os.Environ(), "BERTHY_PIPE_PROBE=holder")
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if cmd.Start() != nil {
			os.Exit(2)
		}
		if os.Getenv("BERTHY_PIPE_CASE") == "cancel" {
			cmd.Wait()
		}
		return
	}
	c, e := net.Dial("tcp", addr)
	if e != nil {
		os.Exit(2)
	}
	defer c.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmdargs := []string{"-test.run=^TestRunnerProbe$"}
	os.Setenv("BERTHY_PIPE_PROBE", "child")
	go func() {
		bufio.NewReader(c).ReadString('\n')
		if os.Getenv("BERTHY_PIPE_CASE") == "cancel" {
			cancel()
		}
	}()
	_, err := runProcess(ctx, os.Args[0], cmdargs, nil)
	result := "other"
	if errors.Is(err, context.Canceled) {
		result = "canceled"
	}
	if errors.Is(err, exec.ErrWaitDelay) {
		result = "waitdelay"
	}
	fmt.Fprintln(c, result)
}
func TestRunnerReturnsWhileChildHoldsPipes(t *testing.T) {
	for _, mode := range []string{"cancel", "exit"} {
		t.Run(mode, func(t *testing.T) {
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			var mu sync.Mutex
			var probe *exec.Cmd
			var connections []net.Conn
			expired := false
			cleanup := func() {
				mu.Lock()
				defer mu.Unlock()
				expired = true
				ln.Close()
				for _, c := range connections {
					c.Close()
				}
				if probe != nil && probe.Process != nil {
					probe.Process.Kill()
				}
			}
			done := make(chan struct{})
			go func() {
				select {
				case <-time.After(2 * time.Minute):
					cleanup()
				case <-done:
				}
			}()
			defer close(done)
			defer cleanup()
			cmd := exec.Command(os.Args[0], "-test.run=^TestRunnerProbe$")
			cmd.Env = append(os.Environ(), "BERTHY_PIPE_PROBE=probe", "BERTHY_PIPE_ADDR="+ln.Addr().String(), "BERTHY_PIPE_CASE="+mode)
			mu.Lock()
			if expired {
				mu.Unlock()
				t.Fatal("fixture watchdog expired")
			}
			err = cmd.Start()
			probe = cmd
			mu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			defer func() { cleanup(); cmd.Wait() }()
			accept := func() net.Conn {
				c, e := ln.Accept()
				if e != nil {
					t.Fatal(e)
				}
				mu.Lock()
				defer mu.Unlock()
				if expired {
					c.Close()
					t.Fatal("fixture watchdog expired")
				}
				connections = append(connections, c)
				return c
			}
			ctl := accept()
			holder := accept()
			fmt.Fprintln(ctl, "go")
			got, err := bufio.NewReader(ctl).ReadString('\n')
			if err != nil {
				t.Fatal(err)
			}
			expected := "waitdelay"
			if mode == "cancel" {
				expected = "canceled"
			}
			if strings.TrimSpace(got) != expected {
				t.Fatalf("got %q, expected %s", got, expected)
			}
			if _, err = holder.Write([]byte{'x'}); err != nil {
				t.Fatal(err)
			}
			b := make([]byte, 1)
			if _, err = holder.Read(b); err != nil || b[0] != 'x' {
				t.Fatal("holder did not remain alive", err)
			}
		})
	}
}
