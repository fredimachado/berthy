package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"berthy/internal/agent"
	"berthy/internal/agentclient"
	"berthy/internal/credstore/credtest"
	"berthy/internal/setup"
)

const fixtureRemovalNetworkID = "wcxk9hlk7pka26iwkly94laa0"

type setupRemovalFixture struct {
	t                               *testing.T
	target                          string
	live, previous, staged, record  []byte
	scripts, writes                 []string
	restarts, dockerRequests        int
	dockerActive, attached          bool
	applied, failRestart, failState bool
}

func newSetupRemovalFixture(t *testing.T) (*setupRemovalFixture, string) {
	t.Helper()
	f := agent.File{Listen: "127.0.0.1:7710", Environments: []agent.Hosted{
		{Project: "alpha", Environment: "dev", StateDir: "/custom/target/state", KeyFile: "/custom/target/key", Network: "alpha-dev",
			Tokens: []agent.Token{{Digest: strings.Repeat("a", 64), Scope: "admin"}}},
		{Project: "peer", Environment: "dev", StateDir: "/peer/state", KeyFile: "/peer/key",
			Tokens: []agent.Token{{Digest: strings.Repeat("b", 64), Scope: "admin"}}},
	}}
	raw, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"projects":{"alpha":{"environments":{"dev":{"url":"http://127.0.0.1:7710","token_env":"TARGET_TOKEN"}}}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	return &setupRemovalFixture{t: t, live: raw, dockerActive: true}, path
}

func (r *setupRemovalFixture) Target() string { return r.target }

func (r *setupRemovalFixture) Run(_ context.Context, script string, stdin io.Reader) (setup.Result, error) {
	r.t.Helper()
	r.scripts = append(r.scripts, script)
	answer := func(s string) (setup.Result, error) { return setup.Result{Stdout: []byte(s)}, nil }
	read := func() []byte {
		r.t.Helper()
		b, err := io.ReadAll(stdin)
		if err != nil {
			r.t.Fatal(err)
		}
		return b
	}
	switch {
	case script == "id -u; id -un":
		return answer("0\nroot\n")
	case strings.HasPrefix(script, "# inspect removal configuration\n"):
		return answer("present\n")
	case script == "cat /etc/berthy/agent.json":
		return answer(string(r.live))
	case strings.HasPrefix(script, "# read removal record\n"), strings.HasPrefix(script, "# read removal guard state\n"):
		return setup.Result{Code: 3}, nil
	case strings.HasPrefix(script, "# inspect removal paths\n"):
		return answer("12 34\n12 35\nabsent\nabsent\n/peer/state\x00/peer/key\x00/peer/key.next\x00/peer/key.prev\x00/\n")
	case strings.HasPrefix(script, "# check applied apps\n"):
		if r.applied {
			return answer("applied\n")
		}
		return answer("empty\n")
	case strings.HasPrefix(script, "# verify removal Docker service\n"):
		if !r.dockerActive {
			return setup.Result{Code: 3}, nil
		}
		return setup.Result{}, nil
	case strings.HasPrefix(script, "timeout 30 docker "):
		r.dockerRequests++
		r.dockerActive = true // The fixture socket can start the stopped daemon if a Docker request gets this far.
		switch {
		case strings.Contains(script, "network ls"):
			return answer(fixtureRemovalNetworkID + " alpha-dev\n")
		case strings.Contains(script, "network inspect"):
			containers := "{}"
			if r.attached {
				containers = `{"fixture-container":{"Name":"attached"}}`
			}
			return answer(`{"Id":"` + fixtureRemovalNetworkID + `","Name":"alpha-dev","Scope":"swarm","Driver":"overlay","Attachable":true,"Ingress":false,"Containers":` + containers + "}\n")
		case strings.Contains(script, "container ls -aq"), strings.Contains(script, "service ls -q"):
			return answer("")
		case strings.Contains(script, "network rm "):
			r.writes = append(r.writes, "network")
			return answer(fixtureRemovalNetworkID + "\n")
		}
	case strings.HasPrefix(script, "# write removal record\n"):
		r.writes = append(r.writes, "record")
		r.record = read()
		return answer("")
	case strings.Contains(script, "cat > /etc/berthy/agent.json.new"):
		r.writes = append(r.writes, "stage")
		r.staged = read()
		return answer("")
	case strings.Contains(script, "mv -f /etc/berthy/agent.json.new /etc/berthy/agent.json"):
		r.writes = append(r.writes, "swap")
		r.live, r.previous, r.staged = r.staged, r.live, nil
		return answer("prev_cfg=yes\n")
	case strings.Contains(script, "systemctl restart berthy"):
		r.restarts++
		if r.failRestart {
			return setup.Result{Code: 1}, nil
		}
		return answer("")
	case strings.HasPrefix(script, "journalctl "):
		return answer("fixture journal\n")
	case strings.HasPrefix(script, "/bin/sh /run/berthy-activation-guard.sh "):
		r.live, r.previous = r.previous, nil
		return answer("guard=restored\n")
	case strings.HasPrefix(script, "# remove state and keys\n"):
		if r.failState {
			return setup.Result{Code: 1, Stdout: []byte("fixture-private-record"), Stderr: []byte("fixture-private-key")}, nil
		}
		r.writes = append(r.writes, "state")
		return answer("changed\n")
	case strings.HasPrefix(script, "# clear removal record\n"):
		r.writes = append(r.writes, "clear")
		r.record = nil
		return answer("")
	}
	r.t.Fatalf("unexpected setup removal command: %q", script)
	return setup.Result{}, nil
}

func useSetupRemovalFixture(t *testing.T, r *setupRemovalFixture) (*[][2]any, *credtest.Memory) {
	t.Helper()
	runner, build, home, input, tokens := newSetupRunner, setupBuild, KeyHome, hiddenInput, agentclient.TokenStore
	t.Cleanup(func() {
		newSetupRunner, setupBuild, KeyHome, hiddenInput, agentclient.TokenStore = runner, build, home, input, tokens
	})
	var destinations [][2]any
	newSetupRunner = func(host string, local bool) (setup.Runner, error) {
		destinations = append(destinations, [2]any{host, local})
		r.target = host
		if local {
			r.target = "this server"
		}
		return r, nil
	}
	setupBuild = func() setup.Build { t.Fatal("removal selected an install binary"); return setup.Build{} }
	KeyHome = func() (string, error) { t.Fatal("removal looked up a home for a key copy"); return "", nil }
	hiddenInput = func() (func() ([]byte, error), bool) {
		t.Fatal("removal tried to prompt for a secret")
		return nil, false
	}
	store := credtest.NewMemory()
	agentclient.TokenStore = store
	return &destinations, store
}

func removalReport(t *testing.T, out string) setupReport {
	t.Helper()
	var report setupReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("invalid removal report: %v, %q", err, out)
	}
	if !strings.Contains(out, `"remove": true`) {
		t.Fatalf("report lacks remove=true: %q", out)
	}
	return report
}

