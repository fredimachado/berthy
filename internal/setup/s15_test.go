package setup

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"berthy/internal/agent"
)

func s15File(t *testing.T, project, env, network string) agent.File {
	t.Helper()
	return agent.File{Listen: listenIP + ":7710", DockerSocket: dockerSocket, Environments: []agent.Hosted{{Project: project, Environment: env, StateDir: "/var/lib/berthy/environments/" + project + "/" + env + "/state", KeyFile: "/var/lib/berthy/environments/" + project + "/" + env + "/key", Network: network, Tokens: []agent.Token{{Name: "admin", Scope: "admin", Digest: strings.Repeat("a", 64)}}}}}
}

func s15Server(t *testing.T, file agent.File) *agentFake {
	t.Helper()
	b, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return &agentFake{probe: with(setUpServer(), "agent_config=yes", "agent_config_stat="+wantPrivate, "user=999", "user_groups=berthy docker", "data_dir_stat="+wantPrivateDir), config: string(b), prevCfg: true}
}

func s15StagedFile(t *testing.T, f *agentFake) agent.File {
	t.Helper()
	c, ok := f.find("cat > " + configPath + ".new")
	if !ok {
		t.Fatal("no staged config")
	}
	file, err := agent.ParseFile(c.stdin)
	if err != nil {
		t.Fatalf("staged config: %v", err)
	}
	if err := file.Validate(); err != nil {
		t.Fatalf("invalid staged config: %v", err)
	}
	return file
}

func TestSetupWritesTheFirstEnvironmentUnderItsOwnPaths(t *testing.T) {
	f := freshAgentServer(t)
	s := agentSession(t, f, elfFile(t, emX86), false, nil)
	got, ok := runSteps(s)
	if !ok {
		t.Fatalf("setup: %+v", got)
	}
	file := s15StagedFile(t, f)
	if len(file.Environments) != 1 {
		t.Fatalf("environments: %+v", file.Environments)
	}
	h := file.Environments[0]
	if h.Project != "shop" || h.Environment != "p5" || h.StateDir != "/var/lib/berthy/environments/shop/p5/state" || h.KeyFile != "/var/lib/berthy/environments/shop/p5/key" || len(h.Tokens) != 1 {
		t.Fatalf("hosted: %+v", h)
	}
	if !f.ran("/var/lib/berthy/environments/shop/p5/key") {
		t.Fatal("key path not used")
	}
}

func s15Add(t *testing.T, project, env string) (agent.File, agent.File, *agentFake, *Session) {
	t.Helper()
	before := s15File(t, project, env, project+"-"+env)
	f := s15Server(t, before)
	s := agentSession(t, f, elfFile(t, emX86), false, nil)
	got, ok := runSteps(s)
	if !ok {
		t.Fatalf("setup: %+v", got)
	}
	after := s15StagedFile(t, f)
	if len(after.Environments) != 2 {
		t.Fatalf("environments: %+v", after.Environments)
	}
	added := after.Environments[1]
	if added.StateDir != "/var/lib/berthy/environments/shop/p5/state" || added.KeyFile != "/var/lib/berthy/environments/shop/p5/key" || added.Network != "p5net" || len(added.Tokens) != 1 || added.Tokens[0].Digest != agent.Digest(s.Token) || added.Tokens[0].Scope != "admin" || !s.TokenLive {
		t.Fatalf("new environment: %+v, token live: %v", added, s.TokenLive)
	}
	for _, c := range f.calls {
		if agentChange(c.script) && (strings.Contains(c.script, before.Environments[0].StateDir) || strings.Contains(c.script, before.Environments[0].KeyFile)) {
			t.Fatalf("a changing script names another environment's state or key: %s", c.script)
		}
	}
	return before, after, f, s
}

func TestSetupAddsAnEnvironmentOfAnotherProject(t *testing.T) {
	_, after, _, s := s15Add(t, "other", "p5")
	if len(after.Environments) != 2 || after.Environments[1].Project != "shop" || after.Environments[1].Environment != "p5" || s.Token == "" {
		t.Fatalf("file %+v, token made %v", after, s.Token != "")
	}
}

func TestSetupAddsASecondEnvironmentOfTheSameProject(t *testing.T) {
	_, after, _, s := s15Add(t, "shop", "prod")
	if len(after.Environments) != 2 || after.Environments[1].Project != "shop" || after.Environments[1].Environment != "p5" || s.Token == "" {
		t.Fatalf("file %+v, token made %v", after, s.Token != "")
	}
}

