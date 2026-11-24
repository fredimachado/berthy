package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"berthy/internal/deploy"
	"berthy/internal/manifest"
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
	mu           sync.Mutex
	deploys      []map[string]any // the body of each deploy request, in the order received
	reads        []string         // the path of each read request
	failOp       map[string]bool  // apps whose operation ends failed
	failPlan     map[string]bool  // apps whose dry run is refused
	undeployed   map[string]bool  // apps that have no applied release
	held         string           // the app whose deploy the agent holds for approval
	mixedPlans   bool
	blockCompose bool
	legacy       bool
	lookupFail   string
	lookups      []string
	protected    bool // whether the agent is configured for a protected environment
	plans        map[string]string
	applied      map[string]string
	calls        int
	policyReply  string
	policyReads  int
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
	f.calls++
	const base = "/v1/projects/demo/environments/dev/"
	rest := strings.TrimPrefix(r.URL.Path, base)
	switch {
	case r.Method == "GET" && rest == "agent":
		f.policyReads++
		if f.policyReply != "" {
			fmt.Fprint(w, f.policyReply)
		} else if f.legacy {
			fmt.Fprintf(w, `{"project":"demo","environment":"dev","version":"test","protected":%t}`, f.protected)
		} else {
			fmt.Fprintf(w, `{"project":"demo","environment":"dev","version":"test","protected":%t,"effective_protected":%t}`, f.protected, f.protected)
		}
	case r.Method == "POST" && rest == "images/resolve" && f.mixedPlans:
		var lookup struct {
			App string `json:"app"`
			Tag string `json:"tag"`
		}
		if err := json.NewDecoder(r.Body).Decode(&lookup); err != nil {
			http.Error(w, "bad lookup", 400)
			return
		}
		f.lookups = append(f.lookups, lookup.App)
		if lookup.App == f.lookupFail {
			w.WriteHeader(404)
			fmt.Fprint(w, `{"code":"not_found","message":"lookup failed"}`)
			return
		}
		fmt.Fprintf(w, `{"image":"ghcr.io/acme/%s@sha256:%s","tag":%q}`, lookup.App, strings.Repeat("a", 64), lookup.Tag)
	case r.Method == "POST" && rest == "deploys":
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad body", 400)
			return
		}
		f.deploys = append(f.deploys, body)
		app, _ := body["app"].(string)
		switch {
		case body["dry_run"] == true && f.plans[app] != "":
			fmt.Fprint(w, f.plans[app])
		case body["dry_run"] == true && f.mixedPlans && app == "api":
			fmt.Fprintf(w, `{"app":"api","image":"ghcr.io/acme/api@sha256:%s","tag":"v1","roles":["web"]}`, strings.Repeat("a", 64))
		case body["dry_run"] == true && f.mixedPlans:
			blockers := ""
			if f.blockCompose {
				blockers = `,"blockers":["blocked"]`
			}
			fmt.Fprintf(w, `{"app":"zstack","image":"","roles":[],"compose":{"project":"demo-dev-zstack","services":[{"name":"side","action":"keep"}],"volumes":[]%s}}`, blockers)
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
		if answer := f.applied[app]; answer != "" {
			fmt.Fprint(w, answer)
			return
		}
		if f.undeployed[app] {
			w.WriteHeader(404)
			fmt.Fprint(w, `{"code":"not_deployed","message":"app is not deployed"}`)
			return
		}
		repo := map[string]string{"api": "ghcr.io/acme/api", "postgres": "docker.io/library/postgres", "redis": "docker.io/library/redis"}[app]
		fmt.Fprintf(w, `{"id":4,"digest":"%s@sha256:%s","tag":"v1.0.2"}`, repo, strings.Repeat("a", 64))
	case r.Method == "GET" && strings.HasPrefix(rest, "operations/") && strings.HasSuffix(rest, "/events"):
		app := strings.TrimSuffix(strings.TrimPrefix(rest, "operations/op-"), "/events")
		state := "succeeded"
		if f.failOp[app] {
			state = "failed"
		}
		fmt.Fprintf(w, "id: 1\nevent: accepted\ndata: {}\n\nid: 2\nevent: %s\ndata: {}\n\n", state)
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
		repo := map[string]string{"api": "ghcr.io/acme/api", "postgres": "docker.io/library/postgres", "redis": "docker.io/library/redis"}[d["app"].(string)]
		if want := fmt.Sprintf("%s@sha256:%s", repo, strings.Repeat("a", 64)); d["keep_digest"] != want {
			t.Errorf("%v: keep_digest %v, want %s", d["app"], d["keep_digest"], want)
		}
	}
	if len(agent.reads) != 3 {
		t.Fatalf("the preflight asked %v", agent.reads)
	}
	if agent.policyReads != 0 {
		t.Fatalf("ordinary kept digests asked for policy %d times", agent.policyReads)
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

func TestDeployAllProtectedTagRefusesNoninteractiveBeforeMutation(t *testing.T) {
	agent := &fakeDeployAgent{protected: true}
	dir, cfg := deployAllFixture(t, allAppsManifest, agent)
	code, _, _ := deployAllCommand(t, dir, cfg, "--version", "api=v1", "--json")
	if code == 0 || len(agent.deploys) != 0 {
		t.Fatalf("exit %d, requests %v", code, agent.apps())
	}
}

func TestDeployAllProtectedMixedPlansRejectNestedBlocker(t *testing.T) {
	agent := &fakeDeployAgent{protected: true, mixedPlans: true, blockCompose: true}
	manifest := "project: demo\nnetwork: demo\napps:\n  api:\n    kind: image\n    image: ghcr.io/acme/api\n    roles:\n      web: {}\n  zstack:\n    kind: compose\n    file: compose.yml\n    depends_on: [api]\n"
	dir, cfg := deployAllFixture(t, manifest, agent)
	if err := os.WriteFile(filepath.Join(dir, "compose.yml"), []byte("services: {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	code, _, errs := deployAllCommand(t, dir, cfg, "--version", "api=v1", "--dry-run", "--json")
	if code == 0 || !strings.Contains(errs, "blocker") {
		t.Fatalf("exit %d stderr %q", code, errs)
	}
	for _, d := range agent.deploys {
		if d["dry_run"] != true {
			t.Fatalf("mutation: %v", d)
		}
	}
}

func TestDeployAllProtectedMixedDryRunUsesValidatedPlans(t *testing.T) {
	agent := &fakeDeployAgent{protected: true, mixedPlans: true}
	manifest := "project: demo\nnetwork: demo\nprotected: true\napps:\n  api:\n    kind: image\n    image: ghcr.io/acme/api\n    roles:\n      web: {}\n  zstack:\n    kind: compose\n    file: compose.yml\n    depends_on: [api]\n"
	dir, cfg := deployAllFixture(t, manifest, agent)
	if err := os.WriteFile(filepath.Join(dir, "compose.yml"), []byte("services: {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	code, out, errs := deployAllCommand(t, dir, cfg, "--version", "api=v1", "--dry-run", "--json")
	if code != 0 || !json.Valid([]byte(out)) || len(agent.deploys) != 2 {
		t.Fatalf("exit %d, stdout %q, stderr %q, requests %v", code, out, errs, agent.apps())
	}
	if agent.deploys[0]["version_tag"] != "v1" || agent.deploys[0]["version"] != "ghcr.io/acme/api@sha256:"+strings.Repeat("a", 64) {
		t.Fatalf("selection %v", agent.deploys[0])
	}
}

func TestDeployAllLegacyGuardKeepsOriginalTagBody(t *testing.T) {
	agent := &fakeDeployAgent{legacy: true, mixedPlans: true}
	manifest := "project: demo\nnetwork: demo\napps:\n  api:\n    kind: image\n    image: ghcr.io/acme/api\n    roles:\n      web: {}\n"
	dir, cfg := deployAllFixture(t, manifest, agent)
	code, out, errs := deployAllCommand(t, dir, cfg, "--version", "api=v1")
	if code != 0 || len(agent.deploys) != 2 {
		t.Fatalf("exit %d stdout %q stderr %q requests %v", code, out, errs, agent.apps())
	}
	if agent.deploys[0]["dry_run"] != true || agent.deploys[1]["dry_run"] == true || agent.deploys[1]["version"] != "v1" || agent.deploys[1]["version_tag"] != nil {
		t.Fatalf("requests %v", agent.deploys)
	}
}

func TestDeployAllLookupFailureLastSendsNoPlansOrMutations(t *testing.T) {
	agent := &fakeDeployAgent{protected: true, mixedPlans: true, lookupFail: "zlast"}
	manifest := "project: demo\nnetwork: demo\napps:\n  api:\n    kind: image\n    image: ghcr.io/acme/api\n    roles:\n      web: {}\n  zlast:\n    kind: image\n    image: ghcr.io/acme/zlast\n    depends_on: [api]\n    roles:\n      web: {}\n"
	dir, cfg := deployAllFixture(t, manifest, agent)
	code, _, _ := deployAllCommand(t, dir, cfg, "--version", "api=v1", "--version", "zlast=v1", "--dry-run", "--json")
	if code == 0 || strings.Join(agent.lookups, ",") != "api,zlast" || len(agent.deploys) != 0 {
		t.Fatalf("exit %d lookups %v deploys %v", code, agent.lookups, agent.apps())
	}
}

func TestDeployAllLegacyGuardIncludesLaterComposeChanges(t *testing.T) {
	agent := &fakeDeployAgent{legacy: true, mixedPlans: true}
	manifest := "project: demo\nnetwork: demo\napps:\n  api:\n    kind: image\n    image: ghcr.io/acme/api\n    roles:\n      web: {}\n  zstack:\n    kind: compose\n    file: compose.yml\n    depends_on: [api]\n"
	dir, cfg := deployAllFixture(t, manifest, agent)
	file := filepath.Join(dir, "compose.yml")
	if err := os.WriteFile(file, []byte("services: {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com", "GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s: %v", args, out, err)
		}
	}
	git("init", "-q")
	git("config", "core.autocrlf", "false")
	git("add", "berthy.yml", "compose.yml")
	git("commit", "-qm", "fixture")
	if err := os.WriteFile(file, []byte("services: {}\n# changed\n"), 0600); err != nil {
		t.Fatal(err)
	}
	code, out, errs := deployAllCommand(t, dir, cfg, "--version", "api=v1")
	if code != 0 || len(agent.deploys) != 3 {
		t.Fatalf("exit %d stdout %q stderr %q requests %v", code, out, errs, agent.apps())
	}
	if agent.deploys[0]["dry_run"] != true || agent.deploys[0]["modified"] != true || agent.deploys[1]["modified"] == true {
		t.Fatalf("guard and actual: %v", agent.deploys)
	}
}

func TestBatchReviewBareDigestPreview(t *testing.T) {
	digest := "sha256:" + strings.Repeat("b", 64)
	agent := &fakeDeployAgent{protected: true, mixedPlans: true, plans: map[string]string{"zlast": fmt.Sprintf(`{"app":"zlast","image":"ghcr.io/acme/zlast@%s","roles":["web"]}`, digest)}}
	manifest := "project: demo\nnetwork: demo\napps:\n  api:\n    kind: image\n    image: ghcr.io/acme/api\n    roles:\n      web: {}\n  zlast:\n    kind: image\n    image: ghcr.io/acme/zlast\n    depends_on: [api]\n    roles:\n      web: {}\n"
	dir, cfg := deployAllFixture(t, manifest, agent)
	code, out, errs := deployAllCommand(t, dir, cfg, "--version", "api=v1", "--version", "zlast="+digest, "--dry-run", "--json")
	if code != 0 || !json.Valid([]byte(out)) || len(agent.deploys) != 2 || agent.deploys[1]["version"] != digest {
		t.Fatalf("exit %d stdout %s stderr %s requests %v", code, out, errs, agent.deploys)
	}
}

func TestBatchReviewInvalidLaterKeptRelease(t *testing.T) {
	manifest := "project: demo\nnetwork: demo\napps:\n  api:\n    kind: image\n    image: ghcr.io/acme/api\n    roles:\n      web: {}\n  zlast:\n    kind: image\n    image: ghcr.io/acme/zlast\n    depends_on: [api]\n    roles:\n      web: {}\n"
	for _, answer := range []string{`{"id":1,"digest":"bad","tag":"v1"}`, fmt.Sprintf(`{"id":1,"digest":"ghcr.io/other/zlast@sha256:%s","tag":"v1"}`, strings.Repeat("a", 64)), fmt.Sprintf(`{"id":1,"digest":"ghcr.io/acme/zlast@sha256:%s","tag":"bad tag"}`, strings.Repeat("a", 64))} {
		agent := &fakeDeployAgent{applied: map[string]string{"zlast": answer}}
		dir, cfg := deployAllFixture(t, manifest, agent)
		code, _, errs := deployAllCommand(t, dir, cfg, "--version", "api=sha256:"+strings.Repeat("a", 64))
		if code == 0 || len(agent.deploys) != 0 {
			t.Fatalf("answer %s: exit %d stderr %s requests %v", answer, code, errs, agent.apps())
		}
	}
}

func TestBatchReviewPreviewSelectionContainingToken(t *testing.T) {
	for _, asJSON := range []bool{false, true} {
		agent := &fakeDeployAgent{protected: true, mixedPlans: true}
		manifest := "project: demo\nnetwork: demo\napps:\n  api:\n    kind: image\n    image: ghcr.io/acme/api\n    roles:\n      web: {}\n"
		dir, cfg := deployAllFixture(t, manifest, agent)
		t.Setenv("BERTHY_TEST_TOKEN", "v1")
		args := []string{"--version", "api=v1", "--dry-run"}
		if asJSON {
			args = append(args, "--json")
		}
		code, out, errs := deployAllCommand(t, dir, cfg, args...)
		if code == 0 || strings.Contains(out, "v1") || strings.Contains(out, "[redacted]") || !strings.Contains(errs, "cannot safely display") {
			t.Fatalf("json %t exit %d stdout %q stderr %q", asJSON, code, out, errs)
		}
	}
}

func TestBatchReviewComposeDecisionShapes(t *testing.T) {
	m := &manifest.Manifest{Project: "demo", Apps: map[string]manifest.App{"z": {Kind: "compose"}}}
	req := deploy.Request{App: "z"}
	for _, extra := range []string{`"tag":null,`, `"remove":{},`, `"exec":{},`} {
		data := `{"app":"z","image":"",` + extra + `"roles":[],"compose":{"project":"demo-dev-z","services":[{"name":"side","action":"keep"}],"volumes":[]}}`
		if batchPlan([]byte(data), m, "dev", req, "", "", "", false) == nil {
			t.Errorf("accepted %s", data)
		}
	}
	for _, field := range []string{`"services":[{"name":"a","action":"keep"},{"name":"a","action":"create"}],"volumes":[]`, `"services":[{"name":"side","action":"keep"}],"volumes":[{"name":"v","action":"keep"},{"name":"v","action":"create"}]`} {
		data := `{"app":"z","image":"","roles":[],"compose":{"project":"demo-dev-z",` + field + `}}`
		if batchPlan([]byte(data), m, "dev", req, "", "", "", false) == nil {
			t.Errorf("accepted %s", data)
		}
	}
}

func TestBatchReviewRejectsTaggedDigestUnderStickyProtection(t *testing.T) {
	agent := &fakeDeployAgent{protected: true, mixedPlans: true}
	manifest := "project: demo\nnetwork: demo\napps:\n  api:\n    kind: image\n    image: ghcr.io/acme/api\n    roles:\n      web: {}\n  zlast:\n    kind: image\n    image: ghcr.io/acme/zlast\n    depends_on: [api]\n    roles:\n      web: {}\n"
	dir, cfg := deployAllFixture(t, manifest, agent)
	code, _, errs := deployAllCommand(t, dir, cfg, "--version", "api=v1", "--version", "zlast=ghcr.io/acme/zlast:old@sha256:"+strings.Repeat("a", 64), "--dry-run")
	if code == 0 || len(agent.lookups) != 0 || len(agent.deploys) != 0 {
		t.Fatalf("exit %d stderr %s lookups %v deploys %v", code, errs, agent.lookups, agent.apps())
	}
}

func TestBatchReviewKeepsStoredDigestAndPreviewsCanonicalIdentity(t *testing.T) {
	hash := strings.Repeat("b", 64)
	stored := "ghcr.io/acme/zlast@sha256:" + hash
	agent := &fakeDeployAgent{protected: true, mixedPlans: true, applied: map[string]string{"zlast": fmt.Sprintf(`{"id":1,"digest":%q,"tag":"old"}`, stored)}, plans: map[string]string{"zlast": fmt.Sprintf(`{"app":"zlast","image":"ghcr.io/acme/zlast@sha256:%s","tag":"old","roles":["web"]}`, hash)}}
	manifest := "project: demo\nnetwork: demo\napps:\n  api:\n    kind: image\n    image: ghcr.io/acme/api\n    roles:\n      web: {}\n  zlast:\n    kind: image\n    image: ghcr.io/acme/zlast\n    depends_on: [api]\n    roles:\n      web: {}\n"
	dir, cfg := deployAllFixture(t, manifest, agent)
	code, out, errs := deployAllCommand(t, dir, cfg, "--version", "api=v1", "--dry-run", "--json")
	if code != 0 || !json.Valid([]byte(out)) || len(agent.deploys) != 2 || agent.deploys[1]["keep_digest"] != stored {
		t.Fatalf("exit %d stdout %s stderr %s requests %v", code, out, errs, agent.deploys)
	}
}

func TestBatchReviewDecodedPreviewRedaction(t *testing.T) {
	for _, jsonOutput := range []bool{false, true} {
		agent := &fakeDeployAgent{protected: true, mixedPlans: true, plans: map[string]string{"api": fmt.Sprintf(`{"app":"api","image":"ghcr.io/acme/api@sha256:%s","tag":"v1","roles":["web"],"notes":["\u0074est-token\u001b[31m"]}`, strings.Repeat("a", 64))}}
		manifest := "project: demo\nnetwork: demo\napps:\n  api:\n    kind: image\n    image: ghcr.io/acme/api\n    roles:\n      web: {}\n"
		dir, cfg := deployAllFixture(t, manifest, agent)
		args := []string{"--version", "api=v1", "--dry-run"}
		if jsonOutput {
			args = append(args, "--json")
		}
		code, out, errs := deployAllCommand(t, dir, cfg, args...)
		if code != 0 || strings.Contains(out, "test-token") || strings.ContainsRune(out, '\x1b') || !strings.Contains(out, "[redacted]") || (jsonOutput && !json.Valid([]byte(out))) {
			t.Fatalf("json %t exit %d stdout %q stderr %q", jsonOutput, code, out, errs)
		}
	}
}

func TestBatchReviewRedactsTokenInAggregateApp(t *testing.T) {
	for _, asJSON := range []bool{false, true} {
		agent := &fakeDeployAgent{protected: true, mixedPlans: true}
		manifest := "project: demo\nnetwork: demo\napps:\n  api:\n    kind: image\n    image: ghcr.io/acme/api\n    roles:\n      web: {}\n"
		dir, cfg := deployAllFixture(t, manifest, agent)
		t.Setenv("BERTHY_TEST_TOKEN", "api")
		args := []string{"--version", "api=v1", "--dry-run"}
		if asJSON {
			args = append(args, "--json")
		}
		code, out, errs := deployAllCommand(t, dir, cfg, args...)
		if code == 0 || strings.Contains(out, "api") || strings.Contains(errs, "api") {
			t.Fatalf("json %t exit %d stdout %q stderr %q", asJSON, code, out, errs)
		}
	}
}

func TestBatchCheckoutSwitchBeforePreparation(t *testing.T) {
	dir, first := checkoutRepo(t, map[string]string{"berthy.yml": allAppsManifest, "berthy.dev.yml": "apps:\n  api:\n    env:\n      plain:\n        MARKER: before\n"})
	if err := os.WriteFile(filepath.Join(dir, "berthy.dev.yml"), []byte("apps:\n  api:\n    env:\n      plain:\n        MARKER: after\n"), 0600); err != nil {
		t.Fatal(err)
	}
	gitRunIn(t, dir, "add", "berthy.dev.yml")
	gitRunIn(t, dir, "commit", "-q", "-m", "second")
	second := gitRunIn(t, dir, "rev-parse", "HEAD")
	gitRunIn(t, dir, "checkout", "-q", "--detach", first)
	agent := &fakeDeployAgent{}
	_, cfg := deployAllFixture(t, allAppsManifest, agent)
	old := loadDeployManifest
	loadDeployManifest = func(dir, env string) (*manifest.Loaded, error) {
		m, err := old(dir, env)
		if err == nil {
			gitRunIn(t, dir, "checkout", "-q", "--detach", second)
		}
		return m, err
	}
	defer func() { loadDeployManifest = old }()
	code, _, errs := deployAllCommand(t, dir, cfg, "--version", "api=v1", "--version", "postgres=v1", "--version", "redis=v1")
	if code == 0 || agent.calls != 0 || !strings.Contains(errs, "checkout changed") {
		t.Fatalf("exit %d calls %d stderr %q", code, agent.calls, errs)
	}
}

func TestBatchComposeDecisionCannotBeRedacted(t *testing.T) {
	manifestText := "project: demo\nnetwork: demo\napps:\n  api:\n    kind: image\n    image: ghcr.io/acme/api\n    roles:\n      web: {}\n  zstack:\n    kind: compose\n    file: compose.yml\n    project_name: test-token-project\n    depends_on: [api]\n"
	for _, tc := range []struct{ project, services, volumes, token string }{
		{"test-token-project", `[{"name":"side","action":"keep"}]`, `[]`, "test-token"},
		{"test-token-project", `[{"name":"test-token-svc","action":"keep"}]`, `[]`, "test-token-svc"},
		{"test-token-project", `[{"name":"side","action":"keep"}]`, `[{"name":"test-token-vol","action":"create"}]`, "test-token-vol"},
		{"test-token-project", `[{"name":"svc","action":"keep"}]`, `[]`, "keep"},
	} {
		plan := fmt.Sprintf(`{"app":"zstack","image":"","roles":[],"compose":{"project":%q,"services":%s,"volumes":%s}}`, tc.project, tc.services, tc.volumes)
		agent := &fakeDeployAgent{protected: true, mixedPlans: true, plans: map[string]string{"zstack": plan}}
		dir, cfg := deployAllFixture(t, manifestText, agent)
		if err := os.WriteFile(filepath.Join(dir, "compose.yml"), []byte("services: {}\n"), 0600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("BERTHY_TEST_TOKEN", tc.token)
		code, out, errs := deployAllCommand(t, dir, cfg, "--version", "api=v1", "--dry-run", "--json")
		if code == 0 || strings.Contains(out, "Proceed with this deploy?") || strings.Contains(out, tc.token) || !strings.Contains(errs, "cannot safely display") {
			t.Fatalf("token %q exit %d stdout %q stderr %q", tc.token, code, out, errs)
		}
		for _, d := range agent.deploys {
			if d["dry_run"] != true {
				t.Fatalf("mutation: %v", d)
			}
		}
	}
}

func TestBatchComposeControlTextAndMetadata(t *testing.T) {
	manifestText := "project: demo\nnetwork: demo\napps:\n  api:\n    kind: image\n    image: ghcr.io/acme/api\n    roles:\n      web: {}\n  zstack:\n    kind: compose\n    file: compose.yml\n    depends_on: [api]\n"
	for _, tc := range []struct {
		name, plan string
		asJSON, ok bool
	}{
		{"control in decision text", `{"app":"zstack","image":"","roles":[],"compose":{"project":"demo-dev-zstack","services":[{"name":"svc\u001b[31m","action":"keep"}],"volumes":[]}}`, false, false},
		{"escaped decision json", `{"app":"zstack","image":"","roles":[],"compose":{"project":"demo-dev-zstack","services":[{"name":"svc\u001b[31m","action":"keep"}],"volumes":[]}}`, true, true},
		{"control metadata text", `{"app":"zstack","image":"","roles":[],"notes":["before\u001b[31mafter"],"compose":{"project":"demo-dev-zstack","services":[{"name":"side","action":"keep"}],"volumes":[]}}`, false, true},
		{"control metadata json", `{"app":"zstack","image":"","roles":[],"notes":["before\u001b[31mafter"],"compose":{"project":"demo-dev-zstack","services":[{"name":"side","action":"keep"}],"volumes":[]}}`, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agent := &fakeDeployAgent{protected: true, mixedPlans: true, plans: map[string]string{"zstack": tc.plan}}
			dir, cfg := deployAllFixture(t, manifestText, agent)
			if err := os.WriteFile(filepath.Join(dir, "compose.yml"), []byte("services: {}\n"), 0600); err != nil {
				t.Fatal(err)
			}
			args := []string{"--version", "api=v1", "--dry-run"}
			if tc.asJSON {
				args = append(args, "--json")
			}
			code, out, errs := deployAllCommand(t, dir, cfg, args...)
			if (code == 0) != tc.ok || strings.ContainsRune(out, '\x1b') || strings.ContainsRune(errs, '\x1b') || (tc.asJSON && !json.Valid([]byte(out))) {
				t.Fatalf("exit %d stdout %q stderr %q", code, out, errs)
			}
			for _, d := range agent.deploys {
				if d["dry_run"] != true {
					t.Fatalf("mutation: %v", d)
				}
			}
		})
	}
}

func TestBatchR1ProtectedTaggedKeepRefusesBeforeFirstDeploy(t *testing.T) {
	hash := strings.Repeat("b", 64)
	agent := &fakeDeployAgent{protected: true, applied: map[string]string{"zlast": fmt.Sprintf(`{"id":1,"digest":"ghcr.io/acme/zlast:old@sha256:%s","tag":"old"}`, hash)}}
	manifestText := "project: demo\nnetwork: demo\nprotected: true\napps:\n  api:\n    kind: image\n    image: ghcr.io/acme/api\n    roles:\n      web: {}\n  zlast:\n    kind: image\n    image: ghcr.io/acme/zlast\n    depends_on: [api]\n    roles:\n      web: {}\n"
	dir, cfg := deployAllFixture(t, manifestText, agent)
	code, _, errs := deployAllCommand(t, dir, cfg, "--version", "api=sha256:"+strings.Repeat("a", 64))
	if code == 0 || len(agent.deploys) != 0 {
		t.Fatalf("exit %d stderr %q deploys %v", code, errs, agent.apps())
	}
}

func TestBatchR1LegacyGuardFailureAggregatesDryRun(t *testing.T) {
	agent := &fakeDeployAgent{legacy: true, failPlan: map[string]bool{"api": true}}
	manifestText := "project: demo\nnetwork: demo\napps:\n  api:\n    kind: image\n    image: ghcr.io/acme/api\n    roles:\n      web: {}\n  zstack:\n    kind: compose\n    file: compose.yml\n    depends_on: [api]\n"
	dir, cfg := deployAllFixture(t, manifestText, agent)
	if err := os.WriteFile(filepath.Join(dir, "compose.yml"), []byte("services: {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	code, out, errs := deployAllCommand(t, dir, cfg, "--version", "api=missing", "--dry-run", "--json")
	var doc struct {
		Apps []struct {
			App  string `json:"app"`
			Exit *int   `json:"exit"`
		} `json:"apps"`
	}
	if code != 1 || json.Unmarshal([]byte(out), &doc) != nil || len(doc.Apps) != 2 || doc.Apps[0].Exit == nil || *doc.Apps[0].Exit != 1 || doc.Apps[1].Exit == nil || *doc.Apps[1].Exit != 0 || strings.Join(agent.apps(), ",") != "api,zstack" {
		t.Fatalf("exit %d stdout %q stderr %q requests %v", code, out, errs, agent.apps())
	}
}

func TestBatchR1ComposeRejectsEmptyAndInvalidDiff(t *testing.T) {
	m := &manifest.Manifest{Project: "demo", Apps: map[string]manifest.App{"z": {Kind: "compose"}}}
	req := deploy.Request{App: "z"}
	for _, data := range []string{
		`{"app":"z","image":"","roles":[],"compose":{"project":"demo-dev-z","services":[],"volumes":[]}}`,
		`{"app":"z","image":"","roles":[],"diff":null,"compose":{"project":"demo-dev-z","services":[{"name":"side","action":"keep"}],"volumes":[]}}`,
		`{"app":"z","image":"","roles":[],"diff":{"same":"yes"},"compose":{"project":"demo-dev-z","services":[{"name":"side","action":"keep"}],"volumes":[]}}`,
	} {
		if batchPlan([]byte(data), m, "dev", req, "", "", "", false) == nil {
			t.Errorf("accepted %s", data)
		}
	}
	for _, diff := range []string{"", `,"diff":{"new":true}`, `,"diff":{"same":true}`, `,"diff":{"changes":[{"area":"compose","change":"create side"}]}`} {
		data := `{"app":"z","image":"","roles":[]` + diff + `,"compose":{"project":"demo-dev-z","services":[{"name":"side","action":"create"}],"volumes":[]}}`
		if err := batchPlan([]byte(data), m, "dev", req, "", "", "", false); err != nil {
			t.Errorf("rejected %s: %v", data, err)
		}
	}
}

func TestBatchR1LegacyTaggedKeepGuard(t *testing.T) {
	digest := "ghcr.io/acme/zlast:old@sha256:" + strings.Repeat("b", 64)
	applied := fmt.Sprintf(`{"id":1,"digest":%q,"tag":"old"}`, digest)
	plan := fmt.Sprintf(`{"app":"zlast","image":%q,"tag":"old","roles":["web"]}`, digest)
	manifestText := "project: demo\nnetwork: demo\napps:\n  api:\n    kind: image\n    image: ghcr.io/acme/api\n    roles:\n      web: {}\n  zlast:\n    kind: image\n    image: ghcr.io/acme/zlast\n    depends_on: [api]\n    roles:\n      web: {}\n"
	for _, tc := range []struct {
		name                    string
		legacy, protected, fail bool
		want                    int
		order                   string
	}{
		{"legacy allowed", true, false, false, 0, "zlast,api,zlast"},
		{"legacy guard refused", true, false, true, 1, "zlast"},
		{"capable unprotected", false, false, false, 0, "api,zlast"},
		{"capable protected", false, true, false, 1, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agent := &fakeDeployAgent{legacy: tc.legacy, protected: tc.protected, applied: map[string]string{"zlast": applied}}
			if tc.fail {
				agent.failPlan = map[string]bool{"zlast": true}
			} else {
				agent.plans = map[string]string{"zlast": plan}
			}
			dir, cfg := deployAllFixture(t, manifestText, agent)
			code, _, errs := deployAllCommand(t, dir, cfg, "--version", "api=sha256:"+strings.Repeat("a", 64))
			if code != tc.want || strings.Join(agent.apps(), ",") != tc.order {
				t.Fatalf("exit %d stderr %q requests %v", code, errs, agent.apps())
			}
			for _, d := range agent.deploys {
				if d["app"] == "zlast" && d["keep_digest"] != digest {
					t.Fatalf("changed kept digest: %v", d)
				}
			}
		})
	}
}

func TestBatchR1LegacyFailedGuardStopsMutation(t *testing.T) {
	agent := &fakeDeployAgent{legacy: true, failPlan: map[string]bool{"api": true}}
	manifestText := "project: demo\nnetwork: demo\napps:\n  api:\n    kind: image\n    image: ghcr.io/acme/api\n    roles:\n      web: {}\n"
	dir, cfg := deployAllFixture(t, manifestText, agent)
	code, _, _ := deployAllCommand(t, dir, cfg, "--version", "api=missing")
	if code == 0 || len(agent.deploys) != 1 || agent.deploys[0]["dry_run"] != true {
		t.Fatalf("exit %d requests %v", code, agent.deploys)
	}
}

func TestBatchR1TaggedKeepRejectsUnknownPolicyAndMalformedLegacyPlan(t *testing.T) {
	digest := "ghcr.io/acme/zlast:old@sha256:" + strings.Repeat("b", 64)
	applied := fmt.Sprintf(`{"id":1,"digest":%q,"tag":"old"}`, digest)
	manifestText := "project: demo\nnetwork: demo\napps:\n  api:\n    kind: image\n    image: ghcr.io/acme/api\n    roles:\n      web: {}\n  zlast:\n    kind: image\n    image: ghcr.io/acme/zlast\n    depends_on: [api]\n    roles:\n      web: {}\n"
	for _, tc := range []struct {
		name   string
		agent  *fakeDeployAgent
		guards int
	}{
		{"unknown policy", &fakeDeployAgent{policyReply: `{"project":"demo","environment":"dev","protected":false,"effective_protected":null}`, applied: map[string]string{"zlast": applied}}, 0},
		{"malformed guard", &fakeDeployAgent{legacy: true, plans: map[string]string{"zlast": `{"app":"zlast","image":"wrong","roles":["web"]}`}, applied: map[string]string{"zlast": applied}}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, cfg := deployAllFixture(t, manifestText, tc.agent)
			code, _, _ := deployAllCommand(t, dir, cfg, "--version", "api=sha256:"+strings.Repeat("a", 64))
			if code == 0 || len(tc.agent.deploys) != tc.guards {
				t.Fatalf("exit %d requests %v", code, tc.agent.deploys)
			}
			for _, d := range tc.agent.deploys {
				if d["dry_run"] != true {
					t.Fatalf("mutation: %v", d)
				}
			}
		})
	}
}

func TestBatchR2AdoptedTaggedKeepUsesPlannerTag(t *testing.T) {
	digest := "ghcr.io/acme/zlast:old@sha256:" + strings.Repeat("b", 64)
	manifestText := "project: demo\nnetwork: demo\napps:\n  api:\n    kind: image\n    image: ghcr.io/acme/api\n    roles:\n      web: {}\n  zlast:\n    kind: image\n    image: ghcr.io/acme/zlast\n    depends_on: [api]\n    roles:\n      web: {}\n"
	for _, tc := range []struct {
		name, appliedTag, planTag string
		wantCode, requests        int
	}{
		{"derived", "", "old", 0, 3},
		{"wrong derived", "", "wrong", 1, 1},
		{"missing derived", "", "", 1, 1},
		{"separate wins", "new", "new", 0, 3},
		{"separate mismatch", "new", "old", 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tagField := ""
			if tc.appliedTag != "" {
				tagField = `,"tag":` + strconv.Quote(tc.appliedTag)
			}
			applied := fmt.Sprintf(`{"id":1,"digest":%q%s}`, digest, tagField)
			planTag := ""
			if tc.planTag != "" {
				planTag = `,"tag":` + strconv.Quote(tc.planTag)
			}
			plan := fmt.Sprintf(`{"app":"zlast","image":%q,"roles":["web"]%s}`, digest, planTag)
			agent := &fakeDeployAgent{legacy: true, applied: map[string]string{"zlast": applied}, plans: map[string]string{"zlast": plan}}
			dir, cfg := deployAllFixture(t, manifestText, agent)
			code, _, errs := deployAllCommand(t, dir, cfg, "--version", "api=sha256:"+strings.Repeat("a", 64))
			if code != tc.wantCode || len(agent.deploys) != tc.requests {
				t.Fatalf("exit %d stderr %q requests %v", code, errs, agent.deploys)
			}
			if tc.wantCode == 0 && (agent.deploys[0]["dry_run"] != true || agent.deploys[2]["dry_run"] == true || agent.deploys[0]["request_id"] != agent.deploys[2]["request_id"]) {
				t.Fatalf("guard and admission differ: %v", agent.deploys)
			}
			for _, d := range agent.deploys {
				if d["app"] == "zlast" && d["keep_digest"] != digest {
					t.Fatalf("changed kept digest: %v", d)
				}
			}
		})
	}
}

func TestBatchR2ComposeRejectsRemoveOnly(t *testing.T) {
	manifestText := "project: demo\nnetwork: demo\napps:\n  api:\n    kind: image\n    image: ghcr.io/acme/api\n    roles:\n      web: {}\n  zstack:\n    kind: compose\n    file: compose.yml\n    depends_on: [api]\n"
	plan := `{"app":"zstack","image":"","roles":[],"compose":{"project":"demo-dev-zstack","services":[{"name":"orphan","action":"remove"}],"volumes":[]}}`
	agent := &fakeDeployAgent{protected: true, mixedPlans: true, plans: map[string]string{"zstack": plan}}
	dir, cfg := deployAllFixture(t, manifestText, agent)
	if err := os.WriteFile(filepath.Join(dir, "compose.yml"), []byte("services: {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	code, out, errs := deployAllCommand(t, dir, cfg, "--version", "api=v1", "--dry-run", "--json")
	if code == 0 || !json.Valid([]byte(out)) || !strings.Contains(errs, "invalid agent deploy plan") {
		t.Fatalf("exit %d stdout %q stderr %q", code, out, errs)
	}
	for _, d := range agent.deploys {
		if d["dry_run"] != true {
			t.Fatalf("mutation: %v", d)
		}
	}
}

func TestBatchR2ComposeAllowsDesiredAndRemovedServices(t *testing.T) {
	m := &manifest.Manifest{Project: "demo", Apps: map[string]manifest.App{"z": {Kind: "compose"}}}
	for _, action := range []string{"create", "recreate", "keep"} {
		data := fmt.Sprintf(`{"app":"z","image":"","roles":[],"diff":{"same":true},"compose":{"project":"demo-dev-z","services":[{"name":"side","action":%q},{"name":"orphan","action":"remove"}],"volumes":[]}}`, action)
		if err := batchPlan([]byte(data), m, "dev", deploy.Request{App: "z"}, "", "", "", false); err != nil {
			t.Errorf("action %s: %v", action, err)
		}
	}
}
