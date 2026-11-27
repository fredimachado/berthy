package agent

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"time"
	"unicode"
	"unicode/utf8"

	"berthy/internal/api"
	"berthy/internal/deploy"
	"berthy/internal/engine"
	"berthy/internal/jobs"
	"berthy/internal/manifest"
	"berthy/internal/metrics"
	"berthy/internal/state"
	"berthy/internal/telemetry"
)

func OpenDeployment(c Config, e *engine.Engine) (*deploy.Controller, error) {
	if c.StateDir == "" && c.KeyFile == "" {
		return nil, nil
	}
	s, err := state.Open(c.StateDir, c.KeyFile, c.Environment)
	if err != nil {
		return nil, err
	}
	controller := &deploy.Controller{Store: s, Environment: c.Environment, Project: c.Project, Protected: c.Protected, CloudflareURL: c.CloudflareAPIURL, StateDir: c.StateDir}
	controller.ProbeSource = uptimeProbeSource
	controller.Compose = engine.Compose{Command: c.composeCommand(), Host: dockerHost(c.DockerSocket)}
	if e != nil {
		controller.Engine = e
	}
	return controller, nil
}
func HandlerWithRuntime(c Config, e *engine.Engine, col *metrics.Collector, d *deploy.Controller) http.Handler {
	return NewDeploymentHandler(c, e, col, d)
}
func NewDeploymentHandler(c Config, e *engine.Engine, col *metrics.Collector, d *deploy.Controller) http.Handler {
	return NewDeploymentHandlerWithTelemetry(c, e, col, d, nil)
}

// NewDeploymentHandlerWithTelemetry is NewDeploymentHandler for an agent that exports telemetry, so that the doctor reports
// the export. tel may be nil.
func NewDeploymentHandlerWithTelemetry(c Config, e *engine.Engine, col *metrics.Collector, d *deploy.Controller, tel *telemetry.Exporter) http.Handler {
	return NewDeploymentHandlerWithJobs(c, e, col, d, tel, nil)
}

// NewDeploymentHandlerWithJobs is NewDeploymentHandlerWithTelemetry for an agent that runs jobs on demand. sched may be
// nil, in which case a request to run a job is refused as unavailable.
func NewDeploymentHandlerWithJobs(c Config, e *engine.Engine, col *metrics.Collector, d *deploy.Controller, tel *telemetry.Exporter, sched *jobs.Scheduler) http.Handler {
	return NewDeploymentHandlerWithBackups(c, e, col, d, tel, sched, nil)
}

// NewDeploymentHandlerWithBackups is NewDeploymentHandlerWithJobs for an agent that backs up volumes. backups may be nil, in
// which case the backup routes answer that backups are unavailable.
func NewDeploymentHandlerWithBackups(c Config, e *engine.Engine, col *metrics.Collector, d *deploy.Controller, tel *telemetry.Exporter, sched *jobs.Scheduler, backups *Backups) http.Handler {
	return newDeploymentHandler(c, e, col, d, tel, sched, backups, newLimits(), nil)
}

// newDeploymentHandler is NewDeploymentHandlerWithBackups with the stream limits of the hosted environment the handler serves.
func newDeploymentHandler(c Config, e *engine.Engine, col *metrics.Collector, d *deploy.Controller, tel *telemetry.Exporter, sched *jobs.Scheduler, backups *Backups, lim *limits, keep func() func(map[string]string) bool) http.Handler {
	tokens := newTokenSource(c, storeOf(d))
	gate := &approvalGate{c: c, d: d, tokens: tokens}
	rt := &routeRuntime{c: c, e: e, collector: col, d: d, tel: tel, sched: sched, backups: backups,
		lim: lim, keep: keep, tokens: tokens, gate: gate}
	gate.serve = rt.serveOrdinary
	// The handler sees the path after the environment, as mount gives it.
	return serveAPI(c, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rest := restOf(r.URL.Path)
		row := findEnvRoute(rest, r.Method, false)
		if row == nil || row.lane == legacyReadRoute {
			rt.serveRead(routeCall{w: w, r: r, rest: rest, row: row})
			return
		}
		required, method := row.scope, row.method
		w.Header().Set("Content-Type", "application/json")
		actor, scope, status := authenticate(tokens, r)
		if status != http.StatusOK {
			refuseToken(w, status)
			return
		}
		// An agent token never makes or revokes a token, whatever its scope, so an AI agent cannot mint access. The refusal comes
		// before the scope and the hold: the request is never held for a person to approve, and it is refused on an unprotected
		// environment too.
		if rest[0] == "tokens" && r.Method != "GET" && actor.Agent {
			writeError(w, 403, "forbidden", "an agent token cannot manage tokens")
			return
		}
		levels := map[string]int{"read": 1, "deploy": 2, "admin": 3}
		if levels[scope] < levels[required] {
			writeError(w, 403, "forbidden", "forbidden")
			return
		}
		if r.Method != method {
			writeError(w, 405, "method_not_allowed", "method not allowed")
			return
		}
		if rest[0] == "images" && actor.Agent {
			writeError(w, 403, "forbidden", "forbidden")
			return
		}
		if d == nil || d.Store == nil {
			writeError(w, 503, "state_unavailable", "state unavailable")
			return
		}
		call := routeCall{w: w, r: r, actor: actor, scope: scope, rest: rest, row: row}
		if row.lane == approvalRoute || row.lane == sessionRoute {
			row.handle(rt, call)
			return
		}
		// The state key is a person's to replace, on any environment: an agent token is refused, never held for approval.
		if actor.Agent && rest[0] == "key" {
			writeError(w, 403, "forbidden", "an agent token cannot rotate the state key")
			return
		}
		// An agent token on a protected environment does not change it: the request is held until a person approves it.
		if actor.Agent && method != "GET" && gate.hold(w, r, actor, rest) {
			return
		}
		row.handle(rt, call)
	}))
}

