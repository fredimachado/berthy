package tui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"berthy/internal/agentclient"
	"berthy/internal/api"

	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/stopwatch"
	"charm.land/bubbles/v2/timer"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"
)

// The actions of the interface: restart, roll back, approve and reject. Each one goes through the same overlay, and nothing is
// sent that the person has not confirmed there with the agent's plan in front of them:
//
//	pick (a role or a release) -> plan (the dry run) -> confirm -> run (the request) -> follow (the operation) -> done

type actionKind int

const (
	actRestart actionKind = iota
	actRollback
	actApprove
	actReject
)

type actionStage int

const (
	stagePick actionStage = iota
	stagePlan
	stageConfirm
	stageRun
	stageFollow
	stageDone
)

// outcome is how an action ended.
type outcome int

const (
	outOK outcome = iota
	outHeld
	outFailed
	outRefused
	outLost
)

// actionResult is what the person is told when an action is over.
type actionResult struct {
	kind  outcome
	title string
	text  string
	hint  string
	// retry is true when pressing r tries the step again; diag is true when d opens the app.
	retry, diag bool
}

// The messages between the overlay and the requests it makes. id tells which overlay asked, so what comes late for one that is
// closed is ignored.
type (
	planMsg struct {
		id   int
		plan json.RawMessage
	}
	approvalRowsMsg struct {
		id   int
		rows []approvalRow
	}
	releasesMsg struct {
		id   int
		rows []historyRow
	}
	// sentMsg is the agent's answer to the change: an operation to follow, an approval that holds it, or a result.
	sentMsg struct {
		id      int
		op      string
		events  string
		held    string
		expires time.Time
		done    bool
		body    []byte
	}
	actionErrMsg struct {
		id      int
		status  int
		text    string
		network bool
		// lost is an operation that can no longer be followed.
		lost bool
	}
	opEventMsg struct {
		id   int
		text string
	}
	opDoneMsg struct {
		id            int
		state, reason string
	}
	formDoneMsg struct {
		id    int
		stage actionStage
	}
	// actionFinishedMsg tells the model that an action is over, so it reads everything again; actionCloseMsg that the overlay
	// went away, with a note for the status line and an app whose detail to open.
	actionFinishedMsg struct{}
	actionCloseMsg    struct{ note, openApp string }
)

var actionSeq atomic.Int64

// actionOverlay is one action in progress.
type actionOverlay struct {
	id        int
	c         *client
	now       func() time.Time
	st        styles
	env       string
	protected bool
	kind      actionKind
	stage     actionStage

	// What it acts on: an app (restart, roll back) or an approval (approve, reject).
	app, appPath, appKind string
	roles                 []string
	role                  string
	// roleLabel is the one role of an app that has one, which the plan names.
	roleLabel string
	release   int64
	releases  []historyRow
	// current is the release that runs now, which a roll back leaves; nil when the history has none.
	current *historyRow
	appr    approvalRow

	dryID, reqID string
	opID         string
	// failedStage is the step to try again after a failure that r can retry.
	failedStage actionStage

	loading bool
	form    *huh.Form
	// The values of the forms.
	typed, reason string
	sure          bool

	planRaw    json.RawMessage
	planHead   string
	planFacts  []planFact
	planEffect string
	planNote   string
	plan       viewport.Model
	planKey    string

	timer   timer.Model
	spin    spinner.Model
	watch   stopwatch.Model
	events  []string
	elapsed time.Duration
	ch      chan tea.Msg
	cancel  context.CancelFunc
	result  actionResult
}

// newActionOverlay makes an action ready and returns the command that starts it. app and appPath are the app of a restart or
// roll back; row is the approval of an approve or reject.
func newActionOverlay(m *Model, kind actionKind, app api.App, appPath string, row approvalRow) (*actionOverlay, tea.Cmd) {
	o := &actionOverlay{
		id: int(actionSeq.Add(1)), c: m.c, now: m.now, st: m.st, env: m.env, protected: m.secure, kind: kind,
		app: app.Ref.Name, appPath: appPath, appKind: app.Ref.Kind, appr: row,
		spin: spinner.New(spinner.WithSpinner(spinner.MiniDot), spinner.WithStyle(m.st.accent)),
	}
	o.plan = viewport.New()
	o.plan.MouseWheelEnabled = false
	if kind == actApprove || kind == actReject {
		o.app = approvalApp(row)
	}
	for _, r := range app.Roles {
		o.roles = append(o.roles, r.Name)
	}
	o.dryID, _ = agentclient.NewRequestID()
	switch {
	case kind == actRestart && app.Ref.Kind != "compose" && len(o.roles) > 1:
		o.stage = stagePick
		return o, o.roleForm()
	case kind == actRestart:
		return o, o.startPlan()
	case kind == actRollback:
		o.stage, o.loading = stagePick, true
		return o, tea.Batch(o.spin.Tick, o.c.fetchReleases(o.id, appPath))
	}
	o.stage, o.loading = stagePlan, true
	return o, tea.Batch(o.spin.Tick, o.c.fetchApprovalRows(o.id))
}

