package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"berthy/internal/manifest"
	"berthy/internal/state"
)

// Diff compares an app's proposed release with its applied release. It uses manifest settings, image references and Compose
// service decisions. Secret variables identify their sources by name.
type Diff struct {
	// New is true for the first deploy of an app: there is no release to compare with.
	New bool `json:"new,omitempty"`
	// Same reports whether the compact comparison found any changes.
	Same bool `json:"same,omitempty"`
	// Changes lists the differences in a fixed order, so two diffs of the same pair are equal.
	Changes []DiffChange `json:"changes,omitempty"`
	// Fields holds the structured comparison separately from the compact JSON.
	Fields []DiffField `json:"-"`
}

// DiffField is a setting change or planner note at an app-relative path.
type DiffField struct {
	Path   []string `json:"path"`
	Before *string  `json:"before,omitempty"`
	After  *string  `json:"after,omitempty"`
	Note   string   `json:"note,omitempty"`
}

// MaxDiffFieldPathDepth is the depth of a role's logging option, the longest field path.
const MaxDiffFieldPathDepth = 5

// DiffChange is one difference. Area is "image", "app", "roles", "deploy", "env", "hooks", "migrations", "ingress", "jobs",
// "backups", "probes", "compose" or the name of a role; Change says what differs, such as "replicas 2 -> 3".
type DiffChange struct {
	Area   string `json:"area"`
	Change string `json:"change"`
}

// ReleaseSide is what one release of an app runs. Manifest is the whole manifest of the release: only the part that belongs to
// the app is compared. Image is the pinned image reference of an image app, or the digest of the Compose file of a Compose app.
type ReleaseSide struct {
	Manifest manifest.Manifest
	Image    string
	Tag      string
}

const (
	noChangeLine = "no changes: the same release would be recorded"
	newAppLine   = "new app"
	minAreaWidth = 8
	// shortDigestLen is how many digits of a digest a line shows.
	shortDigestLen = 12
	// maxValueLen is how many characters of a plain value or a template a line shows.
	maxValueLen = 60
)

// ReleaseDiff compares what the release prev runs with what the release next would run, for app. prev is nil for a first
// deploy. Both manifests are compared after the defaults a stored manifest takes, so a release written before a default existed
// does not differ from one that has it.
func ReleaseDiff(app string, prev, next *ReleaseSide) Diff {
	if prev == nil {
		return Diff{New: true}
	}
	pm, nm := prev.Manifest.Defaulted().AppScope(app), next.Manifest.Defaulted().AppScope(app)
	pa, na := pm.Apps[app], nm.Apps[app]
	var d differ
	d.image(pa, na, prev, next)
	d.app(pa, na)
	d.roles(pa, na)
	d.deploy(pa.Deploy, na.Deploy)
	d.env(pa.Env, na.Env)
	d.hooks("pre_deploy", pa.Hooks.PreDeploy, na.Hooks.PreDeploy)
	d.hooks("post_deploy", pa.Hooks.PostDeploy, na.Hooks.PostDeploy)
	d.migrations(pa.Migrations, na.Migrations)
	d.ingress(pa, na)
	d.jobs(pm.Jobs, nm.Jobs)
	d.backups(pm.Backups, nm.Backups)
	d.probes(pm.Probes, nm.Probes)
	return Diff{Same: len(d.changes) == 0, Changes: d.changes, Fields: d.fields}
}

// WithCompose adds the services that the Compose plan creates, recreates or removes. A service it keeps is not a difference. The
// diff of a new app stays as it is: the plan lists every service it creates.
func (d Diff) WithCompose(plan ComposePlan) Diff {
	if d.New {
		return d
	}
	verbs := map[string]string{"create": "create", "recreate": "recreate", "remove": "remove"}
	for _, s := range plan.Services {
		if verb, ok := verbs[s.Action]; ok {
			note := verb + " " + s.Name
			d.Changes = append(d.Changes, DiffChange{Area: "compose", Change: note})
			d.Fields = append(d.Fields, DiffField{Path: []string{"compose", s.Name}, Note: note})
		}
	}
	d.Same = len(d.Changes) == 0
	return d
}

