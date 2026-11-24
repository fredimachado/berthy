package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"berthy/internal/deploy"
	"berthy/internal/manifest"
)

func TestProtectedTagDryRunPinsBeforePreview(t *testing.T) {
	digest := "docker.io/library/alpine@sha256:" + strings.Repeat("a", 64)
	var lookup, plans int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/agent"):
			fmt.Fprint(w, `{"project":"demo","environment":"dev","protected":true,"effective_protected":true}`)
		case strings.HasSuffix(r.URL.Path, "/images/resolve"):
			lookup++
			fmt.Fprintf(w, `{"image":%q,"tag":"stable"}`, digest)
		case strings.HasSuffix(r.URL.Path, "/deploys"):
			plans++
			var req deploy.Request
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Error(err)
				return
			}
			if !req.DryRun || req.Version != digest || req.VersionTag != "stable" || !req.Manifest.Protected {
				t.Errorf("unexpected preview request: %+v", req)
			}
			fmt.Fprintf(w, `{"app":"api","image":%q,"tag":"stable","roles":["web"]}`, digest)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	cfg := ingressCLIConfig(t, server.URL)
	dir := writeAliasDir(t, "protected: true\n"+versionsManifest)
	var out, errs bytes.Buffer
	code := Run([]string{"deploy", "api", "-e", "dev", "--config", cfg, "--manifest-dir", dir, "--version", "stable", "--dry-run", "--json"}, &out, &errs)
	if code != 0 || lookup != 1 || plans != 1 || !strings.Contains(out.String(), digest) {
		t.Fatalf("exit %d, lookup %d, plans %d, output %q, error %q", code, lookup, plans, out.String(), errs.String())
	}
}

// answerCapablePolicy supplies the scoped capability to fixtures testing unrelated deploy behavior.
func answerCapablePolicy(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodGet || !strings.HasSuffix(r.URL.Path, "/agent") {
		return false
	}
	parts := strings.Split(r.URL.Path, "/")
	if len(parts) < 7 {
		return false
	}
	fmt.Fprintf(w, `{"project":%q,"environment":%q,"protected":false,"effective_protected":false}`, parts[3], parts[5])
	return true
}

type selectionAgent struct {
	policy, resolved, plan string
	status                 int
	calls                  []string
	requests               []map[string]json.RawMessage
}

func (a *selectionAgent) serve(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
	a.calls = append(a.calls, path)
	switch path {
	case "agent":
		fmt.Fprint(w, a.policy)
	case "resolve":
		if a.status != 0 {
			w.WriteHeader(a.status)
		}
		fmt.Fprint(w, a.resolved)
	case "deploys":
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid", 400)
			return
		}
		a.requests = append(a.requests, body)
		fmt.Fprint(w, a.plan)
	default:
		http.NotFound(w, r)
	}
}

func runSelection(t *testing.T, a *selectionAgent, manifestText string, extra ...string) (int, string, string) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(a.serve))
	defer server.Close()
	cfg := ingressCLIConfig(t, server.URL)
	dir := writeAliasDir(t, manifestText)
	var out, errs bytes.Buffer
	args := append([]string{"deploy", "api", "-e", "dev", "--config", cfg, "--manifest-dir", dir}, extra...)
	code := Run(args, &out, &errs)
	return code, out.String(), errs.String()
}

