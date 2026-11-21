package cli

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// An agent of a release before projects serves only /v1/environments/<environment>/, so every request of this CLI gets 404 from
// it. The person is told to upgrade the agent, whatever the command.
func TestACommandOnAnAgentWithoutProjectsSaysToUpgradeIt(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/environments/dev/agent" && r.Header.Get("Authorization") == "Bearer test-token" {
			fmt.Fprint(w, `{"environment":"dev","version":"v0.8.0"}`)
			return
		}
		w.WriteHeader(404)
		fmt.Fprint(w, `{"code":"not_found","message":"not found"}`)
	}))
	defer server.Close()
	cfg := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(cfg, []byte(fmt.Sprintf(`{"projects":{"demo":{"environments":{"dev":{"url":%q,"token_env":"BERTHY_TEST_TOKEN"}}}}}`, server.URL)), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BERTHY_TEST_TOKEN", "test-token")
	var out, errs bytes.Buffer
	code := Run([]string{"ps", "-e", "dev", "--config", cfg}, &out, &errs)
	if code != 1 || !strings.Contains(errs.String(), "the agent runs an older release than this CLI; upgrade it with berthy setup") {
		t.Fatalf("exit %d: %s", code, errs.String())
	}
}
