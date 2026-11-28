package agent

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode"

	"berthy/internal/deploy"
	"berthy/internal/monitor"
	"berthy/internal/scope"
	"berthy/internal/state"
	"berthy/internal/telemetry"
)

// Reports tells the alert channel and the telemetry backend about operations. It is the deploy controller's observer. Every
// operation, of every kind, becomes two log records when telemetry is configured. A deploy or a rollback that ends the way
// alerts.deploys says becomes one message on every alert channel. Neither ever makes an operation wait: the records are queued
// and the messages are sent in the background, and a failure is logged, without the bot token or the URL, and dropped.
type Reports struct {
	cfg   Config
	store *state.Store
	logs  *telemetry.Logs
	log   func(string)
	wg    sync.WaitGroup

	wakeAppChecks func()
}

// NewReports returns the observer of an agent with state. log receives operational messages, which never contain a secret; it
// may be nil.
func NewReports(c Config, s *state.Store, log func(string)) *Reports {
	return &Reports{
		cfg:   c,
		store: s,
		log:   log,
		logs: &telemetry.Logs{
			Project: c.Project,
			Env:     c.Environment,
			Config:  func(ctx context.Context) (*telemetry.Config, error) { return telemetryConfig(ctx, s) },
			Log:     log,
		},
	}
}

// Run sends the queued telemetry records until ctx ends, and then the ones still queued.
func (r *Reports) Run(ctx context.Context) { r.logs.Run(ctx) }

// Wait returns once the alert messages that were started have been sent or have failed.
func (r *Reports) Wait() { r.wg.Wait() }

// OperationStarted queues the start record of an operation.
func (r *Reports) OperationStarted(_ context.Context, e deploy.OperationEvent) {
	r.logs.Emit(operationRecord(r.cfg.ID(), e, e.Started, e.Kind+" started"))
}

// OperationEnded queues the end record of an operation and, for a deploy or rollback the policy lets through, sends the
// message. An operation that already ended as recovery_required and is decided later has its second end recorded and not
// alerted: the channel heard of the failure when it first ended.
func (r *Reports) OperationEnded(_ context.Context, e deploy.OperationEvent) {
	// Docker may have sent all its events before the applied release and bindings were committed.
	if r.wakeAppChecks != nil && (e.Kind == "deploy" || e.Kind == "rollback") && e.State != "recovery_required" {
		r.wakeAppChecks()
	}
	r.logs.Emit(operationRecord(r.cfg.ID(), e, e.Ended, e.Kind+" "+strings.ReplaceAll(e.State, "_", " ")))
	if !alertableKind(e) {
		return
	}
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		r.alert(e)
	}()
}

// alert sends the message of e when the manifest that decides it asks for one.
func (r *Reports) alert(e deploy.OperationEvent) {
	ctx, cancel := context.WithTimeout(context.Background(), alertTimeout)
	defer cancel()
	m := e.Manifest
	if m == nil {
		latest, err := settingsManifest(ctx, r.store)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return
		case err != nil:
			r.logf("deploy alert not sent: " + errAlertConfig.Error())
			return
		}
		m = &latest
	}
	if m.Alerts == nil || !wantsDeployAlert(m.Alerts.Deploys, e) {
		return
	}
	notifiers := alertNotifiers(ctx, r.cfg, r.store, *m.Alerts)
	if len(notifiers) == 0 {
		return
	}
	// The failure names the channel, never its URL or token.
	if err := notifiers.Notify(ctx, deployAlert(r.cfg.ID(), e)); err != nil {
		r.logf("deploy alert not sent: " + err.Error())
	}
}

func (r *Reports) logf(msg string) {
	if r.log != nil {
		r.log(msg)
	}
}

// alertTimeout bounds one message, as the monitor's own messages are bounded.
const alertTimeout = 15 * time.Second

// alertableKind reports whether the end of e could reach the alert channel at all: a deploy or a rollback, and not the later
// end of an operation that was reported when it first ended as recovery_required.
func alertableKind(e deploy.OperationEvent) bool {
	return (e.Kind == "deploy" || e.Kind == "rollback") && !e.Resumed
}

