package deploy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"berthy/internal/engine"
	"berthy/internal/manifest"
	"berthy/internal/scope"
	"berthy/internal/state"
)

const (
	diagnosticTasksPerRole = 5
	diagnosticCallTimeout  = 5 * time.Second
	diagnosticCaptureLimit = time.Minute
	diagnosticLogLines     = 200
	diagnosticLogBytes     = 64 << 10
	diagnosticTotalBytes   = 1 << 20
	maxReasonBytes         = 300
	maxSwarmErrorBytes     = 100

	// errTimedOut is the Error of a one-off that did not finish within its time limit.
	errTimedOut = "timed out"
	// errLogsUnavailable marks output that could not be read or safely kept.
	errLogsUnavailable = "logs unavailable"
)

// Diagnostic is evidence about one failed piece of an operation.
type Diagnostic struct {
	Kind        string               `json:"kind"`            // "task", "oneoff", "backup" or "compose"
	Name        string               `json:"name"`            // service name, one-off hook role, backup name or Compose command
	Role        string               `json:"role,omitempty"`  // of a backup: the hook list it ran in, "pre_hook" or "post_hook"
	Index       int                  `json:"index,omitempty"` // 1-based position of a pre or post-deploy command or backup in the manifest
	TaskID      string               `json:"task_id,omitempty"`
	Container   string               `json:"container,omitempty"`
	State       string               `json:"state,omitempty"` // Swarm task state or container state
	Error       string               `json:"error,omitempty"` // Swarm task error or container error
	ExitCode    *int                 `json:"exit_code,omitempty"`
	OOMKilled   bool                 `json:"oom_killed,omitempty"`
	Attempts    int                  `json:"attempts,omitempty"`      // how many times a retried one-off ran, when more than once
	GaveUpAfter int                  `json:"gave_up_after,omitempty"` // the start attempts a rollout used up before it gave up on this role
	Health      string               `json:"health,omitempty"`
	HealthLog   []engine.HealthProbe `json:"health_log,omitempty"`
	Logs        string               `json:"logs,omitempty"` // last lines, secrets redacted
	Truncated   bool                 `json:"truncated,omitempty"`
	Captured    time.Time            `json:"captured"`
}

// diagnosticEngine is what capture needs from Docker beyond the rollout. An engine without it records the Swarm view only.
type diagnosticEngine interface {
	DiagnoseContainer(context.Context, string) (engine.ContainerDiagnosis, error)
	LogTail(context.Context, string, int, int) (string, bool, error)
}

// oneOffFailure carries the evidence of a failed one-off up to the operation that ran it.
type oneOffFailure struct {
	err        error
	diagnostic Diagnostic
}

func (f *oneOffFailure) Error() string { return f.err.Error() }
func (f *oneOffFailure) Unwrap() error { return f.err }

// failureDiagnostics returns the evidence a failed hook or migration carried, or nil.
func failureDiagnostics(err error) []Diagnostic {
	var f *oneOffFailure
	if errors.As(err, &f) {
		return []Diagnostic{f.diagnostic}
	}
	return nil
}

// captureContext outlives the context of the operation that failed, whose deadline has usually passed by now.
func captureContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), diagnosticCaptureLimit)
}

// Redactor is redactor for callers outside the package: a function that replaces every managed secret value in its argument,
// or false when the secrets cannot be read.
func (c *Controller) Redactor(ctx context.Context) (func(string) string, bool) {
	return c.redactor(ctx)
}

// redactor returns a function that replaces every managed secret value in its argument. It reports false when the secrets
// cannot be read: nothing that might hold one is stored then.
func (c *Controller) redactor(ctx context.Context) (func(string) string, bool) {
	if c.Store == nil {
		return nil, false
	}
	secrets, err := c.redactionValues(ctx)
	if err != nil {
		return nil, false
	}
	return func(s string) string { return redactOutput([]byte(s), secrets) }, true
}

func addNote(d *Diagnostic, note string) {
	if d.Error == "" {
		d.Error = note
	} else if !strings.Contains(d.Error, note) {
		d.Error += "; " + note
	}
}

