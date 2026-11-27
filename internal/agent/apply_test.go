package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"berthy/internal/deploy"
	"berthy/internal/manifest"
)

// applyBody is the body of an apply of the manifest in JSON.
func applyBody(t *testing.T, manifestJSON string, dry bool) string {
	t.Helper()
	m, err := manifest.DecodeJSON([]byte(manifestJSON))
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(deploy.ApplyRequest{Manifest: *m, Commit: "abc123", DryRun: dry})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestARepeatedUserMapKeyIsRefusedOnARouteMainDoesNotGuard(t *testing.T) {
	f := newApprovalFixture(t, false, nil)
	body := `{"manifest":{"project":"demo","network":"overlay","apps":{"api":{"kind":"image","image":"ghcr.io/acme/api","roles":{"web":{}},"env":{"plain":{"A":"1","A":"2"}}}}},"commit":"c","dry_run":true}`
	for _, tc := range []struct{ path, body string }{{"/apply", body}, {"/restart", `{"app":"api","app":"api","request_id":"r","dry_run":true}`}} {
		w := f.call("POST", tc.path, apAdminToken, tc.body)
		if w.Code != 400 || strings.TrimSpace(w.Body.String()) != `{"code":"bad_request","message":"invalid request"}` {
			t.Errorf("%s: %d %s", tc.path, w.Code, w.Body.String())
		}
	}
	good := strings.Replace(body, `"A":"1","A":"2"`, `"A":"1","a":"2"`, 1)
	if w := f.call("POST", "/apply", apAdminToken, good); w.Code != 200 {
		t.Fatalf("distinct keys: %d %s", w.Code, w.Body.String())
	}
}

func applyChanges(t *testing.T, f *approvalFixture, token, body string) []string {
	t.Helper()
	w := f.call("POST", "/apply", token, body)
	var got deploy.ApplyResult
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &got) != nil {
		t.Fatalf("apply: %d %s", w.Code, w.Body.String())
	}
	return got.Changes
}

func TestApplyChangesAProbeWithoutADeploy(t *testing.T) {
	f := newApprovalFixture(t, false, nil)
	ctx := context.Background()
	recordRelease(t, f.s, "first", twoAppManifest("* * * * *", "0 * * * *", "1"))
	releasesBefore, _ := f.s.History(ctx, "")
	w := NewMonitoring(monitoringConfig(""), nil, nil, f.ctrl, func(string) {})
	if checkNames(ctx, w)["probe:site"] {
		t.Fatal("the probe exists before it was applied")
	}
	withProbe := strings.Replace(twoAppManifest("* * * * *", "0 * * * *", "1"), `"alerts"`, `"probes":{"site":{"http":"http://127.0.0.1:8080/"}},"alerts"`, 1)
	if got := f.call("POST", "/apply", apDeployToken, applyBody(t, withProbe, true)); got.Code != http.StatusOK || !strings.Contains(got.Body.String(), "probes: + site") {
		t.Fatalf("dry run: %d %s", got.Code, got.Body.String())
	}
	if checkNames(ctx, w)["probe:site"] {
		t.Fatal("a dry run applied the probe")
	}
	changes := applyChanges(t, f, apDeployToken, applyBody(t, withProbe, false))
	if len(changes) != 1 || changes[0] != "probes: + site" {
		t.Fatalf("changes: %v", changes)
	}
	if !checkNames(ctx, w)["probe:site"] {
		t.Fatal("the probe is not monitored after the apply")
	}
	releasesAfter, _ := f.s.History(ctx, "")
	if len(releasesAfter) != len(releasesBefore) {
		t.Fatalf("an apply recorded a release: %d -> %d", len(releasesBefore), len(releasesAfter))
	}
	st, ok, err := f.s.Settings(ctx)
	if err != nil || !ok || st.Commit != "abc123" || st.OperationID != "" {
		t.Fatalf("settings: %+v %v %v", st, ok, err)
	}
	// The same manifest again changes nothing.
	if again := applyChanges(t, f, apDeployToken, applyBody(t, withProbe, false)); len(again) != 0 {
		t.Fatalf("a repeat apply: %v", again)
	}
	if audit := f.audit(); audit[len(audit)-1] != "ci apply succeeded" {
		t.Fatalf("audit: %v", audit)
	}
	// An app's own part is not applied: a job of an app changes only with a deploy of it.
	moved := strings.Replace(withProbe, `"* * * * *"`, `"3 * * * *"`, 1)
	if got := applyChanges(t, f, apDeployToken, applyBody(t, moved, false)); len(got) != 0 {
		t.Fatalf("an app's job counted as a setting: %v", got)
	}
	m, _ := runningManifest(ctx, f.s)
	if m.Jobs["tick"].Schedule != "* * * * *" {
		t.Fatalf("apply changed an app's job: %q", m.Jobs["tick"].Schedule)
	}
}