// Lines is the diff as text, one line per change with the areas in a column, as `berthy deploy --dry-run` prints it.
func (d Diff) Lines() []string {
	switch {
	case d.New:
		return []string{newAppLine}
	case len(d.Changes) == 0:
		return []string{noChangeLine}
	}
	width := minAreaWidth
	for _, c := range d.Changes {
		width = max(width, len([]rune(c.Area)))
	}
	out := make([]string, len(d.Changes))
	for i, c := range d.Changes {
		out[i] = fmt.Sprintf("%-*s %s", width, c.Area, c.Change)
	}
	return out
}

// DiffReleases compares release id of app with release base, or with the release before it when base is 0: the diff says what
// release id changes compared with base. Both must be releases of the app. The first release of an app has none before it, so
// its diff is a new app.
func (c *Controller) DiffReleases(ctx context.Context, app string, id, base int64) (Diff, error) {
	if c.Store == nil {
		return Diff{}, errors.New("release history unavailable")
	}
	history, err := c.Store.History(ctx, app)
	if err != nil {
		return Diff{}, errors.New("release history unavailable")
	}
	at := slices.IndexFunc(history, func(r state.Release) bool { return r.ID == id })
	if at < 0 {
		return Diff{}, notARelease(id, app)
	}
	var prev *state.Release
	switch {
	case base != 0:
		i := slices.IndexFunc(history, func(r state.Release) bool { return r.ID == base })
		if i < 0 {
			return Diff{}, notARelease(base, app)
		}
		prev = &history[i]
	default:
		var previousID int64
		if at+1 < len(history) {
			prev = &history[at+1]
			previousID = prev.ID
		}
		retiredID, err := c.Store.LatestRetiredReleaseBefore(ctx, app, id)
		if err != nil {
			return Diff{}, errors.New("release history unavailable")
		}
		if retiredID > previousID {
			return Diff{}, Refusal("previous release is outside retained history")
		}
	}
	next, err := releaseSide(history[at])
	if err != nil {
		return Diff{}, err
	}
	if prev == nil {
		return ReleaseDiff(app, nil, next), nil
	}
	before, err := releaseSide(*prev)
	if err != nil {
		return Diff{}, err
	}
	return ReleaseDiff(app, before, next), nil
}

func notARelease(id int64, app string) Refusal {
	return Refusal(fmt.Sprintf("release %d is not a release of app %s", id, app))
}

// releaseSide is what a stored release runs.
func releaseSide(r state.Release) (*ReleaseSide, error) {
	var m manifest.Manifest
	if json.Unmarshal(r.Manifest, &m) != nil {
		return nil, Refusal("saved release invalid")
	}
	return &ReleaseSide{Manifest: m, Image: r.Digest, Tag: r.Tag}, nil
}

// differ collects the changes of one comparison in the order the areas are compared.
type differ struct {
	changes []DiffChange
	fields  []DiffField
}

func (d *differ) add(area, format string, args ...any) {
	d.changes = append(d.changes, DiffChange{Area: area, Change: fmt.Sprintf(format, args...)})
}

func (d *differ) row(path []string, before, after *string) {
	d.fields = append(d.fields, DiffField{Path: path, Before: before, After: after})
}

func diffSide(value string, present bool) *string {
	if !present {
		return nil
	}
	return &value
}

// setting compares scalar settings and JSON lists. For optional settings, missing is the compact label for an absent value.
func (d *differ) setting(area, name, prev, next, missing string, path ...string) {
	if prev != next {
		if len(path) == 0 {
			path = []string{area, name}
		}
		d.row(path, diffSide(prev, missing == "" || prev != ""), diffSide(next, missing == "" || next != ""))
	}
	if a, b := orNone(prev, missing), orNone(next, missing); a != b {
		d.add(area, "%s %s -> %s", name, a, b)
	}
}

