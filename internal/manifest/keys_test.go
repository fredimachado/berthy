package manifest

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"
)

// manifestPaths walks the Manifest type by its json tags and returns the path of every key a manifest may hold, written as
// Keys writes them. A struct field is a key, and so is what it holds. A map of objects adds its placeholder to the path
// and its fields follow; a map of values has one key, the placeholder's. A list of objects adds "[]". A type that reads its
// own JSON shape, and any, hold nothing more.
func manifestPaths(t *testing.T) []string {
	t.Helper()
	var paths []string
	var walk func(typ reflect.Type, path string)
	walk = func(typ reflect.Type, path string) {
		for typ.Kind() == reflect.Pointer {
			typ = typ.Elem()
		}
		if reflect.PointerTo(typ).Implements(jsonUnmarshaler) {
			return
		}
		switch typ.Kind() {
		case reflect.Struct:
			for i := 0; i < typ.NumField(); i++ {
				name := strings.Split(typ.Field(i).Tag.Get("json"), ",")[0]
				if name == "" || name == "-" {
					continue
				}
				p := name
				if path != "" {
					p = path + "." + name
				}
				paths = append(paths, p)
				walk(typ.Field(i).Type, p)
			}
		case reflect.Map:
			ph, ok := placeholder(path)
			if !ok {
				t.Errorf("Keys has no key under %s.<...>, which names the keys of that map", path)
				return
			}
			elem := typ.Elem()
			for elem.Kind() == reflect.Pointer {
				elem = elem.Elem()
			}
			if elem.Kind() == reflect.Struct {
				walk(elem, path+"."+ph)
			} else {
				paths = append(paths, path+"."+ph)
			}
		case reflect.Slice:
			elem := typ.Elem()
			for elem.Kind() == reflect.Pointer {
				elem = elem.Elem()
			}
			if elem.Kind() == reflect.Struct && !reflect.PointerTo(elem).Implements(jsonUnmarshaler) {
				walk(elem, path+"[]")
			}
		}
	}
	walk(reflect.TypeOf(Manifest{}), "")
	return paths
}

func TestKeysMatchManifestType(t *testing.T) {
	want := manifestPaths(t)
	defined := map[string]bool{}
	for _, k := range Keys {
		if defined[k.Path] {
			t.Errorf("Keys defines %s twice", k.Path)
		}
		defined[k.Path] = true
	}
	for _, p := range want {
		if !defined[p] {
			t.Errorf("the Manifest type has the key %s, and Keys does not define it", p)
		}
	}
	for _, k := range Keys {
		if !slices.Contains(want, k.Path) {
			t.Errorf("Keys defines %s, and the Manifest type has no such key", k.Path)
		}
	}
}

func TestKeysAreWellFormed(t *testing.T) {
	sections := map[string]Section{}
	for _, s := range Sections {
		if s.Name == "" || s.Title == "" {
			t.Errorf("section %+v needs a name and a title", s)
		}
		if _, dup := sections[s.Name]; dup {
			t.Errorf("section %s is defined twice", s.Name)
		}
		sections[s.Name] = s
	}
	used := map[string]bool{}
	for _, k := range Keys {
		s, ok := sections[k.Section]
		if !ok {
			t.Errorf("%s: unknown section %q", k.Path, k.Section)
			continue
		}
		used[k.Section] = true
		if !strings.HasPrefix(k.Path, s.Prefix) || k.Path == s.Prefix {
			t.Errorf("%s: section %s lists only keys under %q", k.Path, s.Name, s.Prefix)
		}
		if k.Type == "" || k.Description == "" {
			t.Errorf("%s: a type and a description are required", k.Path)
		}
		if !slices.Contains([]string{"", "image", "compose"}, k.Applies) {
			t.Errorf("%s: Applies is %q", k.Path, k.Applies)
		}
		if k.Required && k.Default != "" {
			t.Errorf("%s: a required key has no default", k.Path)
		}
		if k.Default != "" && len(k.Enum) > 0 && !slices.Contains(k.Enum, k.Default) {
			t.Errorf("%s: default %q is not one of %v", k.Path, k.Default, k.Enum)
		}
		if k.NamePattern != "" && k.Type != "map" {
			t.Errorf("%s: only a map has a NamePattern", k.Path)
		}
		if strings.Contains(k.Description, "|") {
			t.Errorf("%s: the description holds a pipe, which breaks a table", k.Path)
		}
	}
	for name := range sections {
		if !used[name] {
			t.Errorf("section %s lists no key", name)
		}
	}
}