// serveOrdinary resolves a route again when the approval gate plans or replays a request.
func (rt *routeRuntime) serveOrdinary(w http.ResponseWriter, r *http.Request, actor deploy.Actor, rest []string) {
	row := findEnvRoute(rest, r.Method, false)
	if row == nil || row.method != r.Method || row.lane != ordinaryRoute {
		writeError(w, 501, "unavailable", "operation unavailable")
		return
	}
	row.handle(rt, routeCall{w: w, r: r, actor: actor, rest: rest, row: row})
}

func (rt *routeRuntime) handleLockStatus(call routeCall) {
	w, ctx, d := call.w, call.r.Context(), rt.d
	l, err := d.Store.Lock(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		json.NewEncoder(w).Encode(map[string]bool{"locked": false}) //nolint:errcheck // This final HTTP response has no remaining error-reporting channel.
		return
	}
	if err != nil {
		operationError(w, err)
		return
	}
	json.NewEncoder(w).Encode(map[string]any{"locked": true, "holder": l.Holder, "reason": l.Reason, "acquired": l.Acquired, "operation_id": l.OperationID}) //nolint:errcheck // This final HTTP response has no remaining error-reporting channel.
}

func (rt *routeRuntime) handleLockAcquire(call routeCall, read readRouteBody[lockBody]) {
	rt.changeLock(call, read, func(req lockBody) error {
		return rt.d.Store.AcquireManual(call.r.Context(), call.stateActor(), call.actor.Name, req.Message)
	})
}

func (rt *routeRuntime) handleLockRelease(call routeCall, read readRouteBody[lockBody]) {
	rt.changeLock(call, read, func(lockBody) error {
		return rt.d.Store.ReleaseManual(call.r.Context(), call.stateActor())
	})
}

func (rt *routeRuntime) changeLock(call routeCall, read readRouteBody[lockBody], change func(lockBody) error) {
	w, ctx, d := call.w, call.r.Context(), rt.d
	req, err := read()
	if err != nil {
		writeError(w, 400, "bad_request", "invalid request")
		return
	}
	if req.DryRun {
		_, lockErr := d.Store.Lock(ctx)
		if lockErr != nil && !errors.Is(lockErr, sql.ErrNoRows) {
			operationError(w, lockErr)
			return
		}
		plan := map[string]any{"action": call.row.kind, "currently_locked": lockErr == nil}
		if req.Message != "" {
			plan["message"] = req.Message
		}
		json.NewEncoder(w).Encode(plan) //nolint:errcheck // This final HTTP response has no remaining error-reporting channel.
		return
	}
	err = change(req)
	if err != nil {
		operationError(w, err)
		return
	}
	json.NewEncoder(w).Encode(map[string]bool{"ok": true}) //nolint:errcheck // This final HTTP response has no remaining error-reporting channel.
}

func (rt *routeRuntime) handleSecretList(call routeCall) {
	w, ctx, d := call.w, call.r.Context(), rt.d
	rows, err := d.Store.Secrets(ctx)
	if err != nil {
		operationError(w, err)
		return
	}
	out := make([]map[string]string, 0, len(rows))
	for _, v := range rows {
		out = append(out, map[string]string{"name": v.Name, "registry": v.Registry})
	}
	json.NewEncoder(w).Encode(out) //nolint:errcheck // This final HTTP response has no remaining error-reporting channel.
}

func (rt *routeRuntime) handleSecretSet(call routeCall, read readRouteBody[secretBody]) {
	rt.changeSecret(call, read, false)
}

func (rt *routeRuntime) handleSecretDelete(call routeCall, read readRouteBody[secretBody]) {
	rt.changeSecret(call, read, true)
}

