package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"berthy/internal/monitor"
	"berthy/internal/state"
)

func newGet(path, token string) *http.Request {
	r := httptest.NewRequest("GET", path, nil)
	r.Header.Set("Authorization", "Bearer "+token)
	return r
}

type createdToken struct {
	Name    string     `json:"name"`
	Scope   string     `json:"scope"`
	Agent   bool       `json:"agent"`
	Expires *time.Time `json:"expires"`
	Token   string     `json:"token"`
}

// makeToken creates a token through the API as the admin token, and returns what the agent answered.
func (f *approvalFixture) makeToken(body string) createdToken {
	f.t.Helper()
	w := f.call("POST", "/tokens", apAdminToken, body)
	var got createdToken
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &got) != nil || got.Token == "" {
		f.t.Fatalf("create %s: %d %s", body, w.Code, w.Body.String())
	}
	return got
}

func (f *approvalFixture) listTokens(token string) map[string]tokenView {
	f.t.Helper()
	w := f.call("GET", "/tokens", token, "")
	var rows []tokenView
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &rows) != nil {
		f.t.Fatalf("list: %d %s", w.Code, w.Body.String())
	}
	out := map[string]tokenView{}
	for _, r := range rows {
		out[r.Name] = r
	}
	return out
}

func TestATokenMadeThroughTheAPIWorksAtOnceAndOnlyItsDigestIsStored(t *testing.T) {
	f := newApprovalFixture(t, false, nil)
	got := f.makeToken(`{"name":"deployer","scope":"deploy"}`)
	if !tokenPattern("lab").MatchString(got.Token) || got.Name != "deployer" || got.Scope != "deploy" || got.Agent || got.Expires != nil {
		t.Fatalf("answer: %+v", got)
	}
	if w := f.call("GET", "/audit", got.Token, ""); w.Code != http.StatusOK {
		t.Fatalf("the new token does not work: %d %s", w.Code, w.Body.String())
	}
	if w := f.call("POST", "/lock/acquire", got.Token, `{"message":"x"}`); w.Code != http.StatusOK {
		t.Fatalf("a deploy token takes a deploy route: %d %s", w.Code, w.Body.String())
	}
	if w := f.call("GET", "/tokens", got.Token, ""); w.Code != http.StatusForbidden {
		t.Fatalf("a deploy token lists tokens: %d", w.Code)
	}
	rows, err := f.s.Tokens(context.Background())
	if err != nil || len(rows) != 1 || rows[0].Digest != Digest(got.Token) || rows[0].Name != "deployer" {
		t.Fatalf("state: %+v %v", rows, err)
	}
	for _, line := range f.audit() {
		if strings.Contains(line, got.Token) {
			t.Fatal("the audit holds the token")
		}
	}
	if audit := f.audit(); audit[0] != "alice token_create:deployer succeeded" {
		t.Fatalf("audit: %v", audit)
	}
}

func TestARevokedTokenGets401AtItsNextRequest(t *testing.T) {
	f := newApprovalFixture(t, false, nil)
	got := f.makeToken(`{"name":"deployer","scope":"deploy"}`)
	if w := f.call("GET", "/audit", got.Token, ""); w.Code != http.StatusOK {
		t.Fatalf("before: %d", w.Code)
	}
	if w := f.call("DELETE", "/tokens/deployer", apAdminToken, `{}`); w.Code != http.StatusOK {
		t.Fatalf("revoke: %d %s", w.Code, w.Body.String())
	}
	// The read routes are served by another router than the deployment routes: both must refuse it.
	for _, path := range []string{"/audit", "/apps", "/lock/acquire"} {
		method := "GET"
		if path == "/lock/acquire" {
			method = "POST"
		}
		if w := f.call(method, path, got.Token, `{"message":"x"}`); w.Code != http.StatusUnauthorized {
			t.Fatalf("%s after revoke: %d %s", path, w.Code, w.Body.String())
		}
	}
	if audit := f.audit(); audit[len(audit)-1] != "alice token_revoke:deployer succeeded" {
		t.Fatalf("audit: %v", audit)
	}
	// The name can be used again, and the old value does not come back with it.
	again := f.makeToken(`{"name":"deployer","scope":"read"}`)
	if again.Token == got.Token || f.call("GET", "/audit", got.Token, "").Code != http.StatusUnauthorized {
		t.Fatal("the revoked value works again")
	}
}

