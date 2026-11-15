package manifest

import (
	"fmt"
	"strings"
	"testing"
)

const reservedMessage = "TUNNEL_TOKEN is reserved for managed ingress"

// webApp is a minimal image app that every ingress manifest in these tests includes.
const webApp = `"apps":{"api":{"kind":"image","image":"ghcr.io/acme/api","roles":{"web":{}}}}`

func TestTunnelTokenIsReservedInEverySecretSlot(t *testing.T) {
	cases := map[string]string{
		"app secret":  `{"project":"demo","network":"demo","apps":{"api":{"kind":"image","image":"ghcr.io/acme/api","roles":{"web":{}},"env":{"secret":["TUNNEL_TOKEN"]}}}}`,
		"app plain":   `{"project":"demo","network":"demo","apps":{"api":{"kind":"image","image":"ghcr.io/acme/api","roles":{"web":{}},"env":{"plain":{"TUNNEL_TOKEN":"x"}}}}}`,
		"registry":    `{"project":"demo","network":"demo","registries":{"ghcr.io":{"username":"ci","password_secret":"TUNNEL_TOKEN"}},` + webApp + `}`,
		"cloudflare":  `{"project":"demo","network":"demo","ingress":{"cloudflare":{"account_id":"acct","api_token_secret":"TUNNEL_TOKEN","image":"cloudflare/cloudflared:2026.11.0"}},` + webApp + `}`,
		"destination": `{"project":"demo","network":"demo","destinations":{"offsite":{"type":"s3","bucket":"b","access_key_secret":"TUNNEL_TOKEN","secret_key_secret":"S3_SECRET"}},` + webApp + `}`,
		"telemetry":   `{"project":"demo","network":"demo","telemetry":{"otlp_endpoint":"http://collector:4318","headers_secret":"TUNNEL_TOKEN"},` + webApp + `}`,
	}
	for name, input := range cases {
		if _, err := DecodeJSON([]byte(input)); err == nil || !strings.Contains(err.Error(), reservedMessage) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestIngressAppNameIsReservedOnlyWithIngress(t *testing.T) {
	withIngress := `{"project":"demo","network":"demo","ingress":{"cloudflare":{"account_id":"acct","api_token_secret":"CF_TOKEN","image":"cloudflare/cloudflared:2026.11.0"}},"apps":{"ingress":{"kind":"image","image":"ghcr.io/acme/edge","roles":{"web":{}}}}}`
	if _, err := DecodeJSON([]byte(withIngress)); err == nil || !strings.Contains(err.Error(), "app name ingress is reserved for the managed ingress connector") {
		t.Fatalf("reserved app name accepted: %v", err)
	}
	withoutIngress := `{"project":"demo","network":"demo","apps":{"ingress":{"kind":"image","image":"ghcr.io/acme/edge","roles":{"web":{}}}}}`
	if _, err := DecodeJSON([]byte(withoutIngress)); err != nil {
		t.Fatalf("app name refused without ingress: %v", err)
	}
}

func TestCloudflareImageNeedsTagOrDigest(t *testing.T) {
	digest := "sha256:" + strings.Repeat("c", 64)
	for image, valid := range map[string]bool{
		"cloudflare/cloudflared":                     false,
		"cloudflare/cloudflared:2026.11.0":           true,
		"cloudflare/cloudflared@" + digest:           true,
		"cloudflare/cloudflared:2026.11.0@" + digest: true,
	} {
		input := fmt.Sprintf(`{"project":"demo","network":"demo","ingress":{"cloudflare":{"account_id":"acct","api_token_secret":"CF_TOKEN","image":%q}},%s}`, image, webApp)
		if _, err := DecodeJSON([]byte(input)); (err == nil) != valid {
			t.Errorf("%s: valid=%v err=%v", image, valid, err)
		}
	}
}

// connectorManifest builds a valid ingress manifest whose connector image is image.
func connectorManifest(t *testing.T, image, replicas string) *Manifest {
	t.Helper()
	extra := ""
	if replicas != "" {
		extra = `,"replicas":` + replicas
	}
	input := fmt.Sprintf(`{"project":"demo","network":"demo","ingress":{"cloudflare":{"account_id":"acct","api_token_secret":"CF_TOKEN","image":%q%s}},%s}`, image, extra, webApp)
	m, err := DecodeJSON([]byte(input))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// requireConnectorValid puts the connector app into a copy of m, as Berthy would, and validates the copy.
// The copy has no ingress block, so this checks the connector against the same per-app rules as user apps.
func requireConnectorValid(t *testing.T, m *Manifest, app App) {
	t.Helper()
	cp := *m
	cp.Ingress = nil
	cp.Apps = map[string]App{ConnectorAppName: app, "api": m.Apps["api"]}
	if err := cp.Validate(); err != nil {
		t.Fatalf("connector app fails validation: %v", err)
	}
}

func TestConnectorAppForTagAndDigest(t *testing.T) {
	m := connectorManifest(t, "cloudflare/cloudflared:2026.11.0", "")
	app, version, err := m.ConnectorApp()
	if err != nil || version != "2026.11.0" || app.Image != "docker.io/cloudflare/cloudflared" || app.Kind != "image" {
		t.Fatalf("tag: %+v %q %v", app, version, err)
	}
	connector := app.Roles["connector"]
	if connector.Replicas == nil || *connector.Replicas != 1 {
		t.Fatalf("default replicas: %+v", connector.Replicas)
	}
	wantCommand := []string{"tunnel", "--no-autoupdate", "--metrics", "0.0.0.0:20241", "run"}
	if fmt.Sprint(connector.Command) != fmt.Sprint(wantCommand) {
		t.Fatalf("command %v", connector.Command)
	}
	if connector.Health == nil || fmt.Sprint(connector.Health.Test) != "[CMD cloudflared tunnel --metrics 127.0.0.1:20241 ready]" || *connector.Health.Retries != 3 || connector.Health.StartPeriod != "30s" {
		t.Fatalf("health %+v", connector.Health)
	}
	if fmt.Sprint(app.Env.Secret) != "[TUNNEL_TOKEN]" || len(app.Env.Plain) != 0 {
		t.Fatalf("env %+v", app.Env)
	}
	if app.Deploy.DeployTimeout != "180s" || app.Deploy.DrainTimeout != "30s" || *app.Deploy.Retain != 3 {
		t.Fatalf("deploy %+v", app.Deploy)
	}
	requireConnectorValid(t, m, app)

	pinned := "sha256:" + strings.Repeat("b", 64)
	m = connectorManifest(t, "cloudflare/cloudflared:2026.11.0@"+pinned, "3")
	app, version, err = m.ConnectorApp()
	if err != nil || version != pinned || *app.Roles["connector"].Replicas != 3 {
		t.Fatalf("tag and digest: %q %v replicas=%v", version, err, app.Roles["connector"].Replicas)
	}
	requireConnectorValid(t, m, app)

	m = connectorManifest(t, "cloudflare/cloudflared@"+pinned, "")
	app, version, err = m.ConnectorApp()
	if err != nil || version != pinned || app.Image != "docker.io/cloudflare/cloudflared" {
		t.Fatalf("digest only: %+v %q %v", app, version, err)
	}
	requireConnectorValid(t, m, app)
}

func TestConnectorAppNeedsIngress(t *testing.T) {
	m := &Manifest{Project: "demo", Network: "demo"}
	if _, _, err := m.ConnectorApp(); err == nil {
		t.Fatal("connector built without ingress")
	}
}

func TestTunnelNameDefaultsToProjectAndEnvironment(t *testing.T) {
	m := connectorManifest(t, "cloudflare/cloudflared:2026.11.0", "")
	if got := m.TunnelName("prod"); got != "demo-prod" {
		t.Fatalf("default tunnel %q", got)
	}
	m.Ingress.Cloudflare.Tunnel = "myapp-prod"
	if got := m.TunnelName("prod"); got != "myapp-prod" {
		t.Fatalf("configured tunnel %q", got)
	}
}
