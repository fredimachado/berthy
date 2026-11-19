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

// Diff is what a deploy of an app changes compared with the release the app runs now. It is built from two manifests and two
// image references alone, so it holds names and settings, never the value of a secret: a secret variable shows by its name
// and the name of its secret.
type Diff struct {
	// New is true for the first deploy of an app: there is no release to compare with.
	New bool `json:"new,omitempty"`
	// Same is true when nothing differs: the release that would be recorded is the one the app runs.
	Same bool `json:"same,omitempty"`
	// Changes lists the differences in a fixed order, so two diffs of the same pair are equal.
	Changes []DiffChange `json:"changes,omitempty"`
}

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
	return Diff{Same: len(d.changes) == 0, Changes: d.changes}
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
			d.Changes = append(d.Changes, DiffChange{Area: "compose", Change: verb + " " + s.Name})
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
	case at+1 < len(history):
		prev = &history[at+1]
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
type differ struct{ changes []DiffChange }

func (d *differ) add(area, format string, args ...any) {
	d.changes = append(d.changes, DiffChange{Area: area, Change: fmt.Sprintf(format, args...)})
}

// field notes a setting whose text differs.
func (d *differ) field(area, name, prev, next string) {
	if prev != next {
		d.add(area, "%s %s -> %s", name, prev, next)
	}
}

// image notes the image the release runs: its digest and tag, and for a Compose app the digest of its file.
func (d *differ) image(pa, na manifest.App, prev, next *ReleaseSide) {
	if na.Kind == "compose" || pa.Kind == "compose" {
		if pd, nd := pinnedDigest(prev.Image), pinnedDigest(next.Image); pd != nd && nd != "" {
			d.add("compose", "file %s -> %s", shortDigest(pd), shortDigest(nd))
		}
		return
	}
	pd, nd := pinnedDigest(prev.Image), pinnedDigest(next.Image)
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
	}
}

// app notes the settings of the app itself.
func (d *differ) app(pa, na manifest.App) {
	d.field("app", "kind", orNone(pa.Kind, "none"), orNone(na.Kind, "none"))
	d.field("app", "stateful", strconv.FormatBool(pa.Stateful), strconv.FormatBool(na.Stateful))
	d.field("app", "file", orNone(pa.File, "none"), orNone(na.File, "none"))
	d.field("app", "project_name", orNone(pa.ProjectName, "none"), orNone(na.ProjectName, "none"))
	d.field("app", "depends_on", list(pa.DependsOn), list(na.DependsOn))
}

