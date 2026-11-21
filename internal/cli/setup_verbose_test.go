package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"berthy/internal/api"
	"berthy/internal/setup"
)

// freshNoDocker is a server with nothing set up. Its probe changes as setup changes the server: no Docker, then Docker with no
// Swarm, then a Swarm with no network.
func freshNoDocker(t *testing.T, target string) *setupServer {
	t.Helper()
	withSetupBuild(t, elfBinary(t))
	dockerOnly := freshProbe + "docker=/usr/bin/docker\ndocker_client_version=29.9.3\ndocker_active=active\ndocker_enabled=enabled\ndocker_version=29.9.3\n"
	return &setupServer{target: target, probes: []string{freshProbe, dockerOnly + "swarm=inactive false\n", dockerOnly + "swarm=active true\n"}}
}

// setupArgs are the arguments of a setup of p5 on ops@s1 that reads no manifest, plus extra.
func setupArgs(t *testing.T, extra ...string) []string {
	t.Helper()
	return append([]string{"-e", "p5", "--host", "ops@s1", "--listen", "172.31.250.10:7710", "--network", "p5net", "--manifest-dir", projectDir(t)}, extra...)
}

// verboseLog is what a verbose log says: the scripts under each step's header, in order, the scripts in the order they were
// printed, and the status line that followed each.
type verboseLog struct {
	byStep   map[string][]string
	all      []string
	statuses []string
}

// parseVerboseLog reads the log strictly: every line is a step header, a script line, or a status line.
func parseVerboseLog(t *testing.T, text string) verboseLog {
	t.Helper()
	got := verboseLog{byStep: map[string][]string{}}
	step := ""
	var cur []string
	open := false
	for _, line := range strings.Split(text, "\n") {
		switch {
		case strings.HasPrefix(line, "# "):
			step = strings.TrimPrefix(line, "# ")
		case line == "$" || strings.HasPrefix(line, "$ "):
			cur = append(cur, strings.TrimPrefix(strings.TrimPrefix(line, "$"), " "))
			open = true
		case strings.HasPrefix(line, "→ "):
			if !open {
				t.Fatalf("a status line with no script before it: %q", line)
			}
			script := strings.Join(cur, "\n")
			got.byStep[step] = append(got.byStep[step], script)
			got.all = append(got.all, script)
			got.statuses = append(got.statuses, strings.TrimPrefix(line, "→ "))
			cur, open = nil, false
		case line == "":
		default:
			t.Fatalf("a line that is neither a header nor a script of the verbose log: %q", line)
		}
	}
	return got
}

// stepGlyphs maps each step of the text output to its glyph: ✓ ok or changed, · would change, – skipped, ✖ failed.
func stepGlyphs(out string) map[string]string {
	glyphs := map[string]bool{"✓": true, "·": true, "–": true, "✖": true}
	got := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if fields := strings.Fields(line); len(fields) >= 2 && glyphs[fields[0]] {
			got[fields[1]] = fields[0]
		}
	}
	return got
}