// approvalApp is the app an approval is about: the second word of its summary, "deploy api sha256:…".
func approvalApp(r approvalRow) string {
	words := strings.Fields(sanitize(r.Summary))
	if len(words) >= 2 {
		return words[1]
	}
	return ""
}

// startPlan asks the agent for the plan of a restart or a roll back.
func (o *actionOverlay) startPlan() tea.Cmd {
	o.stage, o.loading, o.form = stagePlan, true, nil
	body := map[string]any{"app": o.app, "request_id": o.dryID}
	path := "restart"
	if o.kind == actRestart && o.role != "" {
		body["role"] = o.role
	}
	if o.kind == actRollback {
		path = "rollback"
		body["target_release"] = o.release
	}
	return tea.Batch(o.spin.Tick, o.c.dryRun(o.id, path, body))
}

// title is the line on the top edge of the overlay.
func (o *actionOverlay) title() string {
	env := sanitize(o.env)
	switch o.kind {
	case actRestart:
		// Until the scope is known, an image app has none to name; only a Compose app restarts as a whole.
		scope := ""
		switch {
		case o.role != "":
			scope = " (" + sanitize(o.role) + ")"
		case o.roleLabel != "":
			scope = " (" + sanitize(o.roleLabel) + ")"
		case o.appKind == "compose":
			scope = " (all roles)"
		}
		return fmt.Sprintf("Restart %s%s on %s", sanitize(o.app), scope, env)
	case actRollback:
		if o.release == 0 {
			return fmt.Sprintf("Roll back %s on %s", sanitize(o.app), env)
		}
		return fmt.Sprintf("Roll back %s to release %d on %s", sanitize(o.app), o.release, env)
	case actApprove:
		return fmt.Sprintf("Approve %s requested by %s", approvalTitle(o.appr), sanitize(o.appr.Actor))
	}
	return fmt.Sprintf("Reject %s requested by %s", approvalTitle(o.appr), sanitize(o.appr.Actor))
}

// warns is true for an action that changes what runs, which is drawn in the colour of a warning.
func (o *actionOverlay) warns() bool {
	switch o.kind {
	case actRestart, actRollback:
		return true
	case actApprove:
		return o.appr.Kind != "deploy" && o.appr.Kind != "scale"
	}
	return false
}

func (o *actionOverlay) tone(st styles) lipgloss.Style {
	if o.warns() {
		return st.warn
	}
	return st.accent
}

// verb is the action in the present tense, "restart api".
func (o *actionOverlay) verb() string {
	switch o.kind {
	case actRestart:
		return "restart"
	case actRollback:
		return "roll back"
	case actApprove:
		return "approve"
	}
	return "reject"
}

// needsEnvName is true when the person must type the environment's name: a roll back or an approval on a protected environment.
func (o *actionOverlay) needsEnvName() bool {
	return o.protected && (o.kind == actRollback || o.kind == actApprove)
}

// The forms. Each is built with the Huh theme of the interface, and ends in a formDoneMsg.

func (o *actionOverlay) newForm(stage actionStage, fields ...huh.Field) tea.Cmd {
	f := huh.NewForm(huh.NewGroup(fields...)).WithTheme(huhTheme(o.st, o.warns())).WithShowHelp(false).WithShowErrors(true)
	id := o.id
	f.SubmitCmd = func() tea.Msg { return formDoneMsg{id: id, stage: stage} }
	f.CancelCmd = f.SubmitCmd
	o.form = f
	return f.Init()
}

func (o *actionOverlay) roleForm() tea.Cmd {
	opts := make([]huh.Option[string], len(o.roles))
	for i, r := range o.roles {
		opts[i] = huh.NewOption(sanitize(r), r)
	}
	o.role = o.roles[0]
	return o.newForm(stagePick, huh.NewSelect[string]().Key("role").Title("Restart which role?").
		Description("The agent restarts one role at a time.").Options(opts...).Height(min(len(opts), 8)+2).Value(&o.role))
}