func (rt *routeRuntime) changeSecret(call routeCall, read readRouteBody[secretBody], deleting bool) {
	w, ctx, d := call.w, call.r.Context(), rt.d
	sa := call.stateActor()
	req, err := read()
	if err != nil {
		writeError(w, 400, "bad_request", "invalid request")
		return
	}
	if deleting {
		req.Name = call.rest[1]
	}
	if req.Name == "" {
		writeError(w, 400, "bad_request", "name required")
		return
	}
	if req.Name == manifest.TunnelTokenSecret {
		writeError(w, 400, "bad_request", manifest.TunnelTokenSecret+" is reserved for managed ingress")
		return
	}
	// A set that names no registry keeps the one the stored secret has, so rotating a registry password does not turn
	// it into a plain secret.
	exists := false
	if req.DryRun || (!deleting && req.Registry == "") {
		stored, listErr := d.Store.Secrets(ctx)
		if listErr != nil {
			operationError(w, listErr)
			return
		}
		for _, info := range stored {
			if info.Name == req.Name {
				exists = true
				if !deleting && req.Registry == "" {
					req.Registry = info.Registry
				}
			}
		}
	}
	if req.DryRun {
		plan := map[string]any{"action": call.row.kind, "name": req.Name, "exists": exists}
		if !deleting {
			plan["registry"] = req.Registry
			if req.Registry != "" {
				if names, listErr := d.RegistryServices(ctx, req.Registry); listErr != nil {
					plan["services_error"] = listErr.Error()
				} else {
					plan["services"] = append([]string{}, names...)
				}
			}
		}
		json.NewEncoder(w).Encode(plan) //nolint:errcheck // This final HTTP response has no remaining error-reporting channel.
		return
	}
	if deleting {
		err = d.Store.DeleteSecret(ctx, sa, req.Name)
	} else {
		err = d.Store.PutSecret(ctx, sa, state.SecretInfo{Name: req.Name, Registry: req.Registry}, []byte(req.Value))
	}
	if err != nil {
		operationError(w, err)
		return
	}
	if deleting || req.Registry == "" {
		json.NewEncoder(w).Encode(map[string]bool{"ok": true}) //nolint:errcheck // This final HTTP response has no remaining error-reporting channel.
		return
	}
	// A registry password: Swarm keeps its own copy with each service that pulls from the registry.
	answer := map[string]any{"ok": true, "registry": req.Registry}
	refreshed, refreshErr := d.RefreshRegistryAuth(ctx, req.Registry)
	outcome := "succeeded"
	if refreshErr != nil {
		answer["services_error"] = refreshErr.Error()
		outcome = "failed"
	} else {
		answer["services"] = append([]deploy.RegistryRefresh{}, refreshed...)
		for _, s := range refreshed {
			if !s.Updated {
				outcome = "failed"
			}
		}
	}
	if refreshErr != nil || len(refreshed) > 0 {
		d.Store.RecordAudit(ctx, sa, "registry_auth_refresh:"+req.Registry, outcome)
	}
	json.NewEncoder(w).Encode(answer) //nolint:errcheck // This final HTTP response has no remaining error-reporting channel.
}

func (rt *routeRuntime) handleChecks(call routeCall) {
	w, ctx, d := call.w, call.r.Context(), rt.d
	rows, err := d.Store.Checks(ctx)
	if err != nil {
		writeError(w, 503, "state_unavailable", "state unavailable")
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		out = append(out, map[string]any{"name": row.Name, "state": row.State, "detail": row.Detail, "since": row.Since})
	}
	json.NewEncoder(w).Encode(out) //nolint:errcheck // This final HTTP response has no remaining error-reporting channel.
}

func (rt *routeRuntime) handleAlertsTest(call routeCall) {
	w, ctx, c, d := call.w, call.r.Context(), rt.c, rt.d
	sa := call.stateActor()
	results, sendErr := sendTestAlert(ctx, c, d.Store)
	outcome := "succeeded"
	if sendErr != nil {
		outcome = "failed"
	}
	for _, res := range results {
		if !res.OK {
			outcome = "failed"
		}
	}
	if err := d.Store.RecordAudit(ctx, sa, "alerts_test", outcome); err != nil {
		writeError(w, 503, "state_unavailable", "state unavailable")
		return
	}
	if sendErr != nil {
		alertError(w, sendErr)
		return
	}
	json.NewEncoder(w).Encode(map[string][]channelResult{"channels": results}) //nolint:errcheck // This final HTTP response has no remaining error-reporting channel.
}

func (rt *routeRuntime) handleAdmission(call routeCall, read readRouteBody[deploy.Request]) {
	w := call.w
	req, err := read()
	if err != nil {
		writeError(w, 400, "bad_request", "invalid request")
		return
	}
	req.Kind = call.row.kind
	if (req.Adopt || req.RevertSchema || req.ReplaceExisting || req.AllowRecreate) && !call.actor.Admin {
		writeError(w, 403, "forbidden", "forbidden")
		return
	}
	rt.planOrAdmit(call, req)
}

