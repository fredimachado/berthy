package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// allAppsManifest is a project of three image apps: api needs postgres and redis first.
const allAppsManifest = `project: demo
network: demo
apps:
  api:
    kind: image
    image: ghcr.io/acme/api
    depends_on: [postgres, redis]
    roles:
      web: {}
  postgres:
    kind: image
    image: postgres
    roles:
      db: {}
  redis:
    kind: image
    image: redis
    roles:
      cache: {}
`

// fakeDeployAgent is an agent that records what berthy deploy sends and answers as each app is told to.
type fakeDeployAgent struct {
	mu         sync.Mutex
	deploys    []map[string]any // the body of each deploy request, in the order received
	reads      []string         // the path of each read request
	failOp     map[string]bool  // apps whose operation ends failed
	failPlan   map[string]bool  // apps whose dry run is refused
	undeployed map[string]bool  // apps that have no applied release
	held       string           // the app whose deploy the agent holds for approval
	protected  bool             // whether the agent is configured for a protected environment
}

// deployAllFixture starts a fake agent and writes the manifest and a config that point at it.
func deployAllFixture(t *testing.T, manifestYAML string, agent *fakeDeployAgent) (dir, cfg string) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(agent.serve))
	t.Cleanup(server.Close)
	dir = t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "berthy.yml"), []byte(manifestYAML), 0600); err != nil {
		t.Fatal(err)
	}
	cfg = filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfg, []byte(fmt.Sprintf(`{"projects":{"demo":{"environments":{"dev":{"url":%q,"token_env":"BERTHY_TEST_TOKEN"}}}}}`, server.URL)), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BERTHY_TEST_TOKEN", "test-token")
	return dir, cfg
}

func (f *fakeDeployAgent) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	const base = "/v1/projects/demo/environments/dev/"
	rest := strings.TrimPrefix(r.URL.Path, base)
	switch {
	case r.Method == "GET" && rest == "agent":
		fmt.Fprintf(w, `{"version":"test","protected":%t}`, f.protected)
	case r.Method == "POST" && rest == "deploys":
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad body", 400)
			return
		}
		f.deploys = append(f.deploys, body)
		app, _ := body["app"].(string)
		switch {
		case body["dry_run"] == true && f.failPlan[app]:
			w.WriteHeader(400)
			fmt.Fprint(w, `{"code":"operation_failed","message":"plan refused for `+app+`"}`)
		case body["dry_run"] == true:
			fmt.Fprintf(w, `{"app":%q,"roles":["web"]}`, app)
		case f.held == app:
			w.Header().Set("Berthy-Approval", "pending")
			w.WriteHeader(202)
			fmt.Fprint(w, `{"approval_id":"ap1","expires":"2030-01-01T00:00:00Z","plan":{}}`)
		default:
			w.WriteHeader(202)
			fmt.Fprintf(w, `{"operation_id":"op-%s","events_url":"%soperations/op-%s/events"}`, app, base, app)
		}
	case r.Method == "GET" && strings.HasPrefix(rest, "applied/"):
		f.reads = append(f.reads, r.URL.Path)
		app := strings.TrimPrefix(rest, "applied/")
		if f.undeployed[app] {
			w.WriteHeader(404)
			fmt.Fprint(w, `{"code":"not_deployed","message":"app is not deployed"}`)
			return
		}
		fmt.Fprintf(w, `{"id":4,"digest":"example/%s@sha256:%s","tag":"v1.0.2"}`, app, strings.Repeat("a", 64))
	case r.Method == "GET" && strings.HasPrefix(rest, "operations/") && strings.HasSuffix(rest, "/events"):
		fmt.Fprint(w, "id: 1\nevent: accepted\ndata: {}\n\n")
	case r.Method == "GET" && strings.HasPrefix(rest, "operations/op-"):
		app := strings.TrimPrefix(rest, "operations/op-")
		if f.failOp[app] {
			fmt.Fprintf(w, `{"id":"op-%s","state":"failed","app":%q,"error":"rollout failed"}`, app, app)
			return
		}
		fmt.Fprintf(w, `{"id":"op-%s","state":"succeeded","app":%q}`, app, app)
	default:
		f.reads = append(f.reads, r.URL.Path)
		http.NotFound(w, r)
	}
}