// image notes the image the release runs: its digest and tag, and for a Compose app the digest of its file.
func (d *differ) image(pa, na manifest.App, prev, next *ReleaseSide) {
	if na.Kind == "compose" || pa.Kind == "compose" {
		if pd, nd := pinnedDigest(prev.Image), pinnedDigest(next.Image); pd != nd && nd != "" {
			d.add("compose", "file %s -> %s", shortDigest(pd), shortDigest(nd))
			d.row([]string{"compose", "file"}, diffSide(pd, pd != ""), &nd)
		}
		return
	}
	pd, nd := pinnedDigest(prev.Image), pinnedDigest(next.Image)
	if pd != nd {
		d.row([]string{"image", "digest"}, diffSide(pd, pd != ""), diffSide(nd, nd != ""))
	}
	if prev.Tag != next.Tag {
		d.row([]string{"image", "tag"}, diffSide(prev.Tag, prev.Tag != ""), diffSide(next.Tag, next.Tag != ""))
	}
	switch {
	case pd != nd:
		line := orNone(shortDigest(pd), "none") + " -> " + orNone(shortDigest(nd), "none")
		switch {
		case prev.Tag == next.Tag && next.Tag != "":
			line += "   (" + next.Tag + ")"
		case prev.Tag != next.Tag:
			line += "   (" + orNone(prev.Tag, "no tag") + " -> " + orNone(next.Tag, "no tag") + ")"
		}
		d.add("image", "%s", line)
	case prev.Tag != next.Tag:
		d.add("image", "tag %s -> %s", orNone(prev.Tag, "none"), orNone(next.Tag, "none"))
	}
	if pa.Image != na.Image {
		d.add("image", "repository %s -> %s", orNone(pa.Image, "none"), orNone(na.Image, "none"))
		d.row([]string{"image", "repository"}, diffSide(pa.Image, pa.Image != ""), diffSide(na.Image, na.Image != ""))
	}
}

// app notes the settings of the app itself.
func (d *differ) app(pa, na manifest.App) {
	d.setting("app", "kind", pa.Kind, na.Kind, "none")
	d.setting("app", "stateful", strconv.FormatBool(pa.Stateful), strconv.FormatBool(na.Stateful), "")
	d.setting("app", "file", pa.File, na.File, "none")
	d.setting("app", "project_name", pa.ProjectName, na.ProjectName, "none")
	d.setting("app", "depends_on", list(pa.DependsOn), list(na.DependsOn), "none")
}

