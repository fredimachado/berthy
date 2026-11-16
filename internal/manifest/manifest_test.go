package manifest

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadOverlayAndDefaults(t *testing.T) {
	dir := t.TempDir()
	base := `project: demo
network: demo
apps:
  api:
    kind: image
    image: ghcr.io/acme/api
    roles:
      web:
        port: 8080
        health:
          test: [CMD, curl]
`
	if err := os.WriteFile(filepath.Join(dir, "berthy.yml"), []byte(base), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "berthy.prod.yml"), []byte("protected: true\napps:\n  api:\n    roles:\n      web:\n        replicas: 2\n"), 0600); err != nil {
		t.Fatal(err)
	}
	m, err := Load(dir, "prod")
	if err != nil {
		t.Fatal(err)
	}
	if !m.Protected || *m.Apps["api"].Roles["web"].Replicas != 2 || *m.Apps["api"].Deploy.Retain != 5 {
		t.Fatalf("unexpected result: %+v", m)
	}
}

func TestRejectInvalidInput(t *testing.T) {
	base := `project: demo
network: demo
apps:
  api:
    kind: image
    image: ghcr.io/acme/api
    roles:
      web:
        health:
          test: [CMD, curl]
`
	cases := []string{
		base + "unknown: true\n", base + "project: duplicate\n", base + "---\nproject: another\n", base + "telemetry: null\n", base + "jobs: &jobs {}\n", strings.Replace(base, "web:\n", "web:\n        replicas: 0\n", 1),
	}
	for _, input := range cases {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "berthy.yml"), []byte(input), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(dir, "dev"); err == nil {
			t.Errorf("accepted invalid input: %s", input)
		}
	}
}

func TestImageSelection(t *testing.T) {
	data := []byte(`{"project":"demo","network":"demo","apps":{"api":{"kind":"image","image":"ghcr.io/acme/api","roles":{"web":{}}}}}`)
	m, err := DecodeJSON(data)
	if err != nil {
		t.Fatal(err)
	}
	hash := strings.Repeat("a", 64)
	if got, err := m.CheckImageDeploy("api", "sha256:"+hash, true); err != nil || got != "ghcr.io/acme/api@sha256:"+hash {
		t.Fatalf("got %q, %v", got, err)
	}
	for _, v := range []string{"latest", "ghcr.io/other/api@sha256:" + hash} {
		if _, err := m.CheckImageDeploy("api", v, true); err == nil {
			t.Errorf("accepted %s", v)
		}
	}
	a := m.Apps["api"]
	a.Hooks.PostDeploy = []Hook{{Backup: "db"}}
	m.Apps["api"] = a
	if _, err := m.CheckImageDeploy("api", "v1", false); err == nil {
		t.Fatal("accepted unsupported backup hook")
	}
}

func TestFutureConfigValidation(t *testing.T) {
	data := []byte(`{"project":"demo","network":"demo","apps":{"api":{"kind":"image","image":"ghcr.io/acme/api","roles":{"web":{}}}},"jobs":{"bad/name":{"app":"api","command":["true"]}}}`)
	if _, err := DecodeJSON(data); err == nil {
		t.Fatal("accepted invalid job name")
	}
}

func TestComposeAndFutureConfiguration(t *testing.T) {
	input := `project: myapp
network: myapp
apps:
  api:
    kind: image
    image: ghcr.io/acme/api
    roles:
      web:
        health:
          test: [CMD, curl, -f, http://localhost:8080/health]
    hooks:
      pre_deploy:
        - backup: main-db
  postgres:
    kind: compose
    file: deploy/postgres/docker-compose.yml
    project_name: existing-postgres
    stateful: true
    env:
      secret: [POSTGRES_PASSWORD]
jobs:
  dump-db:
    app: postgres
    service: postgres
    command: [sh, -ec, 'echo dump']
destinations:
  offsite:
    type: s3
    endpoint: https://s3.example.com
    bucket: backups
    access_key_secret: S3_ACCESS_KEY
    secret_key_secret: S3_SECRET_KEY
backups:
  main-db:
    app: postgres
    volume: db-dumps
    destination: offsite
    before: dump-db
    retention:
      keep: all
telemetry:
  otlp_endpoint: http://collector:4318
  headers_secret: OTLP_HEADERS
`
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "berthy.yml"), []byte(input), 0600); err != nil {
		t.Fatal(err)
	}
	m, err := Load(dir, "dev")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"api", "postgres"} {
		if _, err := m.CheckImageDeploy(name, "v1", false); err == nil {
			t.Errorf("accepted unsupported deployment %s", name)
		}
	}
}

