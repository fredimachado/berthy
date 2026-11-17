package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"berthy/internal/engine"
	"berthy/internal/state"
)

type oneOffEngine interface {
	CreateContainer(context.Context, string, json.RawMessage) (string, error)
	StartContainer(context.Context, string) error
	WaitContainer(context.Context, string) (int, error)
	ContainerLogs(context.Context, string) ([]byte, error)
	InspectContainer(context.Context, string) (engine.ContainerSnapshot, error)
	StopContainer(context.Context, string, int) error
}

var ErrOneOffUncertain = errors.New("one-off outcome uncertain")

// oneOffRetryDelay is the wait between an attempt that exited non-zero and the next one.
var oneOffRetryDelay = 5 * time.Second

// Journal ordinals of an operation's steps. Every attempt of a command has its own one-off container and intent, so a
// command owns a block of attemptsPerCommand ordinals and attempt a (0 to 5) uses the block's first ordinal plus a.
//
//	0..n          services of the rollout (n roles)
//	9000          migration source; 9001, 9002 Compose source and run; 9999 verification barrier
//	10000 + 10*i  pre-deploy command i (0-based), i < maxHooksPerList
//	15000         schema state before; 15010 migration apply; 15020 schema state after
//	20000 + 10*i  post-deploy command i, i < maxHooksPerList
//	30000..30002  schema revert steps, never retried (an exec of its own operation also uses 30000)
const (
	preHookOrdinal        = 10000
	schemaBeforeOrdinal   = 15000
	migrationApplyOrdinal = 15010
	schemaAfterOrdinal    = 15020
	postHookOrdinal       = 20000
	attemptsPerCommand    = 10
	maxHooksPerList       = 500
)

func (c *Controller) runPre(ctx context.Context, opID string, p preparedPlan) (string, string, error) {
	app := p.Request.Manifest.Apps[p.Request.App]
	if len(app.Hooks.PreDeploy) > maxHooksPerList {
		return "", "", errors.New("too many pre-deploy commands")
	}
	for i, h := range app.Hooks.PreDeploy {
		if h.Backup != "" {
			if err := c.runBackupHook(ctx, opID, p, "pre_hook", i+1, h.Backup); err != nil {
				return "", "", err
			}
			continue
		}
		if _, err := c.runRetrying(ctx, opID, p, preHookOrdinal+attemptsPerCommand*i, "pre_hook", h.Run, false, h.TimeoutDuration(), h.RetryCount()); err != nil {
			return "", "", err
		}
	}
	m := app.Migrations
	if m == nil {
		return "", "", nil
	}
	timeout := m.TimeoutDuration()
	before, err := c.runOneOff(ctx, opID, p, schemaBeforeOrdinal, "schema_before", m.Version, true, timeout)
	if err != nil {
		return "", "", err
	}
	// Only apply is retried: version only reads, and a half-applied migration is re-run only when the manifest says its
	// command is safe to run again.
	_, applyErr := c.runRetrying(ctx, opID, p, migrationApplyOrdinal, "migration_apply", m.Apply, false, timeout, m.RetryCount())
	if errors.Is(applyErr, ErrOneOffUncertain) {
		return before, "", applyErr
	}
	after, afterErr := c.runOneOff(ctx, opID, p, schemaAfterOrdinal, "schema_after", m.Version, true, timeout)
	if applyErr != nil {
		return before, after, applyErr
	}
	if afterErr != nil {
		return before, "", afterErr
	}
	return before, after, nil
}
func (c *Controller) runPost(ctx context.Context, opID string, p preparedPlan) error {
	hooks := p.Request.Manifest.Apps[p.Request.App].Hooks.PostDeploy
	if len(hooks) > maxHooksPerList {
		return errors.New("too many post-deploy commands")
	}
	for i, h := range hooks {
		if h.Backup != "" {
			if err := c.runBackupHook(ctx, opID, p, "post_hook", i+1, h.Backup); err != nil {
				return err
			}
			continue
		}
		if _, err := c.runRetrying(ctx, opID, p, postHookOrdinal+attemptsPerCommand*i, "post_hook", h.Run, false, h.TimeoutDuration(), h.RetryCount()); err != nil {
			return err
		}
	}
	return nil
}

