package agent

import (
	"context"
	"database/sql"
	"errors"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"berthy/internal/api"
	"berthy/internal/deploy"
	"berthy/internal/engine"
	"berthy/internal/manifest"
	"berthy/internal/metrics"
	"berthy/internal/monitor"
	"berthy/internal/scope"
	"berthy/internal/state"
)

type jobScheduleSighting struct {
	expression string
	timezone   string
	firstSeen  time.Time
}

// Monitoring runs the agent's health checks. The checks come from the running manifest and from the built-in
// conditions, and their states are stored in the agent's state. Monitoring needs state, so NewMonitoring returns nil
// without it.
type Monitoring struct {
	cfg     Config
	store   *state.Store
	engine  *engine.Engine
	metrics *metrics.Collector
	deploy  *deploy.Controller
	watch   *monitor.RestartWatch
	down    *monitor.DownWatch
	mon     *monitor.Monitor
	// helper runs the service probes from the overlay network. It is never nil.
	helper *helperManager
	// composeServices lists the services of a Compose project that have containers.
	composeServices func(ctx context.Context, project string) ([]string, error)
	// lastApp holds the restart and down checks of the last successful inventory, kept while Docker cannot be asked.
	lastApp []monitor.Check
	// read is the last inventory read, which the restart and down watches are shown again at each rebuild until the next read
	// is due. Only the monitor's goroutine uses it. readNow asks for the next rebuild to read the inventory: an event set it,
	// or the last read failed.
	read    *inventoryRead
	readNow atomic.Bool
	// root is Docker's data root once Docker has reported it. Only the monitor's goroutine uses it.
	root string
	// seen is when each backup was first in the manifest, and now is the clock of the backup checks. stateSeen is the same for
	// the state backup.
	seen         backupSeen
	stateSeen    backupSeen
	jobSightings map[string]jobScheduleSighting
	now          func() time.Time
	// beat sends the heartbeat pings, and remembers when one was last accepted across the monitor's rebuilds of its checks.
	beat *monitor.Heartbeat
}

// NewMonitoring returns the monitoring of an agent with state, or nil for an agent without state. log receives
// operational messages, which never contain secrets.
func NewMonitoring(c Config, e *engine.Engine, col *metrics.Collector, d *deploy.Controller, log func(string)) *Monitoring {
	if d == nil || d.Store == nil {
		return nil
	}
	w := &Monitoring{cfg: c, store: d.Store, engine: e, metrics: col, deploy: d, watch: monitor.NewRestartWatch(nil), down: monitor.NewDownWatch(nil), helper: newHelperManager(c.ID(), e, log)}
	w.composeServices = w.projectServices
	w.beat = &monitor.Heartbeat{Alive: w.dockerAnswers, Client: c.outboundHTTP()}
	w.mon = &monitor.Monitor{Env: c.Environment, Store: d.Store, Checks: w.checks, Notifier: w.notifier, Log: log}
	return w
}

// clock is the time of the backup checks.
func (w *Monitoring) clock() time.Time {
	if w.now != nil {
		return w.now()
	}
	return time.Now()
}

// HelperImage returns the helper image of the agent's binary, importing it when Docker lacks it. The backup containers run
// from it, and it shares the import with the overlay helper.
func (w *Monitoring) HelperImage(ctx context.Context) (string, error) {
	if w == nil || w.helper == nil || w.helper.engine == nil {
		return "", errors.New("helper image unavailable")
	}
	return w.helper.Image(ctx)
}

// Reach is the deploy's check that a role's port answers on the shared network, which the overlay helper makes. It starts the
// helper when no service probe has.
func (w *Monitoring) Reach(ctx context.Context, network, host string, port int) error {
	return w.helper.Reach(ctx, network, host, port)
}

// Run runs the monitor until ctx ends, and follows Docker's events to wake it early. It does nothing on a nil Monitoring.
func (w *Monitoring) Run(ctx context.Context) {
	if w == nil {
		return
	}
	// Subscribed before the monitor starts, so no event of its first rebuild is missed.
	events, stop := w.engine.Watch(func(engine.Event) bool { return true })
	defer stop()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		w.followEvents(ctx, events)
	}()
	defer wg.Wait()
	w.mon.Run(ctx)
}

