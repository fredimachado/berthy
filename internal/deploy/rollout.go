package deploy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"time"

	"berthy/internal/engine"
	"berthy/internal/state"
)

type journal interface {
	RecordIntent(context.Context, string, state.Intent) error
	MarkStep(context.Context, string, int, string, json.RawMessage, ...state.Event) error
}

// rolloutMilestone is a role started/ready event persisted inside a journal transaction.
func rolloutMilestone(role, status string) state.Event {
	data, err := json.Marshal(map[string]string{"role": role, "status": status})
	if err != nil {
		return state.Event{}
	}
	return state.Event{Kind: "rollout", Data: data}
}

type serviceMutator interface {
	InspectService(context.Context, string) (engine.ServiceSnapshot, error)
	CreateService(context.Context, json.RawMessage, *engine.RegistryAuth) (string, error)
	UpdateService(context.Context, string, uint64, json.RawMessage, *engine.RegistryAuth) error
	DeleteService(context.Context, string) error
}

// sameSpec reports whether two service specs are the same once each is canonicalized. Docker stores a spec through types
// that omit zero values, so a spec read back never equals the one that was sent unless both sides drop them.
func sameSpec(a, b json.RawMessage) bool {
	var x, y any
	da := json.NewDecoder(bytes.NewReader(a))
	da.UseNumber()
	db := json.NewDecoder(bytes.NewReader(b))
	db.UseNumber()
	if da.Decode(&x) != nil || db.Decode(&y) != nil {
		return false
	}
	ax, _ := json.Marshal(canonicalSpec(x)) //nolint:errcheck // Canonicalization retains only decoded JSON values.
	by, _ := json.Marshal(canonicalSpec(y)) //nolint:errcheck // Canonicalization retains only decoded JSON values.
	return bytes.Equal(ax, by)
}

// canonicalSpec drops every object field whose value is one Docker omits when it stores a service: null, false, 0, "", an
// empty list, and an empty object, which includes an object whose own fields were all dropped. List elements are kept in
// place, and so are the zero values inside lists, because a list is not omitted field by field.
func canonicalSpec(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			if e = canonicalSpec(e); isZeroSpecValue(e) {
				delete(x, k)
			} else {
				x[k] = e
			}
		}
	case []any:
		for i, e := range x {
			x[i] = canonicalSpec(e)
		}
	}
	return v
}

func isZeroSpecValue(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case bool:
		return !x
	case string:
		return x == ""
	case json.Number:
		f, err := x.Float64()
		return err == nil && f == 0
	case []any:
		return len(x) == 0
	case map[string]any:
		return len(x) == 0
	}
	return false
}

type beforeServiceMutation struct{ err error }

func (e beforeServiceMutation) Error() string { return e.err.Error() }
func (e beforeServiceMutation) Unwrap() error { return e.err }

func applyIntent(ctx context.Context, j journal, e serviceMutator, op string, i state.Intent, auth *engine.RegistryAuth) error {
	if err := ctx.Err(); err != nil {
		return beforeServiceMutation{err}
	}
	if err := j.RecordIntent(ctx, op, i); err != nil {
		return beforeServiceMutation{err}
	}
	if err := ctx.Err(); err != nil {
		return beforeServiceMutation{err}
	}
	if i.Kind == "create" {
		_, err := e.CreateService(ctx, i.Desired, auth)
		if err != nil {
			live, inspectErr := e.InspectService(ctx, i.Name)
			if inspectErr != nil || !matchesStoredSpec(i.Desired, live.Spec) {
				return errors.New("service create outcome is uncertain")
			}
		}
	} else if i.Kind == "update" {
		live, err := e.InspectService(ctx, i.Name)
		if err := ctx.Err(); err != nil {
			return beforeServiceMutation{err}
		}
		if err != nil || live.ID != i.ServiceID || !sameSpec(live.Spec, i.Before) {
			return beforeServiceMutation{errors.New("service changed before update")}
		}
		if err = e.UpdateService(ctx, live.ID, live.Version, i.Desired, auth); err != nil {
			live, inspectErr := e.InspectService(ctx, i.Name)
			if inspectErr != nil || live.ID != i.ServiceID || !matchesStoredSpec(i.Desired, live.Spec) {
				return errors.New("service update outcome is uncertain")
			}
		}
	} else {
		return beforeServiceMutation{errors.New("invalid service intent")}
	}
	return j.MarkStep(ctx, op, i.Ordinal, "issued", json.RawMessage(`{}`))
}

