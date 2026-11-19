package deploy

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"berthy/internal/engine"
	"berthy/internal/manifest"
	"berthy/internal/state"
)

// runStopGrace is the seconds a run that ran past its time limit gets to stop on SIGTERM before Docker kills it.
const runStopGrace = 10

// runOrdinal is the journal ordinal of a run's container; its output follows at runOrdinal+1. It is after the ordinals of an
// exec and a schema revert, which an operation of its own kind never shares.
const runOrdinal = 30100

const (
	errRunCommand     Refusal = "run requires a command after --"
	errRunTimeout     Refusal = "timeout must be from 1s to 24h"
	errRunCompose     Refusal = "run is for image apps; use exec --service"
	errRunRoles       Refusal = "the app has several roles: name one with --role"
	errRunRoleUnknown Refusal = "role is not a role of the app"
	errRunNonzero     Refusal = "run returned nonzero"
	errRunPrepare     Refusal = "run preflight failed"
)

// errRunNotDeployed is the refusal of a remove of an app that is not deployed, for a run of one too.
const errRunNotDeployed = errRemoveNotDeployed

// runTimedOut matches the failure of a run that ran past its time limit.
var runTimedOut = regexp.MustCompile(`^run timed out after [0-9a-z.]{2,24}$`)

// runEngine is what a run needs of Docker beyond the one-off container calls: removing the container once its output is kept.
type runEngine interface {
	oneOffEngine
	RemoveContainer(context.Context, string) error
}

// runTarget is what a run resolves from the request and the applied release: the release, the role it runs as and the time
// limit. It reads state only, so Admit and the plan can refuse a request before anything is journaled.
type runTarget struct {
	Release  state.Release
	Manifest manifest.Manifest
	Role     string
	Limits   *manifest.Resources
	Volumes  []string
	Timeout  time.Duration
}

func (c *Controller) runTarget(ctx context.Context, req Request) (runTarget, error) {
	if len(req.Command) == 0 || req.Command[0] == "" {
		return runTarget{}, errRunCommand
	}
	timeout, err := manifest.ParseCommandTimeout(req.Timeout)
	if err != nil {
		return runTarget{}, errRunTimeout
	}
	if c.Store == nil {
		return runTarget{}, errors.New("state unavailable")
	}
	release, err := c.Store.Applied(ctx, req.App)
	if errors.Is(err, sql.ErrNoRows) {
		return runTarget{}, errRunNotDeployed
	}
	if err != nil {
		return runTarget{}, errors.New("applied release unavailable")
	}
	t := runTarget{Release: release, Timeout: timeout}
	if json.Unmarshal(release.Manifest, &t.Manifest) != nil {
		return runTarget{}, errors.New("applied manifest unavailable")
	}
	app, ok := t.Manifest.Apps[req.App]
	if !ok {
		return runTarget{}, errRunNotDeployed
	}
	if app.Kind == "compose" {
		return runTarget{}, errRunCompose
	}
	t.Role = req.Role
	if t.Role == "" {
		if len(app.Roles) != 1 {
			return runTarget{}, errRunRoles
		}
		for name := range app.Roles {
			t.Role = name
		}
	}
	role, ok := app.Roles[t.Role]
	if !ok {
		return runTarget{}, errRunRoleUnknown
	}
	t.Limits, t.Volumes = role.Resources, role.Volumes
	return t, nil
}

// prepareRun readies a run of the applied release: it resolves the target, prepares the image's execution and makes sure the
// image is on the host. An error that may be shown to the person is a Refusal; any other is not.
func (c *Controller) prepareRun(ctx context.Context, req Request) (runTarget, preparedPlan, error) {
	t, err := c.runTarget(ctx, req)
	if err != nil {
		return runTarget{}, preparedPlan{}, err
	}
	p, err := c.prepareExecution(ctx, Request{App: req.App, Manifest: t.Manifest}, Plan{Image: t.Release.Digest, Tag: t.Release.Tag})
	if err != nil {
		return runTarget{}, preparedPlan{}, errRunPrepare
	}
	if _, msg, err := c.ensureImage(ctx, p); err != nil {
		return runTarget{}, preparedPlan{}, Refusal(msg)
	}
	return t, p, nil
}

// planRun is the dry run and the approval plan of a run: the applied release's image and the role, never a container, so the
// plan stays the same between a hold and its approval.
func (c *Controller) planRun(ctx context.Context, actor Actor, req Request) (Plan, error) {
	if !actor.Admin {
		return Plan{}, errors.New("administrator required")
	}
	t, err := c.runTarget(ctx, req)
	if err != nil {
		return Plan{}, err
	}
	return Plan{App: req.App, Image: t.Release.Digest, Tag: t.Release.Tag, Roles: []string{t.Role}, Command: req.Command}, nil
}

