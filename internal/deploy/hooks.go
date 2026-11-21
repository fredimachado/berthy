package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"berthy/internal/engine"
	"berthy/internal/manifest"
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
		if _, err := c.runRetrying(ctx, opID, p, oneOff{Ordinal: preHookOrdinal + attemptsPerCommand*i, Kind: "pre_hook", Argv: h.Run, Timeout: h.TimeoutDuration()}, h.RetryCount()); err != nil {
			return "", "", err
		}
	}
	m := app.Migrations
	if m == nil {
		return "", "", nil
	}
	// The migration cannot be stopped without leaving the schema half changed, so a cancel is refused until it is over.
	leave, err := c.enter(ctx, reasonMigration)
	if err != nil {
		return "", "", err
	}
	defer leave()
	timeout := m.TimeoutDuration()
	before, err := c.runOneOff(ctx, opID, p, oneOff{Ordinal: schemaBeforeOrdinal, Kind: "schema_before", Argv: m.Version, Version: true, Timeout: timeout})
	if err != nil {
		return "", "", err
	}
	// Only apply is retried: version only reads, and a half-applied migration is re-run only when the manifest says its
	// command is safe to run again.
	_, applyErr := c.runRetrying(ctx, opID, p, oneOff{Ordinal: migrationApplyOrdinal, Kind: "migration_apply", Argv: m.Apply, Timeout: timeout}, m.RetryCount())
	if errors.Is(applyErr, ErrOneOffUncertain) {
		return before, "", applyErr
	}
	after, afterErr := c.runOneOff(ctx, opID, p, oneOff{Ordinal: schemaAfterOrdinal, Kind: "schema_after", Argv: m.Version, Version: true, Timeout: timeout})
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
		if _, err := c.runRetrying(ctx, opID, p, oneOff{Ordinal: postHookOrdinal + attemptsPerCommand*i, Kind: "post_hook", Argv: h.Run, Timeout: h.TimeoutDuration()}, h.RetryCount()); err != nil {
			return err
		}
	}
	return nil
}

// runRetrying runs a command, and runs it again in a new one-off container after a non-zero exit, up to retries more
// times with oneOffRetryDelay between attempts. An attempt that timed out or whose outcome is otherwise uncertain is never
// retried: the command may still be running. The error of the last attempt is returned, with the number of attempts in its
// diagnostic. o.Ordinal is the journal ordinal of the first attempt.
func (c *Controller) runRetrying(parent context.Context, opID string, p preparedPlan, o oneOff, retries int) (string, error) {
	first, kind := o.Ordinal, o.Kind
	for attempt := 0; ; attempt++ {
		o.Ordinal = first + attempt
		v, err := c.runOneOff(parent, opID, p, o)
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
		// A cancel ends the wait too: the next attempt then stops at once with the *Cancelled.
		waitCtx, stop := interruptible(parent)
		select {
		case <-waitCtx.Done():
		case <-time.After(oneOffRetryDelay):
		}
		stop()
		if parent.Err() != nil {
			return v, err
		}
	}
}

// oneOff is one command run in a new container of the prepared image.
type oneOff struct {
	Ordinal int
	// Kind is the berthy.oneoff label and the intent's role: pre_hook, post_hook, schema_before, migration_apply, schema_after,
	// the schema revert steps, or run.
	Kind string
	// Role is the role whose environment the container takes. It is empty for a hook or a migration, which belong to the app.
	Role    string
	Argv    []string
	Version bool // the command prints a schema version, which is read from its output
	Timeout time.Duration
	// Resources are the limits of the container. Nil means none.
	Resources *manifest.Resources
	// Volumes are the named volumes of the container, as the role writes them. Empty means none.
	Volumes []string
}

// runOneOff runs a command in a new container and waits for it. A cancel of the operation stops the container and ends the
// command with the *Cancelled, whether the cancel came before the command began or while it ran.
func (c *Controller) runOneOff(parent context.Context, opID string, p preparedPlan, o oneOff) (string, error) {
	if err := cancelledBy(parent); err != nil {
		return "", err
	}
	waitCtx, stop := interruptible(parent)
	defer stop()
	v, err := c.runOneOffUntilDone(waitCtx, opID, p, o)
	if err != nil {
		if cancel := cancelledBy(parent); cancel != nil {
			return "", c.settleCancelled(parent, opID, o.Ordinal, cancel)
		}
	}
	return v, err
}

// settleCancelled stops the container of a command a cancel interrupted and journals that it is over, so nothing is left running
// under the lock the operation is about to release. It returns the *Cancelled, or ErrOneOffUncertain when the container could
// not be stopped, which leaves the operation to recovery as a command that timed out does.
func (c *Controller) settleCancelled(ctx context.Context, opID string, ordinal int, cancel error) error {
	steps, err := c.Store.Intents(ctx, opID)
	if err != nil {
		return ErrOneOffUncertain
	}
	var step *state.Intent
	for i := range steps {
		if steps[i].Kind == "oneoff" && steps[i].Ordinal == ordinal {
			step = &steps[i]
		}
	}
	if step == nil {
		return cancel
	}
	var receipt struct {
		ID string `json:"id"`
	}
	if len(step.Receipt) > 0 && json.Unmarshal(step.Receipt, &receipt) != nil {
		return ErrOneOffUncertain
	}
	container := receipt.ID
	if container == "" {
		// A container that was never confirmed is stopped by its name.
		container = step.Name
	}
	e, ok := c.Engine.(oneOffEngine)
	if !ok {
		return ErrOneOffUncertain
	}
	stopCtx, done := captureContext(ctx)
	defer done()
	var status *engine.StatusError
	if err = e.StopContainer(stopCtx, container, runStopGrace); err != nil && !(errors.As(err, &status) && status.Code == 404) {
		return ErrOneOffUncertain
	}
	if step.Phase != "finished" {
		finished, _ := json.Marshal(struct {
			ID   string `json:"id"`
			Exit int    `json:"exit"`
		}{receipt.ID, -1})
		if err = c.Store.MarkStep(stopCtx, opID, ordinal, "finished", finished); err != nil {
			return ErrOneOffUncertain
		}
	}
	return cancel
}