// followEvents wakes the checks that an event of Docker concerns. An event only asks for an early read: the checks decide
// as before, and the periodic read stays the safety net for an event that never comes.
func (w *Monitoring) followEvents(ctx context.Context, events <-chan engine.Event) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-events:
			if prefixes := eventPrefixes(ev); prefixes != nil {
				w.readNow.Store(true)
				w.mon.Kick(prefixes...)
			}
		}
	}
}

// eventPrefixes names the checks an event wakes, by the start of their names, or nil when the event wakes none. A container
// that starts, restarts, dies, is killed for memory or changes health concerns its restart and down checks. A service that is updated or removed
// may also be a change made outside Berthy, which the drift check reports. The resync, sent when the feed returns after a
// gap, wakes all of them.
func eventPrefixes(ev engine.Event) []string {
	switch {
	case ev.Resync():
		return []string{"restarts:", "down:", "drift:"}
	case ev.Type == "container" && (ev.Action == "start" || ev.Action == "restart" || ev.Action == "die" || ev.Action == "oom" || ev.Action == "health_status"):
		return []string{"restarts:", "down:"}
	case ev.Type == "service" && (ev.Action == "update" || ev.Action == "remove"):
		return []string{"restarts:", "down:", "drift:"}
	}
	return nil
}

// checks returns the checks that should run now. A nil result keeps the checks that are running, which the monitor does
// when the manifest cannot be read, or when the first host sample has not been taken yet: the disk checks cannot be
// named until then, so the monitor asks again soon.
func (w *Monitoring) checks(ctx context.Context) []monitor.Check {
	if _, ok := w.samples(); !ok && w.metrics != nil {
		return nil
	}
	m, manifestErr := runningManifest(ctx, w.store)
	if manifestErr != nil && !errors.Is(manifestErr, sql.ErrNoRows) {
		return nil
	}
	st, ingressErr := w.store.Ingress(ctx)
	if ingressErr != nil && !errors.Is(ingressErr, sql.ErrNoRows) {
		return nil
	}
	var mp *manifest.Manifest
	if manifestErr == nil {
		mp = &m
	}
	var ingress *state.IngressState
	if ingressErr == nil {
		ingress = &st
	}
	w.maintainHelper(ctx, mp)
	out := w.builtins(ctx, ingressErr == nil, mp)
	out = append(out, w.credentialChecks(mp, ingress)...)
	out = append(out, w.driftChecks(ctx)...)
	tokenChecks, tokenErr := w.tokenChecks(ctx)
	if tokenErr != nil {
		return nil
	}
	out = append(out, tokenChecks...)
	if mp != nil {
		checks, err := w.manifestChecks(ctx, *mp)
		if err != nil {
			return nil
		}
		out = append(out, checks...)
	}
	return out
}

// maintainHelper creates or repairs the overlay helper while the running manifest has a service probe, and removes it
// otherwise. The probes report the outcome, so nothing is returned.
func (w *Monitoring) maintainHelper(ctx context.Context, m *manifest.Manifest) {
	if m != nil && hasServiceProbe(m.Probes) {
		_ = w.helper.ensure(ctx, m.NetworkName(w.cfg.Environment))
		return
	}
	_ = w.helper.remove(ctx)
}

// hasServiceProbe reports whether any probe runs against a service name.
func hasServiceProbe(probes map[string]manifest.Probe) bool {
	for _, p := range probes {
		if p.Service != nil {
			return true
		}
	}
	return false
}