func TestAnExpiredTokenGets401AtItsNextRequest(t *testing.T) {
	f := newApprovalFixture(t, false, nil)
	expires := f.now.Add(2 * time.Hour).Format(time.RFC3339)
	got := f.makeToken(`{"name":"short","scope":"read","expires":"` + expires + `"}`)
	if got.Expires == nil || !got.Expires.Equal(f.now.Add(2*time.Hour)) {
		t.Fatalf("expires: %v", got.Expires)
	}
	f.now = f.now.Add(time.Hour)
	if w := f.call("GET", "/audit", got.Token, ""); w.Code != http.StatusOK {
		t.Fatalf("before the expiry: %d", w.Code)
	}
	f.now = f.now.Add(time.Hour)
	for _, path := range []string{"/audit", "/apps"} {
		if w := f.call("GET", path, got.Token, ""); w.Code != http.StatusUnauthorized {
			t.Fatalf("%s at the expiry: %d", path, w.Code)
		}
	}
	// It still shows in the list, so it can be revoked.
	if row, ok := f.listTokens(apAdminToken)["short"]; !ok || row.Expires == nil {
		t.Fatalf("list: %+v", row)
	}
}

func TestAnAgentTokenCannotManageTokensAndIsNeverHeld(t *testing.T) {
	for _, protected := range []bool{false, true} {
		f := newApprovalFixture(t, protected, nil)
		f.makeToken(`{"name":"victim","scope":"deploy"}`)
		for _, call := range [][3]string{
			{"POST", "/tokens", `{"name":"mine","scope":"admin"}`},
			{"POST", "/tokens", `{"name":"mine","scope":"admin","dry_run":true}`},
			{"DELETE", "/tokens/victim", `{}`},
			{"DELETE", "/tokens/victim", `{"dry_run":true}`},
		} {
			w := f.call(call[0], call[1], apAgentAdmin, call[2])
			if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "an agent token cannot manage tokens") || w.Header().Get("Berthy-Approval") != "" {
				t.Fatalf("protected=%v %v: %d %v %s", protected, call, w.Code, w.Header(), w.Body.String())
			}
		}
		if w := f.call("POST", "/tokens", apAgentDeploy, `{"name":"mine","scope":"admin"}`); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "an agent token cannot manage tokens") {
			t.Fatalf("an agent deploy token: %d", w.Code)
		}
		if w := f.call("GET", "/tokens", apAgentAdmin, ""); w.Code != http.StatusOK {
			t.Fatalf("an agent admin token may list: %d", w.Code)
		}
		if w := f.call("GET", "/tokens", apAgentDeploy, ""); w.Code != http.StatusForbidden {
			t.Fatalf("an agent deploy token lists: %d", w.Code)
		}
		if rows, _ := f.s.Tokens(context.Background()); len(rows) != 1 || rows[0].Name != "victim" {
			t.Fatalf("tokens: %+v", rows)
		}
		if list := f.call("GET", "/approvals", apReadToken, ""); strings.TrimSpace(list.Body.String()) != "[]" {
			t.Fatalf("a token request was held: %s", list.Body.String())
		}
	}
}

func TestAnAgentTokenMadeThroughTheAPIIsAnAgentToken(t *testing.T) {
	f := newApprovalFixture(t, true, nil)
	bot := f.makeToken(`{"name":"bot","scope":"admin","agent":true}`)
	if !bot.Agent {
		t.Fatalf("answer: %+v", bot)
	}
	if w := f.call("POST", "/tokens", bot.Token, `{"name":"mine","scope":"admin"}`); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "cannot manage tokens") {
		t.Fatalf("create by a state agent token: %d %s", w.Code, w.Body.String())
	}
	f.hold("POST", "/lock/acquire", bot.Token, `{"message":"x"}`)
}

func TestCreateAndRevokeNeedAnAdminToken(t *testing.T) {
	f := newApprovalFixture(t, false, nil)
	for _, token := range []string{apReadToken, apDeployToken} {
		if w := f.call("POST", "/tokens", token, `{"name":"x","scope":"read"}`); w.Code != http.StatusForbidden {
			t.Fatalf("create: %d", w.Code)
		}
		if w := f.call("DELETE", "/tokens/ci", token, `{}`); w.Code != http.StatusForbidden {
			t.Fatalf("revoke: %d", w.Code)
		}
		if w := f.call("GET", "/tokens", token, ""); w.Code != http.StatusForbidden {
			t.Fatalf("list: %d", w.Code)
		}
	}
	if w := f.call("GET", "/tokens", "", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("no token: %d", w.Code)
	}
}

