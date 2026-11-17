package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"berthy/internal/api"
)

// replyingAgent records each request as its method, path and body, and answers every request with status and reply.
func replyingAgent(bodies *[]string, status int, reply string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		*bodies = append(*bodies, r.Method+" "+r.URL.Path+" "+string(data))
		if status == http.StatusAccepted {
			w.Header().Set("Berthy-Approval", "pending")
		}
		w.WriteHeader(status)
		fmt.Fprint(w, reply)
	}))
}

// importLines returns a file of n NAME=value lines: S01=value-01, S02=value-02 and so on.
func importLines(n int) string {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "S%02d=value-%02d\n", i, i)
	}
	return b.String()
}

// withStdinFile replaces standard input with a file that holds input. withStdin writes its input into a pipe before the command
// reads it, so an input larger than the pipe's buffer would block the test; a file has no such limit.
func withStdinFile(t *testing.T, input string) {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "stdin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(input); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	old := os.Stdin
	os.Stdin = f
	t.Cleanup(func() {
		os.Stdin = old
		f.Close()
	})
}

// stubStdinTerminal makes standard input a terminal for the rest of the test.
func stubStdinTerminal(t *testing.T) {
	t.Helper()
	old := stdinTerminal
	stdinTerminal = func() bool { return true }
	t.Cleanup(func() { stdinTerminal = old })
}

func TestParseSecretLines(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input string
		want  []api.SecretEntry
	}{
		{"names and values", "A=1\nB=two\n", []api.SecretEntry{{Name: "A", Value: "1"}, {Name: "B", Value: "two"}}},
		{"comments and blank lines are skipped", "# header\n\nA=1\n   \n# B=2\nC=3\n", []api.SecretEntry{{Name: "A", Value: "1"}, {Name: "C", Value: "3"}}},
		{"equals signs in the value", "URL=postgres://u:p@h/db?x=1&y==2\n", []api.SecretEntry{{Name: "URL", Value: "postgres://u:p@h/db?x=1&y==2"}}},
		{"one trailing carriage return is removed", "A=value\r\nB=x\r\n", []api.SecretEntry{{Name: "A", Value: "value"}, {Name: "B", Value: "x"}}},
		{"a value keeps its spaces", "A= spaced value \n", []api.SecretEntry{{Name: "A", Value: " spaced value "}}},
		{"an empty value", "A=\n", []api.SecretEntry{{Name: "A", Value: ""}}},
		{"a last line without a newline", "A=1\nB=2", []api.SecretEntry{{Name: "A", Value: "1"}, {Name: "B", Value: "2"}}},
		{"nothing at all", "", nil},
	} {
		got, err := parseSecretLines(strings.NewReader(tc.input))
		if err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: got %+v, %v; want %+v", tc.name, got, err, tc.want)
		}
	}
}

func TestParseSecretLinesRefusesALineByItsNumberAndNeverEchoesIt(t *testing.T) {
	for _, tc := range []struct {
		name, input, want string
	}{
		{"no equals sign", "A=1\nB=2\nSECRET-CANARY-LINE\n", "line 3: expected NAME=value"},
		{"a name that is not a secret name", "A=1\n1BAD=canary-value\n", "line 2: the name is not a valid secret name"},
		{"a name with a space", "OK=1\nBAD NAME=canary-value\n", "line 2: the name is not a valid secret name"},
		{"the tunnel token", "TUNNEL_TOKEN=canary-value\n", "line 1: TUNNEL_TOKEN is reserved for managed ingress"},
		{"an empty name", "A=1\n=canary-value\n", "line 2: the name is not a valid secret name"},
		{"a line longer than a request", "A=" + strings.Repeat("x", 1<<20) + "\n", "line 1: longer than 1048576 bytes"},
	} {
		_, err := parseSecretLines(strings.NewReader(tc.input))
		if err == nil || !strings.HasPrefix(err.Error(), tc.want) {
			t.Errorf("%s: err %v, want %q", tc.name, err, tc.want)
			continue
		}
		if strings.Contains(err.Error(), "canary") || strings.Contains(err.Error(), "SECRET-CANARY") {
			t.Errorf("%s: the error echoes the line: %q", tc.name, err)
		}
	}
}

