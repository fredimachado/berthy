package setup

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"berthy/internal/agentclient"
	"berthy/internal/credstore/credtest"
)

func TestRemovalFinishesLocalContextAfterServerCleanup(t *testing.T) {
	for _, name := range []string{"sibling remains", "project pruned", "missing entry", "missing file", "other server", "preview", "write fails and retries"} {
		t.Run(name, func(t *testing.T) {
			r, opts := newRemovalRunner(t)
			store := credtest.NewMemory()
			opts.Tokens = store
			read := func() agentclient.Config {
				t.Helper()
				c, err := agentclient.ReadConfig(opts.CLIConfig)
				if err != nil {
					t.Fatal(err)
				}
				return c
			}
			local := read()
			add := func(project, env, url, token string) {
				t.Helper()
				if _, err := local.Put(project, env, agentclient.Environment{URL: url, TokenEnv: token}); err != nil {
					t.Fatal(err)
				}
			}
			add("peer", "dev", "http://127.0.0.1:7710", "PEER_TOKEN")
			switch name {
			case "sibling remains":
				add("alpha", "prod", "http://127.0.0.1:7710", "PROD_TOKEN")
			case "other server":
				add("alpha", "dev", "http://127.0.0.2:7710", "FIXTURE_TOKEN")
			case "missing entry":
				local.Remove("alpha", "dev")
			}
			if err := agentclient.WriteConfig(opts.CLIConfig, local); err != nil {
				t.Fatal(err)
			}
			original, err := os.ReadFile(opts.CLIConfig)
			if err != nil {
				t.Fatal(err)
			}
			if name == "missing file" {
				if err := os.Remove(opts.CLIConfig); err != nil {
					t.Fatal(err)
				}
			}
			target := local.Projects["alpha"].Environments["dev"]
			if target != (agentclient.Environment{}) {
				if err := store.Set(agentclient.StoreKey("alpha", "dev", target), "fixture-token"); err != nil {
					t.Fatal(err)
				}
			}
			storeCalls := store.Calls()
			run := func() (string, bool) {
				t.Helper()
				var detail string
				s := &Session{R: r, Opts: opts, DryRun: name == "preview"}
				ok := Run(context.Background(), s, RemovalSteps(false), func(result StepResult) {
					if result.Status == StatusFailed || result.Step == "cli_config" {
						detail = result.Detail
					}
				})
				return detail, ok
			}
			if name == "write fails and retries" {
				if runtime.GOOS != "linux" || os.Geteuid() == 0 {
					t.Skip("requires directory write permissions enforced against a non-root user")
				}
				dir := filepath.Dir(opts.CLIConfig)
				if err := os.Chmod(dir, 0500); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := os.Chmod(dir, 0700); err != nil {
						t.Error(err)
					}
				})
				detail, ok := run()
				if ok || !strings.Contains(detail, "server cleanup finished") || r.record == nil || read().Projects["alpha"].Environments["dev"] != target {
					t.Fatalf("write refusal = %q, success=%t, record retained=%t", detail, ok, r.record != nil)
				}
				r.checkMutations("record:pending,stage,swap,restart,record:confirmed,state,network")
				if err := os.Chmod(dir, 0700); err != nil {
					t.Fatal(err)
				}
				r.mutations = nil
			}
			detail, ok := run()
			if !ok || r.record != nil || store.Calls() != storeCalls {
				t.Fatalf("result=%q, success=%t, record retained=%t, store calls=%d", detail, ok, r.record != nil, store.Calls()-storeCalls)
			}
			got := read()
			switch name {
			case "preview", "other server", "missing entry":
				after, err := os.ReadFile(opts.CLIConfig)
				if err != nil || !bytes.Equal(original, after) {
					t.Fatalf("local config changed: %v", err)
				}
			case "missing file":
				if _, err := os.Stat(opts.CLIConfig); !os.IsNotExist(err) {
					t.Fatalf("created a missing local config: %v", err)
				}
			default:
				if _, present := got.Projects["alpha"].Environments["dev"]; present {
					t.Fatal("target CLI entry survived")
				}
			}
			if name != "missing file" && got.Projects["peer"].Environments["dev"] != local.Projects["peer"].Environments["dev"] {
				t.Fatal("another project's entry changed")
			}
			switch name {
			case "other server":
				if !strings.Contains(detail, "berthy context remove --project alpha -e dev") {
					t.Fatalf("missing explicit removal advice: %q", detail)
				}
			case "sibling remains":
				if got.Projects["alpha"].Environments["prod"] != local.Projects["alpha"].Environments["prod"] {
					t.Fatal("sibling CLI environment changed")
				}
			case "project pruned":
				if _, present := got.Projects["alpha"]; present {
					t.Fatal("empty CLI project survived")
				}
			}
			switch name {
			case "preview":
				r.checkMutations("")
			case "write fails and retries":
				r.checkMutations("clear")
			default:
				r.checkMutations("record:pending,stage,swap,restart,record:confirmed,state,network,clear")
			}
		})
	}
}
