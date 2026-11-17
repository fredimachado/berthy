package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/charmbracelet/x/term"

	"berthy/internal/agentclient"
	"berthy/internal/session"
)

// exitProcess ends the process. It is a variable so a test can stand in for it.
var exitProcess = os.Exit

// stdinChunk is how much of standard input one Stdin frame carries.
const stdinChunk = 32 << 10

// terminalSize is the size of the terminal on fd, or nil when it has none the agent would accept.
func terminalSize(fd uintptr) *session.Size {
	cols, rows, err := term.GetSize(fd)
	if err != nil || rows < 1 || rows > 1000 || cols < 1 || cols > 1000 {
		return nil
	}
	return &session.Size{Rows: uint16(rows), Cols: uint16(cols)}
}

// runInteractive runs an exec or run session: it allocates a terminal when stdin and out are both terminals, putting the
// terminal in raw mode (and always restoring it), sends its size and every change of it, copies stdin into Stdin frames
// (CloseStdin at its end), writes Stdout and Stderr frames to out and errout, and returns the remote exit code. A Failure or a
// lost connection is a message on errout and 1.
func runInteractive(ctx context.Context, t *agentclient.Target, req session.Request, stdin io.Reader, out, errout io.Writer) int {
	in, _ := stdin.(*os.File)
	screen, _ := out.(*os.File)
	if in != nil && screen != nil && term.IsTerminal(in.Fd()) && term.IsTerminal(screen.Fd()) {
		req.TTY = true
		req.Size = terminalSize(screen.Fd())
	}
	conn, br, err := agentclient.OpenSession(ctx, t, req)
	if err != nil {
		if ctx.Err() != nil {
			fmt.Fprintln(errout, "interrupted")
			return 130
		}
		fmt.Fprintln(errout, err)
		return 1
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	w := session.NewWriter(conn)
	if req.TTY {
		state, err := term.MakeRaw(in.Fd())
		if err != nil {
			fmt.Fprintln(errout, "cannot put the terminal in raw mode")
			return 1
		}
		// The process exits next, so a failed restore has nowhere to be reported.
		restore := func() { _ = term.Restore(in.Fd(), state) }
		defer restore()
		// Ctrl-C is a byte for the remote while the terminal is raw, but a terminated CLI or a closed window must not leave the
		// person's terminal raw: restore it, drop the connection and exit as a shell reports a signal, 128 plus its number.
		signals := make(chan os.Signal, 1)
		signal.Notify(signals, terminationSignals...)
		defer signal.Stop(signals)
		go func() {
			sig, ok := <-signals
			if !ok {
				return
			}
			restore()
			conn.Close()
			code := 1
			if n, isNumber := sig.(syscall.Signal); isNumber {
				code = 128 + int(n)
			}
			exitProcess(code)
		}()
		go func() {
			for range terminalResizes(ctx, screen.Fd()) {
				if size := terminalSize(screen.Fd()); size != nil {
					_ = w.Frame(session.Resize, size.Encode())
				}
			}
		}()
	}
	// A read of the terminal cannot be cancelled, so this goroutine may outlive the session; the process exits with it.
	go func() {
		buf := make([]byte, stdinChunk)
		for {
			n, err := stdin.Read(buf)
			if n > 0 && w.Frame(session.Stdin, buf[:n]) != nil {
				return
			}
			if err != nil {
				_ = w.Frame(session.CloseStdin, nil)
				return
			}
		}
	}()
	for {
		kind, payload, err := session.ReadFrame(br)
		if err != nil {
			if ctx.Err() != nil {
				fmt.Fprintln(errout, "interrupted")
				return 130
			}
			fmt.Fprintln(errout, "the session ended unexpectedly")
			return 1
		}
		switch kind {
		case session.Stdout:
			_, _ = out.Write(payload)
		case session.Stderr:
			_, _ = errout.Write(payload)
		case session.Exit:
			var status session.ExitStatus
			if json.Unmarshal(payload, &status) != nil || status.Code < 0 {
				fmt.Fprintln(errout, "the session ended unexpectedly")
				return 1
			}
			return status.Code
		case session.Failure:
			fmt.Fprintln(errout, string(payload))
			return 1
		}
	}
}