// TestSecretImportOfTwentyLinesIsOneRequest checks the command the issue asks for: a file of twenty lines, with a comment and a
// blank line, is one request that carries the twenty secrets.
func TestSecretImportOfTwentyLinesIsOneRequest(t *testing.T) {
	var bodies []string
	server := replyingAgent(&bodies, http.StatusOK, `{"created":["S01","S02"],"replaced":[]}`)
	defer server.Close()
	cfg := ingressCLIConfig(t, server.URL)
	withStdin(t, "# the twenty secrets\n\n"+importLines(20))
	var out, errout bytes.Buffer
	if code := Run([]string{"secret", "import", "-e", "dev", "--config", cfg}, &out, &errout); code != 0 {
		t.Fatalf("exit %d: %s", code, errout.String())
	}
	if len(bodies) != 1 || !strings.HasPrefix(bodies[0], "POST /v1/environments/dev/secrets/import ") {
		t.Fatalf("requests %q, want one POST to secrets/import", bodies)
	}
	var sent api.SecretImport
	if err := json.Unmarshal([]byte(strings.SplitN(bodies[0], " ", 3)[2]), &sent); err != nil {
		t.Fatal(err)
	}
	if len(sent.Secrets) != 20 || sent.Secrets[0] != (api.SecretEntry{Name: "S01", Value: "value-01"}) || sent.Secrets[19].Name != "S20" || sent.DryRun {
		t.Fatalf("sent %+v", sent)
	}
	if out.String() != "created: S01, S02\n" {
		t.Fatalf("stdout %q", out.String())
	}
}

func TestSecretImportWithABadLineSendsNothing(t *testing.T) {
	var bodies []string
	server := replyingAgent(&bodies, http.StatusOK, `{}`)
	defer server.Close()
	cfg := ingressCLIConfig(t, server.URL)
	withStdin(t, importLines(6)+"NOT A PAIR\n"+importLines(3))
	var out, errout bytes.Buffer
	code := Run([]string{"secret", "import", "-e", "dev", "--config", cfg}, &out, &errout)
	if code != 1 || errout.String() != "line 7: expected NAME=value\n" || len(bodies) != 0 {
		t.Fatalf("exit %d stderr %q requests %q", code, errout.String(), bodies)
	}
}

func TestSecretImportDryRunPrintsNamesAndNoValue(t *testing.T) {
	var bodies []string
	server := replyingAgent(&bodies, http.StatusOK, `{"action":"secret import","create":["S01","S02"],"replace":["S03"]}`)
	defer server.Close()
	cfg := ingressCLIConfig(t, server.URL)
	withStdin(t, "S01=value-01-canary\nS02=value-02-canary\nS03=value-03-canary\n")
	var out, errout bytes.Buffer
	if code := Run([]string{"secret", "import", "--dry-run", "-e", "dev", "--config", cfg}, &out, &errout); code != 0 {
		t.Fatalf("exit %d: %s", code, errout.String())
	}
	if out.String() != "would create: S01, S02\nwould replace: S03\n" {
		t.Fatalf("stdout %q", out.String())
	}
	if strings.Contains(out.String()+errout.String(), "canary") || !strings.Contains(bodies[0], `"dry_run":true`) {
		t.Fatalf("the dry run printed a value or was not sent as a dry run: %q %q %q", out.String(), errout.String(), bodies)
	}
}

// TestSecretImportWithJSONPrintsTheAnswer checks that --json prints the agent's answer as it is.
func TestSecretImportWithJSONPrintsTheAnswer(t *testing.T) {
	var bodies []string
	server := replyingAgent(&bodies, http.StatusOK, `{"created":["S01"],"replaced":[]}`)
	defer server.Close()
	cfg := ingressCLIConfig(t, server.URL)
	withStdin(t, "S01=v\n")
	var out, errout bytes.Buffer
	if code := Run([]string{"secret", "import", "--json", "-e", "dev", "--config", cfg}, &out, &errout); code != 0 {
		t.Fatalf("exit %d: %s", code, errout.String())
	}
	if !json.Valid(out.Bytes()) || !strings.Contains(out.String(), `"created"`) || strings.Contains(out.String(), "created: ") {
		t.Fatalf("stdout %q, want the JSON answer", out.String())
	}
}

