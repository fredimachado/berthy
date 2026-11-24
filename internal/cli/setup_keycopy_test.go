package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"berthy/internal/keywrap"
	"berthy/internal/scope"
	"berthy/internal/setup"
)

func TestSetupFallbackKeyAdvice(t *testing.T) {
	for _, local := range []bool{false, true} {
		got := setupNextSteps(nextSteps{id: scope.ID{Project: "shop", Environment: "prod"}, local: local})[0]
		if local {
			for _, want := range []string{"shop/prod", "--project shop", "same build", "--binary", "interrupted key rotation", "same --network and --protected"} {
				if !strings.Contains(got, want) {
					t.Errorf("missing %q", want)
				}
			}
		} else if got != "setup saved no copy of the encryption key (cli_key above says why): fix that, then run the same berthy setup command again, from this same directory and with the same environment" {
			t.Errorf("bad retry: %s", got)
		}
		if strings.Contains(got, "sudo cat") || strings.Contains(got, "berthy setup -e") {
			t.Error("unsafe or synthesized command")
		}
	}
}

func TestSetupCopiesKeyAndReportsOutcomes(t *testing.T) {
	key := bytes.Repeat([]byte{0x31}, 32)
	home := t.TempDir()
	oldHome := KeyHome
	KeyHome = func() (string, error) { return home, nil }
	t.Cleanup(func() { KeyHome = oldHome })
	server := installedServer(t, "ops@s1")
	server.key = key
	withSetupRunner(t, server)
	cfg := filepath.Join(t.TempDir(), "config.json")
	dir := projectDir(t)
	args := []string{"-e", "p5", "--host", "ops@s1", "--manifest-dir", dir, "--config", cfg, "--json", "--verbose"}
	var path string
	for _, want := range []string{"changed", "ok"} {
		code, out, errout := runSetupCmd(args...)
		var rep setupReport
		if code != 0 || json.Unmarshal([]byte(out), &rep) != nil || !rep.OK || len(rep.Steps) != 15 || rep.Steps[14].Status != want || rep.KeyCopy == "" {
			t.Fatalf("incorrect report: code %d", code)
		}
		if path != "" && path != rep.KeyCopy {
			t.Fatal("copy path changed")
		}
		path = rep.KeyCopy
		got, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(got, key) {
			t.Fatal("incorrect key copy")
		}
		if !strings.Contains(rep.NextSteps[0], "keep the copy of the encryption key") || !strings.Contains(errout, "# cli_key") {
			t.Fatal("missing copy output")
		}
		for _, form := range []string{string(key), hex.EncodeToString(key), base64.StdEncoding.EncodeToString(key), base64.RawStdEncoding.EncodeToString(key), base64.URLEncoding.EncodeToString(key), base64.RawURLEncoding.EncodeToString(key)} {
			if strings.Contains(out, form) || strings.Contains(errout, form) {
				t.Fatal("key exposed")
			}
		}
	}
}

func TestSetupKeyRefusalAndRetryAdvice(t *testing.T) {
	home := t.TempDir()
	oldHome := KeyHome
	KeyHome = func() (string, error) { return home, nil }
	t.Cleanup(func() { KeyHome = oldHome })
	server := installedServer(t, "ops@s1")
	server.keyAnswer = func(context.Context, []byte) (setup.Result, error) {
		return setup.Result{Code: 3, Stdout: []byte(keywrap.UnsettledLine)}, nil
	}
	withSetupRunner(t, server)
	code, out, _ := runSetupCmd("-e", "p5", "--host", "ops@s1", "--manifest-dir", projectDir(t), "--json")
	var rep setupReport
	want := "a key rotation on the server did not finish, so setup copied no key. On the server, find and fix what kept the agent from putting its key files in place (for example a leftover <key>.prev.tmp it cannot remove, a full disk, or wrong ownership). Do not restart the agent before that: it finishes the rotation when it starts, and it cannot start while the cause remains. Then restart it and run the same berthy setup command again, from this same directory and with the same environment"
	if code != 0 || json.Unmarshal([]byte(out), &rep) != nil || rep.KeyCopy != "" || rep.Steps[len(rep.Steps)-1].Status != "skipped" || rep.NextSteps[0] != want || strings.Contains(strings.Join(rep.NextSteps, "\n"), "sudo cat") {
		t.Fatal("incorrect unsettled report")
	}
	server.keyAnswer = nil
	server.key = bytes.Repeat([]byte{0x58}, 32)
	code, out, _ = runSetupCmd("-e", "p5", "--host", "ops@s1", "--manifest-dir", projectDir(t), "--json")
	if code != 0 || json.Unmarshal([]byte(out), &rep) != nil || rep.KeyCopy == "" || rep.Steps[len(rep.Steps)-1].Status != "changed" {
		t.Fatal("settled key was not copied")
	}
}

