package manifest

import (
	"errors"
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
}

func TestBackupHooksNameABackupOfTheManifest(t *testing.T) {
	const base = `"project":"demo","network":"demo","apps":{"api":{"kind":"image","image":"ghcr.io/acme/api","roles":{"web":{}},"hooks":{"%s":[{"backup":"main-db"},{"run":["true"]}]}},"db":{"kind":"compose","file":"db/compose.yaml"}}`
	backups := `,"destinations":{"offsite":{"type":"s3","bucket":"bkt","access_key_secret":"A","secret_key_secret":"B"}},"backups":{"main-db":{"app":"db","volume":"dumps","destination":"offsite"}}`
	for _, list := range []string{"pre_deploy", "post_deploy"} {
		m, err := DecodeJSON([]byte("{" + fmt.Sprintf(base, list) + backups + "}"))
		if err != nil {
			t.Fatalf("%s hook of a declared backup: %v", list, err)
		}
		if _, err = m.CheckImageDeploy("api", "v1", false); err != nil {
			t.Fatalf("%s: a backup hook stops the deploy check: %v", list, err)
		}
		if _, err = DecodeJSON([]byte("{" + fmt.Sprintf(base, list) + "}")); err == nil || !strings.Contains(err.Error(), "unknown backup") {
			t.Fatalf("%s hook of a backup that is not declared: %v", list, err)
		}
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
	for _, name := range []string{"postgres"} {
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

func TestBackupScheduleIsValidated(t *testing.T) {
	const pre = `{"project":"demo","network":"demo","apps":{"db":{"kind":"compose","file":"db/compose.yaml"}},"destinations":{"offsite":{"type":"s3","bucket":"bkt","access_key_secret":"A","secret_key_secret":"B"}},"backups":{"main":{"app":"db","volume":"dumps","destination":"offsite"`
	for _, ok := range []string{`,"schedule":"0 0 * * *"`, `,"schedule":"*/2 * * * *"`, ``} {
		if _, err := DecodeJSON([]byte(pre + ok + `}}}`)); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{`,"schedule":"0 0 * *"`, `,"schedule":"61 * * * *"`, `,"schedule":"daily"`} {
		if _, err := DecodeJSON([]byte(pre + bad + `}}}`)); err == nil || !strings.Contains(err.Error(), "schedule") {
			t.Errorf("%q: %v", bad, err)
		}
	}
}

func TestImageReposListsTheSortedDistinctRepositoriesOfOneRegistry(t *testing.T) {
	m := Manifest{Project: "demo", Apps: map[string]App{
		"api":    {Kind: "image", Image: "ghcr.io/acme/api:1.2"},
		"worker": {Kind: "image", Image: "ghcr.io/acme/api"},
		"web":    {Kind: "image", Image: "ghcr.io/acme/web:latest"},
		"proxy":  {Kind: "image", Image: "nginx"},
		"shop":   {Kind: "compose", File: "compose.yml"},
	}}
	if got := strings.Join(m.ImageRepos("ghcr.io"), ","); got != "acme/api,acme/web" {
		t.Fatalf("ghcr.io repositories %q, want acme/api,acme/web", got)
	}
	if got := strings.Join(m.ImageRepos("docker.io"), ","); got != "library/nginx" {
		t.Fatalf("docker.io repositories %q, want library/nginx", got)
	}
	if got := m.ImageRepos("quay.io"); len(got) != 0 {
		t.Fatalf("quay.io repositories %v, want none", got)
	}
}

func TestLoadTakesTheEnvironmentRuleOfScope(t *testing.T) {
	dir := t.TempDir()
	base := `project: demo
network: demo
apps:
  api:
    kind: image
    image: ghcr.io/acme/api
    roles:
      web: {}
`
	if err := os.WriteFile(filepath.Join(dir, "berthy.yml"), []byte(base), 0600); err != nil {
		t.Fatal(err)
	}
	for _, env := range []string{"prod", strings.Repeat("a", 50), "a-1"} {
		if _, err := Load(dir, env); err != nil {
			t.Errorf("%q: %v", env, err)
		}
	}
	for _, env := range []string{"-prod", "prod_1", strings.Repeat("a", 64), ""} {
		if _, err := Load(dir, env); err == nil {
			t.Errorf("%q loaded", env)
		}
	}
}

func TestLoadRefusesAServiceNameOver63CharactersForThatEnvironment(t *testing.T) {
	dir := t.TempDir()
	base := "project: demo\nnetwork: demo\napps:\n  api:\n    kind: image\n    image: ghcr.io/acme/api\n    roles:\n      web: {}\n"
	if err := os.WriteFile(filepath.Join(dir, "berthy.yml"), []byte(base), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadDetailed(dir, "prod"); err != nil {
		t.Fatal(err)
	}
	env := strings.Repeat("a", 51)
	_, err := LoadDetailed(dir, env)
	m := Manifest{Project: "demo"}
	ps, ok := err.(Problems)
	if !ok || len(ps) != 1 || ps[0].Message != m.ServiceNameError(env, "api", "web") || ps[0].File != "berthy.yml" || ps[0].Line != 8 || ps[0].Path != "apps.api.roles.web" {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestLoadChecksManagedIngressConnectorServiceName(t *testing.T) {
	dir := t.TempDir()
	base := "project: demo\nnetwork: demo\ningress:\n  cloudflare:\n    account_id: acct\n    api_token_secret: CF_TOKEN\n    image: cloudflare/cloudflared:2026.11.0\napps:\n  api:\n    kind: image\n    image: ghcr.io/acme/api\n    roles:\n      web: {}\n"
	if err := os.WriteFile(filepath.Join(dir, "berthy.yml"), []byte(base), 0600); err != nil {
		t.Fatal(err)
	}
	m := Manifest{Project: "demo"}
	for _, n := range []int{40, 41} {
		env := strings.Repeat("a", n)
		_, err := LoadDetailed(dir, env)
		if n == 40 {
			if err != nil {
				t.Fatalf("63-character connector: %v", err)
			}
			continue
		}
		ps, ok := err.(Problems)
		if !ok || len(ps) != 1 || ps[0].Path != "ingress.cloudflare" || ps[0].File != "berthy.yml" || ps[0].Line != 4 || ps[0].Message != m.ServiceNameError(env, ConnectorAppName, "connector") {
			t.Fatalf("64-character connector: %v (%+v)", err, ps)
		}
	}
}

func TestLoadChecksScopedVolumeIdentifierAtItsMount(t *testing.T) {
	for _, n := range []int{118, 119} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			dir := t.TempDir()
			volume := strings.Repeat("v", n)
			base := fmt.Sprintf("project: demo\nnetwork: demo\napps:\n  api:\n    kind: image\n    image: ghcr.io/acme/api\n    roles:\n      web:\n        volumes:\n          - %s:/data\n", volume)
			if err := os.WriteFile(filepath.Join(dir, "berthy.yml"), []byte(base), 0600); err != nil {
				t.Fatal(err)
			}
			_, err := LoadDetailed(dir, "prod")
			if n == 118 {
				if err != nil {
					t.Fatalf("128-character volume: %v", err)
				}
				return
			}
			ps, ok := err.(Problems)
			if !ok || len(ps) != 1 || ps[0].Path != "apps.api.roles.web.volumes.0" || ps[0].File != "berthy.yml" || ps[0].Line != 10 || !strings.Contains(ps[0].Message, "shorten the volume, project or environment") || !strings.Contains(ps[0].Message, "demo-prod_"+volume) {
				t.Fatalf("129-character volume: %v (%+v)", err, ps)
			}
		})
	}
}

func TestAManifestWithoutANetworkLoads(t *testing.T) {
	dir := t.TempDir()
	data := "project: shop\napps:\n  api:\n    kind: image\n    image: alpine\n    roles:\n      web: {}\n"
	if err := os.WriteFile(filepath.Join(dir, "berthy.yml"), []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir, "prod"); err != nil {
		t.Fatal(err)
	}
}

func TestNetworkNameDefaultsToProjectAndEnvironment(t *testing.T) {
	m := &Manifest{Project: "shop"}
	if got := m.NetworkName("prod"); got != "shop-prod" {
		t.Fatalf("network = %q", got)
	}
	m.Network = "existing"
	if got := m.NetworkName("prod"); got != "existing" {
		t.Fatalf("override = %q", got)
	}
}

func TestTheDefaultNetworkOfTheLongestPairIsAValidNetworkName(t *testing.T) {
	m := &Manifest{Project: strings.Repeat("p", 29)}
	if got := m.NetworkName(strings.Repeat("e", 29)); !ValidNetwork(got) {
		t.Fatalf("invalid network %q", got)
	}
}

func TestLoadRejectsCollidingRoleDNSNames(t *testing.T) {
	for _, tc := range []struct{ name, apps, second, first, clash string }{
		{"two aliases", "  api:\n    kind: image\n    image: alpine\n    roles:\n      blue-web: {}\n  api-blue:\n    kind: image\n    image: alpine\n    roles:\n      web: {}\n", "api-blue/web", "api/blue-web", "api-blue-web"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			doc := "project: shop\napps:\n" + tc.apps
			if err := os.WriteFile(filepath.Join(dir, "berthy.yml"), []byte(doc), 0600); err != nil {
				t.Fatal(err)
			}
			_, err := LoadDetailed(dir, "prod")
			var ps Problems
			if !errors.As(err, &ps) || len(ps) != 1 {
				t.Fatalf("problems: %v", err)
			}
			wantPath := "apps." + strings.Replace(tc.second, "/", ".roles.", 1)
			if ps[0].Path != wantPath || ps[0].File != "berthy.yml" || !strings.Contains(ps[0].Message, tc.first) || !strings.Contains(ps[0].Message, tc.second) || !strings.Contains(ps[0].Message, tc.clash) {
				t.Fatalf("problem: %+v", ps[0])
			}
		})
	}
}

func TestLoadAllowsANameThatMayBeABoundConnector(t *testing.T) {
	dir := t.TempDir()
	doc := "project: shop\ningress:\n  cloudflare:\n    account_id: acct\n    api_token_secret: CF_TOKEN\n    image: cloudflare/cloudflared:2026.11.0\napps:\n  shop-prod-ingress:\n    kind: image\n    image: alpine\n    roles:\n      connector: {}\n"
	if err := os.WriteFile(filepath.Join(dir, "berthy.yml"), []byte(doc), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadDetailed(dir, "prod")
	if err != nil {
		t.Fatalf("error: %v", err)
	}
}

func TestNetworkNameProblemsChecksEveryNameAfterAClash(t *testing.T) {
	m := Manifest{Project: "shop", Apps: map[string]App{
		"api":      {Kind: "image", Roles: map[string]Role{"blue-web": {}}},
		"api-blue": {Kind: "image", Roles: map[string]Role{"web": {}}},
		"last":     {Kind: "image", Roles: map[string]Role{"main": {}}},
	}}
	got := m.NetworkNameProblems("prod", func(app, role string) (string, bool) {
		if app == "api-blue" {
			return "last-main", true
		}
		return "", true
	}, "")
	if len(got) != 2 || !strings.Contains(got[1].Message, "last-main") {
		t.Fatalf("problems: %+v", got)
	}
}

func TestLoadAllowsAServiceNameThatMayBeBoundElsewhere(t *testing.T) {
	dir := t.TempDir()
	doc := "project: shop\napps:\n  api:\n    kind: image\n    image: alpine\n    roles:\n      web: {}\n  shop-prod-api:\n    kind: image\n    image: alpine\n    roles:\n      web: {}\n"
	if err := os.WriteFile(filepath.Join(dir, "berthy.yml"), []byte(doc), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadDetailed(dir, "prod"); err != nil {
		t.Fatal(err)
	}
}

func TestLoadRefusesSharedComposeProjects(t *testing.T) {
	for _, project := range []string{"shared", "demo-prod-alpha"} {
		t.Run(project, func(t *testing.T) {
			first := "shared"
			if project == "demo-prod-alpha" {
				first = ""
			}
			src := "project: demo\napps:\n  alpha:\n    kind: compose\n    file: alpha.yml\n"
			if first != "" {
				src += "    project_name: " + first + "\n"
			}
			src += "  beta:\n    kind: compose\n    file: beta.yml\n    project_name: " + project + "\n"
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "berthy.yml"), []byte(src), 0600); err != nil {
				t.Fatal(err)
			}
			_, err := LoadDetailed(dir, "prod")
			ps, ok := err.(Problems)
			want := "Compose project " + project + " is shared by apps alpha and beta"
			if !ok || len(ps) != 1 || ps[0].Path != "apps.beta" || ps[0].Message != want {
				t.Fatalf("problems: %v", err)
			}
		})
	}
}
