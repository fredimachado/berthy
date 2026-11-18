package manifest

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// TestSecretNeedsNamesEverySecretWithWhatNeedsIt checks that SecretNeeds lists each secret the manifest names once, sorted by
// name, with every part that needs it: the app's plain and mapped variables, registries, destinations, telemetry, the Telegram
// bot token and the managed ingress token.
func TestSecretNeedsNamesEverySecretWithWhatNeedsIt(t *testing.T) {
	input := `project: demo
network: demo
registries:
  ghcr.io:
    username: u
    password_secret: GHCR_TOKEN
destinations:
  backups:
    type: s3
    endpoint: https://s3.example.com
    bucket: berthy-backups
    access_key_secret: S3_KEY
    secret_key_secret: S3_SECRET
telemetry:
  otlp_endpoint: https://otel.example.com
  headers_secret: OTLP_HEADERS
alerts:
  telegram:
    bot_token_secret: BOT_TOKEN
    chat_id: "42"
  webhook:
    url_secret: HOOK_URL
    headers_secret: HOOK_HEADERS
  heartbeat:
    url_secret: BEAT_URL
ingress:
  cloudflare:
    account_id: acct
    api_token_secret: CF_TOKEN
    image: cloudflare/cloudflared:2026.11.0
apps:
  api:
    kind: image
    image: ghcr.io/acme/api
    roles:
      web: {}
    env:
      plain:
        LOG_LEVEL: info
      secret:
        - DATABASE_URL: API_DB
        - GHCR_TOKEN
        - JWT_KEY
  shop:
    kind: compose
    file: compose.yml
    env:
      secret:
        - API_DB
`
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "berthy.yml"), []byte(input), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "compose.yml"), []byte("services:\n  db:\n    image: postgres:16\n"), 0600); err != nil {
		t.Fatal(err)
	}
	m, err := Load(dir, "dev")
	if err != nil {
		t.Fatal(err)
	}
	got := m.SecretNeeds()
	want := []SecretNeed{
		{Name: "API_DB", NeededBy: []string{"app api", "app shop"}},
		{Name: "BEAT_URL", NeededBy: []string{"alerts heartbeat"}},
		{Name: "BOT_TOKEN", NeededBy: []string{"alerts telegram"}},
		{Name: "CF_TOKEN", NeededBy: []string{"ingress cloudflare"}},
		{Name: "GHCR_TOKEN", NeededBy: []string{"app api", "registry ghcr.io"}},
		{Name: "HOOK_HEADERS", NeededBy: []string{"alerts webhook"}},
		{Name: "HOOK_URL", NeededBy: []string{"alerts webhook"}},
		{Name: "JWT_KEY", NeededBy: []string{"app api"}},
		{Name: "OTLP_HEADERS", NeededBy: []string{"telemetry"}},
		{Name: "S3_KEY", NeededBy: []string{"destination backups"}},
		{Name: "S3_SECRET", NeededBy: []string{"destination backups"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("SecretNeeds:\n got %+v\nwant %+v", got, want)
	}
}

// TestSecretNeedsIsEmptyForAManifestWithoutSecrets checks that a manifest that names no secret has none, and that the list is
// empty rather than nil, so it is printed as an empty list.
func TestSecretNeedsIsEmptyForAManifestWithoutSecrets(t *testing.T) {
	m := &Manifest{Project: "demo", Apps: map[string]App{"web": {Kind: "image", Image: "ghcr.io/acme/web", Env: Env{Plain: map[string]string{"A": "b"}}}}}
	if got := m.SecretNeeds(); got == nil || len(got) != 0 {
		t.Fatalf("SecretNeeds %#v, want an empty list", got)
	}
}