// builtins returns the conditions that always run: disk and memory from the host sample, Docker's data disk, restart
// loops and missing replicas of the apps Berthy manages, and the managed tunnel when managed is true. m is the running
// manifest, or nil.
func (w *Monitoring) builtins(ctx context.Context, managed bool, m *manifest.Manifest) []monitor.Check {
	sample, ok := w.samples()
	var mounts []string
	if ok {
		for _, mount := range sample.Host.Mounts {
			mounts = append(mounts, mount.Path)
		}
	}
	root := w.dockerDataRoot(ctx)
	holder := monitor.MountFor(root, mounts)
	var out []monitor.Check
	for _, path := range monitor.DiskMounts(sample.Host.Mounts, root) {
		out = append(out, monitor.DiskCheck(w.samples, path, path == holder))
	}
	// While Docker cannot say where its data is, the Docker check reports unknown. Once it can, the disk check of the
	// mount that holds the data carries the Docker label, so no separate check is needed.
	if root == "" {
		out = append(out, monitor.DockerDiskCheck(w.samples, ""))
	}
	out = append(out, monitor.MemoryCheck(w.samples))
	out = append(out, w.appChecks(ctx, m)...)
	out = append(out, hostChecks(w.engine, w.cfg.Listen)...)
	if w.cfg.startupChecks != nil {
		for _, check := range w.cfg.startupChecks() {
			result := monitor.Result{State: monitor.OK, Detail: check.Detail}
			if !check.OK {
				result.State = monitor.Critical
			}
			out = append(out, monitor.NewFunc(check.Name, hostCheckInterval, 1, func(context.Context) monitor.Result {
				return result
			}))
		}
	}
	if managed {
		out = append(out, monitor.TunnelCheck(w.tunnelStatus))
	}
	return out
}

// manifestChecks returns the probes, the peer checks, and the alert configuration of the running manifest.
func (w *Monitoring) manifestChecks(ctx context.Context, m manifest.Manifest) ([]monitor.Check, error) {
	var out []monitor.Check
	sightings := make(map[string]jobScheduleSighting)
	jobChecks := make([]monitor.Check, 0, len(m.Jobs))
	for _, name := range slices.Sorted(maps.Keys(m.Jobs)) {
		expression := m.Jobs[name].Schedule
		if expression == "" {
			continue
		}
		sched, err := m.ParseSchedule(expression)
		if err != nil {
			return nil, err
		}
		zone := m.Location().String()
		sighting, ok := w.jobSightings[name]
		if !ok || sighting.expression != expression || sighting.timezone != zone {
			sighting = jobScheduleSighting{expression: expression, timezone: zone, firstSeen: w.clock()}
		}
		sightings[name] = sighting
		jobChecks = append(jobChecks, jobCheck(w.store, name, sched, sighting.firstSeen, w.clock))
	}
	for _, name := range slices.Sorted(maps.Keys(m.Probes)) {
		p := m.Probes[name]
		interval, timeout := p.Durations()
		failures := p.FailureLimit()
		switch {
		case p.HTTP != "":
			out = append(out, monitor.HTTPProbe("probe:"+name, p.HTTP, p.ExpectStatus, interval, timeout, failures, w.cfg.dial))
		case p.TCP != "":
			out = append(out, monitor.TCPProbe("probe:"+name, p.TCP, interval, timeout, failures, w.cfg.dial))
		default:
			out = append(out, w.serviceCheck(ctx, "probe:"+name, m, p, interval, timeout, failures))
		}
	}
	out = append(out, jobChecks...)
	for _, name := range slices.Sorted(maps.Keys(m.Backups)) {
		out = append(out, backupCheck(w.store, name, m.Backups[name], m.Location(), &w.seen, w.clock))
	}
	// The state backup is environment-wide, so the block is read whatever apps are deployed.
	if m.StateBackup != nil {
		out = append(out, stateBackupCheck(w.store, *m.StateBackup, m.Location(), &w.stateSeen, w.clock))
	}
	for _, name := range slices.Sorted(maps.Keys(m.Peers)) {
		out = append(out, monitor.PeerCheck("peer:"+name, m.Peers[name], w.cfg.dial))
	}
	if m.Alerts != nil {
		alerts := *m.Alerts
		for _, ch := range alertChannels(ctx, w.cfg, w.store, alerts) {
			out = append(out, monitor.AlertSecretCheck(ch.name, func(ctx context.Context) (bool, string) {
				return alertChannelStatus(ctx, w.cfg, w.store, alerts, ch.name)
			}))
		}
	}
	if m.Alerts != nil && m.Alerts.Heartbeat != nil {
		hb := *m.Alerts.Heartbeat
		out = append(out, w.beat.Check(hb.Every(), func(ctx context.Context) (string, error) {
			return urlSecret(ctx, w.store, "heartbeat", hb.URLSecret)
		}))
	}
	w.jobSightings = sightings
	return out, nil
}