func TestSingleTagPolicyAndLegacyGuard(t *testing.T) {
	image := "docker.io/library/alpine@sha256:" + strings.Repeat("a", 64)
	goodPlan := fmt.Sprintf(`{"app":"api","image":%q,"tag":"stable","roles":["web"]}`, image)
	capable := `{"project":"demo","environment":"dev","protected":false,"effective_protected":false}`
	legacy := `{"project":"demo","environment":"dev","protected":false}`
	for _, tc := range []struct {
		name, policy, plan  string
		dry                 bool
		wantCode, wantPosts int
	}{
		{"capable", capable, `{}`, false, 0, 1},
		{"legacy", legacy, goodPlan, false, 0, 2},
		{"legacy dry", legacy, goodPlan, true, 0, 1},
		{"legacy wrong app", legacy, strings.Replace(goodPlan, `"api"`, `"other"`, 1), false, 1, 1},
		{"legacy wrong repository", legacy, strings.Replace(goodPlan, "library/alpine", "library/busybox", 1), false, 1, 1},
		{"legacy blocked", legacy, strings.TrimSuffix(goodPlan, "}") + `,"blockers":["refused"]}`, false, 1, 1},
		{"legacy protected", `{"project":"demo","environment":"dev","protected":true}`, goodPlan, false, 1, 0},
		{"null capability", `{"project":"demo","environment":"dev","protected":false,"effective_protected":null}`, goodPlan, false, 1, 0},
		{"wrong scope", `{"project":"other","environment":"dev","protected":false,"effective_protected":false}`, goodPlan, false, 1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &selectionAgent{policy: tc.policy, plan: tc.plan}
			args := []string{"--version", "stable"}
			if tc.dry {
				args = append(args, "--dry-run", "--json")
			}
			code, _, errs := runSelection(t, a, versionsManifest, args...)
			if code != tc.wantCode || len(a.requests) != tc.wantPosts {
				t.Fatalf("exit %d, requests %d, stderr %q", code, len(a.requests), errs)
			}
			for _, req := range a.requests {
				if string(req["version"]) != `"stable"` || req["version_tag"] != nil || req["expected_applied"] != nil {
					t.Fatalf("legacy original request changed: %v", req)
				}
			}
			if strings.Contains(strings.Join(a.calls, ","), "resolve") {
				t.Fatal("unexpected image lookup")
			}
		})
	}
}

func TestProtectedTagRejectsInvalidLookupAndPlan(t *testing.T) {
	image := "docker.io/library/alpine@sha256:" + strings.Repeat("a", 64)
	lookup := fmt.Sprintf(`{"image":%q,"tag":"stable"}`, image)
	plan := fmt.Sprintf(`{"app":"api","image":%q,"tag":"stable","roles":["web"]}`, image)
	policy := `{"project":"demo","environment":"dev","protected":true,"effective_protected":true}`
	for _, tc := range []struct {
		name, lookup, plan string
		status, posts      int
	}{
		{"wrong tag", strings.Replace(lookup, "stable", "other", 1), plan, 0, 0},
		{"wrong repo", strings.Replace(lookup, "library/alpine", "library/busybox", 1), plan, 0, 0},
		{"duplicate image", strings.TrimSuffix(lookup, "}") + `,"image":"other"}`, plan, 0, 0},
		{"wrong plan app", lookup, strings.Replace(plan, `"api"`, `"other"`, 1), 0, 1},
		{"wrong plan digest", lookup, strings.Replace(plan, strings.Repeat("a", 64), strings.Repeat("b", 64), 1), 0, 1},
		{"blocked plan", lookup, strings.TrimSuffix(plan, "}") + `,"blockers":["stop"]}`, 0, 1},
		{"malformed plan", lookup, `{}`, 0, 1},
		{"registry refusal", `{"message":"test-token"}`, plan, 500, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &selectionAgent{policy: policy, resolved: tc.lookup, plan: tc.plan, status: tc.status}
			code, out, errs := runSelection(t, a, "protected: true\n"+versionsManifest, "--version", "stable", "--dry-run", "--json")
			if code == 0 || out != "" || len(a.requests) != tc.posts || strings.Contains(errs, "test-token") {
				t.Fatalf("exit %d output %q error %q posts %d", code, out, errs, len(a.requests))
			}
		})
	}
}

func TestProtectedTagNoninteractiveStopsBeforeLookup(t *testing.T) {
	for _, args := range [][]string{{"--version", "stable"}, {"--version", "stable", "--json"}} {
		a := &selectionAgent{policy: `{"project":"demo","environment":"dev","protected":true,"effective_protected":true}`}
		code, out, _ := runSelection(t, a, "protected: true\n"+versionsManifest, args...)
		if code == 0 || out != "" || len(a.calls) != 0 {
			t.Fatalf("exit %d output %q calls %v", code, out, a.calls)
		}
	}
}

func TestDigestDeploySkipsPolicyLookup(t *testing.T) {
	a := &selectionAgent{plan: `{}`}
	code, _, errs := runSelection(t, a, "protected: true\n"+versionsManifest, "--version", "sha256:"+strings.Repeat("a", 64), "--dry-run")
	if code != 0 || strings.Join(a.calls, ",") != "deploys" {
		t.Fatalf("exit %d calls %v error %q", code, a.calls, errs)
	}
}

