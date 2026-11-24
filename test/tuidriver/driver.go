//go:build unix

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/charmbracelet/x/vt"
	"github.com/creack/pty"
)

// step is one line of the script. Exactly one of the action fields is set; Timeout limits a wait.
type step struct {
	WaitText   string   `json:"wait_text,omitempty"`
	WaitGone   string   `json:"wait_gone,omitempty"`
	Timeout    string   `json:"timeout,omitempty"`
	Keys       []string `json:"keys,omitempty"`
	Type       string   `json:"type,omitempty"`
	Resize     string   `json:"resize,omitempty"`
	Sleep      string   `json:"sleep,omitempty"`
	Snapshot   string   `json:"snapshot,omitempty"`
	ExpectExit *int     `json:"expect_exit,omitempty"`
}

// describe names a step for the results.
func (s step) describe() string {
	switch {
	case s.WaitText != "":
		return "wait_text " + strconv.Quote(s.WaitText)
	case s.WaitGone != "":
		return "wait_gone " + strconv.Quote(s.WaitGone)
	case len(s.Keys) > 0:
		return "keys " + strings.Join(s.Keys, " ")
	case s.Type != "":
		return "type"
	case s.Resize != "":
		return "resize " + s.Resize
	case s.Sleep != "":
		return "sleep " + s.Sleep
	case s.Snapshot != "":
		return "snapshot " + s.Snapshot
	case s.ExpectExit != nil:
		return "expect_exit " + strconv.Itoa(*s.ExpectExit)
	}
	return "empty step"
}

// stepResult is what the results file says of a step.
type stepResult struct {
	Index   int    `json:"index"`
	Step    string `json:"step"`
	OK      bool   `json:"ok"`
	Error   string `json:"error,omitempty"`
	Elapsed string `json:"elapsed"`
}

// redactor replaces the values of secrets in what the driver writes, and finds them in what the program prints.
type redactor struct{ values []string }

func (r *redactor) apply(s string) string {
	for _, v := range r.values {
		s = strings.ReplaceAll(s, v, "[redacted]")
	}
	return s
}

func (r *redactor) contains(s string) bool {
	for _, v := range r.values {
		if strings.Contains(s, v) {
			return true
		}
	}
	return false
}

func (r *redactor) longest() int {
	n := 0
	for _, v := range r.values {
		n = max(n, len(v))
	}
	return n
}

var tokenVar = regexp.MustCompile(`^BERTHY_.*TOKEN.*$`)

// newRedactor collects the values of every BERTHY_*TOKEN* variable and of the named variables.
// BERTHY_TOKEN_STORE is a typed store selector, not a secret; automatic matching skips it, but
// an explicit name in names (--redact-env) still includes its value.
func newRedactor(environ, names []string) *redactor {
	want := map[string]bool{}
	for _, n := range names {
		want[n] = true
	}
	r := &redactor{}
	seen := map[string]bool{}
	for _, kv := range environ {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || v == "" || seen[v] {
			continue
		}
		if want[k] || (tokenVar.MatchString(k) && k != "BERTHY_TOKEN_STORE") {
			seen[v] = true
			r.values = append(r.values, v)
		}
	}
	return r
}

// session is a program running in a pseudo-terminal, with an emulated screen that shows what it drew.
type session struct {
	cmd    *exec.Cmd
	ptmx   *os.File
	em     *vt.SafeEmulator
	rd     *redactor
	exited chan struct{}

	// screenMu orders the writers of the emulator (the pump, resize) and the copy of its cells that a reader takes. The
	// emulator locks each call on its own, but hands out pointers to its cells, which a reader would use after the lock.
	screenMu sync.Mutex

	mu       sync.Mutex
	exitCode int
	leaked   bool
	tail     []byte
}