// compensate touches only a service that still exactly matches the journaled target.
func compensate(ctx context.Context, e serviceMutator, i state.Intent, auth *engine.RegistryAuth) error {
	live, err := e.InspectService(ctx, i.Name)
	if err != nil {
		var status *engine.StatusError
		if i.Kind == "create" && errors.As(err, &status) && status.Code == 404 {
			return nil
		}
		return errors.New("service recovery inspection failed")
	}
	if i.Kind == "update" && live.ID == i.ServiceID && sameSpec(live.Spec, i.Before) {
		return nil
	}
	if (i.Kind == "update" && live.ID != i.ServiceID) || !matchesStoredSpec(i.Desired, live.Spec) {
		return errors.New("service changed outside operation")
	}
	if i.Kind == "create" {
		return e.DeleteService(ctx, live.ID)
	}
	if i.Kind != "update" {
		return errors.New("invalid service intent")
	}
	return e.UpdateService(ctx, live.ID, live.Version, i.Before, auth)
}

type healthReader interface {
	ServiceTasks(context.Context, string) ([]engine.ServiceTask, error)
	InspectContainerHealth(context.Context, string) (string, error)
}

const (
	rolloutMonitor           time.Duration = 5 * time.Second
	rolloutVerificationGrace time.Duration = 15 * time.Second
)

func rolloutAllowance(hold time.Duration) time.Duration {
	if hold <= 0 {
		return 0
	}
	return hold + rolloutVerificationGrace
}

type rolloutTiming struct {
	window time.Duration
	cutoff time.Time
	hold   time.Duration
	drain  time.Duration
}

func withRolloutCutoff(ctx context.Context, cutoff time.Time) (context.Context, context.CancelFunc) {
	if !cutoff.IsZero() {
		return context.WithDeadline(ctx, cutoff)
	}
	return ctx, func() {}
}

func healthyOnce(ctx context.Context, e healthReader, id string, generation string, replicas int) error {
	tasks, err := e.ServiceTasks(ctx, id)
	if err != nil || ctx.Err() != nil {
		return errors.New("task inspection failed")
	}
	if healthyIn(ctx, e, tasks, id, generation) < replicas || ctx.Err() != nil {
		return errors.New("service generation is not healthy")
	}
	return nil
}

// healthyIn counts distinct healthy slots of the candidate generation.
func healthyIn(ctx context.Context, e healthReader, tasks []engine.ServiceTask, id, generation string) int {
	slots := make(map[uint64]bool)
	for _, t := range tasks {
		if t.ServiceID != id || t.DesiredState != "running" || t.State != "running" || t.Labels["berthy.operation"] != generation || t.Slot == 0 || t.ContainerID == "" {
			continue
		}
		health, err := e.InspectContainerHealth(ctx, t.ContainerID)
		if ctx.Err() != nil {
			break
		}
		if err == nil && health == "healthy" {
			slots[t.Slot] = true
		}
	}
	return len(slots)
}

// Slot numbers are Swarm's names for replicas. A service scaled down to one replica can still be running slot 2.
func slotsCover(slots map[uint64]bool, replicas int) bool { return len(slots) >= replicas }

// errStartAttempts reports that the new version used up its start attempts: a rollout stops waiting as soon as the
// generation has had limit failed task starts, instead of waiting out the deploy timeout.
type errStartAttempts struct {
	Role          string
	Failed, Limit int
}

func (e *errStartAttempts) Error() string { return "service gave up after failed starts" }

type startBudget struct {
	limit         int
	countComplete bool
}