// fullManifest sets every key of the manifest, in the shape a person writes it. It is valid.
const fullManifest = `
project: demo
network: demo
protected: true
servers:
  pg:
    ssh: ops@deploy.example.com
    address: 10.0.0.5
min_version: 0.9.0
timezone: Europe/Lisbon
registries:
  ghcr.io:
    username: acme
    password_secret: GHCR_TOKEN
apps:
  web:
    kind: image
    image: ghcr.io/acme/web
    depends_on: [db]
    roles:
      main:
        service: demo-web-main
        command: ["serve"]
        entrypoint: ["/app/run"]
        user: "1000:1000"
        replicas: 2
        port: 8080
        health:
          test: ["CMD", "true"]
          interval: 10s
          timeout: 5s
          start_period: 1s
          start_interval: 1s
          retries: 3
        volumes: ["uploads:/data:ro"]
        resources:
          cpu: "0.5"
          memory: 512M
        hostnames: [web.example.com]
        logging:
          driver: json-file
          options:
            max-size: 10m
    env:
      plain:
        LOG_LEVEL: info
      secret:
        - API_KEY
        - DB_URL: DATABASE_URL
      template:
        DATABASE_DSN: "postgres://app:${DATABASE_URL}@db:5432/app"
    deploy:
      deploy_timeout: 120s
      drain_timeout: 30s
      start_attempts: 3
      verify: true
      retain: 5
      order: stop-first
    hooks:
      pre_deploy:
        - backup: main
        - run: ["true"]
          timeout: 1m
          retries: 1
      post_deploy:
        - backup: main
        - run: ["true"]
          timeout: 1m
          retries: 1
    migrations:
      version: ["migrate", "version"]
      apply: ["migrate", "up"]
      revert: ["migrate", "down-to", "{target}"]
      timeout: 5m
      retries: 1
  queue:
    kind: image
    image: ghcr.io/acme/worker
    roles:
      worker:
        health:
          uptime: 20s
  db:
    kind: compose
    file: deploy/db.yml
    project_name: demo-db
    stateful: true
secrets:
  API_KEY:
    generate:
      format: hex
      bytes: 32
jobs:
  cleanup:
    app: web
    role: main
    schedule: "30 2 * * *"
    timeout: 15m
    command: ["clean"]
  dump:
    app: db
    service: postgres
    command: ["dump"]
backups:
  main:
    app: db
    volume: data
    schedule: "0 1 * * *"
    destination: offsite
    prefix: db/data
    before: dump
    retention:
      keep: 14
  main-dump:
    app: web
    role: main
    command: ["pg_dump"]
    timeout: 30m
    destination: offsite
  db-dump:
    app: db
    service: postgres
    command: ["pg_dump"]
    destination: offsite
destinations:
  offsite:
    type: s3
    endpoint: https://s3.example.com
    region: eu-central-1
    bucket: acme-backups
    access_key_secret: S3_KEY
    secret_key_secret: S3_SECRET
state_backup:
  destination: offsite
  prefix: berthy-state/prod
  schedule: "17 3 * * *"
  keep: 14
telemetry:
  otlp_endpoint: http://10.0.0.6:4318
  headers_secret: OTLP_HEADERS
probes:
  site:
    http: https://web.example.com/health
    expect_status: [200]
    interval: 30s
    timeout: 5s
    failures: 3
  database:
    tcp: 10.0.0.5:5432
  inner:
    service:
      app: web
      role: main
      port: 8080
      path: /health
peers:
  dev: http://10.0.0.4:7710
alerts:
  reminder: 4h
  deploys: all
  telegram:
    bot_token_secret: TG_TOKEN
    chat_id: "-1001"
  webhook:
    url_secret: HOOK_URL
    headers_secret: HOOK_HEADERS
  heartbeat:
    url_secret: BEAT_URL
    interval: 1m
ingress:
  cloudflare:
    account_id: abc123
    api_token_secret: CF_TOKEN
    image: cloudflare/cloudflared:2026.11.0
    replicas: 1
    tunnel: demo-prod
    dns: true
aliases:
  weblogs: logs web
`