// inspectInto adds what Docker knows about the container to d and reports whether it could be read.
func inspectInto(ctx context.Context, de diagnosticEngine, d *Diagnostic, redact func(string) string) {
	callCtx, cancel := context.WithTimeout(ctx, diagnosticCallTimeout)
	diag, err := de.DiagnoseContainer(callCtx, d.Container)
	cancel()
	if err != nil {
		addNote(d, "container unavailable")
		return
	}
	if diag.OOMKilled {
		d.OOMKilled = true
	}
	if diag.Error != "" {
		addNote(d, redact(diag.Error))
	}
	if d.State == "" {
		d.State = diag.State
	}
	switch diag.State {
	case "exited", "dead":
		code := diag.ExitCode
		d.ExitCode = &code
	}
	d.Health = diag.Health
	for _, probe := range diag.HealthLog {
		probe.Output = redact(probe.Output)
		d.HealthLog = append(d.HealthLog, probe)
	}
}

// logsInto adds the container's last lines to d, redacted.
func logsInto(ctx context.Context, de diagnosticEngine, d *Diagnostic, redact func(string) string, redactable bool) {
	if !redactable {
		addNote(d, errLogsUnavailable)
		return
	}
	callCtx, cancel := context.WithTimeout(ctx, diagnosticCallTimeout)
	text, truncated, err := de.LogTail(callCtx, d.Container, diagnosticLogLines, diagnosticLogBytes)
	cancel()
	if err != nil {
		addNote(d, errLogsUnavailable)
		return
	}
	d.Logs = redact(text)
	d.Truncated = truncated
}

// captureRollout collects evidence for every task of the operation's generation of every role in the rollout, before
// compensation removes anything: newest tasks first, at most five per role, including failed, shut down and rejected ones.
// A task that runs healthy needs none. Each Docker call has a short timeout, and a piece that cannot be read is noted in
// that diagnostic's Error instead of failing the capture. The total is kept within 1 MiB by dropping the logs of the oldest
// tasks first.
func (c *Controller) captureRollout(ctx context.Context, opID string, work []roleWork) []Diagnostic {
	redact, redactable := c.redactor(ctx)
	if !redactable {
		redact = func(string) string { return "" }
	}
	de, _ := c.Engine.(diagnosticEngine)
	var out []Diagnostic
	for _, w := range work {
		out = append(out, c.captureRole(ctx, opID, w.intent.Role, w.intent.Name, w.intent.ServiceID, w.uptime, de, redact, redactable)...)
	}
	return capDiagnostics(out)
}

func (c *Controller) captureRole(ctx context.Context, opID, role, name, serviceID string, uptime time.Duration, de diagnosticEngine, redact func(string) string, redactable bool) []Diagnostic {
	unavailable := func(note string) []Diagnostic {
		return []Diagnostic{{Kind: "task", Name: name, Role: role, Error: note, Captured: time.Now().UTC()}}
	}
	callCtx, cancel := context.WithTimeout(ctx, diagnosticCallTimeout)
	defer cancel()
	if serviceID == "" {
		live, err := c.Engine.InspectService(callCtx, name)
		if err != nil {
			var status *engine.StatusError
			if errors.As(err, &status) && status.Code == 404 {
				return nil // the role was never created
			}
			return unavailable("service unavailable")
		}
		serviceID = live.ID
	}
	tasks, err := c.Engine.ServiceTasks(callCtx, serviceID)
	if err != nil {
		return unavailable("tasks unavailable")
	}
	var mine []engine.ServiceTask
	for _, t := range tasks {
		if t.ServiceID == serviceID && t.Labels["berthy.operation"] == opID {
			mine = append(mine, t)
		}
	}
	sort.SliceStable(mine, func(i, j int) bool {
		if !mine[i].Timestamp.Equal(mine[j].Timestamp) {
			return mine[i].Timestamp.After(mine[j].Timestamp)
		}
		return mine[i].Version > mine[j].Version
	})
	var out []Diagnostic
	now := time.Now()
	for _, t := range mine {
		if len(out) == diagnosticTasksPerRole {
			break
		}
		if uptime > 0 {
			if ready, _ := taskReady(ctx, c.Engine, t, uptime, now); ready {
				continue
			}
		}
		d := Diagnostic{Kind: "task", Name: name, Role: role, TaskID: t.ID, Container: t.ContainerID, State: t.State, Captured: time.Now().UTC()}
		if t.Err != "" {
			d.Error = redact(t.Err)
		} else if t.State == "rejected" || t.State == "failed" {
			d.Error = redact(t.Message)
		}
		if t.ExitCode != nil {
			code := *t.ExitCode
			d.ExitCode = &code
		}
		if t.ContainerID != "" && de != nil {
			inspectInto(ctx, de, &d, redact)
			if uptime == 0 && t.State == "running" && d.Health == "healthy" {
				continue
			}
			logsInto(ctx, de, &d, redact, redactable)
		}
		out = append(out, d)
	}
	return out
}