// splitCommand splits a command line into words, with single and double quotes and backslashes as a shell does.
func splitCommand(s string) ([]string, error) {
	var words []string
	var cur strings.Builder
	inWord := false
	var quote rune
	escaped := false
	for _, r := range s {
		switch {
		case escaped:
			cur.WriteRune(r)
			escaped = false
		case r == '\\' && quote != '\'':
			escaped = true
			inWord = true
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote = r
			inWord = true
		case r == ' ' || r == '\t' || r == '\n':
			if inWord {
				words = append(words, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteRune(r)
			inWord = true
		}
	}
	if quote != 0 || escaped {
		return nil, errors.New("unterminated quote in command")
	}
	if inWord {
		words = append(words, cur.String())
	}
	if len(words) == 0 {
		return nil, errors.New("empty command")
	}
	return words, nil
}

func parseSize(s string) (cols, rows int, err error) {
	w, h, ok := strings.Cut(strings.ToLower(s), "x")
	if !ok {
		return 0, 0, fmt.Errorf("size %q is not COLSxROWS", s)
	}
	cols, err1 := strconv.Atoi(w)
	rows, err2 := strconv.Atoi(h)
	if err1 != nil || err2 != nil || cols < 10 || rows < 3 || cols > 1000 || rows > 1000 {
		return 0, 0, fmt.Errorf("size %q is not COLSxROWS", s)
	}
	return cols, rows, nil
}

// startSession runs argv in a pseudo-terminal of the given size.
func startSession(argv, extraEnv []string, cols, rows int, rd *redactor) (*session, error) {
	cmd := exec.Command(argv[0], argv[1:]...)
	env := make([]string, 0, len(os.Environ())+len(extraEnv)+3)
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "TERM=") || strings.HasPrefix(kv, "COLORTERM=") || strings.HasPrefix(kv, "NO_COLOR=") {
			continue
		}
		env = append(env, kv)
	}
	env = append(env, "TERM=xterm-256color", "COLORTERM=truecolor")
	env = append(env, extraEnv...)
	cmd.Env = env
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
	if err != nil {
		return nil, err
	}
	s := &session{cmd: cmd, ptmx: ptmx, em: vt.NewSafeEmulator(cols, rows), rd: rd, exited: make(chan struct{})}
	// What the emulator answers to the program's questions (cursor position, device attributes) goes back to the program.
	go func() { _, _ = io.Copy(ptmx, s.em) }()
	go s.pump()
	go func() {
		err := cmd.Wait()
		code := 0
		var ee *exec.ExitError
		switch {
		case errors.As(err, &ee):
			code = ee.ExitCode()
			if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
				code = 128 + int(ws.Signal())
			}
		case err != nil:
			code = -1
		}
		s.mu.Lock()
		s.exitCode = code
		s.mu.Unlock()
		close(s.exited)
	}()
	return s, nil
}

// pump feeds what the program writes to the emulated screen and looks for secrets in it.
func (s *session) pump() {
	buf := make([]byte, 32<<10)
	for {
		n, err := s.ptmx.Read(buf)
		if n > 0 {
			s.scan(buf[:n])
			s.screenMu.Lock()
			_, _ = s.em.Write(buf[:n])
			s.screenMu.Unlock()
		}
		if err != nil {
			return
		}
	}
}

// scan marks the session as leaking when a secret is in the output, even when it is cleared from the screen at once. It keeps
// the end of the previous chunk, so a secret split between two reads is found too.
func (s *session) scan(chunk []byte) {
	if len(s.rd.values) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	data := append(append([]byte(nil), s.tail...), chunk...)
	if s.rd.contains(string(data)) {
		s.leaked = true
	}
	keep := min(s.rd.longest(), len(data))
	s.tail = append(s.tail[:0], data[len(data)-keep:]...)
}

func (s *session) hasLeaked() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.leaked
}

// text is the screen as plain lines.
func (s *session) text() []string { return plainText(s.grab()) }

// html is the screen as a page.
func (s *session) html(title string) string { return renderHTML(s.grab(), title) }

// grab copies the screen, so that it can be read while the program goes on drawing.
func (s *session) grab() *grid {
	s.screenMu.Lock()
	defer s.screenMu.Unlock()
	return copyScreen(s.em)
}