// runRetrying runs a command, and runs it again in a new one-off container after a non-zero exit, up to retries more
// times with oneOffRetryDelay between attempts. An attempt that timed out or whose outcome is otherwise uncertain is never
// retried: the command may still be running. The error of the last attempt is returned, with the number of attempts in its
// diagnostic. first is the journal ordinal of the first attempt.
func (c *Controller) runRetrying(parent context.Context, opID string, p preparedPlan, first int, kind string, argv []string, version bool, timeout time.Duration, retries int) (string, error) {
	for attempt := 0; ; attempt++ {
		v, err := c.runOneOff(parent, opID, p, first+attempt, kind, argv, version, timeout)
		var failure *oneOffFailure
		if err == nil || !errors.As(err, &failure) {
			return v, err
		}
		if attempt > 0 {
			failure.diagnostic.Attempts = attempt + 1
		}
		if errors.Is(err, ErrOneOffUncertain) || attempt >= retries {
			return v, err
		}
		data, _ := json.Marshal(map[string]string{"step": fmt.Sprintf("%s failed; retrying (attempt %d of %d)", oneOffLabel(kind, commandIndex(kind, first)), attempt+2, retries+1)})
		_ = c.Store.AppendEvent(parent, opID, state.Event{Kind: "step", Data: data})
		select {
		case <-parent.Done():
			return v, err
		case <-time.After(oneOffRetryDelay):
		}
	}
}