// bareManifest sets only what the manifest requires, and one of each thing that has defaults.
const bareManifest = `
project: demo
network: demo
apps:
  web:
    kind: image
    image: ghcr.io/acme/web
    roles:
      main:
        health:
          test: ["CMD", "true"]
    hooks:
      pre_deploy:
        - run: ["true"]
    migrations:
      version: ["v"]
      apply: ["a"]
jobs:
  j:
    app: web
    command: ["x"]
destinations:
  d:
    type: s3
    bucket: acme-backups
    access_key_secret: A
    secret_key_secret: B
state_backup:
  destination: d
probes:
  p:
    http: https://example.com
alerts:
  telegram:
    bot_token_secret: T
    chat_id: "1"
`

// yamlDoc reads text as the generic document the manifest decodes from.
func yamlDoc(t *testing.T, text string) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(text), &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

// decodeDoc runs a document through the validation the agent applies to a manifest it is sent.
func decodeDoc(doc map[string]any) (*Manifest, error) {
	data, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	return DecodeJSON(data)
}

// eachAt calls fn for every place in doc that a key path names, with the map that holds it and its name, and returns how
// many places there were. A placeholder matches every key of a map, and "[]" every item of a list.
func eachAt(node any, segs []string, fn func(holder map[string]any, name string)) int {
	holder, ok := node.(map[string]any)
	if !ok || len(segs) == 0 {
		return 0
	}
	seg, list := strings.CutSuffix(segs[0], "[]")
	var names []string
	if strings.HasPrefix(seg, "<") {
		for name := range holder {
			names = append(names, name)
		}
	} else if _, ok := holder[seg]; ok {
		names = []string{seg}
	}
	n := 0
	for _, name := range names {
		switch {
		case len(segs) == 1:
			fn(holder, name)
			n++
		case list:
			items, _ := holder[name].([]any)
			for _, item := range items {
				n += eachAt(item, segs[1:], fn)
			}
		default:
			n += eachAt(holder[name], segs[1:], fn)
		}
	}
	return n
}

func eachKeyAt(doc map[string]any, path string, fn func(holder map[string]any, name string)) int {
	return eachAt(doc, strings.Split(path, "."), fn)
}

func TestFullManifestIsValidAndSetsEveryKey(t *testing.T) {
	if _, err := decodeDoc(yamlDoc(t, fullManifest)); err != nil {
		t.Fatalf("the full manifest of the tests is not valid: %v", err)
	}
	doc := yamlDoc(t, fullManifest)
	for _, k := range Keys {
		if eachKeyAt(doc, k.Path, func(map[string]any, string) {}) == 0 {
			t.Errorf("the full manifest of the tests does not set %s", k.Path)
		}
	}
}

// exclusive are keys of which a manifest sets exactly one, or that others refer to, so a manifest without them is refused
// although they are optional. Each says why.
var exclusive = map[string]string{
	"apps.<name>.roles.<role>.health.test":   "health has one of test and uptime",
	"apps.<name>.roles.<role>.health.uptime": "health has one of test and uptime",
	"apps.<name>.hooks.pre_deploy[].backup":  "a hook is a backup or a run",
	"apps.<name>.hooks.pre_deploy[].run":     "a hook is a backup or a run",
	"apps.<name>.hooks.post_deploy[].backup": "a hook is a backup or a run",
	"apps.<name>.hooks.post_deploy[].run":    "a hook is a backup or a run",
	"probes.<name>.http":                     "a probe has one of http, tcp and service",
	"probes.<name>.tcp":                      "a probe has one of http, tcp and service",
	"probes.<name>.service":                  "a probe has one of http, tcp and service",
	"destinations":                           "backups and state_backup name a destination",
	"backups":                                "hooks name a backup",
	"backups.<name>.volume":                  "a backup has one of volume and command",
	"backups.<name>.command":                 "a backup has one of volume and command",
}

// companions are keys that must go with another to leave a valid manifest: what refers to the key being removed.
var companions = map[string][]string{
	"jobs":                          {"backups.<name>.before"},
	"ingress":                       {"apps.<name>.roles.<role>.hostnames"},
	"apps.<name>.roles.<role>.port": {"apps.<name>.roles.<role>.hostnames"},
	"apps.<name>.env":               {"secrets"},
	"apps.<name>.env.secret":        {"secrets"},
}