// reserveOneOffMounts leases the resolved Docker volumes before any one-off container is created.
func (c *Controller) reserveOneOffMounts(ctx context.Context, p preparedPlan, volumes []string) (reservation, error) {
	if c.Reserve == nil {
		return reservation{}, nil
	}
	var claims []state.Claim
	for _, raw := range volumes {
		mount, err := manifest.ParseMount(raw)
		if err != nil {
			return reservation{}, err
		}
		claim := state.Claim{Kind: state.ClaimVolume, Name: p.Request.Manifest.VolumeName(p.Identity.Environment, mount.Source)}
		if !slices.Contains(claims, claim) {
			claims = append(claims, claim)
		}
	}
	return c.reserveClaims(ctx, claims)
}

func (c *Controller) runOneOffUntilDone(parent context.Context, opID string, p preparedPlan, o oneOff) (string, error) {
	ordinal, kind, argv, version, timeout := o.Ordinal, o.Kind, o.Argv, o.Version, o.Timeout
	if c.Store == nil || len(argv) == 0 || argv[0] == "" {
		return "", errors.New("invalid one-off command")
	}
	e, ok := c.Engine.(oneOffEngine)
	if !ok {
		return "", errors.New("one-off containers unavailable")
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	name := fmt.Sprintf("berthy-%s-%s-%s-%d", kind, c.id().Slug(), opID, ordinal)
	labels := c.id().Labels()
	labels["berthy.operation"], labels["berthy.oneoff"] = opID, kind
	spec, err := p.oneOffSpec(argv, o.Role, o.Resources, o.Volumes, labels, nil)
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

// terminal says how a one-off container serves a person: its standard input stays open and is attached, and a terminal is
// allocated when TTY is set. A batch one-off has none.
// terminal is how an attached one-off container is made: with a terminal or without, and with or without a log. NoLog is for a
// command whose output is the product, such as a database dump, which Docker would otherwise copy to a log on the server's disk.
type terminal struct{ TTY, NoLog bool }

// oneOffSpec is the Docker spec of a one-off container of the prepared image: the command as entrypoint and arguments, the
// environment of role, the labels, the limits, the named volumes of the role, and the shared network. Every kind of one-off is
// built here, so a hook, a run, a job and an interactive session differ only in the labels and in term, which is nil for a
// batch one. A hook belongs to the app and has no role, so it has no limits and no volumes.
func (p preparedPlan) oneOffSpec(argv []string, role string, limits *manifest.Resources, volumes []string, labels map[string]string, term *terminal) (json.RawMessage, error) {
	nano, memory, err := resourceLimits(limits)
	if err != nil {
		return nil, err
	}
	mounts, err := volumeMounts(volumes, func(v string) string { return p.Request.Manifest.VolumeName(p.Identity.Environment, v) })
	if err != nil {
		return nil, err
	}
	var host *hostConfig
	if nano != 0 || memory != 0 || len(mounts) > 0 || term != nil && term.NoLog {
		host = &hostConfig{NanoCPUs: nano, Memory: memory, Mounts: mounts}
		if term != nil && term.NoLog {
			host.LogConfig = &logConfig{Type: "none"}
		}
	}
	spec := struct {
		Image            string            `json:"Image"`
		Entrypoint       []string          `json:"Entrypoint"`
		Cmd              []string          `json:"Cmd"`
		Env              []string          `json:"Env,omitempty"`
		Labels           map[string]string `json:"Labels"`
		HostConfig       *hostConfig       `json:"HostConfig,omitempty"`
		NetworkingConfig any               `json:"NetworkingConfig"`
		Tty              bool              `json:"Tty,omitempty"`
		OpenStdin        bool              `json:"OpenStdin,omitempty"`
		StdinOnce        bool              `json:"StdinOnce,omitempty"`
		AttachStdin      bool              `json:"AttachStdin,omitempty"`
		AttachStdout     bool              `json:"AttachStdout,omitempty"`
		AttachStderr     bool              `json:"AttachStderr,omitempty"`
	}{Image: p.Image, Entrypoint: argv[:1], Cmd: argv[1:], Env: p.containerEnv(role), Labels: labels, HostConfig: host,
		NetworkingConfig: map[string]any{"EndpointsConfig": map[string]any{p.NetworkID: map[string]any{}}}}
	if term != nil {
		spec.Tty, spec.OpenStdin, spec.StdinOnce, spec.AttachStdin, spec.AttachStdout, spec.AttachStderr = term.TTY, true, true, true, true, true
	}
	return json.Marshal(spec)
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
	for _, name := range p.Request.Manifest.Apps[p.Request.App].Env.SensitiveVars() {
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
		if !c.id().Owns(live.Labels) || live.Labels["berthy.operation"] != opID || live.Labels["berthy.oneoff"] != step.Role {
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