// captureOneOff collects evidence for a failed or timed-out one-off container: its state, exit code, error and last lines
// of output.
func (c *Controller) captureOneOff(ctx context.Context, containerID, role string) Diagnostic {
	d := Diagnostic{Kind: "oneoff", Name: role, Container: containerID, Captured: time.Now().UTC()}
	de, ok := c.Engine.(diagnosticEngine)
	if !ok || containerID == "" {
		return d
	}
	redact, redactable := c.redactor(ctx)
	if !redactable {
		redact = func(string) string { return "" }
	}
	inspectInto(ctx, de, &d, redact)
	logsInto(ctx, de, &d, redact, redactable)
	return d
}

// capDiagnostics keeps the stored size of the diagnostics within diagnosticTotalBytes. It drops the logs of the oldest
// diagnostics, which come last, first, and marks each one Truncated.
func capDiagnostics(d []Diagnostic) []Diagnostic {
	sizes := make([]int, len(d))
	total := 0
	for i := range d {
		b, _ := json.Marshal(d[i])
		sizes[i] = len(b)
		total += len(b)
	}
	for i := len(d) - 1; i >= 0 && total > diagnosticTotalBytes; i-- {
		if d[i].Logs == "" {
			continue
		}
		total -= len(d[i].Logs)
		d[i].Logs = ""
		d[i].Truncated = true
	}
	return d
}

// storeDiagnostics keeps the evidence with the operation and returns the public reason it adds up to. Recording the
// evidence is best effort: a failure to store it must not hide the failure it explains.
func (c *Controller) storeDiagnostics(ctx context.Context, opID string, d []Diagnostic, fallback string) string {
	if len(d) > 0 && c.Store != nil {
		items := make([]json.RawMessage, 0, len(d))
		for _, item := range d {
			if b, err := json.Marshal(item); err == nil {
				items = append(items, b)
			}
		}
		_ = c.Store.PutDiagnostics(ctx, opID, items) //nolint:errcheck // Diagnostics are best effort and must not hide the rollout failure.
	}
	return failureReason(d, fallback)
}

// recordRolloutFailure captures and stores the evidence of a failed rollout and returns the public reason. It runs before
// anything is cleaned up. fallback is the reason when no task left evidence.
func (c *Controller) recordRolloutFailure(ctx context.Context, opID string, work []roleWork, fallback string, gaveUp *errStartAttempts) string {
	capCtx, cancel := captureContext(ctx)
	defer cancel()
	d := c.captureRollout(capCtx, opID, work)
	if gaveUp != nil {
		for i := range d {
			if d[i].Kind == "task" && d[i].Role == gaveUp.Role {
				d[i].GaveUpAfter = gaveUp.Limit
			}
		}
	}
	return c.storeDiagnostics(capCtx, opID, d, fallback)
}

func (c *Controller) recordComposeUpFailure(ctx context.Context, opID string, req Request, stderr []byte) {
	if len(stderr) == 0 {
		return
	}
	capCtx, cancel := captureContext(ctx)
	defer cancel()
	d := Diagnostic{Kind: "compose", Name: "up", Captured: time.Now().UTC()}
	values, inventoryErr := c.redactionValues(capCtx)
	vars, envErr := c.resolveEnv(capCtx, req.Manifest.Apps[req.App].Env)
	if inventoryErr != nil || envErr != nil {
		d.Error = errLogsUnavailable
	} else {
		for _, v := range vars {
			if v.Sensitive {
				values = append(values, []byte(v.Value))
			}
		}
		id := scope.ID{Project: req.Manifest.Project, Environment: c.Environment}
		d.Logs, d.Truncated = composeDiagnosticTail(stderr, values, id)
	}
	c.storeDiagnostics(capCtx, opID, capDiagnostics([]Diagnostic{d}), string(errComposeUp))
}