// dockerAnswers reports whether Docker answers now. The heartbeat is sent only while it does.
func (w *Monitoring) dockerAnswers(ctx context.Context) bool {
	if w.engine == nil {
		return false
	}
	dctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	var version struct{}
	return w.engine.Get(dctx, "/version", &version) == nil
}

// serviceCheck returns the check of a probe against a service name. The name is resolved now; when it cannot be, the check
// reports unknown with the reason. Otherwise the probe runs in the overlay helper, as HTTP when the probe has a path.
func (w *Monitoring) serviceCheck(ctx context.Context, name string, m manifest.Manifest, p manifest.Probe, interval, timeout time.Duration, failures int) monitor.Check {
	s := *p.Service
	target, err := w.serviceTarget(ctx, m, s)
	if err != nil {
		detail := err.Error()
		return monitor.NewFunc(name, interval, failures, func(context.Context) monitor.Result {
			return monitor.Result{State: monitor.Unknown, Detail: detail}
		})
	}
	req := monitor.HelperProbe{Kind: monitor.KindTCP, Target: target + ":" + strconv.Itoa(s.Port), TimeoutMS: int(timeout / time.Millisecond)}
	if s.Path != "" {
		req.Kind, req.Path, req.Expect = monitor.KindHTTP, s.Path, p.ExpectStatus
	}
	return monitor.ServiceProbe(name, interval, failures, w.helper, req)
}

// serviceTarget returns the name a service probe reaches on the overlay network: the Swarm service of an image app's role,
// or the Compose service of a Compose app. The error is the check's detail when no name is known.
func (w *Monitoring) serviceTarget(ctx context.Context, m manifest.Manifest, s manifest.ServiceProbe) (string, error) {
	app, ok := m.Apps[s.App]
	if !ok {
		return "", errors.New("probe target unknown: app not in the manifest")
	}
	switch app.Kind {
	case "image":
		role := s.Role
		if role == "" && len(app.Roles) == 1 {
			for r := range app.Roles {
				role = r
			}
		}
		if role == "" {
			return "", errors.New("probe target unknown: role not named")
		}
		bindings, err := w.store.Bindings(ctx, s.App)
		if err != nil {
			return "", errors.New("probe target unknown: bindings unavailable")
		}
		for _, b := range bindings {
			if b.Role == role {
				return b.Name, nil
			}
		}
		// Not deployed yet: the name the deploy gives the role's Swarm service.
		return m.ServiceName(w.cfg.Environment, s.App, role), nil
	case "compose":
		if s.Role != "" {
			return s.Role, nil
		}
		services, err := w.composeServices(ctx, m.ComposeProject(w.cfg.Environment, s.App))
		if err != nil {
			return "", errors.New("probe target unknown: Compose services unavailable")
		}
		if len(services) != 1 {
			return "", errors.New("probe target unknown: name the Compose service as role")
		}
		return services[0], nil
	}
	return "", errors.New("probe target unknown")
}