func TestAddingAnEnvironmentLeavesTheOtherEntriesByteIdentical(t *testing.T) {
	before := s15File(t, "other", "prod", "other-prod")
	f := s15Server(t, before)
	raw := `{ "tokens" : [ { "scope": "admin", "digest": "` + strings.Repeat("a", 64) + `", "name": "admin" } ], "network": "other-prod", "key_file": "/var/lib/berthy/environments/other/prod/key", "state_dir": "/var/lib/berthy/environments/other/prod/state", "environment": "prod", "project": "other" }`
	f.config = `{"listen":"` + listenIP + `:7710", "docker_socket":"/var/run/docker.sock", "environments":[` + raw + `]}`
	s := agentSession(t, f, elfFile(t, emX86), false, nil)
	got, ok := runSteps(s)
	if !ok {
		t.Fatalf("setup: %+v", got)
	}
	staged, found := f.find("cat > " + configPath + ".new")
	if !found || !bytes.Contains(staged.stdin, []byte(raw)) {
		t.Fatalf("other entry changed: %s", staged.stdin)
	}
}

func TestSetupRefusesAServerOfAnEarlierReleaseAndTellsToMoveItAside(t *testing.T) {
	f := s15Server(t, s15File(t, "other", "prod", "other-prod"))
	f.config = `{"environment":"prod"}`
	s := agentSession(t, f, elfFile(t, emX86), false, nil)
	got, ok := runSteps(s)
	const want = "this server has the agent of an earlier Berthy release, which cannot be upgraded in place. Move it aside, do not delete it: sudo systemctl stop berthy && sudo mv /etc/berthy /etc/berthy.pre-projects && sudo mv /var/lib/berthy /var/lib/berthy.pre-projects && sudo chmod 0700 /var/lib/berthy.pre-projects, then run setup again. Its apps keep running and are yours to remove or adopt; its state and key stay in /var/lib/berthy.pre-projects."
	if ok || lastResult(got).Detail != want {
		t.Fatalf("result: %+v", got)
	}
	for _, c := range f.calls {
		if agentChange(c.script) {
			t.Fatalf("sent changing script: %s", c.script)
		}
	}
}

func TestSetupRefusesAnEnvironmentThatWouldShareDockerNames(t *testing.T) {
	for _, tc := range []struct{ project, env, network, reason string }{{"shop-p5", "x", "p5net", "duplicate network"}, {"shop", "p5-x", "other", "duplicate project-environment prefix"}} {
		f := s15Server(t, s15File(t, tc.project, tc.env, tc.network))
		s := agentSession(t, f, elfFile(t, emX86), false, func(o *Options) {
			if tc.network == "other" {
				o.Project, o.Env, o.Network = "shop-p5", "x", "another"
			}
		})
		got, ok := runSteps(s)
		if ok || lastResult(got).Step != "check" || !strings.Contains(lastResult(got).Detail, tc.reason) {
			t.Fatalf("collision accepted: %+v", got)
		}
		for _, c := range f.calls {
			if agentChange(c.script) {
				t.Fatalf("sent changing script: %s", c.script)
			}
		}
	}
}

func TestAnAgentThatDoesNotAnswerGetsThePreviousFileBack(t *testing.T) {
	f := s15Server(t, s15File(t, "other", "prod", "other-prod"))
	f.waits = []int{1, 0}
	before := f.config
	server := &s15RollbackServer{agentFake: f, live: before}
	s := agentSession(t, server, elfFile(t, emX86), false, nil)
	got, ok := runSteps(s)
	if ok || lastResult(got).Step != "service" || !f.ran("mv -f "+configPath+".prev "+configPath) || server.live != before || server.staged == "" {
		t.Fatalf("rollback: %+v", got)
	}
}

type s15RollbackServer struct {
	*agentFake
	live, staged, previous string
}

func (f *s15RollbackServer) Run(ctx context.Context, script string, stdin io.Reader) (Result, error) {
	var in []byte
	if stdin != nil {
		in, _ = io.ReadAll(stdin)
		stdin = bytes.NewReader(in)
	}
	if strings.Contains(script, "cat > "+configPath+".new") {
		f.staged = string(in)
	}
	if strings.Contains(script, "mv -f "+configPath+".new "+configPath) {
		f.previous, f.live = f.live, f.staged
	}
	if strings.Contains(script, "mv -f "+configPath+".prev "+configPath) {
		f.live = f.previous
	}
	return f.agentFake.Run(ctx, script, stdin)
}

