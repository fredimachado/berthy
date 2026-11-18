//go:build unix

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// fakeTUI is a small Bubble Tea program the tests drive: a counter moved by down, text typed in, the size of the terminal, a
// coloured line, and on t it prints the value of BERTHY_FAKE_TOKEN. It exits with code 3 on q.
type fakeTUI struct {
	cursor, w, h int
	typed        string
	leak         bool
}

func (f *fakeTUI) Init() tea.Cmd { return nil }

func (f *fakeTUI) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch m := msg.(type) {
	case tea.WindowSizeMsg:
		f.w, f.h = m.Width, m.Height
	case tea.KeyPressMsg:
		switch s := m.String(); s {
		case "q":
			return f, tea.Quit
		case "down":
			f.cursor++
		case "t":
			f.leak = true
		case "enter", "esc", "tab", "shift+tab", "up", "left", "right":
			f.typed += "<" + s + ">"
		default:
			f.typed += s
		}
	}
	return f, nil
}

func (f *fakeTUI) View() tea.View {
	red := lipgloss.NewStyle().Foreground(lipgloss.Color("#ff0000")).Bold(true).Render("RED BOLD")
	blue := lipgloss.NewStyle().Background(lipgloss.Color("#0000ff")).Foreground(lipgloss.Color("#ffffff")).Render("WHITE ON BLUE")
	lines := []string{"FAKE TUI", fmt.Sprintf("cursor: %d", f.cursor), "typed: " + f.typed, fmt.Sprintf("size: %dx%d", f.w, f.h), red + " " + blue, "wide: 日本語 end"}
	if f.leak {
		lines = append(lines, "token: "+os.Getenv("BERTHY_FAKE_TOKEN"))
	}
	v := tea.NewView(strings.Join(lines, "\n"))
	v.AltScreen = true
	return v
}

// TestFakeTUIProcess is the program the other tests start, in a separate process, through the test binary.
func TestFakeTUIProcess(t *testing.T) {
	if os.Getenv("TUIDRIVER_FAKE") != "1" {
		t.Skip("only runs as the program under test")
	}
	if _, err := tea.NewProgram(&fakeTUI{}).Run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(3)
}

