package agent

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"berthy/internal/deploy"
	"berthy/internal/state"
)

// An approvalGate holds the changes an AI agent's token asks for on a protected environment, and carries them out once a
// person approves them. serve is the handler that carries out an ordinary request; the gate calls it for the dry run that
// produces a plan and for the approved request itself, so the plan a person reviews is the plan the route's own dry run gives.
type approvalGate struct {
	c     Config
	d     *deploy.Controller
	serve func(http.ResponseWriter, *http.Request, deploy.Actor, []string)
}

var approvalLevels = map[string]int{"read": 1, "deploy": 2, "admin": 3}

// capture is a response writer that keeps what a handler answers, so the gate can read a plan or replay an answer.
type capture struct {
	header http.Header
	code   int
	body   bytes.Buffer
}

func newCapture() *capture { return &capture{header: http.Header{}} }

func (c *capture) Header() http.Header { return c.header }
func (c *capture) WriteHeader(code int) {
	if c.code == 0 {
		c.code = code
	}
}
func (c *capture) Write(p []byte) (int, error) {
	c.WriteHeader(http.StatusOK)
	return c.body.Write(p)
}

// status is the HTTP status the handler answered with.
func (c *capture) status() int {
	if c.code == 0 {
		return http.StatusOK
	}
	return c.code
}

// message is the message of an error answer, or "" when it has none.
func (c *capture) message() string {
	var e struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(c.body.Bytes(), &e) != nil {
		return ""
	}
	return e.Message
}

// replay writes the captured answer to w.
func (c *capture) replay(w http.ResponseWriter) {
	for k, v := range c.header {
		w.Header()[k] = v
	}
	w.WriteHeader(c.status())
	_, _ = w.Write(c.body.Bytes())
}

// restOf is the path after the environment, as the routes see it. It is nil for a path that is too short.
func restOf(path string) []string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) < 4 {
		return nil
	}
	return parts[3:]
}

// readBody reads a request body of at most 1 MiB and puts it back, so the handler can read it again.
func readBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	r.Body = io.NopCloser(bytes.NewReader(b))
	return b, nil
}

// withBody returns a copy of r for method and path with body as its body.
func withBody(r *http.Request, method, path string, body []byte) *http.Request {
	r2 := r.Clone(r.Context())
	r2.Method = method
	u := *r.URL
	u.Path, u.RawPath, u.RawQuery = path, "", ""
	r2.URL = &u
	r2.Body = io.NopCloser(bytes.NewReader(body))
	r2.ContentLength = int64(len(body))
	return r2
}

// supportsDryRun reports whether the route takes a dry_run in its body and answers it with the plan of the change.
func supportsDryRun(rest []string) bool {
	switch {
	case len(rest) == 1:
		switch rest[0] {
		case "deploys", "rollback", "restart", "scale", "exec", "prune", "remove", "destroy", "restore", "secrets":
			return true
		}
	case len(rest) == 2:
		return rest[0] == "ingress" && rest[1] == "apply" || rest[0] == "lock" || rest[0] == "secrets"
	case len(rest) == 3:
		return rest[0] == "operations" && rest[2] == "resolve"
	}
	return false
}

// isDryRun reports whether a request body asks for a dry run of a route that has one.
func isDryRun(rest []string, body []byte) bool {
	if !supportsDryRun(rest) {
		return false
	}
	var v struct {
		DryRun bool `json:"dry_run"`
	}
	_ = json.Unmarshal(body, &v)
	return v.DryRun
}

// withDryRun returns body with dry_run set to true. A body that is not a JSON object is returned as it is, and the route
// refuses it as it would any request.
func withDryRun(body []byte) []byte {
	var m map[string]json.RawMessage
	if len(bytes.TrimSpace(body)) == 0 || json.Unmarshal(body, &m) != nil || m == nil {
		return body
	}
	m["dry_run"] = json.RawMessage("true")
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}

// canonicalJSON returns b with its keys in a fixed order and no white space, so two plans compare by what they say.
func canonicalJSON(b []byte) (json.RawMessage, error) {
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return nil, err
	}
	return json.Marshal(v)
}

