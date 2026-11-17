package cli

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
)

// changeCommand is one change command, or one subcommand of one, under the name changeFlags uses. pos is the command's
// words and positional arguments, fixed are flags it always needs, and tail goes last, after the flags (exec's command).
type changeCommand struct {
	name  string
	pos   []string
	fixed []string
	tail  []string
	local bool // the command reads no agent, so it takes no --config
}

// argv returns cc's arguments with extra flags after its fixed ones and before its tail.
func (cc changeCommand) argv(cfg string, extra ...string) []string {
	args := append([]string{}, cc.pos...)
	args = append(args, cc.fixed...)
	args = append(args, "-e", "dev")
	if !cc.local {
		args = append(args, "--config", cfg)
	}
	args = append(args, extra...)
	return append(args, cc.tail...)
}

var changeCommands = []changeCommand{
	{name: "deploy", pos: []string{"deploy", "api"}},
	{name: "rollback", pos: []string{"rollback", "api"}},
	{name: "restart", pos: []string{"restart", "api"}},
	{name: "scale", pos: []string{"scale", "api"}},
	{name: "exec", pos: []string{"exec", "api"}, tail: []string{"--", "true"}},
	{name: "prune", pos: []string{"prune"}},
	{name: "remove", pos: []string{"remove", "api"}},
	{name: "destroy", pos: []string{"destroy"}, fixed: []string{"--confirm", "dev"}},
	{name: "restore", pos: []string{"restore", "key"}},
	{name: "wait", pos: []string{"wait", "api"}},
	{name: "history", pos: []string{"history", "api"}},
	{name: "diagnostics", pos: []string{"diagnostics", "op1"}},
	{name: "audit", pos: []string{"audit"}},
	{name: "lock status", pos: []string{"lock", "status"}},
	{name: "lock acquire", pos: []string{"lock", "acquire"}},
	{name: "lock release", pos: []string{"lock", "release"}},
	{name: "recovery resolve", pos: []string{"recovery", "resolve", "op1"}},
	{name: "ingress apply", pos: []string{"ingress", "apply"}},
	{name: "ingress status", pos: []string{"ingress", "status"}},
	{name: "alerts test", pos: []string{"alerts", "test"}},
	{name: "secret list", pos: []string{"secret", "list"}},
	{name: "secret set", pos: []string{"secret", "set", "DB_URL"}},
	{name: "secret delete", pos: []string{"secret", "delete", "DB_URL"}},
	{name: "secret rm", pos: []string{"secret", "rm", "DB_URL"}},
	{name: "job run", pos: []string{"job", "run", "dump-db"}},
	{name: "backup list", pos: []string{"backup", "list"}},
	{name: "backup run", pos: []string{"backup", "run", "main-db"}},
	{name: "state-backup", pos: []string{"state-backup"}},
	{name: "approvals", pos: []string{"approvals"}},
	{name: "approve", pos: []string{"approve", "ap1"}},
	{name: "reject", pos: []string{"reject", "ap1"}},
	{name: "config show", pos: []string{"config", "show"}, local: true},
	{name: "config validate", pos: []string{"config", "validate"}, local: true},
}

// changeFlagArgs is every flag a change command can be given, with the value it takes. -e is left out: every command reads it.
var changeFlagArgs = [][]string{
	{"--config", "cfg.json"}, {"--json"}, {"--dry-run"}, {"--adopt"}, {"--healthy"}, {"--replace-existing"},
	{"--allow-recreate"}, {"--volumes"}, {"--tunnel"}, {"--all"}, {"--revert-schema"},
	{"--confirm", "dev"}, {"--backup", "main-db"}, {"--into", "volume"}, {"--manifest-dir", "dir"},
	{"--version", "v1"}, {"--commit", "abc123"}, {"--request-id", "r1"}, {"--role", "web"}, {"--service", "postgres"}, {"--timeout", "1m"},
	{"--registry", "ghcr.io"}, {"-m", "reason"}, {"--message", "reason"}, {"--to", "2"}, {"--replicas", "2"},
}