// An uptime rollout also spends an attempt when a candidate exits cleanly. Swarm can already want that task shut down.
func startFailed(t engine.ServiceTask, budget startBudget) bool {
	return t.State == "failed" || t.State == "rejected" || budget.countComplete && t.State == "complete"
}

// waitHealthy renews startup time on a new high count of healthy slots. The hold ends only after a fresh healthy
// poll begun at or after its boundary. Losing coverage returns the wait to the last startup cutoff.
func waitHealthy(ctx context.Context, e healthReader, id, generation string, replicas int, budget startBudget, timing rolloutTiming) (time.Time, error) {
	pace := newPacer(e, serviceEvents(id))
	defer pace.close()
	readCutoff := timing.cutoff
	readCtx, stopRead := withRolloutCutoff(ctx, readCutoff)
	defer func() { stopRead() }()
	setReadCutoff := func(cutoff time.Time) {
		if !cutoff.Equal(readCutoff) {
			stopRead()
			readCutoff = cutoff
			readCtx, stopRead = withRolloutCutoff(ctx, cutoff)
		}
	}
	expired := errors.New("service health deadline exceeded")
	failed := map[string]bool{}
	best := 0
	var healthySince time.Time
	for {
		if readCtx.Err() != nil {
			return time.Time{}, expired
		}
		pollStarted := time.Now()
		tasks, err := e.ServiceTasks(readCtx, id)
		if readCtx.Err() != nil {
			return time.Time{}, expired
		}
		if err == nil {
			newFailure := false
			for _, t := range tasks {
				if t.ServiceID == id && t.Labels["berthy.operation"] == generation && startFailed(t, budget) && !failed[t.ID] {
					failed[t.ID] = true
					newFailure = true
				}
			}
			if budget.limit > 0 && len(failed) >= budget.limit {
				return time.Time{}, &errStartAttempts{Failed: len(failed), Limit: budget.limit}
			}
			if newFailure {
				healthySince = time.Time{}
				setReadCutoff(timing.cutoff)
			}
			if readCtx.Err() != nil {
				return time.Time{}, expired
			}
			healthy := healthyIn(readCtx, e, tasks, id, generation)
			if readCtx.Err() != nil {
				return time.Time{}, expired
			}
			if timing.window > 0 && healthy > best {
				best = healthy
				timing.cutoff = time.Now().Add(timing.window)
			}
			if healthy >= replicas {
				if healthySince.IsZero() {
					healthySince = time.Now()
				}
				if timing.hold == 0 || !pollStarted.Before(healthySince.Add(timing.hold)) {
					deadline, _ := readCtx.Deadline()
					return deadline, nil
				}
			} else {
				healthySince = time.Time{}
			}
		} else {
			healthySince = time.Time{}
		}
		var next time.Time
		cutoff := timing.cutoff
		if !healthySince.IsZero() {
			next = healthySince.Add(timing.hold)
			if !cutoff.IsZero() {
				cutoff = cutoff.Add(rolloutAllowance(timing.hold))
			}
		}
		setReadCutoff(cutoff)
		if !pace.waitUntil(readCtx, next) {
			return time.Time{}, expired
		}
	}
}

// dropEngineDefaults removes from a decoded service spec the non-zero defaults Docker adds when it stores a service, on
// create and on update alike: the "container" runtime and the "default" isolation. Zero values it omits are handled by
// canonicalSpec.
func dropEngineDefaults(spec map[string]any) {
	if task, ok := spec["TaskTemplate"].(map[string]any); ok {
		if task["Runtime"] == "container" {
			delete(task, "Runtime")
		}
		if c, ok := task["ContainerSpec"].(map[string]any); ok && c["Isolation"] == "default" {
			delete(c, "Isolation")
		}
	}
}

// matchesStoredSpec reports whether the service Docker stored from desired is live: the two specs are the same once the
// defaults Docker adds are set aside on both sides.
func matchesStoredSpec(desired, live json.RawMessage) bool {
	var d, l map[string]any
	dd := json.NewDecoder(bytes.NewReader(desired))
	dd.UseNumber()
	dl := json.NewDecoder(bytes.NewReader(live))
	dl.UseNumber()
	if dd.Decode(&d) != nil || dl.Decode(&l) != nil {
		return false
	}
	dropEngineDefaults(d)
	dropEngineDefaults(l)
	db, _ := json.Marshal(d) //nolint:errcheck // Defaults are removed from decoded JSON values only.
	lb, _ := json.Marshal(l) //nolint:errcheck // Defaults are removed from decoded JSON values only.
	return sameSpec(db, lb)
}