func (f *fakeDeployAgent) apps() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var apps []string
	for _, d := range f.deploys {
		apps = append(apps, d["app"].(string))
	}
	return apps
}

// deployAllCommand runs `berthy deploy` without an app against the fixture.
func deployAllCommand(t *testing.T, dir, cfg string, extra ...string) (code int, out, errs string) {
	t.Helper()
	var o, e bytes.Buffer
	args := append([]string{"deploy", "-e", "dev", "--config", cfg, "--manifest-dir", dir}, extra...)
	code = Run(args, &o, &e)
	return code, o.String(), e.String()
}

func TestDeployAllRunsEveryAppInDependsOnOrder(t *testing.T) {
	agent := &fakeDeployAgent{}
	dir, cfg := deployAllFixture(t, allAppsManifest, agent)
	code, out, errs := deployAllCommand(t, dir, cfg, "--version", "api=v1.0.2", "--commit", "abc123")
	if code != 0 {
		t.Fatalf("exit %d: %s\n%s", code, errs, out)
	}
	if got := strings.Join(agent.apps(), ","); got != "postgres,redis,api" {
		t.Fatalf("deploy order %s", got)
	}
	ids := map[string]bool{}
	for _, d := range agent.deploys {
		app := d["app"].(string)
		if id, _ := d["request_id"].(string); id == "" || ids[id] {
			t.Fatalf("%s: request id %q is empty or repeated", app, id)
		} else {
			ids[id] = true
		}
		if d["commit"] != "abc123" {
			t.Errorf("%s: commit %v", app, d["commit"])
		}
		if app == "api" {
			if d["version"] != "v1.0.2" || d["keep_version"] != nil || d["keep_digest"] != nil {
				t.Errorf("api: version %v keep_version %v keep_digest %v", d["version"], d["keep_version"], d["keep_digest"])
			}
			continue
		}
		if d["version"] != "" || d["keep_version"] != true {
			t.Errorf("%s: version %q keep_version %v, want to keep the current version", app, d["version"], d["keep_version"])
		}
	}
	headers := []string{"==> deploy postgres (keeps v1.0.2)", "==> deploy redis (keeps v1.0.2)", "==> deploy api (v1.0.2)"}
	last := -1
	for _, h := range headers {
		i := strings.Index(out, h)
		if i < 0 || i < last {
			t.Fatalf("header %q is missing or out of order in:\n%s", h, out)
		}
		last = i
	}
	for _, line := range []string{"postgres  succeeded", "redis  succeeded", "api  succeeded"} {
		if !strings.Contains(out, line) {
			t.Errorf("summary lacks %q:\n%s", line, out)
		}
	}
}

func TestDeployAllStopsAtTheFirstFailure(t *testing.T) {
	agent := &fakeDeployAgent{failOp: map[string]bool{"redis": true}}
	dir, cfg := deployAllFixture(t, allAppsManifest, agent)
	code, out, errs := deployAllCommand(t, dir, cfg, "--version", "api=v1.0.2")
	if code != 1 {
		t.Fatalf("exit %d: %s\n%s", code, errs, out)
	}
	if got := strings.Join(agent.apps(), ","); got != "postgres,redis" {
		t.Fatalf("api was requested after redis failed: %s", got)
	}
	if !strings.Contains(errs, "deploy stopped at redis; not run: api") {
		t.Fatalf("stderr %q", errs)
	}
}

func TestDeployAllStoppedAtTheLastAppNamesNothingNotRun(t *testing.T) {
	agent := &fakeDeployAgent{failOp: map[string]bool{"api": true}}
	dir, cfg := deployAllFixture(t, allAppsManifest, agent)
	code, _, errs := deployAllCommand(t, dir, cfg, "--version", "api=v1.0.2")
	if code != 1 || !strings.HasSuffix(strings.TrimSpace(errs), "deploy stopped at api") {
		t.Fatalf("exit %d: %q", code, errs)
	}
}