func TestApplyIsRefusedWhileTheLockIsHeld(t *testing.T) {
	f := newApprovalFixture(t, false, nil)
	recordRelease(t, f.s, "first", twoAppManifest("* * * * *", "0 * * * *", "1"))
	if w := f.call("POST", "/lock/acquire", apDeployToken, `{"message":"maintenance"}`); w.Code != http.StatusOK {
		t.Fatalf("lock: %d %s", w.Code, w.Body.String())
	}
	before := f.audit()
	body := applyBody(t, twoAppManifest("* * * * *", "0 * * * *", "2"), false)
	if w := f.call("POST", "/apply", apDeployToken, body); w.Code != http.StatusConflict {
		t.Fatalf("apply under a lock: %d %s", w.Code, w.Body.String())
	}
	if len(f.audit()) != len(before) {
		t.Fatalf("a refused apply was audited: %v", f.audit())
	}
	// A dry run only reads, so it answers under a lock.
	if w := f.call("POST", "/apply", apDeployToken, applyBody(t, twoAppManifest("* * * * *", "0 * * * *", "2"), true)); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "alerts: changed") {
		t.Fatalf("dry run under a lock: %d %s", w.Code, w.Body.String())
	}
}

func TestApplyOfAnotherProjectIsRefused(t *testing.T) {
	f := newApprovalFixture(t, false, nil)
	recordRelease(t, f.s, "first", twoAppManifest("* * * * *", "0 * * * *", "1"))
	other := strings.Replace(twoAppManifest("* * * * *", "0 * * * *", "1"), `"project":"demo"`, `"project":"other"`, 1)
	for _, dry := range []bool{true, false} {
		w := f.call("POST", "/apply", apDeployToken, applyBody(t, other, dry))
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "environment lab runs project demo; this manifest is for project other") {
			t.Fatalf("dry %v: %d %s", dry, w.Code, w.Body.String())
		}
	}
}

func TestApplyNeedsTheDeployScopeAndAnAgentsApplyIsHeldOnAProtectedEnvironment(t *testing.T) {
	f := newApprovalFixture(t, true, nil)
	recordRelease(t, f.s, "first", twoAppManifest("* * * * *", "0 * * * *", "1"))
	body := applyBody(t, twoAppManifest("* * * * *", "0 * * * *", "2"), false)
	if w := f.call("POST", "/apply", apReadToken, body); w.Code != http.StatusForbidden {
		t.Fatalf("a read token: %d", w.Code)
	}
	h := f.hold("POST", "/apply", apAgentDeploy, body)
	if !strings.Contains(string(h.Plan), "alerts: changed") {
		t.Fatalf("the plan of the held apply: %s", h.Plan)
	}
	st, _, _ := f.s.Settings(context.Background())
	m, _ := manifest.DecodeSettings(st.Manifest)
	if m.Alerts.Telegram.ChatID != "1" {
		t.Fatal("a held apply changed the settings")
	}
	list := f.call("GET", "/approvals", apAdminToken, "")
	if !strings.Contains(list.Body.String(), "apply the environment settings") {
		t.Fatalf("approvals: %s", list.Body.String())
	}
	if w := f.call("POST", "/approvals/"+h.ID+"/approve", apAdminToken, ""); w.Code != http.StatusOK {
		t.Fatalf("approve: %d %s", w.Code, w.Body.String())
	}
	st, _, _ = f.s.Settings(context.Background())
	m, _ = manifest.DecodeSettings(st.Manifest)
	if m.Alerts.Telegram.ChatID != "2" {
		t.Fatalf("the approved apply did not change the settings: %+v", m.Alerts)
	}
}