func TestSetupRetrySuffixesAndLocalAdvice(t *testing.T) {
	id := scope.ID{Project: "shop", Environment: "prod"}
	for _, n := range []nextSteps{{id: id, envFromVariable: true}, {id: id, newToken: true}, {id: id, keyUnsettled: true, envFromVariable: true, newToken: true}} {
		first := setupNextSteps(n)[0]
		if n.envFromVariable && !strings.Contains(first, "(BERTHY_ENV=prod)") {
			t.Error("missing environment suffix")
		}
		if n.newToken && !strings.Contains(first, "leave out --new-token") {
			t.Error("missing token suffix")
		}
		if strings.Contains(first, "berthy setup -e") || strings.Contains(first, "sudo cat") {
			t.Error("unsafe retry")
		}
	}
	for _, manifest := range []bool{false, true} {
		first := setupNextSteps(nextSteps{id: id, local: true, inferProject: manifest})[0]
		if strings.Contains(first, "--host") || strings.Contains(first, "sudo cat") {
			t.Error("unsafe local instruction")
		}
		if manifest && !strings.Contains(first, "directory of the shop manifest") {
			t.Error("missing manifest instruction")
		}
		if !manifest && !strings.Contains(first, "same --network and --protected") {
			t.Error("missing settings instruction")
		}
	}
}

func TestSetupInterruptedHandoverDoesNotStartSecretsGuide(t *testing.T) {
	home := t.TempDir()
	oldHome := KeyHome
	KeyHome = func() (string, error) { return home, nil }
	t.Cleanup(func() { KeyHome = oldHome })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := freshServer(t, "ops@s1")
	server.keyAnswer = func(context.Context, []byte) (setup.Result, error) { cancel(); return setup.Result{}, context.Canceled }
	withSetupRunner(t, server)
	called := false
	hiddenInput = func() (func() ([]byte, error), bool) { called = true; return nil, false }
	var out, errout bytes.Buffer
	code := runSetupWithContext(ctx, []string{"-e", "p5", "--host", "ops@s1", "--listen", "172.31.250.10:7710", "--network", "p5net", "--manifest-dir", projectDir(t), "--config", filepath.Join(t.TempDir(), "cli.json")}, &out, &errout)
	if code != 0 || called || !strings.Contains(out.String(), "not saved: interrupted") || !strings.Contains(out.String(), "next steps:") || strings.Contains(out.String(), "\nsecrets:") {
		t.Fatal("incorrect interrupted outcome")
	}
}

func TestSetupDryRunDoesNotCopyKey(t *testing.T) {
	home := t.TempDir()
	oldHome := KeyHome
	KeyHome = func() (string, error) { return home, nil }
	t.Cleanup(func() { KeyHome = oldHome })
	server := installedServer(t, "ops@s1")
	withSetupRunner(t, server)
	code, out, _ := runSetupCmd("-e", "p5", "--host", "ops@s1", "--manifest-dir", projectDir(t), "--dry-run", "--json")
	var rep setupReport
	if code != 0 || json.Unmarshal([]byte(out), &rep) != nil || rep.KeyCopy != "" || rep.Steps[len(rep.Steps)-1].Detail != "would keep a copy of the encryption key on this machine" || server.ran("# key copy") {
		t.Fatal("dry run attempted copy")
	}
	if _, err := os.Stat(filepath.Join(home, ".berthy")); !os.IsNotExist(err) {
		t.Fatal("dry run created key folder")
	}
}