type shortPreviewWriter struct{}

func (shortPreviewWriter) Write(p []byte) (int, error) { return len(p) / 2, nil }

func TestProtectedTagRefusesPartialPreviewWrite(t *testing.T) {
	image := "docker.io/library/alpine@sha256:" + strings.Repeat("a", 64)
	a := &selectionAgent{policy: `{"project":"demo","environment":"dev","protected":true,"effective_protected":true}`, resolved: fmt.Sprintf(`{"image":%q,"tag":"stable"}`, image), plan: fmt.Sprintf(`{"app":"api","image":%q,"tag":"stable","roles":["web"]}`, image)}
	server := httptest.NewServer(http.HandlerFunc(a.serve))
	defer server.Close()
	cfg := ingressCLIConfig(t, server.URL)
	dir := writeAliasDir(t, "protected: true\n"+versionsManifest)
	var errs bytes.Buffer
	code := Run([]string{"deploy", "api", "-e", "dev", "--config", cfg, "--manifest-dir", dir, "--version", "stable", "--dry-run"}, shortPreviewWriter{}, &errs)
	if code == 0 || len(a.requests) != 1 || !strings.Contains(errs.String(), "short write") {
		t.Fatalf("exit %d posts %d error %q", code, len(a.requests), errs.String())
	}
}

func TestProtectedDirtyTagStopsBeforeAnyAgentRequest(t *testing.T) {
	a := &selectionAgent{}
	server := httptest.NewServer(http.HandlerFunc(a.serve))
	defer server.Close()
	cfg := ingressCLIConfig(t, server.URL)
	dir, _ := checkoutRepo(t, map[string]string{"berthy.yml": "protected: true\n" + versionsManifest})
	editFile(t, dir, "berthy.yml")
	var out, errs bytes.Buffer
	code := Run([]string{"deploy", "api", "-e", "dev", "--config", cfg, "--manifest-dir", dir, "--version", "stable", "--dry-run"}, &out, &errs)
	if code == 0 || len(a.calls) != 0 || !strings.Contains(errs.String(), "commit them first") {
		t.Fatalf("exit %d calls %v stderr %q", code, a.calls, errs.String())
	}
}

func TestProtectedTagWithDigestStillRefused(t *testing.T) {
	a := &selectionAgent{}
	code, _, _ := runSelection(t, a, "protected: true\n"+versionsManifest, "--version", "alpine:stable@sha256:"+strings.Repeat("a", 64), "--dry-run")
	if code == 0 || len(a.calls) != 0 {
		t.Fatalf("exit %d calls %v", code, a.calls)
	}
}

func TestProtectedTagReleaseVersionPreflightRedactsToken(t *testing.T) {
	stampCLI(t, "v1.0.0")
	var lookup atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/agent") {
			fmt.Fprint(w, `{"project":"demo","environment":"dev","protected":true,"effective_protected":true,"version":"v0.8.2"}`)
			return
		}
		lookup.Add(1)
	}))
	defer server.Close()
	cfg := ingressCLIConfig(t, server.URL)
	t.Setenv("BERTHY_TEST_TOKEN", "v0.8.2")
	dir := writeAliasDir(t, "protected: true\nmin_version: 0.9.0\n"+versionsManifest)
	var out, errs bytes.Buffer
	code := Run([]string{"deploy", "api", "-e", "dev", "--config", cfg, "--manifest-dir", dir, "--version", "stable", "--dry-run"}, &out, &errs)
	if code == 0 || lookup.Load() != 0 || out.Len() != 0 || strings.Contains(errs.String(), "v0.8.2") || !strings.Contains(errs.String(), "[redacted]") {
		t.Fatal("minimum-version refusal was missing, leaked the token, or reached lookup")
	}
}