func TestSetupRemoveFlagContract(t *testing.T) {
	for _, tt := range []struct {
		name                         string
		args                         []string
		varEnv, wantHost, diagnostic string
		local, manifest              bool
	}{
		{name: "explicit SSH", args: []string{"--remove", "--host", "ops@example", "-e", "dev"}, varEnv: "wrong", wantHost: "ops@example"},
		{name: "implicit manifest server", args: []string{"--remove"}, varEnv: "dev", wantHost: "ops@fixture", manifest: true},
		{name: "named manifest server", args: []string{"--remove", "--server", "primary", "-e", "dev"}, wantHost: "ops@fixture", manifest: true},
		{name: "local with purge", args: []string{"--remove", "--local", "--purge", "-e", "dev"}, local: true},
		{name: "empty explicit environment", args: []string{"--remove", "--local", "-e", ""}, varEnv: "dev", diagnostic: "needs -e"},
		{name: "purge alone", args: []string{"--purge", "--local", "-e", "dev"}, diagnostic: "--purge requires --remove"},
		{name: "ambiguous destination", args: []string{"--remove", "--local", "--host", "ops@example", "-e", "dev"}, diagnostic: "use only one"},
		{name: "manifest project conflict", args: []string{"--remove", "--project", "wrong", "-e", "dev"}, manifest: true, diagnostic: "differs from the project"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r, cfg := newSetupRemovalFixture(t)
			calls, _ := useSetupRemovalFixture(t, r)
			t.Setenv(agentclient.EnvironmentEnv, tt.varEnv)
			dir := t.TempDir()
			args := append([]string{"--config", cfg, "--manifest-dir", dir, "--dry-run", "--json"}, tt.args...)
			if tt.manifest {
				body := "project: alpha\nservers:\n  primary:\n    ssh: ops@fixture\n    address: 10.0.0.5\n" +
					"apps:\n  web:\n    kind: image\n    image: example.com/web\n    roles:\n      main: {}\n"
				if err := os.WriteFile(filepath.Join(dir, "berthy.yml"), []byte(body), 0600); err != nil {
					t.Fatal(err)
				}
			} else {
				args = append(args, "--project", "alpha")
			}
			code, out, diag := runSetupCmd(args...)
			if tt.diagnostic != "" {
				if code != 2 || out != "" || !strings.Contains(diag, tt.diagnostic) || len(*calls) != 0 {
					t.Fatalf("usage: exit %d stdout %q stderr %q destinations %v", code, out, diag, *calls)
				}
				return
			}
			if code != 0 || len(*calls) != 1 || (*calls)[0] != [2]any{tt.wantHost, tt.local} {
				t.Fatalf("exit %d stdout %q stderr %q destinations %v", code, out, diag, *calls)
			}
			report := removalReport(t, out)
			if report.Project != "alpha" || report.Environment != "dev" || report.Target != r.target || !report.OK {
				t.Fatalf("selection: %+v", report)
			}
		})
	}
	for _, flag := range []string{"--listen=", "--network=", "--protected=false", "--binary=", "--key-file=", "--new-token=false", "--show-token=false", "--no-install-docker=false"} {
		t.Run(flag, func(t *testing.T) {
			r, cfg := newSetupRemovalFixture(t)
			calls, _ := useSetupRemovalFixture(t, r)
			code, out, diag := runSetupCmd("--remove", "-e", "dev", "--local", "--project", "alpha", "--manifest-dir", t.TempDir(), "--config", cfg, flag)
			name, _, _ := strings.Cut(flag, "=")
			if code != 2 || out != "" || !strings.Contains(diag, name+" cannot be used with --remove") || len(*calls) != 0 {
				t.Fatalf("usage: exit %d stdout %q stderr %q destinations %v", code, out, diag, *calls)
			}
		})
	}
}