// TestSecretImportHeldForApprovalExitsHeld checks that an agent token's import is held: the command exits with the held code
// and prints the plan, which names the secrets and no value.
func TestSecretImportHeldForApprovalExitsHeld(t *testing.T) {
	var bodies []string
	server := replyingAgent(&bodies, http.StatusAccepted, `{"approval_id":"ap1","expires":"2026-11-18T12:00:00Z","plan":{"action":"secret import","create":["S01"],"replace":[]}}`)
	defer server.Close()
	cfg := ingressCLIConfig(t, server.URL)
	withStdin(t, "S01=value-01-canary\n")
	var out, errout bytes.Buffer
	if code := Run([]string{"secret", "import", "-e", "dev", "--config", cfg}, &out, &errout); code != exitHeld {
		t.Fatalf("exit %d, want %d: %s", code, exitHeld, errout.String())
	}
	if !strings.Contains(out.String(), "held for approval: ap1") || !strings.Contains(out.String(), "S01") || strings.Contains(out.String(), "canary") {
		t.Fatalf("stdout %q", out.String())
	}
}

// TestSecretImportTooLargeForOneRequestIsRefusedBeforeSending checks that an import whose request would not fit the agent's
// limit is refused, with the advice to split it, and nothing is sent.
func TestSecretImportTooLargeForOneRequestIsRefusedBeforeSending(t *testing.T) {
	var bodies []string
	server := replyingAgent(&bodies, http.StatusOK, `{}`)
	defer server.Close()
	cfg := ingressCLIConfig(t, server.URL)
	var file strings.Builder
	for i := 1; i <= 20; i++ {
		// Twenty values of 60000 bytes each: every value is within the limit, and the request is about 1.2 MB.
		fmt.Fprintf(&file, "BIG_%02d=%s\n", i, strings.Repeat("x", 60000))
	}
	withStdinFile(t, file.String())
	var out, errout bytes.Buffer
	code := Run([]string{"secret", "import", "-e", "dev", "--config", cfg}, &out, &errout)
	if code != 1 || !strings.Contains(errout.String(), "the import is too large: split it") || len(bodies) != 0 {
		t.Fatalf("exit %d stderr %q requests %d", code, errout.String(), len(bodies))
	}
}

// TestSecretImportAtATerminalIsRefused checks that the import needs a pipe: at a terminal nothing is read or sent.
func TestSecretImportAtATerminalIsRefused(t *testing.T) {
	stubStdinTerminal(t)
	var bodies []string
	server := replyingAgent(&bodies, http.StatusOK, `{}`)
	defer server.Close()
	cfg := ingressCLIConfig(t, server.URL)
	var out, errout bytes.Buffer
	code := Run([]string{"secret", "import", "-e", "dev", "--config", cfg}, &out, &errout)
	if code != 2 || !strings.Contains(errout.String(), "pipe NAME=value lines into secret import") || len(bodies) != 0 {
		t.Fatalf("exit %d stderr %q requests %q", code, errout.String(), bodies)
	}
}

// secretListManifest names two secrets, DATABASE_URL (from MISSING_DB, which is not stored) and API_DB (which is).
const secretListManifest = `project: demo
network: demo
apps:
  api:
    kind: image
    image: ghcr.io/acme/api
    roles:
      web:
        port: 8080
    env:
      secret:
        - DATABASE_URL: MISSING_DB
        - API_DB
`

// TestSecretListWithManifestDirPrintsTheMissingNames checks that a secret the manifest needs and that is not stored is named
// after the list, with what needs it.
func TestSecretListWithManifestDirPrintsTheMissingNames(t *testing.T) {
	var bodies []string
	server := answeringAgent(&bodies, `[{"name":"API_DB","registry":""}]`, "")
	defer server.Close()
	cfg := ingressCLIConfig(t, server.URL)
	var out, errout bytes.Buffer
	args := []string{"secret", "list", "-e", "dev", "--config", cfg, "--manifest-dir", secretManifestDir(t, secretListManifest)}
	if code := Run(args, &out, &errout); code != 0 {
		t.Fatalf("exit %d: %s", code, errout.String())
	}
	if !strings.Contains(out.String(), "name: API_DB") || !strings.Contains(out.String(), "missing: MISSING_DB (needed by app api)\n") || strings.Contains(out.String(), "missing: API_DB") {
		t.Fatalf("stdout %q", out.String())
	}
}