func (rt *routeRuntime) handleApply(call routeCall, read readRouteBody[deploy.ApplyRequest]) {
	w := call.w
	req, err := read()
	if err != nil {
		writeError(w, 400, "bad_request", "invalid request")
		return
	}
	result, err := rt.d.ApplySettings(call.r.Context(), call.actor, req)
	if err != nil {
		operationError(w, err)
		return
	}
	json.NewEncoder(w).Encode(result) //nolint:errcheck // This final HTTP response has no remaining error-reporting channel.
}

func (rt *routeRuntime) handleRestore(call routeCall, read readRouteBody[restoreBody]) {
	w := call.w
	body, err := read()
	if err != nil {
		writeError(w, 400, "bad_request", "invalid request")
		return
	}
	req := deploy.Request{Kind: "restore", RestoreKey: body.Key, RestoreInto: body.Into, App: body.App, Service: body.Service, Role: body.Role,
		Command: body.Command, Timeout: body.Timeout, DryRun: body.DryRun, RequestID: body.RequestID}
	if !req.DryRun && req.RequestID == "" {
		var random [16]byte
		if _, err := rand.Read(random[:]); err != nil {
			writeError(w, 503, "unavailable", "operation unavailable")
			return
		}
		req.RequestID = hex.EncodeToString(random[:])
	}
	rt.planOrAdmit(call, req)
}

func (rt *routeRuntime) handleIngressApply(call routeCall, read readRouteBody[deploy.Request]) {
	w := call.w
	req, err := read()
	if err != nil {
		writeError(w, 400, "bad_request", "invalid request")
		return
	}
	req.Kind = "ingress"
	req.App = manifest.ConnectorAppName
	if req.ReplaceExisting && !call.actor.Admin {
		writeError(w, 403, "forbidden", "forbidden")
		return
	}
	rt.planOrAdmit(call, req)
}

func (rt *routeRuntime) handleIngressStatus(call routeCall) {
	status, err := rt.d.IngressStatus(call.r.Context())
	if err != nil {
		writeError(call.w, 503, "unavailable", "ingress status unavailable")
		return
	}
	json.NewEncoder(call.w).Encode(status)
}

func (rt *routeRuntime) planOrAdmit(call routeCall, req deploy.Request) {
	w, r, actor := call.w, call.r, call.actor
	ctx := r.Context()
	if req.DryRun {
		plan, err := rt.d.Plan(ctx, actor, req)
		if err != nil {
			operationError(w, err)
			return
		}
		if req.Kind == "deploy" && !planning(r) && r.URL.Query().Get("diff") == "fields" {
			writeDeployFieldPreview(w, plan)
			return
		}
		json.NewEncoder(w).Encode(plan) //nolint:errcheck // This final HTTP response has no remaining error-reporting channel.
		return
	}
	op, _, err := rt.d.Admit(ctx, actor, req)
	if err != nil {
		operationError(w, err)
		return
	}
	path := apiPrefix(rt.c) + "operations/" + op.ID + "/events"
	w.Header().Set("Location", path)
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]string{"operation_id": op.ID, "events_url": path}) //nolint:errcheck // This final HTTP response has no remaining error-reporting channel.
}

func (rt *routeRuntime) handleOperation(call routeCall) {
	w, ctx, id := call.w, call.r.Context(), call.rest[1]
	d := rt.d
	op, err := d.Store.Operation(ctx, id)
	if err != nil {
		operationError(w, err)
		return
	}
	json.NewEncoder(w).Encode(publicOperation(op)) //nolint:errcheck // This final HTTP response has no remaining error-reporting channel.
}

func (rt *routeRuntime) handleOutput(call routeCall) {
	w, ctx, id := call.w, call.r.Context(), call.rest[1]
	d := rt.d
	op, err := d.Store.Operation(ctx, id)
	if err != nil {
		operationError(w, err)
		return
	}
	if op.Kind != "exec" && op.Kind != "run" {
		writeError(w, 404, "not_found", "not found")
		return
	}
	intents, err := d.Store.Intents(ctx, op.ID)
	if err != nil {
		operationError(w, err)
		return
	}
	for _, i := range intents {
		if i.Kind == "exec" && i.Phase == "finished" {
			var output struct {
				Output   string `json:"output"`
				ExitCode int    `json:"exit_code"`
			}
			if json.Unmarshal(i.Receipt, &output) != nil {
				writeError(w, 503, "unavailable", "output unavailable")
				return
			}
			json.NewEncoder(w).Encode(output) //nolint:errcheck // This final HTTP response has no remaining error-reporting channel.
			return
		}
	}
	writeError(w, 404, "not_found", "output unavailable")
}