func TestStrictJSONBoundary(t *testing.T) {
	for _, input := range []string{`{"project":"a","project":"b"}`, `{"project":null}`, `{"project":"a"} {"project":"b"}`} {
		if _, err := DecodeJSON([]byte(input)); err == nil {
			t.Errorf("accepted %s", input)
		}
	}
}

func TestSecurityBoundaries(t *testing.T) {
	base := `{"project":"demo","network":"demo","protected":true,"apps":{"api":{"kind":"image","image":"ghcr.io/acme/api","roles":{"web":{%s}},"migrations":{"version":["v"],"apply":["a"],"revert":[%s]}}}}`
	for _, input := range []string{
		strings.Replace(fmt.Sprintf(base, "", `"{target}"`), `"protected":true`, `"protected":true,"Protected":false`, 1),
		fmt.Sprintf(base, `"volumes":["/:/host"]`, `"{target}"`),
		fmt.Sprintf(base, `"resources":{"cpu":"invalid"}`, `"{target}"`),
		fmt.Sprintf(base, "", `"--target={target}"`),
	} {
		if _, err := DecodeJSON([]byte(input)); err == nil {
			t.Errorf("accepted %s", input)
		}
	}
	m, err := DecodeJSON([]byte(fmt.Sprintf(base, "", `"{target}"`)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.CheckImageDeploy("api", "ghcr.io/acme/api:latest@sha256:"+strings.Repeat("a", 64), true); err == nil {
		t.Fatal("accepted tagged digest")
	}
}

func TestJSONRejectsYAMLTrailingSyntax(t *testing.T) {
	for _, data := range []string{`{"project":"demo","network":"demo","apps":{"api":{"kind":"image","image":"x","roles":{"web":{}}}}} # comment`, "{\"project\":\"demo\",\"network\":\"demo\",\"apps\":{\"api\":{\"kind\":\"image\",\"image\":\"x\",\"roles\":{\"web\":{}}}}}\n...\n"} {
		if _, err := DecodeJSON([]byte(data)); err == nil {
			t.Fatalf("accepted trailing YAML syntax %q", data)
		}
	}
}

func TestManagedIngressHostnamesPassImageCheck(t *testing.T) {
	data := []byte(`{"project":"demo","network":"demo","ingress":{"cloudflare":{"account_id":"account","api_token_secret":"CF_TOKEN","image":"cloudflare/cloudflared:v1","dns":false}},"apps":{"api":{"kind":"image","image":"ghcr.io/acme/api","roles":{"web":{"port":8080,"hostnames":["api.example.com"]}}},"worker":{"kind":"image","image":"ghcr.io/acme/worker","roles":{"main":{}}}}}`)
	m, err := DecodeJSON(data)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := m.CheckImageDeploy("api", "v1", false); err != nil || got != "ghcr.io/acme/api:v1" {
		t.Fatalf("hostname app refused: %v", err)
	}
	if _, err = m.CheckImageDeploy("worker", "v1", false); err != nil {
		t.Fatal(err)
	}
}

// destinationManifest returns a valid JSON manifest whose only destination is the given JSON object.
func destinationManifest(dest string) string {
	return `{"project":"demo","network":"demo","apps":{"api":{"kind":"image","image":"ghcr.io/acme/api","roles":{"web":{}}}},"destinations":{"offsite":` + dest + `}}`
}

func s3Destination(endpoint, region, bucket string) string {
	d := `{"type":"s3","bucket":"` + bucket + `","access_key_secret":"S3_ACCESS_KEY","secret_key_secret":"S3_SECRET_KEY"`
	if endpoint != "" {
		d += `,"endpoint":"` + endpoint + `"`
	}
	if region != "" {
		d += `,"region":"` + region + `"`
	}
	return d + `}`
}

func TestDestinationEndpointRules(t *testing.T) {
	valid := []string{
		"https://s3.example.com",
		"https://s3.example.com:9443",
		"http://127.0.0.1:19900",
		"http://localhost:9000",
		"http://[::1]:9000",
	}
	for _, endpoint := range valid {
		if _, err := DecodeJSON([]byte(destinationManifest(s3Destination(endpoint, "", "backups")))); err != nil {
			t.Errorf("endpoint %q refused: %v", endpoint, err)
		}
	}
	invalid := []string{
		"http://10.0.0.5:9000",
		"http://s3.example.com",
		"https://s3.example.com/bucket",
		"https://s3.example.com/",
		"https://s3.example.com?region=x",
		"https://user:secret@s3.example.com",
		"https://s3.example.com#fragment",
		"https://s3.example.com:0",
		"https://s3.example.com:70000",
		"ftp://s3.example.com",
		"s3.example.com",
	}
	for _, endpoint := range invalid {
		_, err := DecodeJSON([]byte(destinationManifest(s3Destination(endpoint, "", "backups"))))
		if err == nil || !strings.Contains(err.Error(), "destination offsite: invalid endpoint") {
			t.Errorf("endpoint %q: %v", endpoint, err)
		}
	}
}

func TestDestinationRegionRules(t *testing.T) {
	for _, region := range []string{"", "us-east-1", "eu-west-1", strings.Repeat("a", 32)} {
		if _, err := DecodeJSON([]byte(destinationManifest(s3Destination("", region, "backups")))); err != nil {
			t.Errorf("region %q refused: %v", region, err)
		}
	}
	for _, region := range []string{"EU-West-1", "eu_west", "eu west", strings.Repeat("a", 33)} {
		_, err := DecodeJSON([]byte(destinationManifest(s3Destination("", region, "backups"))))
		if err == nil || !strings.Contains(err.Error(), "destination offsite: invalid region") {
			t.Errorf("region %q: %v", region, err)
		}
	}
}

func TestDestinationBucketNameRules(t *testing.T) {
	for _, bucket := range []string{"abc", "berthy-test", "backups.prod.1", strings.Repeat("a", 63)} {
		if _, err := DecodeJSON([]byte(destinationManifest(s3Destination("", "", bucket)))); err != nil {
			t.Errorf("bucket %q refused: %v", bucket, err)
		}
	}
	for _, bucket := range []string{"", "ab", "Backups", "-backups", "backups-", "back_ups", "back ups", strings.Repeat("a", 64)} {
		_, err := DecodeJSON([]byte(destinationManifest(s3Destination("", "", bucket))))
		if err == nil || !strings.Contains(err.Error(), "destination offsite") {
			t.Errorf("bucket %q: %v", bucket, err)
		}
	}
}

func TestDestinationRegionIsReadFromYAML(t *testing.T) {
	dir := t.TempDir()
	base := `project: demo
network: demo
apps:
  api:
    kind: image
    image: ghcr.io/acme/api
    roles:
      web: {}
destinations:
  offsite:
    type: s3
    endpoint: https://s3.example.com
    region: eu-west-1
    bucket: backups
    access_key_secret: S3_ACCESS_KEY
    secret_key_secret: S3_SECRET_KEY
`
	if err := os.WriteFile(filepath.Join(dir, "berthy.yml"), []byte(base), 0600); err != nil {
		t.Fatal(err)
	}
	m, err := Load(dir, "prod")
	if err != nil {
		t.Fatal(err)
	}
	if got := m.Destinations["offsite"].Region; got != "eu-west-1" {
		t.Fatalf("region: %q", got)
	}
}