func TestSetupRepairsRefusedCopy(t *testing.T) {
	home := t.TempDir()
	oldHome := KeyHome
	KeyHome = func() (string, error) { return home, nil }
	t.Cleanup(func() { KeyHome = oldHome })
	obstacle := filepath.Join(home, ".berthy")
	if err := os.WriteFile(obstacle, []byte("obstacle"), 0600); err != nil {
		t.Fatal(err)
	}
	server := installedServer(t, "ops@s1")
	server.key = bytes.Repeat([]byte{0x27}, 32)
	withSetupRunner(t, server)
	args := []string{"-e", "p5", "--host", "ops@s1", "--manifest-dir", projectDir(t), "--config", filepath.Join(t.TempDir(), "cli.json"), "--json"}
	run := func() setupReport {
		t.Helper()
		code, out, _ := runSetupCmd(args...)
		var rep setupReport
		if code != 0 || json.Unmarshal([]byte(out), &rep) != nil {
			t.Fatal("setup failed")
		}
		return rep
	}
	first := run()
	if first.KeyCopy != "" || first.Steps[14].Status != "skipped" || !strings.Contains(first.NextSteps[0], "run the same berthy setup command again") {
		t.Fatal("incorrect refusal")
	}
	if err := os.Remove(obstacle); err != nil {
		t.Fatal(err)
	}
	repaired := run()
	if repaired.KeyCopy == "" || repaired.Steps[14].Status != "changed" {
		t.Fatal("repair did not save")
	}
}

func TestSetupFailedProvisioningDoesNotCopyKey(t *testing.T) {
	home := t.TempDir()
	oldHome := KeyHome
	KeyHome = func() (string, error) { return home, nil }
	t.Cleanup(func() { KeyHome = oldHome })
	server := &setupServer{probe: "os=Darwin\n", target: "ops@s1"}
	withSetupRunner(t, server)
	code, out, _ := runSetupCmd("-e", "p5", "--host", "ops@s1", "--listen", "172.31.250.10:7710", "--manifest-dir", projectDir(t), "--json")
	var rep setupReport
	if code != 1 || json.Unmarshal([]byte(out), &rep) != nil || rep.KeyCopy != "" || server.ran("# key copy") {
		t.Fatal("copy attempted after failure")
	}
}

func TestKeyHomeDefaultsToUnavailable(t *testing.T) {
	if home, err := KeyHome(); err == nil || home != "" {
		t.Fatal("default key home was enabled")
	}
}

func TestSetupRetryAfterUnsettledRotationOmitsKeyFile(t *testing.T) {
	for _, tc := range []struct {
		name                               string
		unsettled, newToken, envVar, empty bool
	}{
		{name: "unsettled", unsettled: true},
		{name: "unsettled with empty key flag", unsettled: true, empty: true},
		{name: "unsettled with token and environment", unsettled: true, newToken: true, envVar: true},
		{name: "unrelated refusal"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			old := KeyHome
			KeyHome = func() (string, error) { return home, nil }
			t.Cleanup(func() { KeyHome = old })
			key := bytes.Repeat([]byte{0x45}, 32)
			keyFile := filepath.Join(t.TempDir(), "saved.key")
			if err := os.WriteFile(keyFile, key, 0600); err != nil {
				t.Fatal(err)
			}
			server := installedServer(t, "ops@s1")
			server.probe = strings.Replace(server.probe, "key_sha="+strings.Repeat("b", 64), "key_sha="+sha(key), 1)
			server.keyAnswer = func(context.Context, []byte) (setup.Result, error) {
				if tc.unsettled {
					return setup.Result{Code: 3, Stdout: []byte(keywrap.UnsettledLine)}, nil
				}
				return setup.Result{Code: 2}, nil
			}
			withSetupRunner(t, server)
			args := []string{"--host", "ops@s1", "--manifest-dir", projectDir(t), "--config", filepath.Join(t.TempDir(), "cli.json"), "--key-file", keyFile, "--json"}
			if tc.empty {
				args[len(args)-2] = ""
			}
			if tc.envVar {
				t.Setenv("BERTHY_ENV", "p5")
			} else {
				args = append(args, "-e", "p5")
			}
			if tc.newToken {
				args = append(args, "--new-token")
			}
			code, out, _ := runSetupCmd(args...)
			var rep setupReport
			if code != 0 || json.Unmarshal([]byte(out), &rep) != nil || !rep.OK || len(rep.NextSteps) == 0 {
				t.Fatal("setup did not reach retry advice")
			}
			advice := rep.NextSteps[0]
			if strings.Contains(advice, "leave out --key-file") != tc.unsettled {
				t.Errorf("wrong key-file advice: %s", advice)
			}
			if !strings.Contains(advice, "from this same directory and with the same environment") {
				t.Error("lost retry scope")
			}
			if tc.unsettled && !strings.Contains(advice, "Do not restart the agent before that") {
				t.Error("lost rotation recovery order")
			}
			if tc.newToken && !strings.Contains(advice, "leave out --new-token") {
				t.Error("lost new-token advice")
			}
			if tc.envVar && !strings.Contains(advice, "(BERTHY_ENV=p5)") {
				t.Error("lost environment advice")
			}
		})
	}
}
