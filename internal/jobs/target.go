package jobs

import (
	"errors"

	"berthy/internal/engine"
)

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
