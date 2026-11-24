package manifest

import (
	"encoding/json"
	"reflect"
	"slices"
	"testing"
)

const scopedManifest = `{"project":"demo","network":"demo","protected":true,
"registries":{"ghcr.io":{"username":"bot","password_secret":"GHCR_TOKEN"}},
"apps":{"api":{"kind":"image","image":"ghcr.io/acme/api","roles":{"web":{}}},"worker":{"kind":"image","image":"ghcr.io/acme/worker","roles":{"main":{}}}},
"jobs":{"tick":{"app":"api","command":["true"],"schedule":"* * * * *"},"sweep":{"app":"worker","command":["true"],"schedule":"0 * * * *"}},
"destinations":{"offsite":{"type":"s3","bucket":"bkt","access_key_secret":"A","secret_key_secret":"S"}},
"backups":{"uploads":{"app":"api","volume":"uploads","destination":"offsite"}},
"probes":{"site":{"http":"http://127.0.0.1:8080/"},"api-up":{"service":{"app":"api","port":8080}},"worker-up":{"service":{"app":"worker","port":9000}}},
"peers":{"prod":"https://prod.example.com:443"},
"alerts":{"telegram":{"bot_token_secret":"TELEGRAM","chat_id":"1"}}}`

func decodeScoped(t *testing.T, doc string) Manifest {
	t.Helper()
	m, err := DecodeJSON([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	return *m
}

func sameManifest(t *testing.T, got, want Manifest) {
	t.Helper()
	g, _ := json.Marshal(got)
	w, _ := json.Marshal(want)
	if string(g) != string(w) {
		t.Fatalf("manifests differ\n got %s\nwant %s", g, w)
	}
}

func TestEnvScopeKeepsOnlyTheEnvironmentsSettings(t *testing.T) {
	m := decodeScoped(t, scopedManifest)
	env := m.EnvScope()
	if len(env.Apps) != 0 || len(env.Jobs) != 0 || len(env.Backups) != 0 {
		t.Fatalf("app-scoped members left: %+v", env)
	}
	if len(env.Probes) != 1 || env.Probes["site"].HTTP == "" {
		t.Fatalf("probes: %+v", env.Probes)
	}
	if env.Project != "demo" || env.Network != "demo" || !env.Protected || env.Alerts == nil || len(env.Peers) != 1 || len(env.Destinations) != 1 || len(env.Registries) != 1 {
		t.Fatalf("environment members lost: %+v", env)
	}
	if len(m.Apps) != 2 || len(m.Probes) != 3 {
		t.Fatal("EnvScope changed the manifest it was called on")
	}
}

func TestAppScopeKeepsOnlyThatAppsPart(t *testing.T) {
	m := decodeScoped(t, scopedManifest)
	a := m.AppScope("api")
	if len(a.Apps) != 1 || a.Apps["api"].Image == "" || len(a.Jobs) != 1 || a.Jobs["tick"].App != "api" || len(a.Backups) != 1 || len(a.Probes) != 1 || a.Probes["api-up"].Service == nil {
		t.Fatalf("api scope: %+v", a)
	}
	if a.Project != "" || a.Alerts != nil || len(a.Peers) != 0 || len(a.Destinations) != 0 {
		t.Fatalf("environment members in an app scope: %+v", a)
	}
	w := m.AppScope("worker")
	if len(w.Jobs) != 1 || w.Jobs["sweep"].App != "worker" || len(w.Backups) != 0 || len(w.Probes) != 1 {
		t.Fatalf("worker scope: %+v", w)
	}
	if got := m.AppScope("nobody"); len(got.Apps)+len(got.Jobs)+len(got.Backups)+len(got.Probes) != 0 {
		t.Fatalf("scope of an unknown app: %+v", got)
	}
}

func TestAssembleOfTheScopesIsTheManifest(t *testing.T) {
	m := decodeScoped(t, scopedManifest)
	got := Assemble(m.EnvScope(), map[string]Manifest{"api": m.AppScope("api"), "worker": m.AppScope("worker")})
	sameManifest(t, got, m)
	// An app that is not in the map contributes nothing: the manifest is the environment's settings and the apps given.
	got = Assemble(m.EnvScope(), map[string]Manifest{"api": m.AppScope("api")})
	if len(got.Apps) != 1 || len(got.Jobs) != 1 || len(got.Backups) != 1 || len(got.Probes) != 2 {
		t.Fatalf("assembled with one app: apps %d jobs %d backups %d probes %d", len(got.Apps), len(got.Jobs), len(got.Backups), len(got.Probes))
	}
}

func TestAssembleTakesEachAppsPartFromItsOwnManifest(t *testing.T) {
	m := decodeScoped(t, scopedManifest)
	older := decodeScoped(t, scopedManifest)
	job := older.Jobs["tick"]
	job.Schedule = "5 * * * *"
	older.Jobs["tick"] = job
	newer := decodeScoped(t, scopedManifest)
	alerts := *newer.Alerts
	tg := *alerts.Telegram
	tg.ChatID = "2"
	alerts.Telegram = &tg
	newer.Alerts = &alerts
	got := Assemble(newer.EnvScope(), map[string]Manifest{"api": older.AppScope("api"), "worker": m.AppScope("worker")})
	if got.Jobs["tick"].Schedule != "5 * * * *" || got.Alerts.Telegram.ChatID != "2" {
		t.Fatalf("assembled: job %q chat %q", got.Jobs["tick"].Schedule, got.Alerts.Telegram.ChatID)
	}
}

func TestAssembleDoesNotShareMapsWithItsInputs(t *testing.T) {
	m := decodeScoped(t, scopedManifest)
	env := m.EnvScope()
	got := Assemble(env, map[string]Manifest{"api": m.AppScope("api")})
	got.Probes["new"] = Probe{HTTP: "http://x/"}
	if _, ok := env.Probes["new"]; ok {
		t.Fatal("Assemble wrote into the environment's probes")
	}
}

func TestSettingsChangesNamesEachDifference(t *testing.T) {
	prev := decodeScoped(t, scopedManifest)
	if got := SettingsChanges(prev, prev, "test"); len(got) != 0 {
		t.Fatalf("no difference reported as %v", got)
	}
	// App-scoped members are not settings: a different job or app changes nothing.
	same := decodeScoped(t, scopedManifest)
	job := same.Jobs["tick"]
	job.Schedule = "5 * * * *"
	same.Jobs["tick"] = job
	if got := SettingsChanges(prev, same, "test"); len(got) != 0 {
		t.Fatalf("an app's job reported as a settings change: %v", got)
	}
	next := decodeScoped(t, scopedManifest)
	next.Network = "other"
	next.Probes["extra"] = Probe{TCP: "10.0.0.5:5432"}
	delete(next.Registries, "ghcr.io")
	d := next.Destinations["offsite"]
	d.Bucket = "other-bucket"
	next.Destinations["offsite"] = d
	next.Alerts.Telegram.ChatID = "2"
	next.Peers["staging"] = "https://staging.example.com:443"
	next.Telemetry = &Telemetry{OTLPEndpoint: "http://127.0.0.1:4318"}
	next.Servers = map[string]Server{"pg": {SSH: "ops@10.0.0.5"}}
	got := SettingsChanges(prev, next, "test")
	want := []string{"network: demo -> other", "registries: - ghcr.io", "destinations: ~ offsite", "telemetry: added", "probes: + extra", "peers: + staging", "alerts: changed", "servers: + pg"}
	slices.Sort(got)
	slices.Sort(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("changes\n got %v\nwant %v", got, want)
	}
	// Going back removes what was added.
	back := SettingsChanges(next, prev, "test")
	if !slices.Contains(back, "telemetry: removed") || !slices.Contains(back, "probes: - extra") {
		t.Fatalf("reverse changes: %v", back)
	}
}

func TestSettingsChangesNamesDeclaredSecrets(t *testing.T) {
	prev := decodeScoped(t, scopedManifest)
	next := decodeScoped(t, scopedManifest)
	n := 32
	next.Secrets = map[string]DeclaredSecret{
		"GEN": {Generate: &GenerateRule{Format: "hex", Bytes: &n}},
	}
	got := SettingsChanges(prev, next, "test")
	if !slices.Contains(got, "secrets: + GEN") {
		t.Fatalf("added secret not named: %v", got)
	}
	back := SettingsChanges(next, prev, "test")
	if !slices.Contains(back, "secrets: - GEN") {
		t.Fatalf("removed secret not named: %v", back)
	}
	changed := decodeScoped(t, scopedManifest)
	changed.Secrets = map[string]DeclaredSecret{
		"GEN": {Generate: &GenerateRule{Format: "base64", Bytes: &n}},
	}
	mid := SettingsChanges(next, changed, "test")
	if !slices.Contains(mid, "secrets: ~ GEN") {
		t.Fatalf("changed secret not named: %v", mid)
	}
}

func TestSettingsChangesNeverShowsSecretValues(t *testing.T) {
	prev := decodeScoped(t, scopedManifest)
	next := decodeScoped(t, scopedManifest)
	next.Alerts.Telegram.BotTokenSecret = "OTHER_SECRET"
	for _, line := range SettingsChanges(prev, next, "test") {
		if line != "alerts: changed" {
			t.Fatalf("line %q", line)
		}
	}
}

func TestDecodeSettingsReadsAStoredEnvironmentScope(t *testing.T) {
	m := decodeScoped(t, scopedManifest)
	raw, err := json.Marshal(m.EnvScope())
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeSettings(raw)
	if err != nil {
		t.Fatal(err)
	}
	sameManifest(t, got, m.EnvScope())
	if _, err = DecodeSettings([]byte(`{"project":"demo","bogus":1}`)); err == nil {
		t.Fatal("unknown member accepted")
	}
}
