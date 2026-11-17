package jobs

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"berthy/internal/engine"
	"berthy/internal/manifest"
	"berthy/internal/state"
)

// Docker is the part of the engine the target lookup uses.
type Docker interface {
	NodeID(ctx context.Context) (string, error)
	ServiceTasks(ctx context.Context, serviceID string) ([]engine.ServiceTask, error)
	ComposeContainers(ctx context.Context, project string) ([]engine.ComposeContainer, error)
}

// EngineTarget finds a job's container through the state's service bindings and Docker.
type EngineTarget struct {
	Store  *state.Store
	Docker Docker
}

// Container returns the container a job runs in. For an image app it is the running task with the lowest slot among those
// on this node; for a Compose app, the lowest-numbered running container of the job's service. Reasons that belong in a
// run record come back as TargetError; any other error is a failure to ask Docker or the state.
func (t EngineTarget) Container(ctx context.Context, m manifest.Manifest, job manifest.Job) (string, error) {
	app, ok := m.Apps[job.App]
	if !ok {
		return "", fmt.Errorf("app %q is not in the manifest", job.App)
	}
	if app.Kind == "compose" {
		return t.composeContainer(ctx, m, job)
	}
	return t.imageContainer(ctx, app, job)
}

func (t EngineTarget) imageContainer(ctx context.Context, app manifest.App, job manifest.Job) (string, error) {
	role := job.Role
	if role == "" && len(app.Roles) == 1 {
		for r := range app.Roles {
			role = r
		}
	}
	none := TargetError("no running container for " + job.App + "/" + role)
	bindings, err := t.Store.Bindings(ctx, job.App)
	if err != nil {
		return "", err
	}
	i := slices.IndexFunc(bindings, func(b state.Binding) bool { return b.Role == role })
	if i < 0 {
		return "", none
	}
	node, err := t.Docker.NodeID(ctx)
	if err != nil {
		return "", err
	}
	tasks, err := t.Docker.ServiceTasks(ctx, bindings[i].ServiceID)
	if err != nil {
		return "", err
	}
	var best *engine.ServiceTask
	for i := range tasks {
		task := &tasks[i]
		if task.State != "running" || task.DesiredState != "running" || task.ContainerID == "" || task.NodeID != node {
			continue
		}
		if best == nil || task.Slot < best.Slot {
			best = task
		}
	}
	if best == nil {
		return "", none
	}
	return best.ContainerID, nil
}

// ErrNoRunningContainer means a Compose app has no running, non-one-off container of the service asked for, or of any
// service when none was named. ErrSeveralServices means none was named and the app runs more than one service.
var (
	ErrNoRunningContainer = errors.New("no running container")
	ErrSeveralServices    = errors.New("several services")
)

// PickComposeContainer chooses the container of a Compose app that a job or an exec runs in: a running container of the
// named service, or of the app's only service when service is empty. A one-off container is never chosen. Among replicas
// it takes the lowest number, then the lowest id.
func PickComposeContainer(containers []engine.ComposeContainer, service string) (string, error) {
	var running []engine.ComposeContainer
	services := map[string]bool{}
	for _, c := range containers {
		if c.State != "running" || c.OneOff || c.Service == "" || service != "" && c.Service != service {
			continue
		}
		running = append(running, c)
		services[c.Service] = true
	}
	switch {
	case len(running) == 0:
		return "", ErrNoRunningContainer
	case len(services) > 1:
		return "", ErrSeveralServices
	}
	best := running[0]
	for _, c := range running[1:] {
		if c.Number < best.Number || c.Number == best.Number && c.ID < best.ID {
			best = c
		}
	}
	return best.ID, nil
}

func (t EngineTarget) composeContainer(ctx context.Context, m manifest.Manifest, job manifest.Job) (string, error) {
	containers, err := t.Docker.ComposeContainers(ctx, m.ComposeProject(job.App))
	if err != nil {
		return "", err
	}
	id, err := PickComposeContainer(containers, job.Service)
	name := job.App
	if job.Service != "" {
		name += "/" + job.Service
	}
	switch {
	case errors.Is(err, ErrNoRunningContainer):
		return "", TargetError("no running container for " + name)
	case errors.Is(err, ErrSeveralServices):
		return "", TargetError("name the Compose service as service")
	}
	return id, err
}
