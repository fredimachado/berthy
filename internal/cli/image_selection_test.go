package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"berthy/internal/deploy"
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
		if code == 0 || out != "" || strings.Join(a.calls, ",") != "agent" {
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