// TestRequiredKeysAgreeWithValidate removes each key from the full manifest. A manifest without a required key is refused,
// and one without an optional key is accepted.
func TestRequiredKeysAgreeWithValidate(t *testing.T) {
	for _, k := range Keys {
		if _, skip := exclusive[k.Path]; skip && !k.Required {
			continue
		}
		doc := yamlDoc(t, fullManifest)
		remove := func(path string) {
			eachKeyAt(doc, path, func(holder map[string]any, name string) { delete(holder, name) })
		}
		remove(k.Path)
		for _, with := range companions[k.Path] {
			remove(with)
		}
		_, err := decodeDoc(doc)
		switch {
		case k.Required && err == nil:
			t.Errorf("%s is documented as required, and a manifest without it is accepted", k.Path)
		case !k.Required && err != nil:
			t.Errorf("%s is documented as optional, and a manifest without it is refused: %v", k.Path, err)
		}
	}
}

// enumProbes make a manifest with each key that has a fixed set of values, set to v.
var enumProbes = map[string]func(v string) string{
	"apps.<name>.kind": func(v string) string {
		return fmt.Sprintf(`project: demo
network: demo
apps:
  web:
    kind: %s
    image: ghcr.io/acme/web
    file: deploy/web.yml
    roles:
      main: {}
`, v)
	},
	"destinations.<name>.type": func(v string) string {
		return strings.Replace(bareManifest, "type: s3", "type: "+v, 1)
	},
	"alerts.deploys": func(v string) string {
		return strings.Replace(bareManifest, "alerts:\n", "alerts:\n  deploys: "+v+"\n", 1)
	},
	"apps.<name>.deploy.order": func(v string) string {
		return strings.Replace(bareManifest, "    hooks:\n", "    deploy:\n      order: "+v+"\n    hooks:\n", 1)
	},
	"secrets.<SECRET>.generate.format": func(v string) string {
		return fmt.Sprintf(`project: demo
network: demo
apps:
  app:
    kind: image
    image: alpine
    roles:
      main: {}
    env:
      secret: [GEN]
secrets:
  GEN:
    generate:
      format: %s
`, v)
	},
}

func TestEnumValuesAreTheOnesValidateAccepts(t *testing.T) {
	enumKeys := map[string]bool{}
	for _, k := range Keys {
		if len(k.Enum) == 0 {
			continue
		}
		enumKeys[k.Path] = true
		probe, ok := enumProbes[k.Path]
		if !ok {
			t.Errorf("%s has an Enum, and enumProbes has no probe for it", k.Path)
			continue
		}
		for _, v := range k.Enum {
			if _, err := decodeDoc(yamlDoc(t, probe(v))); err != nil {
				t.Errorf("%s: %q is documented as allowed, and a manifest with it is refused: %v", k.Path, v, err)
			}
		}
		if _, err := decodeDoc(yamlDoc(t, probe("sometimes"))); err == nil {
			t.Errorf("%s: a value outside the Enum is accepted", k.Path)
		}
	}
	for path := range enumProbes {
		if !enumKeys[path] {
			t.Errorf("the probe of %s has no Enum in Keys", path)
		}
	}
}

// sameValue reports whether the default Keys documents and the value a decoded manifest holds are one value: the same text,
// or the same duration, as the manifest stores 4h as 4h0m0s.
func sameValue(documented, stored string) bool {
	if documented == stored {
		return true
	}
	a, errA := time.ParseDuration(documented)
	b, errB := time.ParseDuration(stored)
	return errA == nil && errB == nil && a == b
}

// TestDefaultsAgreeWithTheManifest decodes a manifest that leaves out the optional keys. Every key the decoding fills in
// has the value Keys documents as its default, and none is filled in without a documented default.
func TestDefaultsAgreeWithTheManifest(t *testing.T) {
	input := yamlDoc(t, bareManifest)
	m, err := decodeDoc(input)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(m)
	var stored map[string]any
	if err := json.Unmarshal(data, &stored); err != nil {
		t.Fatal(err)
	}
	for _, k := range Keys {
		if eachKeyAt(input, k.Path, func(map[string]any, string) {}) > 0 {
			continue // the manifest sets it, so it holds what was set
		}
		var found []string
		eachKeyAt(stored, k.Path, func(holder map[string]any, name string) { found = append(found, fmt.Sprint(holder[name])) })
		// bareManifest has a command check. Its uptime counterpart calculates its timing when needed.
		commandDefault := strings.TrimSuffix(k.Default, " for test")
		for _, f := range found {
			switch {
			case k.Default != "" && !sameValue(commandDefault, f):
				t.Errorf("%s: Keys says the default is %q, and the decoded manifest holds %q", k.Path, k.Default, f)
			case k.Default == "" && k.Type != "map" && k.Type != "list":
				t.Errorf("%s: the decoded manifest fills in %q, and Keys documents no default", k.Path, f)
			}
		}
	}
}
