package agent

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"berthy/internal/api"
	"berthy/internal/deploy"
	"berthy/internal/engine"
	"berthy/internal/manifest"
	"berthy/internal/metrics"
	"berthy/internal/monitor"
	"berthy/internal/state"
)

// errAlertsNotConfigured means no alert channel is set up for the environment, or its bot token secret is not stored.
var errAlertsNotConfigured = errors.New("alerts are not configured")

// errTelegramSecret means the bot token secret of the last applied manifest's Telegram channel is not stored.
var errTelegramSecret = errors.New(monitor.MissingTelegramSecret)

// errAlertConfig means the last applied manifest could not be read, so the alert settings are unknown.
var errAlertConfig = errors.New("alerts configuration unavailable")

// Monitoring runs the agent's health checks. The checks come from the last applied manifest and from the built-in
// conditions, and their states are stored in the agent's state. Monitoring needs state, so NewMonitoring returns nil
// without it.
type Monitoring struct {
	cfg     Config
	store   *state.Store
	engine  *engine.Engine
	metrics *metrics.Collector
	deploy  *deploy.Controller
	watch   *monitor.RestartWatch
	mon     *monitor.Monitor
	// restarts holds the restart checks of the last successful inventory, kept while Docker cannot be asked.
	restarts []monitor.Check
	// root is Docker's data root once Docker has reported it. Only the monitor's goroutine uses it.
	root string
}

// NewMonitoring returns the monitoring of an agent with state, or nil for an agent without state. log receives
// operational messages, which never contain secrets.
func NewMonitoring(c Config, e *engine.Engine, col *metrics.Collector, d *deploy.Controller, log func(string)) *Monitoring {
	if d == nil || d.Store == nil {
		return nil
	}
	w := &Monitoring{cfg: c, store: d.Store, engine: e, metrics: col, deploy: d, watch: monitor.NewRestartWatch(nil)}
	w.mon = &monitor.Monitor{Env: c.Environment, Store: d.Store, Checks: w.checks, Notifier: w.notifier, Log: log}
	return w
}

// Run runs the monitor until ctx ends. It does nothing on a nil Monitoring.
func (w *Monitoring) Run(ctx context.Context) {
	if w != nil {
		w.mon.Run(ctx)
	}
}

// checks returns the checks that should run now. A nil result keeps the checks that are running, which the monitor does
// when the manifest cannot be read, or when the first host sample has not been taken yet: the disk checks cannot be
// named until then, so the monitor asks again soon.
func (w *Monitoring) checks(ctx context.Context) []monitor.Check {
	if _, ok := w.samples(); !ok && w.metrics != nil {
		return nil
	}
	m, _, manifestErr := w.store.LatestManifest(ctx)
	if manifestErr != nil && !errors.Is(manifestErr, sql.ErrNoRows) {
		return nil
	}
	_, ingressErr := w.store.Ingress(ctx)
	if ingressErr != nil && !errors.Is(ingressErr, sql.ErrNoRows) {
		return nil
	}
	var mp *manifest.Manifest
	if manifestErr == nil {
		mp = &m
	}
	out := w.builtins(ctx, ingressErr == nil, mp)
	if mp != nil {
		out = append(out, w.manifestChecks(*mp)...)
	}
	return out
}

// builtins returns the conditions that always run: disk and memory from the host sample, Docker's data disk, restart
// loops of the apps Berthy manages, and the managed tunnel when managed is true. m is the last applied manifest, or nil.
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
	out = append(out, w.restartChecks(ctx, m)...)
	if managed {
		out = append(out, monitor.TunnelCheck(w.tunnelStatus))
	}
	return out
}

// manifestChecks returns the probes, the peer checks, and the alert configuration of the last applied manifest.
func (w *Monitoring) manifestChecks(m manifest.Manifest) []monitor.Check {
	var out []monitor.Check
	for _, name := range slices.Sorted(maps.Keys(m.Probes)) {
		p := m.Probes[name]
		interval, timeout := p.Durations()
		failures := p.FailureLimit()
		switch {
		case p.HTTP != "":
			out = append(out, monitor.HTTPProbe("probe:"+name, p.HTTP, p.ExpectStatus, interval, timeout, failures))
		case p.TCP != "":
			out = append(out, monitor.TCPProbe("probe:"+name, p.TCP, interval, timeout, failures))
		default:
			out = append(out, monitor.ServiceProbe("probe:"+name, interval, failures))
		}
	}
	for _, name := range slices.Sorted(maps.Keys(m.Peers)) {
		out = append(out, monitor.PeerCheck("peer:"+name, m.Peers[name]))
	}
	if m.Alerts != nil && m.Alerts.Telegram != nil {
		secret := m.Alerts.Telegram.BotTokenSecret
		out = append(out, monitor.AlertSecretCheck(func(ctx context.Context) bool { return w.secretStored(ctx, secret) }))
	}
	return out
}

// restartChecks returns the restart check of every role of the apps Berthy manages. While the inventory or the task list
// cannot be read, the checks of the last good read keep running, and their results turn unknown as they age.
func (w *Monitoring) restartChecks(ctx context.Context, m *manifest.Manifest) []monitor.Check {
	apps, err := w.inventory(ctx)
	if err != nil {
		return w.previousRestartChecks(ctx)
	}
	now := time.Now()
	failures, err := w.taskFailures(ctx, now)
	if err != nil {
		return w.previousRestartChecks(ctx)
	}
	managed := managedRestartApps(apps, m)
	w.watch.Observe(now, managed, failures)
	var out []monitor.Check
	for _, app := range managed {
		for _, role := range app.Roles {
			out = append(out, w.watch.Check(app.Ref, role.Name))
		}
	}
	w.restarts = out
	return out
}

