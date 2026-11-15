package deploy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"berthy/internal/engine"
	"berthy/internal/manifest"
	"berthy/internal/state"
	"github.com/distribution/reference"
)

func runtimeSpec(raw json.RawMessage, b state.Binding, environment, operation, kind string, count *int) (json.RawMessage, int, error) {
	var spec map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&spec); err != nil {
		return nil, 0, errors.New("invalid service spec")
	}
	if spec["Name"] != b.Name {
		return nil, 0, errors.New("service identity mismatch")
	}
	task, ok := spec["TaskTemplate"].(map[string]any)
	if !ok {
		return nil, 0, errors.New("invalid service task")
	}
	container, ok := task["ContainerSpec"].(map[string]any)
	if !ok {
		return nil, 0, errors.New("invalid service container")
	}
	mode, ok := spec["Mode"].(map[string]any)
	if !ok {
		return nil, 0, errors.New("service is not replicated")
	}
	replicated, ok := mode["Replicated"].(map[string]any)
	if !ok {
		return nil, 0, errors.New("service is not replicated")
	}
	num, ok := replicated["Replicas"].(json.Number)
	if !ok {
		return nil, 0, errors.New("invalid replica count")
	}
	n, err := strconv.Atoi(string(num))
	if err != nil || n < 1 || n > 1000 {
		return nil, 0, errors.New("invalid replica count")
	}
	for _, target := range []map[string]any{spec, container} {
		labels, ok := target["Labels"].(map[string]any)
		if !ok {
			if target["Labels"] != nil {
				return nil, 0, errors.New("invalid labels")
			}
			labels = map[string]any{}
			target["Labels"] = labels
		}
		for key, value := range map[string]string{"berthy.environment": environment, "berthy.app": b.App, "berthy.role": b.Role} {
			if existing, ok := labels[key]; ok && existing != value {
				return nil, 0, errors.New("service ownership mismatch")
			}
			labels[key] = value
		}
		labels["berthy.operation"] = operation
	}
	switch kind {
	case "restart":
		var current uint64
		if v, ok := task["ForceUpdate"]; ok {
			num, ok := v.(json.Number)
			if !ok {
				return nil, 0, errors.New("invalid restart counter")
			}
			current, err = strconv.ParseUint(string(num), 10, 64)
			if err != nil || current == ^uint64(0) {
				return nil, 0, errors.New("invalid restart counter")
			}
		}
		task["ForceUpdate"] = current + 1
	case "scale":
		if count == nil || *count < 1 || *count > 1000 {
			return nil, 0, errors.New("invalid replica count")
		}
		n = *count
		replicated["Replicas"] = n
	default:
		return nil, 0, errors.New("unsupported operation")
	}
	out, err := json.Marshal(spec)
	return out, n, err
}
func (c *Controller) operationBinding(ctx context.Context, app, role string) (state.Binding, error) {
	if c.Store == nil || c.Engine == nil || app == "" {
		return state.Binding{}, errors.New("managed app unavailable")
	}
	bindings, err := c.Store.Bindings(ctx, app)
	if err != nil {
		return state.Binding{}, err
	}
	if role == "" {
		if len(bindings) != 1 {
			return state.Binding{}, errors.New("role required")
		}
		return bindings[0], nil
	}
	for _, b := range bindings {
		if b.Role == role {
			return b, nil
		}
	}
	return state.Binding{}, errors.New("role not found")
}
func (c *Controller) inspectOperation(ctx context.Context, req Request, operation string) (state.Binding, engine.ServiceSnapshot, json.RawMessage, int, error) {
	b, err := c.operationBinding(ctx, req.App, req.Role)
	if err != nil {
		return b, engine.ServiceSnapshot{}, nil, 0, err
	}
	live, err := c.Engine.InspectService(ctx, b.Name)
	if err != nil || live.ID != b.ServiceID {
		return b, live, nil, 0, errors.New("bound service unavailable")
	}
	desired, n, err := runtimeSpec(live.Spec, b, c.Environment, operation, req.Kind, req.Replicas)
	return b, live, desired, n, err
}
func (c *Controller) planOperation(ctx context.Context, actor Actor, req Request) (Plan, error) {
	switch req.Kind {
	case "restart", "scale":
		b, _, _, _, err := c.inspectOperation(ctx, req, "plan")
		if err != nil {
			return Plan{}, err
		}
		return Plan{App: req.App, Roles: []string{b.Role}}, nil
	default:
		return Plan{}, errors.New("operation unavailable")
	}
}
func (c *Controller) operationAuth(ctx context.Context, app string, spec json.RawMessage) (*engine.RegistryAuth, error) {
	r, err := c.Store.Applied(ctx, app)
	if err != nil {
		return nil, errors.New("applied release unavailable")
	}
	var m manifest.Manifest
	if err = json.Unmarshal(r.Manifest, &m); err != nil {
		return nil, errors.New("applied manifest unavailable")
	}
	var v struct {
		TaskTemplate struct{ ContainerSpec struct{ Image string } }
	}
	if err = json.Unmarshal(spec, &v); err != nil {
		return nil, errors.New("service image unavailable")
	}
	ref, err := reference.ParseNormalizedNamed(v.TaskTemplate.ContainerSpec.Image)
	if err != nil {
		return nil, errors.New("service image unavailable")
	}
	host := reference.Domain(ref)
	reg, ok := m.Registries[host]
	if !ok {
		return nil, nil
	}
	info, value, err := c.Store.Secret(ctx, reg.PasswordSecret)
	if err != nil || info.Registry != host {
		return nil, errors.New("registry credential unavailable")
	}
	return &engine.RegistryAuth{Username: reg.Username, Password: string(value), ServerAddress: host}, nil
}
func (c *Controller) runOperation(ctx context.Context, op state.Operation, actor Actor, req Request) state.Outcome {
	if req.Kind != "restart" && req.Kind != "scale" {
		return state.Outcome{State: "failed", Err: "operation unavailable"}
	}
	b, live, desired, n, err := c.inspectOperation(ctx, req, op.ID)
	if err != nil {
		return state.Outcome{State: "failed", Err: "operation preflight failed"}
	}
	auth, err := c.operationAuth(ctx, req.App, live.Spec)
	if err != nil {
		return state.Outcome{State: "failed", Err: "operation preflight failed"}
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	i := state.Intent{Ordinal: 1, Kind: "update", Role: b.Role, Name: b.Name, ServiceID: b.ServiceID, SeenVersion: live.Version, Generation: op.ID, Deadline: time.Now().Add(2 * time.Minute), Before: live.Spec, Desired: desired}
	if err = applyIntent(ctx, c.Store, c.Engine, op.ID, i, auth); err != nil {
		return state.Outcome{State: "recovery_required", Err: "service update requires recovery"}
	}
	if err = waitHealthy(ctx, c.Engine, b.ServiceID, op.ID, n); err != nil {
		return state.Outcome{State: "recovery_required", Err: "service health requires recovery"}
	}
	if err = c.Store.MarkStep(ctx, op.ID, i.Ordinal, "verified", json.RawMessage(`{}`)); err != nil {
		return state.Outcome{State: "recovery_required", Err: "service journal requires recovery"}
	}
	return state.Outcome{State: "succeeded"}
}
func (c *Controller) Wait(ctx context.Context, app, role string) error {
	b, err := c.operationBinding(ctx, app, role)
	if err != nil {
		return err
	}
	live, err := c.Engine.InspectService(ctx, b.Name)
	if err != nil || live.ID != b.ServiceID {
		return errors.New("bound service unavailable")
	}
	var v struct {
		Name         string
		TaskTemplate struct {
			ContainerSpec struct{ Labels map[string]string }
		}
		Mode struct{ Replicated *struct{ Replicas int } }
	}
	if json.Unmarshal(live.Spec, &v) != nil || v.Name != b.Name || v.Mode.Replicated == nil || v.Mode.Replicated.Replicas < 1 {
		return errors.New("invalid bound service")
	}
	generation := v.TaskTemplate.ContainerSpec.Labels["berthy.operation"]
	if generation == "" {
		return errors.New("service generation unavailable")
	}
	if _, ok := ctx.Deadline(); !ok {
		return fmt.Errorf("wait requires a deadline")
	}
	return waitHealthy(ctx, c.Engine, b.ServiceID, generation, v.Mode.Replicated.Replicas)
}
func redactOutput(output []byte, secrets [][]byte) string {
	for _, secret := range secrets {
		if len(secret) > 0 {
			output = bytes.ReplaceAll(output, secret, []byte("[redacted]"))
		}
	}
	if len(output) > 65536 {
		output = output[:65536]
	}
	return string(bytes.ToValidUTF8(output, []byte("?")))
}
