package agent

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"berthy/internal/deploy"
	"berthy/internal/engine"
	"berthy/internal/metrics"
	"berthy/internal/state"
)

func OpenDeployment(c Config, e *engine.Engine) (*deploy.Controller, error) {
	if c.StateDir == "" && c.KeyFile == "" {
		return nil, nil
	}
	s, err := state.Open(c.StateDir, c.KeyFile, c.Environment)
	if err != nil {
		return nil, err
	}
	controller := &deploy.Controller{Store: s, Environment: c.Environment, Protected: c.Protected}
	if e != nil {
		controller.Engine = e
	}
	return controller, nil
}
func HandlerWithRuntime(c Config, e *engine.Engine, col *metrics.Collector, d *deploy.Controller) http.Handler {
	return NewDeploymentHandler(c, e, col, d)
}
func NewDeploymentHandler(c Config, e *engine.Engine, col *metrics.Collector, d *deploy.Controller) http.Handler {
	legacy := handlerWithState(c, e, col, d)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(parts) < 4 || parts[0] != "v1" || parts[1] != "environments" {
			legacy.ServeHTTP(w, r)
			return
		}
		rest := parts[3:]
		required := ""
		method := ""
		switch {
		case len(rest) == 1 && rest[0] == "deploys":
			required = "deploy"
			method = "POST"
		case len(rest) == 1 && (rest[0] == "rollback" || rest[0] == "restart" || rest[0] == "scale"):
			required = "deploy"
			method = "POST"
		case len(rest) == 1 && (rest[0] == "exec" || rest[0] == "prune"):
			required = "admin"
			method = "POST"
		case len(rest) == 2 && rest[0] == "operations":
			required = "read"
			method = "GET"
		case len(rest) == 3 && rest[0] == "operations" && rest[2] == "output":
			required = "admin"
			method = "GET"
		case len(rest) == 3 && rest[0] == "operations" && rest[2] == "events":
			required = "read"
			method = "GET"
		case len(rest) == 2 && (rest[0] == "history" || rest[0] == "wait"):
			required = "read"
			method = "GET"
		case len(rest) == 1 && (rest[0] == "audit" || rest[0] == "lock"):
			required = "read"
			method = "GET"
		case len(rest) == 2 && rest[0] == "lock" && (rest[1] == "acquire" || rest[1] == "release"):
			required = "deploy"
			method = "POST"
		case len(rest) == 1 && rest[0] == "secrets":
			required = "admin"
			if r.Method == "GET" {
				method = "GET"
			} else {
				method = "POST"
			}
		case len(rest) == 2 && rest[0] == "secrets":
			required = "admin"
			method = "DELETE"
		default:
			legacy.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		actor, scope, ok := authenticate(c, r)
		if !ok {
			writeError(w, 401, "unauthorized", "unauthorized")
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
		if d == nil || d.Store == nil {
			writeError(w, 503, "state_unavailable", "state unavailable")
			return
		}
		ctx := r.Context()
		sa := state.Actor{ID: actor.ID, Name: actor.Name, Remote: actor.Remote}
		switch {
		case len(rest) == 1 && (rest[0] == "deploys" || rest[0] == "rollback" || rest[0] == "restart" || rest[0] == "scale" || rest[0] == "exec" || rest[0] == "prune"):
			var req deploy.Request
			if err := decodeBody(w, r, &req); err != nil {
				writeError(w, 400, "bad_request", "invalid request")
				return
			}
			if rest[0] == "deploys" {
				req.Kind = "deploy"
			} else {
				req.Kind = rest[0]
			}
			if (req.Adopt || req.RevertSchema) && !actor.Admin {
				writeError(w, 403, "forbidden", "forbidden")
				return
			}
			if req.DryRun {
				plan, err := d.Plan(ctx, actor, req)
				if err != nil {
					operationError(w, err)
					return
				}
				json.NewEncoder(w).Encode(plan)
				return
			}
			op, _, err := d.Admit(ctx, actor, req)
			if err != nil {
				operationError(w, err)
				return
			}
			path := "/v1/environments/" + c.Environment + "/operations/" + op.ID + "/events"
			w.Header().Set("Location", path)
			w.WriteHeader(http.StatusAccepted)
			json.NewEncoder(w).Encode(map[string]string{"operation_id": op.ID, "events_url": path})
		case rest[0] == "operations" && len(rest) == 2:
			op, err := d.Store.Operation(ctx, rest[1])
			if err != nil {
				operationError(w, err)
				return
			}
			json.NewEncoder(w).Encode(publicOperation(op))
		case rest[0] == "operations" && len(rest) == 3 && rest[2] == "output":
			op, err := d.Store.Operation(ctx, rest[1])
			if err != nil {
				operationError(w, err)
				return
			}
			if op.Kind != "exec" {
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
					json.NewEncoder(w).Encode(output)
					return
				}
			}
			writeError(w, 404, "not_found", "output unavailable")
		case rest[0] == "operations":
			streamOperation(w, r, d.Store, rest[1])
		case rest[0] == "wait":
			timeout := 2 * time.Minute
			if raw := r.URL.Query().Get("timeout"); raw != "" {
				var err error
				timeout, err = time.ParseDuration(raw)
				if err != nil || timeout <= 0 || timeout > 10*time.Minute {
					writeError(w, 400, "bad_request", "invalid wait timeout")
					return
				}
			}
			waitCtx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			if err := d.Wait(waitCtx, rest[1], r.URL.Query().Get("role")); err != nil {
				operationError(w, err)
				return
			}
			json.NewEncoder(w).Encode(map[string]string{"status": "healthy"})
		case rest[0] == "history":
			rows, err := d.Store.History(ctx, rest[1])
			if err != nil {
				operationError(w, err)
				return
			}
			out := make([]map[string]any, 0, len(rows))
			for _, r := range rows {
				row := map[string]any{"id": r.ID, "app": r.App, "digest": r.Digest, "commit": r.Commit, "created": r.Created}
				addSchemaStates(row, r.SchemaBefore, r.SchemaAfter)
				out = append(out, row)
			}
			failures, err := d.Store.DeployHistory(ctx, rest[1])
			if err != nil {
				operationError(w, err)
				return
			}
			for _, op := range failures {
				row := map[string]any{"operation_id": op.OperationID, "kind": op.Kind, "app": op.App, "commit": op.Commit, "state": op.State, "created": op.Started}
				addSchemaStates(row, op.SchemaBefore, op.SchemaAfter)
				out = append(out, row)
			}
			sort.SliceStable(out, func(i, j int) bool { return out[i]["created"].(time.Time).After(out[j]["created"].(time.Time)) })
			json.NewEncoder(w).Encode(out)
		case rest[0] == "audit":
			rows, err := d.Store.AuditLog(ctx, 100)
			if err != nil {
				operationError(w, err)
				return
			}
			out := make([]map[string]any, 0, len(rows))
			for _, a := range rows {
				out = append(out, map[string]any{"operation_id": a.OperationID, "actor_id": a.ActorID, "actor_name": a.ActorName, "remote": a.Remote, "action": a.Action, "outcome": a.Outcome, "time": a.Time})
			}
			json.NewEncoder(w).Encode(out)
		case len(rest) == 1 && rest[0] == "lock":
			l, err := d.Store.Lock(ctx)
			if errors.Is(err, sql.ErrNoRows) {
				json.NewEncoder(w).Encode(map[string]bool{"locked": false})
				return
			}
			if err != nil {
				operationError(w, err)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"locked": true, "holder": l.Holder, "reason": l.Reason, "acquired": l.Acquired, "operation_id": l.OperationID})
		case rest[0] == "lock":
			var req struct {
				Message string `json:"message"`
				DryRun  bool   `json:"dry_run"`
			}
			if decodeBody(w, r, &req) != nil {
				writeError(w, 400, "bad_request", "invalid request")
				return
			}
			if req.DryRun {
				json.NewEncoder(w).Encode(map[string]bool{"dry_run": true})
				return
			}
			var err error
			if rest[1] == "acquire" {
				err = d.Store.AcquireManual(ctx, sa, actor.Name, req.Message)
			} else {
				err = d.Store.ReleaseManual(ctx, sa)
			}
			if err != nil {
				operationError(w, err)
				return
			}
			json.NewEncoder(w).Encode(map[string]bool{"ok": true})
		case rest[0] == "secrets" && r.Method == "GET":
			rows, err := d.Store.Secrets(ctx)
			if err != nil {
				operationError(w, err)
				return
			}
			out := make([]map[string]string, 0, len(rows))
			for _, v := range rows {
				out = append(out, map[string]string{"name": v.Name, "registry": v.Registry})
			}
			json.NewEncoder(w).Encode(out)
		case rest[0] == "secrets":
			var req struct {
				Name     string `json:"name"`
				Value    string `json:"value"`
				Registry string `json:"registry"`
				DryRun   bool   `json:"dry_run"`
			}
			if decodeBody(w, r, &req) != nil {
				writeError(w, 400, "bad_request", "invalid request")
				return
			}
			if len(rest) == 2 {
				req.Name = rest[1]
			}
			if req.Name == "" {
				writeError(w, 400, "bad_request", "name required")
				return
			}
			if req.DryRun {
				json.NewEncoder(w).Encode(map[string]bool{"dry_run": true})
				return
			}
			var err error
			if r.Method == "DELETE" {
				err = d.Store.DeleteSecret(ctx, sa, req.Name)
			} else {
				err = d.Store.PutSecret(ctx, sa, state.SecretInfo{Name: req.Name, Registry: req.Registry}, []byte(req.Value))
			}
			if err != nil {
				operationError(w, err)
				return
			}
			json.NewEncoder(w).Encode(map[string]bool{"ok": true})
		default:
			writeError(w, 501, "unavailable", "operation unavailable")
		}
	})
}
func authenticate(c Config, r *http.Request) (deploy.Actor, string, bool) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	raw := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	sum := sha256.Sum256([]byte(raw))
	var found *Token
	for i := range c.Tokens {
		b, err := hex.DecodeString(c.Tokens[i].Digest)
		if err == nil && subtle.ConstantTimeCompare(sum[:], b) == 1 {
			found = &c.Tokens[i]
		}
	}
	if len(parts) < 3 || parts[2] != c.Environment || !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") || !tokenPattern(c.Environment).MatchString(raw) || found == nil {
		return deploy.Actor{}, "", false
	}
	fingerprint := sha256.Sum256(sum[:])
	id := hex.EncodeToString(fingerprint[:])
	name := found.Name
	if name == "" {
		name = "token:" + id[:12]
	}
	return deploy.Actor{ID: id, Name: name, Remote: r.RemoteAddr, Admin: found.Scope == "admin"}, found.Scope, true
}
func decodeBody(w http.ResponseWriter, r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.Decode(&struct{}{}) != io.EOF {
		return errors.New("trailing data")
	}
	return nil
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
		SchemaBefore string    `json:"schema_before,omitempty"`
		SchemaAfter  string    `json:"schema_after,omitempty"`
	}{o.ID, o.State, o.Kind, o.App, o.Commit, o.Started, safeOperationError(o), o.SchemaBefore, o.SchemaAfter}
}