func TestANameIsUniqueAcrossTheFileAndTheAPI(t *testing.T) {
	f := newApprovalFixture(t, false, nil)
	f.makeToken(`{"name":"deployer","scope":"deploy"}`)
	for _, name := range []string{"deployer", "ci", "alice"} {
		w := f.call("POST", "/tokens", apAdminToken, `{"name":"`+name+`","scope":"read"}`)
		if w.Code != http.StatusConflict {
			t.Fatalf("%s: %d %s", name, w.Code, w.Body.String())
		}
	}
	for body, why := range map[string]string{
		`{"name":"","scope":"read"}`:                                   "no name",
		`{"name":"has space","scope":"read"}`:                          "bad name",
		`{"name":"x","scope":"owner"}`:                                 "bad scope",
		`{"name":"x"}`:                                                 "no scope",
		`{"name":"x","scope":"read","expires":"tomorrow"}`:             "bad expires",
		`{"name":"x","scope":"read","expires":"2026-11-15T12:00:00Z"}`: "past expires",
		`{"name":"x","scope":"read","bogus":1}`:                        "unknown field",
	} {
		if w := f.call("POST", "/tokens", apAdminToken, body); w.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", why, w.Code, w.Body.String())
		}
	}
}

func TestATokenOfTheFileCannotBeRevokedThroughTheAPI(t *testing.T) {
	f := newApprovalFixture(t, false, nil)
	w := f.call("DELETE", "/tokens/ci", apAdminToken, `{}`)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "token ci is defined in /etc/berthy/agent.json; remove it there") {
		t.Fatalf("revoke: %d %s", w.Code, w.Body.String())
	}
	if w := f.call("DELETE", "/tokens/nobody", apAdminToken, `{}`); w.Code != http.StatusNotFound {
		t.Fatalf("absent: %d", w.Code)
	}
	if w := f.call("GET", "/audit", apDeployToken, ""); w.Code != http.StatusOK {
		t.Fatalf("the file's token stopped working: %d", w.Code)
	}
}

// onlyStateAdmins is a configuration whose only tokens that may manage tokens are agent tokens, so the people who can are the
// ones the test puts in the state.
func onlyStateAdmins(c *Config) {
	kept := c.Tokens[:0:0]
	for _, tok := range c.Tokens {
		if tok.Name != "alice" {
			kept = append(kept, tok)
		}
	}
	c.Tokens = kept
}

func TestTheLastAdminTokenCannotBeRevoked(t *testing.T) {
	f := newApprovalFixture(t, false, onlyStateAdmins)
	ctx := context.Background()
	admin := func(name, digest string, expires *time.Time) {
		t.Helper()
		if err := f.s.CreateToken(ctx, state.Actor{ID: "x", Name: "x"}, state.Token{Name: name, Digest: digest, Scope: "admin", Created: f.now, Expires: expires}); err != nil {
			t.Fatal(err)
		}
	}
	boss, second := "bty.lab."+strings.Repeat("8", 64), "bty.lab."+strings.Repeat("9", 64)
	admin("boss", Digest(boss), nil)
	// The agent admin tokens of the file do not count: they cannot manage tokens.
	if w := f.call("DELETE", "/tokens/boss", boss, `{}`); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "last admin token") {
		t.Fatalf("sole admin: %d %s", w.Code, w.Body.String())
	}
	if w := f.call("DELETE", "/tokens/boss", boss, `{"dry_run":true}`); w.Code != http.StatusConflict {
		t.Fatalf("sole admin, dry run: %d", w.Code)
	}
	// An expired admin token does not count either.
	past := f.now.Add(-time.Hour)
	admin("old", Digest("bty.lab."+strings.Repeat("7", 64)), &past)
	if w := f.call("DELETE", "/tokens/boss", boss, `{}`); w.Code != http.StatusConflict {
		t.Fatalf("an expired admin counted: %d", w.Code)
	}
	// An expired admin token can be revoked: it is not the one that remains.
	if w := f.call("DELETE", "/tokens/old", boss, `{}`); w.Code != http.StatusOK {
		t.Fatalf("revoke the expired one: %d %s", w.Code, w.Body.String())
	}
	admin("second", Digest(second), nil)
	if w := f.call("DELETE", "/tokens/boss", second, `{}`); w.Code != http.StatusOK {
		t.Fatalf("revoke with another admin left: %d %s", w.Code, w.Body.String())
	}
	if w := f.call("DELETE", "/tokens/second", second, `{}`); w.Code != http.StatusConflict {
		t.Fatalf("the last one: %d", w.Code)
	}
	if rows, _ := f.s.Tokens(ctx); len(rows) != 1 || rows[0].Name != "second" {
		t.Fatalf("tokens: %+v", rows)
	}
}