// plan returns the plan of a change: what the route's own dry run answers for the request, or for a route that has no dry
// run a short description of the action. When the route refuses the request, or cannot plan it, the refusal is returned as the
// captured answer and no plan.
func (g *approvalGate) plan(r *http.Request, actor deploy.Actor, rest []string, method, path string, body []byte) (json.RawMessage, *capture) {
	if !supportsDryRun(rest) {
		b, _ := json.Marshal(describe(rest))
		return b, nil
	}
	rec := newCapture()
	rec.header.Set("Content-Type", "application/json")
	g.serve(rec, withBody(r, method, path, withDryRun(body)), actor, rest)
	if rec.status() != http.StatusOK {
		return nil, rec
	}
	// A plan can repeat what the request holds, such as an exec command line, so a managed secret value is replaced before the
	// plan is stored, compared or shown. When the secrets cannot be read nothing that might hold one is kept.
	redact, ok := g.d.Redactor(r.Context())
	if !ok {
		failure := newCapture()
		writeError(failure, 503, "unavailable", "plan unavailable")
		return nil, failure
	}
	plan, err := redactPlan(rec.body.Bytes(), redact)
	if err != nil {
		failure := newCapture()
		writeError(failure, 503, "unavailable", "plan unavailable")
		return nil, failure
	}
	return plan, nil
}

// redactPlan returns the canonical form of a JSON plan with redact applied to every string in it, keys included.
func redactPlan(plan []byte, redact func(string) string) (json.RawMessage, error) {
	var v any
	if err := json.Unmarshal(plan, &v); err != nil {
		return nil, err
	}
	var walk func(any) any
	walk = func(x any) any {
		switch t := x.(type) {
		case string:
			return redact(t)
		case []any:
			for i := range t {
				t[i] = walk(t[i])
			}
			return t
		case map[string]any:
			out := make(map[string]any, len(t))
			for k, e := range t {
				out[redact(k)] = walk(e)
			}
			return out
		}
		return x
	}
	return json.Marshal(walk(v))
}

// protected reports whether the environment is protected, by the agent's configuration or by a protected deploy.
func (g *approvalGate) protected(ctx context.Context) (bool, error) {
	if g.c.Protected || g.d.Protected {
		return true, nil
	}
	return g.d.Store.Protected(ctx)
}

// hold answers a change that an agent token asked for, when the environment is protected: it stores the request as a pending
// approval and tells the agent so, and nothing changes. It returns false, and writes nothing, when the request is to be
// served as usual: the environment is not protected, or the request is a dry run.
func (g *approvalGate) hold(w http.ResponseWriter, r *http.Request, actor deploy.Actor, rest []string) bool {
	ctx := r.Context()
	protected, err := g.protected(ctx)
	if err != nil {
		writeError(w, 503, "state_unavailable", "state unavailable")
		return true
	}
	if !protected {
		return false
	}
	body, err := readBody(w, r)
	if err != nil {
		writeError(w, 400, "bad_request", "invalid request")
		return true
	}
	if isDryRun(rest, body) {
		return false
	}
	plan, refused := g.plan(r, actor, rest, r.Method, r.URL.Path, body)
	if refused != nil {
		refused.replay(w)
		return true
	}
	now := g.c.now().UTC().Truncate(time.Millisecond)
	held, err := g.d.Store.CreateApproval(ctx, state.Approval{
		ActorID: actor.ID, ActorName: actor.Name, Remote: actor.Remote, Method: r.Method, Path: r.URL.Path,
		Summary: summarize(rest, body), Plan: plan, Created: now, Expires: now.Add(g.c.approvalTTL()),
	}, body)
	switch {
	case errors.Is(err, state.ErrTooManyApprovals):
		writeError(w, 429, "too_many_approvals", "too many pending approvals")
		return true
	case err != nil:
		writeError(w, 503, "state_unavailable", "state unavailable")
		return true
	}
	w.Header().Set("Berthy-Approval", "pending")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(struct {
		ID      string          `json:"approval_id"`
		Expires time.Time       `json:"expires"`
		Plan    json.RawMessage `json:"plan"`
	}{held.ID, held.Expires, held.Plan})
	return true
}

// approvalView is what the approvals route shows of an approval. It never shows the request, which can hold a secret value.
type approvalView struct {
	ID        string          `json:"id"`
	Actor     string          `json:"actor"`
	Kind      string          `json:"kind"`
	Summary   string          `json:"summary"`
	Plan      json.RawMessage `json:"plan,omitempty"`
	Created   time.Time       `json:"created"`
	Expires   time.Time       `json:"expires"`
	Status    string          `json:"status"`
	DecidedBy string          `json:"decided_by,omitempty"`
	DecidedAt *time.Time      `json:"decided_at,omitempty"`
	Result    string          `json:"result,omitempty"`
}

