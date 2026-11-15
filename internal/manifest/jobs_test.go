package manifest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// jobManifest returns a valid JSON manifest with an image app "api" of two roles, an image app "solo" of one role, and a
// Compose app "db", plus the given members of "jobs".
func jobManifest(jobs string) string {
	return `{"project":"demo","network":"demo","apps":{"api":{"kind":"image","image":"ghcr.io/acme/api","roles":{"web":{},"worker":{}}},"solo":{"kind":"image","image":"ghcr.io/acme/solo","roles":{"main":{}}},"db":{"kind":"compose","file":"db/compose.yml"}},"jobs":{` + jobs + `}}`
}

func TestJobValidation(t *testing.T) {
	valid := map[string]string{
		"single role omitted":     `"x":{"app":"solo","command":["true"]}`,
		"role named":              `"x":{"app":"api","role":"worker","command":["true"]}`,
		"schedule and timeout":    `"x":{"app":"api","role":"web","schedule":"30 2 * * *","timeout":"90m","command":["true"]}`,
		"macro schedule":          `"x":{"app":"solo","schedule":"@daily","command":["true"]}`,
		"timeout at one second":   `"x":{"app":"solo","timeout":"1s","command":["true"]}`,
		"timeout at 24 hours":     `"x":{"app":"solo","timeout":"24h","command":["true"]}`,
		"compose without service": `"x":{"app":"db","command":["true"]}`,
		"compose with service":    `"x":{"app":"db","service":"postgres","command":["true"]}`,
	}
	for name, members := range valid {
		if _, err := DecodeJSON([]byte(jobManifest(members))); err != nil {
			t.Errorf("%s refused: %v", name, err)
		}
	}
	invalid := map[string]struct{ members, want string }{
		"schedule minute out of range": {`"x":{"app":"solo","schedule":"60 * * * *","command":["true"]}`, "job x: invalid schedule: invalid minute field"},
		"schedule field count":         {`"x":{"app":"solo","schedule":"* * * *","command":["true"]}`, "job x: invalid schedule: expected five fields"},
		"schedule unknown macro":       {`"x":{"app":"solo","schedule":"@reboot","command":["true"]}`, "job x: invalid schedule:"},
		"timeout zero":                 {`"x":{"app":"solo","timeout":"0s","command":["true"]}`, "job x: invalid timeout"},
		"timeout below one second":     {`"x":{"app":"solo","timeout":"500ms","command":["true"]}`, "job x: invalid timeout"},
		"timeout above 24 hours":       {`"x":{"app":"solo","timeout":"25h","command":["true"]}`, "job x: invalid timeout"},
		"timeout negative":             {`"x":{"app":"solo","timeout":"-1h","command":["true"]}`, "job x: invalid timeout"},
		"timeout not a duration":       {`"x":{"app":"solo","timeout":"soon","command":["true"]}`, "job x: invalid timeout"},
		"image app with service":       {`"x":{"app":"api","service":"web","command":["true"]}`, "job x: use role for an image app"},
		"image app unknown role":       {`"x":{"app":"api","role":"cron","command":["true"]}`, "job x: role is not a role of the app"},
		"several roles, no role":       {`"x":{"app":"api","command":["true"]}`, "job x: role required for an app with several roles"},
		"compose with role":            {`"x":{"app":"db","role":"web","command":["true"]}`, "job x: use service for a Compose app"},
		"unknown app":                  {`"x":{"app":"missing","command":["true"]}`, "job x: unknown app"},
	}
	for name, tc := range invalid {
		_, err := DecodeJSON([]byte(jobManifest(tc.members)))
		if err == nil {
			t.Errorf("%s accepted", name)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error %q does not contain %q", name, err, tc.want)
		}
	}
}

func TestJobCannotTargetConnectorAppWhenIngressIsManaged(t *testing.T) {
	ingress := `"ingress":{"cloudflare":{"account_id":"account","api_token_secret":"CF_TOKEN","image":"cloudflare/cloudflared:v1"}}`
	apps := `"apps":{"api":{"kind":"image","image":"ghcr.io/acme/api","roles":{"web":{}}}}`
	managed := `{"project":"demo","network":"demo",` + ingress + `,` + apps + `,"jobs":{"x":{"app":"ingress","command":["true"]}}}`
	if _, err := DecodeJSON([]byte(managed)); err == nil || !strings.Contains(err.Error(), "job x: app ingress is reserved for the managed ingress connector") {
		t.Fatalf("job on the connector accepted with managed ingress: %v", err)
	}
	plain := `{"project":"demo","network":"demo","apps":{"ingress":{"kind":"image","image":"ghcr.io/acme/ingress","roles":{"main":{}}}},"jobs":{"x":{"app":"ingress","command":["true"]}}}`
	if _, err := DecodeJSON([]byte(plain)); err != nil {
		t.Fatalf("app named ingress refused without managed ingress: %v", err)
	}
}

func TestJobTimeoutDefaultsToOneHour(t *testing.T) {
	m, err := DecodeJSON([]byte(jobManifest(`"x":{"app":"solo","command":["true"]},"y":{"app":"solo","timeout":"30m","command":["true"]}`)))
	if err != nil {
		t.Fatal(err)
	}
	if got := m.Jobs["x"].Timeout; got != "1h" {
		t.Fatalf("default timeout %q, want \"1h\"", got)
	}
	if got := m.Jobs["y"].Timeout; got != "30m" {
		t.Fatalf("explicit timeout %q, want \"30m\"", got)
	}
	if d := m.Jobs["x"].TimeoutDuration(); d != time.Hour {
		t.Fatalf("default duration %v", d)
	}
	if d := m.Jobs["y"].TimeoutDuration(); d != 30*time.Minute {
		t.Fatalf("explicit duration %v", d)
	}
}

func TestJobsLoadFromYAML(t *testing.T) {
	dir := t.TempDir()
	base := "project: demo\nnetwork: demo\napps:\n  api:\n    kind: image\n    image: ghcr.io/acme/api\n    roles:\n      web: {}\njobs:\n  nightly:\n    app: api\n    schedule: \"30 2 * * *\"\n    timeout: 2h\n    command: [php, artisan, prune]\n  hourly:\n    app: api\n    schedule: \"@hourly\"\n    command: [\"true\"]\n"
	if err := os.WriteFile(filepath.Join(dir, "berthy.yml"), []byte(base), 0600); err != nil {
		t.Fatal(err)
	}
	m, err := Load(dir, "dev")
	if err != nil {
		t.Fatal(err)
	}
	if j := m.Jobs["nightly"]; j.Schedule != "30 2 * * *" || j.Timeout != "2h" {
		t.Fatalf("nightly not loaded: %+v", j)
	}
	if got := m.Jobs["hourly"].Timeout; got != "1h" {
		t.Fatalf("hourly default timeout %q", got)
	}
	typo := strings.Replace(base, "timeout: 2h", "timout: 2h", 1)
	if err := os.WriteFile(filepath.Join(dir, "berthy.yml"), []byte(typo), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir, "dev"); err == nil {
		t.Fatal("misspelled job field accepted")
	}
}