func (rt *routeRuntime) handleDiagnostics(call routeCall) {
	w, ctx, id := call.w, call.r.Context(), call.rest[1]
	d := rt.d
	op, err := d.Store.Operation(ctx, id)
	if err != nil {
		operationError(w, err)
		return
	}
	items, err := d.Store.Diagnostics(ctx, op.ID)
	if err != nil {
		writeError(w, 503, "unavailable", "diagnostics unavailable")
		return
	}
	if len(items) == 0 {
		writeError(w, 404, "not_found", "no diagnostics for this operation")
		return
	}
	json.NewEncoder(w).Encode(struct { //nolint:errcheck // This final HTTP response has no remaining error-reporting channel.
		OperationID string            `json:"operation_id"`
		Reason      string            `json:"reason"`
		Diagnostics []json.RawMessage `json:"diagnostics"`
	}{op.ID, safeReason(op.Reason), items})
}

func (rt *routeRuntime) handleResolve(call routeCall, read readRouteBody[resolveBody]) {
	w, ctx, id := call.w, call.r.Context(), call.rest[1]
	d, actor := rt.d, call.actor
	req, err := read()
	if err != nil {
		writeError(w, 400, "bad_request", "invalid request")
		return
	}
	if !validReason(req.Reason) {
		writeError(w, 400, "bad_request", "reason required")
		return
	}
	report, err := d.ResolveRecovery(ctx, actor, id, req.Reason, req.DryRun)
	if err != nil {
		operationError(w, err)
		return
	}
	json.NewEncoder(w).Encode(report) //nolint:errcheck // This final HTTP response has no remaining error-reporting channel.
}

func (rt *routeRuntime) handleCancel(call routeCall, read readRouteBody[dryRunBody]) {
	w, ctx, id := call.w, call.r.Context(), call.rest[1]
	d, actor := rt.d, call.actor
	req, err := read()
	if err != nil {
		writeError(w, 400, "bad_request", "invalid request")
		return
	}
	plan, err := d.Cancel(ctx, actor, id, req.DryRun)
	var refused deploy.CancelRefused
	if errors.As(err, &refused) {
		writeError(w, 409, "cancel_refused", string(refused))
		return
	}
	if err != nil {
		operationError(w, err)
		return
	}
	json.NewEncoder(w).Encode(plan) //nolint:errcheck // This final HTTP response has no remaining error-reporting channel.
}

func (rt *routeRuntime) handleEvents(call routeCall) {
	streamOperation(call.w, call.r, rt.d.Store, call.rest[1], rt.lim)
}

func (rt *routeRuntime) handleWait(call routeCall) {
	w, r, app := call.w, call.r, call.rest[1]
	timeout := 2 * time.Minute
	if raw := r.URL.Query().Get("timeout"); raw != "" {
		var err error
		timeout, err = time.ParseDuration(raw)
		if err != nil || timeout <= 0 || timeout > 10*time.Minute {
			writeError(w, 400, "bad_request", "invalid wait timeout")
			return
		}
	}
	waitCtx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()
	if err := rt.d.Wait(waitCtx, app, r.URL.Query().Get("role")); err != nil {
		operationError(w, err)
		return
	}
	json.NewEncoder(w).Encode(map[string]string{"status": "healthy"}) //nolint:errcheck // This final HTTP response has no remaining error-reporting channel.
}

func (rt *routeRuntime) handleApplied(call routeCall) {
	w, ctx, app := call.w, call.r.Context(), call.rest[1]
	d := rt.d
	// The release the app runs now: berthy deploy asks before it deploys an app that keeps its version.
	release, err := d.Store.Applied(ctx, app)
	if errors.Is(err, sql.ErrNoRows) {
		// A distinct code, so a client never takes the 404 of an agent without this route for an app that was never deployed.
		writeError(w, 404, "not_deployed", "app is not deployed")
		return
	}
	if err != nil {
		operationError(w, err)
		return
	}
	row := map[string]any{"id": release.ID, "digest": release.Digest}
	if release.Tag != "" {
		row["tag"] = release.Tag
	}
	json.NewEncoder(w).Encode(row) //nolint:errcheck // This final HTTP response has no remaining error-reporting channel.
}

func (rt *routeRuntime) handleHistoryDiff(call routeCall) {
	w, r, app := call.w, call.r, call.rest[1]
	// What release `release` of the app changes compared with release `base`, or with the release before it.
	id, base, ok := historyDiffIDs(r.URL.Query())
	if !ok {
		writeError(w, 400, "bad_request", "release and base must be release numbers")
		return
	}
	diff, err := rt.d.DiffReleases(r.Context(), app, id, base)
	if err != nil {
		operationError(w, err)
		return
	}
	json.NewEncoder(w).Encode(diff) //nolint:errcheck // This final HTTP response has no remaining error-reporting channel.
}