// candidates are the earlier releases an app can be rolled back to: the successful ones of its history, newest first, without
// the one that runs now and without any that has the same image. current is the one that runs now, nil when there is none.
func candidates(rows []historyRow) (out []historyRow, current *historyRow) {
	for i, h := range rows {
		if h.ID == 0 || h.failed() {
			continue
		}
		if current == nil {
			current = &rows[i]
			continue
		}
		if h.Digest != current.Digest {
			out = append(out, h)
		}
	}
	return out, current
}

// releaseFact is a release for the facts: its number, short digest, commit and age.
func (o *actionOverlay) releaseFact(h historyRow) string {
	digest := h.Digest
	if digest != "" && !strings.Contains(digest, "@") {
		digest = "x@" + digest
	}
	return mdText(fmt.Sprintf("release %d %s %s %s commit %s %s %s", h.ID, glyphDot, tagged(h.Tag, shortImage(sanitize(digest))), glyphDot, h.commitText(12), glyphDot, ago(o.now(), h.Created)))
}

func (o *actionOverlay) releaseLabel(h historyRow) string {
	digest := h.Digest
	if digest != "" && !strings.Contains(digest, "@") {
		digest = "x@" + digest
	}
	return fmt.Sprintf("release %d  %s  commit %s  %s", h.ID, tagged(h.Tag, shortImage(sanitize(digest))), h.commitText(12), ago(o.now(), h.Created))
}

func (o *actionOverlay) releaseForm() tea.Cmd {
	opts := make([]huh.Option[int64], len(o.releases))
	for i, h := range o.releases {
		opts[i] = huh.NewOption(o.releaseLabel(h), h.ID)
	}
	o.release = o.releases[0].ID
	return o.newForm(stagePick, huh.NewSelect[int64]().Key("release").Title("Roll back to which release?").
		Description("The newest earlier release is chosen.").Options(opts...).Height(min(len(opts), 8)+2).Value(&o.release))
}

func (o *actionOverlay) confirmForm() tea.Cmd {
	o.typed, o.reason, o.sure = "", "", false
	var fields []huh.Field
	if o.needsEnvName() {
		env := o.env
		fields = append(fields, huh.NewInput().Key("env").Title("Type the environment name to confirm: "+sanitize(env)).
			Placeholder(sanitize(env)).CharLimit(64).Value(&o.typed).Validate(func(s string) error {
			if s != env {
				return errors.New("the name must match exactly")
			}
			return nil
		}))
	}
	if o.kind == actReject {
		fields = append(fields, huh.NewInput().Key("reason").Title("Reason (optional)").Placeholder("shown to the agent that asked").
			CharLimit(200).Value(&o.reason).Validate(func(s string) error {
			if s != "" && !agentclient.PrintableMessage(s) {
				return errors.New("the reason cannot have control characters")
			}
			return nil
		}))
	}
	affirm := map[actionKind]string{actRestart: "Restart", actRollback: "Roll back", actApprove: "Approve", actReject: "Reject"}[o.kind]
	fields = append(fields, huh.NewConfirm().Key("sure").Affirmative(affirm).Negative("Cancel").Value(&o.sure))
	return o.newForm(stageConfirm, fields...)
}

// Update handles a message for the overlay: keys, the answers of the agent and the animations.
func (o *actionOverlay) Update(msg tea.Msg) (overlay, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		return o.key(msg)
	case spinner.TickMsg:
		var c tea.Cmd
		o.spin, c = o.spin.Update(msg)
		if o.loading || o.stage == stageRun || o.stage == stageFollow || o.stage == stagePlan {
			return o, c
		}
		return o, nil
	case stopwatch.TickMsg, stopwatch.StartStopMsg, stopwatch.ResetMsg:
		var c tea.Cmd
		o.watch, c = o.watch.Update(msg)
		return o, c
	case timer.TickMsg, timer.TimeoutMsg, timer.StartStopMsg:
		var c tea.Cmd
		o.timer, c = o.timer.Update(msg)
		return o, c
	case planMsg:
		if msg.id == o.id && o.stage == stagePlan {
			return o, o.planned(msg.plan)
		}
		return o, nil
	case approvalRowsMsg:
		if msg.id == o.id && o.stage == stagePlan {
			return o, o.approvalPlanned(msg.rows)
		}
		return o, nil
	case releasesMsg:
		if msg.id == o.id && o.stage == stagePick {
			o.loading = false
			var cur *historyRow
			o.releases, cur = candidates(msg.rows)
			o.current = nil
			if cur != nil {
				c := *cur
				o.current = &c
			}
			if len(o.releases) == 0 {
				return o, o.finish(actionResult{kind: outRefused, title: "Nothing to roll back to", text: "There is no earlier successful release of " + sanitize(o.app) + " in its history."})
			}
			return o, o.releaseForm()
		}
		return o, nil
	case formDoneMsg:
		if msg.id == o.id && msg.stage == o.stage {
			return o.formDone()
		}
		return o, nil
	case sentMsg:
		if msg.id == o.id && o.stage == stageRun {
			return o, o.sent(msg)
		}
		return o, nil
	case actionErrMsg:
		if msg.id == o.id {
			return o, o.failure(msg)
		}
		return o, nil
	case opEventMsg:
		if msg.id == o.id && o.stage == stageFollow {
			o.events = append(o.events, msg.text)
			return o, waitFor(o.ch)
		}
		return o, nil
	case opDoneMsg:
		if msg.id == o.id && o.stage == stageFollow {
			return o, o.operationDone(msg)
		}
		return o, nil
	}
	if o.form != nil && o.stage != stageDone {
		_, c := o.form.Update(msg)
		return o, c
	}
	return o, nil
}