// wantsDeployAlert is the policy of alerts.deploys: failed (also when empty) sends a message when a deploy or
// rollback ended as failed, rolled_back, interrupted or recovery_required, all sends one for a success too, and none sends
// none.
func wantsDeployAlert(deploys string, e deploy.OperationEvent) bool {
	if !alertableKind(e) {
		return false
	}
	switch deploys {
	case "none":
		return false
	case "all":
		return true
	}
	return e.State != "succeeded"
}

// deployAlert is the alert of a finished deploy or rollback: the fields a webhook gets, and the text Telegram gets. The detail
// is the public reason for a failure, and the version for a success.
func deployAlert(id scope.ID, e deploy.OperationEvent) monitor.Alert {
	detail := oneLine(e.Version)
	if e.State != "succeeded" {
		detail = deployReason(e)
	}
	return monitor.Alert{Project: id.Project, Environment: id.Environment, Kind: "deploy", Name: oneLine(e.App), State: e.State, Detail: detail, Since: e.Ended, Text: deployAlertText(id, e)}
}

// deployReason is the public reason a deploy or rollback did not succeed, or empty.
func deployReason(e deploy.OperationEvent) string {
	reason := safeReason(e.Reason)
	if reason == "" {
		reason = safeOperationError(state.Operation{Err: e.Err})
	}
	return reason
}

// deployAlertText is the message of a finished deploy or rollback: the environment, the app and its version, who asked, and
// how long it took, and for a failure the public reason and the command that shows the operation's evidence. It holds no
// output of the application, only the reason the deploy package wrote.
func deployAlertText(id scope.ID, e deploy.OperationEvent) string {
	version := oneLine(e.Version)
	head := fmt.Sprintf("%s: %s", oneLine(id.String()), oneLine(e.App))
	tail := fmt.Sprintf("by %s in %s", oneLine(e.Actor), e.Ended.Sub(e.Started).Round(time.Second))
	rollback := e.Kind == "rollback"
	if e.State == "succeeded" {
		if rollback {
			return fmt.Sprintf("✅ %s rolled back to %s %s", head, version, tail)
		}
		return fmt.Sprintf("✅ %s %s deployed %s", head, version, tail)
	}
	reason := deployReason(e)
	var what string
	switch {
	case rollback && version != "":
		what = "rollback to " + version
	case rollback:
		what = "rollback"
	default:
		what = version
	}
	outcome := map[string]string{"failed": "failed", "rolled_back": "rolled back", "interrupted": "interrupted", "recovery_required": "needs recovery"}[e.State]
	if outcome == "" {
		outcome = e.State
	}
	line := "🔴 " + head
	if what != "" {
		line += " " + what
	}
	line += " " + outcome
	if reason != "" {
		line += ": " + reason
	}
	return fmt.Sprintf("%s\n%s\nberthy diagnostics %s -e %s --project %s", line, tail, oneLine(e.OperationID), oneLine(id.Environment), oneLine(id.Project))
}

// oneLine turns every control character into a space, so a name cannot add a line to a message.
func oneLine(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
}

// operationRecord is the log record of one operation start or end. Attributes whose value is unknown are left out.
func operationRecord(id scope.ID, e deploy.OperationEvent, at time.Time, body string) telemetry.LogRecord {
	rec := telemetry.LogRecord{Time: at, Severity: telemetry.SeverityInfo, Body: body}
	switch e.State {
	case "failed", "rolled_back", "interrupted", "recovery_required":
		rec.Severity = telemetry.SeverityWarn
	}
	for _, a := range [][2]string{
		{scope.LabelProject, id.Project},
		{scope.LabelEnvironment, id.Environment},
		{"berthy.app", e.App},
		{"berthy.operation.id", e.OperationID},
		{"berthy.operation.kind", e.Kind},
		{"berthy.version", e.Version},
		{"berthy.image.digest", e.Digest},
		{"berthy.operation.state", e.State},
		{"berthy.actor", e.Actor},
	} {
		if a[1] != "" {
			rec.Attrs = append(rec.Attrs, telemetry.String(a[0], a[1]))
		}
	}
	if !e.Ended.IsZero() {
		rec.Attrs = append(rec.Attrs, telemetry.Int("berthy.operation.duration_ms", e.Ended.Sub(e.Started).Milliseconds()))
	}
	return rec
}

var _ deploy.Observer = (*Reports)(nil)