func TestProtectedTagPreviewRedactsDecodedResponse(t *testing.T) {
	image := "docker.io/library/alpine@sha256:" + strings.Repeat("a", 64)
	a := &selectionAgent{
		policy:   `{"project":"demo","environment":"dev","protected":true,"effective_protected":true}`,
		resolved: fmt.Sprintf(`{"image":%q,"tag":"stable"}`, image),
		plan:     fmt.Sprintf(`{"app":"api","image":%q,"tag":"stable","roles":["web"],"notes":["start\u001b[2Kend","\u0074est-token"],"x\u001b[31m":{"\u0074est-token":"details"}}`, image),
	}
	for _, asJSON := range []bool{false, true} {
		args := []string{"--version", "stable", "--dry-run"}
		if asJSON {
			args = append(args, "--json")
		}
		code, out, errs := runSelection(t, a, "protected: true\n"+versionsManifest, args...)
		if code != 0 {
			t.Fatalf("json %t: exit %d: %s", asJSON, code, errs)
		}
		if strings.Contains(out, "test-token") || strings.ContainsRune(out, '\x1b') {
			t.Errorf("json %t: output contains bearer or terminal control", asJSON)
		}
		if !strings.Contains(out, "[redacted]") || !strings.Contains(out, image) || !strings.Contains(out, "details") {
			t.Errorf("json %t: missing expected preview content", asJSON)
		}
		if asJSON {
			var plan struct {
				Notes []string `json:"notes"`
			}
			if err := json.Unmarshal([]byte(out), &plan); err != nil || len(plan.Notes) != 2 || plan.Notes[0] != "start\x1b[2Kend" || plan.Notes[1] != "[redacted]" {
				t.Error("machine preview did not preserve decoded data and redact its credential")
			}
		}
	}
}

func TestDeploySubmissionRefusalRedactsDecodedBearer(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if answerCapablePolicy(w, r) {
			return
		}
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"message":"denied \u0074est-token"}`)
	}))
	defer server.Close()
	cfg := ingressCLIConfig(t, server.URL)
	dir := writeAliasDir(t, versionsManifest)
	var out, errs bytes.Buffer
	code := Run([]string{"deploy", "api", "-e", "dev", "--config", cfg, "--manifest-dir", dir, "--version", "stable"}, &out, &errs)
	if code != 1 || out.Len() != 0 || strings.Contains(errs.String(), "test-token") || !strings.Contains(errs.String(), "[redacted]") || !strings.Contains(errs.String(), "HTTP 403") {
		t.Errorf("submission refusal is not safely redacted: exit %d", code)
	}
}

func TestProtectedTagPreviewRefusesRedactionCollisions(t *testing.T) {
	image := "docker.io/library/alpine@sha256:" + strings.Repeat("a", 64)
	for _, tc := range []struct{ name, token, extra string }{
		{"selection", "stable", ""},
		{"decision key", "roles", ""},
		{"redacted key collision", "test-token", `,"extra":{"[redacted]":"one","\u0074est-token":"two"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &selectionAgent{policy: `{"project":"demo","environment":"dev","protected":true,"effective_protected":true}`, resolved: fmt.Sprintf(`{"image":%q,"tag":"stable"}`, image), plan: fmt.Sprintf(`{"app":"api","image":%q,"tag":"stable","roles":["web"]%s}`, image, tc.extra)}
			server := httptest.NewServer(http.HandlerFunc(a.serve))
			defer server.Close()
			cfg := ingressCLIConfig(t, server.URL)
			t.Setenv("BERTHY_TEST_TOKEN", tc.token)
			dir := writeAliasDir(t, "protected: true\n"+versionsManifest)
			for _, extra := range [][]string{{}, {"--json"}} {
				var out, errs bytes.Buffer
				args := append([]string{"deploy", "api", "-e", "dev", "--config", cfg, "--manifest-dir", dir, "--version", "stable", "--dry-run"}, extra...)
				code := Run(args, &out, &errs)
				if code == 0 || out.Len() != 0 || strings.Contains(errs.String(), tc.token) {
					t.Errorf("preview collision did not fail safely")
				}
			}
		})
	}
}

func TestImagePlanAllowsOnlyOmittedOrValidEmptyTag(t *testing.T) {
	m := &manifest.Manifest{Apps: map[string]manifest.App{"api": {Kind: "image", Image: "alpine", Roles: map[string]manifest.Role{"web": {}}}}}
	image := "docker.io/library/alpine@sha256:" + strings.Repeat("a", 64)
	req := deploy.Request{App: "api"}
	for _, tc := range []struct {
		name, field string
		valid       bool
	}{
		{"omitted", "", true}, {"empty", `,"tag":""`, true}, {"null", `,"tag":null`, false}, {"number", `,"tag":1`, false}, {"nonempty", `,"tag":"stable"`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := fmt.Sprintf(`{"app":"api","image":%q,"roles":["web"]%s}`, image, tc.field)
			if got := imagePlan([]byte(data), m, req, image, ""); (got == nil) != tc.valid {
				t.Errorf("plan accepted = %t", got == nil)
			}
		})
	}
}

