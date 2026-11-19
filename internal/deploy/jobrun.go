package deploy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"berthy/internal/engine"
	"berthy/internal/jobs"
	"berthy/internal/manifest"
)

// composeJobEngine is what a command of a Compose app needs of Docker: the containers of its project, and an exec that keeps
// the tail of its output, or streams it.
type composeJobEngine interface {
	ComposeContainers(context.Context, string) ([]engine.ComposeContainer, error)
	ExecStream(ctx context.Context, containerID string, command []string, stdin io.Reader, stdout io.Writer, tailMax int) (exit int, tail string, err error)
}

// streamEngine is what a streamed command of an image app needs of Docker: the one-off container calls, and the attach that
// carries its input and output.
type streamEngine interface {
	runEngine
	AttachStream(ctx context.Context, id string, stdin io.Reader, stdout io.Writer, tailMax int, start func() error) (tail string, inErr, err error)
}

// commandKinds are the kinds of command the agent runs in a container of its own, which name the container and the label
// berthy.oneoff that finds it again: a job, a backup that takes the output of a command, and a restore that feeds one.
var commandKinds = map[string]bool{"job": true, "backup": true, "restore": true}

// RunJob runs the command of a job and says how it ended; it is the jobs.Runner of the agent.
func (c *Controller) RunJob(ctx context.Context, m manifest.Manifest, name string, job manifest.Job, runID int64) (jobs.Outcome, error) {
	return c.RunCommand(ctx, m, "job", name, strconv.FormatInt(runID, 10), job, jobs.Streams{})
}

// RunCommand runs the command of a job, a backup or a restore and says how it ended. kind is "job", "backup" or "restore", name
// the job's or backup's name, and id tells the run from others of its kind. A command of an image app runs in a container of its
// own, the way berthy run does; a command of a Compose app runs as an exec in its service's container. Neither takes the deploy
// lock, so a deploy never waits for a command and a command never holds a deploy back. With streams the output goes to
// streams.Stdout as it is written and the input comes from streams.Stdin; the outcome's Output is then the last lines of the
// standard error of a failed or timed-out run, redacted.
func (c *Controller) RunCommand(ctx context.Context, m manifest.Manifest, kind, name, id string, job manifest.Job, st jobs.Streams) (jobs.Outcome, error) {
	app, ok := m.Apps[job.App]
	if !ok {
		return jobs.Outcome{Detail: "app is not in the manifest"}, nil
	}
	if app.Kind == "compose" {
		return c.runComposeCommand(ctx, m, job, st)
	}
	return c.runImageCommand(ctx, kind, name, id, job, st)
}

// runImageCommand runs the command in a new container of the app's applied release, with the role's environment, limits and named
// volumes, on the shared network. The container is stopped when the run is over before the command is, and removed in every case,
// once the last lines of a run that failed have been read. It needs no running replica.
func (c *Controller) runImageCommand(ctx context.Context, kind, name, id string, job manifest.Job, st jobs.Streams) (jobs.Outcome, error) {
	streamed := st != jobs.Streams{}
	var e runEngine
	var se streamEngine
	var ok bool
	if streamed {
		se, ok = c.Engine.(streamEngine)
		e = se
	} else {
		e, ok = c.Engine.(runEngine)
	}
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
	labels := map[string]string{"berthy.environment": c.Environment, "berthy." + kind: name, "berthy.oneoff": kind}
	var term *terminal
	if streamed {
		term = &terminal{NoLog: true}
	}
	spec, err := p.oneOffSpec(job.Command, t.Role, t.Limits, t.Volumes, labels, term)
	if err != nil {
		return jobs.Outcome{}, err
	}
	cid, err := e.CreateContainer(ctx, fmt.Sprintf("berthy-%s-%s-%s", kind, c.Environment, id), spec)
	if err != nil || cid == "" {
		return jobs.Outcome{}, errors.New("job container unavailable")
	}
	out := jobs.Outcome{Container: cid}
	defer func() {
		rmCtx, cancel := captureContext(ctx)
		defer cancel()
		_ = e.RemoveContainer(rmCtx, cid)
	}()
	var tail string
	var inErr error
	if streamed {
		// The attach comes before the start, so the first output is not lost. It ends when the container's output does.
		tail, inErr, err = se.AttachStream(ctx, cid, st.Stdin, st.Stdout, diagnosticLogBytes, func() error { return e.StartContainer(ctx, cid) })
		if err != nil {
			// The run ended before the command did, or its output could not be taken. A container that stops takes the command
			// with it, which is why the command runs in one.
			capCtx, cancel := captureContext(ctx)
			defer cancel()
			_ = e.StopContainer(capCtx, cid, runStopGrace)
			if cancelledJob(ctx) {
				out.Output = c.lastLines(capCtx, tail)
				return out, err
			}
			if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return out, err
			}
			out.TimedOut, out.Output = true, c.lastLines(capCtx, tail)
			return out, nil
		}
	} else if err = e.StartContainer(ctx, cid); err != nil {
		return out, err
	}
	exit, err := e.WaitContainer(ctx, cid)
	// What follows outlives ctx, which has usually ended by now.
	capCtx, cancel := captureContext(ctx)
	defer cancel()
	if err != nil {
		_ = e.StopContainer(capCtx, cid, runStopGrace)
		if cancelledJob(ctx) {
			// A cancel stopped it: the last lines it wrote are kept, as for a run that failed.
			out.Output = c.jobOutput(capCtx, cid, streamed, tail)
			return out, err
		}
		if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return out, err
		}
		out.TimedOut, out.Output = true, c.jobOutput(capCtx, cid, streamed, tail)
		return out, nil
	}
	out.Exit = &exit
	if exit != 0 {
		out.Output = c.jobOutput(capCtx, cid, streamed, tail)
	} else if inErr != nil {
		// The command ended well without taking all of its input.
		return out, inErr
	}
	return out, nil
}

// cancelledJob reports whether a person or an agent cancelled the run whose context this is.
func cancelledJob(ctx context.Context) bool {
	var by *jobs.Cancelled
	return errors.As(context.Cause(ctx), &by)
}

// jobOutput is the last lines of a container's output, with every managed secret value replaced. It is empty when they cannot be
// read or redacted: output that might hold a secret is never kept. A streamed run has its stderr tail already, and the container
// holds no log to read.
func (c *Controller) jobOutput(ctx context.Context, container string, streamed bool, tail string) string {
	if streamed {
		return c.lastLines(ctx, tail)
	}
	de, ok := c.Engine.(diagnosticEngine)
	if !ok {
		return ""
	}
	redact, redactable := c.redactor(ctx)
	d := Diagnostic{Container: container}
	logsInto(ctx, de, &d, redact, redactable)
	return d.Logs
}

// runComposeCommand runs the command as an exec in the running container of the job's service. Docker cannot stop an exec, so a
// command that runs past its time limit is recorded as timed out and may go on. A command that is streamed ends with its stream.
func (c *Controller) runComposeCommand(ctx context.Context, m manifest.Manifest, job manifest.Job, st jobs.Streams) (jobs.Outcome, error) {
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
	exit, tail, err := e.ExecStream(ctx, id, job.Command, st.Stdin, st.Stdout, diagnosticLogBytes)
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
