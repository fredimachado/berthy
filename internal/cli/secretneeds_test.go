package cli

import (
	"berthy/internal/manifest"
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

func TestUnmetSecrets(t *testing.T) {
	plain := secretManifestDir(t, "project: demo\nnetwork: overlay\napps:\n  api:\n    kind: image\n    image: ghcr.io/a/b\n    env:\n      secret: [PULL_TOKEN]\n    roles:\n      web:\n        port: 8080\n")
	reg := secretManifestDir(t, "project: demo\nnetwork: overlay\nregistries:\n  ghcr.io:\n    username: u\n    password_secret: PULL_TOKEN\napps:\n  api:\n    kind: image\n    image: ghcr.io/a/b\n    roles:\n      web:\n        port: 8080\n")
	shared := secretManifestDir(t, "project: demo\nnetwork: overlay\nregistries:\n  ghcr.io:\n    username: u\n    password_secret: PULL_TOKEN\n  example.com:\n    username: u\n    password_secret: PULL_TOKEN\napps:\n  api:\n    kind: image\n    image: ghcr.io/a/b\n    roles:\n      web:\n        port: 8080\n")
	cases := []struct {
		dir  string
		held []storedSecret
		want int
	}{
		{plain, nil, int(unmetMissing)}, {plain, []storedSecret{{Name: "PULL_TOKEN"}}, -1},
		{reg, nil, int(unmetMissing)}, {reg, []storedSecret{{Name: "PULL_TOKEN"}}, int(unmetRegistry)},
		{reg, []storedSecret{{Name: "PULL_TOKEN", Registry: "ghcr.io"}}, -1},
		{reg, []storedSecret{{Name: "PULL_TOKEN", Registry: "old.example.com"}}, int(unmetRegistry)},
		{plain, []storedSecret{{Name: "PULL_TOKEN", Registry: "ghcr.io"}}, int(unmetNotPlain)},
		{shared, nil, int(unmetShared)}, {shared, []storedSecret{{Name: "PULL_TOKEN", Registry: "ghcr.io"}}, int(unmetShared)},
	}
	for i, tc := range cases {
		m, err := manifest.Load(tc.dir, "dev")
		if err != nil {
			t.Fatal(err)
		}
		got := unmetSecrets(m, tc.held)
		if tc.want == -1 {
			if len(got) != 0 {
				t.Errorf("case %d: %+v", i, got)
			}
		} else if len(got) != 1 || int(got[0].Kind) != tc.want {
			t.Errorf("case %d: %+v", i, got)
		}
	}
}

func TestUnmetSecretsRegistryPasswordAlsoNeededByApp(t *testing.T) {
	text := registryManifest("ghcr.io", "ghcr.io/acme/api")
	text = strings.Replace(text, "    roles:\n", "    env:\n      secret: [REGISTRY_TOKEN]\n    roles:\n", 1)
	m, err := manifest.Load(secretManifestDir(t, text), "dev")
	if err != nil {
		t.Fatal(err)
	}
	got := unmetSecrets(m, []storedSecret{{Name: "REGISTRY_TOKEN", Registry: "ghcr.io"}})
	if len(got) == 0 {
		t.Fatal("registry password also read by an app was reported usable")
	}
}