func TestSingleTagRejectsMalformedPlanBeforeMutation(t *testing.T) {
	image := "docker.io/library/alpine@sha256:" + strings.Repeat("a", 64)
	policy := `{"project":"demo","environment":"dev","protected":false}`
	for _, tc := range []struct{ name, roles, extra string }{
		{"null role", `[null]`, ""}, {"empty role", `[""]`, ""}, {"missing role", `[]`, ""},
		{"duplicate role", `["web","web"]`, ""}, {"extra role", `["web","other"]`, ""},
		{"null diff", `["web"]`, `,"diff":null`}, {"unknown diff", `["web"]`, `,"diff":{"other":true}`},
		{"extra diff field", `["web"]`, `,"diff":{"same":true,"other":"hidden"}`},
		{"extra change field", `["web"]`, `,"diff":{"changes":[{"area":"image","change":"x","other":"hidden"}]}`},
		{"invalid diff item", `["web"]`, `,"diff":{"changes":[null]}`},
		{"contradictory diff", `["web"]`, `,"diff":{"new":true,"same":true}`},
		{"wrong plan kind", `["web"]`, `,"remove":{"app":"api"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan := fmt.Sprintf(`{"app":"api","image":%q,"tag":"stable","roles":%s%s}`, image, tc.roles, tc.extra)
			a := &selectionAgent{policy: policy, plan: plan}
			code, _, _ := runSelection(t, a, versionsManifest, "--version", "stable")
			if code == 0 || len(a.requests) != 1 || string(a.requests[0]["dry_run"]) != "true" {
				t.Errorf("legacy guard accepted malformed plan or reached mutation: exit %d, requests %d", code, len(a.requests))
			}
			a = &selectionAgent{policy: `{"project":"demo","environment":"dev","protected":true,"effective_protected":true}`, resolved: fmt.Sprintf(`{"image":%q,"tag":"stable"}`, image), plan: plan}
			code, _, _ = runSelection(t, a, "protected: true\n"+versionsManifest, "--version", "stable", "--dry-run")
			if code == 0 || len(a.requests) != 1 || string(a.requests[0]["dry_run"]) != "true" {
				t.Errorf("protected preview accepted malformed plan: exit %d, requests %d", code, len(a.requests))
			}
		})
	}
}

func TestProtectedTagJSONEscapesC1(t *testing.T) {
	image := "docker.io/library/alpine@sha256:" + strings.Repeat("a", 64)
	a := &selectionAgent{policy: `{"project":"demo","environment":"dev","protected":true,"effective_protected":true}`, resolved: fmt.Sprintf(`{"image":%q,"tag":"stable"}`, image), plan: fmt.Sprintf(`{"app":"api","image":%q,"tag":"stable","roles":["web"],"notes":["before\u009bafter"]}`, image)}
	code, out, errs := runSelection(t, a, "protected: true\n"+versionsManifest, "--version", "stable", "--dry-run", "--json")
	var got struct {
		Notes []string `json:"notes"`
	}
	if code != 0 || strings.Contains(out, "\u009b") || json.Unmarshal([]byte(out), &got) != nil || len(got.Notes) != 1 || got.Notes[0] != "before\u009bafter" {
		t.Errorf("C1 serialization incorrect: exit %d, err %q", code, errs)
	}
}

func TestImagePlanAcceptsPlannerShapes(t *testing.T) {
	m := &manifest.Manifest{Apps: map[string]manifest.App{"api": {Kind: "image", Image: "alpine", Roles: map[string]manifest.Role{"web": {}, "worker": {}}}}}
	image := "docker.io/library/alpine@sha256:" + strings.Repeat("a", 64)
	for _, diff := range []string{"", `,"diff":{"new":true}`, `,"diff":{"same":true}`, `,"diff":{"changes":[{"area":"image","change":"old -> new"}]}`} {
		data := fmt.Sprintf(`{"app":"api","image":%q,"tag":"stable","roles":["worker","web"]%s}`, image, diff)
		if err := imagePlan([]byte(data), m, deploy.Request{App: "api"}, image, "stable"); err != nil {
			t.Errorf("valid planner shape %q rejected: %v", diff, err)
		}
	}
}

func TestImagePlanAcceptsAdoptionAndIngressShapes(t *testing.T) {
	m := &manifest.Manifest{Apps: map[string]manifest.App{"api": {Kind: "image", Image: "alpine", Roles: map[string]manifest.Role{"web": {}}}}}
	image := "docker.io/library/alpine@sha256:" + strings.Repeat("a", 64)
	for _, extra := range []string{
		`,"ingress":{"tunnel":"t","dns":false,"changes":[]}`,
		`,"ingress":{"tunnel":"t","tunnel_id":"id","dns":true,"changes":[{"hostname":"one.example","app":"api","role":"web","route":"add","dns":"adopt","target":"http://api-web:80"},{"hostname":"old.example","app":"api","role":"former","route":"release","dns":"release","previous":"http://old:80"}]}`,
	} {
		data := fmt.Sprintf(`{"app":"api","image":%q,"tag":"stable","roles":["web"]%s}`, image, extra)
		if err := imagePlan([]byte(data), m, deploy.Request{App: "api"}, image, "stable"); err != nil {
			t.Errorf("valid ingress rejected: %v", err)
		}
	}
	data := fmt.Sprintf(`{"app":"api","image":%q,"roles":["web"],"changes":["web: replicas 2 -> 1"]}`, image)
	if err := imagePlan([]byte(data), m, deploy.Request{App: "api", Adopt: true}, image, ""); err != nil {
		t.Errorf("valid adoption rejected: %v", err)
	}
}

func TestProtectedTagRefusesCheckoutSwitchDuringCapture(t *testing.T) {
	dir, aHead := checkoutRepo(t, map[string]string{"berthy.yml": "protected: true\n" + versionsManifest, "berthy.dev.yml": "apps:\n  api:\n    env:\n      plain:\n        MARKER: before\n"})
	if err := os.WriteFile(filepath.Join(dir, "berthy.dev.yml"), []byte("apps:\n  api:\n    env:\n      plain:\n        MARKER: after\n"), 0600); err != nil {
		t.Fatal(err)
	}
	gitRunIn(t, dir, "add", "berthy.dev.yml")
	gitRunIn(t, dir, "commit", "-q", "-m", "second")
	bHead := gitRunIn(t, dir, "rev-parse", "HEAD")
	gitRunIn(t, dir, "checkout", "-q", "--detach", aHead)
	image := "docker.io/library/alpine@sha256:" + strings.Repeat("a", 64)
	a := &selectionAgent{policy: `{"project":"demo","environment":"dev","protected":true,"effective_protected":true}`, resolved: fmt.Sprintf(`{"image":%q,"tag":"stable"}`, image), plan: fmt.Sprintf(`{"app":"api","image":%q,"tag":"stable","roles":["web"]}`, image)}
	server := httptest.NewServer(http.HandlerFunc(a.serve))
	defer server.Close()
	cfg := ingressCLIConfig(t, server.URL)
	old := loadDeployManifest
	loadDeployManifest = func(dir, env string) (*manifest.Manifest, error) {
		m, err := old(dir, env)
		if err == nil {
			gitRunIn(t, dir, "checkout", "-q", "--detach", bHead)
		}
		return m, err
	}
	defer func() { loadDeployManifest = old }()
	var out, errs bytes.Buffer
	code := Run([]string{"deploy", "api", "-e", "dev", "--config", cfg, "--manifest-dir", dir, "--version", "stable", "--dry-run"}, &out, &errs)
	if code == 0 || len(a.calls) != 0 || !strings.Contains(errs.String(), "checkout changed") {
		t.Errorf("checkout switch not refused before remote work: exit %d calls %v stderr %q", code, a.calls, errs.String())
	}
}

func TestSingleTagRejectsMalformedIngressBeforeMutation(t *testing.T) {
	image := "docker.io/library/alpine@sha256:" + strings.Repeat("a", 64)
	for _, tc := range []struct{ name, extra string }{
		{"conflicts without blockers", `,"ingress":{"tunnel":"t","dns":false,"changes":[],"conflicts":["route occupied"]}`},
		{"missing fields", `,"ingress":{}`},
		{"null ingress", `,"ingress":null`},
		{"null changes", `,"ingress":{"tunnel":"t","dns":false,"changes":null}`},
		{"null conflicts", `,"ingress":{"tunnel":"t","dns":false,"changes":[],"conflicts":null}`},
		{"apply-only connector", `,"ingress":{"tunnel":"t","dns":false,"changes":[],"connector":{}}`},
		{"wrong dns type", `,"ingress":{"tunnel":"t","dns":"yes","changes":[]}`},
		{"malformed change", `,"ingress":{"tunnel":"t","dns":false,"changes":[{"hostname":"api.example","route":null}]}`},
		{"unknown route", `,"ingress":{"tunnel":"t","dns":false,"changes":[{"hostname":"api.example","route":"restart"}]}`},
		{"ordinary adoption changes", `,"changes":["adoption-only change"]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan := fmt.Sprintf(`{"app":"api","image":%q,"tag":"stable","roles":["web"]%s}`, image, tc.extra)
			legacy := &selectionAgent{policy: `{"project":"demo","environment":"dev","protected":false}`, plan: plan}
			code, _, _ := runSelection(t, legacy, versionsManifest, "--version", "stable")
			if code == 0 || len(legacy.requests) != 1 || string(legacy.requests[0]["dry_run"]) != "true" {
				t.Errorf("legacy plan accepted or mutation submitted: exit %d posts %d", code, len(legacy.requests))
			}
			protected := &selectionAgent{policy: `{"project":"demo","environment":"dev","protected":true,"effective_protected":true}`, resolved: fmt.Sprintf(`{"image":%q,"tag":"stable"}`, image), plan: plan}
			code, out, _ := runSelection(t, protected, "protected: true\n"+versionsManifest, "--version", "stable", "--dry-run")
			if code == 0 || out != "" || len(protected.requests) != 1 || string(protected.requests[0]["dry_run"]) != "true" {
				t.Errorf("protected plan accepted: exit %d posts %d", code, len(protected.requests))
			}
		})
	}
}

