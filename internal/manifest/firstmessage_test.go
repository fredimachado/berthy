package manifest

import (
	"encoding/json"
	"strings"
	"testing"
)

// firstMessageBase is a valid manifest; every case below breaks it in one place.
const firstMessageBase = `{
 "project":"demo","network":"demo",
 "apps":{
  "api":{"kind":"image","image":"ghcr.io/acme/api","roles":{"web":{"port":8080}}},
  "db":{"kind":"compose","file":"compose.yml"}},
 "jobs":{"seed":{"app":"api","command":["true"]}},
 "destinations":{"s3":{"type":"s3","bucket":"my-bucket","endpoint":"https://s3.example.com","access_key_secret":"AK","secret_key_secret":"SK"}},
 "backups":{"data":{"app":"api","volume":"data","destination":"s3"}}
}`

const validIngress = `json:{"cloudflare":{"account_id":"acct","api_token_secret":"CF_TOKEN","image":"cloudflare/cloudflared:2024.1.0"}}`

// brokenManifest returns the base manifest with the given edits applied. An edit is a dotted path and a value; a nil value
// removes the key. A JSON string value of the form "json:..." is decoded first, so a case can write an object inline.
func brokenManifest(t *testing.T, edits ...any) []byte {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(firstMessageBase), &m); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < len(edits); i += 2 {
		path, _ := edits[i].(string)
		value := edits[i+1]
		if s, ok := value.(string); ok && strings.HasPrefix(s, "json:") {
			if err := json.Unmarshal([]byte(strings.TrimPrefix(s, "json:")), &value); err != nil {
				t.Fatalf("%s: %v", path, err)
			}
		}
		segs := strings.Split(path, ".")
		node := m
		for _, seg := range segs[:len(segs)-1] {
			next, ok := node[seg].(map[string]any)
			if !ok {
				next = map[string]any{}
				node[seg] = next
			}
			node = next
		}
		if value == nil {
			delete(node, segs[len(segs)-1])
		} else {
			node[segs[len(segs)-1]] = value
		}
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

type brokenCase struct {
	name  string
	want  string
	edits []any
}

func broken(name, want string, edits ...any) brokenCase { return brokenCase{name, want, edits} }

// firstMessageCases pins the message Validate (and so the agent) gives for each kind of mistake. A manifest with several
// mistakes still gives the first of them.
func firstMessageCases() []brokenCase {
	return []brokenCase{
		broken("ingress without account", "invalid Cloudflare ingress", "ingress", `json:{"cloudflare":{"account_id":"","api_token_secret":"CF","image":"cloudflare/cloudflared:1"}}`),
		broken("ingress without cloudflare", "invalid Cloudflare ingress", "ingress", "json:{}"),
		broken("ingress token reserved", "TUNNEL_TOKEN is reserved for managed ingress", "ingress", `json:{"cloudflare":{"account_id":"a","api_token_secret":"TUNNEL_TOKEN","image":"cloudflare/cloudflared:1"}}`),
		broken("ingress bad image", "invalid Cloudflare image", "ingress", `json:{"cloudflare":{"account_id":"a","api_token_secret":"CF","image":"###"}}`),
		broken("ingress image without tag", "invalid Cloudflare image: a tag or digest is required", "ingress", `json:{"cloudflare":{"account_id":"a","api_token_secret":"CF","image":"cloudflare/cloudflared"}}`),
		broken("bad project", "invalid project or network", "project", "Bad Name"),
		broken("bad network", "invalid project or network", "network", ""),
		broken("no apps", "at least one app is required", "apps", "json:{}", "jobs", nil, "backups", nil),
		broken("registry without username", `invalid registry "ghcr.io"`, "registries", `json:{"ghcr.io":{"username":"","password_secret":"P"}}`),
		broken("registry token reserved", "registry ghcr.io: TUNNEL_TOKEN is reserved for managed ingress", "registries", `json:{"ghcr.io":{"username":"u","password_secret":"TUNNEL_TOKEN"}}`),
		broken("bad app name", `invalid app "Bad"`, "apps", `json:{"Bad":{"kind":"compose","file":"c.yml"}}`, "jobs", nil, "backups", nil),
		broken("app named ingress", "app name ingress is reserved for the managed ingress connector", "ingress", validIngress, "apps.ingress", `json:{"kind":"compose","file":"c.yml"}`),
		broken("bad project name", "app api: invalid project name", "apps.api.project_name", "Bad Name"),
		broken("bad kind", "app api: invalid kind", "apps.api.kind", "vm"),
		broken("image with a tag", "app api: image must be a repository and roles required", "apps.api.image", "ghcr.io/acme/api:1"),
		broken("image without roles", "app api: image must be a repository and roles required", "apps.api.roles", nil),
		broken("absolute compose file", "app db: invalid compose file", "apps.db.file", "/etc/compose.yml"),
		broken("compose file climbs", "app db: invalid compose file", "apps.db.file", "../compose.yml"),
		broken("depends on itself", "apps.api.depends_on: an app cannot depend on itself", "apps.api.depends_on", []any{"api"}),
		broken("depends twice", `apps.api.depends_on: duplicate app "db"`, "apps.api.depends_on", []any{"db", "db"}),
		broken("depends on unknown", `apps.api.depends_on: unknown app "nope"`, "apps.api.depends_on", []any{"nope"}),
		broken("env tunnel token", "app api: TUNNEL_TOKEN is reserved for managed ingress", "apps.api.env.plain", `json:{"TUNNEL_TOKEN":"x"}`),
		broken("env berthy prefix", "app api: names starting with BERTHY_ are reserved for Berthy", "apps.api.env.plain", `json:{"BERTHY_X":"x"}`),
		broken("env bad name", "app api: invalid environment name or value", "apps.api.env.plain", `json:{"1x":"x"}`),
		broken("secret tunnel token", "app api: TUNNEL_TOKEN is reserved for managed ingress", "apps.api.env.secret", []any{"TUNNEL_TOKEN"}),
		broken("secret berthy prefix", "app api: names starting with BERTHY_ are reserved for Berthy", "apps.api.env.secret", []any{"BERTHY_X"}),
		broken("secret bad name", "app api: invalid secret", "apps.api.env.secret", []any{"1x"}),
		broken("secret listed twice", "app api: variable A is listed twice", "apps.api.env.secret", []any{"A", "A"}),
		broken("secret also plain", "app api: duplicate environment name", "apps.api.env.plain", `json:{"A":"x"}`, "apps.api.env.secret", []any{"A"}),
		broken("secret entry shape", "env.secret entry must be a name or VARIABLE: SECRET", "apps.api.env.secret", []any{float64(5)}),
		broken("bad deploy timeout", "app api: invalid deploy settings", "apps.api.deploy.deploy_timeout", "soon"),
		broken("bad drain timeout", "app api: invalid deploy settings", "apps.api.deploy.drain_timeout", "0s"),
		broken("bad retain", "app api: invalid deploy settings", "apps.api.deploy.retain", float64(0)),
		broken("bad start attempts", "app api: invalid start_attempts", "apps.api.deploy.start_attempts", float64(0)),
		broken("hostname without ingress", "app api role web: invalid or duplicate hostname", "apps.api.roles.web.hostnames", []any{"a.example.com"}),
		broken("hostname twice", "app api role web: invalid or duplicate hostname", "ingress", validIngress, "apps.api.roles.web.hostnames", []any{"a.example.com", "a.example.com"}),
		broken("volume", "app api role web: invalid volume", "apps.api.roles.web.volumes", []any{"nope"}),
		broken("cpu", "app api role web: invalid CPU", "apps.api.roles.web.resources.cpu", "lots"),
		broken("memory", "app api role web: invalid memory", "apps.api.roles.web.resources.memory", "lots"),
		broken("logging on compose", "app db role web: logging is for image apps", "apps.db.roles.web.logging", `json:{"driver":"local"}`),
		broken("logging driver", "app api role web: invalid logging", "apps.api.roles.web.logging", `json:{"driver":"Bad Driver"}`),
		broken("logging option", "app api role web: invalid logging", "apps.api.roles.web.logging", `json:{"driver":"local","options":{"Bad Key":"x"}}`),
		broken("role name", "app api role Web: invalid settings", "apps.api.roles", `json:{"Web":{}}`),
		broken("role service", "app api role web: invalid settings", "apps.api.roles.web.service", "Bad Name"),
		broken("role replicas", "app api role web: invalid settings", "apps.api.roles.web.replicas", float64(0)),
		broken("role port", "app api role web: invalid settings", "apps.api.roles.web.port", float64(70000)),
		broken("role command", "app api role web: invalid settings", "apps.api.roles.web.command", []any{""}),
		broken("health test", "app api role web: invalid health", "apps.api.roles.web.health", `json:{"test":["curl"]}`),
		broken("health interval", "app api role web: invalid health", "apps.api.roles.web.health", `json:{"test":["CMD","x"],"interval":"soon"}`),
		broken("health retries", "app api role web: invalid health", "apps.api.roles.web.health", `json:{"test":["CMD","x"],"retries":0}`),
		broken("hook with neither", "app api: invalid hook", "apps.api.hooks.pre_deploy", `json:[{}]`),
		broken("hook with both", "app api: invalid hook", "apps.api.hooks.post_deploy", `json:[{"backup":"data","run":["x"]}]`),
		broken("backup hook timeout", "app api: pre_deploy hook 1: timeout and retries apply to run hooks only", "apps.api.hooks.pre_deploy", `json:[{"backup":"data","timeout":"5s"}]`),
		broken("hook timeout", "app api: post_deploy hook 1: invalid timeout", "apps.api.hooks.post_deploy", `json:[{"run":["x"],"timeout":"soon"}]`),
		broken("hook retries", "app api: post_deploy hook 1: invalid retries", "apps.api.hooks.post_deploy", `json:[{"run":["x"],"retries":9}]`),
		broken("hook backup unknown", "app api: pre_deploy hook 2: unknown backup", "apps.api.hooks.pre_deploy", `json:[{"run":["x"]},{"backup":"nope"}]`),
		broken("migrations argv", "app api: invalid migrations", "apps.api.migrations", `json:{"version":[],"apply":["x"]}`),
		broken("migrations timeout", "app api: invalid migrations timeout", "apps.api.migrations", `json:{"version":["v"],"apply":["x"],"timeout":"soon"}`),
		broken("migrations retries", "app api: invalid migrations retries", "apps.api.migrations", `json:{"version":["v"],"apply":["x"],"retries":9}`),
		broken("migrations target embedded", "app api: target must be a standalone argument", "apps.api.migrations", `json:{"version":["v"],"apply":["x"],"revert":["r","t={target}"]}`),
		broken("migrations target missing", "app api: revert requires one {target}", "apps.api.migrations", `json:{"version":["v"],"apply":["x"],"revert":["r"]}`),
		broken("depends cycle", "depends_on cycle: api -> db -> api", "apps.api.depends_on", []any{"db"}, "apps.db.depends_on", []any{"api"}),
		broken("job shape", `invalid job "seed"`, "jobs.seed.command", []any{}),
		broken("job role and service", `invalid job "seed"`, "jobs.seed.role", "web", "jobs.seed.service", "web"),
		broken("job on the connector", "job seed: app ingress is reserved for the managed ingress connector", "ingress", validIngress, "jobs.seed.app", "ingress"),
		broken("job unknown app", "job seed: unknown app", "jobs.seed.app", "nope"),
		broken("job schedule", "job seed: invalid schedule: ", "jobs.seed.schedule", "never"),
		broken("job timeout", "job seed: invalid timeout", "jobs.seed.timeout", "soon"),
		broken("job role on compose", "job seed: use service for a Compose app", "jobs.seed.app", "db", "jobs.seed.role", "web"),
		broken("job service on image", "job seed: use role for an image app", "jobs.seed.service", "web"),
		broken("job unknown role", "job seed: role is not a role of the app", "jobs.seed.role", "nope"),
		broken("job needs a role", "job seed: role required for an app with several roles", "apps.api.roles.worker", "json:{}"),
		broken("destination token", "destination s3: TUNNEL_TOKEN is reserved for managed ingress", "destinations.s3.access_key_secret", "TUNNEL_TOKEN"),
		broken("destination type", `invalid destination "s3"`, "destinations.s3.type", "gcs"),
		broken("destination bucket", "destination s3: invalid bucket name", "destinations.s3.bucket", "X"),
		broken("destination endpoint", "destination s3: invalid endpoint: ", "destinations.s3.endpoint", "ftp://x"),
		broken("destination region", "destination s3: invalid region", "destinations.s3.region", "Bad Region"),
		broken("backup shape", `invalid backup "data"`, "backups.data.volume", ""),
		broken("backup unknown app", "backup data: unknown app", "backups.data.app", "nope"),
		broken("backup unknown destination", "backup data: unknown destination", "backups.data.destination", "nope"),
		broken("backup schedule", "backup data: invalid schedule: ", "backups.data.schedule", "never"),
		broken("backup unknown job", "backup data: unknown job", "backups.data.before", "nope"),
		broken("backup retention word", "backup data: invalid retention", "backups.data.retention", `json:{"keep":"some"}`),
		broken("backup retention number", "backup data: invalid retention", "backups.data.retention", `json:{"keep":0}`),
		broken("backup retention shape", "backup data: invalid retention", "backups.data.retention", `json:{"keep":true}`),
		broken("state backup destination", "state_backup: unknown destination", "state_backup", `json:{"destination":"nope"}`),
		broken("state backup schedule", "state_backup: invalid schedule: ", "state_backup", `json:{"destination":"s3","schedule":"never"}`),
		broken("state backup keep", "state_backup: keep must be from 1 to 1000", "state_backup", `json:{"destination":"s3","keep":0}`),
		broken("state backup prefix", "state_backup: invalid prefix", "state_backup", `json:{"destination":"s3","prefix":"a/../b"}`),
		broken("telemetry token", "telemetry: TUNNEL_TOKEN is reserved for managed ingress", "telemetry", `json:{"otlp_endpoint":"https://o.example.com","headers_secret":"TUNNEL_TOKEN"}`),
		broken("telemetry endpoint", "invalid telemetry", "telemetry", `json:{"otlp_endpoint":""}`),
		broken("min version", `invalid min_version "soon": use a version such as 0.9.0`, "min_version", "soon"),
		broken("alias name", "alias Bad: invalid name", "aliases", `json:{"Bad":"status"}`),
		broken("alias empty", "alias up: a command is required", "aliases", `json:{"up":" "}`),
		broken("alias quotes", "alias up: quotes are not interpreted; separate the words with spaces", "aliases", `json:{"up":"deploy 'x'"}`),
		broken("probe name", `invalid probe "Bad"`, "probes", `json:{"Bad":{"tcp":"db:5432"}}`),
		broken("probe kinds", "probe p: exactly one of http, tcp or service is required", "probes", `json:{"p":{}}`),
		broken("probe http", "probe p: http must be an http or https URL with a host", "probes", `json:{"p":{"http":"x"}}`),
		broken("probe tcp", "probe p: tcp must be host:port with a port from 1 to 65535", "probes", `json:{"p":{"tcp":"x"}}`),
		broken("probe service app", "probe p: service app is not in the manifest", "probes", `json:{"p":{"service":{"app":"nope","port":80}}}`),
		broken("probe service port", "probe p: service port must be from 1 to 65535", "probes", `json:{"p":{"service":{"app":"api","port":0}}}`),
		broken("probe service path", "probe p: service path must start with /", "probes", `json:{"p":{"service":{"app":"api","port":80,"path":"x"}}}`),
		broken("probe status", "probe p: expect_status entries must be from 100 to 599", "probes", `json:{"p":{"tcp":"db:80","expect_status":[99]}}`),
		broken("probe interval", "probe p: interval must be a duration of at least 5s", "probes", `json:{"p":{"tcp":"db:80","interval":"1s"}}`),
		broken("probe failures", "probe p: failures must be at least 1", "probes", `json:{"p":{"tcp":"db:80","failures":0}}`),
		broken("peer name", `invalid peer "Bad"`, "peers", `json:{"Bad":"http://a:1"}`),
		broken("peer url", "peer a: URL must be http or https with a host and a port, and no path, query or user", "peers", `json:{"a":"nope"}`),
		broken("telegram token", "alerts.telegram: TUNNEL_TOKEN is reserved for managed ingress", "alerts", `json:{"telegram":{"bot_token_secret":"TUNNEL_TOKEN","chat_id":"1"}}`),
		broken("telegram chat", "alerts.telegram: chat_id must be a numeric chat ID or a @channel name", "alerts", `json:{"telegram":{"bot_token_secret":"BOT","chat_id":"x"}}`),
		broken("telegram deploys", "alerts.telegram: deploys must be failed, all or none", "alerts", `json:{"telegram":{"bot_token_secret":"BOT","chat_id":"1","deploys":"some"}}`),
		broken("unknown field", `unknown field "wat"`, "wat", true),
		broken("unknown nested field", `unknown field "wat"`, "apps.api.roles.web.wat", true),
		broken("an unknown field comes before an earlier wrong value", `unknown field "wat"`, "apps.api.roles.web.replicas", "two", "wat", true),
		broken("type error", "json: cannot unmarshal string into Go struct field Role.apps.roles.replicas of type int", "apps.api.roles.web.replicas", "two"),
		broken("type error in the root", "json: cannot unmarshal number into Go struct field Manifest.project of type string", "project", float64(5)),
		broken("object where a list belongs", "json: cannot unmarshal object into Go struct field App.apps.depends_on of type []string", "apps.api.depends_on", "json:{}"),
		broken("two mistakes give the first", "app api role web: invalid settings", "apps.api.roles.web.port", float64(70000), "backups.data.destination", "nope"),
	}
}

func TestFirstMessageIsStable(t *testing.T) {
	for _, c := range firstMessageCases() {
		t.Run(c.name, func(t *testing.T) {
			_, err := DecodeJSON(brokenManifest(t, c.edits...))
			if err == nil {
				t.Fatal("accepted a broken manifest")
			}
			if got := err.Error(); !strings.HasPrefix(got, c.want) {
				t.Fatalf("got %q, want %q", got, c.want)
			}
		})
	}
}