func TestSetupRemoveDryRunOnlyReads(t *testing.T) {
	for _, tt := range []struct {
		name, want                         string
		purge, attached, applied, inactive bool
	}{
		{name: "archive", want: "would archive state at"},
		{name: "purge", want: "would purge /custom/target/state", purge: true},
		{name: "attached", want: "would keep network alpha-dev", attached: true},
		{name: "applied", want: "berthy destroy --project alpha -e dev --confirm alpha/dev --allow-recreate", applied: true},
		{name: "inactive socket", want: "cannot establish the disposition of network alpha-dev", inactive: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r, cfg := newSetupRemovalFixture(t)
			_, store := useSetupRemovalFixture(t, r)
			r.attached, r.applied, r.dockerActive = tt.attached, tt.applied, !tt.inactive
			original := bytes.Clone(r.live)
			before, err := os.ReadFile(cfg)
			if err != nil {
				t.Fatal(err)
			}
			args := []string{"--remove", "-e", "dev", "--project", "alpha", "--host", "ops@fixture", "--manifest-dir", t.TempDir(), "--config", cfg, "--dry-run", "--verbose", "--json"}
			if tt.purge {
				args = append(args, "--purge")
			}
			code, out, diag := runSetupCmd(args...)
			wantCode := 0
			if tt.applied || tt.inactive {
				wantCode = 1
			}
			if code != wantCode || !strings.Contains(out, tt.want) {
				t.Fatalf("exit %d stdout %q stderr %q", code, out, diag)
			}
			report := removalReport(t, out)
			after, err := os.ReadFile(cfg)
			if report.OK != (wantCode == 0) || !report.DryRun || err != nil || !bytes.Equal(before, after) ||
				!bytes.Equal(original, r.live) || len(r.writes) != 0 || r.restarts != 0 || store.Calls() != 0 {
				t.Fatalf("preview changed the fixture: writes %v, restarts %d, store calls %d, config error %v", r.writes, r.restarts, store.Calls(), err)
			}
			if tt.inactive && (r.dockerActive || r.dockerRequests != 0 || !strings.Contains(diag, "systemctl is-active --quiet docker")) {
				t.Fatalf("inactive daemon was not gated: active=%t requests=%d log %q", r.dockerActive, r.dockerRequests, diag)
			}
			if wantCode == 0 && (!strings.Contains(out, "would remove the local CLI entry") || !strings.Contains(diag, "not run: dry run")) {
				t.Fatalf("missing preview or verbose detail: %q %q", out, diag)
			}
		})
	}
}