// roles notes the roles that are added or removed, then the settings of each role that both releases have. A role's hostnames
// are noted by ingress.
func (d *differ) roles(pa, na manifest.App) {
	for _, name := range slices.Sorted(maps.Keys(na.Roles)) {
		if _, ok := pa.Roles[name]; !ok {
			d.add("roles", "+ %s", name)
			d.row([]string{"roles", name}, nil, &name)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(pa.Roles)) {
		if _, ok := na.Roles[name]; !ok {
			d.add("roles", "- %s", name)
			d.row([]string{"roles", name}, &name, nil)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(na.Roles)) {
		if p, ok := pa.Roles[name]; ok {
			d.role(name, p, na.Roles[name])
		}
	}
}

func (d *differ) role(name string, p, n manifest.Role) {
	setting := func(key, before, after string) {
		path := append([]string{"roles", name}, strings.Split(key, ".")...)
		d.setting(name, key, before, after, "none", path...)
	}
	setting("service", p.Service, n.Service)
	setting("command", list(p.Command), list(n.Command))
	setting("entrypoint", list(p.Entrypoint), list(n.Entrypoint))
	setting("user", p.User, n.User)
	setting("replicas", intOf(p.Replicas), intOf(n.Replicas))
	setting("port", intOrEmpty(p.Port), intOrEmpty(n.Port))
	switch {
	case p.Health == nil && n.Health != nil:
		d.add(name, "+ health")
		d.row([]string{"roles", name, "health"}, nil, diffSide("configured", true))
	case p.Health != nil && n.Health == nil:
		d.add(name, "- health")
		d.row([]string{"roles", name, "health"}, diffSide("configured", true), nil)
	case p.Health != nil:
		setting("health.test", list(p.Health.Test), list(n.Health.Test))
		setting("health.interval", p.Health.Interval, n.Health.Interval)
		setting("health.timeout", p.Health.Timeout, n.Health.Timeout)
		setting("health.start_period", p.Health.StartPeriod, n.Health.StartPeriod)
		setting("health.start_interval", p.Health.StartInterval, n.Health.StartInterval)
		setting("health.retries", intOf(p.Health.Retries), intOf(n.Health.Retries))
	}
	before, after := uptimeDiffValue(p.Health), uptimeDiffValue(n.Health)
	setting("health.uptime", before, after)
	switch {
	case before == "" && after != "":
		d.add(name, "+ read-only probe bind %s", manifest.UptimeProbeTarget)
	case before != "" && after == "":
		d.add(name, "- read-only probe bind %s", manifest.UptimeProbeTarget)
	}
	for _, v := range added(p.Volumes, n.Volumes) {
		d.add(name, "+ volume %s", v)
		d.row([]string{"roles", name, "volumes"}, nil, &v)
	}
	for _, v := range added(n.Volumes, p.Volumes) {
		d.add(name, "- volume %s", v)
		d.row([]string{"roles", name, "volumes"}, &v, nil)
	}
	var pr, nr manifest.Resources
	if p.Resources != nil {
		pr = *p.Resources
	}
	if n.Resources != nil {
		nr = *n.Resources
	}
	setting("resources.cpu", pr.CPU, nr.CPU)
	setting("resources.memory", pr.Memory, nr.Memory)
	var pl, nl manifest.Logging
	if p.Logging != nil {
		pl = *p.Logging
	}
	if n.Logging != nil {
		nl = *n.Logging
	}
	setting("logging.driver", pl.Driver, nl.Driver)
	for _, k := range union(pl.Options, nl.Options) {
		before, had := pl.Options[k]
		after, has := nl.Options[k]
		if had != has || before != after {
			d.row([]string{"roles", name, "logging", "options", k}, diffSide(before, had), diffSide(after, has))
		}
		if a, b := orNone(before, "none"), orNone(after, "none"); a != b {
			d.add(name, "logging.options.%s %s -> %s", k, a, b)
		}
	}
}

func uptimeDiffValue(h *manifest.Health) string {
	if h == nil || h.Uptime == nil {
		return ""
	}
	if timing, err := h.UptimeTiming(); err == nil {
		return timing.Threshold.String()
	}
	return *h.Uptime
}

// deploy notes the app's rollout settings.
func (d *differ) deploy(p, n manifest.Deploy) {
	d.setting("deploy", "deploy_timeout", p.DeployTimeout, n.DeployTimeout, "none")
	d.setting("deploy", "drain_timeout", p.DrainTimeout, n.DrainTimeout, "none")
	d.setting("deploy", "retain", intOf(p.Retain), intOf(n.Retain), "none")
	d.setting("deploy", "start_attempts", intOf(p.StartAttempts), intOf(n.StartAttempts), "none")
	d.setting("deploy", "order", orNone(p.Order, "auto"), orNone(n.Order, "auto"), "")
	d.setting("deploy", "verify", strconv.FormatBool(p.Verifies()), strconv.FormatBool(n.Verifies()), "")
}

// envValue is where one variable of an app takes its value from: the value itself when it is plain, the name of the secret, or
// the text of a template, which refers to secrets by name.
type envValue struct{ kind, value string }

func envValues(e manifest.Env) map[string]envValue {
	out := map[string]envValue{}
	for k, v := range e.Plain {
		out[k] = envValue{"plain", v}
	}
	for _, s := range e.Secret {
		out[s.Var] = envValue{"secret", s.Secret}
	}
	for k, v := range e.Template {
		out[k] = envValue{"template", v}
	}
	return out
}

// env notes the variables that are added, removed or taken from somewhere else. A secret shows by its name and the name of its
// secret; a plain value shows only when it changes.
func (d *differ) env(p, n manifest.Env) {
	pv, nv := envValues(p), envValues(n)
	for _, name := range union(pv, nv) {
		a, hadA := pv[name]
		b, hasB := nv[name]
		if hadA != hasB || a != b {
			kindChanged := hadA && hasB && a.kind != b.kind
			d.row([]string{"env", name}, diffSide(a.fieldValue(kindChanged), hadA), diffSide(b.fieldValue(kindChanged), hasB))
		}
		switch {
		case !hadA:
			d.add("env", "+ %s (%s)", name, b.kind)
		case !hasB:
			d.add("env", "- %s (%s)", name, a.kind)
		case a == b:
		case a.kind != b.kind:
			d.add("env", "~ %s now %s", name, b.source())
		case b.kind == "secret":
			d.add("env", "~ %s now %s", name, b.source())
		default:
			d.add("env", "~ %s (%s) %s -> %s", name, b.kind, quote(a.value), quote(b.value))
		}
	}
}

func (v envValue) fieldValue(showKind bool) string {
	if v.kind == "secret" {
		return v.source()
	}
	if showKind {
		return v.kind + " " + v.value
	}
	return v.value
}

// source says where a variable takes its value from, for a variable that is now taken from somewhere else.
func (v envValue) source() string {
	switch v.kind {
	case "secret":
		return "from secret " + v.value
	case "template":
		return "a template " + quote(v.value)
	}
	return "plain " + quote(v.value)
}

// hooks notes the hooks that are added or removed, or put in another order.
func (d *differ) hooks(when string, p, n []manifest.Hook) {
	pt, nt := hookTexts(p), hookTexts(n)
	gone, fresh := added(nt, pt), added(pt, nt)
	for _, h := range fresh {
		d.add("hooks", "+ %s: %s", when, h)
	}
	for _, h := range gone {
		d.add("hooks", "- %s: %s", when, h)
	}
	if len(gone) == 0 && len(fresh) == 0 && !slices.Equal(pt, nt) {
		d.add("hooks", "~ %s: order changed", when)
	}
	// The compact sentence can collapse distinct argument lists into the same text.
	for len(p) > 0 && len(n) > 0 && equalHooks(p[0], n[0]) {
		p, n = p[1:], n[1:]
	}
	for len(p) > 0 && len(n) > 0 && equalHooks(p[len(p)-1], n[len(n)-1]) {
		p, n = p[:len(p)-1], n[:len(n)-1]
	}
	for i := range max(len(p), len(n)) {
		if i < len(p) && i < len(n) && equalHooks(p[i], n[i]) {
			continue
		}
		d.row([]string{"hooks", when}, hookFieldSide(p, i), hookFieldSide(n, i))
	}
}

func equalHooks(a, b manifest.Hook) bool {
	return a.Backup == b.Backup && slices.Equal(a.Run, b.Run) &&
		a.Timeout == b.Timeout && intOf(a.Retries) == intOf(b.Retries)
}

func hookFieldSide(hooks []manifest.Hook, i int) *string {
	if i >= len(hooks) {
		return nil
	}
	h := hooks[i]
	text := hookText(h, jsonText(h.Run))
	return &text
}

// hookTexts describes each hook, with its time limit and retries when they are not the defaults.
func hookTexts(hooks []manifest.Hook) []string {
	out := make([]string, len(hooks))
	for i, h := range hooks {
		out[i] = hookText(h, strings.Join(h.Run, " "))
	}
	return out
}

func hookText(h manifest.Hook, command string) string {
	if h.Backup != "" {
		return "backup " + h.Backup
	}
	text := "run " + command
	var extra []string
	if h.Timeout != "" && h.Timeout != "5m" {
		extra = append(extra, "timeout "+h.Timeout)
	}
	if h.Retries != nil && *h.Retries != 0 {
		extra = append(extra, "retries "+strconv.Itoa(*h.Retries))
	}
	if len(extra) > 0 {
		text += " (" + strings.Join(extra, ", ") + ")"
	}
	return text
}

// migrations notes the migration commands that are added, removed or changed.
func (d *differ) migrations(p, n *manifest.Migrations) {
	switch {
	case p == nil && n == nil:
	case p == nil:
		d.add("migrations", "+ migrations")
		d.row([]string{"migrations"}, nil, diffSide("configured", true))
	case n == nil:
		d.add("migrations", "- migrations")
		d.row([]string{"migrations"}, diffSide("configured", true), nil)
	default:
		d.setting("migrations", "version", list(p.Version), list(n.Version), "none")
		d.setting("migrations", "apply", list(p.Apply), list(n.Apply), "none")
		d.setting("migrations", "revert", list(p.Revert), list(n.Revert), "none")
		d.setting("migrations", "timeout", p.Timeout, n.Timeout, "none")
		d.setting("migrations", "retries", intOf(p.Retries), intOf(n.Retries), "none")
	}
}

// ingress notes the hostnames that are added, removed or moved to another role.
func (d *differ) ingress(p, n manifest.App) {
	ph, nh := hostRoles(p), hostRoles(n)
	for _, host := range union(ph, nh) {
		pr, hadP := ph[host]
		nr, hasN := nh[host]
		if hadP != hasN || pr != nr {
			d.row([]string{"ingress", host}, diffSide(pr, hadP), diffSide(nr, hasN))
		}
		switch {
		case !hadP:
			d.add("ingress", "+ %s", host)
		case !hasN:
			d.add("ingress", "- %s", host)
		case pr != nr:
			d.add("ingress", "~ %s role %s -> %s", host, pr, nr)
		}
	}
}

// hostRoles maps each public hostname of the app to the role that serves it.
func hostRoles(a manifest.App) map[string]string {
	out := map[string]string{}
	for role, r := range a.Roles {
		for _, host := range r.Hostnames {
			out[host] = role
		}
	}
	return out
}

func (d *differ) jobs(p, n map[string]manifest.Job) {
	for _, name := range union(p, n) {
		a, hadA := p[name]
		b, hasB := n[name]
		switch {
		case !hadA:
			d.add("jobs", "+ %s", name)
			d.row([]string{"jobs", name}, nil, &name)
		case !hasB:
			d.add("jobs", "- %s", name)
			d.row([]string{"jobs", name}, &name, nil)
		default:
			d.fieldOf("jobs", name, "role", a.Role, b.Role, quote)
			d.fieldOf("jobs", name, "service", a.Service, b.Service, quote)
			d.fieldOf("jobs", name, "schedule", a.Schedule, b.Schedule, quote)
			d.fieldOf("jobs", name, "command", list(a.Command), list(b.Command), compactList)
			d.fieldOf("jobs", name, "timeout", a.Timeout, b.Timeout, quote)
		}
	}
}

func (d *differ) backups(p, n map[string]manifest.Backup) {
	for _, name := range union(p, n) {
		a, hadA := p[name]
		b, hasB := n[name]
		switch {
		case !hadA:
			d.add("backups", "+ %s", name)
			d.row([]string{"backups", name}, nil, &name)
		case !hasB:
			d.add("backups", "- %s", name)
			d.row([]string{"backups", name}, &name, nil)
		default:
			d.fieldOf("backups", name, "volume", a.Volume, b.Volume, quote)
			d.fieldOf("backups", name, "command", list(a.Command), list(b.Command), compactList)
			d.fieldOf("backups", name, "role", a.Role, b.Role, quote)
			d.fieldOf("backups", name, "service", a.Service, b.Service, quote)
			d.fieldOf("backups", name, "timeout", a.Timeout, b.Timeout, quote)
			d.fieldOf("backups", name, "schedule", a.Schedule, b.Schedule, quote)
			d.fieldOf("backups", name, "destination", a.Destination, b.Destination, quote)
			d.fieldOf("backups", name, "prefix", a.Prefix, b.Prefix, quote)
			d.fieldOf("backups", name, "before", a.Before, b.Before, quote)
			prev, next := jsonText(a.Retention), jsonText(b.Retention)
			if prev != next {
				d.add("backups", "~ %s retention %s -> %s", name, prev, next)
				d.row([]string{"backups", name, "retention"}, diffSide(prev, a.Retention != nil), diffSide(next, b.Retention != nil))
			}
		}
	}
}

// probes notes the service probes aimed at the app. A probe that changed is named, not described.
func (d *differ) probes(p, n map[string]manifest.Probe) {
	for _, name := range union(p, n) {
		a, hadA := p[name]
		b, hasB := n[name]
		switch {
		case !hadA:
			d.add("probes", "+ %s", name)
			d.row([]string{"probes", name}, nil, &name)
		case !hasB:
			d.add("probes", "- %s", name)
			d.row([]string{"probes", name}, &name, nil)
		case jsonText(a) != jsonText(b):
			d.add("probes", "~ %s changed", name)
			d.fields = append(d.fields, DiffField{Path: []string{"probes", name}, Note: "changed"})
		}
	}
}

// fieldOf compares a setting of a named job or backup before formatting its compact values.
func (d *differ) fieldOf(area, name, setting, prev, next string, compact func(string) string) {
	if prev != next {
		d.row([]string{area, name, setting}, diffSide(prev, prev != ""), diffSide(next, next != ""))
		if a, b := compact(prev), compact(next); a != b {
			d.add(area, "~ %s %s %s -> %s", name, setting, a, b)
		}
	}
}

// union lists the keys of both maps, sorted and without duplicates.
func union[V any](a, b map[string]V) []string {
	keys := map[string]bool{}
	for k := range a {
		keys[k] = true
	}
	for k := range b {
		keys[k] = true
	}
	return slices.Sorted(maps.Keys(keys))
}

// added lists the values of next that prev lacks, in the order of next.
func added(prev, next []string) []string {
	var out []string
	for _, v := range next {
		if !slices.Contains(prev, v) && !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	return out
}

// pinnedDigest is the digest of an image reference, or the reference itself when it has none.
func pinnedDigest(image string) string {
	if _, d, ok := strings.Cut(image, "@"); ok {
		return d
	}
	return image
}

// shortDigest cuts a digest to its first digits, as in sha256:41ab1c2d3e4f….
func shortDigest(d string) string {
	algo, hex, ok := strings.Cut(d, ":")
	if !ok || len(hex) <= shortDigestLen {
		return d
	}
	return algo + ":" + hex[:shortDigestLen] + "…"
}

func orNone(s, none string) string {
	if s == "" {
		return none
	}
	return s
}

func intOf(n *int) string {
	if n == nil {
		return ""
	}
	return strconv.Itoa(*n)
}

func intOrEmpty(n int) string {
	if n == 0 {
		return ""
	}
	return strconv.Itoa(n)
}

// list writes a nonempty list of words as JSON, such as ["serve","--fast"].
func list(s []string) string {
	if len(s) == 0 {
		return ""
	}
	return jsonText(s)
}

func compactList(s string) string {
	return orNone(s, "none")
}

func jsonText(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "?"
	}
	return string(b)
}

// quote writes a value in quotes and cuts a long one, so a line stays a line.
func quote(s string) string {
	r := []rune(s)
	if len(r) > maxValueLen {
		return strconv.Quote(string(r[:maxValueLen])) + "…"
	}
	return strconv.Quote(s)
}