func TestDeployAllPreparesEveryAppBeforeItSendsAnything(t *testing.T) {
	agent := &fakeDeployAgent{}
	dir, cfg := deployAllFixture(t, allAppsManifest+"  zstack:\n    kind: compose\n    file: missing.yml\n", agent)
	code, out, errs := deployAllCommand(t, dir, cfg, "--version", "api=v1.0.2")
	if code != 1 {
		t.Fatalf("exit %d: %s\n%s", code, errs, out)
	}
	if len(agent.deploys) != 0 || len(agent.reads) != 0 {
		t.Fatalf("requests were sent: deploys %v reads %v", agent.apps(), agent.reads)
	}
	if !strings.Contains(errs, "zstack") || !strings.Contains(errs, "nothing was deployed") {
		t.Fatalf("stderr %q", errs)
	}
}

func TestDeployAllVersionRulesAreRefusedBeforeAnyRequest(t *testing.T) {
	singleApp := "project: demo\nnetwork: demo\napps:\n  api:\n    kind: image\n    image: ghcr.io/acme/api\n    roles:\n      web: {}\n"
	withCompose := allAppsManifest + "  zstack:\n    kind: compose\n    file: compose.yml\n"
	for _, tc := range []struct {
		name, manifest string
		args           []string
		want           string
	}{
		{"plain version with two image apps", allAppsManifest, []string{"--version", "v1"}, "exactly one image app"},
		{"unknown app", allAppsManifest, []string{"--version", "cache=v1"}, "cache"},
		{"compose app", withCompose, []string{"--version", "zstack=v1"}, "zstack"},
		{"plain and named", allAppsManifest, []string{"--version", "v1", "--version", "api=v2"}, "cannot be combined"},
		{"two plain", singleApp, []string{"--version", "v1", "--version", "v2"}, "more than once"},
		{"empty reference", allAppsManifest, []string{"--version", "api="}, "api="},
		{"the same app twice", allAppsManifest, []string{"--version", "api=v1", "--version", "api=v2"}, "api"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agent := &fakeDeployAgent{}
			dir, cfg := deployAllFixture(t, tc.manifest, agent)
			code, _, errs := deployAllCommand(t, dir, cfg, tc.args...)
			if code != 2 || !strings.Contains(errs, tc.want) {
				t.Fatalf("exit %d: %q, want exit 2 naming %q", code, errs, tc.want)
			}
			if len(agent.deploys) != 0 || len(agent.reads) != 0 {
				t.Fatalf("requests were sent: %v %v", agent.apps(), agent.reads)
			}
		})
	}
}

func TestDeployAllAcceptsAPlainVersionForTheOnlyImageApp(t *testing.T) {
	agent := &fakeDeployAgent{}
	manifest := "project: demo\nnetwork: demo\napps:\n  api:\n    kind: image\n    image: ghcr.io/acme/api\n    roles:\n      web: {}\n"
	dir, cfg := deployAllFixture(t, manifest, agent)
	if code, out, errs := deployAllCommand(t, dir, cfg, "--version", "v1.0.2"); code != 0 {
		t.Fatalf("exit %d: %s\n%s", code, errs, out)
	}
	if len(agent.deploys) != 1 || agent.deploys[0]["version"] != "v1.0.2" || agent.deploys[0]["keep_version"] != nil {
		t.Fatalf("requests %v", agent.deploys)
	}
}

func TestSingleAppDeployReadsOneVersionOnly(t *testing.T) {
	agent := &fakeDeployAgent{}
	dir, cfg := deployAllFixture(t, allAppsManifest, agent)
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"twice", []string{"--version", "a", "--version", "b"}, "--version given more than once"},
		{"APP=REF", []string{"--version", "api=x"}, "--version APP=REF is for deploy without an app"},
		{"none", nil, "deploy requires --version"},
	} {
		var o, e bytes.Buffer
		args := append([]string{"deploy", "api", "-e", "dev", "--config", cfg, "--manifest-dir", dir}, tc.args...)
		if code := Run(args, &o, &e); code != 2 || strings.TrimSpace(e.String()) != tc.want {
			t.Errorf("%s: exit %d: %q, want exit 2 and %q", tc.name, code, e.String(), tc.want)
		}
	}
	if len(agent.deploys) != 0 {
		t.Fatalf("requests were sent: %v", agent.apps())
	}
}