// previousRestartChecks are the restart checks to keep while the inventory cannot be read: those of the last good read,
// or, after a restart of the agent, placeholders for the stored restart checks that report unknown.
func (w *Monitoring) previousRestartChecks(ctx context.Context) []monitor.Check {
	if len(w.restarts) > 0 {
		return w.restarts
	}
	return w.staleRestartChecks(ctx)
}

// managedRestartApps keeps the apps Berthy manages: image apps, which appear in the inventory only through their bindings
// (the ingress connector among them), and Compose apps of the last applied manifest, matched by project name. Unmanaged
// Swarm services and Compose projects that no manifest names are left out.
func managedRestartApps(apps []api.App, m *manifest.Manifest) []api.App {
	projects := map[string]bool{}
	if m != nil {
		for name, a := range m.Apps {
			if a.Kind == "compose" {
				projects[m.ComposeProject(name)] = true
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

// staleRestartChecks keeps the stored restart checks of an earlier run in place, unknown, when no inventory has been read
// since the agent started. Their stored states survive, so a restart does not send their alerts again.
func (w *Monitoring) staleRestartChecks(ctx context.Context) []monitor.Check {
	stored, err := w.store.Checks(ctx)
	if err != nil {
		return nil
	}
	var out []monitor.Check
	for _, s := range stored {
		if strings.HasPrefix(s.Name, "restarts:") {
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

// secretStored reports whether a plain secret with this name is stored. A registry credential does not count.
func (w *Monitoring) secretStored(ctx context.Context, name string) bool {
	return plainSecretStored(ctx, w.store, name)
}

func plainSecretStored(ctx context.Context, s *state.Store, name string) bool {
	secrets, err := s.Secrets(ctx)
	if err != nil {
		return false
	}
	for _, info := range secrets {
		if info.Name == name && info.Registry == "" {
			return true
		}
	}
	return false
}

// notifier is the monitor's alert sender. It returns nil when no alerts are configured.
func (w *Monitoring) notifier(ctx context.Context) (monitor.Notifier, time.Duration, error) {
	tg, reminder, err := telegramFor(ctx, w.cfg, w.store)
	if err != nil || tg == nil {
		return nil, 0, err
	}
	return tg, reminder, nil
}

// telegramFor returns the Telegram sender of the last applied manifest and its reminder interval. It returns a nil sender
// when no alerts are configured, errTelegramSecret when the bot token secret is not stored, and errAlertConfig when the
// manifest cannot be read.
func telegramFor(ctx context.Context, c Config, s *state.Store) (*monitor.Telegram, time.Duration, error) {
	m, _, err := s.LatestManifest(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, errAlertConfig
	}
	if m.Alerts == nil || m.Alerts.Telegram == nil {
		return nil, 0, nil
	}
	t := m.Alerts.Telegram
	info, token, err := s.Secret(ctx, t.BotTokenSecret)
	if err != nil || info.Registry != "" || len(token) == 0 {
		return nil, 0, errTelegramSecret
	}
	return &monitor.Telegram{BaseURL: c.TelegramAPIURL, Token: string(token), ChatID: t.ChatID}, t.ReminderInterval(), nil
}

// sendTestAlert sends one test message through the configured Telegram channel.
func sendTestAlert(ctx context.Context, c Config, s *state.Store) error {
	tg, _, err := telegramFor(ctx, c, s)
	if errors.Is(err, errTelegramSecret) {
		return errAlertsNotConfigured
	}
	if err != nil {
		return err
	}
	if tg == nil {
		return errAlertsNotConfigured
	}
	return tg.Notify(ctx, fmt.Sprintf("🧪 %s test alert from berthy", c.Environment))
}

// alertError answers a failed test alert. The messages name only the outcome, never the URL or the token.
func alertError(w http.ResponseWriter, err error) {
	var status *monitor.TelegramStatusError
	switch {
	case errors.Is(err, errAlertsNotConfigured):
		writeError(w, http.StatusBadRequest, "alerts_not_configured", "alerts are not configured")
	case errors.Is(err, monitor.ErrTelegramUnavailable):
		writeError(w, http.StatusBadRequest, "telegram_unavailable", "telegram unavailable")
	case errors.As(err, &status):
		writeError(w, http.StatusBadRequest, "telegram_rejected", status.Error())
	default:
		writeError(w, http.StatusServiceUnavailable, "alerts_unavailable", "alerts unavailable")
	}
}

// monitoringChecks returns the doctor's non-critical checks of the last applied manifest: one per peer, and the bot token
// secret when alerts need it. A peer's stored state is used when the monitor has one; otherwise the peer is asked now.
func monitoringChecks(ctx context.Context, s *state.Store) []api.Check {
	m, _, err := s.LatestManifest(ctx)
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
			out[i] = peerDoctorCheck(ctx, name, m.Peers[name], states)
		}()
	}
	wg.Wait()
	if m.Alerts != nil && m.Alerts.Telegram != nil {
		present := plainSecretStored(ctx, s, m.Alerts.Telegram.BotTokenSecret)
		detail := "bot token secret present"
		if !present {
			detail = monitor.MissingTelegramSecret
		}
		out = append(out, api.Check{Name: "telegram", OK: present, Detail: detail})
	}
	return out
}

// peerDoctorCheck reports one peer for the doctor.
func peerDoctorCheck(ctx context.Context, name, base string, states map[string]state.CheckState) api.Check {
	c := api.Check{Name: "peer:" + name}
	if st, ok := states["peer:"+name]; ok && st.State != monitor.Unknown && st.State != "" {
		c.OK = st.State == monitor.OK
		c.Detail = st.Detail
		return c
	}
	pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	res := monitor.ProbePeer(pctx, base, 3*time.Second)
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