func viewApproval(a state.Approval) approvalView {
	v := approvalView{ID: a.ID, Actor: a.ActorName, Kind: routeKind(restOf(a.Path)), Summary: a.Summary, Plan: a.Plan, Created: a.Created, Expires: a.Expires,
		Status: a.Status, DecidedBy: a.DecidedBy, Result: a.Result}
	if !a.DecidedAt.IsZero() {
		t := a.DecidedAt
		v.DecidedAt = &t
	}
	return v
}

// serveApprovals answers the approvals routes: the list, and a person's decision. rest is the path after the environment, and
// the caller has checked the scope and the method.
func (g *approvalGate) serveApprovals(w http.ResponseWriter, r *http.Request, actor deploy.Actor, scope string, rest []string) {
	ctx := r.Context()
	if len(rest) == 1 {
		// The plan can repeat a command line, so only a token that may deploy sees it.
		seesPlan := approvalLevels[scope] >= approvalLevels["deploy"]
		var redact func(string) string
		if seesPlan {
			var ok bool
			if redact, ok = g.d.Redactor(ctx); !ok {
				writeError(w, 503, "state_unavailable", "state unavailable")
				return
			}
		}
		status := r.URL.Query().Get("status")
		switch status {
		case "", "pending", "approved", "rejected", "expired", "failed":
		default:
			writeError(w, 400, "bad_request", "invalid status")
			return
		}
		rows, err := g.d.Store.Approvals(ctx, status, g.c.now().UTC())
		if err != nil {
			writeError(w, 503, "state_unavailable", "state unavailable")
			return
		}
		out := make([]approvalView, 0, len(rows))
		for _, a := range rows {
			v := viewApproval(a)
			v.Plan = nil
			if seesPlan {
				// A secret set after the request is hidden too.
				if plan, err := redactPlan(a.Plan, redact); err == nil {
					v.Plan = plan
				}
			}
			out = append(out, v)
		}
		_ = json.NewEncoder(w).Encode(out)
		return
	}
	if actor.Agent {
		// An agent token never decides, whatever its scope: the person is the check on the agent.
		writeError(w, 403, "forbidden", "an agent token cannot approve or reject")
		return
	}
	decider := state.Actor{ID: actor.ID, Name: actor.Name, Remote: actor.Remote}
	a, ok := g.pending(w, r, decider, rest[1])
	if !ok {
		return
	}
	if rest[2] == "reject" {
		g.reject(w, r, decider, a)
		return
	}
	g.approve(w, r, decider, a)
}

// pending loads an approval for a decision. It expires the approval when it is past its expiry, and answers when the
// approval cannot be decided.
func (g *approvalGate) pending(w http.ResponseWriter, r *http.Request, decider state.Actor, id string) (state.Approval, bool) {
	ctx := r.Context()
	a, err := g.d.Store.Approval(ctx, id)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		writeError(w, 404, "not_found", "not found")
		return a, false
	case err != nil:
		writeError(w, 503, "state_unavailable", "state unavailable")
		return a, false
	}
	if now := g.c.now().UTC(); a.Status == "pending" && !now.Before(a.Expires) {
		if _, err = g.d.Store.ExpireApprovals(ctx, decider, now); err != nil {
			writeError(w, 503, "state_unavailable", "state unavailable")
			return a, false
		}
		a.Status = "expired"
	}
	if a.Status != "pending" {
		refuseApproval(w, a.Status)
		return a, false
	}
	return a, true
}

// refuseApproval answers a decision on an approval that is not pending.
func refuseApproval(w http.ResponseWriter, status string) {
	if status == "expired" {
		writeError(w, 409, "approval_expired", "approval expired")
		return
	}
	writeError(w, 409, "approval_not_pending", "approval is not pending")
}

// decide records a decision and answers a refusal when the approval can no longer take it. It returns false after answering.
func (g *approvalGate) decide(w http.ResponseWriter, r *http.Request, decider state.Actor, id, decision, result string) bool {
	_, err := g.d.Store.DecideApproval(r.Context(), id, decider, decision, result, g.c.now().UTC().Truncate(time.Millisecond))
	switch {
	case errors.Is(err, state.ErrApprovalExpired):
		refuseApproval(w, "expired")
	case errors.Is(err, state.ErrApprovalNotPending):
		refuseApproval(w, "approved")
	case errors.Is(err, sql.ErrNoRows):
		writeError(w, 404, "not_found", "not found")
	case err != nil:
		writeError(w, 503, "state_unavailable", "state unavailable")
	default:
		return true
	}
	return false
}