// projectServices returns the sorted names of the services of a Compose project that have containers, one-off runs
// excepted.
func (w *Monitoring) projectServices(ctx context.Context, project string) ([]string, error) {
	if w.engine == nil {
		return nil, errors.New("Docker unavailable")
	}
	containers, err := w.engine.InventoryContainers(ctx)
	if err != nil {
		return nil, err
	}
	names := map[string]bool{}
	for _, c := range containers {
		oneOff := c.Labels["com.docker.compose.oneoff"]
		if c.Labels["com.docker.compose.project"] != project || oneOff == "True" || oneOff == "true" {
			continue
		}
		if svc := c.Labels["com.docker.compose.service"]; svc != "" {
			names[svc] = true
		}
	}
	return slices.Sorted(maps.Keys(names)), nil
}

// appChecks returns the restart check of every role of the apps Berthy manages, and their down checks. Both are fed by the
// one inventory read here. While the inventory, the task list or the operation state cannot be read, the checks of the last
// good read keep running, and their results turn unknown as they age.
func (w *Monitoring) appChecks(ctx context.Context, m *manifest.Manifest) []monitor.Check {
	now := w.clock()
	if w.readDue(now) {
		read, err := w.readInventory(ctx, now)
		if err != nil {
			// Tried again at the next rebuild, not after the whole interval.
			w.readNow.Store(true)
			return w.previousAppChecks(ctx)
		}
		w.read = read
	}
	quiet, err := w.quietApps(ctx, m, now)
	if err != nil {
		return w.previousAppChecks(ctx)
	}
	// The watches are shown the read again at each rebuild, with the read's own time: what changes between reads is the
	// manifest and which apps are quiet, and the age of the read is what makes the checks unknown once it is too old.
	managed := managedRestartApps(w.read.apps, m, w.cfg.Environment)
	w.watch.Observe(w.read.at, managed, w.read.failures)
	w.down.Observe(w.read.at, downApps(managed, m, w.cfg.Environment), quiet)
	var out []monitor.Check
	for _, app := range managed {
		for _, role := range app.Roles {
			out = append(out, w.watch.Check(app.Ref, role.Name))
		}
	}
	out = append(out, w.down.Checks()...)
	w.lastApp = out
	return out
}

// inventoryRead is one read of Docker for the restart and down checks: the apps, and the failed tasks of each role, at a time.
type inventoryRead struct {
	at       time.Time
	apps     []api.App
	failures map[string]int
}

// readDue reports whether the inventory is to be read at this rebuild: it has never been, an event or a failed read asked for
// it, or the last read is older than the cadence. The cadence is slow while Docker's events are flowing, because an event
// reads it sooner, and a minute while they are not.
func (w *Monitoring) readDue(now time.Time) bool {
	wanted := w.readNow.Swap(false)
	if w.read == nil || wanted {
		return true
	}
	every := monitor.InventoryFallbackEvery
	if w.engine.EventsLive() {
		every = monitor.InventoryEvery
	}
	return now.Sub(w.read.at) >= every
}

// readInventory reads the inventory and the task failures at now.
func (w *Monitoring) readInventory(ctx context.Context, now time.Time) (*inventoryRead, error) {
	apps, err := w.inventory(ctx)
	if err != nil {
		return nil, err
	}
	failures, err := w.taskFailures(ctx, now)
	if err != nil {
		return nil, err
	}
	return &inventoryRead{at: now, apps: apps, failures: failures}, nil
}

// previousAppChecks are the checks to keep while the inventory cannot be read: those of the last good read, or, after a
// restart of the agent, placeholders for the stored restart and down checks that report unknown.
func (w *Monitoring) previousAppChecks(ctx context.Context) []monitor.Check {
	if len(w.lastApp) > 0 {
		return w.lastApp
	}
	return w.staleAppChecks(ctx)
}

// managedRestartApps keeps the apps Berthy manages: image apps, which appear in the inventory only through their bindings
// (the ingress connector among them), and Compose apps of the running manifest, matched by project name. Unmanaged
// Swarm services and Compose projects that no manifest names are left out.
func managedRestartApps(apps []api.App, m *manifest.Manifest, environment string) []api.App {
	projects := map[string]bool{}
	if m != nil {
		for name, a := range m.Apps {
			if a.Kind == "compose" {
				projects[m.ComposeProject(environment, name)] = true
			}
		}
	}
	var out []api.App
	for _, app := range apps {
		switch app.Ref.Kind {
		case "image":
			out = append(out, app)
		case "compose":
			if projects[app.Ref.Name] {
				out = append(out, app)
			}
		}
	}
	return out
}

