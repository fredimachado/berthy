package agent

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"berthy/internal/api"
	"berthy/internal/deploy"
	"berthy/internal/state"
)

// An approvalGate holds the changes an AI agent's token asks for on a protected environment, and carries them out once a
// person approves them. serve is the handler that carries out an ordinary request; the gate calls it for the dry run that
// produces a plan and for the approved request itself, so the plan a person reviews is the plan the route's own dry run gives.
type approvalGate struct {
	c Config
	d *deploy.Controller
	// tokens is every token that may authenticate now, which is who a held request can still be carried out for.
	tokens *tokenSource
	serve  func(http.ResponseWriter, *http.Request, deploy.Actor, []string)
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

// restOf splits a path relative to the environment, as mount gives it to a handler, into segments for route matching.
// It is nil for the environment's own path.
func restOf(path string) []string {
	path = strings.TrimRight(path, "/")
	if path == "" {
		return nil
	}
	return strings.Split(path, "/")
}

// readBody reads a request body of at most 1 MiB and puts it back, so the handler can read it again.
func readBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, api.MaxRequestBody))
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
func supportsDryRun(rest []string, method string) bool {
	row := findEnvRoute(rest, method, true)
	return row != nil && row.gateDryRun
}

// isDryRun reports whether a request body asks for a dry run of a route that has one.
func isDryRun(rest []string, method string, body []byte) bool {
	if !supportsDryRun(rest, method) {
		return false
	}
	var v dryRunBody
	_ = json.Unmarshal(body, &v)
	return v.DryRun
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
	if !supportsDryRun(rest, method) {
		b, _ := json.Marshal(describe(rest, method))
		return b, nil
	}
	rec := newCapture()
	rec.header.Set("Content-Type", "application/json")
	g.serve(rec, asPlan(withBody(r, method, path, body)), actor, rest)
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
	managedRedact := redact
	tokens := regexp.MustCompile(g.c.ID().TokenExpression())
	redact = func(s string) string { return managedRedact(tokens.ReplaceAllString(s, "[redacted]")) }
	plan, err := redactApprovalPlan(rest, method, rec.body.Bytes(), redact)
	if err != nil {
		failure := newCapture()
		writeError(failure, 503, "unavailable", "plan unavailable")
		return nil, failure
	}
	return plan, nil
}