// runRun runs the command in a new container of the applied release and removes the container once its output is recorded.
func (c *Controller) runRun(ctx context.Context, op state.Operation, actor Actor, req Request) state.Outcome {
	if !actor.Admin {
		return state.Outcome{State: "failed", Err: "administrator required"}
	}
	e, ok := c.Engine.(runEngine)
	if !ok {
		return state.Outcome{State: "failed", Err: "one-off containers unavailable"}
	}
	t, p, err := c.prepareRun(ctx, req)
	if err != nil {
		return state.Outcome{State: "failed", Err: publicOr(err, errRunPrepare).Error()}
	}
	redact, ok := c.redactor(ctx)
	if !ok {
		return state.Outcome{State: "failed", Err: "secret redaction unavailable"}
	}
	_, runErr := c.runOneOff(ctx, op.ID, p, oneOff{Ordinal: runOrdinal, Kind: "run", Role: t.Role, Argv: req.Command, Timeout: t.Timeout, Resources: t.Limits, Volumes: t.Volumes})
	container, exit, finished, err := c.runContainer(ctx, op.ID)
	if err != nil {
		return state.Outcome{State: "recovery_required", Err: "run journal requires recovery"}
	}
	outcome := state.Outcome{State: "failed", Err: string(errRunNonzero)}
	switch {
	case runErr == nil:
		outcome = state.Outcome{State: "succeeded"}
	case isCancelled(runErr):
		// The container was stopped when the cancel was accepted. Its output so far is kept, as for a run that timed out.
		outcome.Err = errCancelled
	case finished:
		// The command ran to an end with a non-zero exit code.
	case errors.Is(runErr, ErrOneOffUncertain):
		// The command may still be running: stop it so its output is complete, and so the lock is not released under it.
		if container != "" {
			stopCtx, cancel := captureContext(ctx)
			err = e.StopContainer(stopCtx, container, runStopGrace)
			cancel()
			var status *engine.StatusError
			if err != nil && !(errors.As(err, &status) && status.Code == 404) {
				return state.Outcome{State: "recovery_required", Err: "one-off outcome uncertain"}
			}
		}
		exit = -1
		outcome.Err = "one-off outcome uncertain"
		var failure *oneOffFailure
		if errors.As(runErr, &failure) && strings.Contains(failure.diagnostic.Error, errTimedOut) {
			outcome.Err = "run timed out after " + t.Timeout.String()
		}
	default:
		return state.Outcome{State: "failed", Err: "run preflight failed"}
	}
	if container != "" {
		if err = c.recordRunOutput(ctx, e, op.ID, req.Command, container, exit, redact); err != nil {
			return state.Outcome{State: "recovery_required", Err: "run result unavailable"}
		}
		rmCtx, cancel := captureContext(ctx)
		_ = e.RemoveContainer(rmCtx, container)
		cancel()
	}
	return outcome
}

// runContainer reads what the journal says of a run's container: its ID, and its exit code once the command has finished. The
// ID is empty when no container was created. A container that was never confirmed is named, so the name stands for it.
func (c *Controller) runContainer(ctx context.Context, opID string) (id string, exit int, finished bool, err error) {
	steps, err := c.Store.Intents(ctx, opID)
	if err != nil {
		return "", 0, false, err
	}
	for _, step := range steps {
		if step.Kind != "oneoff" || step.Ordinal != runOrdinal {
			continue
		}
		var receipt struct {
			ID   string `json:"id"`
			Exit int    `json:"exit"`
		}
		if len(step.Receipt) > 0 && json.Unmarshal(step.Receipt, &receipt) != nil {
			return "", 0, false, errors.New("invalid one-off receipt")
		}
		if receipt.ID == "" {
			return step.Name, 0, false, nil
		}
		return receipt.ID, receipt.Exit, step.Phase == "finished", nil
	}
	return "", 0, false, nil
}

// recordRunOutput reads the container's output, redacts it and journals it with the exit code as the operation's output,
// which the output route serves. The record has the shape of an exec's, so the same route and recovery serve both.
func (c *Controller) recordRunOutput(ctx context.Context, e runEngine, opID string, command []string, container string, exit int, redact func(string) string) error {
	readCtx, cancel := captureContext(ctx)
	logs, err := e.ContainerLogs(readCtx, container)
	cancel()
	output := redact(string(logs))
	if err != nil {
		output = "[output unavailable]"
	}
	desired, err := json.Marshal(struct {
		Command     []string `json:"command"`
		ContainerID string   `json:"container_id"`
	}{command, container})
	if err != nil {
		return err
	}
	intent := state.Intent{Ordinal: runOrdinal + 1, Kind: "exec", Name: container, Desired: desired}
	if err = c.Store.RecordIntent(ctx, opID, intent); err != nil {
		return err
	}
	receipt, err := json.Marshal(struct {
		Output   string `json:"output"`
		ExitCode int    `json:"exit_code"`
	}{output, exit})
	if err != nil {
		return err
	}
	if err = c.Store.MarkStep(ctx, opID, intent.Ordinal, "finished", receipt); err != nil {
		return err
	}
	return c.Store.AppendEvent(ctx, opID, state.Event{Kind: "exec_finished", Data: json.RawMessage(fmt.Sprintf(`{"exit_code":%d}`, exit))})
}
