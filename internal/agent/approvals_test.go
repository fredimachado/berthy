package agent

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"berthy/internal/deploy"
	"berthy/internal/manifest"
	"berthy/internal/state"
)

// The tokens of the approval tests, by role.
var (
	apReadToken      = "bty.lab." + strings.Repeat("1", 64)
	apDeployToken    = "bty.lab." + strings.Repeat("2", 64)
	apAdminToken     = "bty.lab." + strings.Repeat("3", 64)
	apAgentRead      = "bty.lab." + strings.Repeat("4", 64)
	apAgentDeploy    = "bty.lab." + strings.Repeat("5", 64)
	apAgentAdmin     = "bty.lab." + strings.Repeat("6", 64)
	apAgentAdminTwo  = "bty.lab." + strings.Repeat("7", 64)
	apStartTime      = time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	apDigest         = "sha256:" + strings.Repeat("a", 64)
	apManifestSource = `{"project":"demo","network":"overlay","apps":{"api":{"kind":"image","image":"ghcr.io/acme/api","roles":{"web":{}}}}}`
)

// approvalFixture is an agent for environment lab with a state store, a clock the test moves, and tokens of every scope, some
// of them agent tokens.
type approvalFixture struct {
	t         *testing.T
	s         *state.Store
	dir       string
	h         http.Handler
	now       time.Time
	cfg       Config
	ctrl      *deploy.Controller
	approvals func() []state.Approval
}

func newApprovalFixture(t *testing.T, protected bool, mutate func(*Config)) *approvalFixture {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(dir, "key")
	if err := os.WriteFile(key, make([]byte, 32), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := state.Open(dir, key, "lab")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	f := &approvalFixture{t: t, s: s, dir: dir, now: apStartTime}
	f.cfg = Config{Environment: "lab", Protected: protected, clock: func() time.Time { return f.now }, Tokens: []Token{
		{Name: "reader", Digest: Digest(apReadToken), Scope: "read"},
		{Name: "ci", Digest: Digest(apDeployToken), Scope: "deploy"},
		{Name: "alice", Digest: Digest(apAdminToken), Scope: "admin"},
		{Name: "agent-read", Digest: Digest(apAgentRead), Scope: "read", Agent: true},
		{Name: "claude", Digest: Digest(apAgentDeploy), Scope: "deploy", Agent: true},
		{Name: "claude-admin", Digest: Digest(apAgentAdmin), Scope: "admin", Agent: true},
		{Name: "claude-two", Digest: Digest(apAgentAdminTwo), Scope: "admin", Agent: true},
	}}
	if mutate != nil {
		mutate(&f.cfg)
	}
	f.ctrl = &deploy.Controller{Engine: planEngine{}, Store: s, Environment: "lab", Protected: protected}
	f.h = NewDeploymentHandler(f.cfg, nil, nil, f.ctrl)
	return f
}

func (f *approvalFixture) call(method, path, token, body string) *httptest.ResponseRecorder {
	f.t.Helper()
	r := httptest.NewRequest(method, "/v1/environments/lab"+path, strings.NewReader(body))
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, r)
	return w
}

// held is the answer to a request that an agent token made on a protected environment.
type held struct {
	ID      string          `json:"approval_id"`
	Expires time.Time       `json:"expires"`
	Plan    json.RawMessage `json:"plan"`
}

// hold sends a request that must be held, and returns what the agent was told.
func (f *approvalFixture) hold(method, path, token, body string) held {
	f.t.Helper()
	w := f.call(method, path, token, body)
	var h held
	if w.Code != http.StatusAccepted || w.Header().Get("Berthy-Approval") != "pending" || json.Unmarshal(w.Body.Bytes(), &h) != nil || h.ID == "" {
		f.t.Fatalf("not held: %d %v %s", w.Code, w.Header(), w.Body.String())
	}
	return h
}

func (f *approvalFixture) audit() []string {
	f.t.Helper()
	rows, err := f.s.AuditLog(context.Background(), 100)
	if err != nil {
		f.t.Fatal(err)
	}
	var out []string
	for i := len(rows) - 1; i >= 0; i-- {
		out = append(out, rows[i].ActorName+" "+rows[i].Action+" "+rows[i].Outcome)
	}
	return out
}

func (f *approvalFixture) noOperation() {
	f.t.Helper()
	if _, err := f.s.Active(context.Background()); !errors.Is(err, sql.ErrNoRows) {
		f.t.Fatalf("an operation was admitted: %v", err)
	}
}

func (f *approvalFixture) status(id string) string {
	f.t.Helper()
	a, err := f.s.Approval(context.Background(), id)
	if err != nil {
		f.t.Fatal(err)
	}
	return a.Status
}

