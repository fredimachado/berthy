package deploy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"time"

	"berthy/internal/engine"
	"berthy/internal/state"
)

type journal interface {
	RecordIntent(context.Context, string, state.Intent) error
	MarkStep(context.Context, string, int, string, json.RawMessage) error
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
			if inspectErr != nil || !sameSpec(live.Spec, i.Desired) {
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
			if inspectErr != nil || live.ID != i.ServiceID || !sameSpec(live.Spec, i.Desired) {
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
	if (i.Kind == "update" && live.ID != i.ServiceID) || !sameSpec(live.Spec, i.Desired) {
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
	for slot := 1; slot <= replicas; slot++ {
		if !slots[uint64(slot)] {
			return errors.New("service generation is not healthy")
		}
	}
	return nil
}

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
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
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
		select {
		case <-ctx.Done():
			return errors.New("service health deadline exceeded")
		case <-ticker.C:
		}
	}
}

// Docker adds these non-zero defaults when it stores a newly created service. Zero values it omits are handled by sameSpec.
func matchesCreatedSpec(desired, live json.RawMessage) bool {
	var d, l map[string]any
	dd := json.NewDecoder(bytes.NewReader(desired))
	dd.UseNumber()
	dl := json.NewDecoder(bytes.NewReader(live))
	dl.UseNumber()
	if dd.Decode(&d) != nil || dl.Decode(&l) != nil {
		return false
	}
	task, ok := l["TaskTemplate"].(map[string]any)
	if ok {
		if task["Runtime"] == "container" {
			delete(task, "Runtime")
		}
		if c, ok := task["ContainerSpec"].(map[string]any); ok && c["Isolation"] == "default" {
			delete(c, "Isolation")
		}
	}
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
	if err := j.RecordIntent(ctx, op, i); err != nil {
		return "", err
	}
	createdID, err := e.CreateService(ctx, i.Desired, auth)
	if err != nil || createdID == "" {
		return "", errors.New("service create outcome uncertain")
	}
	live, err := e.InspectService(ctx, i.Name)
	if err != nil || live.ID != createdID || !matchesCreatedSpec(i.Desired, live.Spec) {
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
	if err = j.MarkStep(ctx, op, i.Ordinal, "verified", receipt); err != nil {
		return "", err
	}
	return live.ID, nil
}

// mergeServiceSpec preserves fields not controlled by the manifest after checking inherited host access.
func mergeServiceSpec(before, desired json.RawMessage, network string) (json.RawMessage, error) {
	if err := validateInheritedRuntime(before); err != nil {
		return nil, err
	}
	var old, want map[string]any
	d := json.NewDecoder(bytes.NewReader(before))
	d.UseNumber()
	if d.Decode(&old) != nil {
		return nil, errors.New("invalid live service spec")
	}
	d = json.NewDecoder(bytes.NewReader(desired))
	d.UseNumber()
	if d.Decode(&want) != nil {
		return nil, errors.New("invalid desired service spec")
	}
	if old["Name"] != want["Name"] {
		return nil, errors.New("service identity mismatch")
	}
	ot, ok := old["TaskTemplate"].(map[string]any)
	if !ok {
		return nil, errors.New("invalid task template")
	}
	wt, ok := want["TaskTemplate"].(map[string]any)
	if !ok {
		return nil, errors.New("invalid task template")
	}
	oc, ok := ot["ContainerSpec"].(map[string]any)
	if !ok {
		return nil, errors.New("invalid container")
	}
	wc, ok := wt["ContainerSpec"].(map[string]any)
	if !ok {
		return nil, errors.New("invalid container")
	}
	for _, k := range []string{"Privileges", "Secrets", "Configs", "DNSConfig", "Hosts", "Init", "Sysctls", "Isolation"} {
		if v, exists := oc[k]; exists && v != nil && !(k == "Isolation" && v == "default") {
			return nil, errors.New("unsafe inherited container settings")
		}
	}
	if mounts, exists := oc["Mounts"]; exists && mounts != nil {
		a, ok := mounts.([]any)
		if !ok {
			return nil, errors.New("invalid mounts")
		}
		for _, v := range a {
			m, ok := v.(map[string]any)
			if !ok || m["Type"] != "volume" {
				return nil, errors.New("unsafe inherited mount")
			}
		}
	}
	if ep, ok := old["EndpointSpec"].(map[string]any); ok {
		if p, exists := ep["Ports"]; exists && p != nil {
			a, ok := p.([]any)
			if !ok || len(a) > 0 {
				return nil, errors.New("published ports are unsupported")
			}
		}
	}
	networks, ok := ot["Networks"].([]any)
	if !ok || len(networks) != 1 {
		return nil, errors.New("incompatible service network")
	}
	n, ok := networks[0].(map[string]any)
	if !ok || n["Target"] != network {
		return nil, errors.New("incompatible service network")
	}
	if mode, ok := old["Mode"].(map[string]any); !ok || mode["Replicated"] == nil {
		return nil, errors.New("service is not replicated")
	}
	for _, k := range []string{"Image", "Env", "Healthcheck", "StopGracePeriod", "Labels", "Mounts", "Args"} {
		if v, ok := wc[k]; ok {
			oc[k] = v
		} else {
			delete(oc, k)
		}
	}
	for _, k := range []string{"Resources", "RestartPolicy"} {
		if v, ok := wt[k]; ok {
			ot[k] = v
		} else {
			delete(ot, k)
		}
	}
	for _, k := range []string{"Labels", "Mode", "UpdateConfig", "RollbackConfig"} {
		if v, ok := want[k]; ok {
			old[k] = v
		}
	}
	return json.Marshal(old)
}

func updateRole(ctx context.Context, j journal, e rolloutEngine, op string, i state.Intent, auth *engine.RegistryAuth, replicas, limit int) error {
	if err := applyIntent(ctx, j, e, op, i, auth); err != nil {
		return err
	}
	live, err := e.InspectService(ctx, i.Name)
	if err != nil || live.ID != i.ServiceID || !sameSpec(live.Spec, i.Desired) {
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
	if err = waitUpdate(ctx, e, i.Name, i.ServiceID); err != nil {
		return err
	}
	if err = waitDrain(ctx, e, live.ID, i.Generation); err != nil {
		return err
	}
	return j.MarkStep(ctx, op, i.Ordinal, "verified", receipt)
}

func waitDrain(ctx context.Context, e healthReader, id, generation string) error {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
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
		select {
		case <-ctx.Done():
			return errors.New("service drain deadline exceeded")
		case <-ticker.C:
		}
	}
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
		if !allowed[k] && len(v) > 0 && string(v) != "null" && string(v) != "[]" && string(v) != "{}" && string(v) != "false" {
			return errors.New("unsupported inherited container settings")
		}
	}
	for _, k := range []string{"CapabilityAdd", "CapabilityDrop", "Privileges", "Secrets", "Configs", "DNSConfig", "Hosts", "Sysctls"} {
		v := x.TaskTemplate.ContainerSpec[k]
		if len(v) > 0 && string(v) != "null" && string(v) != "[]" && string(v) != "{}" {
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
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
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
		select {
		case <-ctx.Done():
			return errors.New("update completion deadline exceeded")
		case <-ticker.C:
		}
	}
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
	for slot := 1; slot <= replicas; slot++ {
		if !slots[uint64(slot)] {
			return errors.New("predecessor image is not healthy")
		}
	}
	return nil
}
func waitImageHealthy(ctx context.Context, e healthReader, id, image string, replicas int) error {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		if healthyImageOnce(ctx, e, id, image, replicas) == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return errors.New("predecessor health deadline exceeded")
		case <-ticker.C:
		}
	}
}
