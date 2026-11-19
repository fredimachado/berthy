package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"

	"berthy/internal/engine"
	"berthy/internal/manifest"
	"berthy/internal/state"
)

// DriftReport is what differs between what an app runs and what its applied release says it should run. It reports and
// corrects nothing: a deploy brings the app back.
type DriftReport struct {
	// Release is the tag of the applied release, or the start of its digest when it has no tag.
	Release string `json:"release,omitempty"`
	// Lines holds one line per difference, such as "api/web: replicas 3, release says 2". A line never holds the value of a
	// variable.
	Lines []string `json:"lines,omitempty"`
	// Scaled holds one line per role that runs another number of replicas because berthy scale set it. That is not drift.
	Scaled []string `json:"scaled,omitempty"`
}

// Drift compares what app runs with its applied release. An image app is compared role by role: the live Swarm service
// against the spec a deploy of the release would send. A Compose app is compared by the plan a deploy of its stored Compose
// file would make: the services it would recreate and the volumes it would create. It makes no change, and it pulls nothing.
// It returns sql.ErrNoRows for an app that has no applied release.
func (c *Controller) Drift(ctx context.Context, app string) (DriftReport, error) {
	if c.Store == nil || c.Engine == nil {
		return DriftReport{}, errors.New("Docker unavailable")
	}
	release, err := c.Store.Applied(ctx, app)
	if err != nil {
		return DriftReport{}, err
	}
	// The manifest is read as a rollback reads it, so the spec is the one a deploy of this release sends.
	var m manifest.Manifest
	if json.Unmarshal(release.Manifest, &m) != nil {
		return DriftReport{}, errors.New("applied manifest unavailable")
	}
	m.Connector = app == manifest.ConnectorAppName
	a, ok := m.Apps[app]
	if !ok {
		return DriftReport{}, errors.New("applied manifest unavailable")
	}
	report := DriftReport{Release: releaseLabel(release)}
	if a.Kind == "compose" {
		report.Lines, err = c.composeDrift(ctx, release, m)
	} else {
		report.Lines, report.Scaled, err = c.imageDrift(ctx, release, m)
	}
	return report, err
}

// releaseLabel names a release in a line: its tag, or the start of its digest.
func releaseLabel(r state.Release) string {
	if r.Tag != "" {
		return r.Tag
	}
	return shortDigest(pinnedDigest(r.Digest))
}

// imageDrift compares each role of the release with its bound service.
func (c *Controller) imageDrift(ctx context.Context, release state.Release, m manifest.Manifest) (lines, scaled []string, err error) {
	app := release.App
	prepared, err := c.prepareExecution(ctx, Request{App: app, Manifest: m}, Plan{App: app, Image: release.Digest, Tag: release.Tag})
	if err != nil {
		return nil, nil, err
	}
	// The release's image is read, never pulled: an image that is not here cannot be what runs.
	img, err := c.Engine.InspectImage(ctx, prepared.Image)
	if err != nil {
		return nil, nil, errors.New("release image unavailable")
	}
	roles := make([]string, 0, len(m.Apps[app].Roles))
	for role := range m.Apps[app].Roles {
		roles = append(roles, role)
	}
	sort.Strings(roles)
	for _, role := range roles {
		label := app + "/" + role
		bi := slices.IndexFunc(release.Bindings, func(b state.Binding) bool { return b.Role == role })
		if bi < 0 {
			lines = append(lines, label+": no service is bound")
			continue
		}
		bound := release.Bindings[bi]
		live, err := c.Engine.InspectService(ctx, bound.Name)
		if err != nil {
			var status *engine.StatusError
			if errors.As(err, &status) && status.Code == 404 {
				lines = append(lines, fmt.Sprintf("%s: service %s is gone", label, bound.Name))
				continue
			}
			return nil, nil, errors.New("service unavailable")
		}
		if live.ID != bound.ServiceID {
			lines = append(lines, fmt.Sprintf("%s: service %s is not the one the release bound", label, bound.Name))
			continue
		}
		want, err := c.releaseRoleSpec(prepared, img, role, "")
		if err != nil {
			return nil, nil, errors.New("release spec unavailable")
		}
		l, s := roleDrift(label, live.Spec, want, c.scaledByOperation(ctx, live.Spec))
		lines = append(lines, l...)
		if s != "" {
			scaled = append(scaled, s)
		}
	}
	return lines, scaled, nil
}

// scaledByOperation reports whether the operation that last edited the live service is a scale or a restart: its berthy.operation
// label names one. Both edit the live spec and carry its replica count forward, and a restart never changes the count, so a role
// scaled and then restarted is still scaled.
func (c *Controller) scaledByOperation(ctx context.Context, live json.RawMessage) bool {
	var spec struct {
		TaskTemplate struct {
			ContainerSpec struct{ Labels map[string]string }
		}
	}
	if json.Unmarshal(live, &spec) != nil {
		return false
	}
	id := spec.TaskTemplate.ContainerSpec.Labels["berthy.operation"]
	if id == "" {
		return false
	}
	op, err := c.Store.Operation(ctx, id)
	return err == nil && (op.Kind == "scale" || op.Kind == "restart")
}