// taskFailures counts, for every role bound to a Swarm service, the tasks that failed inside the restart window ending
// at now. The keys are monitor.RoleKey values.
func (w *Monitoring) taskFailures(ctx context.Context, now time.Time) (map[string]int, error) {
	bindings, err := w.store.Bindings(ctx, "")
	if err != nil {
		return nil, err
	}
	tctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	since := now.Add(-monitor.RestartWindow)
	out := make(map[string]int, len(bindings))
	for _, b := range bindings {
		tasks, err := w.engine.ServiceTasks(tctx, b.ServiceID)
		if err != nil {
			return nil, err
		}
		out[monitor.RoleKey(api.AppRef{Kind: "image", Name: b.App}, b.Role)] = engine.FailedTasksSince(tasks, since)
	}
	return out, nil
}

// staleAppChecks keeps the stored restart and down checks of an earlier run in place, unknown, when no inventory has been
// read since the agent started. Their stored states survive, so a restart does not send their alerts again.
func (w *Monitoring) staleAppChecks(ctx context.Context) []monitor.Check {
	stored, err := w.store.Checks(ctx)
	if err != nil {
		return nil
	}
	var out []monitor.Check
	for _, s := range stored {
		if strings.HasPrefix(s.Name, "restarts:") || strings.HasPrefix(s.Name, "down:") {
			out = append(out, monitor.NewFunc(s.Name, time.Minute, 1, func(context.Context) monitor.Result {
				return monitor.Result{State: monitor.Unknown, Detail: "Docker inventory unavailable"}
			}))
		}
	}
	return out
}

// inventory returns the apps and their replicas, as the API shows them.
func (w *Monitoring) inventory(ctx context.Context) ([]api.App, error) {
	if w.engine == nil {
		return nil, errors.New("Docker unavailable")
	}
	ictx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return appInventory(ictx, w.engine, w.deploy)
}

// dockerDataRoot returns the directory Docker keeps its data in, or empty while Docker cannot say. The answer does not
// change, so the first one is kept.
func (w *Monitoring) dockerDataRoot(ctx context.Context) string {
	if w.root != "" || w.engine == nil {
		return w.root
	}
	dctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var info struct {
		DockerRootDir string
	}
	if err := w.engine.Get(dctx, "/info", &info); err == nil {
		w.root = info.DockerRootDir
	}
	return w.root
}

// samples returns the latest host sample.
func (w *Monitoring) samples() (metrics.Sample, bool) {
	if w.metrics == nil {
		return metrics.Sample{}, false
	}
	return w.metrics.Latest()
}

// tunnelStatus returns the managed tunnel's status as Cloudflare reports it.
func (w *Monitoring) tunnelStatus(ctx context.Context) string {
	if w.deploy == nil {
		return "unavailable"
	}
	_, status := w.deploy.TunnelHealth(ctx)
	return status
}

// notifier is the monitor's alert sender. It returns nil when no alert channel is configured.
func (w *Monitoring) notifier(ctx context.Context) (monitor.Notifier, time.Duration, error) {
	m, err := settingsManifest(ctx, w.store)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, errAlertConfig
	}
	if m.Alerts == nil {
		return nil, 0, nil
	}
	notifiers := alertNotifiers(ctx, w.cfg, w.store, *m.Alerts)
	if len(notifiers) == 0 {
		return nil, 0, nil
	}
	return notifiers, m.Alerts.ReminderInterval(), nil
}

