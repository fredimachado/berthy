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

func sameSpec(a, b json.RawMessage) bool {
	var x, y any
	da := json.NewDecoder(bytes.NewReader(a))
	da.UseNumber()
	db := json.NewDecoder(bytes.NewReader(b))
	db.UseNumber()
	if da.Decode(&x) != nil || db.Decode(&y) != nil {
		return false
	}
	ax, _ := json.Marshal(x)
	by, _ := json.Marshal(y)
	return bytes.Equal(ax, by)
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

func waitHealthy(ctx context.Context, e healthReader, id string, generation string, replicas int) error {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		if healthyOnce(ctx, e, id, generation, replicas) == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return errors.New("service health deadline exceeded")
		case <-ticker.C:
		}
	}
}

// Docker adds these defaults when it stores a newly created service.
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
		if task["ForceUpdate"] == json.Number("0") {
			delete(task, "ForceUpdate")
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

func createRole(ctx context.Context, j journal, e rolloutEngine, op string, i state.Intent, auth *engine.RegistryAuth, replicas int) (string, error) {
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
	if err = waitHealthy(ctx, e, live.ID, i.Generation, replicas); err != nil {
		return "", err
	}
	if err = j.MarkStep(ctx, op, i.Ordinal, "verified", receipt); err != nil {
		return "", err
	}
	return live.ID, nil
}

// mergeServiceSpec preserves fields not controlled by the manifest after checking inherited host access.
func mergeServiceSpec(before, desired json.RawMessage, network string) (json.RawMessage, error) {
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
	for _, k := range []string{"Resources"} {
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

func updateRole(ctx context.Context, j journal, e rolloutEngine, op string, i state.Intent, auth *engine.RegistryAuth, replicas int) error {
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
	if err = waitHealthy(ctx, e, live.ID, i.Generation, replicas); err != nil {
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
