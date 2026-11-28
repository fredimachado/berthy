package cli

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"

	"berthy/internal/agentclient"
	"berthy/internal/setup"
)

type removedContextServer struct {
	t       *testing.T
	cleared bool
}

func (*removedContextServer) Target() string { return "ops@fixture" }

func (r *removedContextServer) Run(_ context.Context, script string, input io.Reader) (setup.Result, error) {
	r.t.Helper()
	if input != nil {
		r.t.Fatalf("unexpected upload: %q", script)
	}
	cfg := `{"listen":"127.0.0.1:7710","environments":[{"project":"peer","environment":"dev","state_dir":"/peer/state","key_file":"/peer/key","tokens":[{"digest":"` +
		strings.Repeat("a", 64) + `","scope":"admin"}]}]}`
	switch {
	case strings.Contains(script, "uid=$(id -u)"):
		return setup.Result{Stdout: []byte("0\nroot\n")}, nil
	case strings.HasPrefix(script, "# inspect removal configuration\n"):
		return setup.Result{Stdout: []byte("present\n")}, nil
	case script == "cat /etc/berthy/agent.json":
		return setup.Result{Stdout: []byte(cfg)}, nil
	case strings.HasPrefix(script, "# read removal record\n"):
		record := fmt.Sprintf(`{"ID":{"Project":"demo","Environment":"dev"},"StateDir":"/demo/state","KeyFile":"/demo/key",
"StateIdentity":{"Device":1,"Inode":2},"KeyIdentity":{"Device":1,"Inode":3},"NextIdentity":null,"PrevIdentity":null,
"Network":"","NetworkID":"","Archive":"/demo/state.removed-20261128T020000Z","OriginalSHA":%q,"CandidateSHA":%q,"Activated":true}`,
			strings.Repeat("b", 64), sha([]byte(cfg)))
		return setup.Result{Stdout: []byte(record)}, nil
	case strings.HasPrefix(script, "# remove state and keys\n"):
		return setup.Result{Stdout: []byte("clean\n")}, nil
	case strings.HasPrefix(script, "# clear removal record\n"):
		r.cleared = true
	default:
		r.t.Fatalf("unexpected command: %q", script)
	}
	return setup.Result{}, nil
}

func TestRemovalAfterContextDeletesStoredToken(t *testing.T) {
	store, cfg, _, _ := tokenFixture(t)
	before := store.Calls()
	code, out, diag := runCLI("context", "remove", "--project", "demo", "-e", "dev", "--delete-token", "--config", cfg)
	if code != 0 || !strings.Contains(out, "deleted the token") || diag != "" || store.Calls() != before+1 {
		t.Fatalf("context remove: %d, %q, %q, store calls = %d", code, out, diag, store.Calls()-before)
	}
	r := &removedContextServer{t: t}
	s := &setup.Session{R: r, Opts: setup.Options{Project: "demo", Env: "dev", CLIConfig: cfg, Tokens: store}}
	ok := setup.Run(context.Background(), s, setup.RemovalSteps(false), func(result setup.StepResult) {
		if result.Status == setup.StatusFailed {
			t.Error(result.Detail)
		}
	})
	if !ok || !r.cleared || store.Calls() != before+1 {
		t.Fatalf("removal: success=%t, cleared=%t, store calls=%d", ok, r.cleared, store.Calls()-before)
	}
	local, err := agentclient.ReadConfig(cfg)
	if err != nil || len(local.Projects) != 0 {
		t.Fatalf("local configuration after removal: %+v, %v", local, err)
	}
}
