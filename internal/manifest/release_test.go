package manifest

import (
	"strings"
	"testing"
)

// The connector's stored release carries the tunnel token. Only the connector's own release may decode with it.
func TestConnectorReleaseDecodesOnlyAsConnector(t *testing.T) {
	stored := []byte(connectorStored)
	if _, err := DecodeRelease(stored, true); err != nil {
		t.Fatalf("connector release refused: %v", err)
	}
	if _, err := DecodeRelease(stored, false); err == nil || !strings.Contains(err.Error(), reservedMessage) {
		t.Fatalf("release of another app accepted: %v", err)
	}
	if _, err := DecodeJSON(stored); err == nil {
		t.Fatal("connector manifest accepted as a sent manifest")
	}
}

const connectorStored = `{"project":"demo","network":"demo","apps":{"ingress":{"kind":"image","image":"docker.io/cloudflare/cloudflared","roles":{"connector":{"replicas":1}},"env":{"secret":["TUNNEL_TOKEN"]}}}}`

func TestReleaseRetentionUsesTheAppliedAppsSetting(t *testing.T) {
	m, err := DecodeRelease([]byte(`{"project":"demo","apps":{"app":{"kind":"image","image":"example/web","roles":{"web":{}},"deploy":{"retain":2}}}}`), false)
	if err != nil {
		t.Fatal(err)
	}
	connector, _, err := connectorManifest(t, "cloudflare/cloudflared:2026.11.0", "").ConnectorApp()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		deploy Deploy
		want   int
	}{
		{"omitted", Deploy{}, 5},
		{"explicit", m.Apps["app"].Deploy, 2},
		{"connector", connector.Deploy, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.deploy.ReleaseRetention(); got != tc.want {
				t.Fatalf("release retention = %d, want %d", got, tc.want)
			}
		})
	}
}