func composeDiagnosticTail(stderr []byte, secrets [][]byte, id scope.ID) (logs string, truncated bool) {
	stderr = stderr[max(0, len(stderr)-engine.MaxComposeOutput-1):]
	window := max(0, len(stderr)-engine.MaxComposeOutput)
	tokens := regexp.MustCompile(id.TokenExpression())
	ends := composeCredentialEnds(stderr, secrets, id, tokens, window > 0)
	if !utf8.Valid(stderr) {
		// Repair can join bytes that form a credential. Map those matches back so they merge with raw matches.
		repaired, offsets := composeUTF8Offsets(stderr)
		for i, end := range composeCredentialEnds(repaired, secrets, id, tokens, window > 0) {
			if end > 0 {
				start := offsets[i]
				ends[start] = max(ends[start], offsets[end-1]+1)
			}
		}
	}

	var redacted strings.Builder
	clearFrom := window
	for i := 0; i < len(ends); {
		start, end := i, ends[i]
		i++
		if end == 0 {
			continue
		}
		for i < end {
			end = max(end, ends[i])
			i++
		}
		if end > clearFrom {
			redacted.Write(stderr[clearFrom:max(clearFrom, start)])
			redacted.WriteString("[redacted]")
			clearFrom = end
		}
	}
	redacted.Write(stderr[clearFrom:])
	logs = strings.ToValidUTF8(redacted.String(), "")
	start := max(0, len(logs)-diagnosticLogBytes)
	last := len(strings.TrimSuffix(logs, "\n"))
	lines := 0
	for i := last - 1; i >= start; i-- {
		if logs[i] == '\n' {
			lines++
			if lines == diagnosticLogLines {
				start = i + 1
				break
			}
		}
	}
	for start < len(logs) && !utf8.RuneStart(logs[start]) {
		start++
	}
	return logs[start:], window > 0 || start > 0
}

func composeUTF8Offsets(data []byte) (repaired []byte, offsets []int) {
	for i := 0; i < len(data); {
		r, size := utf8.DecodeRune(data[i:])
		if r != utf8.RuneError || size > 1 {
			repaired = append(repaired, data[i:i+size]...)
			for j := range size {
				offsets = append(offsets, i+j)
			}
		}
		i += size
	}
	return repaired, offsets
}

// For each credential start, keep the furthest end of any match.
func composeCredentialEnds(data []byte, secrets [][]byte, id scope.ID, tokens *regexp.Regexp, cut bool) []int {
	ends := make([]int, len(data))
	if len(data) == 0 {
		return ends
	}
	for _, secret := range secrets {
		if len(secret) == 0 {
			continue
		}
		markComposeLiteralMatches(data, secret, ends)
		if cut {
			ends[0] = max(ends[0], composeLiteralSuffixOverlap(data, secret))
		}
	}
	for offset := 0; offset < len(data); {
		match := tokens.FindIndex(data[offset:])
		if match == nil {
			break
		}
		start, end := offset+match[0], offset+match[1]
		ends[start] = max(ends[start], end)
		// A token ending in b can share that byte with the start of the next token.
		offset = start + 1
	}
	if cut {
		ends[0] = max(ends[0], id.TokenSuffixOverlap(data))
	}
	return ends
}

func markComposeLiteralMatches(data, value []byte, ends []int) {
	first := bytes.Index(data, value)
	if first < 0 {
		return
	}
	end := first + len(value)
	ends[first] = max(ends[first], end)
	fallback := composePrefixTable(value)
	matched := fallback[len(value)-1]
	for i := end; i < len(data); i++ {
		matched = composeMatchByte(value, fallback, matched, data[i])
		if matched == len(value) {
			start := i + 1 - len(value)
			ends[start] = max(ends[start], i+1)
			matched = fallback[matched-1]
		}
	}
}

func composeLiteralSuffixOverlap(data, value []byte) int {
	n := min(len(data), len(value)-1)
	suffix := value[len(value)-n:]
	if bytes.IndexByte(suffix, data[0]) < 0 {
		return 0
	}
	pattern := data[:n]
	fallback := composePrefixTable(pattern)
	matched := 0
	for _, b := range suffix {
		matched = composeMatchByte(pattern, fallback, matched, b)
	}
	return matched
}

