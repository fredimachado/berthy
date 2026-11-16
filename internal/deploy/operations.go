package deploy

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
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
	if err = validateInheritedRuntime(live.Spec); err != nil {
		return b, live, nil, 0, err
	}
	desired, n, err := runtimeSpec(live.Spec, b, c.Environment, operation, req.Kind, req.Replicas)
	return b, live, desired, n, err
}
func (c *Controller) planOperation(ctx context.Context, actor Actor, req Request) (Plan, error) {
	if req.Kind == "exec" || req.Kind == "restart" || req.Kind == "scale" {
		m, compose, err := c.appliedCompose(ctx, req.App)
		if err != nil {
			return Plan{}, err
		}
		if compose {
			return c.planComposeOperation(ctx, req, m)
		}
	}
	switch req.Kind {
	case "remove":
		return c.planRemove(ctx, actor, req)
	case "destroy":
		return c.planDestroy(ctx, actor, req)
	case restoreKind:
		return c.planRestore(ctx, actor, req)
	case "exec":
		if !actor.Admin {
			return Plan{}, errors.New("administrator required")
		}
		if _, err := c.execTarget(ctx, req); err != nil {
			return Plan{}, err
		}
		return Plan{App: req.App}, nil
	case "prune":
		if !actor.Admin {
			return Plan{}, errors.New("administrator required")
		}
		if _, err := c.prunePlan(ctx); err != nil {
			return Plan{}, err
		}
		if _, err := c.prunableHooks(ctx); err != nil {
			return Plan{}, err
		}
		return Plan{}, nil
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
	if req.Kind == "remove" {
		return c.runRemove(ctx, op, actor, req)
	}
	if req.Kind == "destroy" {
		return c.runDestroy(ctx, op, actor, req)
	}
	if req.Kind == restoreKind {
		return c.runRestore(ctx, op, actor, req)
	}
	if req.Kind == "exec" {
		return c.runExec(ctx, op, actor, req)
	}
	if req.Kind == "prune" {
		if !actor.Admin {
			return state.Outcome{State: "failed", Err: "administrator required"}
		}
		active, err := c.Store.Active(ctx)
		if err != nil || active.ID != op.ID {
			return state.Outcome{State: "failed", Err: "operation ownership unavailable"}
		}
		if _, failure := c.pruneNow(ctx); failure != "" {
			return state.Outcome{State: "failed", Err: failure}
		}
		return state.Outcome{State: "succeeded"}
	}
	if req.Kind != "restart" && req.Kind != "scale" {
		return state.Outcome{State: "failed", Err: "operation unavailable"}
	}
	m, compose, err := c.appliedCompose(ctx, req.App)
	if err != nil {
		return state.Outcome{State: "failed", Err: "operation preflight failed"}
	}
	if compose {
		if req.Kind == "scale" {
			return state.Outcome{State: "failed", Err: string(errScaleCompose)}
		}
		return c.runComposeRestart(ctx, m, req)
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
	if err = waitHealthy(ctx, c.Engine, b.ServiceID, op.ID, n, 0); err != nil {
		return state.Outcome{State: "recovery_required", Err: "service health requires recovery"}
	}
	if err = waitDrain(ctx, c.Engine, b.ServiceID, op.ID); err != nil {
		return state.Outcome{State: "recovery_required", Err: "service drain requires recovery"}
	}
	if err = c.Store.MarkStep(ctx, op.ID, i.Ordinal, "verified", json.RawMessage(`{}`)); err != nil {
		return state.Outcome{State: "recovery_required", Err: "service journal requires recovery"}
	}
	return state.Outcome{State: "succeeded"}
}
func (c *Controller) Wait(ctx context.Context, app, role string) error {
	if _, ok := ctx.Deadline(); !ok {
		return errors.New("wait requires a deadline")
	}
	if c.Store == nil || c.Engine == nil || app == "" {
		return errors.New("managed app unavailable")
	}
	bindings, err := c.Store.Bindings(ctx, app)
	if err != nil {
		return err
	}
	if len(bindings) == 0 {
		return errors.New("managed app unavailable")
	}
	matched := false
	for _, b := range bindings {
		if role != "" && b.Role != role {
			continue
		}
		matched = true
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
		if err = waitHealthy(ctx, c.Engine, b.ServiceID, generation, v.Mode.Replicated.Replicas, 0); err != nil {
			return err
		}
	}
	if !matched {
		return errors.New("role not found")
	}
	return nil
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

type inventoryEngine interface {
	InventoryImages(context.Context) ([]engine.InventoryImage, error)
	InventoryContainers(context.Context) ([]engine.InventoryContainer, error)
	InventoryServices(context.Context) ([]engine.InventoryService, error)
	RemoveImageRef(context.Context, string) error
}

func pruneCandidates(owned []state.AcquiredImage, refs, ids map[string]bool) []state.AcquiredImage {
	var out []state.AcquiredImage
	for _, i := range owned {
		if !refs[i.Ref] && !ids[i.ID] {
			out = append(out, i)
		}
	}
	return out
}

// errPruneHistory is the failure to read what the state holds about releases and acquired images.
var errPruneHistory = errors.New("release history unavailable")

// inventoryFailure names why an inventory could not be read: Docker itself, or the one inventory.
func inventoryFailure(err error, what string) error {
	if strings.Contains(err.Error(), "Docker unavailable") {
		return errors.New("Docker unavailable")
	}
	return errors.New(what)
}

// pruneFailure is the public reason a prune could not plan: one of the fixed inventory texts, never a raw error.
func pruneFailure(err error) string {
	switch err.Error() {
	case "Docker unavailable", "image inventory unavailable", "container inventory unavailable", "service inventory unavailable", "container image unknown", "service image unknown", "release history unavailable":
		return err.Error()
	}
	if strings.HasPrefix(err.Error(), "applied ") {
		return "release history unavailable"
	}
	return "image inventory unavailable"
}

// pruneNow removes the finished hook containers Berthy journaled, and the images Berthy acquired that no container or
// service uses and no retained release needs. It returns the references of the images it removed, and a public failure
// text, which is empty on success.
func (c *Controller) pruneNow(ctx context.Context) (removed []string, failure string) {
	candidates, err := c.prunePlan(ctx)
	if err != nil {
		return nil, pruneFailure(err)
	}
	hooks, err := c.prunableHooks(ctx)
	if err != nil {
		return nil, "hook inventory unavailable"
	}
	if len(hooks) > 0 {
		he := c.Engine.(hookPruner)
		for _, h := range hooks {
			container, e := he.InspectContainer(ctx, h.ID)
			if e != nil {
				var status *engine.StatusError
				if errors.As(e, &status) && status.Code == 404 {
					continue
				}
				return nil, "hook inspection failed"
			}
			if !ownedFinishedHook(container, h, c.Environment) {
				continue
			}
			if e = he.RemoveContainer(ctx, h.ID); e != nil {
				return nil, "hook removal failed"
			}
		}
	}

	e := c.Engine.(inventoryEngine)
	for _, i := range candidates {
		current, err := c.Engine.InspectImage(ctx, i.Ref)
		if err != nil || current.ID != i.ID {
			return removed, "image identity changed"
		}
		if err = e.RemoveImageRef(ctx, i.Ref); err != nil {
			return removed, "image removal failed"
		}
		if err = c.Store.ForgetImage(ctx, i.Ref, i.ID); err != nil {
			return removed, "image journal failed"
		}
		removed = append(removed, i.Ref)
	}
	return removed, ""
}

// pruneExclusions lists what a caller is about to remove, so the images only that needs are counted as unused. A plan made
// with them is what prune would find once the removals are done.
type pruneExclusions struct {
	containers, services, apps map[string]bool // container IDs, service IDs and app names that are going away
}

func (c *Controller) prunePlan(ctx context.Context) ([]state.AcquiredImage, error) {
	return c.prunePlanWithout(ctx, pruneExclusions{})
}

func (c *Controller) prunePlanWithout(ctx context.Context, gone pruneExclusions) ([]state.AcquiredImage, error) {
	e, ok := c.Engine.(inventoryEngine)
	if !ok || c.Store == nil {
		return nil, errors.New("image inventory unavailable")
	}
	owned, err := c.Store.AcquiredImages(ctx)
	if err != nil {
		return nil, errPruneHistory
	}
	images, err := e.InventoryImages(ctx)
	if err != nil {
		return nil, inventoryFailure(err, "image inventory unavailable")
	}
	containers, err := e.InventoryContainers(ctx)
	if err != nil {
		return nil, inventoryFailure(err, "container inventory unavailable")
	}
	services, err := e.InventoryServices(ctx)
	if err != nil {
		return nil, inventoryFailure(err, "service inventory unavailable")
	}
	history, err := c.Store.History(ctx, "")
	if err != nil {
		return nil, errPruneHistory
	}
	refs := map[string]bool{}
	ids := map[string]bool{}
	aliases := map[string]string{}
	for _, img := range images {
		for _, ref := range append(append([]string{}, img.RepoTags...), img.RepoDigests...) {
			aliases[ref] = img.ID
		}
		aliases[img.ID] = img.ID
	}
	for _, ctr := range containers {
		if gone.containers[ctr.ID] {
			continue
		}
		if ctr.ImageID == "" {
			return nil, errors.New("container image unknown")
		}
		ids[ctr.ImageID] = true
	}
	for _, svc := range services {
		if gone.services[svc.ID] {
			continue
		}
		for _, spec := range []json.RawMessage{svc.Spec, svc.PreviousSpec} {
			if len(spec) == 0 || string(spec) == "null" {
				continue
			}
			var v struct {
				TaskTemplate struct{ ContainerSpec struct{ Image string } }
			}
			if json.Unmarshal(spec, &v) != nil || v.TaskTemplate.ContainerSpec.Image == "" {
				return nil, errors.New("service image unknown")
			}
			ref := v.TaskTemplate.ContainerSpec.Image
			refs[ref] = true
			// A reference the engine no longer resolves, such as the previous spec of a service whose old image was
			// removed, protects nothing: there is no local image for it. Every reference that does resolve stays protected.
			if id := aliases[ref]; id != "" {
				ids[id] = true
			}
		}
	}
	seen := map[string]int{}
	retains := map[string]int{}
	for _, r := range history {
		if _, ok := retains[r.App]; !ok {
			current, e := c.Store.Applied(ctx, r.App)
			if gone.apps[r.App] {
				e = sql.ErrNoRows
			}
			switch {
			case errors.Is(e, sql.ErrNoRows):
				// An app that was removed, such as the connector once managed ingress is gone, or an app taken down with
				// berthy remove, keeps its history but has no applied release. Nothing runs from it, and nothing can
				// roll back to it, so none of its releases protects an image.
				retains[r.App] = 0
				continue
			case e != nil:
				return nil, errors.New("applied release unavailable")
			}
			refs[current.Digest] = true
			var m manifest.Manifest
			if json.Unmarshal(current.Manifest, &m) != nil {
				return nil, errors.New("applied manifest unavailable")
			}
			app, ok := m.Apps[r.App]
			if !ok {
				return nil, errors.New("applied app unavailable")
			}
			if app.Deploy.Retain == nil {
				retains[r.App] = len(history)
			} else {
				retains[r.App] = *app.Deploy.Retain
			}
		}
		if seen[r.App] < retains[r.App] {
			refs[r.Digest] = true
		}
		seen[r.App]++
	}
	for ref := range refs {
		if id := aliases[ref]; id != "" {
			ids[id] = true
		}
	}
	return pruneCandidates(owned, refs, ids), nil
}

type execEngine interface {
	InspectContainer(context.Context, string) (engine.ContainerSnapshot, error)
	Exec(context.Context, string, []string) (engine.ExecResult, error)
}

func (c *Controller) execTarget(ctx context.Context, req Request) (string, error) {
	if len(req.Command) == 0 || req.Command[0] == "" {
		return "", errors.New("command required")
	}
	e, ok := c.Engine.(execEngine)
	if !ok {
		return "", errors.New("exec unavailable")
	}
	b, err := c.operationBinding(ctx, req.App, req.Role)
	if err != nil {
		return "", err
	}
	live, err := c.Engine.InspectService(ctx, b.Name)
	if err != nil || live.ID != b.ServiceID {
		return "", errors.New("bound service unavailable")
	}
	if err = validateInheritedRuntime(live.Spec); err != nil {
		return "", err
	}
	var spec struct {
		Name         string
		TaskTemplate struct {
			ContainerSpec struct{ Labels map[string]string }
		}
	}
	if json.Unmarshal(live.Spec, &spec) != nil || spec.Name != b.Name {
		return "", errors.New("invalid service")
	}
	generation := spec.TaskTemplate.ContainerSpec.Labels["berthy.operation"]
	if generation == "" {
		return "", errors.New("generation unavailable")
	}
	tasks, err := c.Engine.ServiceTasks(ctx, b.ServiceID)
	if err != nil {
		return "", errors.New("task inspection failed")
	}
	sort.Slice(tasks, func(i, j int) bool { return tasks[i].Slot < tasks[j].Slot })
	for _, task := range tasks {
		if task.ServiceID != b.ServiceID || task.State != "running" || task.DesiredState != "running" || task.Labels["berthy.operation"] != generation || task.ContainerID == "" {
			continue
		}
		container, err := e.InspectContainer(ctx, task.ContainerID)
		if err != nil || !container.Running || container.ID != task.ContainerID || container.Labels["berthy.operation"] != generation {
			continue
		}
		health, err := c.Engine.InspectContainerHealth(ctx, task.ContainerID)
		if err == nil && health == "healthy" {
			return task.ContainerID, nil
		}
	}
	return "", errors.New("healthy local container unavailable")
}
func (c *Controller) runExec(ctx context.Context, op state.Operation, actor Actor, req Request) state.Outcome {
	if !actor.Admin {
		return state.Outcome{State: "failed", Err: "administrator required"}
	}
	target, err := c.execTarget(ctx, req)
	if err != nil {
		return state.Outcome{State: "failed", Err: "exec target unavailable"}
	}
	infos, err := c.Store.Secrets(ctx)
	if err != nil {
		return state.Outcome{State: "failed", Err: "secret redaction unavailable"}
	}
	var secrets [][]byte
	for _, info := range infos {
		_, value, e := c.Store.Secret(ctx, info.Name)
		if e != nil {
			return state.Outcome{State: "failed", Err: "secret redaction unavailable"}
		}
		secrets = append(secrets, value)
	}
	desired, err := json.Marshal(struct {
		Command     []string `json:"command"`
		ContainerID string   `json:"container_id"`
	}{req.Command, target})
	if err != nil {
		return state.Outcome{State: "failed", Err: "exec preparation failed"}
	}
	intent := state.Intent{Ordinal: 30000, Kind: "exec", Name: target, Desired: desired}
	if err = c.Store.RecordIntent(ctx, op.ID, intent); err != nil {
		return state.Outcome{State: "failed", Err: "exec journal failed"}
	}
	if err = c.Store.MarkStep(ctx, op.ID, intent.Ordinal, "start_pending", json.RawMessage(`{}`)); err != nil {
		return state.Outcome{State: "failed", Err: "exec journal failed"}
	}
	execCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	result, err := c.Engine.(execEngine).Exec(execCtx, target, req.Command)
	if err != nil {
		return state.Outcome{State: "recovery_required", Err: "exec outcome uncertain"}
	}
	receipt, err := json.Marshal(struct {
		Output   string `json:"output"`
		ExitCode int    `json:"exit_code"`
	}{redactOutput(result.Output, secrets), result.ExitCode})
	if err != nil {
		return state.Outcome{State: "recovery_required", Err: "exec result unavailable"}
	}
	if err = c.Store.MarkStep(ctx, op.ID, intent.Ordinal, "finished", receipt); err != nil {
		return state.Outcome{State: "recovery_required", Err: "exec result unavailable"}
	}
	if err = c.Store.AppendEvent(ctx, op.ID, state.Event{Kind: "exec_finished", Data: json.RawMessage(fmt.Sprintf(`{"exit_code":%d}`, result.ExitCode))}); err != nil {
		return state.Outcome{State: "recovery_required", Err: "exec result unavailable"}
	}
	if result.ExitCode != 0 {
		return state.Outcome{State: "failed", Err: "exec returned nonzero"}
	}
	return state.Outcome{State: "succeeded"}
}

type hookPruner interface {
	InspectContainer(context.Context, string) (engine.ContainerSnapshot, error)
	RemoveContainer(context.Context, string) error
}

func ownedFinishedHook(c engine.ContainerSnapshot, h state.CompletedOneOff, environment string) bool {
	return !c.Running && c.ID == h.ID && c.Labels["berthy.environment"] == environment && c.Labels["berthy.operation"] == h.OperationID && c.Labels["berthy.oneoff"] == h.Role
}

func (c *Controller) prunableHooks(ctx context.Context) ([]state.CompletedOneOff, error) {
	hooks, err := c.Store.CompletedOneOffs(ctx)
	if err != nil {
		return nil, err
	}
	if len(hooks) == 0 {
		return nil, nil
	}
	e, ok := c.Engine.(hookPruner)
	if !ok {
		return nil, errors.New("hook inventory unavailable")
	}
	var out []state.CompletedOneOff
	for _, h := range hooks {
		container, err := e.InspectContainer(ctx, h.ID)
		if err != nil {
			var status *engine.StatusError
			if errors.As(err, &status) && status.Code == 404 {
				continue
			}
			return nil, errors.New("hook inspection failed")
		}
		if ownedFinishedHook(container, h, c.Environment) {
			out = append(out, h)
		}
	}
	return out, nil
}
