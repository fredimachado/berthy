package agent

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"berthy/internal/api"
	"berthy/internal/deploy"
)

func TestComposeCommandConfig(t *testing.T) {
	if got := strings.Join((Config{}).composeCommand(), " "); got != "docker compose" {
		t.Fatalf("default command %q", got)
	}
	if got := (Config{ComposeCommand: []string{"/opt/compose"}}).composeCommand(); len(got) != 1 || got[0] != "/opt/compose" {
		t.Fatalf("configured command %q", got)
	}
	if got := dockerHost(""); got != "unix:///var/run/docker.sock" {
		t.Fatalf("default host %q", got)
	}
	if got := dockerHost("/srv/docker.sock"); got != "unix:///srv/docker.sock" {
		t.Fatalf("host %q", got)
	}
	base := Config{Project: "demo", Environment: "lab", Listen: "127.0.0.1:19387", Tokens: []Token{{Digest: Digest("bty.demo.lab.x"), Scope: "read"}}}
	base.ComposeCommand = []string{"docker", ""}
	if err := base.Validate(); err == nil {
		t.Fatal("empty compose_command element accepted")
	}
	base.ComposeCommand = []string{"/usr/libexec/docker/cli-plugins/docker-compose"}
	if err := base.Validate(); err != nil {
		t.Fatalf("valid compose_command refused: %v", err)
	}
}

func TestRecreateFlagNeedsAdministratorAtTheHandler(t *testing.T) {
	s := openIngressStore(t)
	readToken := "bty.demo.lab." + strings.Repeat("a", 64)
	deployToken := "bty.demo.lab." + strings.Repeat("b", 64)
	adminToken := "bty.demo.lab." + strings.Repeat("c", 64)
	cfg := Config{Project: "demo", Environment: "lab", Tokens: []Token{{Digest: Digest(readToken), Scope: "read"}, {Digest: Digest(deployToken), Scope: "deploy"}, {Digest: Digest(adminToken), Scope: "admin"}}}
	h := NewDeploymentHandler(cfg, nil, nil, &deploy.Controller{Store: s, Environment: "lab"})
	body := `{"app":"db","dry_run":true,"allow_recreate":true,"compose_file":"services: {}\n","manifest":{"project":"demo","network":"berthy","apps":{"db":{"kind":"compose","file":"compose.yaml"}}}}`
	call := func(token string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/v1/projects/demo/environments/lab/deploys", strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := call(deployToken); w.Code != http.StatusForbidden {
		t.Fatalf("deploy scope with --allow-recreate: %d %s", w.Code, w.Body.String())
	}
	// The administrator gets past the scope check; without a Compose runner the dry run then refuses.
	if w := call(adminToken); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "Compose unavailable") {
		t.Fatalf("administrator dry run: %d %s", w.Code, w.Body.String())
	}
}

func TestDoctorListsComposeCheckAsNonCritical(t *testing.T) {
	s := openIngressStore(t)
	readToken := "bty.demo.lab." + strings.Repeat("a", 64)
	cfg := Config{Project: "demo", Environment: "lab", StateDir: t.TempDir(), DockerSocket: "/nonexistent/berthy-test.sock", Tokens: []Token{{Digest: Digest(readToken), Scope: "read"}}}
	h := NewDeploymentHandler(cfg, nil, nil, &deploy.Controller{Store: s, Environment: "lab"})
	r := httptest.NewRequest(http.MethodGet, "/v1/projects/demo/environments/lab/doctor", nil)
	r.Header.Set("Authorization", "Bearer "+readToken)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	var body struct {
		Checks []api.Check `json:"checks"`
	}
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"name":"compose"`) {
		t.Fatalf("doctor %d %s", w.Code, w.Body.String())
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	for _, c := range body.Checks {
		if c.Name == "compose" && c.Critical {
			t.Fatal("compose check is critical")
		}
	}
}