// The fallback table preserves overlapping matches without comparing the same run of bytes repeatedly.
func composePrefixTable(pattern []byte) []int {
	table := make([]int, len(pattern))
	for i := 1; i < len(pattern); i++ {
		table[i] = composeMatchByte(pattern, table, table[i-1], pattern[i])
	}
	return table
}

func composeMatchByte(pattern []byte, fallback []int, matched int, b byte) int {
	for matched > 0 && pattern[matched] != b {
		matched = fallback[matched-1]
	}
	if pattern[matched] == b {
		matched++
	}
	return matched
}

// captureInterrupted keeps the evidence of an interrupted rollout before recovery compensates it, unless the operation
// already has some. It returns the public reason the evidence adds up to, or "".
func (c *Controller) captureInterrupted(ctx context.Context, opID string, intents []state.Intent) string {
	if existing, err := c.Store.Diagnostics(ctx, opID); err != nil || len(existing) > 0 {
		return ""
	}
	var work []roleWork
	for _, i := range intents {
		if i.Kind == "create" || i.Kind == "update" {
			work = append(work, roleWork{intent: i})
		}
	}
	if len(work) == 0 {
		return ""
	}
	return c.recordRolloutFailure(ctx, opID, work, "", nil)
}

// recordOneOffFailure stores the evidence a failed hook or migration carried and returns the public reason, or "" when the
// failure carried none.
func (c *Controller) recordOneOffFailure(ctx context.Context, opID string, err error) string {
	capCtx, cancel := captureContext(ctx)
	defer cancel()
	return c.storeDiagnostics(capCtx, opID, failureDiagnostics(err), "")
}

// rolloutFallback is the public reason when no task left evidence, chosen from the rollout's own error. The texts are fixed.
func rolloutFallback(cause error) string {
	var final finalRolloutFailure
	if errors.As(cause, &final) {
		return final.Error()
	}
	var gaveUp *errStartAttempts
	if errors.As(cause, &gaveUp) {
		return fmt.Sprintf("role %s: gave up after %d failed starts", printable(gaveUp.Role, 64), gaveUp.Limit)
	}
	if cause != nil {
		switch cause.Error() {
		case "service health deadline exceeded":
			return "deploy deadline exceeded before the task was healthy"
		case "update completion deadline exceeded":
			return "deploy deadline exceeded before the service update completed"
		case "native update did not complete":
			return "service update did not complete"
		}
	}
	return "rollout failed"
}

// addStoppedFirst adds to the reason of a failed rollout that each role updated stop-first was down until its previous version
// came back. work is the roles the rollout reached, so a role it never got to is not named. The reason stays within
// maxReasonBytes; the cause is cut to make room for the clause, not the clause.
func addStoppedFirst(reason string, app manifest.App, work []roleWork) string {
	var clauses []string
	for _, w := range work {
		if w.intent.Kind != "update" {
			continue
		}
		if order, _ := app.UpdateOrder(w.intent.Role); order == manifest.StopFirst {
			clauses = append(clauses, "role "+printable(w.intent.Role, 64)+" was stopped first, so it was down until the previous version came back")
		}
	}
	if len(clauses) == 0 {
		return reason
	}
	note := strings.Join(clauses, "; ")
	cause := reason
	if room := maxReasonBytes - len(note) - len("; "); len(cause) > room {
		cause = printable(cause, max(room, 0))
	}
	if cause == "" {
		return printable(note, maxReasonBytes)
	}
	return printable(cause+"; "+note, maxReasonBytes)
}

// failureReason summarizes diagnostics into one public line, "role <r>: <what happened>" per role joined by "; ", and
// "<command> exited with code N" for a one-off. It never includes log or health-check text: those can hold secrets or
// personal data and live only in the sealed diagnostics. fallback is the reason when the diagnostics say nothing.
func failureReason(d []Diagnostic, fallback string) string {
	var parts []string
	seen := map[string]bool{}
	for _, item := range d {
		switch item.Kind {
		case "task":
			if seen[item.Role] {
				continue
			}
			seen[item.Role] = true
			var tasks []Diagnostic
			for _, t := range d {
				if t.Kind == "task" && t.Role == item.Role {
					tasks = append(tasks, t)
				}
			}
			if cause := taskCause(tasks); cause != "" {
				parts = append(parts, "role "+printable(item.Role, 64)+": "+cause)
			}
		case "oneoff":
			parts = append(parts, oneOffCause(item))
		case "backup":
			parts = append(parts, backupCause(item))
		}
	}
	reason := strings.Join(parts, "; ")
	if reason == "" {
		reason = fallback
	}
	return printable(reason, maxReasonBytes)
}

