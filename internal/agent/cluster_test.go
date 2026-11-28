package agent

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"berthy/internal/manifest"
	"berthy/internal/scope"
	"berthy/internal/state"
)

func clusterFileJSON(t *testing.T, f File, cluster any) []byte {
	t.Helper()
	raw, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	if cluster == nil {
		delete(fields, "cluster")
	} else if fields["cluster"], err = json.Marshal(cluster); err != nil {
		t.Fatal(err)
	}
	raw, err = json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestFileRequiresIndependentClusterState(t *testing.T) {
	f := testFile(testHosted("shop", "prod", "shop-prod"))
	h := f.Environments[0]
	for _, tc := range []struct {
		name    string
		cluster any
		want    string
	}{
		{"valid without alerts", map[string]string{"state_dir": "/var/lib/berthy/cluster/state", "key_file": "/var/lib/berthy/cluster/key"}, ""},
		{"absent", nil, "needs state_dir and key_file"},
		{"missing paths", map[string]string{"state_dir": "/cluster/state"}, "needs state_dir and key_file"},
		{"nested state", map[string]string{"state_dir": h.StateDir + "/sub", "key_file": "/cluster/key"}, "paths overlap"},
		{"hosted key inside cluster state", map[string]string{"state_dir": "/etc/./berthy", "key_file": "/cluster/key"}, "paths overlap"},
		{"rotation sibling", map[string]string{"state_dir": "/cluster/state", "key_file": h.KeyFile + state.KeyNextSuffix}, "paths overlap"},
		{"manifest-only field", json.RawMessage(`{"state_dir":"/cluster/state","key_file":"/cluster/key","alerts":{"deploys":"all"}}`), `unknown field "deploys"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseFile(clusterFileJSON(t, f, tc.cluster))
			if err == nil {
				err = got.Validate()
			}
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("validation = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestClusterAndManifestUseTheSameChannelRules(t *testing.T) {
	alerts := manifest.Alerts{
		Reminder: "tomorrow",
		Telegram: &manifest.Telegram{BotTokenSecret: manifest.TunnelTokenSecret, ChatID: "two chats"},
		Webhook:  &manifest.Webhook{URLSecret: "bad name", HeadersSecret: manifest.TunnelTokenSecret},
	}
	m := manifest.Manifest{Project: "shop", Alerts: &alerts}
	var want manifest.Problems
	var paths []string
	for _, problem := range m.Check() {
		if strings.HasPrefix(problem.Path, "alerts.") {
			want = append(want, problem)
			paths = append(paths, problem.Path)
		}
	}
	if !slices.Equal(paths, []string{"alerts.reminder", "alerts.telegram.bot_token_secret", "alerts.telegram.chat_id", "alerts.webhook.url_secret", "alerts.webhook.headers_secret"}) {
		t.Fatalf("manifest alert problem paths = %v", paths)
	}
	f := testFile(testHosted("shop", "prod", "shop-prod"))
	f, err := ParseFile(clusterFileJSON(t, f, map[string]any{"state_dir": "/cluster/state", "key_file": "/cluster/key", "alerts": alerts}))
	if err != nil {
		t.Fatal(err)
	}
	var got manifest.Problems
	if err := f.Validate(); !errors.As(err, &got) || !slices.Equal(got, want) {
		t.Fatalf("cluster problem list = %v (%+v), want %+v", err, got, want)
	}
}

func TestOpenClaimsClusterStateBeforeOpeningEnvironments(t *testing.T) {
	sh, f := hostedFile(t, scope.ID{Project: "shop", Environment: "prod"})
	dir, key := f.Cluster.StateDir, f.Cluster.KeyFile
	ctx := context.Background()
	s, err := state.Open(dir, key, "prod")
	if err != nil {
		t.Fatal(err)
	}
	claimErr, closeErr := s.Claim(ctx, "shop"), s.Close()
	if err := errors.Join(claimErr, closeErr); err != nil {
		t.Fatal(err)
	}
	h, err := Open(sh, f)
	if h != nil {
		h.Close()
	}
	var owner *state.OwnerError
	if !errors.As(err, &owner) || owner.Have != "shop/prod" || owner.Want != "(cluster)/cluster" {
		t.Errorf("Open = %v, want a cluster ownership refusal", err)
	}
	if _, err := os.Stat(filepath.Join(f.Environments[0].StateDir, "state.db")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("environment state was touched: %v", err)
	}
	s, err = state.Open(dir, key, "prod")
	if err != nil {
		t.Fatalf("cluster claim failure left the state open: %v", err)
	}
	defer s.Close()
	if err := s.CheckOwner(ctx, "shop"); err != nil {
		t.Fatal(err)
	}
}

func TestAnEnvironmentCannotClaimClusterState(t *testing.T) {
	sh, f := hostedFile(t, scope.ID{Project: "shop", Environment: "prod"})
	h, err := Open(sh, f)
	if err != nil {
		t.Fatal(err)
	}
	h.Close()
	s, err := state.Open(f.Cluster.StateDir, f.Cluster.KeyFile, "prod")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var owner *state.OwnerError
	if err := s.Claim(context.Background(), "shop"); !errors.As(err, &owner) || owner.Have != "(cluster)/cluster" || owner.Want != "shop/prod" {
		t.Fatalf("environment claim = %v, want a cluster ownership refusal", err)
	}
}

func TestClusterStateSurvivesHostCloseAndReopen(t *testing.T) {
	sh, f := hostedFile(t, scope.ID{Project: "shop", Environment: "prod"})
	ctx := context.Background()
	h, err := Open(sh, f)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	s := h.cluster.store
	if err := s.CheckOwner(ctx, "(cluster)"); err != nil {
		t.Fatalf("host did not claim cluster state: %v", err)
	}
	actor := state.Actor{ID: "setup", Name: "setup"}
	secret := []byte("fixture credential")
	if err := s.PutSecret(ctx, actor, state.SecretInfo{Name: "HOOK"}, secret); err != nil {
		t.Fatal(err)
	}
	check := state.CheckState{Name: "public_ports", State: "warning"}
	if err := s.PutCheck(ctx, check); err != nil {
		t.Fatal(err)
	}
	h.Close()
	h, err = Open(sh, f)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	env := h.order[0].deployment.Store
	if _, _, err := env.Secret(ctx, "HOOK"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("environment secret lookup = %v, want absent", err)
	}
	if rows, err := env.Checks(ctx); err != nil || len(rows) != 0 {
		t.Fatalf("environment checks = %v, %v", rows, err)
	}
	s = h.cluster.store
	if _, got, err := s.Secret(ctx, "HOOK"); err != nil || !bytes.Equal(got, secret) {
		t.Fatalf("cluster secret was not retained: %v", err)
	}
	if rows, err := s.Checks(ctx); err != nil || !slices.Equal(rows, []state.CheckState{check}) {
		t.Fatalf("cluster checks = %v, %v", rows, err)
	}
}