// screenText is the screen as one string.
func (s *session) screenText() string { return strings.Join(s.text(), "\n") }

func (s *session) send(b []byte) error {
	_, err := s.ptmx.Write(b)
	return err
}

func (s *session) resize(cols, rows int) error {
	if err := pty.Setsize(s.ptmx, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)}); err != nil {
		return err
	}
	s.screenMu.Lock()
	s.em.Resize(cols, rows)
	s.screenMu.Unlock()
	return nil
}

func (s *session) running() bool {
	select {
	case <-s.exited:
		return false
	default:
		return true
	}
}

// stop ends the program if it is still running and releases the terminal.
func (s *session) stop() {
	if s.running() {
		_ = s.cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-s.exited:
		case <-time.After(3 * time.Second):
			_ = s.cmd.Process.Kill()
			<-s.exited
		}
	}
	_ = s.ptmx.Close()
	// Closing the emulator itself races with the goroutine that reads its answers; closing its pipe ends that goroutine.
	if c, ok := s.em.InputPipe().(io.Closer); ok {
		_ = c.Close()
	}
}

// runner runs a script against a session and writes what it finds to a directory.
type runner struct {
	s    *session
	out  string
	rd   *redactor
	logf func(string, ...any)
}

func (r *runner) write(name, content string) error {
	return os.WriteFile(filepath.Join(r.out, name), []byte(r.rd.apply(content)), 0o600)
}

