// Package setup prepares a server for the agent: it connects to the server, checks it, and makes sure Docker, Swarm and the
// shared network are in place. It runs shell scripts as root through a Runner, over the operator's own ssh client or on the
// server itself. Every step checks before it acts, so a second run on a server that is set up changes nothing.
package setup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"

	"berthy/internal/sshhost"
)

// Runner runs shell scripts as root on the server being set up.
type Runner interface {
	// Run runs script with sh as root (through sudo -n when the login is not root). stdin, when not nil, is the script's
	// standard input, for uploads. It returns the exit code for a completed script; err reports a runner or transport failure.
	Run(ctx context.Context, script string, stdin io.Reader) (Result, error)
	// Target is what the output calls the server: "ops@s1", or "this server" for --local.
	Target() string
}

// SudoSetter is implemented by runners that can wrap their scripts in sudo. Connect turns it on when the login is not root.
type SudoSetter interface {
	SetSudo(bool)
}

// Result is what a script printed and the exit code it ended with.
type Result struct {
	Stdout, Stderr []byte
	Code           int
}

// maxCaptured is how much of each output stream is kept: the tail, because the end of an installer's output is what says why
// it failed.
const maxCaptured = 1 << 20

const pipeGrace = 5 * time.Second

// shellQuote quotes s as one word for sh: in single quotes, where each embedded single quote ends the quoting, is escaped
// with a backslash and starts it again. Inside single quotes nothing is special, so $, backticks, backslashes and newlines
// stay as they are.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// remoteCommand is the command line the server's shell runs for a script: sh -c SCRIPT, through sudo -n when sudo is set.
func remoteCommand(sudo bool, script string) string {
	cmd := "sh -c " + shellQuote(script)
	if sudo {
		return "sudo -n " + cmd
	}
	return cmd
}

// sshArgs is the argument list of the ssh process for a script on dest. The destination follows "--", so it can never be an
// option, and BatchMode keeps ssh from ever asking for a password.
func sshArgs(dest string, sudo bool, script string) []string {
	return []string{"-o", "BatchMode=yes", "-o", "ConnectTimeout=30", "-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=3", "-T", "--", dest, remoteCommand(sudo, script)}
}

// localArgs is the command and arguments that run a script on this server: sh -c directly for root, sudo -n sh -c otherwise.
func localArgs(sudo bool, script string) (string, []string) {
	if sudo {
		return "sudo", []string{"-n", "sh", "-c", script}
	}
	return "sh", []string{"-c", script}
}

// SSH runs scripts through the operator's own ssh client, found on PATH, so keys, agents, jump hosts and host key checks
// behave as they do for ssh itself. It never asks for a password and never stores a key.
type SSH struct {
	Dest string
	// Program is the ssh executable.
	Program string
	sudo    bool
}

// NewSSH checks dest and finds ssh on PATH.
func NewSSH(dest string) (*SSH, error) {
	if !sshhost.Valid(dest) {
		return nil, fmt.Errorf("invalid host %q: use USER@SERVER or SERVER", dest)
	}
	program, err := exec.LookPath("ssh")
	if err != nil {
		return nil, errors.New("ssh was not found on PATH: install an OpenSSH client")
	}
	return &SSH{Dest: dest, Program: program}, nil
}

func (r *SSH) Target() string { return r.Dest }

func (r *SSH) SetSudo(v bool) { r.sudo = v }

// SSHTransportError reports a failed SSH connection without exposing remote output.
type SSHTransportError struct {
	Target string
	reason string
}

func (e *SSHTransportError) Error() string {
	return "SSH connection to " + e.Target + " failed" + e.reason
}

func (r *SSH) Run(ctx context.Context, script string, stdin io.Reader) (Result, error) {
	return r.run(ctx, r.sudo, script, stdin)
}

func (r *SSH) runSSHLogin(ctx context.Context, script string) (Result, error) {
	return r.run(ctx, false, script, nil)
}

func (r *SSH) run(ctx context.Context, sudo bool, script string, stdin io.Reader) (Result, error) {
	if !sshhost.Valid(r.Dest) {
		return Result{}, fmt.Errorf("invalid host %q: use USER@SERVER or SERVER", r.Dest)
	}
	res, err := runProcess(ctx, r.Program, sshArgs(r.Dest, sudo, script), stdin)
	if err != nil {
		return res, err
	}
	if res.Code != 255 {
		return res, nil
	}
	reason := ""
	switch {
	case strings.Contains(string(res.Stderr), "Host key verification failed"):
		reason = ": Host key verification failed; connect once with ssh " + r.Dest + " to check and accept the server's host key"
	case strings.Contains(string(res.Stderr), "Connection refused"):
		reason = ": Connection refused"
	case strings.Contains(string(res.Stderr), "Connection timed out") || strings.Contains(string(res.Stderr), "Operation timed out") ||
		strings.Contains(string(res.Stderr), "Timeout, server ") && strings.Contains(string(res.Stderr), " not responding."):
		reason = ": Connection timed out"
	}
	return Result{}, &SSHTransportError{Target: r.Dest, reason: reason}
}