func (rt *routeRuntime) handleHistory(call routeCall) {
	w, ctx, app := call.w, call.r.Context(), call.rest[1]
	d := rt.d
	rows, err := d.Store.History(ctx, app)
	if err != nil {
		operationError(w, err)
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		out = append(out, publicRelease(r))
	}
	failures, err := d.Store.DeployHistory(ctx, app)
	if err != nil {
		operationError(w, err)
		return
	}
	for _, op := range failures {
		row := map[string]any{"operation_id": op.OperationID, "kind": op.Kind, "app": op.App, "commit": op.Commit, "state": op.State, "created": op.Started}
		if op.Tag != "" {
			row["tag"] = op.Tag
		}
		if reason := safeReason(op.Reason); reason != "" {
			row["reason"] = reason
		}
		if op.Modified {
			row["modified"] = true
		}
		addSchemaStates(row, op.SchemaBefore, op.SchemaAfter)
		out = append(out, row)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i]["created"].(time.Time).After(out[j]["created"].(time.Time)) })
	json.NewEncoder(w).Encode(out) //nolint:errcheck // This final HTTP response has no remaining error-reporting channel.
}

func (rt *routeRuntime) handleAudit(call routeCall) {
	w, ctx := call.w, call.r.Context()
	d := rt.d
	rows, err := d.Store.AuditLog(ctx, 100)
	if err != nil {
		operationError(w, err)
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, a := range rows {
		row := map[string]any{"operation_id": a.OperationID, "actor_id": a.ActorID, "actor_name": a.ActorName, "remote": a.Remote, "action": a.Action, "outcome": a.Outcome, "time": a.Time}
		if a.Tag != "" {
			row["tag"] = a.Tag
		}
		out = append(out, row)
	}
	json.NewEncoder(w).Encode(out) //nolint:errcheck // This final HTTP response has no remaining error-reporting channel.
}

func publicRelease(r state.Release) map[string]any {
	row := map[string]any{"id": r.ID, "app": r.App, "digest": r.Digest, "commit": r.Commit, "created": r.Created}
	if r.OperationID != "" {
		row["operation_id"] = r.OperationID
	}
	if r.Tag != "" {
		row["tag"] = r.Tag
	}
	if r.Modified {
		row["modified"] = true
	}
	addSchemaStates(row, r.SchemaBefore, r.SchemaAfter)
	return row
}

// historyDiffIDs reads the release numbers of a history diff: release is required, base is optional and means the release
// before it when it is left out.
func historyDiffIDs(q url.Values) (id, base int64, ok bool) {
	id, err := strconv.ParseInt(q.Get("release"), 10, 64)
	if err != nil || id < 1 {
		return 0, 0, false
	}
	if raw := q.Get("base"); raw != "" {
		if base, err = strconv.ParseInt(raw, 10, 64); err != nil || base < 1 {
			return 0, 0, false
		}
	}
	return id, base, true
}

// routeScope is the route table of the deployment API. rest is the path after the environment. It returns the scope the
// route needs and the one HTTP method it takes. ok is false for a path that is not one of these routes. Every route whose
// method is not GET changes something.
func routeScope(rest []string, httpMethod string) (required, method string, ok bool) {
	row := findEnvRoute(rest, httpMethod, false)
	if row == nil || row.lane == legacyReadRoute {
		return "", "", false
	}
	return row.scope, row.method, true
}

// authenticate finds the token a request carries and the actor it stands for. status is as tokenSource.identify gives it: 200
// when the request is authenticated.
func authenticate(tokens *tokenSource, r *http.Request) (actor deploy.Actor, scope string, status int) {
	token, status := tokens.identify(r)
	if status != http.StatusOK {
		return deploy.Actor{}, "", status
	}
	sum, _ := token.sum()
	return tokenActor(token.Token, sum, r.RemoteAddr), token.Scope, http.StatusOK
}

// tokenActor is the actor a token stands for. sum is the SHA-256 of the token's value, which is also its digest. The actor's
// ID is a fingerprint of the digest, so the ID never reveals what verifies the token.
func tokenActor(t Token, sum [32]byte, remote string) deploy.Actor {
	id := tokenID(sum)
	name := t.Name
	if name == "" {
		name = "token:" + id[:12]
	}
	return deploy.Actor{ID: id, Name: name, Remote: remote, Admin: t.Scope == "admin", Agent: t.Agent}
}

func (rt *routeRuntime) handleSecretImport(call routeCall, read readRouteBody[api.SecretImport]) {
	serveSecretImport(call.w, call.r, rt.d, call.stateActor(), read)
}

// serveSecretImport answers POST secrets/import. A dry run plans the import by its names alone. A real import stores every
// entry in one transaction, so a failure stores none. A name stored as a registry password is refused: a plain write would
// turn that password into a plain secret.
func serveSecretImport(w http.ResponseWriter, r *http.Request, d *deploy.Controller, sa state.Actor, read readRouteBody[api.SecretImport]) {
	ctx := r.Context()
	req, err := read()
	if err != nil {
		writeError(w, 400, "bad_request", "invalid request")
		return
	}
	if err := api.CheckSecretImport(req.Secrets); err != nil {
		writeError(w, 400, "bad_request", err.Error())
		return
	}
	// Sorted, so the plan and the audit rows come out in the same order every time.
	sort.Slice(req.Secrets, func(i, j int) bool { return req.Secrets[i].Name < req.Secrets[j].Name })
	if req.DryRun {
		names := make([]string, 0, len(req.Secrets))
		for _, e := range req.Secrets {
			names = append(names, e.Name)
		}
		create, replace, err := d.Store.ClassifySecrets(ctx, names)
		if err != nil {
			secretImportError(w, err)
			return
		}
		json.NewEncoder(w).Encode(api.SecretImportPlan{Action: "secret import", Create: create, Replace: replace}) //nolint:errcheck // This final HTTP response has no remaining error-reporting channel.
		return
	}
	writes := make([]state.SecretWrite, 0, len(req.Secrets))
	for _, e := range req.Secrets {
		writes = append(writes, state.SecretWrite{Name: e.Name, Value: []byte(e.Value)})
	}
	created, replaced, err := d.Store.PutSecrets(ctx, sa, writes)
	if err != nil {
		secretImportError(w, err)
		return
	}
	json.NewEncoder(w).Encode(api.SecretImportResult{Created: created, Replaced: replaced}) //nolint:errcheck // This final HTTP response has no remaining error-reporting channel.
}

// secretImportError answers a failed import. A registry password name gets a refusal that says what to use instead; any other
// failure is answered as operationError answers it.
func secretImportError(w http.ResponseWriter, err error) {
	var rs state.RegistrySecretError
	if errors.As(err, &rs) {
		writeError(w, 400, "bad_request", rs.Name+" is a registry password: use secret set --registry")
		return
	}
	operationError(w, err)
}

func writeDeployFieldPreview(w http.ResponseWriter, plan deploy.Plan) {
	if plan.DiffBase == nil || plan.Diff == nil {
		writeError(w, http.StatusBadRequest, "operation_failed", "deploy field comparison unavailable")
		return
	}
	fields := plan.Diff.Fields
	if fields == nil {
		fields = []deploy.DiffField{}
	}
	response := struct {
		deploy.Plan
		ReleaseDiff struct {
			Base   int64              `json:"base"`
			Fields []deploy.DiffField `json:"fields"`
		} `json:"release_diff"`
	}{Plan: plan}
	response.ReleaseDiff.Base = *plan.DiffBase
	response.ReleaseDiff.Fields = fields
	data, err := json.Marshal(response)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "operation_failed", "deploy field comparison unavailable")
		return
	}
	if len(data) >= api.MaxDeployDiffResponse { // Leave room for the final newline.
		writeError(w, http.StatusBadRequest, "operation_failed", "deploy field preview exceeds response limit")
		return
	}
	w.Write(append(data, '\n'))
}