func TestASecondRunOfSetupChangesNothing(t *testing.T) {
	exe := elfFile(t, emX86)
	file := s15File(t, "shop", "p5", "p5net")
	f := s15Server(t, file)
	f.probe = with(f.probe, "bin_sha="+fileHash(t, exe), "unit_sha="+sha256Hex(unitContent()), "service_active=active", "service_enabled=enabled", "state_dir_stat="+wantPrivateDir, "state_db=yes", "key_sha="+strings.Repeat("b", 64), "key_stat=32 "+wantPrivate)
	cli := t.TempDir() + "/cli.json"
	s := agentSession(t, f, exe, false, func(o *Options) { o.CLIConfig = cli })
	// First populate the local CLI configuration, then inspect the re-run.
	if _, _, err := cliConfigStep(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	f.calls = nil
	got, ok := runSteps(s)
	if !ok {
		t.Fatalf("rerun: %+v", got)
	}
	for _, r := range got {
		if r.Status != StatusOK && !((r.Step == "cli_token" || r.Step == "cli_key") && r.Status == StatusSkipped) {
			t.Errorf("step: %+v", r)
		}
	}
	for _, c := range f.calls {
		if agentChange(c.script) {
			t.Errorf("sent changing script: %s", c.script)
		}
	}
}

func TestHostedPathsUseForwardSlashes(t *testing.T) {
	state, key := hostedPaths("shop", "prod")
	if state != "/var/lib/berthy/environments/shop/prod/state" || key != "/var/lib/berthy/environments/shop/prod/key" {
		t.Fatalf("paths: %q %q", state, key)
	}
}

func TestSetupRefusesAnOverlongNetworkBeforeAnyChange(t *testing.T) {
	f := s15Server(t, s15File(t, "other", "prod", "other-prod"))
	s := agentSession(t, f, elfFile(t, emX86), false, func(o *Options) { o.Network = strings.Repeat("n", 64) })
	got, ok := runSteps(s)
	if ok || lastResult(got).Step != "check" || !strings.Contains(lastResult(got).Detail, "invalid network") {
		t.Fatalf("result: %+v", got)
	}
	for _, c := range f.calls {
		if agentChange(c.script) {
			t.Fatalf("sent changing script: %s", c.script)
		}
	}
}

func TestSetupRefusesOverlappingRotationKeyBeforeChanges(t *testing.T) {
	file := s15File(t, "other", "prod", "other-prod")
	file.Environments[0].KeyFile = "/var/lib/berthy/environments/shop/prod/key.next"
	f := s15Server(t, file)
	original := f.config
	s := agentSession(t, f, elfFile(t, emX86), false, func(o *Options) { o.Env = "prod" })
	got, ok := runSteps(s)
	if ok || lastResult(got).Step != "check" {
		t.Fatalf("result: %+v", got)
	}
	if f.config != original {
		t.Fatal("existing config changed")
	}
	for _, c := range f.calls {
		if agentChange(c.script) {
			t.Fatalf("changing script: %s", c.script)
		}
	}
}

func TestSetupMakesRestoredStateParentsTraversable(t *testing.T) {
	exe := elfFile(t, emX86)
	f := setUpAgent(t, exe)
	f.probe = with(f.probe, "parent_0=root:root 700", "parent_1=root:root 700", "parent_2=root:root 700")
	s := agentSession(t, f, exe, false, nil)
	var got []StepResult
	Run(context.Background(), s, []Step{{"connect", connectStep}, {"check", checkStep}, {"key", keyStep}}, func(r StepResult) { got = append(got, r) })
	if lastResult(got).Status != StatusChanged {
		t.Fatalf("key step: %+v", got)
	}
	for _, p := range []string{"/var/lib/berthy/environments", "/var/lib/berthy/environments/shop"} {
		if !f.ran("install -d -o root -g berthy -m 0750 '" + p + "'") {
			t.Errorf("missing traversal permission for %s: %v", p, f.scripts())
		}
	}
	if !f.ran("install -d -o berthy -g berthy -m 0700 '/var/lib/berthy/environments/shop/p5'") {
		t.Error("key directory is not writable by berthy")
	}
}

func TestSetupRepairsRootRestoredDirectoriesForTheAgent(t *testing.T) {
	exe := elfFile(t, emX86)
	f := setUpAgent(t, exe)
	f.probe = without(f.probe, "state_dir_stat=", "data_dir_stat=")
	f.probe = with(f.probe, "data_dir_stat=root:root 700", "state_dir_stat=root:root 700", "state_foreign=yes", "parent_0=root:root 700", "parent_1=root:root 700", "parent_2=root:root 700")
	s := agentSession(t, f, exe, false, nil)
	var got []StepResult
	Run(context.Background(), s, []Step{{"connect", connectStep}, {"check", checkStep}, {"key", keyStep}}, func(r StepResult) { got = append(got, r) })
	if lastResult(got).Status != StatusChanged {
		t.Fatalf("result: %+v", got)
	}
	c, ok := f.find("install -d -o root -g berthy -m 0750")
	if !ok {
		t.Fatal("directories not repaired")
	}
	for _, want := range []string{"install -d -o berthy -g berthy -m 0700 /var/lib/berthy", "install -d -o root -g berthy -m 0750 '/var/lib/berthy/environments'", "install -d -o root -g berthy -m 0750 '/var/lib/berthy/environments/shop'", "install -d -o berthy -g berthy -m 0700 '/var/lib/berthy/environments/shop/p5'", "install -d -o berthy -g berthy -m 0700 '" + stateDir + "'"} {
		if !strings.Contains(c.script, want) {
			t.Errorf("missing %q in %s", want, c.script)
		}
	}
}

func TestSetupPreservesBerthyOwnedKeyDirectory(t *testing.T) {
	exe := elfFile(t, emX86)
	f := setUpAgent(t, exe)
	f.config = strings.Replace(f.config, `"key_file": "`+keyPath+`"`, `"key_file": "`+stateDir+`/keys/key"`, 1)
	f.probe = with(f.probe, "parent_0=root:berthy 750", "parent_1=root:berthy 750", "parent_2=berthy:berthy 700", "parent_3=berthy:berthy 700", "parent_4=berthy:berthy 750")
	s := agentSession(t, f, exe, false, nil)
	var got []StepResult
	Run(context.Background(), s, []Step{{"connect", connectStep}, {"check", checkStep}, {"key", keyStep}}, func(r StepResult) { got = append(got, r) })
	if lastResult(got).Status != StatusOK {
		t.Fatalf("existing berthy-owned directory changed: %+v", got)
	}
	for _, c := range f.calls {
		if agentChange(c.script) {
			t.Fatalf("changing script: %s", c.script)
		}
	}
}

func TestRootRestoreWithSavedKeyRepairsWritableKeyDirectory(t *testing.T) {
	exe := elfFile(t, emX86)
	f := setUpAgent(t, exe)
	f.probe = without(f.probe, "key_sha=", "key_stat=", "state_dir_stat=", "data_dir_stat=")
	f.probe = with(f.probe, "data_dir_stat=root:root 700", "state_dir_stat=root:root 700", "state_db=yes", "state_foreign=yes", "parent_0=root:root 700", "parent_1=root:root 700", "parent_2=root:root 700")
	key := keyBytes()
	s := agentSession(t, f, exe, false, func(o *Options) { o.KeyFile = writeKey(t, key) })
	var got []StepResult
	Run(context.Background(), s, []Step{{"connect", connectStep}, {"check", checkStep}, {"key", keyStep}}, func(r StepResult) { got = append(got, r) })
	if lastResult(got).Status != StatusChanged {
		t.Fatalf("key step: %+v", got)
	}
	c, ok := f.find("cat > \"$tmp\"")
	if !ok || !bytes.Equal(c.stdin, key) {
		t.Fatal("saved key not uploaded")
	}
	if !strings.Contains(c.script, "if [ ! -d '/var/lib/berthy/environments/shop/p5' ]; then") {
		t.Fatal("key upload would change an existing directory")
	}
	for _, want := range []string{"install -d -o berthy -g berthy -m 0700 /var/lib/berthy", "install -d -o root -g berthy -m 0750 '/var/lib/berthy/environments'", "install -d -o root -g berthy -m 0750 '/var/lib/berthy/environments/shop'", "install -d -o berthy -g berthy -m 0700 '/var/lib/berthy/environments/shop/p5'", "install -d -o berthy -g berthy -m 0700 '" + stateDir + "'"} {
		if !strings.Contains(c.script, want) {
			t.Errorf("missing %q in %s", want, c.script)
		}
	}
}