func (g *approvalGate) reject(w http.ResponseWriter, r *http.Request, decider state.Actor, a state.Approval) {
	body, err := readBody(w, r)
	if err != nil {
		writeError(w, 400, "bad_request", "invalid request")
		return
	}
	var req struct {
		Reason string `json:"reason"`
	}
	if len(bytes.TrimSpace(body)) > 0 {
		if decodeBody(w, r, &req) != nil {
			writeError(w, 400, "bad_request", "invalid request")
			return
		}
		if req.Reason != "" && !validReason(req.Reason) {
			writeError(w, 400, "bad_request", "invalid reason")
			return
		}
	}
	if g.decide(w, r, decider, a.ID, "rejected", req.Reason) {
		_ = json.NewEncoder(w).Encode(map[string]string{"id": a.ID, "status": "rejected"})
	}
}

// requester is the actor an approval was requested by, from the configured tokens. ok is false when no token has the
// approval's fingerprint any more. The name and the address are the ones recorded at the request.
func (g *approvalGate) requester(a state.Approval) (deploy.Actor, string, bool) {
	for _, t := range g.c.Tokens {
		b, err := hex.DecodeString(t.Digest)
		if err != nil || len(b) != 32 {
			continue
		}
		var sum [32]byte
		copy(sum[:], b)
		actor := tokenActor(t, sum, a.Remote)
		if actor.ID == a.ActorID {
			actor.Name = a.ActorName
			return actor, t.Scope, true
		}
	}
	return deploy.Actor{}, "", false
}

// approve carries out the request an approval holds, as the agent that made it. It first plans the request again and refuses
// when the plan is not the one that was reviewed.
func (g *approvalGate) approve(w http.ResponseWriter, r *http.Request, decider state.Actor, a state.Approval) {
	ctx := r.Context()
	fail := func(message, result string) {
		if g.decide(w, r, decider, a.ID, "failed", result) {
			writeError(w, 409, "approval_failed", message)
		}
	}
	rest := restOf(a.Path)
	required, method, known := routeScope(rest, a.Method)
	actor, scope, found := g.requester(a)
	if !found || !known || method != a.Method || approvalLevels[scope] < approvalLevels[required] {
		fail("the requesting token no longer exists or no longer has the scope; ask for a new request", "the requesting token is gone or lost its scope")
		return
	}
	body, err := g.d.Store.ApprovalRequest(ctx, a.ID)
	if err != nil {
		writeError(w, 503, "state_unavailable", "state unavailable")
		return
	}
	plan, refused := g.plan(r, actor, rest, a.Method, a.Path, body)
	if refused != nil {
		fail("the plan can no longer be computed ("+safeApprovalText(refused.message())+"); ask for a new request", "the plan can no longer be computed")
		return
	}
	if stored, err := canonicalJSON(a.Plan); err != nil || !bytes.Equal(stored, plan) {
		fail("the plan changed since it was requested; ask for a new request", "the plan changed since it was requested")
		return
	}
	if !g.decide(w, r, decider, a.ID, "approved", "") {
		return
	}
	// The request runs as the agent that made it, so the operation and its audit rows name the agent; the approver is named
	// beside it.
	actor.Name = a.ActorName + " (approved by " + decider.Name + ")"
	rec := newCapture()
	rec.header.Set("Content-Type", "application/json")
	g.serve(rec, withBody(r, a.Method, a.Path, body), actor, rest)
	status, result := "approved", approvalResult(rec)
	if rec.status() < 200 || rec.status() > 299 {
		status = "failed"
	}
	if err := g.d.Store.FinishApproval(context.WithoutCancel(ctx), a.ID, status, result); err != nil {
		// The request has run; only its record is missing. The answer below still tells the approver what happened.
		w.Header().Set("Berthy-Approval-Record", "unavailable")
	}
	rec.replay(w)
}

// approvalResult is the short outcome recorded with an approved request: the operation or run it started, or the refusal.
func approvalResult(c *capture) string {
	if c.status() < 200 || c.status() > 299 {
		return "HTTP " + strconv.Itoa(c.status()) + ": " + safeApprovalText(c.message())
	}
	dec := json.NewDecoder(bytes.NewReader(c.body.Bytes()))
	dec.UseNumber()
	var raw map[string]any
	if dec.Decode(&raw) == nil {
		if id, ok := raw["operation_id"].(string); ok && id != "" {
			return "operation " + id
		}
		for _, k := range []string{"run_id", "backup_run_id", "state_backup_run_id"} {
			if n, ok := raw[k].(json.Number); ok {
				return "run " + n.String()
			}
		}
	}
	return "done"
}

// safeApprovalText keeps a message that is shown or recorded to a short printable line.
func safeApprovalText(s string) string {
	s = cleanText(s, 200)
	if s == "" {
		return "no reason given"
	}
	return s
}