func operationError(w http.ResponseWriter, err error) {
	if errors.Is(err, state.ErrConflict) {
		writeError(w, 409, "conflict", "environment or request conflict")
		return
	}
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, 404, "not_found", "not found")
		return
	}
	var r deploy.Refusal
	if errors.As(err, &r) {
		writeError(w, 400, "operation_failed", string(r))
		return
	}
	writeError(w, 400, "operation_failed", "operation unavailable or invalid")
}
func publicOperation(o state.Operation) any {
	return struct {
		ID           string    `json:"id"`
		State        string    `json:"state"`
		Kind         string    `json:"kind"`
		App          string    `json:"app"`
		Commit       string    `json:"commit"`
		Started      time.Time `json:"started"`
		Error        string    `json:"error,omitempty"`
		Reason       string    `json:"reason,omitempty"`
		SchemaBefore string    `json:"schema_before,omitempty"`
		SchemaAfter  string    `json:"schema_after,omitempty"`
	}{o.ID, o.State, o.Kind, o.App, o.Commit, o.Started, safeOperationError(o), safeReason(o.Reason), o.SchemaBefore, o.SchemaAfter}
}

// safeReason returns a failure reason when it is the public kind the deploy package writes: at most 300 bytes
// of printable ASCII. Anything else is dropped rather than shown.
func safeReason(reason string) string {
	if len(reason) > 300 {
		return ""
	}
	for _, r := range reason {
		if r < 0x20 || r > 0x7e {
			return ""
		}
	}
	return reason
}