func TestQualifiedSha256HostTagCapturesCheckout(t *testing.T) {
	image := "sha256:5000/acme/api@sha256:" + strings.Repeat("a", 64)
	manifestText := strings.Replace(versionsManifest, "image: alpine", "image: sha256:5000/acme/api", 1)
	dir, head := checkoutRepo(t, map[string]string{"berthy.yml": "protected: true\n" + manifestText})
	a := &selectionAgent{policy: `{"project":"demo","environment":"dev","protected":true,"effective_protected":true}`, resolved: fmt.Sprintf(`{"image":%q,"tag":"stable"}`, image), plan: fmt.Sprintf(`{"app":"api","image":%q,"tag":"stable","roles":["web"]}`, image)}
	server := httptest.NewServer(http.HandlerFunc(a.serve))
	defer server.Close()
	cfg := ingressCLIConfig(t, server.URL)
	var out, errs bytes.Buffer
	code := Run([]string{"deploy", "api", "-e", "dev", "--config", cfg, "--manifest-dir", dir, "--version", "sha256:5000/acme/api:stable", "--dry-run", "--json"}, &out, &errs)
	if code != 0 || len(a.requests) != 1 || string(a.requests[0]["commit"]) != strconv.Quote(head) || !strings.Contains(out.String(), image) {
		t.Errorf("qualified tag did not plan with captured checkout: exit %d posts %d stderr %q", code, len(a.requests), errs.String())
	}
}

func TestBareSHA256Classification(t *testing.T) {
	for _, tc := range []struct {
		value string
		bare  bool
	}{
		{"sha256:" + strings.Repeat("a", 64), true},
		{"sha256:5000/acme/api:stable", false},
		{"sha256:" + strings.Repeat("A", 64), false},
		{"sha256:" + strings.Repeat("a", 63), false},
		{"example/api:stable@sha256:" + strings.Repeat("a", 64), false},
	} {
		if got := bareSHA256(tc.value); got != tc.bare {
			t.Errorf("bareSHA256(%q) = %v", tc.value, got)
		}
	}
}