// refusalMessage is what a flag that a command does not use gets. A flag with its own check keeps that check's message.
func refusalMessage(name, flag string) string {
	cmd, _, _ := strings.Cut(name, " ")
	switch {
	case flag == "--volumes" && cmd != "remove" && cmd != "destroy":
		return "--volumes applies to remove and destroy only"
	case flag == "--all" && cmd != "approvals":
		return "--all applies to approvals only"
	case (flag == "--tunnel" || flag == "--confirm") && cmd != "destroy":
		return "--tunnel and --confirm apply to destroy only"
	case flag == "--backup" && cmd != "backup", flag == "--into" && cmd != "restore":
		return "--backup applies to backup list, and --into to restore"
	}
	return flag + " is not a flag of " + name
}

// withStdin replaces standard input with input for the rest of the test.
func withStdin(t *testing.T, input string) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(w, input); err != nil {
		t.Fatal(err)
	}
	w.Close()
	old := os.Stdin
	os.Stdin = r
	t.Cleanup(func() {
		os.Stdin = old
		r.Close()
	})
}

// countingAgent answers every request with a failure and counts the requests, so a refused command can be seen to send none.
func countingAgent(calls *atomic.Int32) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"code":"test","message":"answered by the test"}`)
	}))
}

// TestChangeCommandsRefuseFlagsTheyDoNotUse runs every change command with every flag it does not read: each must exit 2
// with its message before any request reaches the agent.
func TestChangeCommandsRefuseFlagsTheyDoNotUse(t *testing.T) {
	var calls atomic.Int32
	server := countingAgent(&calls)
	defer server.Close()
	cfg := ingressCLIConfig(t, server.URL)
	for _, cc := range changeCommands {
		used, ok := changeFlags[cc.name]
		if !ok {
			t.Fatalf("%s has no entry in changeFlags", cc.name)
		}
		for _, fl := range changeFlagArgs {
			if slices.Contains(used, canonicalFlag(fl[0])) {
				continue
			}
			calls.Store(0)
			var out, errout bytes.Buffer
			code := Run(cc.argv(cfg, fl...), &out, &errout)
			want := refusalMessage(cc.name, fl[0])
			if code != 2 || strings.TrimSpace(errout.String()) != want {
				t.Errorf("%s %v: exit %d stderr %q, want exit 2 with %q", cc.name, fl, code, errout.String(), want)
			}
			if n := calls.Load(); n != 0 {
				t.Errorf("%s %v: the agent was called %d times", cc.name, fl, n)
			}
		}
	}
}

// TestChangeCommandsStillAcceptTheFlagsTheyUse gives every change command each flag it reads: none may be refused as a
// flag of the command. The agent fails every request, so the commands end with their own errors.
func TestChangeCommandsStillAcceptTheFlagsTheyUse(t *testing.T) {
	var calls atomic.Int32
	server := countingAgent(&calls)
	defer server.Close()
	cfg := ingressCLIConfig(t, server.URL)
	withStdin(t, "postgres://db\n")
	for _, cc := range changeCommands {
		for _, fl := range changeFlagArgs {
			if !slices.Contains(changeFlags[cc.name], canonicalFlag(fl[0])) || fl[0] == "--config" {
				// --config is in every remote command's arguments already, so it is not added again.
				continue
			}
			var out, errout bytes.Buffer
			Run(cc.argv(cfg, fl...), &out, &errout)
			if msg := errout.String(); strings.Contains(msg, "is not a flag of") || strings.Contains(msg, "applies to") || strings.Contains(msg, "unknown flag") {
				t.Errorf("%s %v: refused: %q", cc.name, fl, msg)
			}
		}
	}
}

// TestDryRunIsRefusedWhereItWouldActForReal names the commands that run for real when given --dry-run before the fix.
func TestDryRunIsRefusedWhereItWouldActForReal(t *testing.T) {
	var calls atomic.Int32
	server := countingAgent(&calls)
	defer server.Close()
	cfg := ingressCLIConfig(t, server.URL)
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"approve", "ap1"}, "--dry-run is not a flag of approve"},
		{[]string{"reject", "ap1"}, "--dry-run is not a flag of reject"},
		{[]string{"backup", "run", "main-db"}, "--dry-run is not a flag of backup run"},
		{[]string{"job", "run", "dump-db"}, "--dry-run is not a flag of job run"},
		{[]string{"state-backup"}, "--dry-run is not a flag of state-backup"},
		{[]string{"alerts", "test"}, "--dry-run is not a flag of alerts test"},
	} {
		calls.Store(0)
		var out, errout bytes.Buffer
		args := append(append([]string{}, tc.args...), "-e", "dev", "--config", cfg, "--dry-run")
		if code := Run(args, &out, &errout); code != 2 || strings.TrimSpace(errout.String()) != tc.want || calls.Load() != 0 {
			t.Errorf("%v: exit %d stderr %q (agent calls %d), want exit 2 with %q and no call", tc.args, code, errout.String(), calls.Load(), tc.want)
		}
	}
}

// TestSecretSetDryRunPlansWithoutStoring checks that secret set takes --dry-run: the agent plans the change and stores nothing.
func TestSecretSetDryRunPlansWithoutStoring(t *testing.T) {
	var method, path, body string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		method, path, body = r.Method, r.URL.Path, string(data)
		fmt.Fprint(w, `{"action":"secret set","name":"DB_URL","exists":false,"registry":""}`)
	}))
	defer server.Close()
	cfg := ingressCLIConfig(t, server.URL)
	withStdin(t, "postgres://db\n")
	var out, errout bytes.Buffer
	if code := Run([]string{"secret", "set", "DB_URL", "--dry-run", "-e", "dev", "--config", cfg}, &out, &errout); code != 0 {
		t.Fatalf("exit %d: %s", code, errout.String())
	}
	if method != "POST" || !strings.HasSuffix(path, "/secrets") || !strings.Contains(body, `"dry_run":true`) {
		t.Fatalf("request %s %s %s, want a dry-run POST to secrets", method, path, body)
	}
	if !strings.Contains(out.String(), "secret set") {
		t.Fatalf("output %q does not show the plan", out.String())
	}
}

// TestLockReleaseRefusesMessage: a release does not record a message, so -m is refused before any request rather than
// dropped while the person thinks it was recorded.
func TestLockReleaseRefusesMessage(t *testing.T) {
	var calls atomic.Int32
	server := countingAgent(&calls)
	defer server.Close()
	cfg := ingressCLIConfig(t, server.URL)
	for _, flag := range [][]string{{"-m", "reason"}, {"--message", "reason"}} {
		calls.Store(0)
		var out, errout bytes.Buffer
		args := append([]string{"lock", "release", "-e", "dev", "--config", cfg}, flag...)
		want := flag[0] + " is not a flag of lock release"
		if code := Run(args, &out, &errout); code != 2 || strings.TrimSpace(errout.String()) != want || calls.Load() != 0 {
			t.Errorf("%v: exit %d stderr %q (agent calls %d), want exit 2 with %q and no call", flag, code, errout.String(), calls.Load(), want)
		}
	}
}

// TestExecRefusesRoleAndServiceTogether: a role and a Compose service name different parts of an app, so both are refused
// before any request reaches the agent.
func TestExecRefusesRoleAndServiceTogether(t *testing.T) {
	var calls atomic.Int32
	server := countingAgent(&calls)
	defer server.Close()
	cfg := ingressCLIConfig(t, server.URL)
	var out, errout bytes.Buffer
	args := []string{"exec", "api", "--service", "x", "--role", "y", "-e", "dev", "--config", cfg, "--", "true"}
	if code := Run(args, &out, &errout); code != 2 || strings.TrimSpace(errout.String()) != "--role and --service cannot be combined" || calls.Load() != 0 {
		t.Fatalf("exit %d stderr %q (agent calls %d), want exit 2 with the refusal and no call", code, errout.String(), calls.Load())
	}
}

// TestExecSendsTheServiceToTheAgent: --service reaches the request body as the Compose service the exec runs in.
func TestExecSendsTheServiceToTheAgent(t *testing.T) {
	var body string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		body = string(data)
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"code":"test","message":"answered by the test"}`)
	}))
	defer server.Close()
	cfg := ingressCLIConfig(t, server.URL)
	var out, errout bytes.Buffer
	Run([]string{"exec", "db", "--service", "postgres", "-e", "dev", "--config", cfg, "--", "psql", "-c", "select 1"}, &out, &errout)
	if !strings.Contains(body, `"service":"postgres"`) || !strings.Contains(body, `"command":["psql","-c","select 1"]`) {
		t.Fatalf("request body %s", body)
	}
}