func safeOperationError(o state.Operation) string {
	switch o.Err {
	case "":
		return ""
	case "pre-deploy command failed", "one-off outcome uncertain", "post-deploy command failed", "post-deploy command outcome uncertain", "rollout recovery required", "rollout failed and was cleaned up", "rollback release unavailable", "schema revert unavailable", "schema revert failed", "schema revert did not reach target", "schema revert outcome uncertain", "operation interrupted and cleaned up", "post-deploy completion was interrupted", "unsupported recovery step", "one-off recovery required", "verified release requires inspection", "service recovery required", "service update requires recovery", "service health requires recovery", "service drain requires recovery", "service journal requires recovery", "exec outcome uncertain", "exec returned nonzero", "deployment preflight failed", "image pull failed", "unsafe or incompatible service", "service name is unavailable":
		return o.Err
	default:
		if deploy.PublicMessage(o.Err) {
			return o.Err
		}
		return "operation failed"
	}
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

var operationWatchers = make(chan struct{}, 16)

func streamOperation(w http.ResponseWriter, r *http.Request, s *state.Store, id string) {
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
	case operationWatchers <- struct{}{}:
		defer func() { <-operationWatchers }()
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
		events, err := s.EventsAfter(r.Context(), id, after, 100)
		if err != nil {
			return
		}
		for _, ev := range events {
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
			after = ev.ID
		}
		op, err := s.Operation(r.Context(), id)
		if err != nil {
			return
		}
		if op.State != "running" && len(events) < 100 {
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
