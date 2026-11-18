package agent

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"berthy/internal/deploy"
	"berthy/internal/manifest"
)

// deployBodyWith is a deploy request for app api that adds the plain variable FEATURE_X to the manifest of the fixture.
func deployBodyWith(t *testing.T, dry bool) string {
	t.Helper()
	source := strings.Replace(apManifestSource, `"roles":{"web":{}}`, `"env":{"plain":{"FEATURE_X":"1"}},"roles":{"web":{}}`, 1)
	m, err := manifest.DecodeJSON([]byte(source))
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(deploy.Request{App: "api", Version: apDigest, Manifest: *m, RequestID: "req-1", DryRun: dry})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

type diffOfPlan struct {
	Diff *deploy.Diff `json:"diff"`
}

func TestHeldDeployCarriesTheDiffAndItIsTheOneAFreshPlanGives(t *testing.T) {
	f := newApprovalFixture(t, true, nil)
	recordAppRelease(t, f.s, "one", "api", apManifestSource)
	h := f.hold("POST", "/deploys", apAgentDeploy, deployBodyWith(t, false))
	var plan diffOfPlan
	if err := json.Unmarshal(h.Plan, &plan); err != nil || plan.Diff == nil {
		t.Fatalf("plan %s: %v", h.Plan, err)
	}
	var env []string
	for _, c := range plan.Diff.Changes {
		if c.Area == "env" {
			env = append(env, c.Change)
		}
	}
	if len(env) != 1 || env[0] != "+ FEATURE_X (plain)" {
		t.Fatalf("diff %+v", plan.Diff)
	}
	// What a person reads in the list is what a new plan says, so approving it runs what they read.
	dry := f.call("POST", "/deploys", apAgentDeploy, deployBodyWith(t, true))
	if dry.Code != http.StatusOK || !sameJSON(t, h.Plan, dry.Body.Bytes()) {
		t.Fatalf("dry run %d:\n%s\n%s", dry.Code, h.Plan, dry.Body.String())
	}
	list := f.call("GET", "/approvals?status=pending", apDeployToken, "")
	if !strings.Contains(list.Body.String(), "FEATURE_X") {
		t.Fatalf("the list does not show the diff: %s", list.Body.String())
	}
}

func TestApproveRefusesAHeldDeployWhenTheAppRunsAnotherReleaseNow(t *testing.T) {
	f := newApprovalFixture(t, true, nil)
	recordAppRelease(t, f.s, "one", "api", apManifestSource)
	h := f.hold("POST", "/deploys", apAgentDeploy, deployBodyWith(t, false))
	// Another deploy landed: the difference the person read is no longer the difference the deploy would make.
	recordAppRelease(t, f.s, "two", "api", strings.Replace(apManifestSource, `"roles":{"web":{}}`, `"env":{"plain":{"FEATURE_X":"1"}},"roles":{"web":{}}`, 1))
	w := f.call("POST", "/approvals/"+h.ID+"/approve", apAdminToken, "")
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "the plan changed since it was requested") {
		t.Fatalf("approve: %d %s", w.Code, w.Body.String())
	}
	f.noOperation()
}

func TestHistoryDiffRouteComparesTwoReleasesAndRefusesAnotherApp(t *testing.T) {
	f := newApprovalFixture(t, false, nil)
	recordAppRelease(t, f.s, "one", "api", apManifestSource)
	recordAppRelease(t, f.s, "two", "api", strings.Replace(apManifestSource, `"roles":{"web":{}}`, `"env":{"plain":{"FEATURE_X":"1"}},"roles":{"web":{}}`, 1))
	recordAppRelease(t, f.s, "three", "other", `{"project":"demo","network":"overlay","apps":{"other":{"kind":"image","image":"ghcr.io/acme/other","roles":{"web":{}}}}}`)
	rows, err := f.s.History(t.Context(), "api")
	if err != nil || len(rows) != 2 {
		t.Fatalf("history %v %d", err, len(rows))
	}
	other, _ := f.s.History(t.Context(), "other")
	newer, older := rows[0].ID, rows[1].ID

	get := func(path, token string) (int, deploy.Diff) {
		w := f.call("GET", path, token, "")
		var d deploy.Diff
		_ = json.Unmarshal(w.Body.Bytes(), &d)
		return w.Code, d
	}
	code, d := get("/history/api/diff?release="+itoa(newer), apReadToken)
	if code != http.StatusOK || len(d.Changes) != 1 || d.Changes[0] != (deploy.DiffChange{Area: "env", Change: "+ FEATURE_X (plain)"}) {
		t.Fatalf("diff %d %+v", code, d)
	}
	code, d = get("/history/api/diff?release="+itoa(newer)+"&base="+itoa(older), apReadToken)
	if code != http.StatusOK || len(d.Changes) != 1 {
		t.Fatalf("diff %d %+v", code, d)
	}
	code, d = get("/history/api/diff?release="+itoa(older), apReadToken)
	if code != http.StatusOK || !d.New {
		t.Fatalf("diff %d %+v", code, d)
	}
	for _, path := range []string{
		"/history/api/diff?release=" + itoa(other[0].ID),
		"/history/api/diff?release=" + itoa(newer) + "&base=" + itoa(other[0].ID),
		"/history/api/diff",
		"/history/api/diff?release=x",
		"/history/api/diff?release=0",
		"/history/api/diff?release=" + itoa(newer) + "&base=-1",
	} {
		if w := f.call("GET", path, apReadToken, ""); w.Code != http.StatusBadRequest {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
	}
	if w := f.call("GET", "/history/api/diff?release="+itoa(newer), "", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("no token: %d", w.Code)
	}
}

func itoa(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}