// Local runs scripts on this server: sh -c directly when it runs as root, sudo -n sh -c otherwise.
type Local struct {
	sudo bool
}

// NewLocal checks that this is a Linux server.
func NewLocal() (*Local, error) { return newLocal(runtime.GOOS) }

func newLocal(goos string) (*Local, error) {
	if goos != "linux" {
		return nil, errors.New("--local sets up the server the command runs on, which must run Linux")
	}
	return &Local{}, nil
}

func (r *Local) Target() string { return "this server" }

func (r *Local) SetSudo(v bool) { r.sudo = v }

func (r *Local) Run(ctx context.Context, script string, stdin io.Reader) (Result, error) {
	name, args := localArgs(r.sudo, script)
	return runProcess(ctx, name, args, stdin)
}

// runProcess runs name with args and captures the tail of both output streams. A non-zero exit is a Result; a process that
// could not start is an error.
func runProcess(ctx context.Context, name string, args []string, stdin io.Reader) (Result, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.WaitDelay = pipeGrace
	cmd.Stdin = stdin
	stdout, stderr := &tailBuffer{max: maxCaptured}, &tailBuffer{max: maxCaptured}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	err := cmd.Run()
	res := Result{Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}
	var exit *exec.ExitError
	switch {
	case ctx.Err() != nil:
		return res, ctx.Err()
	case err == nil:
	case errors.As(err, &exit):
		res.Code = exit.ExitCode()
		if res.Code < 0 {
			res.Code = 1
		}
	default:
		return res, err
	}
	return res, nil
}

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	max int
	buf []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = append([]byte(nil), t.buf[len(t.buf)-t.max:]...)
	}
	return len(p), nil
}

func (t *tailBuffer) Bytes() []byte { return bytes.Clone(t.buf) }

// Login is who setup runs as on the server.
type Login struct {
	User string
	Sudo bool
}

const loginScript = `uid=$(id -u) || exit 1
user=$(id -un) || exit 1
if [ "$uid" != 0 ]; then
    sudo -n true || exit 10
fi
printf '%s\n%s\n' "$uid" "$user"
`

// Connect selects whether subsequent scripts require sudo after one login probe.
func Connect(ctx context.Context, r Runner) (Login, error) {
	res, err := r.Run(ctx, loginScript, nil)
	if err != nil {
		return Login{}, fmt.Errorf("could not run commands on %s: %w", r.Target(), err)
	}
	if res.Code == 10 {
		return Login{}, errors.New("the account must be root or able to run sudo without a password")
	}
	if res.Code != 0 {
		return Login{}, fmt.Errorf("could not run commands on %s: %s", r.Target(), failureText(res))
	}
	lines := strings.Split(strings.TrimSuffix(string(res.Stdout), "\n"), "\n")
	if len(lines) != 2 || lines[0] == "" || strings.Trim(lines[0], "0123456789") != "" || lines[1] == "" || len(strings.Fields(lines[1])) != 1 || strings.Fields(lines[1])[0] != lines[1] || clean(lines[1]) != lines[1] {
		return Login{}, fmt.Errorf("could not tell which account runs on %s", r.Target())
	}
	uid, err := strconv.ParseUint(lines[0], 10, 64)
	if err != nil {
		return Login{}, fmt.Errorf("could not tell which account runs on %s", r.Target())
	}
	login := Login{User: lines[1], Sudo: uid != 0}
	if setter, ok := r.(SudoSetter); ok {
		setter.SetSudo(login.Sudo)
	}
	return login, nil
}

// failureText is the last line a failed script printed to stderr, or to stdout when stderr is empty, or its exit code.
func failureText(res Result) string {
	if lines := nonEmptyLines(res.Stderr); len(lines) > 0 {
		return clean(lines[len(lines)-1])
	}
	if lines := nonEmptyLines(res.Stdout); len(lines) > 0 {
		return clean(lines[len(lines)-1])
	}
	return fmt.Sprintf("exit code %d", res.Code)
}

func nonEmptyLines(b []byte) []string {
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

// clean makes text from the server safe to print: control characters become spaces.
func clean(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r >= 0x80 && r < 0xa0 {
			return ' '
		}
		return r
	}, s)
}