func TestDeployAllKeepsTheVersionAnAppRunsNow(t *testing.T) {
	agent := &fakeDeployAgent{}
	dir, cfg := deployAllFixture(t, allAppsManifest, agent)
	if code, out, errs := deployAllCommand(t, dir, cfg); code != 0 {
		t.Fatalf("exit %d: %s\n%s", code, errs, out)
	}
	if got := strings.Join(agent.apps(), ","); got != "postgres,redis,api" {
		t.Fatalf("deploy order %s", got)
	}
	for _, d := range agent.deploys {
		if d["keep_version"] != true || d["version"] != "" {
			t.Errorf("%v: want keep_version and no version", d)
		}
		// The digest the CLI read is the one the agent must still find applied.
		if want := fmt.Sprintf("example/%s@sha256:%s", d["app"], strings.Repeat("a", 64)); d["keep_digest"] != want {
			t.Errorf("%v: keep_digest %v, want %s", d["app"], d["keep_digest"], want)
		}
	}
	if len(agent.reads) != 3 {
		t.Fatalf("the preflight asked %v", agent.reads)
	}
}

func TestDeployAllRefusesAnImageAppThatWasNeverDeployedBeforeDeployingAnything(t *testing.T) {
	agent := &fakeDeployAgent{undeployed: map[string]bool{"api": true}}
	dir, cfg := deployAllFixture(t, allAppsManifest, agent)
	code, _, errs := deployAllCommand(t, dir, cfg)
	want := "image app api is not deployed yet: give --version api=REF (nothing was deployed)"
	if code != 2 || strings.TrimSpace(errs) != want {
		t.Fatalf("exit %d: %q", code, errs)
	}
	if len(agent.deploys) != 0 {
		t.Fatalf("deploys were sent: %v", agent.apps())
	}
	// Giving it a version is what the message asks for.
	agent.undeployed = nil
	if code, _, errs = deployAllCommand(t, dir, cfg, "--version", "api=v1"); code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
}

func TestDeployAllRefusesFlagsThatNeedAnApp(t *testing.T) {
	agent := &fakeDeployAgent{}
	dir, cfg := deployAllFixture(t, allAppsManifest, agent)
	for _, tc := range []struct{ flag, value string }{{"--adopt", ""}, {"--replace-existing", ""}, {"--allow-recreate", ""}, {"--request-id", "r1"}} {
		args := []string{"--version", "api=v1", tc.flag}
		if tc.value != "" {
			args = append(args, tc.value)
		}
		code, _, errs := deployAllCommand(t, dir, cfg, args...)
		if want := tc.flag + " needs an app: berthy deploy APP " + tc.flag; code != 2 || strings.TrimSpace(errs) != want {
			t.Errorf("%s: exit %d: %q, want %q", tc.flag, code, errs, want)
		}
	}
	if len(agent.deploys) != 0 || len(agent.reads) != 0 {
		t.Fatalf("requests were sent: %v %v", agent.apps(), agent.reads)
	}
}