// imagePath is where a spec holds its image.
const imagePath = "TaskTemplate.ContainerSpec.Image"

// roleDrift lists, one line each, how the live spec of a role differs from the spec its release produces: a field that has
// another value, one only the live service has and one only the release has. scaled says that berthy scale set the live replica
// count; a count that differs then comes back as scaledLine and is not a difference. The name, the Berthy labels, what Docker
// adds on its own and the force-update counter are set aside. A variable is named, and its value is never shown.
func roleDrift(label string, live, desired json.RawMessage, scaled bool) (lines []string, scaledLine string) {
	l, ok1 := comparableSpec(live)
	d, ok2 := comparableSpec(desired)
	if !ok1 || !ok2 {
		return []string{label + ": service spec unreadable"}, ""
	}
	have, want := flattenSpec(l), flattenSpec(d)
	paths := make([]string, 0, len(have)+len(want))
	for p := range have {
		paths = append(paths, p)
	}
	for p := range want {
		if _, both := have[p]; !both {
			paths = append(paths, p)
		}
	}
	sort.Strings(paths)
	for _, p := range paths {
		h, hasLive := have[p]
		w, hasWant := want[p]
		if hasLive && hasWant && valueJSON(h.value) == valueJSON(w.value) {
			continue
		}
		path := h.path
		if !hasLive {
			path = w.path
		}
		name, _ := specFieldName(path)
		if p == imagePath {
			name = "image"
		}
		if scaled && p == "Mode.Replicated.Replicas" {
			scaledLine = fmt.Sprintf("%s: scaled to %s with berthy scale", label, driftValue(path, h.value, hasLive))
			continue
		}
		lines = append(lines, fmt.Sprintf("%s: %s %s, release says %s", label, name, driftValue(path, h.value, hasLive), driftValue(path, w.value, hasWant)))
	}
	liveEnv, wantEnv := envOf(l), envOf(d)
	names := map[string]bool{}
	for name, value := range liveEnv {
		if next, ok := wantEnv[name]; !ok || next != value {
			names[name] = true
		}
	}
	for name := range wantEnv {
		if _, ok := liveEnv[name]; !ok {
			names[name] = true
		}
	}
	if len(names) > 0 {
		sorted := make([]string, 0, len(names))
		for name := range names {
			sorted = append(sorted, name)
		}
		sort.Strings(sorted)
		lines = append(lines, fmt.Sprintf("%s: env %s %s", label, strings.Join(sorted, ", "), pick(len(sorted), "differs", "differ")))
	}
	if want, have := specLogDriver(d), specLogDriver(l); !sameLogDriver(want, have) {
		lines = append(lines, fmt.Sprintf("%s: logging %s, release says %s", label, describeLogDriver(have), describeLogDriver(want)))
	}
	return lines, scaledLine
}

// driftValue is a field's value in a line: "none" when the spec does not have it, the image by its digest's first digits, and
// any other value as short JSON.
func driftValue(path []string, v any, present bool) string {
	if !present {
		return "none"
	}
	if s, ok := v.(string); ok && strings.Join(path, ".") == imagePath {
		name, digest, found := strings.Cut(s, "@")
		if found {
			return name + "@" + shortDigest(digest)
		}
		return s
	}
	return shownValue(v)
}

// composeDrift lists the services a deploy of the stored Compose file would recreate, create or remove, and the volumes it
// would create. It asks Compose for the plan a deploy runs, so it says what that deploy would do. The plan's blockers are not
// drift and are left out.
func (c *Controller) composeDrift(ctx context.Context, release state.Release, m manifest.Manifest) ([]string, error) {
	file, err := c.composeSource(ctx, release.OperationID)
	if err != nil {
		return nil, err
	}
	if err = c.composeReady(); err != nil {
		return nil, err
	}
	req := Request{App: release.App, Manifest: m, ComposeFile: file}
	dir, err := c.newComposeDir(".drift-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	auth, err := c.stageCompose(ctx, dir, req)
	if err != nil {
		return nil, err
	}
	if auth != "" {
		defer os.RemoveAll(auth)
	}
	plan, err := c.composePlanIn(ctx, dir, Actor{Admin: true}, req, false)
	if err != nil {
		return nil, err
	}
	var lines []string
	for _, s := range plan.Services {
		switch s.Action {
		case "recreate", "create", "remove":
			lines = append(lines, fmt.Sprintf("%s: service %s would be %sd", release.App, s.Name, s.Action))
		}
	}
	for _, v := range plan.Volumes {
		if v.Action == "create" {
			lines = append(lines, fmt.Sprintf("%s: volume %s would be created", release.App, v.Name))
		}
	}
	return lines, nil
}