func safeOperationError(o state.Operation) string {
	switch o.Err {
	case "":
		return ""
	case "pre-deploy command failed", "one-off outcome uncertain", "post-deploy command failed", "post-deploy command outcome uncertain", "rollout recovery required", "rollout failed and was cleaned up", "rollback release unavailable", "schema revert unavailable", "schema revert failed", "schema revert did not reach target", "schema revert outcome uncertain", "operation interrupted and cleaned up", "post-deploy completion was interrupted", "unsupported recovery step", "one-off recovery required", "verified release requires inspection", "service recovery required", "service update requires recovery", "service health requires recovery", "service drain requires recovery", "service journal requires recovery", "exec outcome uncertain", "exec returned nonzero", "deployment preflight failed", "image pull failed", "unsafe or incompatible service", "service name is unavailable", "recovery resolved by admin", "Docker unavailable", "image inventory unavailable", "container inventory unavailable", "service inventory unavailable", "container image unknown", "service image unknown", "release history unavailable", "hook inventory unavailable", "bound service unavailable", "invalid bound service", "bound service ownership changed", "prior service health cannot be verified", "prior image identity unavailable", "prior image is not pinned", "prior image credential unavailable", "removing managed roles unavailable", "role preflight failed", "image inspection failed", "image provenance recording failed", "adoption preflight failed", "migration source journal failed", "Cloudflare API token secret unavailable", "Cloudflare token check failed", "Cloudflare unavailable", "tunnel is not remotely managed", "ingress hostname conflict", "ingress publish failed", "managed ingress is not configured", "tunnel token unavailable", "tunnel token removal failed", "connector removal failed", "ingress state recording failed", "managed ingress is not applied; run ingress apply", "managed ingress changed; run ingress apply":
		return o.Err
	default:
		if deploy.PublicMessage(o.Err) {
			return o.Err
		}
		return "operation failed"
	}
}

// validReason reports whether s is 1 to 200 bytes of valid UTF-8 without control characters.
func validReason(s string) bool {
	if s == "" || len(s) > 200 || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// addSchemaStates adds the recorded schema states to a history row when there are any.
func addSchemaStates(row map[string]any, before, after string) {
	if before != "" {
		row["schema_before"] = before
	}
	if after != "" {
		row["schema_after"] = after
	}
}

func streamOperation(w http.ResponseWriter, r *http.Request, s *state.Store, id string, lim *limits) {
	if _, err := s.Operation(r.Context(), id); err != nil {
		operationError(w, err)
		return
	}
	var after uint64
	var err error
	if v := r.Header.Get("Last-Event-ID"); v != "" {
		after, err = strconv.ParseUint(v, 10, 64)
		if err != nil {
			writeError(w, 400, "bad_request", "invalid event cursor")
			return
		}
	}
	select {
	case lim.watchers <- struct{}{}:
		defer func() { <-lim.watchers }()
	default:
		writeError(w, 503, "too_many_followers", "too many followers")
		return
	}
	f, ok := w.(http.Flusher)
	if !ok {
		writeError(w, 500, "stream_unavailable", "stream unavailable")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		op, err := s.Operation(r.Context(), id)
		if err != nil {
			return
		}
		events, err := s.EventsAfter(r.Context(), id, after, 100)
		if err != nil {
			return
		}
		for _, ev := range events {
			after = ev.ID
			if ev.Kind == "rollout" && r.URL.Query().Get("progress") != "1" {
				continue
			}
			_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(5 * time.Second))
			payload, marshalErr := json.Marshal(ev.Data)
			if marshalErr != nil {
				return
			}
			kind := ev.Kind
			for _, ch := range kind {
				if !(ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9' || ch == '_') {
					kind = "progress"
					break
				}
			}
			if _, err = fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", ev.ID, kind, payload); err != nil {
				return
			}
			f.Flush()
			_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})
		}
		if op.State != "running" && len(events) < 100 {
			latest, err := s.Operation(r.Context(), id)
			if err != nil || r.Context().Err() != nil {
				return
			}
			if latest.State != op.State {
				continue
			}
			op = latest
			result, _ := json.Marshal(struct {
				Status    string `json:"status"`
				Operation any    `json:"operation"`
			}{op.State, publicOperation(op)})
			_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(5 * time.Second))
			_, _ = fmt.Fprintf(w, "event: terminal\ndata: %s\n\n", result)
			f.Flush()
			_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})
			return
		}
		if op.State != "running" && len(events) == 100 {
			continue
		}

		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
		case <-heartbeat.C:
			_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(5 * time.Second))
			if _, err = fmt.Fprint(w, ": heartbeat\n\n"); err != nil {
				return
			}
			f.Flush()
			_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})
		}
	}
}