func TestDeployAllJSONIsOneDocumentWithEveryApp(t *testing.T) {
	agent := &fakeDeployAgent{failOp: map[string]bool{"redis": true}}
	dir, cfg := deployAllFixture(t, allAppsManifest, agent)
	code, out, errs := deployAllCommand(t, dir, cfg, "--version", "api=v1.0.2", "--json")
	if code != 1 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	var doc struct {
		Apps []struct {
			App    string         `json:"app"`
			Exit   *int           `json:"exit"`
			State  string         `json:"state"`
			Result map[string]any `json:"result"`
		} `json:"apps"`
	}
	dec := json.NewDecoder(strings.NewReader(out))
	if err := dec.Decode(&doc); err != nil || dec.More() {
		t.Fatalf("stdout is not one JSON document (%v):\n%s", err, out)
	}
	if len(doc.Apps) != 3 {
		t.Fatalf("apps %+v", doc.Apps)
	}
	if a := doc.Apps[0]; a.App != "postgres" || a.Exit == nil || *a.Exit != 0 || a.Result["state"] != "succeeded" {
		t.Errorf("postgres %+v", a)
	}
	if a := doc.Apps[1]; a.App != "redis" || a.Exit == nil || *a.Exit != 1 || a.Result["state"] != "failed" {
		t.Errorf("redis %+v", a)
	}
	if a := doc.Apps[2]; a.App != "api" || a.State != "not_run" || a.Exit != nil || a.Result != nil {
		t.Errorf("api %+v", a)
	}
}

func TestDeployAllJSONOfASuccessfulRun(t *testing.T) {
	agent := &fakeDeployAgent{}
	dir, cfg := deployAllFixture(t, allAppsManifest, agent)
	code, out, errs := deployAllCommand(t, dir, cfg, "--version", "api=v1.0.2", "--json")
	if code != 0 || !json.Valid([]byte(out)) || strings.Count(out, `"exit": 0`) != 3 || strings.Contains(out, "not_run") {
		t.Fatalf("exit %d: %s\n%s", code, errs, out)
	}
}

func TestDeployAllDryRunPlansEveryAppAndReportsFailures(t *testing.T) {
	agent := &fakeDeployAgent{failPlan: map[string]bool{"postgres": true}}
	dir, cfg := deployAllFixture(t, allAppsManifest, agent)
	code, out, errs := deployAllCommand(t, dir, cfg, "--version", "api=v1.0.2", "--dry-run")
	if code != 1 {
		t.Fatalf("exit %d: %s\n%s", code, errs, out)
	}
	if got := strings.Join(agent.apps(), ","); got != "postgres,redis,api" {
		t.Fatalf("a failing plan stopped the dry run: %s", got)
	}
	for _, d := range agent.deploys {
		if d["dry_run"] != true {
			t.Errorf("%v is not a dry run", d)
		}
	}
	if !strings.Contains(errs, "plan refused for postgres") || !strings.Contains(out, "postgres  failed") || !strings.Contains(out, "redis  planned") {
		t.Fatalf("stdout:\n%s\nstderr: %s", out, errs)
	}
}

func TestDeployAllHeldForApprovalStopsTheRun(t *testing.T) {
	agent := &fakeDeployAgent{held: "redis"}
	dir, cfg := deployAllFixture(t, allAppsManifest, agent)
	code, out, errs := deployAllCommand(t, dir, cfg, "--version", "api=v1.0.2", "--json")
	if code != exitHeld {
		t.Fatalf("exit %d: %s\n%s", code, errs, out)
	}
	if got := strings.Join(agent.apps(), ","); got != "postgres,redis" {
		t.Fatalf("requests %s", got)
	}
	if !json.Valid([]byte(out)) || !strings.Contains(out, "not_run") || !strings.Contains(out, "approval_id") {
		t.Fatalf("stdout:\n%s", out)
	}
}

// TestDeployAllAgainstAnAgentWithoutTheAppliedRead: the 404 of an agent that does not have the route is not a never-deployed app.
func TestDeployAllAgainstAnAgentWithoutTheAppliedRead(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(404)
		fmt.Fprint(w, `{"code":"not_found","message":"not found"}`)
	}))
	defer server.Close()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "berthy.yml"), []byte(allAppsManifest), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfg, []byte(fmt.Sprintf(`{"projects":{"demo":{"environments":{"dev":{"url":%q,"token_env":"BERTHY_TEST_TOKEN"}}}}}`, server.URL)), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BERTHY_TEST_TOKEN", "test-token")
	code, _, errs := deployAllCommand(t, dir, cfg)
	if code != 1 || !strings.Contains(errs, "agent returned HTTP 404") || strings.Contains(errs, "not deployed yet") {
		t.Fatalf("exit %d: %q", code, errs)
	}
}