func TestAnAdminTokenOfTheFileCountsAsRemaining(t *testing.T) {
	f := newApprovalFixture(t, false, nil)
	extra := f.makeToken(`{"name":"extra","scope":"admin"}`)
	if w := f.call("DELETE", "/tokens/extra", extra.Token, `{}`); w.Code != http.StatusOK {
		t.Fatalf("alice of the file remains: %d %s", w.Code, w.Body.String())
	}
}

func TestADryRunChangesNothing(t *testing.T) {
	f := newApprovalFixture(t, false, nil)
	w := f.call("POST", "/tokens", apAdminToken, `{"name":"x","scope":"deploy","agent":true,"dry_run":true}`)
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), "bty.") || !strings.Contains(w.Body.String(), `"name":"x"`) {
		t.Fatalf("create dry run: %d %s", w.Code, w.Body.String())
	}
	f.makeToken(`{"name":"y","scope":"read"}`)
	before := len(f.audit())
	if w := f.call("DELETE", "/tokens/y", apAdminToken, `{"dry_run":true}`); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "token revoke") {
		t.Fatalf("revoke dry run: %d %s", w.Code, w.Body.String())
	}
	if rows, _ := f.s.Tokens(context.Background()); len(rows) != 1 || rows[0].Name != "y" || len(f.audit()) != before {
		t.Fatalf("a dry run changed the state: %+v", rows)
	}
}

func TestAnApprovalHeldByATokenThatIsRevokedCannotBeApproved(t *testing.T) {
	f := newApprovalFixture(t, true, nil)
	bot := f.makeToken(`{"name":"bot","scope":"deploy","agent":true}`)
	h := f.hold("POST", "/lock/acquire", bot.Token, `{"message":"x"}`)
	if w := f.call("DELETE", "/tokens/bot", apAdminToken, `{}`); w.Code != http.StatusOK {
		t.Fatalf("revoke: %d %s", w.Code, w.Body.String())
	}
	w := f.call("POST", "/approvals/"+h.ID+"/approve", apAdminToken, "")
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "requesting token") {
		t.Fatalf("approve: %d %s", w.Code, w.Body.String())
	}
	if _, err := f.s.Lock(context.Background()); err == nil {
		t.Fatal("the request ran")
	}
}

func TestAnApprovalHeldByAnAPITokenIsCarriedOutAsThatToken(t *testing.T) {
	f := newApprovalFixture(t, true, nil)
	bot := f.makeToken(`{"name":"bot","scope":"deploy","agent":true}`)
	h := f.hold("POST", "/lock/acquire", bot.Token, `{"message":"x"}`)
	if w := f.call("POST", "/approvals/"+h.ID+"/approve", apAdminToken, ""); w.Code != http.StatusOK {
		t.Fatalf("approve: %d %s", w.Code, w.Body.String())
	}
	if l, err := f.s.Lock(context.Background()); err != nil || !strings.Contains(l.Holder, "bot") {
		t.Fatalf("lock: %+v %v", l, err)
	}
}

func TestTheListShowsOriginAndWhenEachTokenWasLastUsed(t *testing.T) {
	f := newApprovalFixture(t, false, nil)
	f.makeToken(`{"name":"deployer","scope":"deploy","expires":"2027-02-16T00:00:00Z"}`)
	f.makeToken(`{"name":"idle","scope":"read"}`)
	rows := f.listTokens(apAdminToken)
	if len(rows) != 9 || rows["ci"].Origin != "config" || rows["ci"].Created != nil || rows["deployer"].Origin != "api" || rows["deployer"].Created == nil || !rows["deployer"].Created.Equal(f.now) {
		t.Fatalf("rows: %+v", rows)
	}
	if e := rows["deployer"].Expires; e == nil || e.Format("2006-01-02") != "2027-02-16" || rows["idle"].Expires != nil {
		t.Fatalf("expires: %+v", rows)
	}
	if !rows["claude"].Agent || rows["ci"].Agent {
		t.Fatalf("agent flags: %+v", rows)
	}
	// Alice has asked, so she has been used. The tokens that have not asked have not.
	if u := rows["alice"].LastUsed; u == nil || !u.Equal(f.now) || rows["idle"].LastUsed != nil || rows["ci"].LastUsed != nil {
		t.Fatalf("last used: %+v", rows)
	}
	f.now = f.now.Add(time.Hour)
	idle := f.makeToken(`{"name":"busy","scope":"read"}`)
	f.call("GET", "/audit", idle.Token, "")
	f.call("GET", "/audit", apDeployToken, "")
	rows = f.listTokens(apAdminToken)
	if u := rows["busy"].LastUsed; u == nil || !u.Equal(f.now) {
		t.Fatalf("a token of the API: %+v", rows["busy"])
	}
	if u := rows["ci"].LastUsed; u == nil || !u.Equal(f.now) {
		t.Fatalf("a token of the file: %+v", rows["ci"])
	}
}