// redactApprovalPlan preserves validated generation decisions while hiding dynamic metadata.
func redactApprovalPlan(rest []string, method string, plan []byte, redact func(string) string) (json.RawMessage, error) {
	kind := routeKind(rest, method)
	if kind == "deploy" {
		var decision struct {
			Image string `json:"image"`
			Tag   string `json:"tag"`
		}
		if json.Unmarshal(plan, &decision) != nil || redact(decision.Image) != decision.Image || redact(decision.Tag) != decision.Tag {
			return nil, errors.New("image selection unavailable")
		}
		return redactDeployPlan(plan, redact)
	}
	if kind != "secret generate" && kind != "secret generate declared" {
		return redactPlan(plan, redact)
	}
	dec := json.NewDecoder(bytes.NewReader(plan))
	dec.DisallowUnknownFields()
	var value any
	if kind == "secret generate" {
		var p struct {
			api.SecretGeneratePlan
			Exists *bool `json:"exists"`
		}
		if err := dec.Decode(&p); err != nil || p.Exists == nil || p.Action != "secret generate" || !slices.Contains(api.GenerateFormats, p.Format) {
			return nil, errors.New("invalid generation plan")
		}
		p.SecretGeneratePlan.Exists = *p.Exists
		p.Name = redact(p.Name)
		if p.App != "" {
			p.App = redact(p.App)
		}
		if p.Volume != "" {
			p.Volume = redact(p.Volume)
		}
		value = p.SecretGeneratePlan
	} else {
		var p api.SecretsDeclared
		if err := dec.Decode(&p); err != nil || p.Action != "secret generate declared" || p.Secrets == nil {
			return nil, errors.New("invalid generation plan")
		}
		for i := range p.Secrets {
			s := &p.Secrets[i]
			if (s.Outcome != api.DeclaredGenerate && s.Outcome != api.DeclaredKeep && s.Outcome != api.DeclaredRefuse) || !slices.Contains(api.GenerateFormats, s.Format) {
				return nil, errors.New("invalid generation plan")
			}
			s.Name = redact(s.Name)
			if s.App != "" {
				s.App = redact(s.App)
			}
			if s.Volume != "" {
				s.Volume = redact(s.Volume)
			}
		}
		value = p
	}
	if dec.Decode(&struct{}{}) != io.EOF {
		return nil, errors.New("invalid generation plan")
	}
	b, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return canonicalJSON(b)
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

// redactDeployPlan keeps protocol field names while redacting values and user-defined map keys.
func redactDeployPlan(plan []byte, redact func(string) string) (json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(plan))
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err != nil {
		return nil, err
	}
	var walk func(any, reflect.Type) (any, error)
	walk = func(v any, typ reflect.Type) (any, error) {
		for typ != nil && typ.Kind() == reflect.Pointer {
			typ = typ.Elem()
		}
		switch item := v.(type) {
		case string:
			return redact(item), nil
		case []any:
			var elem reflect.Type
			if typ != nil && (typ.Kind() == reflect.Slice || typ.Kind() == reflect.Array) {
				elem = typ.Elem()
			}
			for i := range item {
				var err error
				item[i], err = walk(item[i], elem)
				if err != nil {
					return nil, err
				}
			}
			return item, nil
		case map[string]any:
			out := make(map[string]any, len(item))
			for key, entry := range item {
				name := redact(key)
				var child reflect.Type
				if typ != nil && typ.Kind() == reflect.Map {
					child = typ.Elem()
				} else if typ != nil && typ.Kind() == reflect.Struct {
					for i := 0; i < typ.NumField(); i++ {
						field := typ.Field(i)
						fieldName := strings.Split(field.Tag.Get("json"), ",")[0]
						if fieldName == "" {
							fieldName = field.Name
						}
						if fieldName != "-" && key == fieldName {
							name, child = key, field.Type
							break
						}
					}
				}
				if _, exists := out[name]; exists {
					return nil, errors.New("redacted plan keys collide")
				}
				redacted, err := walk(entry, child)
				if err != nil {
					return nil, err
				}
				out[name] = redacted
			}
			return out, nil
		}
		return v, nil
	}
	redacted, err := walk(value, reflect.TypeOf(deploy.Plan{}))
	if err != nil {
		return nil, err
	}
	return json.Marshal(redacted)
}

// protected reports whether the environment is protected, by the agent's configuration or by a protected deploy.
func (g *approvalGate) protected(ctx context.Context) (bool, error) {
	if g.c.Protected || g.d.Protected {
		return true, nil
	}
	return g.d.Store.Protected(ctx)
}

// hold answers a change that an agent token asked for, when the environment is protected: it stores the request as a pending
// approval and tells the agent so, and nothing changes. It serves a dry run as a planning call. It returns false and writes
// nothing only when the environment is not protected.
func (g *approvalGate) hold(w http.ResponseWriter, r *http.Request, actor deploy.Actor, rest []string) bool {
	ctx := r.Context()
	manifestProtected := false
	if len(rest) == 1 && rest[0] == "deploys" {
		body, err := readBody(w, r)
		if err != nil {
			writeError(w, 400, "bad_request", "invalid request")
			return true
		}
		var req deploy.Request
		if err := decodeBody(w, withBody(r, r.Method, r.URL.Path, body), &req); err != nil {
			writeError(w, 400, "bad_request", "invalid request")
			return true
		}
		if err := g.d.CheckSubmitted(ctx, deploy.Submitted{Manifest: req.Manifest}); err != nil {
			operationError(w, err)
			return true
		}
		manifestProtected = req.Manifest.Protected
	}
	protected, err := g.protected(ctx)
	if err != nil {
		writeError(w, 503, "state_unavailable", "state unavailable")
		return true
	}
	if !protected && !manifestProtected {
		return false
	}
	body, err := readBody(w, r)
	if err != nil {
		writeError(w, 400, "bad_request", "invalid request")
		return true
	}
	if isDryRun(rest, r.Method, body) {
		g.serve(w, asPlan(r), actor, rest)
		return true
	}
	plan, refused := g.plan(r, actor, rest, r.Method, r.URL.Path, body)
	if refused != nil {
		refused.replay(w)
		return true
	}
	now := g.c.now().UTC().Truncate(time.Millisecond)
	held, err := g.d.Store.CreateApproval(ctx, state.Approval{
		ActorID: actor.ID, ActorName: actor.Name, Remote: actor.Remote, Method: r.Method, Path: r.URL.Path,
		Summary: summarize(rest, r.Method, body), Plan: plan, Created: now, Expires: now.Add(g.c.approvalTTL()),
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
	v := approvalView{ID: a.ID, Actor: a.ActorName, Kind: routeKind(restOf(a.Path), a.Method), Summary: a.Summary, Plan: a.Plan, Created: a.Created, Expires: a.Expires,
		Status: a.Status, DecidedBy: a.DecidedBy, Result: a.Result}
	if !a.DecidedAt.IsZero() {
		t := a.DecidedAt
		v.DecidedAt = &t
	}
	return v
}

func (rt *routeRuntime) handleApprovals(call routeCall) {
	w, r, scope := call.w, call.r, call.scope
	ctx, g := r.Context(), rt.gate
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
			if plan, err := redactApprovalPlan(restOf(a.Path), a.Method, a.Plan, redact); err == nil {
				v.Plan = plan
			}
		}
		out = append(out, v)
	}
	_ = json.NewEncoder(w).Encode(out)
}