// cleanText returns s with every character that is not printable replaced by ?, cut to at most n characters.
func cleanText(s string, n int) string {
	var b strings.Builder
	count := 0
	for _, r := range s {
		if count == n {
			break
		}
		if r < 0x20 || r == 0x7f || r == 0xfffd || (r >= 0x80 && r < 0xa0) {
			r = '?'
		}
		b.WriteRune(r)
		count++
	}
	return b.String()
}

// shortVersion shortens an image digest to its first 12 characters, for a summary a person reads.
func shortVersion(v string) string {
	if h, ok := strings.CutPrefix(v, "sha256:"); ok && len(h) > 12 {
		return "sha256:" + h[:12]
	}
	return v
}

// summarize is the one line that says what a held request does, such as "deploy api 1.4.2". It carries no secret value.
func summarize(rest []string, body []byte) string {
	var b struct {
		App           string   `json:"app"`
		Version       string   `json:"version"`
		Role          string   `json:"role"`
		Replicas      *int     `json:"replicas"`
		TargetRelease int64    `json:"target_release"`
		Name          string   `json:"name"`
		Key           string   `json:"key"`
		Into          string   `json:"into"`
		Volumes       bool     `json:"volumes"`
		Command       []string `json:"command"`
	}
	_ = json.Unmarshal(body, &b)
	app := cleanText(b.App, 80)
	if b.Role != "" {
		app += "/" + cleanText(b.Role, 40)
	}
	switch routeKind(rest) {
	case "deploy":
		return strings.TrimSpace("deploy " + cleanText(b.App, 80) + " " + cleanText(shortVersion(b.Version), 80))
	case "rollback":
		if b.TargetRelease > 0 {
			return "rollback " + cleanText(b.App, 80) + " to release " + strconv.FormatInt(b.TargetRelease, 10)
		}
		return "rollback " + cleanText(b.App, 80)
	case "restart":
		return "restart " + app
	case "scale":
		if b.Replicas != nil {
			return "scale " + app + " to " + strconv.Itoa(*b.Replicas)
		}
		return "scale " + app
	case "exec":
		return "exec " + app
	case "prune":
		return "prune"
	case "remove":
		if b.Volumes {
			return "remove " + cleanText(b.App, 80) + " and its volumes"
		}
		return "remove " + cleanText(b.App, 80)
	case "destroy":
		return "destroy the environment"
	case "restore":
		return "restore " + cleanText(b.Key, 100) + " into " + cleanText(b.Into, 60)
	case "ingress apply":
		return "ingress apply"
	case "secret set":
		return "secret set " + cleanText(b.Name, 80)
	case "secret delete":
		return "secret delete " + cleanText(rest[1], 80)
	case "lock acquire", "lock release", "state backup run", "alerts test":
		return routeKind(rest)
	case "backup run", "job run":
		return routeKind(rest) + " " + cleanText(rest[1], 80)
	case "recovery resolve":
		return "recovery resolve " + cleanText(rest[1], 80)
	}
	return "change"
}

// routeKind names what a change route does. It is "" for a route that changes nothing.
func routeKind(rest []string) string {
	switch {
	case len(rest) == 1 && rest[0] == "deploys":
		return "deploy"
	case len(rest) == 1:
		switch rest[0] {
		case "rollback", "restart", "scale", "exec", "prune", "remove", "destroy", "restore":
			return rest[0]
		case "secrets":
			return "secret set"
		case "state-backup":
			return "state backup run"
		}
	case len(rest) == 2 && rest[0] == "ingress" && rest[1] == "apply":
		return "ingress apply"
	case len(rest) == 2 && rest[0] == "secrets":
		return "secret delete"
	case len(rest) == 2 && rest[0] == "lock" && (rest[1] == "acquire" || rest[1] == "release"):
		return "lock " + rest[1]
	case len(rest) == 2 && rest[0] == "alerts" && rest[1] == "test":
		return "alerts test"
	case len(rest) == 3 && rest[0] == "backups" && rest[2] == "run":
		return "backup run"
	case len(rest) == 3 && rest[0] == "jobs" && rest[2] == "run":
		return "job run"
	case len(rest) == 3 && rest[0] == "operations" && rest[2] == "resolve":
		return "recovery resolve"
	}
	return ""
}

// describe is the plan of a held request whose route has no dry run: what it does, and to what.
func describe(rest []string) map[string]string {
	out := map[string]string{"action": routeKind(rest)}
	switch out["action"] {
	case "backup run", "job run":
		out["target"] = rest[1]
	}
	return out
}