type rolloutEngine interface {
	serviceMutator
	healthReader
}

// withRole names the role whose rollout gave up; other errors pass through.
func withRole(err error, role string) error {
	var gave *errStartAttempts
	if errors.As(err, &gave) {
		gave.Role = role
	}
	return err
}

func createRole(ctx context.Context, j journal, e rolloutEngine, op string, i state.Intent, auth *engine.RegistryAuth, replicas int, budget startBudget, timing rolloutTiming) (string, error) {
	startupCtx, stopStartup := withRolloutCutoff(ctx, timing.cutoff)
	defer stopStartup()
	if startupCtx.Err() != nil {
		return "", beforeServiceMutation{errors.New("service health deadline exceeded")}
	}
	started := rolloutMilestone(i.Role, "started")
	i.Event = &started
	if err := j.RecordIntent(startupCtx, op, i); err != nil {
		if startupCtx.Err() != nil {
			return "", beforeServiceMutation{errors.New("service health deadline exceeded")}
		}
		return "", beforeServiceMutation{err}
	}
	if startupCtx.Err() != nil {
		return "", beforeServiceMutation{errors.New("service health deadline exceeded")}
	}
	createdID, err := e.CreateService(startupCtx, i.Desired, auth)
	if err != nil || createdID == "" {
		return "", errors.New("service create outcome uncertain")
	}
	live, err := e.InspectService(startupCtx, i.Name)
	if startupCtx.Err() != nil {
		return "", errors.New("service health deadline exceeded")
	}
	if err != nil || live.ID != createdID || !matchesStoredSpec(i.Desired, live.Spec) {
		return "", errors.New("created service cannot be verified")
	}
	receipt, err := json.Marshal(struct {
		ID   string          `json:"id"`
		Spec json.RawMessage `json:"spec"`
	}{live.ID, live.Spec})
	if err != nil {
		return "", err
	}
	err = j.MarkStep(startupCtx, op, i.Ordinal, "observed", receipt)
	if startupCtx.Err() != nil {
		return "", errors.New("service health deadline exceeded")
	}
	if err != nil {
		return "", err
	}
	stopStartup()
	deadline, err := waitHealthy(ctx, e, live.ID, i.Generation, replicas, budget, timing)
	if err != nil {
		return "", withRole(err, i.Role)
	}
	verifyCtx, stopVerification := withRolloutCutoff(ctx, deadline)
	defer stopVerification()
	if err = j.MarkStep(verifyCtx, op, i.Ordinal, "verified", receipt, rolloutMilestone(i.Role, "ready")); err != nil {
		return "", err
	}
	return live.ID, nil
}