// The dry run of --verbose lists, for each step that would change the server, the commands a real run sends in that step, and
// it sends the server only reads. On a fresh server there is no command a real run sends that the dry run does not list.
func TestSetupDryRunVerboseListsWhatARealRunSends(t *testing.T) {
	dry := freshNoDocker(t, "ops@s1")
	withSetupRunner(t, dry)
	code, dryOut, dryErr := runSetupCmd(setupArgs(t, "--dry-run", "--verbose")...)
	if code != 0 {
		t.Fatalf("dry run exit %d:\n%s\n%s", code, dryOut, dryErr)
	}
	real := freshNoDocker(t, "ops@s1")
	withSetupRunner(t, real)
	code, _, realErr := runSetupCmd(setupArgs(t, "--verbose")...)
	if code != 0 {
		t.Fatalf("real run exit %d: %s", code, realErr)
	}

	for _, sc := range dry.scripts {
		if sc != "id -u; id -un" && !strings.Contains(sc, "uname -s") {
			t.Errorf("a dry run sent %q", sc)
		}
	}
	dryLog, realLog := parseVerboseLog(t, dryErr), parseVerboseLog(t, realErr)
	if !slices.Contains(dryLog.byStep["connect"], "id -u; id -un") || !slices.Contains(dryLog.byStep["check"], probeScriptOf(dryLog.all)) {
		t.Errorf("the dry run's log does not list the reads it ran:\n%s", dryErr)
	}
	// Every script the real run sends is in the dry run's log.
	for _, sc := range realLog.all {
		if !slices.Contains(dryLog.all, sc) {
			t.Errorf("a real run sends a script the dry run does not list:\n%s", sc)
		}
	}

	// Every step that would change the server, and the skipped verify step, which a real run checks the agent in.
	changing := 0
	for step, glyph := range stepGlyphs(dryOut) {
		if glyph == "·" {
			changing++
		}
		if glyph != "·" && glyph != "–" {
			continue
		}
		if !slices.Equal(sorted(withoutProbes(dryLog.byStep[step])), sorted(withoutProbes(realLog.byStep[step]))) {
			t.Errorf("%s: the dry run lists\n%q\nthe real run sends\n%q", step, dryLog.byStep[step], realLog.byStep[step])
		}
	}
	if changing != 10 {
		t.Fatalf("%d steps would change, want 10:\n%s", changing, dryOut)
	}
}

// probeScriptOf is the probe among scripts, or "" when there is none.
func probeScriptOf(scripts []string) string {
	for _, sc := range scripts {
		if strings.Contains(sc, "uname -s") {
			return sc
		}
	}
	return ""
}

// withoutFinalNewlines is each script without the newline that ends it.
func withoutFinalNewlines(scripts []string) []string {
	out := make([]string, len(scripts))
	for i, sc := range scripts {
		out[i] = strings.TrimSuffix(sc, "\n")
	}
	return out
}

// withoutProbes drops the probes of a step. A step that changed the server probes it again, to read what changed; a dry run
// changes nothing, so it does not probe again, and the probe is listed once, under the check step.
func withoutProbes(scripts []string) []string {
	var out []string
	for _, sc := range scripts {
		if !strings.Contains(sc, "uname -s") {
			out = append(out, sc)
		}
	}
	return out
}

func sorted(s []string) []string {
	out := slices.Clone(s)
	slices.Sort(out)
	return out
}

// Without --verbose nothing goes to standard error; with it, the step lines and the summary on standard output are the same, and
// only the token, which is generated for each server, differs.
func TestSetupVerboseKeepsStdoutAndWritesTheLogToStderr(t *testing.T) {
	plainServer := freshNoDocker(t, "ops@s1")
	withSetupRunner(t, plainServer)
	code, plain, plainErr := runSetupCmd(setupArgs(t)...)
	if code != 0 || plainErr != "" {
		t.Fatalf("plain run: exit %d, stderr %q", code, plainErr)
	}
	verboseServer := freshNoDocker(t, "ops@s1")
	withSetupRunner(t, verboseServer)
	code, verbose, verboseErr := runSetupCmd(setupArgs(t, "--verbose")...)
	if code != 0 || verboseErr == "" {
		t.Fatalf("verbose run: exit %d, stderr %q", code, verboseErr)
	}
	mask := regexp.MustCompile(`bty\.p5\.\S+`)
	if mask.ReplaceAllString(verbose, "<token>") != mask.ReplaceAllString(plain, "<token>") {
		t.Fatalf("stdout changed with --verbose:\n%s\nwant:\n%s", verbose, plain)
	}
}

// Every script the real run sends is in the log once, in the order it was sent, with its exit status after it.
func TestSetupVerboseLogsEachScriptOnceBeforeItsStatus(t *testing.T) {
	server := freshNoDocker(t, "ops@s1")
	withSetupRunner(t, server)
	code, _, errout := runSetupCmd(setupArgs(t, "--verbose")...)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errout)
	}
	log := parseVerboseLog(t, errout)
	if len(log.all) != len(server.scripts) {
		t.Fatalf("the log has %d scripts, the server received %d", len(log.all), len(server.scripts))
	}
	for i, sc := range server.scripts {
		if log.all[i] != strings.TrimSuffix(sc, "\n") {
			t.Errorf("script %d in the log:\n%s\nwhat the server received:\n%s", i, log.all[i], sc)
		}
	}
	for i, st := range log.statuses {
		if st != "exit 0" {
			t.Errorf("status %d = %q", i, st)
		}
	}
}

