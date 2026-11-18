package deploy

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"berthy/internal/engine"
	"berthy/internal/jobs"
	"berthy/internal/manifest"
)

// composeJobEngine is what a job of a Compose app needs of Docker: the containers of its project, and an exec that keeps the
// tail of its output.
type composeJobEngine interface {
	ComposeContainers(context.Context, string) ([]engine.ComposeContainer, error)
	ExecTail(ctx context.Context, containerID string, command []string, maxBytes int) (exit int, tail string, err error)
}

// RunJob runs the command of a job and says how it ended; it is the jobs.Runner of the agent. A job of an image app runs in a
// container of its own, the way berthy run does; a job of a Compose app runs as an exec in its service's container. Neither
// takes the deploy lock, so a deploy never waits for a job and a job never holds a deploy back.
func (c *Controller) RunJob(ctx context.Context, m manifest.Manifest, name string, job manifest.Job, runID int64) (jobs.Outcome, error) {
	app, ok := m.Apps[job.App]
	if !ok {
		return jobs.Outcome{Detail: "app is not in the manifest"}, nil
	}
	if app.Kind == "compose" {
		return c.runComposeJob(ctx, m, job)
	}
	return c.runImageJob(ctx, name, job, runID)
}

// runImageJob runs the command in a new container of the app's applied release, with the role's environment, limits and named
// volumes, on the shared network. The container is stopped when the run is over before the command is, and removed in every case,
// once the last lines of a run that failed have been read. It needs no running replica.
func (c *Controller) runImageJob(ctx context.Context, name string, job manifest.Job, runID int64) (jobs.Outcome, error) {
	e, ok := c.Engine.(runEngine)
	if !ok {
		return jobs.Outcome{}, errors.New("one-off containers unavailable")
	}
	t, p, err := c.prepareRun(ctx, Request{App: job.App, Role: job.Role, Command: job.Command, Timeout: job.Timeout})
	if err != nil {
		var r Refusal
		if errors.As(err, &r) {
			return jobs.Outcome{Detail: string(r)}, nil
		}
		return jobs.Outcome{}, err
	}
	labels := map[string]string{"berthy.environment": c.Environment, "berthy.job": name, "berthy.oneoff": "job"}
	spec, err := p.oneOffSpec(job.Command, t.Role, t.Limits, t.Volumes, labels, nil)
	if err != nil {
		return jobs.Outcome{}, err
	}
	id, err := e.CreateContainer(ctx, fmt.Sprintf("berthy-job-%s-%d", c.Environment, runID), spec)
	if err != nil || id == "" {
		return jobs.Outcome{}, errors.New("job container unavailable")
	}
	out := jobs.Outcome{Container: id}
	defer func() {
		rmCtx, cancel := captureContext(ctx)
		defer cancel()
		_ = e.RemoveContainer(rmCtx, id)
	}()
	if err = e.StartContainer(ctx, id); err != nil {
		return out, err
	}
	exit, err := e.WaitContainer(ctx, id)
	// What follows outlives ctx, which has usually ended by now.
	capCtx, cancel := captureContext(ctx)
	defer cancel()
	if err != nil {
		// The run ended before the command did. A container that stops takes the command with it, which is why the job runs in
		// one.
		_ = e.StopContainer(capCtx, id, runStopGrace)
		if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return out, err
		}
		out.TimedOut, out.Output = true, c.jobOutput(capCtx, id)
		return out, nil
	}
	out.Exit = &exit
	if exit != 0 {
		out.Output = c.jobOutput(capCtx, id)
	}
	return out, nil
}

// jobOutput is the last lines of a container's output, with every managed secret value replaced. It is empty when they cannot be
// read or redacted: output that might hold a secret is never kept.
func (c *Controller) jobOutput(ctx context.Context, container string) string {
	de, ok := c.Engine.(diagnosticEngine)
	if !ok {
		return ""
	}
	redact, redactable := c.redactor(ctx)
	d := Diagnostic{Container: container}
	logsInto(ctx, de, &d, redact, redactable)
	return d.Logs
}

// runComposeJob runs the command as an exec in the running container of the job's service. Docker cannot stop an exec, so a job
// that runs past its time limit is recorded as timed out and its command may go on.
func (c *Controller) runComposeJob(ctx context.Context, m manifest.Manifest, job manifest.Job) (jobs.Outcome, error) {
	e, ok := c.Engine.(composeJobEngine)
	if !ok {
		return jobs.Outcome{}, errors.New("exec unavailable")
	}
	containers, err := e.ComposeContainers(ctx, m.ComposeProject(job.App))
	if err != nil {
		return jobs.Outcome{}, err
	}
	id, err := jobs.PickComposeContainer(containers, job.Service)
	switch {
	case errors.Is(err, jobs.ErrNoRunningContainer):
		return jobs.Outcome{Detail: noRunningContainer(job.App, job.Service)}, nil
	case errors.Is(err, jobs.ErrSeveralServices):
		return jobs.Outcome{Detail: "name the Compose service as service"}, nil
	case err != nil:
		return jobs.Outcome{}, err
	}
	out := jobs.Outcome{Container: id}
	exit, tail, err := e.ExecTail(ctx, id, job.Command, diagnosticLogBytes)
	capCtx, cancel := captureContext(ctx)
	defer cancel()
	switch {
	case err == nil:
		out.Exit = &exit
		if exit != 0 {
			out.Output = c.lastLines(capCtx, tail)
		}
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		out.TimedOut, out.Output = true, c.lastLines(capCtx, tail)
	default:
		return out, err
	}
	return out, nil
}

// lastLines is the last diagnosticLogLines lines of the tail of a command's output, redacted. A tail that filled its buffer
// starts in the middle of a line, which is dropped. It is empty when the secrets cannot be read.
func (c *Controller) lastLines(ctx context.Context, tail string) string {
	redact, ok := c.redactor(ctx)
	if !ok {
		return ""
	}
	if len(tail) >= diagnosticLogBytes {
		if _, rest, found := strings.Cut(tail, "\n"); found {
			tail = rest
		}
	}
	lines := strings.SplitAfter(tail, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) > diagnosticLogLines {
		lines = lines[len(lines)-diagnosticLogLines:]
	}
	return redact(strings.Join(lines, ""))
}