func (rt *routeRuntime) handleApprove(call routeCall) {
	decider, a, ok := rt.gate.pendingDecision(call)
	if ok {
		rt.gate.approve(call.w, call.r, decider, a)
	}
}

func (rt *routeRuntime) handleReject(call routeCall, read readRouteBody[rejectBody]) {
	decider, a, ok := rt.gate.pendingDecision(call)
	if ok {
		rt.gate.reject(call.w, call.r, decider, a, read)
	}
}

func (g *approvalGate) pendingDecision(call routeCall) (state.Actor, state.Approval, bool) {
	if call.actor.Agent {
		// An agent token never decides, whatever its scope: the person is the check on the agent.
		writeError(call.w, 403, "forbidden", "an agent token cannot approve or reject")
		return state.Actor{}, state.Approval{}, false
	}
	decider := call.stateActor()
	a, ok := g.pending(call.w, call.r, decider, call.rest[1])
	return decider, a, ok
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

func (g *approvalGate) reject(w http.ResponseWriter, r *http.Request, decider state.Actor, a state.Approval, read readRouteBody[rejectBody]) {
	req, err := read()
	if err != nil {
		writeError(w, 400, "bad_request", "invalid request")
		return
	}
	if req.Reason != "" && !validReason(req.Reason) {
		writeError(w, 400, "bad_request", "invalid reason")
		return
	}
	if g.decide(w, r, decider, a.ID, "rejected", req.Reason) {
		_ = json.NewEncoder(w).Encode(map[string]string{"id": a.ID, "status": "rejected"})
	}
}

// requester is the actor an approval was requested by, from the tokens that work now, whichever source defines them. ok is
// false when no such token has the approval's fingerprint any more: it was removed from the configuration, revoked or has
// expired. err is set when the tokens could not be read. The name and the address are the ones recorded at the request.
func (g *approvalGate) requester(ctx context.Context, a state.Approval) (actor deploy.Actor, scope string, ok bool, err error) {
	entries, err := g.tokens.entries(ctx)
	if err != nil {
		return deploy.Actor{}, "", false, err
	}
	now := g.c.now()
	for _, e := range entries {
		sum, valid := e.sum()
		if !valid || e.expired(now) {
			continue
		}
		actor := tokenActor(e.Token, sum, a.Remote)
		if actor.ID == a.ActorID {
			actor.Name = a.ActorName
			return actor, e.Scope, true, nil
		}
	}
	return deploy.Actor{}, "", false, nil
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
	actor, scope, found, err := g.requester(ctx, a)
	if err != nil {
		writeError(w, 503, "state_unavailable", "state unavailable")
		return
	}
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
	stored, err := canonicalJSON(a.Plan)
	if kind := routeKind(rest, a.Method); err == nil && (kind == "secret generate" || kind == "secret generate declared") {
		_, err = redactApprovalPlan(rest, a.Method, a.Plan, func(s string) string { return s })
	}
	if err != nil || !bytes.Equal(stored, plan) {
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

// routeKind names what a change route does. It is empty for a route that changes nothing.
func routeKind(rest []string, method string) string {
	if row := findEnvRoute(rest, method, true); row != nil {
		return row.kind
	}
	return ""
}

// describe is the plan of a held request whose route has no dry run.
func describe(rest []string, method string) map[string]string {
	out := map[string]string{"action": ""}
	if row := findEnvRoute(rest, method, true); row != nil {
		out["action"] = row.kind
		if row.describeTarget {
			out["target"] = rest[1]
		}
	}
	return out
}