// waitFor waits for the next message of the operation that is followed.
func waitFor(ch <-chan tea.Msg) tea.Cmd {
	return func() tea.Msg {
		m, ok := <-ch
		if !ok {
			return nil
		}
		return m
	}
}

// close ends the overlay. What is followed goes on at the agent.
func (o *actionOverlay) close(note, openApp string) (overlay, tea.Cmd) {
	if o.cancel != nil {
		o.cancel()
	}
	if note == "" && openApp == "" {
		return nil, nil
	}
	return nil, func() tea.Msg { return actionCloseMsg{note: note, openApp: openApp} }
}

func (o *actionOverlay) key(msg tea.KeyPressMsg) (overlay, tea.Cmd) {
	s := msg.String()
	switch o.stage {
	case stageDone:
		switch {
		case s == "r" && o.result.retry:
			return o, o.retry()
		case s == "d" && o.result.diag:
			return o.close("", o.app)
		}
		return o.close("", "")
	case stageRun:
		return o, nil
	case stageFollow:
		if s == "esc" {
			return o.close(fmt.Sprintf("operation %s continues on the agent", shortID(o.opID)), "")
		}
		return o, nil
	}
	if s == "esc" {
		return o.close("", "")
	}
	if o.form == nil {
		return o, nil
	}
	if o.stage == stageConfirm {
		scrolls := s == "pgup" || s == "pgdown"
		if f := o.form.GetFocusedField(); f != nil && f.GetKey() == "sure" {
			scrolls = scrolls || s == "up" || s == "down" || s == "k" || s == "j"
		}
		if scrolls {
			o.plan, _ = o.plan.Update(msg)
			return o, nil
		}
	}
	_, c := o.form.Update(msg)
	return o, c
}

// shortID is the first 8 characters of an id, which is enough to tell operations apart in a note.
func shortID(id string) string {
	id = sanitize(id)
	return id[:min(len(id), 8)]
}

// retry tries again the step that failed with the agent out of reach, with the same request id.
func (o *actionOverlay) retry() tea.Cmd {
	o.result = actionResult{}
	switch o.failedStage {
	case stageRun:
		return o.run()
	case stagePick:
		o.stage, o.loading = stagePick, true
		return tea.Batch(o.spin.Tick, o.c.fetchReleases(o.id, o.appPath))
	}
	if o.kind == actApprove || o.kind == actReject {
		o.stage, o.loading = stagePlan, true
		return tea.Batch(o.spin.Tick, o.c.fetchApprovalRows(o.id))
	}
	return o.startPlan()
}

// scopeHint is what to tell a person whose token the agent refused.
func (o *actionOverlay) scopeHint(status int, text string) string {
	if status != 403 || !strings.Contains(text, "forbidden") {
		return ""
	}
	if o.kind == actApprove || o.kind == actReject {
		return "this token needs the admin scope"
	}
	return "this token needs the deploy scope"
}

// failure handles an answer that is not what was needed: a refusal, or an agent that cannot be reached.
func (o *actionOverlay) failure(e actionErrMsg) tea.Cmd {
	if e.text == "stopped" {
		return nil
	}
	text := sanitize(e.text)
	if e.lost {
		return o.finish(actionResult{kind: outLost, title: "Lost track of the operation", text: text,
			hint: "operation " + sanitize(o.opID) + " continues on the agent"})
	}
	r := actionResult{kind: outRefused, title: "The agent refused", text: text, hint: o.scopeHint(e.status, text)}
	if e.network {
		r = actionResult{kind: outRefused, title: "The agent cannot be reached", text: "Nothing was changed that the agent has not confirmed.", retry: true}
		if o.stage == stageRun {
			r.text = "The request may not have arrived. Press r to send it again with the same id."
		}
	}
	o.failedStage = o.stage
	return o.finish(r)
}