func fakeConfig(t *testing.T, script string) (config, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "steps.json")
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "out")
	return config{
		command: os.Args[0] + " -test.run=^TestFakeTUIProcess$",
		size:    "100x30", script: path, out: out, timeout: 30 * time.Second,
		env: []string{"TUIDRIVER_FAKE=1"},
	}, out
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestDriveWaitKeysTypeResizeSnapshotExit(t *testing.T) {
	c, out := fakeConfig(t, `[
		{"wait_text": "FAKE TUI", "timeout": "15s"},
		{"snapshot": "first"},
		{"keys": ["down", "down"]},
		{"wait_text": "cursor: 2"},
		{"type": "ab"},
		{"keys": ["enter", "tab", "shift+tab", "esc"]},
		{"wait_text": "typed: ab<enter><tab><shift+tab><esc>"},
		{"resize": "80x24"},
		{"wait_text": "size: 80x24"},
		{"sleep": "100ms"},
		{"snapshot": "second"},
		{"keys": ["q"]},
		{"expect_exit": 3}
	]`)
	var stderr bytes.Buffer
	if code := run(c, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	first := read(t, filepath.Join(out, "first.txt"))
	if !strings.Contains(first, "FAKE TUI") || !strings.Contains(first, "cursor: 0") || !strings.Contains(first, "size: 100x30") || !strings.Contains(first, "wide: 日本語 end") {
		t.Fatalf("first.txt:\n%s", first)
	}
	if strings.Contains(first, " \n") {
		t.Fatal("trailing spaces were not trimmed")
	}
	if second := read(t, filepath.Join(out, "second.txt")); !strings.Contains(second, "size: 80x24") {
		t.Fatalf("second.txt:\n%s", second)
	}
	page := read(t, filepath.Join(out, "first.html"))
	for _, want := range []string{"color:#ff0000", "font-weight:bold", "background:#0000ff", "color:#ffffff", "background:#0d1117", "monospace", "RED BOLD"} {
		if !strings.Contains(page, want) {
			t.Errorf("first.html lacks %q", want)
		}
	}
	var results struct {
		OK    bool         `json:"ok"`
		Steps []stepResult `json:"steps"`
	}
	if err := json.Unmarshal([]byte(read(t, filepath.Join(out, "steps.json"))), &results); err != nil {
		t.Fatal(err)
	}
	if !results.OK || len(results.Steps) != 13 {
		t.Fatalf("results %+v", results)
	}
	for _, s := range results.Steps {
		if !s.OK || s.Elapsed == "" {
			t.Errorf("step %+v", s)
		}
	}
}

func TestDriveFailedWaitWritesSnapshotAndFails(t *testing.T) {
	c, out := fakeConfig(t, `[{"wait_text": "FAKE TUI"}, {"wait_text": "never shown", "timeout": "300ms"}, {"snapshot": "unreached"}]`)
	var stderr bytes.Buffer
	if code := run(c, &stderr); code != 1 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(stderr.String(), "timed out") {
		t.Fatalf("stderr %q", stderr.String())
	}
	failed := read(t, filepath.Join(out, "FAILED-1.txt"))
	if !strings.Contains(failed, "never shown") || !strings.Contains(failed, "FAKE TUI") {
		t.Fatalf("FAILED-1.txt:\n%s", failed)
	}
	if _, err := os.Stat(filepath.Join(out, "FAILED-1.html")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(out, "unreached.txt")); err == nil {
		t.Fatal("a step ran after the failure")
	}
	var results struct {
		OK    bool         `json:"ok"`
		Steps []stepResult `json:"steps"`
	}
	if err := json.Unmarshal([]byte(read(t, filepath.Join(out, "steps.json"))), &results); err != nil || results.OK || len(results.Steps) != 2 || results.Steps[1].OK || results.Steps[1].Error == "" {
		t.Fatalf("results %+v %v", results, err)
	}
}

func TestDriveWaitGoneAndExitCodeMismatch(t *testing.T) {
	c, _ := fakeConfig(t, `[{"wait_text": "FAKE TUI"}, {"keys": ["x"]}, {"wait_text": "typed: x"}, {"wait_gone": "no such text"}, {"keys": ["q"]}, {"expect_exit": 0}]`)
	var stderr bytes.Buffer
	if code := run(c, &stderr); code != 1 || !strings.Contains(stderr.String(), "exited with 3, expected 0") {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
}

func TestDriveRedactsTokensAndFailsWhenOneIsOnScreen(t *testing.T) {
	const token = "bty.prod.0123456789abcdef-secret"
	t.Setenv("BERTHY_FAKE_TOKEN", token)
	t.Setenv("OTHER_SECRET_NAME", "other-secret-value-9876")
	c, out := fakeConfig(t, `[{"wait_text": "FAKE TUI"}, {"snapshot": "clean"}, {"keys": ["t"]}, {"sleep": "500ms"}, {"snapshot": "leaky"}]`)
	c.redactEnv = []string{"OTHER_SECRET_NAME"}
	var stderr bytes.Buffer
	if code := run(c, &stderr); code != 1 {
		t.Fatalf("exit %d (a token on the screen must fail the run): %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "secret value appeared") {
		t.Fatalf("stderr %q", stderr.String())
	}
	entries, err := os.ReadDir(out)
	if err != nil || len(entries) == 0 {
		t.Fatalf("no output files: %v", err)
	}
	for _, e := range entries {
		body := read(t, filepath.Join(out, e.Name()))
		if strings.Contains(body, token) || strings.Contains(body, "other-secret-value-9876") {
			t.Errorf("%s holds a secret", e.Name())
		}
	}
	if strings.Contains(stderr.String(), token) {
		t.Error("stderr holds the token")
	}
}

func TestRedactorAppliesToNamedVariables(t *testing.T) {
	r := newRedactor([]string{"BERTHY_ADMIN_TOKEN_X=aaa111", "BERTHY_TOKEN=bbb222", "PATH=/bin", "MINE=ccc333", "EMPTY=", "BERTHY_OTHER=ddd444"}, []string{"MINE", "EMPTY"})
	got := r.apply("aaa111 bbb222 /bin ccc333 ddd444")
	if got != "[redacted] [redacted] /bin [redacted] ddd444" {
		t.Fatalf("%q", got)
	}
	if !r.contains("xx bbb222") || r.contains("clean") {
		t.Fatal("contains")
	}
}

func TestKeyBytes(t *testing.T) {
	for name, want := range map[string]string{
		"enter": "\r", "esc": "\x1b", "tab": "\t", "shift+tab": "\x1b[Z", "up": "\x1b[A", "down": "\x1b[B", "left": "\x1b[D", "right": "\x1b[C",
		"pgup": "\x1b[5~", "pgdown": "\x1b[6~", "home": "\x1b[H", "end": "\x1b[F", "backspace": "\x7f", "space": " ",
		"ctrl+c": "\x03", "ctrl+u": "\x15", "alt+x": "\x1bx", "q": "q", "/": "/", "?": "?", "é": "é",
	} {
		got, err := keyBytes(name)
		if err != nil || string(got) != want {
			t.Errorf("%s: %q %v, want %q", name, got, err, want)
		}
	}
	for _, bad := range []string{"", "nokey", "ctrl+", "ctrl+ab"} {
		if _, err := keyBytes(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestSplitCommand(t *testing.T) {
	got, err := splitCommand(`berthy ui -e "my env" --config 'a b.json' plain\ word`)
	if err != nil || strings.Join(got, "|") != "berthy|ui|-e|my env|--config|a b.json|plain word" {
		t.Fatalf("%v %v", got, err)
	}
	for _, bad := range []string{"", `a "b`, "a\\"} {
		if _, err := splitCommand(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestParseSize(t *testing.T) {
	if c, r, err := parseSize("120x40"); err != nil || c != 120 || r != 40 {
		t.Fatal(c, r, err)
	}
	for _, bad := range []string{"", "120", "ax4", "5x5", "9999x40"} {
		if _, _, err := parseSize(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestInvalidScriptIsRefused(t *testing.T) {
	c, _ := fakeConfig(t, `[{"wait_txt": "typo"}]`)
	var stderr bytes.Buffer
	if code := run(c, &stderr); code != 2 {
		t.Fatalf("exit %d", code)
	}
}

// The screen can be read while the program is still drawing on it: the driver's pump writes the emulator in one goroutine and a
// step reads it in another, and the race detector must see no overlap.
func TestScreenReadWhileTheProgramDraws(t *testing.T) {
	s, err := startSession([]string{"sh", "-c", `i=0; while :; do printf '\033[Hline %d\033[K\n' $i; i=$((i+1)); done`}, nil, 40, 10, &redactor{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.stop()
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		_ = s.screenText()
		_ = s.html("x")
	}
}