func TestLastUsedIsWrittenAtMostOncePerTokenPerMinute(t *testing.T) {
	f := newApprovalFixture(t, false, nil)
	start := f.now
	f.call("GET", "/audit", apReadToken, "")
	use := func() time.Time {
		t.Helper()
		m, err := f.s.TokenUse(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		return m[Digest(apReadToken)]
	}
	if !use().Equal(start) {
		t.Fatalf("first use: %v", use())
	}
	f.now = start.Add(59 * time.Second)
	f.call("GET", "/audit", apReadToken, "")
	if !use().Equal(start) {
		t.Fatalf("a second request within the minute wrote: %v", use())
	}
	f.now = start.Add(61 * time.Second)
	f.call("GET", "/audit", apReadToken, "")
	if !use().Equal(f.now) {
		t.Fatalf("a request after the minute did not write: %v", use())
	}
}

func TestAnAgentWithoutStateAuthenticatesFromTheFileAlone(t *testing.T) {
	c := Config{Project: "demo", Environment: "lab", Tokens: []Token{{Name: "ci", Digest: Digest(apDeployToken), Scope: "deploy"}}}
	src := newTokenSource(c, nil)
	r := newGet("/v1/projects/demo/environments/lab/apps", apDeployToken)
	if e, status := src.identify(r); status != http.StatusOK || e.Name != "ci" {
		t.Fatalf("identify: %+v %d", e, status)
	}
	if _, status := src.identify(newGet("/v1/projects/demo/environments/lab/apps", apReadToken)); status != http.StatusUnauthorized {
		t.Fatalf("unknown: %d", status)
	}
}

func TestConfigNamesAreValidatedAndUnique(t *testing.T) {
	base := Config{Project: "demo", Environment: "lab", Listen: "127.0.0.1:9999"}
	tok := func(name, token string) Token { return Token{Name: name, Digest: Digest(token), Scope: "admin"} }
	for name, tokens := range map[string][]Token{
		"a bad name":    {tok("has space", apAdminToken)},
		"a duplicate":   {tok("a", apAdminToken), tok("a", apDeployToken)},
		"a name with :": {tok("token:abc", apAdminToken)},
	} {
		c := base
		c.Tokens = tokens
		if err := c.Validate(); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	c := base
	c.Tokens = []Token{tok("", apAdminToken), tok("", apDeployToken), tok("ci", apReadToken)}
	if err := c.Validate(); err != nil {
		t.Fatalf("unnamed tokens: %v", err)
	}
}

func TestTheExpiryCheckWarnsFourteenDaysBefore(t *testing.T) {
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	expires := func(d time.Duration) state.Token {
		e := now.Add(d)
		return state.Token{Name: "ci", Expires: &e}
	}
	day := 24 * time.Hour
	if r := tokenResult(expires(60*day), now); r.State != monitor.OK || !strings.HasPrefix(r.Detail, "expires 2027-01-17") {
		t.Fatalf("far: %+v", r)
	}
	if r := tokenResult(expires(15*day), now); r.State != monitor.OK {
		t.Fatalf("15 days: %+v", r)
	}
	r := tokenResult(expires(13*day+12*time.Hour), now)
	if r.State != monitor.Warning || r.Detail != "token ci expires in 13d (2026-12-02): create a new one with berthy token create" {
		t.Fatalf("13 days: %+v", r)
	}
	if r := tokenResult(expires(-time.Hour), now); r.State != monitor.Warning || !strings.Contains(r.Detail, "expired on 2026-11-18") {
		t.Fatalf("expired: %+v", r)
	}
}

func TestTheMonitorChecksOnlyTokensThatExpire(t *testing.T) {
	f := newApprovalFixture(t, false, nil)
	f.makeToken(`{"name":"forever","scope":"read"}`)
	f.makeToken(`{"name":"short","scope":"read","expires":"2026-11-22T00:00:00Z"}`)
	w := &Monitoring{cfg: f.cfg, store: f.s, now: func() time.Time { return f.now }}
	checks, err := w.tokenChecks(context.Background())
	if err != nil || len(checks) != 1 || checks[0].Name() != "token:short" {
		t.Fatalf("checks: %v %v", checks, err)
	}
	if res := checks[0].Run(context.Background()); res.State != monitor.Warning || !strings.Contains(res.Detail, "token short expires in 5d (2026-11-22)") {
		t.Fatalf("result: %+v", res)
	}
}