func updateRole(ctx context.Context, j journal, e rolloutEngine, op string, i state.Intent, auth *engine.RegistryAuth, replicas int, budget startBudget, timing rolloutTiming) error {
	startupCtx, stopStartup := withRolloutCutoff(ctx, timing.cutoff)
	defer stopStartup()
	if startupCtx.Err() != nil {
		return beforeServiceMutation{errors.New("service health deadline exceeded")}
	}
	started := rolloutMilestone(i.Role, "started")
	i.Event = &started
	if err := applyIntent(startupCtx, j, e, op, i, auth); err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			deadline := errors.New("service health deadline exceeded")
			var before beforeServiceMutation
			if errors.As(err, &before) {
				return beforeServiceMutation{deadline}
			}
			return deadline
		}
		return err
	}
	live, err := e.InspectService(startupCtx, i.Name)
	if startupCtx.Err() != nil {
		return errors.New("service health deadline exceeded")
	}
	if err != nil || live.ID != i.ServiceID || !matchesStoredSpec(i.Desired, live.Spec) {
		return errors.New("updated service cannot be verified")
	}
	receipt, err := json.Marshal(struct {
		ID   string          `json:"id"`
		Spec json.RawMessage `json:"spec"`
	}{live.ID, live.Spec})
	if err != nil {
		return err
	}
	err = j.MarkStep(startupCtx, op, i.Ordinal, "observed", receipt)
	if startupCtx.Err() != nil {
		return errors.New("service health deadline exceeded")
	}
	if err != nil {
		return err
	}
	stopStartup()
	if _, err = waitHealthy(ctx, e, live.ID, i.Generation, replicas, budget, timing); err != nil {
		return withRole(err, i.Role)
	}
	// Add each duration separately so a long configured drain cannot overflow when the grace is added.
	drainCtx, stopDrain := context.WithDeadline(ctx, time.Now().Add(timing.drain).Add(rolloutVerificationGrace))
	defer stopDrain()
	if updateHasTasks(i.Before) {
		if err = waitUpdate(drainCtx, e, i.Name, i.ServiceID); err != nil {
			return err
		}
	}
	if err = waitDrain(drainCtx, e, live.ID, i.Generation); err != nil {
		return err
	}
	return j.MarkStep(drainCtx, op, i.Ordinal, "verified", receipt, rolloutMilestone(i.Role, "ready"))
}

func waitDrain(ctx context.Context, e healthReader, id, generation string) error {
	pace := newPacer(e, serviceEvents(id))
	defer pace.close()
	for {
		tasks, err := e.ServiceTasks(ctx, id)
		if err != nil {
			return errors.New("task inspection failed")
		}
		if ctx.Err() != nil {
			return errors.New("service drain deadline exceeded")
		}
		pending := false
		for _, t := range tasks {
			if t.ServiceID != id || t.Labels["berthy.operation"] == generation {
				continue
			}
			switch t.State {
			case "complete", "failed", "rejected", "shutdown", "orphaned", "remove":
			default:
				pending = true
			}
		}
		if !pending {
			return nil
		}
		if !pace.wait(ctx) {
			return errors.New("service drain deadline exceeded")
		}
	}
}

// emptyInherited reports whether an inherited container setting holds nothing: null, false, zero, an empty string, list or
// object, or an object whose fields are all such. It is the rule sameSpec uses, because Docker stores the settings it does
// not use with empty values, and both checks of an inherited service must read those values the same way.
func emptyInherited(v any) bool { return isZeroSpecValue(canonicalSpec(v)) }

// emptyInheritedRaw is emptyInherited for a setting still in JSON form. A missing setting is empty; one that cannot be read
// is not.
func emptyInheritedRaw(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return true
	}
	var v any
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	return d.Decode(&v) == nil && emptyInherited(v)
}