// TestSecretListWithManifestDirAndJSONIsAnObjectWithMissing checks the JSON shape with --manifest-dir: one object with the
// stored secrets and the missing ones.
func TestSecretListWithManifestDirAndJSONIsAnObjectWithMissing(t *testing.T) {
	var bodies []string
	server := answeringAgent(&bodies, `[{"name":"API_DB","registry":""}]`, "")
	defer server.Close()
	cfg := ingressCLIConfig(t, server.URL)
	var out, errout bytes.Buffer
	args := []string{"secret", "list", "--json", "-e", "dev", "--config", cfg, "--manifest-dir", secretManifestDir(t, secretListManifest)}
	if code := Run(args, &out, &errout); code != 0 {
		t.Fatalf("exit %d: %s", code, errout.String())
	}
	var got struct {
		Secrets []storedSecret `json:"secrets"`
		Missing []struct {
			Name     string   `json:"name"`
			NeededBy []string `json:"needed_by"`
		} `json:"missing"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("stdout is not JSON: %v: %q", err, out.String())
	}
	if len(got.Secrets) != 1 || got.Secrets[0].Name != "API_DB" || len(got.Missing) != 1 || got.Missing[0].Name != "MISSING_DB" || !reflect.DeepEqual(got.Missing[0].NeededBy, []string{"app api"}) {
		t.Fatalf("got %+v", got)
	}
}

// TestSecretListWithoutManifestDirIsUnchanged checks that the list without --manifest-dir prints what it did: the stored
// secrets, and as a JSON array.
func TestSecretListWithoutManifestDirIsUnchanged(t *testing.T) {
	var bodies []string
	server := answeringAgent(&bodies, `[{"name":"API_DB","registry":""}]`, "")
	defer server.Close()
	cfg := ingressCLIConfig(t, server.URL)
	var out, errout bytes.Buffer
	if code := Run([]string{"secret", "list", "-e", "dev", "--config", cfg}, &out, &errout); code != 0 {
		t.Fatalf("exit %d: %s", code, errout.String())
	}
	if out.String() != "name: API_DB\nregistry: \n\n" || strings.Contains(out.String(), "missing") {
		t.Fatalf("stdout %q", out.String())
	}
	out.Reset()
	if code := Run([]string{"secret", "list", "--json", "-e", "dev", "--config", cfg}, &out, &errout); code != 0 || !strings.HasPrefix(strings.TrimSpace(out.String()), "[") {
		t.Fatalf("json exit %d stdout %q", code, out.String())
	}
}

// TestSecretListWithManifestDirSaysWhenNothingIsMissing checks the line that says every needed secret is stored.
func TestSecretListWithManifestDirSaysWhenNothingIsMissing(t *testing.T) {
	var bodies []string
	server := answeringAgent(&bodies, `[{"name":"MISSING_DB","registry":""},{"name":"API_DB","registry":""}]`, "")
	defer server.Close()
	cfg := ingressCLIConfig(t, server.URL)
	var out, errout bytes.Buffer
	args := []string{"secret", "list", "-e", "dev", "--config", cfg, "--manifest-dir", secretManifestDir(t, secretListManifest)}
	if code := Run(args, &out, &errout); code != 0 {
		t.Fatalf("exit %d: %s", code, errout.String())
	}
	if !strings.Contains(out.String(), "every secret the manifest needs is stored") {
		t.Fatalf("stdout %q", out.String())
	}
}

// TestSecretListWithManifestDirFailsWhenTheManifestDoesNotLoad checks that a missing manifest is reported and nothing is asked
// of the agent.
func TestSecretListWithManifestDirFailsWhenTheManifestDoesNotLoad(t *testing.T) {
	var calls atomic.Int32
	server := countingAgent(&calls)
	defer server.Close()
	cfg := ingressCLIConfig(t, server.URL)
	var out, errout bytes.Buffer
	code := Run([]string{"secret", "list", "-e", "dev", "--config", cfg, "--manifest-dir", t.TempDir()}, &out, &errout)
	if code != 1 || errout.Len() == 0 || calls.Load() != 0 {
		t.Fatalf("exit %d stderr %q calls %d", code, errout.String(), calls.Load())
	}
}