// snapshot writes the screen as NAME.txt and NAME.html.
func (r *runner) snapshot(name string) error {
	if name == "" || strings.ContainsAny(name, `/\`) || strings.HasPrefix(name, ".") {
		return fmt.Errorf("invalid snapshot name %q", name)
	}
	txt := strings.Join(r.s.text(), "\n") + "\n"
	if err := r.write(name+".txt", txt); err != nil {
		return err
	}
	return r.write(name+".html", r.s.html(name))
}

// wait polls the screen until cond holds.
func (r *runner) wait(ctx context.Context, timeout time.Duration, desc string, cond func(string) bool) error {
	deadline := time.Now().Add(timeout)
	for {
		if r.s.hasLeaked() {
			return errors.New("a secret value appeared in the program's output")
		}
		if cond(r.s.screenText()) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out after %s waiting for %s", timeout, desc)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
}

func stepTimeout(s step, def time.Duration) (time.Duration, error) {
	if s.Timeout == "" {
		return def, nil
	}
	return time.ParseDuration(s.Timeout)
}

// do runs one step.
func (r *runner) do(ctx context.Context, s step) error {
	switch {
	case s.WaitText != "":
		d, err := stepTimeout(s, 10*time.Second)
		if err != nil {
			return err
		}
		return r.wait(ctx, d, "text "+strconv.Quote(s.WaitText), func(t string) bool { return strings.Contains(t, s.WaitText) })
	case s.WaitGone != "":
		d, err := stepTimeout(s, 10*time.Second)
		if err != nil {
			return err
		}
		return r.wait(ctx, d, "text "+strconv.Quote(s.WaitGone)+" to go", func(t string) bool { return !strings.Contains(t, s.WaitGone) })
	case len(s.Keys) > 0:
		for _, k := range s.Keys {
			b, err := keyBytes(k)
			if err != nil {
				return err
			}
			if err = r.s.send(b); err != nil {
				return err
			}
			// A short pause between keys: the program sees them one at a time, as from a person. After Esc it is longer, so
			// that the next key is not read as an Alt combination or part of an escape sequence.
			if b[0] == 0x1b && len(b) == 1 {
				time.Sleep(200 * time.Millisecond)
			} else {
				time.Sleep(30 * time.Millisecond)
			}
		}
		return nil
	case s.Type != "":
		for _, c := range s.Type {
			if err := r.s.send([]byte(string(c))); err != nil {
				return err
			}
			time.Sleep(10 * time.Millisecond)
		}
		return nil
	case s.Resize != "":
		cols, rows, err := parseSize(s.Resize)
		if err != nil {
			return err
		}
		return r.s.resize(cols, rows)
	case s.Sleep != "":
		d, err := time.ParseDuration(s.Sleep)
		if err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(d):
		}
		return nil
	case s.Snapshot != "":
		return r.snapshot(s.Snapshot)
	case s.ExpectExit != nil:
		d, err := stepTimeout(s, 10*time.Second)
		if err != nil {
			return err
		}
		select {
		case <-r.s.exited:
		case <-time.After(d):
			return fmt.Errorf("the program did not exit within %s", d)
		case <-ctx.Done():
			return ctx.Err()
		}
		r.s.mu.Lock()
		code := r.s.exitCode
		r.s.mu.Unlock()
		if code != *s.ExpectExit {
			return fmt.Errorf("the program exited with %d, expected %d", code, *s.ExpectExit)
		}
		return nil
	}
	return errors.New("a step needs one of wait_text, wait_gone, keys, type, resize, sleep, snapshot or expect_exit")
}

// config is what the command line asks for.
type config struct {
	command    string
	size       string
	script     string
	out        string
	timeout    time.Duration
	redactEnv  []string
	env        []string
	progressTo io.Writer
}

// run runs the script and returns the exit code of the driver: 0 when every step worked, 1 when a step failed or a secret was on
// the screen, 2 for a bad invocation.
func run(c config, stderr io.Writer) int {
	argv, err := splitCommand(c.command)
	if err != nil {
		fmt.Fprintln(stderr, "tuidriver:", err)
		return 2
	}
	cols, rows, err := parseSize(c.size)
	if err != nil {
		fmt.Fprintln(stderr, "tuidriver:", err)
		return 2
	}
	raw, err := os.ReadFile(c.script)
	if err != nil {
		fmt.Fprintln(stderr, "tuidriver: cannot read the script")
		return 2
	}
	var steps []step
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err = dec.Decode(&steps); err != nil {
		fmt.Fprintln(stderr, "tuidriver: invalid script:", err)
		return 2
	}
	if err = os.MkdirAll(c.out, 0o700); err != nil {
		fmt.Fprintln(stderr, "tuidriver: cannot create the output directory")
		return 2
	}
	rd := newRedactor(os.Environ(), c.redactEnv)
	s, err := startSession(argv, c.env, cols, rows, rd)
	if err != nil {
		fmt.Fprintln(stderr, "tuidriver: cannot start the program:", rd.apply(err.Error()))
		return 1
	}
	defer s.stop()
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()
	r := &runner{s: s, out: c.out, rd: rd}
	results := make([]stepResult, 0, len(steps))
	code := 0
	for i, st := range steps {
		start := time.Now()
		err := r.do(ctx, st)
		if err == nil && s.hasLeaked() {
			err = errors.New("a secret value appeared in the program's output")
		}
		res := stepResult{Index: i, Step: st.describe(), OK: err == nil, Elapsed: time.Since(start).Round(time.Millisecond).String()}
		if err != nil {
			res.Error = rd.apply(err.Error())
			name := fmt.Sprintf("FAILED-%d", i)
			_ = r.write(name+".txt", res.Step+": "+res.Error+"\n\n"+strings.Join(s.text(), "\n")+"\n")
			_ = r.write(name+".html", s.html(name))
			fmt.Fprintf(stderr, "tuidriver: step %d (%s) failed: %s\n", i, res.Step, res.Error)
			code = 1
		}
		results = append(results, res)
		if err != nil {
			break
		}
	}
	name := "steps.json"
	if abs1, e1 := filepath.Abs(c.script); e1 == nil {
		if abs2, e2 := filepath.Abs(filepath.Join(c.out, name)); e2 == nil && abs1 == abs2 {
			name = "steps-result.json"
		}
	}
	if b, err := json.MarshalIndent(map[string]any{"ok": code == 0, "steps": results}, "", "  "); err == nil {
		_ = r.write(name, string(b)+"\n")
	}
	return code
}
