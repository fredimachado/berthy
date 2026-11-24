package deploy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"

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
	ax, _ := json.Marshal(canonicalSpec(x))
	by, _ := json.Marshal(canonicalSpec(y))
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

func applyIntent(ctx context.Context, j journal, e serviceMutator, op string, i state.Intent, auth *engine.RegistryAuth) error {
	if err := j.RecordIntent(ctx, op, i); err != nil {
		return err
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
		if err != nil || live.ID != i.ServiceID || !sameSpec(live.Spec, i.Before) {
			return errors.New("service changed before update")
		}
		if err = e.UpdateService(ctx, live.ID, live.Version, i.Desired, auth); err != nil {
			live, inspectErr := e.InspectService(ctx, i.Name)
			if inspectErr != nil || live.ID != i.ServiceID || !matchesStoredSpec(i.Desired, live.Spec) {
				return errors.New("service update outcome is uncertain")
			}
		}
	} else {
		return errors.New("invalid service intent")
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

func healthyOnce(ctx context.Context, e healthReader, id string, generation string, replicas int) error {
	tasks, err := e.ServiceTasks(ctx, id)
	if err != nil {
		return errors.New("task inspection failed")
	}
	return healthyIn(ctx, e, tasks, id, generation, replicas)
}

// healthyIn reports whether every replica slot of the generation has a running task whose container is healthy.
func healthyIn(ctx context.Context, e healthReader, tasks []engine.ServiceTask, id string, generation string, replicas int) error {
	slots := make(map[uint64]bool)
	for _, t := range tasks {
		if t.ServiceID != id || t.DesiredState != "running" || t.State != "running" || t.Labels["berthy.operation"] != generation || t.Slot == 0 || t.ContainerID == "" {
			continue
		}
		health, err := e.InspectContainerHealth(ctx, t.ContainerID)
		if err == nil && health == "healthy" {
			slots[t.Slot] = true
		}
	}
	if !slotsCover(slots, replicas) {
		return errors.New("service generation is not healthy")
	}
	return nil
}

// slotsCover reports whether the healthy slots are as many as the replicas. Slot numbers are Swarm's names for replicas,
// not positions: a service scaled up and down by hand keeps the replica that started last, which can be slot 2 of 1.
func slotsCover(slots map[uint64]bool, replicas int) bool { return len(slots) >= replicas }

// errStartAttempts reports that the new version used up its start attempts: a rollout stops waiting as soon as the
// generation has had limit failed task starts, instead of waiting out the deploy timeout.
type errStartAttempts struct {
	Role          string
	Failed, Limit int
}

func (e *errStartAttempts) Error() string { return "service gave up after failed starts" }

// startFailed is a task that ended in error: its container exited non-zero or was killed as unhealthy, or Swarm refused to
// start it.
func startFailed(t engine.ServiceTask) bool { return t.State == "failed" || t.State == "rejected" }

// waitHealthy waits until every replica of the generation runs healthy, the context ends, or, with a positive limit, the
// generation has had limit failed tasks. Swarm keeps few old tasks, so failures are counted by task ID across polls.
func waitHealthy(ctx context.Context, e healthReader, id string, generation string, replicas, limit int) error {
	pace := newPacer(e, serviceEvents(id))
	defer pace.close()
	failed := map[string]bool{}
	for {
		tasks, err := e.ServiceTasks(ctx, id)
		if err == nil {
			if healthyIn(ctx, e, tasks, id, generation, replicas) == nil {
				return nil
			}
			for _, t := range tasks {
				if t.ServiceID == id && t.Labels["berthy.operation"] == generation && startFailed(t) {
					failed[t.ID] = true
				}
			}
			if limit > 0 && len(failed) >= limit {
				return &errStartAttempts{Failed: len(failed), Limit: limit}
			}
		}
		if !pace.wait(ctx) {
			return errors.New("service health deadline exceeded")
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
	db, _ := json.Marshal(d)
	lb, _ := json.Marshal(l)
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

func createRole(ctx context.Context, j journal, e rolloutEngine, op string, i state.Intent, auth *engine.RegistryAuth, replicas, limit int) (string, error) {
	started := rolloutMilestone(i.Role, "started")
	i.Event = &started
	if err := j.RecordIntent(ctx, op, i); err != nil {
		return "", err
	}
	createdID, err := e.CreateService(ctx, i.Desired, auth)
	if err != nil || createdID == "" {
		return "", errors.New("service create outcome uncertain")
	}
	live, err := e.InspectService(ctx, i.Name)
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
	if err = j.MarkStep(ctx, op, i.Ordinal, "observed", receipt); err != nil {
		return "", err
	}
	if err = waitHealthy(ctx, e, live.ID, i.Generation, replicas, limit); err != nil {
		return "", withRole(err, i.Role)
	}
	if err = j.MarkStep(ctx, op, i.Ordinal, "verified", receipt, rolloutMilestone(i.Role, "ready")); err != nil {
		return "", err
	}
	return live.ID, nil
}

func updateRole(ctx context.Context, j journal, e rolloutEngine, op string, i state.Intent, auth *engine.RegistryAuth, replicas, limit int) error {
	started := rolloutMilestone(i.Role, "started")
	i.Event = &started
	if err := applyIntent(ctx, j, e, op, i, auth); err != nil {
		return err
	}
	live, err := e.InspectService(ctx, i.Name)
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
	if err = j.MarkStep(ctx, op, i.Ordinal, "observed", receipt); err != nil {
		return err
	}
	if err = waitHealthy(ctx, e, live.ID, i.Generation, replicas, limit); err != nil {
		return withRole(err, i.Role)
	}
	if updateHasTasks(i.Before) {
		if err = waitUpdate(ctx, e, i.Name, i.ServiceID); err != nil {
			return err
		}
	}
	if err = waitDrain(ctx, e, live.ID, i.Generation); err != nil {
		return err
	}
	return j.MarkStep(ctx, op, i.Ordinal, "verified", receipt, rolloutMilestone(i.Role, "ready"))
}

func waitDrain(ctx context.Context, e healthReader, id, generation string) error {
	pace := newPacer(e, serviceEvents(id))
	defer pace.close()
	for {
		tasks, err := e.ServiceTasks(ctx, id)
		if err != nil {
			return errors.New("task inspection failed")
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

func validateInheritedRuntime(raw json.RawMessage) error {
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
	if rawMounts := x.TaskTemplate.ContainerSpec["Mounts"]; len(rawMounts) > 0 && string(rawMounts) != "null" {
		var mounts []struct{ Type string }
		if json.Unmarshal(rawMounts, &mounts) != nil {
			return errors.New("invalid mounts")
		}
		for _, m := range mounts {
			if m.Type != "volume" {
				return errors.New("unsafe inherited mount")
			}
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