// monitoringChecks returns the doctor's non-critical checks of the running manifest: one per peer, one per alert channel for the
// secrets it needs, and the overlay helper when a service probe needs it. A peer's stored state is used when
// the monitor has one; otherwise the peer is asked now.
func monitoringChecks(ctx context.Context, c Config, s *state.Store, e *engine.Engine, environment string) []api.Check {
	m, err := runningManifest(ctx, s)
	if err != nil {
		return nil
	}
	stored, _ := s.Checks(ctx)
	states := make(map[string]state.CheckState, len(stored))
	for _, c := range stored {
		states[c.Name] = c
	}
	peers := slices.Sorted(maps.Keys(m.Peers))
	out := make([]api.Check, len(peers))
	var wg sync.WaitGroup
	for i, name := range peers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out[i] = peerDoctorCheck(ctx, name, m.Peers[name], states, c.dial)
		}()
	}
	wg.Wait()
	if m.Alerts != nil {
		for _, ch := range alertChannels(ctx, c, s, *m.Alerts) {
			ok, detail := alertChannelStatus(ctx, c, s, *m.Alerts, ch.name)
			out = append(out, api.Check{Name: ch.name, OK: ok, Detail: detail})
		}
	}
	// A backup's doctor check mirrors the state of its monitor check, so the doctor and the alerts always agree. It is never
	// critical for the doctor's exit code: a failed backup is an alert, and the agent itself is healthy.
	for _, name := range slices.Sorted(maps.Keys(m.Backups)) {
		c := api.Check{Name: "backup:" + name, OK: true, Detail: "not checked yet"}
		if st, ok := states["backup:"+name]; ok && st.State != "" {
			c.OK = st.State == monitor.OK || st.State == monitor.Unknown
			c.Detail = st.State + ": " + st.Detail
		}
		out = append(out, c)
	}
	if m.Alerts != nil && m.Alerts.Heartbeat != nil {
		out = append(out, heartbeatDoctorCheck(ctx, s, *m.Alerts.Heartbeat, states))
	}
	if hasServiceProbe(m.Probes) {
		ok, detail := newHelperManager(scope.ID{Project: c.Project, Environment: environment}, e, nil).status(ctx, m.NetworkName(environment))
		out = append(out, api.Check{Name: "probe_helper", OK: ok, Detail: detail})
	}
	if check, ok := orphanAppsCheck(ctx, s); ok {
		out = append(out, check)
	}
	return out
}

// heartbeatDoctorCheck reports the heartbeat for the doctor: a secret that cannot be used is a failure, and otherwise the monitor's
// stored state, which says when the last ping was accepted. It mirrors that state, so the doctor and `berthy checks` agree, and
// it is never critical: an unknown heartbeat is no failure of the agent.
func heartbeatDoctorCheck(ctx context.Context, s *state.Store, h manifest.Heartbeat, states map[string]state.CheckState) api.Check {
	c := api.Check{Name: "heartbeat", OK: true, Detail: "not checked yet"}
	if _, err := urlSecret(ctx, s, "heartbeat", h.URLSecret); err != nil {
		c.OK, c.Detail = false, err.Error()
		return c
	}
	if st, ok := states["heartbeat"]; ok && st.State != "" {
		c.Detail = st.State + ": " + st.Detail
	}
	return c
}

// peerDoctorCheck reports one peer for the doctor.
func peerDoctorCheck(ctx context.Context, name, base string, states map[string]state.CheckState, dial monitor.DialFunc) api.Check {
	c := api.Check{Name: "peer:" + name}
	if st, ok := states["peer:"+name]; ok && st.State != monitor.Unknown && st.State != "" {
		c.OK = st.State == monitor.OK
		c.Detail = st.Detail
		return c
	}
	pctx, cancel := context.WithTimeout(ctx, doctorPeerTimeout)
	defer cancel()
	res := monitor.ProbePeer(pctx, base, 3*time.Second, dial)
	c.OK = res.State == monitor.OK
	c.Detail = res.Detail
	return c
}

// serveHealth answers /healthz, which peer agents ask without a token. It says nothing about the environment. It reports
// whether it handled the request.
func serveHealth(w http.ResponseWriter, r *http.Request) bool {
	if r.URL.Path != "/healthz" {
		return false
	}
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		return true
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"ok":true}`))
	return true
}