func validateInheritedRuntime(raw json.RawMessage, expected *expectedUptimeProbe) error {
	var x struct {
		TaskTemplate struct {
			Runtime       string
			ContainerSpec map[string]json.RawMessage
		}
	}
	if json.Unmarshal(raw, &x) != nil || x.TaskTemplate.ContainerSpec == nil {
		return errors.New("invalid service runtime")
	}
	if x.TaskTemplate.Runtime != "" && x.TaskTemplate.Runtime != "container" {
		return errors.New("unsafe service runtime")
	}
	allowed := map[string]bool{"Image": true, "Labels": true, "Command": true, "Args": true, "Env": true, "Dir": true, "User": true, "Groups": true, "ReadOnly": true, "Mounts": true, "StopGracePeriod": true, "Healthcheck": true, "Isolation": true, "Hostname": true}
	for k, v := range x.TaskTemplate.ContainerSpec {
		if !allowed[k] && !emptyInheritedRaw(v) {
			return errors.New("unsupported inherited container settings")
		}
	}
	for _, k := range []string{"CapabilityAdd", "CapabilityDrop", "Privileges", "Secrets", "Configs", "DNSConfig", "Hosts", "Sysctls"} {
		if !emptyInheritedRaw(x.TaskTemplate.ContainerSpec[k]) {
			return errors.New("unsafe inherited container settings")
		}
	}
	foundProbe := false
	if rawMounts := x.TaskTemplate.ContainerSpec["Mounts"]; len(rawMounts) > 0 {
		var mounts []json.RawMessage
		if json.Unmarshal(rawMounts, &mounts) != nil {
			return errors.New("invalid mounts")
		}
		for _, m := range mounts {
			var mount struct{ Type string }
			if json.Unmarshal(m, &mount) != nil {
				return errors.New("invalid mounts")
			}
			if mount.Type == "volume" {
				if expected == nil {
					continue
				}
				var volume struct{ Target string }
				if json.Unmarshal(m, &volume) != nil || uptimeMountOverlap(volume.Target) {
					return errors.New("unsafe inherited mount")
				}
				continue
			}
			var bind map[string]any
			if expected == nil || foundProbe || json.Unmarshal(m, &bind) != nil || !reflect.DeepEqual(bind, expected.mount) {
				return errors.New("unsafe inherited mount")
			}
			foundProbe = true
		}
	}
	if expected != nil {
		if !foundProbe {
			return errors.New("managed uptime mount unavailable")
		}
		var health struct{ Test []string }
		if json.Unmarshal(x.TaskTemplate.ContainerSpec["Healthcheck"], &health) != nil || !slices.Equal(health.Test, expected.test) {
			return errors.New("managed uptime health command changed")
		}
	}
	var outer struct {
		EndpointSpec struct{ Ports []json.RawMessage }
	}
	if json.Unmarshal(raw, &outer) != nil || len(outer.EndpointSpec.Ports) > 0 {
		return errors.New("published ports unsupported")
	}
	return nil
}

func waitUpdate(ctx context.Context, e serviceMutator, name, id string) error {
	pace := newPacer(e, serviceEvents(id))
	defer pace.close()
	for {
		live, err := e.InspectService(ctx, name)
		if err != nil || live.ID != id {
			return errors.New("update status unavailable")
		}
		if ctx.Err() != nil {
			return errors.New("update completion deadline exceeded")
		}
		switch live.UpdateStatus.State {
		case "completed":
			return nil
		case "rollback_started", "rollback_paused", "rollback_completed", "paused":
			return errors.New("native update did not complete")
		}
		if !pace.wait(ctx) {
			return errors.New("update completion deadline exceeded")
		}
	}
}

// updateHasTasks reports whether the spec an update replaces ran any replicas. Swarm reports an update's status only
// for running tasks it replaces, so an update of a service stopped at zero replicas never reports "completed": it has
// no update to wait for. A spec that cannot be read keeps the wait, the safe default.
func updateHasTasks(before json.RawMessage) bool {
	var v struct {
		Mode struct{ Replicated *struct{ Replicas int } }
	}
	if json.Unmarshal(before, &v) != nil || v.Mode.Replicated == nil {
		return true
	}
	return v.Mode.Replicated.Replicas != 0
}

func healthyImageOnce(ctx context.Context, e healthReader, id, image string, replicas int) error {
	tasks, err := e.ServiceTasks(ctx, id)
	if err != nil {
		return errors.New("task inspection failed")
	}
	slots := map[uint64]bool{}
	for _, t := range tasks {
		if t.ServiceID == id && t.DesiredState == "running" && t.State == "running" && t.Image == image && t.ContainerID != "" && t.Slot > 0 {
			h, e := e.InspectContainerHealth(ctx, t.ContainerID)
			if e == nil && h == "healthy" {
				slots[t.Slot] = true
			}
		}
	}
	if !slotsCover(slots, replicas) {
		return errors.New("predecessor image is not healthy")
	}
	return nil
}
func waitImageHealthy(ctx context.Context, e healthReader, id, image string, replicas int) error {
	pace := newPacer(e, serviceEvents(id))
	defer pace.close()
	for {
		if healthyImageOnce(ctx, e, id, image, replicas) == nil {
			return nil
		}
		if !pace.wait(ctx) {
			return errors.New("predecessor health deadline exceeded")
		}
	}
}
