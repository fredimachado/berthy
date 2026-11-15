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
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
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
	if json.Unmarshal(desired, &d) != nil || json.Unmarshal(live, &l) != nil {
		return false
	}
	task, ok := l["TaskTemplate"].(map[string]any)
	if ok {
		if task["Runtime"] == "container" {
			delete(task, "Runtime")
		}
		if task["ForceUpdate"] == float64(0) {
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