func TestSetupRemoveReportsRemovalWithoutInstallAdvice(t *testing.T) {
	for _, tt := range []struct {
		name, want                              string
		failRestart, failState, purge, attached bool
	}{
		{name: "archive success", want: "archived state at"},
		{name: "purge with attachment", want: "kept network alpha-dev because it is in use", purge: true, attached: true},
		{name: "rollback", want: "the previous one was put back", failRestart: true},
		{name: "partial cleanup", want: "could not finish state and key cleanup", failState: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r, cfg := newSetupRemovalFixture(t)
			_, store := useSetupRemovalFixture(t, r)
			r.failRestart, r.failState, r.attached = tt.failRestart, tt.failState, tt.attached
			args := []string{"--remove", "-e", "dev", "--project", "alpha", "--host", "ops@fixture", "--manifest-dir", t.TempDir(), "--config", cfg}
			if tt.purge {
				args = append(args, "--purge")
			}
			wantCode := 0
			if tt.failRestart || tt.failState {
				wantCode = 1
			}
			for _, jsonOutput := range []bool{true, false} {
				if !jsonOutput {
					// A separate invocation gets the original server and local config.
					fresh, path := newSetupRemovalFixture(t)
					*r = *fresh
					r.failRestart, r.failState, r.attached = tt.failRestart, tt.failState, tt.attached
					args = append(args[:len(args):len(args)], "--config", path)
				}
				run := append([]string{"--verbose"}, args...)
				if jsonOutput {
					run = append(run, "--json")
				}
				code, out, diag := runSetupCmd(run...)
				if code != wantCode || !strings.Contains(out, tt.want) {
					t.Fatalf("exit %d stdout %q stderr %q", code, out, diag)
				}
				for _, leak := range []string{`"token":`, `"key_copy":`, `"next_steps":`, "next steps:", "admin token for", "secrets:", "fixture-private-record", "fixture-private-key", `"StateIdentity"`} {
					if strings.Contains(out+diag, leak) {
						t.Fatalf("removal exposed %q: %q %q", leak, out, diag)
					}
				}
				if jsonOutput {
					report := removalReport(t, out)
					if report.OK != (wantCode == 0) || report.Project != "alpha" || report.Environment != "dev" || report.Target != "ops@fixture" {
						t.Fatalf("report: %+v", report)
					}
				} else if !strings.Contains(out, "removal of alpha/dev on ops@fixture") {
					t.Fatalf("wrong removal summary: %q", out)
				}
			}
			if store.Calls() != 0 {
				t.Fatalf("removal contacted the credential store %d times", store.Calls())
			}
		})
	}
}
