package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"berthy/internal/state"
)

const apRunDigest = "ghcr.io/acme/api@sha256:" + "a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1"

// seedRunApp records a succeeded deploy of app api that runs apRunDigest, so a run has a release to take its image from.
func seedRunApp(t *testing.T, s *state.Store) {
	t.Helper()
	ctx := context.Background()
	op, _, err := s.Admit(ctx, state.Admission{RequestID: "seed", ActorID: "a", Kind: "deploy", App: "api", PublicRequest: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	release := &state.Release{App: "api", Digest: apRunDigest, Tag: "v1", Manifest: json.RawMessage(`{"project":"demo","network":"overlay","apps":{"api":{"kind":"image","image":"ghcr.io/acme/api","roles":{"web":{}}}}}`)}
	if err = s.Finish(ctx, op.ID, state.Outcome{State: "succeeded", Release: release}); err != nil {
		t.Fatal(err)
	}
}

func TestRunNeedsAnAdministratorToken(t *testing.T) {
	f := newApprovalFixture(t, false, nil)
	seedRunApp(t, f.s)
	body := `{"app":"api","command":["true"],"dry_run":true}`
	if w := f.call("POST", "/run", apDeployToken, body); w.Code != http.StatusForbidden {
		t.Fatalf("a deploy token: %d %s", w.Code, w.Body.String())
	}
	if w := f.call("GET", "/run", apAdminToken, ""); w.Code == http.StatusOK {
		t.Fatalf("run answered a GET: %d", w.Code)
	}
	w := f.call("POST", "/run", apAdminToken, body)
	var plan struct {
		App, Image string
		Roles      []string
		Command    []string
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &plan) != nil || plan.Image != apRunDigest || strings.Join(plan.Roles, ",") != "web" || strings.Join(plan.Command, " ") != "true" {
		t.Fatalf("dry run: %d %s", w.Code, w.Body.String())
	}
	f.noOperation()
}

func TestRunRefusalsAreSentToTheClient(t *testing.T) {
	f := newApprovalFixture(t, false, nil)
	w := f.call("POST", "/run", apAdminToken, `{"app":"api","command":["true"],"request_id":"r1"}`)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "app is not deployed") {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	f.noOperation()
}

func TestRunOutputIsServedForARunOperation(t *testing.T) {
	f := newApprovalFixture(t, false, nil)
	ctx := context.Background()
	op, _, err := f.s.Admit(ctx, state.Admission{RequestID: "r", ActorID: "a", Kind: "run", App: "api", PublicRequest: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err = f.s.RecordIntent(ctx, op.ID, state.Intent{Ordinal: 30101, Kind: "exec", Desired: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if err = f.s.MarkStep(ctx, op.ID, 30101, "finished", json.RawMessage(`{"output":"done\n","exit_code":0}`)); err != nil {
		t.Fatal(err)
	}
	w := f.call("GET", "/operations/"+op.ID+"/output", apAdminToken, "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"output":"done\n"`) {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if w = f.call("GET", "/operations/"+op.ID+"/output", apReadToken, ""); w.Code != http.StatusForbidden {
		t.Fatalf("a read token saw a run's output: %d", w.Code)
	}
}

func TestAgentRunOnAProtectedEnvironmentIsHeldWithTheCommandAndTheImage(t *testing.T) {
	f := newApprovalFixture(t, true, nil)
	seedRunApp(t, f.s)
	body := `{"app":"api","role":"web","command":["rake","db:seed"],"request_id":"r1"}`
	h := f.hold("POST", "/run", apAgentAdmin, body)
	f.noOperation()
	var plan struct {
		Image   string
		Roles   []string
		Command []string
	}
	if err := json.Unmarshal(h.Plan, &plan); err != nil || plan.Image != apRunDigest || strings.Join(plan.Roles, ",") != "web" || strings.Join(plan.Command, " ") != "rake db:seed" {
		t.Fatalf("plan %s: %v", h.Plan, err)
	}
	a, err := f.s.Approval(context.Background(), h.ID)
	if err != nil || a.Summary != "run api/web" {
		t.Fatalf("approval %+v: %v", a, err)
	}
	// A deploy token of an agent cannot ask for it at all.
	if w := f.call("POST", "/run", apAgentDeploy, body); w.Code != http.StatusForbidden {
		t.Fatalf("an agent deploy token: %d %s", w.Code, w.Body.String())
	}
}