// finish ends the action with a result and tells the model to read everything again.
func (o *actionOverlay) finish(r actionResult) tea.Cmd {
	o.stage, o.loading, o.form, o.result = stageDone, false, nil, r
	return func() tea.Msg { return actionFinishedMsg{} }
}

// planned receives the plan of a restart or a roll back and goes to the confirmation.
func (o *actionOverlay) planned(raw json.RawMessage) tea.Cmd {
	raw = json.RawMessage(o.c.target.Redact(string(raw)))
	o.planRaw = raw
	o.loading = false
	facts := planFacts(raw)
	var extra []planFact
	switch o.kind {
	case actRestart:
		role := "all roles"
		switch {
		case o.role != "":
			role = mdText(o.role)
		case o.appKind != "compose" && len(planRoles(raw)) == 1:
			o.roleLabel = planRoles(raw)[0]
			role = mdText(o.roleLabel)
		}
		o.planHead = "Restart " + mdText(o.app) + " " + glyphDot + " " + role
		o.planEffect = "The replicas of the role are replaced one after the other, with the same image and configuration. The app keeps serving while they are."
		if o.appKind == "compose" {
			o.planEffect = "The containers of the Compose project are restarted. Nothing is recreated."
		}
	case actRollback:
		o.planHead = fmt.Sprintf("Roll back %s to release %d", mdText(o.app), o.release)
		if o.current != nil {
			extra = append(extra, planFact{label: "From", md: o.releaseFact(*o.current)})
		}
		for _, h := range o.releases {
			if h.ID == o.release {
				extra = append(extra, planFact{label: "To", md: o.releaseFact(h)})
			}
		}
		o.planEffect = "The app is deployed again with the image of that release, as a new deploy."
	}
	o.planFacts = insertFacts(facts, extra)
	o.stage = stageConfirm
	return o.confirmForm()
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// insertFacts puts extra facts after the app's.
func insertFacts(facts, extra []planFact) []planFact {
	at := 0
	if len(facts) > 0 && facts[0].label == "App" {
		at = 1
	}
	out := append([]planFact(nil), facts[:at]...)
	out = append(out, extra...)
	return append(out, facts[at:]...)
}

// approvalPlanned finds the approval in the answer of the agent and goes to the confirmation, unless it can no longer be decided.
func (o *actionOverlay) approvalPlanned(rows []approvalRow) tea.Cmd {
	o.loading = false
	var row *approvalRow
	for i := range rows {
		if rows[i].ID == o.appr.ID {
			row = &rows[i]
		}
	}
	short := sanitize(o.appr.ID[:min(len(o.appr.ID), 8)])
	switch {
	case row == nil:
		return o.finish(actionResult{kind: outRefused, title: "Not found", text: "Approval " + short + " is not there any more."})
	case row.Status != "pending":
		return o.finish(actionResult{kind: outRefused, title: "No longer pending", text: "Approval " + short + " was already " + sanitize(row.Status) + "."})
	case !o.now().Before(row.Expires):
		return o.finish(actionResult{kind: outRefused, title: "Expired", text: "Approval " + short + " expired " + ago(o.now(), row.Expires) + "."})
	}
	o.appr = *row
	o.planRaw = json.RawMessage(o.c.target.Redact(string(row.Plan)))
	o.planHead = mdText(approvalTitle(*row))
	o.planFacts = planFacts(o.planRaw)
	if len(bytesTrim(row.Plan)) == 0 {
		o.planNote = "*The plan is not shown to this token: it needs the deploy scope.*"
	}
	o.planEffect = approvalEffect(*row, o.kind == actReject)
	o.timer = timer.New(max(row.Expires.Sub(o.now()), 0), timer.WithInterval(time.Second))
	o.stage = stageConfirm
	return tea.Batch(o.confirmForm(), o.timer.Init())
}

func bytesTrim(b []byte) string {
	s := strings.TrimSpace(string(b))
	if s == "null" {
		return ""
	}
	return s
}

// approvalEffect says what approving does: the agent carries out the request as the agent that made it.
func approvalEffect(r approvalRow, reject bool) string {
	if reject {
		return "Rejecting refuses the request: nothing runs, and " + mdText(r.Actor) + " is told it was rejected."
	}
	if r.Kind == "" {
		return ""
	}
	return "Approving carries out the " + mdText(r.Kind) + " as " + mdText(r.Actor) + ", exactly as planned, and follows what the agent does with it."
}

// formDone handles the end of a form: the choice, or the answer to the confirmation.
func (o *actionOverlay) formDone() (overlay, tea.Cmd) {
	switch o.stage {
	case stagePick:
		return o, o.startPlan()
	case stageConfirm:
		if !o.sure || (o.needsEnvName() && o.typed != o.env) {
			return o.close("", "")
		}
		o.reqID, _ = agentclient.NewRequestID()
		return o, o.run()
	}
	return o, nil
}

// run sends the change the person confirmed.
func (o *actionOverlay) run() tea.Cmd {
	o.stage, o.loading, o.form = stageRun, false, nil
	var path string
	var body any
	switch o.kind {
	case actRestart:
		req := map[string]any{"app": o.app, "request_id": o.reqID, "dry_run": false}
		if o.role != "" {
			req["role"] = o.role
		}
		path, body = "restart", req
	case actRollback:
		path, body = "rollback", map[string]any{"app": o.app, "target_release": o.release, "request_id": o.reqID, "dry_run": false}
	case actApprove:
		path = "approvals/" + url.PathEscape(o.appr.ID) + "/approve"
	case actReject:
		path = "approvals/" + url.PathEscape(o.appr.ID) + "/reject"
		if o.reason != "" {
			body = map[string]string{"reason": o.reason}
		}
	}
	return tea.Batch(o.spin.Tick, o.c.send(o.id, path, body))
}

// sent handles the agent's answer to the change.
func (o *actionOverlay) sent(m sentMsg) tea.Cmd {
	switch {
	case m.held != "":
		text := ""
		if !m.expires.IsZero() {
			text = "It expires in " + until(o.now(), m.expires) + "."
		}
		return o.finish(actionResult{kind: outHeld, title: "Held for approval " + sanitize(m.held) + " " + glyphArrow + " a person must approve it", text: text,
			hint: "berthy approve " + sanitize(m.held) + " -e " + sanitize(o.env)})
	case m.done:
		switch o.kind {
		case actReject:
			text := ""
			if o.reason != "" {
				text = "Reason: " + sanitize(o.reason)
			}
			return o.finish(actionResult{kind: outOK, title: "Rejected " + approvalTitle(o.appr), text: text})
		case actApprove:
			return o.finish(actionResult{kind: outOK, title: "Approved " + approvalTitle(o.appr)})
		}
		return o.finish(actionResult{kind: outOK, title: "Done"})
	}
	o.opID = m.op
	o.stage = stageFollow
	o.watch = stopwatch.New(stopwatch.WithInterval(time.Second))
	ctx, cancel := context.WithCancel(o.c.ctx)
	o.cancel = cancel
	o.ch = make(chan tea.Msg, 64)
	ch, id, op, events := o.ch, o.id, m.op, m.events
	go func() {
		defer close(ch)
		o.c.followOperation(ctx, id, op, events, channelSender(ctx, ch))
	}()
	return tea.Batch(o.spin.Tick, o.watch.Start(), waitFor(ch))
}

// operationDone handles the end of the operation the action started.
func (o *actionOverlay) operationDone(m opDoneMsg) tea.Cmd {
	o.elapsed = o.watch.Elapsed()
	stop := o.watch.Stop()
	took := ""
	if o.elapsed >= time.Second {
		took = " in " + api.HumanAge(o.elapsed)
	}
	var r actionResult
	switch m.state {
	case "succeeded":
		r = actionResult{kind: outOK, title: o.doneVerb() + took}
	default:
		state := strings.ReplaceAll(m.state, "_", " ")
		if state == "" {
			state = "failed"
		}
		r = actionResult{kind: outFailed, title: o.failVerb() + " " + glyphDot + " " + state, text: m.reason, diag: o.app != "",
			hint: "berthy diagnostics " + sanitize(o.opID) + " -e " + sanitize(o.env)}
	}
	return tea.Batch(o.finish(r), stop)
}

// doneVerb is what was done, "Restarted api".
func (o *actionOverlay) doneVerb() string {
	app := sanitize(o.app)
	switch o.kind {
	case actRestart:
		return "Restarted " + app
	case actRollback:
		return fmt.Sprintf("Rolled back %s to release %d", app, o.release)
	}
	switch o.appr.Kind {
	case "deploy":
		return "Deployed " + app
	case "rollback":
		return "Rolled back " + app
	case "restart":
		return "Restarted " + app
	case "scale":
		return "Scaled " + app
	}
	return "Carried out " + approvalTitle(o.appr)
}

func (o *actionOverlay) failVerb() string {
	app := sanitize(o.app)
	switch o.kind {
	case actRestart:
		return "Restart of " + app + " did not succeed"
	case actRollback:
		return "Roll back of " + app + " did not succeed"
	}
	return approvalTitle(o.appr) + " did not succeed"
}

// View draws the overlay as a bordered pane for the middle of the body, w by h cells at most. The confirmation takes all the
// room it can, for the plan; the other stages are as tall as what they say.
func (o *actionOverlay) View(st styles, w, h int) string {
	o.st = st
	bw := min(max(w-4, 40), 100)
	maxH := min(max(h-2, 10), 30)
	tone := lipgloss.NewStyle().Foreground(o.tone(st).GetForeground())
	ps := st
	ps.paneBorder = tone
	ps.paneTitle = tone.Bold(true)
	right := ""
	var content string
	bh := maxH
	if o.stage == stageConfirm && o.form != nil {
		var inner int
		content, inner = o.confirmBody(st, bw-2, bh-2)
		bh = inner + 2
		if o.plan.TotalLineCount() > o.plan.Height() {
			right = st.faint.Render(fmt.Sprintf("%3.0f%%", o.plan.ScrollPercent()*100))
		}
	} else {
		lines := o.compactLines(st, bw-2, maxH-2)
		bh = min(len(lines)+2, maxH)
		content = strings.Join(lines, "\n")
	}
	return ps.pane(o.title(), right, content, bw, bh)
}

// compactLines is the content of the stages that are only as tall as what they say, with a blank line above and below.
func (o *actionOverlay) compactLines(st styles, iw, ih int) []string {
	var lines []string
	switch o.stage {
	case stagePick:
		if o.loading || o.form == nil {
			lines = []string{o.waitingLine(st, iw, "Reading the releases…")}
			break
		}
		o.form.WithWidth(iw - 2)
		lines = append(strings.Split(trimForm(o.form.View()), "\n"), "", o.helpLine(st, "↑↓ choose", "enter select", "esc cancel"))
	case stagePlan:
		lines = []string{o.waitingLine(st, iw, "Planning the "+o.verb()+"… a dry run, nothing changes")}
	case stageRun:
		lines = []string{o.waitingLine(st, iw, "Sending the "+o.verb()+" to the agent…")}
	default:
		lines = o.followLines(st, iw, ih-2)
	}
	return append(append([]string{""}, lines...), "")
}

// trimForm removes the empty lines a Huh form leaves below its fields.
func trimForm(view string) string { return strings.TrimRight(view, " \n") }

func (o *actionOverlay) waitingLine(st styles, iw int, text string) string {
	return lipgloss.PlaceHorizontal(iw, lipgloss.Center, o.spin.View()+st.muted.Render(" "+text))
}

// helpLine is the keys of the overlay, in the style of the footer.
func (o *actionOverlay) helpLine(st styles, hints ...string) string {
	parts := make([]string, len(hints))
	for i, h := range hints {
		k, d, _ := strings.Cut(h, " ")
		parts[i] = st.key.Render(k) + " " + st.muted.Render(d)
	}
	return " " + strings.Join(parts, st.faint.Render(" "+glyphDot+" "))
}

// meta is the fixed lines of an approval: who asked, and the time left to decide.
func (o *actionOverlay) meta(st styles) []string {
	if o.kind != actApprove && o.kind != actReject {
		return nil
	}
	r := o.appr
	now := o.now()
	lines := []string{" " + st.muted.Render("requested by ") + st.text.Render(sanitize(r.Actor)) + st.faint.Render(" "+glyphDot+" ") + st.muted.Render(ago(now, r.Created)) +
		st.faint.Render(" "+glyphDot+" ") + st.muted.Render(sanitize(r.Kind))}
	// Once the decision is sent there is nothing left to decide.
	if o.stage != stageConfirm && o.stage != stagePlan {
		return lines
	}
	left := o.timer.Timeout
	if o.timer.Timedout() || left <= 0 {
		lines = append(lines, " "+st.err.Render("expired"))
	} else {
		lines = append(lines, " "+st.warn.Render("⏳ "+countdown(left))+st.muted.Render(" left to decide")+st.faint.Render(" "+glyphDot+" expires "+r.Expires.Local().Format("15:04:05")))
	}
	return lines
}

// layoutPlan renders the Markdown of the plan for the width, when the width or the colours changed.
func (o *actionOverlay) layoutPlan(st styles, w int) {
	key := fmt.Sprintf("%d/%t", w, st.p.dark)
	if key == o.planKey {
		return
	}
	o.planKey = key
	md := planDocument(o.planHead, o.planFacts, o.planEffect, []byte(o.planRaw), o.planNote, w-2)
	lines := renderMarkdown(st, md, w-2)
	for i := range lines {
		lines[i] = " " + lines[i]
	}
	o.plan.SetContentLines(lines)
}

// confirmBody is the confirmation: the plan above the form. It is as tall as the plan needs, up to ih lines, and returns its height.
func (o *actionOverlay) confirmBody(st styles, iw, ih int) (string, int) {
	meta := o.meta(st)
	o.form.WithWidth(iw - 2)
	formLines := strings.Split(trimForm(o.form.View()), "\n")
	help := o.helpLine(st, "←→ choose", "enter confirm", "pgup/pgdn scroll the plan", "esc cancel")
	if o.needsEnvName() || o.kind == actReject {
		help = o.helpLine(st, "enter next", "pgup/pgdn scroll the plan", "esc cancel")
	}
	fixed := len(meta) + len(formLines) + 3 // a blank line above the form and above the help, and the help
	if len(meta) > 0 {
		fixed++ // the rule under them
	}
	o.plan.SetWidth(iw)
	o.layoutPlan(st, iw)
	vh := min(max(o.plan.TotalLineCount(), 1), max(ih-fixed, 1))
	o.plan.SetHeight(vh)
	var b []string
	b = append(b, meta...)
	if len(meta) > 0 {
		b = append(b, " "+st.faint.Render(strings.Repeat("─", max(iw-2, 0))))
	}
	b = append(b, o.plan.View(), "")
	b = append(b, formLines...)
	b = append(b, "", help)
	return strings.Join(b, "\n"), fixed + vh
}

// followLines is the events of the operation and, at the end, how it ended.
func (o *actionOverlay) followLines(st styles, iw, ih int) []string {
	var lines []string
	add := func(s string) { lines = append(lines, " "+s) }
	lines = append(lines, o.meta(st)...)
	if o.stage == stageFollow {
		add(o.spin.View() + st.muted.Render(" "+o.followVerb()+" · ") + st.text.Render(o.watch.View()))
		add(st.faint.Render("operation " + sanitize(o.opID)))
	}
	if len(lines) > 0 {
		lines = append(lines, "")
	}
	// The events that fit, the newest last.
	room := max(ih-len(lines)-8, 2)
	events := o.events
	if len(events) > room {
		events = events[len(events)-room:]
	}
	for i, e := range events {
		glyph := st.faint.Render(glyphDot)
		style := st.muted
		if i == len(events)-1 && o.stage == stageFollow {
			glyph, style = st.accent.Render("▸"), st.text
		}
		add(glyph + " " + style.Render(clip(sanitize(e), max(iw-6, 10))))
	}
	if len(events) > 0 {
		lines = append(lines, "")
	}
	if o.stage == stageDone {
		return append(lines, o.resultLines(st, iw)...)
	}
	return append(lines, o.helpLine(st, "esc close, the operation goes on"))
}

func (o *actionOverlay) followVerb() string {
	switch o.kind {
	case actRestart:
		return "Restarting"
	case actRollback:
		return "Rolling back"
	}
	return "Carrying it out"
}

// resultLines say how the action ended, with the keys that go on from there.
func (o *actionOverlay) resultLines(st styles, iw int) []string {
	r := o.result
	glyph, style := glyphOK, st.ok
	switch r.kind {
	case outHeld:
		glyph, style = "ℹ", st.info
	case outFailed, outRefused:
		glyph, style = glyphFail, st.err
	case outLost:
		glyph, style = glyphStarting, st.warn
	}
	var out []string
	for i, l := range wrapIndent(r.title, max(iw-6, 20), 0) {
		lead := "  "
		if i == 0 {
			lead = glyph + " "
		}
		out = append(out, " "+style.Bold(true).Render(lead+l))
	}
	if r.text != "" {
		for _, l := range wrapIndent(r.text, max(iw-6, 20), 0) {
			out = append(out, "   "+st.muted.Render(l))
		}
	}
	if r.hint != "" {
		out = append(out, "   "+st.faint.Render(clip(sanitize(r.hint), max(iw-6, 10))))
	}
	keys := []string{"any key close"}
	if r.retry {
		keys = append([]string{"r try again"}, keys...)
	}
	if r.diag {
		keys = append([]string{"d open " + sanitize(o.app)}, keys...)
	}
	return append(out, "", o.helpLine(st, keys...))
}

// planRoles are the roles a plan names.
func planRoles(raw []byte) []string {
	var p struct {
		Roles []string `json:"roles"`
	}
	_ = json.Unmarshal(raw, &p)
	return p.Roles
}