// roles notes the roles that are added or removed, then the settings of each role that both releases have. A role's hostnames
// are noted by ingress.
func (d *differ) roles(pa, na manifest.App) {
	for _, name := range slices.Sorted(maps.Keys(na.Roles)) {
		if _, ok := pa.Roles[name]; !ok {
			d.add("roles", "+ %s", name)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(pa.Roles)) {
		if _, ok := na.Roles[name]; !ok {
			d.add("roles", "- %s", name)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(na.Roles)) {
		if p, ok := pa.Roles[name]; ok {
			d.role(name, p, na.Roles[name])
		}
	}
}

func (d *differ) role(name string, p, n manifest.Role) {
	d.field(name, "service", orNone(p.Service, "none"), orNone(n.Service, "none"))
	d.field(name, "command", list(p.Command), list(n.Command))
	d.field(name, "entrypoint", list(p.Entrypoint), list(n.Entrypoint))
	d.field(name, "user", orNone(p.User, "none"), orNone(n.User, "none"))
	d.field(name, "replicas", intOf(p.Replicas), intOf(n.Replicas))
	d.field(name, "port", intOrNone(p.Port), intOrNone(n.Port))
	switch {
	case p.Health == nil && n.Health != nil:
		d.add(name, "+ health")
	case p.Health != nil && n.Health == nil:
		d.add(name, "- health")
	case p.Health != nil:
		d.field(name, "health.test", list(p.Health.Test), list(n.Health.Test))
		d.field(name, "health.interval", orNone(p.Health.Interval, "none"), orNone(n.Health.Interval, "none"))
		d.field(name, "health.timeout", orNone(p.Health.Timeout, "none"), orNone(n.Health.Timeout, "none"))
		d.field(name, "health.start_period", orNone(p.Health.StartPeriod, "none"), orNone(n.Health.StartPeriod, "none"))
		d.field(name, "health.start_interval", orNone(p.Health.StartInterval, "none"), orNone(n.Health.StartInterval, "none"))
		d.field(name, "health.retries", intOf(p.Health.Retries), intOf(n.Health.Retries))
	}
	for _, v := range added(p.Volumes, n.Volumes) {
		d.add(name, "+ volume %s", v)
	}
	for _, v := range added(n.Volumes, p.Volumes) {
		d.add(name, "- volume %s", v)
	}
	var pr, nr manifest.Resources
	if p.Resources != nil {
		pr = *p.Resources
	}
	if n.Resources != nil {
		nr = *n.Resources
	}
	d.field(name, "resources.cpu", orNone(pr.CPU, "none"), orNone(nr.CPU, "none"))
	d.field(name, "resources.memory", orNone(pr.Memory, "none"), orNone(nr.Memory, "none"))
	var pl, nl manifest.Logging
	if p.Logging != nil {
		pl = *p.Logging
	}
	if n.Logging != nil {
		nl = *n.Logging
	}
	d.field(name, "logging.driver", orNone(pl.Driver, "none"), orNone(nl.Driver, "none"))
	for _, k := range union(pl.Options, nl.Options) {
		d.field(name, "logging.options."+k, orNone(pl.Options[k], "none"), orNone(nl.Options[k], "none"))
	}
}

// deploy notes the app's rollout settings.
func (d *differ) deploy(p, n manifest.Deploy) {
	d.field("deploy", "deploy_timeout", orNone(p.DeployTimeout, "none"), orNone(n.DeployTimeout, "none"))
	d.field("deploy", "drain_timeout", orNone(p.DrainTimeout, "none"), orNone(n.DrainTimeout, "none"))
	d.field("deploy", "retain", intOf(p.Retain), intOf(n.Retain))
	d.field("deploy", "start_attempts", intOf(p.StartAttempts), intOf(n.StartAttempts))
	d.field("deploy", "order", orNone(p.Order, "auto"), orNone(n.Order, "auto"))
	d.field("deploy", "verify", strconv.FormatBool(p.Verifies()), strconv.FormatBool(n.Verifies()))
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
}

// hookTexts describes each hook, with its time limit and retries when they are not the defaults.
func hookTexts(hooks []manifest.Hook) []string {
	out := make([]string, len(hooks))
	for i, h := range hooks {
		if h.Backup != "" {
			out[i] = "backup " + h.Backup
			continue
		}
		out[i] = "run " + strings.Join(h.Run, " ")
		var extra []string
		if h.Timeout != "" && h.Timeout != "5m" {
			extra = append(extra, "timeout "+h.Timeout)
		}
		if h.Retries != nil && *h.Retries != 0 {
			extra = append(extra, "retries "+strconv.Itoa(*h.Retries))
		}
		if len(extra) > 0 {
			out[i] += " (" + strings.Join(extra, ", ") + ")"
		}
	}
	return out
}

// migrations notes the migration commands that are added, removed or changed.
func (d *differ) migrations(p, n *manifest.Migrations) {
	switch {
	case p == nil && n == nil:
	case p == nil:
		d.add("migrations", "+ migrations")
	case n == nil:
		d.add("migrations", "- migrations")
	default:
		d.field("migrations", "version", list(p.Version), list(n.Version))
		d.field("migrations", "apply", list(p.Apply), list(n.Apply))
		d.field("migrations", "revert", list(p.Revert), list(n.Revert))
		d.field("migrations", "timeout", orNone(p.Timeout, "none"), orNone(n.Timeout, "none"))
		d.field("migrations", "retries", intOf(p.Retries), intOf(n.Retries))
	}
}

// ingress notes the hostnames that are added, removed or moved to another role.
func (d *differ) ingress(p, n manifest.App) {
	ph, nh := hostRoles(p), hostRoles(n)
	for _, host := range union(ph, nh) {
		pr, hadP := ph[host]
		nr, hasN := nh[host]
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
		case !hasB:
			d.add("jobs", "- %s", name)
		default:
			d.fieldOf("jobs", name, "role", quote(a.Role), quote(b.Role))
			d.fieldOf("jobs", name, "service", quote(a.Service), quote(b.Service))
			d.fieldOf("jobs", name, "schedule", quote(a.Schedule), quote(b.Schedule))
			d.fieldOf("jobs", name, "command", list(a.Command), list(b.Command))
			d.fieldOf("jobs", name, "timeout", quote(a.Timeout), quote(b.Timeout))
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
		case !hasB:
			d.add("backups", "- %s", name)
		default:
			d.fieldOf("backups", name, "volume", quote(a.Volume), quote(b.Volume))
			d.fieldOf("backups", name, "command", list(a.Command), list(b.Command))
			d.fieldOf("backups", name, "role", quote(a.Role), quote(b.Role))
			d.fieldOf("backups", name, "service", quote(a.Service), quote(b.Service))
			d.fieldOf("backups", name, "timeout", quote(a.Timeout), quote(b.Timeout))
			d.fieldOf("backups", name, "schedule", quote(a.Schedule), quote(b.Schedule))
			d.fieldOf("backups", name, "destination", quote(a.Destination), quote(b.Destination))
			d.fieldOf("backups", name, "prefix", quote(a.Prefix), quote(b.Prefix))
			d.fieldOf("backups", name, "before", quote(a.Before), quote(b.Before))
			d.fieldOf("backups", name, "retention", jsonText(a.Retention), jsonText(b.Retention))
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
		case !hasB:
			d.add("probes", "- %s", name)
		case jsonText(a) != jsonText(b):
			d.add("probes", "~ %s changed", name)
		}
	}
}

// fieldOf notes a setting of a named job or backup whose text differs.
func (d *differ) fieldOf(area, name, setting, prev, next string) {
	if prev != next {
		d.add(area, "~ %s %s %s -> %s", name, setting, prev, next)
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
		return "none"
	}
	return strconv.Itoa(*n)
}

func intOrNone(n int) string {
	if n == 0 {
		return "none"
	}
	return strconv.Itoa(n)
}

// list writes a list of words as JSON, such as ["serve","--fast"], or none when it is empty.
func list(s []string) string {
	if len(s) == 0 {
		return "none"
	}
	return jsonText(s)
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