func (c *Controller) runOneOff(parent context.Context, opID string, p preparedPlan, ordinal int, kind string, argv []string, version bool, timeout time.Duration) (string, error) {
	if c.Store == nil || len(argv) == 0 || argv[0] == "" {
		return "", errors.New("invalid one-off command")
	}
	e, ok := c.Engine.(oneOffEngine)
	if !ok {
		return "", errors.New("one-off containers unavailable")
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	name := fmt.Sprintf("berthy-%s-%d", opID, ordinal)
	labels := map[string]string{"berthy.environment": c.Environment, "berthy.operation": opID, "berthy.oneoff": kind}
	spec, err := json.Marshal(struct {
		Image            string            `json:"Image"`
		Entrypoint       []string          `json:"Entrypoint"`
		Cmd              []string          `json:"Cmd"`
		Env              []string          `json:"Env,omitempty"`
		Labels           map[string]string `json:"Labels"`
		NetworkingConfig any               `json:"NetworkingConfig"`
	}{p.Image, argv[:1], argv[1:], p.containerEnv(""), labels, map[string]any{"EndpointsConfig": map[string]any{p.NetworkID: map[string]any{}}}})
	if err != nil {
		return "", errors.New("one-off preparation failed")
	}
	if err = c.Store.RecordIntent(ctx, opID, state.Intent{Ordinal: ordinal, Kind: "oneoff", Role: kind, Name: name, Desired: spec}); err != nil {
		return "", errors.New("one-off journal failed")
	}
	id, err := e.CreateContainer(ctx, name, spec)
	if err != nil || id == "" {
		return "", ErrOneOffUncertain
	}
	receipt, _ := json.Marshal(struct {
		ID string `json:"id"`
	}{id})
	if err = c.Store.MarkStep(ctx, opID, ordinal, "created", receipt); err != nil {
		return "", ErrOneOffUncertain
	}
	if err = c.Store.MarkStep(ctx, opID, ordinal, "start_pending", receipt); err != nil {
		return "", ErrOneOffUncertain
	}
	if err = e.StartContainer(ctx, id); err != nil {
		return "", c.uncertainOneOff(parent, ctx, err, id, kind, ordinal, timeout)
	}
	if err = c.Store.MarkStep(ctx, opID, ordinal, "started", receipt); err != nil {
		return "", ErrOneOffUncertain
	}
	exit, err := e.WaitContainer(ctx, id)
	if err != nil {
		return "", c.uncertainOneOff(parent, ctx, err, id, kind, ordinal, timeout)
	}
	receipt, _ = json.Marshal(struct {
		ID   string `json:"id"`
		Exit int    `json:"exit"`
	}{id, exit})
	if err = c.Store.MarkStep(ctx, opID, ordinal, "finished", receipt); err != nil {
		return "", ErrOneOffUncertain
	}
	if exit != 0 {
		// The evidence is read now: nothing removes the container, but the next steps of the operation may.
		capCtx, cancelCapture := captureContext(parent)
		d := c.captureOneOff(capCtx, id, kind)
		cancelCapture()
		d.Index = commandIndex(kind, ordinal)
		if d.ExitCode == nil {
			d.ExitCode = &exit
		}
		return "", &oneOffFailure{err: fmt.Errorf("%s failed with exit code %d", kind, exit), diagnostic: d}
	}
	if !version {
		return "", nil
	}
	output, err := e.ContainerLogs(ctx, id)
	if err != nil {
		return "", errors.New("schema version unavailable")
	}
	v, err := safeVersion(output, p)
	if err != nil {
		return "", err
	}
	// Journal the version so recovery keeps the schema state even if this process never returns it.
	receipt, _ = json.Marshal(struct {
		ID      string `json:"id"`
		Exit    int    `json:"exit"`
		Version string `json:"version"`
	}{id, exit, v})
	if err = c.Store.MarkStep(ctx, opID, ordinal, "finished", receipt); err != nil {
		return "", ErrOneOffUncertain
	}
	return v, nil
}

// uncertainOneOff is the error of a one-off whose outcome is unknown, such as one that ran past its time limit. It carries
// what the container shows so far and leaves the container alone: recovery stops it.
func (c *Controller) uncertainOneOff(parent, ctx context.Context, cause error, id, kind string, ordinal int, timeout time.Duration) error {
	capCtx, cancel := captureContext(parent)
	defer cancel()
	d := c.captureOneOff(capCtx, id, kind)
	d.Index = commandIndex(kind, ordinal)
	if errors.Is(cause, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		addNote(&d, errTimedOut+" after "+timeout.String())
	}
	return &oneOffFailure{err: ErrOneOffUncertain, diagnostic: d}
}

// commandIndex is the 1-based position of a pre or post-deploy command among the manifest's, or 0 for any other one-off.
func commandIndex(kind string, ordinal int) int {
	switch {
	case kind == "pre_hook" && ordinal >= preHookOrdinal:
		return (ordinal-preHookOrdinal)/attemptsPerCommand + 1
	case kind == "post_hook" && ordinal >= postHookOrdinal:
		return (ordinal-postHookOrdinal)/attemptsPerCommand + 1
	}
	return 0
}
func safeVersion(output []byte, p preparedPlan) (string, error) {
	if len(output) > 1024 || !utf8.Valid(output) {
		return "", errors.New("invalid schema version")
	}
	s := strings.TrimSuffix(string(output), "\n")
	s = strings.TrimSuffix(s, "\r")
	if s == "" || strings.ContainsAny(s, "\r\n") || strings.IndexFunc(s, unicode.IsControl) >= 0 {
		return "", errors.New("invalid schema version")
	}
	private := map[string]bool{}
	for _, name := range p.Request.Manifest.Apps[p.Request.App].Env.Secret {
		private[name] = true
	}
	for _, entry := range p.Env {
		key, value, ok := strings.Cut(entry, "=")
		if ok && private[key] && value != "" && strings.Contains(s, value) {
			return "", errors.New("invalid schema version")
		}
	}
	if p.Auth != nil && p.Auth.Password != "" && strings.Contains(s, p.Auth.Password) {
		return "", errors.New("invalid schema version")
	}
	return s, nil
}

// recoverHooks never restarts a potentially executed command.
func (c *Controller) recoverHooks(ctx context.Context, opID string) error {
	e, ok := c.Engine.(oneOffEngine)
	if !ok {
		return errors.New("one-off recovery unavailable")
	}
	steps, err := c.Store.Intents(ctx, opID)
	if err != nil {
		return err
	}
	for _, step := range steps {
		if step.Kind != "oneoff" {
			continue
		}
		live, err := e.InspectContainer(ctx, step.Name)
		if err != nil {
			var status *engine.StatusError
			if errors.As(err, &status) && status.Code == 404 {
				continue
			}
			return errors.New("one-off recovery inspection failed")
		}
		if live.Labels["berthy.environment"] != c.Environment || live.Labels["berthy.operation"] != opID || live.Labels["berthy.oneoff"] != step.Role {
			return errors.New("one-off recovery ownership mismatch")
		}
		if live.Running {
			if err = e.StopContainer(ctx, live.ID, 10); err != nil {
				return errors.New("one-off recovery stop failed")
			}
			live, err = e.InspectContainer(ctx, live.ID)
			if err != nil || live.Running {
				return errors.New("one-off recovery stop unconfirmed")
			}
		}
	}
	return nil
}