func deployBody(t *testing.T, dry bool) string {
	t.Helper()
	m, err := manifest.DecodeJSON([]byte(apManifestSource))
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(deploy.Request{App: "api", Version: apDigest, Manifest: *m, RequestID: "req-1", DryRun: dry})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func sameJSON(t *testing.T, a, b []byte) bool {
	t.Helper()
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		t.Fatalf("not JSON: %s | %s", a, b)
	}
	ax, _ := json.Marshal(x)
	by, _ := json.Marshal(y)
	return bytes.Equal(ax, by)
}

func TestAuthenticateCarriesTheAgentMark(t *testing.T) {
	f := newApprovalFixture(t, true, nil)
	for token, want := range map[string]bool{apAdminToken: false, apDeployToken: false, apAgentDeploy: true, apAgentAdmin: true} {
		r := httptest.NewRequest("GET", "/v1/environments/lab/audit", nil)
		r.Header.Set("Authorization", "Bearer "+token)
		actor, _, ok := authenticate(f.cfg, r)
		if !ok || actor.Agent != want {
			t.Errorf("%s: agent=%v ok=%v", token[:12], actor.Agent, ok)
		}
	}
}

func TestApprovalTTLIsValidated(t *testing.T) {
	base := Config{Environment: "lab", Listen: "127.0.0.1:9999", Tokens: []Token{{Digest: Digest(apAdminToken), Scope: "admin"}}}
	for ttl, ok := range map[string]bool{"": true, "15m": true, "1m": true, "2h": true, "59s": false, "0s": false, "-5m": false, "soon": false} {
		c := base
		c.ApprovalTTL = ttl
		if err := c.Validate(); (err == nil) != ok {
			t.Errorf("approval_ttl %q: %v", ttl, err)
		}
	}
	if (Config{}).approvalTTL() != 15*time.Minute || (Config{ApprovalTTL: "3m"}).approvalTTL() != 3*time.Minute {
		t.Fatal("the default or the configured expiry is wrong")
	}
}

func TestAgentDeployOnProtectedEnvironmentIsHeldAndNothingRuns(t *testing.T) {
	f := newApprovalFixture(t, true, nil)
	dry := f.call("POST", "/deploys", apAgentDeploy, deployBody(t, true))
	if dry.Code != http.StatusOK || dry.Header().Get("Berthy-Approval") != "" {
		t.Fatalf("an agent dry run is served as it is: %d %s", dry.Code, dry.Body.String())
	}
	if list := f.call("GET", "/approvals", apReadToken, ""); strings.TrimSpace(list.Body.String()) != "[]" {
		t.Fatalf("a dry run created an approval: %s", list.Body.String())
	}
	h := f.hold("POST", "/deploys", apAgentDeploy, deployBody(t, false))
	if !sameJSON(t, h.Plan, dry.Body.Bytes()) {
		t.Fatalf("the held plan is not the dry-run plan:\n%s\n%s", h.Plan, dry.Body.String())
	}
	if !h.Expires.Equal(apStartTime.Add(15 * time.Minute)) {
		t.Fatalf("expires %v", h.Expires)
	}
	f.noOperation()
	if history := f.call("GET", "/history/api", apReadToken, ""); strings.TrimSpace(history.Body.String()) != "[]" {
		t.Fatalf("history: %s", history.Body.String())
	}
	var list []struct {
		ID, Actor, Summary, Kind, Status string
		Plan                             json.RawMessage
		Expires                          time.Time
	}
	w := f.call("GET", "/approvals?status=pending", apDeployToken, "")
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &list) != nil || len(list) != 1 {
		t.Fatalf("list: %d %s", w.Code, w.Body.String())
	}
	if got := list[0]; got.ID != h.ID || got.Actor != "claude" || got.Summary != "deploy api sha256:aaaaaaaaaaaa" || got.Kind != "deploy" || got.Status != "pending" || !sameJSON(t, got.Plan, h.Plan) {
		t.Fatalf("listed: %+v", got)
	}
	if audit := f.audit(); len(audit) != 1 || audit[0] != "claude approval_request:deploy api sha256:aaaaaaaaaaaa held "+h.ID {
		t.Fatalf("audit: %v", audit)
	}
}

func TestAgentTokensActLikeAnyTokenOnAnUnprotectedEnvironment(t *testing.T) {
	f := newApprovalFixture(t, false, nil)
	w := f.call("POST", "/lock/acquire", apAgentDeploy, `{"message":"freeze"}`)
	if w.Code != http.StatusOK || w.Header().Get("Berthy-Approval") != "" {
		t.Fatalf("lock on an unprotected environment: %d %s", w.Code, w.Body.String())
	}
	if l, err := f.s.Lock(context.Background()); err != nil || l.Holder != "claude" {
		t.Fatalf("lock: %+v %v", l, err)
	}
}

