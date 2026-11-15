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
