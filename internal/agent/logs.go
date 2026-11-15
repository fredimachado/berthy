package agent

import (
	"berthy/internal/api"
	"berthy/internal/deploy"
	"berthy/internal/engine"
	"berthy/internal/logs"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var followSlots = make(chan struct{}, 16)

func serveLogs(w http.ResponseWriter, r *http.Request, e *engine.Engine, name string, deployment *deploy.Controller) {
	q := r.URL.Query()
	tail := 100
	var err error
	if q.Get("tail") != "" {
		tail, err = strconv.Atoi(q.Get("tail"))
		if err != nil || tail < 0 || tail > logs.MaxTail {
			writeError(w, 400, "bad_request", "invalid tail")
			return
		}
	}
	var since time.Time
	if q.Get("since") != "" {
		since, err = parseSince(q.Get("since"), time.Now())
		if err != nil {
			writeError(w, 400, "bad_request", "invalid since")
			return
		}
	}
	var re *regexp.Regexp
	if q.Get("grep") != "" {
		re, err = regexp.Compile(q.Get("grep"))
		if err != nil {
			writeError(w, 400, "bad_request", "invalid grep expression")
			return
		}
	}
	if e == nil {
		writeError(w, 503, "docker_unavailable", "docker unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	apps, err := e.Apps(ctx)
	cancel()
	if err != nil {
		code := dockerErrorCode(err)
		writeError(w, 503, code, strings.ReplaceAll(code, "_", " "))
		return
	}
	app, code := engine.Select(apps, name)
	if code != 200 {
		if code == 409 {
			writeError(w, code, "conflict", "ambiguous app; use swarm: or compose:")
		} else {
			writeError(w, code, "not_found", "app not found")
		}
		return
	}
	ids, status, message := logSources(app, q.Get("role"), q.Get("replica"))
	if status != 200 {
		writeError(w, status, "not_found", message)
		return
	}
	if len(ids) > logs.MaxSources {
		writeError(w, 400, "bad_request", "too many sources")
		return
	}
	ctx, cancel = context.WithTimeout(r.Context(), 5*time.Second)
	secrets, err := managedLogSecrets(ctx, deployment, e, app, ids)
	cancel()
	if err != nil {
		writeError(w, 503, "logs_unavailable", "logs unavailable")
		return
	}
	opts := logs.Options{Tail: tail, Since: since}
	if q.Get("follow") != "1" {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		rows, err := logs.Tail(ctx, e, ids, opts)
		if err != nil {
			code := logsErrorCode(err)
			writeError(w, 503, code, strings.ReplaceAll(code, "_", " "))
			return
		}
		rows = redactRecords(rows, secrets, re)
		if rows == nil {
			rows = []logs.Record{}
		}
		json.NewEncoder(w).Encode(rows)
		return
	}
	select {
	case followSlots <- struct{}{}:
		defer func() { <-followSlots }()
	default:
		writeError(w, 503, "too_many_followers", "too many followers")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, 500, "stream_unavailable", "stream unavailable")
		return
	}
	ctx, cancel = context.WithCancel(r.Context())
	defer cancel()
	type event struct {
		record logs.Record
		err    error
		done   bool
	}
	events := make(chan event)
	go func() {
		err := logs.Follow(ctx, e, ids, opts, func(record logs.Record) error {
			filtered := redactRecords([]logs.Record{record}, secrets, re)
			if len(filtered) == 0 {
				return nil
			}
			select {
			case events <- event{record: filtered[0]}:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
		select {
		case events <- event{err: err, done: true}:
		case <-ctx.Done():
		}
	}()
	started := false
	send := func(kind string, v any) bool {
		b, _ := json.Marshal(v)
		if !started {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			started = true
		}
		http.NewResponseController(w).SetWriteDeadline(time.Now().Add(5 * time.Second))
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", kind, b); err != nil {
			return false
		}
		flusher.Flush()
		http.NewResponseController(w).SetWriteDeadline(time.Time{})
		return true
	}
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-events:
			if ev.done {
				if ev.err != nil {
					code := logsErrorCode(ev.err)
					message := strings.ReplaceAll(code, "_", " ")
					if !started {
						writeError(w, 503, code, message)
					} else {
						send("error", map[string]string{"code": code, "message": message})
					}
				} else {
					send("done", map[string]bool{"complete": true})
				}
				return
			}
			if !send("log", ev.record) {
				return
			}
		case <-ticker.C:
			if !started {
				w.Header().Set("Content-Type", "text/event-stream")
				started = true
			}
			http.NewResponseController(w).SetWriteDeadline(time.Now().Add(5 * time.Second))
			if _, err := fmt.Fprint(w, ": heartbeat\n\n"); err != nil {
				return
			}
			flusher.Flush()
			http.NewResponseController(w).SetWriteDeadline(time.Time{})
		}
	}
}

// parseSince accepts an RFC3339 timestamp or a positive Go duration, which is
// subtracted from now.
func parseSince(value string, now time.Time) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return t, nil
	}
	d, err := time.ParseDuration(value)
	if err != nil || d <= 0 {
		return time.Time{}, fmt.Errorf("invalid since")
	}
	return now.Add(-d), nil
}

// logSources returns the containers this node can read for the role and
// replica filters. A replica not inspected on this node (Local false) still
// counts as found but is not a log source.
func logSources(app api.App, wantRole, wantReplica string) (ids []logs.Identity, status int, message string) {
	roleFound := wantRole == ""
	replicaFound := wantReplica == ""
	replicaRemote := false
	for _, role := range app.Roles {
		if wantRole != "" && wantRole != role.Name {
			continue
		}
		roleFound = true
		for _, rep := range role.Replicas {
			identity := rep.ContainerID
			if rep.TaskID != "" {
				identity = rep.TaskID
			}
			if wantReplica != "" && wantReplica != identity && wantReplica != rep.ContainerID {
				continue
			}
			replicaFound = true
			if rep.ContainerID == "" {
				continue
			}
			if !rep.Local {
				if wantReplica != "" {
					replicaRemote = true
				}
				continue
			}
			ids = append(ids, logs.Identity{Role: role.Name, Replica: identity, ContainerID: rep.ContainerID})
		}
	}
	if !roleFound || !replicaFound {
		return nil, 404, "role or replica not found"
	}
	if replicaRemote && len(ids) == 0 {
		return nil, 404, "replica not on this node"
	}
	return ids, 200, ""
}

func logsErrorCode(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "docker_deadline"
	}
	return "logs_unavailable"
}