func TestAProtectedDeployMakesTheEnvironmentProtectedForAgentTokensToo(t *testing.T) {
	// The agent config is not marked protected, but a protected deploy marked the state.
	f := newApprovalFixture(t, false, nil)
	m, err := manifest.DecodeJSON([]byte(`{"project":"demo","network":"overlay","protected":true,"apps":{"api":{"kind":"image","image":"ghcr.io/acme/api","roles":{"web":{}}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	op, _, err := f.s.Admit(context.Background(), state.Admission{RequestID: "p", ActorID: "ci", Kind: "deploy", App: "api", PublicRequest: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err = f.s.Finish(context.Background(), op.ID, state.Outcome{State: "succeeded", Protected: true, Release: &state.Release{App: "api", Digest: apDigest, Manifest: mustJSON(t, m)}}); err != nil {
		t.Fatal(err)
	}
	f.hold("POST", "/lock/acquire", apAgentDeploy, `{"message":"freeze"}`)
	if _, err = f.s.Lock(context.Background()); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("lock taken: %v", err)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestPeopleAndCITokensAreNeverHeld(t *testing.T) {
	f := newApprovalFixture(t, true, nil)
	if w := f.call("POST", "/secrets", apAdminToken, `{"name":"A","value":"v"}`); w.Code != http.StatusOK {
		t.Fatalf("admin token: %d %s", w.Code, w.Body.String())
	}
	if w := f.call("POST", "/lock/acquire", apDeployToken, `{"message":"freeze"}`); w.Code != http.StatusOK {
		t.Fatalf("deploy token: %d %s", w.Code, w.Body.String())
	}
}

func TestAgentReadTokenStillCannotWrite(t *testing.T) {
	f := newApprovalFixture(t, true, nil)
	for path, body := range map[string]string{"/deploys": deployBody(t, false), "/lock/acquire": `{"message":"x"}`, "/secrets": `{"name":"A","value":"v"}`} {
		if w := f.call("POST", path, apAgentRead, body); w.Code != http.StatusForbidden {
			t.Errorf("%s: %d %s", path, w.Code, w.Body.String())
		}
	}
	if got, _ := f.s.Approvals(context.Background(), "", f.now); len(got) != 0 {
		t.Fatalf("a refused request was held: %+v", got)
	}
}

func TestAgentSecretSetIsHeldAndTheValueIsNeverStoredInTheClear(t *testing.T) {
	f := newApprovalFixture(t, true, nil)
	const value = "postgres://user:hunter2-canary@db/prod"
	h := f.hold("POST", "/secrets", apAgentAdmin, `{"name":"DATABASE_URL","value":"`+value+`"}`)
	if strings.Contains(string(h.Plan), "canary") {
		t.Fatalf("the plan shows the value: %s", h.Plan)
	}
	if _, _, err := f.s.Secret(context.Background(), "DATABASE_URL"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("the secret was written: %v", err)
	}
	list := f.call("GET", "/approvals", apReadToken, "")
	if !strings.Contains(list.Body.String(), `"summary":"secret set DATABASE_URL"`) || strings.Contains(list.Body.String(), "canary") {
		t.Fatalf("list: %s", list.Body.String())
	}
	for _, name := range []string{"state.db", "state.db-wal"} {
		raw, err := os.ReadFile(filepath.Join(f.dir, name))
		if err == nil && bytes.Contains(raw, []byte("hunter2-canary")) {
			t.Fatalf("the secret value is in %s", name)
		}
	}
	// A person approves it, and only then does the secret exist.
	if w := f.call("POST", "/approvals/"+h.ID+"/approve", apAdminToken, ""); w.Code != http.StatusOK {
		t.Fatalf("approve: %d %s", w.Code, w.Body.String())
	}
	if _, v, err := f.s.Secret(context.Background(), "DATABASE_URL"); err != nil || string(v) != value {
		t.Fatalf("approved secret: %q %v", v, err)
	}
}

func TestApproveExecutesTheStoredRequestAsTheAgentAndAuditsTheApprover(t *testing.T) {
	f := newApprovalFixture(t, true, nil)
	h := f.hold("POST", "/lock/acquire", apAgentDeploy, `{"message":"freeze for review"}`)
	f.now = f.now.Add(2 * time.Minute)
	w := f.call("POST", "/approvals/"+h.ID+"/approve", apAdminToken, "")
	if w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != `{"ok":true}` {
		t.Fatalf("approve: %d %s", w.Code, w.Body.String())
	}
	if l, err := f.s.Lock(context.Background()); err != nil || l.Reason != "freeze for review" {
		t.Fatalf("the stored request did not run: %+v %v", l, err)
	}
	audit := f.audit()
	want := []string{
		"claude approval_request:lock acquire held " + h.ID,
		"alice approval_approve approved " + h.ID,
		"claude (approved by alice) lock_acquire succeeded",
	}
	if strings.Join(audit, "|") != strings.Join(want, "|") {
		t.Fatalf("audit:\n%s\nwant:\n%s", strings.Join(audit, "\n"), strings.Join(want, "\n"))
	}
	a, _ := f.s.Approval(context.Background(), h.ID)
	if a.Status != "approved" || a.DecidedBy != "alice" || a.Result == "" {
		t.Fatalf("approval: %+v", a)
	}
	if again := f.call("POST", "/approvals/"+h.ID+"/approve", apAdminToken, ""); again.Code != http.StatusConflict {
		t.Fatalf("second approve: %d %s", again.Code, again.Body.String())
	}
}

func TestApprovedOperationReportsItsOperationAndRunsAsTheAgent(t *testing.T) {
	f := newApprovalFixture(t, true, nil)
	// api was deployed and then removed, so removing it again is a plan that needs no Docker.
	recordAppRelease(t, f.s, "api", "api", apManifestSource)
	forgetApp(t, f.s, "api")
	body := `{"app":"api","request_id":"rm-1"}`
	h := f.hold("POST", "/remove", apAgentAdmin, body)
	w := f.call("POST", "/approvals/"+h.ID+"/approve", apAdminToken, "")
	var accepted struct {
		OperationID string `json:"operation_id"`
		EventsURL   string `json:"events_url"`
	}
	if w.Code != http.StatusAccepted || json.Unmarshal(w.Body.Bytes(), &accepted) != nil || accepted.OperationID == "" || w.Header().Get("Location") != accepted.EventsURL {
		t.Fatalf("approve: %d %v %s", w.Code, w.Header(), w.Body.String())
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		op, err := f.s.Operation(context.Background(), accepted.OperationID)
		if err != nil {
			t.Fatal(err)
		}
		if op.ActorName != "claude-admin (approved by alice)" || op.ActorID == "" {
			t.Fatalf("the operation did not run as the agent: %q", op.ActorName)
		}
		if op.State != "running" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("operation did not finish")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if a, _ := f.s.Approval(context.Background(), h.ID); a.Result != "operation "+accepted.OperationID {
		t.Fatalf("result %q", a.Result)
	}
}

func TestAgentTokensCannotApproveOrReject(t *testing.T) {
	f := newApprovalFixture(t, true, nil)
	h := f.hold("POST", "/lock/acquire", apAgentDeploy, `{"message":"x"}`)
	for _, token := range []string{apAgentAdmin, apAgentAdminTwo, apAgentRead, apAgentDeploy, apReadToken, apDeployToken} {
		for _, verb := range []string{"approve", "reject"} {
			if w := f.call("POST", "/approvals/"+h.ID+"/"+verb, token, ""); w.Code != http.StatusForbidden {
				t.Errorf("%s with %s: %d %s", verb, token[:12], w.Code, w.Body.String())
			}
		}
	}
	if f.status(h.ID) != "pending" {
		t.Fatal("the approval was decided")
	}
	if _, err := f.s.Lock(context.Background()); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("the request ran: %v", err)
	}
	if w := f.call("POST", "/approvals/"+h.ID+"/approve", "", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("no token: %d", w.Code)
	}
}

func TestExpiredApprovalCannotRunAndIsMarkedExpired(t *testing.T) {
	f := newApprovalFixture(t, true, nil)
	h := f.hold("POST", "/lock/acquire", apAgentDeploy, `{"message":"x"}`)
	f.now = f.now.Add(15*time.Minute + time.Second)
	w := f.call("POST", "/approvals/"+h.ID+"/approve", apAdminToken, "")
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "approval expired") {
		t.Fatalf("approve after expiry: %d %s", w.Code, w.Body.String())
	}
	if f.status(h.ID) != "expired" {
		t.Fatalf("status %q", f.status(h.ID))
	}
	if _, err := f.s.Lock(context.Background()); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("an expired request ran: %v", err)
	}
	if audit := f.audit(); audit[len(audit)-1] != "alice approval_expired expired "+h.ID {
		t.Fatalf("audit: %v", audit)
	}
	if w = f.call("POST", "/approvals/"+h.ID+"/reject", apAdminToken, ""); w.Code != http.StatusConflict {
		t.Fatalf("reject after expiry: %d", w.Code)
	}
}

func TestListingExpiresStaleRequestsAndHonoursTheConfiguredLifetime(t *testing.T) {
	f := newApprovalFixture(t, true, func(c *Config) { c.ApprovalTTL = "2m" })
	h := f.hold("POST", "/lock/acquire", apAgentDeploy, `{"message":"x"}`)
	if !h.Expires.Equal(apStartTime.Add(2 * time.Minute)) {
		t.Fatalf("expires %v", h.Expires)
	}
	f.now = f.now.Add(2 * time.Minute)
	w := f.call("GET", "/approvals?status=pending", apReadToken, "")
	if strings.TrimSpace(w.Body.String()) != "[]" {
		t.Fatalf("pending: %s", w.Body.String())
	}
	w = f.call("GET", "/approvals", apReadToken, "")
	if !strings.Contains(w.Body.String(), `"status":"expired"`) {
		t.Fatalf("all: %s", w.Body.String())
	}
	if w = f.call("GET", "/approvals?status=bogus", apReadToken, ""); w.Code != http.StatusBadRequest {
		t.Fatalf("bad status: %d", w.Code)
	}
}

func TestApproveRefusesWhenThePlanChangedSinceTheRequest(t *testing.T) {
	f := newApprovalFixture(t, true, nil)
	recordAppRelease(t, f.s, "api", "api", apManifestSource)
	h := f.hold("POST", "/remove", apAgentAdmin, `{"app":"api","request_id":"rm-1"}`)
	// Someone removed the app after the request: what approving would do is no longer what was reviewed.
	forgetApp(t, f.s, "api")
	w := f.call("POST", "/approvals/"+h.ID+"/approve", apAdminToken, "")
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "the plan changed since it was requested; ask for a new request") {
		t.Fatalf("approve: %d %s", w.Code, w.Body.String())
	}
	a, _ := f.s.Approval(context.Background(), h.ID)
	if a.Status != "failed" || !strings.Contains(a.Result, "plan changed") {
		t.Fatalf("approval: %+v", a)
	}
	f.noOperation()
	if ops := f.call("GET", "/history/api", apReadToken, ""); strings.Contains(ops.Body.String(), `"kind":"remove"`) {
		t.Fatalf("an operation ran: %s", ops.Body.String())
	}
}

func TestApproveRefusesWhenTheRequestingTokenIsGone(t *testing.T) {
	f := newApprovalFixture(t, true, nil)
	h := f.hold("POST", "/lock/acquire", apAgentDeploy, `{"message":"x"}`)
	// The agent restarts with the token removed from its configuration.
	cfg := f.cfg
	cfg.Tokens = append([]Token(nil), cfg.Tokens...)
	for i := range cfg.Tokens {
		if cfg.Tokens[i].Name == "claude" {
			cfg.Tokens = append(cfg.Tokens[:i], cfg.Tokens[i+1:]...)
			break
		}
	}
	f.h = NewDeploymentHandler(cfg, nil, nil, f.ctrl)
	w := f.call("POST", "/approvals/"+h.ID+"/approve", apAdminToken, "")
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "requesting token") {
		t.Fatalf("approve: %d %s", w.Code, w.Body.String())
	}
	if _, err := f.s.Lock(context.Background()); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("the request ran: %v", err)
	}
}

func TestRejectRecordsTheReasonAndStopsTheRequest(t *testing.T) {
	f := newApprovalFixture(t, true, nil)
	h := f.hold("POST", "/lock/acquire", apAgentDeploy, `{"message":"x"}`)
	if w := f.call("POST", "/approvals/"+h.ID+"/reject", apAdminToken, `{"reason":"not during the sale"}`); w.Code != http.StatusOK {
		t.Fatalf("reject: %d %s", w.Code, w.Body.String())
	}
	a, _ := f.s.Approval(context.Background(), h.ID)
	if a.Status != "rejected" || a.DecidedBy != "alice" || a.Result != "not during the sale" {
		t.Fatalf("approval: %+v", a)
	}
	if audit := f.audit(); audit[len(audit)-1] != "alice approval_reject rejected "+h.ID {
		t.Fatalf("audit: %v", audit)
	}
	if w := f.call("POST", "/approvals/"+h.ID+"/approve", apAdminToken, ""); w.Code != http.StatusConflict {
		t.Fatalf("approve after reject: %d", w.Code)
	}
	if w := f.call("POST", "/approvals/"+h.ID+"/reject", apAdminToken, ""); w.Code != http.StatusConflict {
		t.Fatalf("second reject: %d", w.Code)
	}
	if w := f.call("POST", "/approvals/nope/reject", apAdminToken, ""); w.Code != http.StatusNotFound {
		t.Fatalf("unknown id: %d", w.Code)
	}
	other := f.hold("POST", "/lock/acquire", apAgentDeploy, `{"message":"y"}`)
	if w := f.call("POST", "/approvals/"+other.ID+"/reject", apAdminToken, ""); w.Code != http.StatusOK {
		t.Fatalf("reject without a reason: %d %s", w.Code, w.Body.String())
	}
	third := f.hold("POST", "/lock/acquire", apAgentDeploy, `{"message":"z"}`)
	if w := f.call("POST", "/approvals/"+third.ID+"/reject", apAdminToken, `{"reason":"`+strings.Repeat("x", 201)+`"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("a long reason: %d", w.Code)
	}
	if _, err := f.s.Lock(context.Background()); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("a rejected request ran: %v", err)
	}
}

func TestHeldRequestsWithoutADryRunGetADescriptionPlan(t *testing.T) {
	f := newApprovalFixture(t, true, nil)
	for _, tc := range []struct{ method, path, body, summary, plan string }{
		{"POST", "/backups/main-db/run", "", "backup run main-db", `{"action":"backup run","target":"main-db"}`},
		{"POST", "/jobs/nightly/run", "", "job run nightly", `{"action":"job run","target":"nightly"}`},
		{"POST", "/state-backup", "", "state backup run", `{"action":"state backup run"}`},
		{"POST", "/alerts/test", "", "alerts test", `{"action":"alerts test"}`},
	} {
		h := f.hold(tc.method, tc.path, apAgentAdmin, tc.body)
		a, err := f.s.Approval(context.Background(), h.ID)
		if err != nil || a.Summary != tc.summary || !sameJSON(t, h.Plan, []byte(tc.plan)) {
			t.Errorf("%s: %+v %s %v", tc.path, a, h.Plan, err)
		}
	}
}

func TestHeldRequestThatCannotBePlannedIsRefusedNotHeld(t *testing.T) {
	f := newApprovalFixture(t, true, nil)
	// An agent with a deploy token cannot ask for what only an administrator may do; it is told so at once.
	m, _ := manifest.DecodeJSON([]byte(apManifestSource))
	body := mustJSON(t, deploy.Request{App: "api", Version: apDigest, Manifest: *m, RequestID: "x", Adopt: true})
	if w := f.call("POST", "/deploys", apAgentDeploy, string(body)); w.Code != http.StatusForbidden {
		t.Fatalf("adopt: %d %s", w.Code, w.Body.String())
	}
	if w := f.call("POST", "/deploys", apAgentDeploy, "not json"); w.Code != http.StatusBadRequest {
		t.Fatalf("bad body: %d %s", w.Code, w.Body.String())
	}
	if w := f.call("POST", "/secrets", apAgentAdmin, `{"name":"TUNNEL_TOKEN","value":"v"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("reserved secret: %d %s", w.Code, w.Body.String())
	}
	if w := f.call("POST", "/remove", apAgentAdmin, `{"app":"ghost","request_id":"r"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("remove of an unknown app: %d %s", w.Code, w.Body.String())
	}
	if got, _ := f.s.Approvals(context.Background(), "", f.now); len(got) != 0 {
		t.Fatalf("a request that has no plan was held: %+v", got)
	}
}

func TestEverySummaryIsOneShortPrintableLine(t *testing.T) {
	for _, tc := range []struct {
		rest       []string
		body, want string
	}{
		{[]string{"deploys"}, `{"app":"api","version":"1.4.2"}`, "deploy api 1.4.2"},
		{[]string{"deploys"}, `{"app":"api","version":"sha256:` + strings.Repeat("b", 64) + `"}`, "deploy api sha256:bbbbbbbbbbbb"},
		{[]string{"deploys"}, `{"app":"api","keep_version":true}`, "deploy api (keeps the current version)"},
		{[]string{"deploys"}, `{"app":"api","keep_version":true,"keep_digest":"example/api@sha256:` + strings.Repeat("c", 64) + `"}`, "deploy api (keeps sha256:cccccccccccc)"},
		{[]string{"deploys"}, `{"app":"api","keep_version":true,"keep_digest":"sha256:` + strings.Repeat("c", 64) + `"}`, "deploy api (keeps sha256:cccccccccccc)"},
		{[]string{"deploys"}, `{"app":"shop"}`, "deploy shop"},
		{[]string{"rollback"}, `{"app":"api"}`, "rollback api"},
		{[]string{"rollback"}, `{"app":"api","target_release":7}`, "rollback api to release 7"},
		{[]string{"restart"}, `{"app":"api","role":"web"}`, "restart api/web"},
		{[]string{"restart"}, `{"app":"api"}`, "restart api"},
		{[]string{"scale"}, `{"app":"api","role":"web","replicas":3}`, "scale api/web to 3"},
		{[]string{"exec"}, `{"app":"api","command":["rm","-rf","/"]}`, "exec api"},
		{[]string{"exec"}, `{"app":"db","service":"postgres","command":["psql"]}`, "exec db/postgres"},
		{[]string{"prune"}, `{}`, "prune"},
		{[]string{"remove"}, `{"app":"api","volumes":true}`, "remove api and its volumes"},
		{[]string{"destroy"}, `{}`, "destroy the environment"},
		{[]string{"restore"}, `{"key":"files/a.tar.gz","into":"data2"}`, "restore files/a.tar.gz into data2"},
		{[]string{"ingress", "apply"}, `{}`, "ingress apply"},
		{[]string{"secrets"}, `{"name":"DATABASE_URL","value":"v"}`, "secret set DATABASE_URL"},
		{[]string{"secrets", "OLD"}, `{}`, "secret delete OLD"},
		{[]string{"lock", "release"}, `{}`, "lock release"},
		{[]string{"operations", "op1", "resolve"}, `{"reason":"gone"}`, "recovery resolve op1"},
		{[]string{"deploys"}, "{\"app\":\"api\\u001b[31m\\nevil\",\"version\":\"1\"}", "deploy api?[31m?evil 1"},
	} {
		if got := summarize(tc.rest, []byte(tc.body)); got != tc.want {
			t.Errorf("%v %s: %q, want %q", tc.rest, tc.body, got, tc.want)
		}
	}
	long := summarize([]string{"deploys"}, []byte(`{"app":"`+strings.Repeat("x", 500)+`","version":"1"}`))
	if len(long) > 120 {
		t.Errorf("summary of %d bytes", len(long))
	}
}

func TestOnlyDeployScopeSeesThePlansInTheListing(t *testing.T) {
	f := newApprovalFixture(t, true, nil)
	h := f.hold("POST", "/lock/acquire", apAgentDeploy, `{"message":"freeze"}`)
	type row struct {
		ID, Summary, Status string
		Plan                json.RawMessage
	}
	list := func(token string) []row {
		t.Helper()
		var rows []row
		w := f.call("GET", "/approvals", token, "")
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &rows) != nil || len(rows) != 1 {
			t.Fatalf("list: %d %s", w.Code, w.Body.String())
		}
		return rows
	}
	if got := list(apReadToken)[0]; got.ID != h.ID || got.Summary != "lock acquire" || got.Status != "pending" || got.Plan != nil {
		t.Fatalf("read scope: %+v", got)
	}
	if strings.Contains(f.call("GET", "/approvals", apAgentRead, "").Body.String(), `"plan"`) {
		t.Fatal("a read-scope agent token sees a plan")
	}
	for _, token := range []string{apDeployToken, apAdminToken} {
		if got := list(token)[0]; !sameJSON(t, got.Plan, []byte(`{"action":"lock acquire","message":"freeze","currently_locked":false}`)) {
			t.Fatalf("deploy or admin scope: %s", got.Plan)
		}
	}
}

func TestAManagedSecretValueInAPlanIsStoredAndListedRedacted(t *testing.T) {
	f := newApprovalFixture(t, true, nil)
	const value = "s3cret-canary-value"
	if err := f.s.PutSecret(context.Background(), state.Actor{ID: "admin"}, state.SecretInfo{Name: "API_KEY"}, []byte(value)); err != nil {
		t.Fatal(err)
	}
	// A lock message stands for any text of a plan, such as an exec command line.
	h := f.hold("POST", "/lock/acquire", apAgentDeploy, `{"message":"curl -H token:`+value+` x"}`)
	if strings.Contains(string(h.Plan), value) || !strings.Contains(string(h.Plan), "[redacted]") {
		t.Fatalf("the answer to the agent shows the value: %s", h.Plan)
	}
	a, err := f.s.Approval(context.Background(), h.ID)
	if err != nil || strings.Contains(string(a.Plan), value) || !strings.Contains(string(a.Plan), "[redacted]") {
		t.Fatalf("the stored plan: %s %v", a.Plan, err)
	}
	if w := f.call("GET", "/approvals", apAdminToken, ""); strings.Contains(w.Body.String(), value) || !strings.Contains(w.Body.String(), "[redacted]") {
		t.Fatalf("listing: %s", w.Body.String())
	}
	// The approver still runs the request as it was made, and the unchanged redacted plan matches.
	if w := f.call("POST", "/approvals/"+h.ID+"/approve", apAdminToken, ""); w.Code != http.StatusOK {
		t.Fatalf("approve: %d %s", w.Code, w.Body.String())
	}
	if l, err := f.s.Lock(context.Background()); err != nil || !strings.Contains(l.Reason, value) {
		t.Fatalf("the approved request was altered: %+v %v", l, err)
	}
}

func TestSecretAndLockDryRunsReturnARealPlan(t *testing.T) {
	f := newApprovalFixture(t, false, nil)
	plan := func(path, token, body string) string {
		t.Helper()
		w := f.call("POST", path, token, body)
		if w.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
		return strings.TrimSpace(w.Body.String())
	}
	check := func(got, want string) {
		t.Helper()
		if !sameJSON(t, []byte(got), []byte(want)) {
			t.Fatalf("plan %s, want %s", got, want)
		}
	}
	const value = "dry-run-canary"
	check(plan("/secrets", apAdminToken, `{"name":"DATABASE_URL","value":"`+value+`","registry":"ghcr.io","dry_run":true}`),
		`{"action":"secret set","name":"DATABASE_URL","registry":"ghcr.io","exists":false,"services":[]}`)
	del := func() string {
		t.Helper()
		r := httptest.NewRequest("DELETE", "/v1/environments/lab/secrets/DATABASE_URL", strings.NewReader(`{"dry_run":true}`))
		r.Header.Set("Authorization", "Bearer "+apAdminToken)
		w := httptest.NewRecorder()
		f.h.ServeHTTP(w, r)
		return w.Body.String()
	}
	check(del(), `{"action":"secret delete","name":"DATABASE_URL","exists":false}`)
	if _, _, err := f.s.Secret(context.Background(), "DATABASE_URL"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("a dry run wrote: %v", err)
	}
	if w := f.call("POST", "/secrets", apAdminToken, `{"name":"DATABASE_URL","value":"`+value+`"}`); w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}
	got := plan("/secrets", apAdminToken, `{"name":"DATABASE_URL","value":"other","dry_run":true}`)
	check(got, `{"action":"secret set","name":"DATABASE_URL","registry":"","exists":true}`)
	if strings.Contains(got, value) || strings.Contains(got, "other") {
		t.Fatal("the plan shows a value")
	}
	check(del(), `{"action":"secret delete","name":"DATABASE_URL","exists":true}`)
	if _, _, err := f.s.Secret(context.Background(), "DATABASE_URL"); err != nil {
		t.Fatalf("a dry run deleted: %v", err)
	}
	check(plan("/lock/acquire", apDeployToken, `{"message":"freeze","dry_run":true}`), `{"action":"lock acquire","message":"freeze","currently_locked":false}`)
	check(plan("/lock/release", apDeployToken, `{"dry_run":true}`), `{"action":"lock release","currently_locked":false}`)
	if w := f.call("POST", "/lock/acquire", apDeployToken, `{"message":"freeze"}`); w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}
	check(plan("/lock/release", apDeployToken, `{"dry_run":true}`), `{"action":"lock release","currently_locked":true}`)
	check(plan("/lock/acquire", apDeployToken, `{"message":"again","dry_run":true}`), `{"action":"lock acquire","message":"again","currently_locked":true}`)
}

func TestHeldSecretSetIsRefusedWhenTheSecretAppearedSinceTheRequest(t *testing.T) {
	f := newApprovalFixture(t, true, nil)
	h := f.hold("POST", "/secrets", apAgentAdmin, `{"name":"DATABASE_URL","value":"v1"}`)
	if !sameJSON(t, h.Plan, []byte(`{"action":"secret set","name":"DATABASE_URL","registry":"","exists":false}`)) {
		t.Fatalf("plan: %s", h.Plan)
	}
	if err := f.s.PutSecret(context.Background(), state.Actor{ID: "admin"}, state.SecretInfo{Name: "DATABASE_URL"}, []byte("someone-else")); err != nil {
		t.Fatal(err)
	}
	w := f.call("POST", "/approvals/"+h.ID+"/approve", apAdminToken, "")
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "the plan changed") {
		t.Fatalf("approve: %d %s", w.Code, w.Body.String())
	}
	if _, v, _ := f.s.Secret(context.Background(), "DATABASE_URL"); string(v) != "someone-else" {
		t.Fatalf("the secret was overwritten: %q", v)
	}
}
