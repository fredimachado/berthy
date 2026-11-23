package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestSecretListMarksAPasswordStoredWithoutItsRegistry(t *testing.T) {
	var requests []string
	server := answeringAgent(&requests, `[{"name":"PULL_TOKEN","registry":""}]`, `{}`)
	defer server.Close()
	cfg := ingressCLIConfig(t, server.URL)
	dir := secretManifestDir(t, "project: demo\nnetwork: overlay\nregistries:\n  ghcr.io:\n    username: user\n    password_secret: PULL_TOKEN\napps:\n  api:\n    kind: image\n    image: ghcr.io/acme/api\n    roles:\n      web:\n        port: 8080\n")
	var out, errout bytes.Buffer
	code := Run([]string{"secret", "list", "-e", "dev", "--config", cfg, "--manifest-dir", dir}, &out, &errout)
	if code != 0 || !strings.Contains(out.String(), "missing: PULL_TOKEN (stored without registry ghcr.io; needed by registry ghcr.io)") {
		t.Fatalf("exit %d stdout %q stderr %q", code, out.String(), errout.String())
	}
}