// taskCause says what went wrong with one role's tasks, newest first.
func taskCause(tasks []Diagnostic) string {
	failed := 0
	for _, t := range tasks {
		if t.State == "failed" || t.State == "rejected" {
			failed++
		}
	}
	cause := ""
	if anyTask(tasks, func(t Diagnostic) bool { return t.OOMKilled }) != nil {
		cause = "task was killed: out of memory"
	} else if t := anyTask(tasks, neverHealthy); t != nil {
		cause = "container never became healthy"
		if n := len(t.HealthLog); n > 0 {
			cause += fmt.Sprintf(" (last check: exit %d)", t.HealthLog[n-1].ExitCode)
		}
	} else if t := anyTask(tasks, exitedOnItsOwn); t != nil {
		cause = fmt.Sprintf("task exited with code %d", *t.ExitCode)
	} else if t := anyTask(tasks, func(t Diagnostic) bool { return t.Error != "" && t.State != "running" }); t != nil {
		cause = "task failed: " + printable(t.Error, maxSwarmErrorBytes)
	} else if len(tasks) > 0 {
		cause = "deploy deadline exceeded before the task was healthy"
	}
	if cause != "" {
		if t := anyTask(tasks, func(t Diagnostic) bool { return t.GaveUpAfter > 0 }); t != nil {
			cause += fmt.Sprintf(" (gave up after %d failed starts)", t.GaveUpAfter)
		} else if failed > 1 {
			cause += fmt.Sprintf(" (%d failed tasks)", failed)
		}
	}
	return cause
}

func anyTask(tasks []Diagnostic, match func(Diagnostic) bool) *Diagnostic {
	for i := range tasks {
		if match(tasks[i]) {
			return &tasks[i]
		}
	}
	return nil
}

// neverHealthy: the health checks ran and the last one failed, or Docker marked a container unhealthy that is still there.
// A container that crashed before any check ran is not one: Docker calls it unhealthy too, but its exit code says more.
func neverHealthy(t Diagnostic) bool {
	if n := len(t.HealthLog); n > 0 {
		return t.HealthLog[n-1].ExitCode != 0
	}
	return t.Health == "unhealthy" && t.ExitCode == nil
}

// exitedOnItsOwn: the task's container ended and reported an exit code.
func exitedOnItsOwn(t Diagnostic) bool {
	return t.ExitCode != nil && t.State != "running" && t.State != "starting"
}

// oneOffLabel names a one-off for people: "pre-deploy command 2", "migration apply". index is the 1-based position of a hook.
func oneOffLabel(kind string, index int) string {
	switch kind {
	case "pre_hook":
		return fmt.Sprintf("pre-deploy command %d", max(index, 1))
	case "post_hook":
		return fmt.Sprintf("post-deploy command %d", max(index, 1))
	case "migration_apply":
		return "migration apply"
	case "schema_before", "schema_after", "revert_before", "revert_after":
		return "schema version command"
	case "migration_revert":
		return "migration revert"
	}
	return "one-off command " + printable(kind, 64)
}

func oneOffCause(d Diagnostic) string {
	what := oneOffLabel(d.Name, d.Index)
	var cause string
	switch {
	case d.OOMKilled:
		cause = what + " was killed: out of memory"
	case strings.HasPrefix(d.Error, errTimedOut):
		cause = what + " timed out"
	case d.ExitCode != nil:
		cause = fmt.Sprintf("%s exited with code %d", what, *d.ExitCode)
	default:
		cause = what + " outcome uncertain"
	}
	if d.Attempts > 1 {
		cause += fmt.Sprintf(" (after %d attempts)", d.Attempts)
	}
	return cause
}

// printable keeps the printable ASCII of s, replacing anything else with '?', and cuts it to limit bytes.
func printable(s string, limit int) string {
	var b bytes.Buffer
	for _, r := range s {
		if b.Len() >= limit {
			break
		}
		if r >= 0x20 && r < 0x7f {
			b.WriteRune(r)
		} else {
			b.WriteByte('?')
		}
	}
	return b.String()
}