// The JSON report has the commands of each step, the same ones the log shows under that step; without --verbose it has no
// commands key anywhere.
func TestSetupVerboseJSONHasTheCommandsOfEachStep(t *testing.T) {
	server := freshNoDocker(t, "ops@s1")
	withSetupRunner(t, server)
	code, out, errout := runSetupCmd(setupArgs(t, "--verbose", "--json")...)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errout)
	}
	var got struct {
		Steps []struct {
			Step     string    `json:"step"`
			Commands *[]string `json:"commands"`
		} `json:"steps"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("%v:\n%s", err, out)
	}
	if len(got.Steps) != 13 {
		t.Fatalf("%d steps", len(got.Steps))
	}
	log := parseVerboseLog(t, errout)
	for _, s := range got.Steps {
		if s.Commands == nil {
			t.Errorf("step %s has no commands key", s.Step)
			continue
		}
		// The log prints each script without its final newline.
		if !slices.Equal(withoutFinalNewlines(*s.Commands), log.byStep[s.Step]) {
			t.Errorf("step %s: JSON commands differ from the log:\n%q\n%q", s.Step, *s.Commands, log.byStep[s.Step])
		}
	}
	if docker := got.Steps[2]; docker.Step != "docker" || docker.Commands == nil || len(*docker.Commands) != 2 || !strings.Contains((*docker.Commands)[0], "get.docker.com") {
		t.Errorf("docker step = %+v", docker)
	}

	withSetupRunner(t, freshNoDocker(t, "ops@s1"))
	code, out, errout = runSetupCmd(setupArgs(t, "--json")...)
	if code != 0 || errout != "" {
		t.Fatalf("plain json: exit %d, stderr %q", code, errout)
	}
	var plain struct {
		Steps []map[string]json.RawMessage `json:"steps"`
	}
	if err := json.Unmarshal([]byte(out), &plain); err != nil {
		t.Fatalf("%v:\n%s", err, out)
	}
	for _, step := range plain.Steps {
		if _, ok := step["commands"]; ok {
			t.Errorf("a step without --verbose has commands: %s", step["step"])
		}
	}
}

// distinctiveKey is a 32-byte encryption key that no other value in the tests resembles.
func distinctiveKey() []byte {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(0x90 + 7*i)
	}
	return key
}

// keyForms are the key as text could hold it: its bytes, in hex, and in each base64 alphabet.
func keyForms(key []byte) []string {
	return []string{string(key), hex.EncodeToString(key), base64.StdEncoding.EncodeToString(key), base64.RawStdEncoding.EncodeToString(key),
		base64.URLEncoding.EncodeToString(key), base64.RawURLEncoding.EncodeToString(key)}
}

// assertAbsent fails when text holds one of the secrets.
func assertAbsent(t *testing.T, where, text string, secrets []string) {
	t.Helper()
	for _, s := range secrets {
		if strings.Contains(text, s) {
			t.Errorf("%s holds a secret", where)
		}
	}
}

// printedToken is the admin token the text output shows, or "" when it shows none.
func printedToken(out string) string {
	if m := regexp.MustCompile(`admin token for \S+ \(shown once, store it now\): (bty\.\S+)`).FindStringSubmatch(out); m != nil {
		return m[1]
	}
	return ""
}

// The verbose log, the JSON report and the scripts hold neither the token nor the encryption key, in any form. The token of an
// install is printed once on standard output, as it always is; the key is never printed.
func TestSetupVerboseNeverShowsTheTokenOrTheKey(t *testing.T) {
	key := distinctiveKey()
	keyFile := filepath.Join(t.TempDir(), "saved.key")
	if err := os.WriteFile(keyFile, key, 0o600); err != nil {
		t.Fatal(err)
	}
	keySecrets := keyForms(key)
	const envToken = "bty.p5.never-in-verbose-output-7f3a9c"

	t.Run("install with a key file", func(t *testing.T) {
		server := freshNoDocker(t, "ops@s1")
		withSetupRunner(t, server)
		code, out, errout := runSetupCmd(setupArgs(t, "--verbose", "--key-file", keyFile)...)
		if code != 0 {
			t.Fatalf("exit %d: %s", code, errout)
		}
		token := printedToken(out)
		if token == "" || strings.Count(out, token) != 1 {
			t.Fatalf("the token is not printed once on stdout:\n%s", out)
		}
		assertAbsent(t, "the verbose log", errout, append(keySecrets, token))
		assertAbsent(t, "stdout", out, keySecrets)
		for _, sc := range server.scripts {
			assertAbsent(t, "a script", sc, append(keySecrets, token))
		}
		if !bytes.Contains(server.stdins[indexOfScript(t, server, "sha256sum /var/lib/berthy/key")], key) {
			t.Fatal("the key did not reach the server on stdin: the test proves nothing")
		}
	})

	t.Run("install with a key file, as JSON", func(t *testing.T) {
		withSetupRunner(t, freshNoDocker(t, "ops@s1"))
		code, out, errout := runSetupCmd(setupArgs(t, "--verbose", "--json", "--key-file", keyFile)...)
		var got setupReport
		if code != 0 || json.Unmarshal([]byte(out), &got) != nil || got.Token == "" {
			t.Fatalf("exit %d:\n%s", code, out)
		}
		if strings.Count(out, got.Token) != 1 {
			t.Errorf("the token is not in the JSON report once: %d times", strings.Count(out, got.Token))
		}
		assertAbsent(t, "the verbose log", errout, append(keySecrets, got.Token))
		assertAbsent(t, "the JSON report", out, keySecrets)
	})

	t.Run("dry run with a key file and a token in the environment", func(t *testing.T) {
		server := freshNoDocker(t, "ops@s1")
		withSetupRunner(t, server)
		t.Setenv("BERTHY_P5_TOKEN", envToken)
		code, out, errout := runSetupCmd(setupArgs(t, "--dry-run", "--verbose", "--key-file", keyFile)...)
		if code != 0 {
			t.Fatalf("exit %d: %s", code, errout)
		}
		assertAbsent(t, "stdout", out, append(keySecrets, envToken))
		assertAbsent(t, "the verbose log", errout, append(keySecrets, envToken))
		for _, sc := range server.scripts {
			assertAbsent(t, "a script", sc, append(keySecrets, envToken))
		}
	})

	t.Run("the token of the environment goes to the server on stdin only", func(t *testing.T) {
		server := installedServer(t, "ops@s1")
		withSetupRunner(t, server)
		setupDoctor = func(context.Context, string, string, string) ([]api.Check, error) { return nil, setup.ErrUnreachable }
		t.Setenv("BERTHY_P5_TOKEN", envToken)
		code, out, errout := runSetupCmd(setupArgs(t, "--verbose")...)
		if code != 0 {
			t.Fatalf("exit %d: %s", code, errout)
		}
		for _, sc := range server.scripts {
			assertAbsent(t, "a script", sc, []string{envToken})
		}
		sent := false
		for _, in := range server.stdins {
			sent = sent || bytes.Contains(in, []byte(envToken))
		}
		if !sent {
			t.Fatal("the token did not reach the server on stdin: the test proves nothing")
		}
		assertAbsent(t, "stdout", out, []string{envToken})
		assertAbsent(t, "the verbose log", errout, []string{envToken})
	})
}

// indexOfScript is the index of the first script the server received that contains marker.
func indexOfScript(t *testing.T, server *setupServer, marker string) int {
	t.Helper()
	for i, sc := range server.scripts {
		if strings.Contains(sc, marker) {
			return i
		}
	}
	t.Fatalf("no script with %q", marker)
	return 0
}
