package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"berthy/internal/api"
	"berthy/internal/metrics"
	"berthy/internal/monitor"
	"berthy/internal/state"
)

const (
	ClusterProject     = "(cluster)"
	ClusterEnvironment = "cluster"
)

// Cluster owns the agent's server state and alert configuration.
type Cluster struct {
	store       *state.Store
	alerts      *ClusterAlerts
	alertConfig Config
	hostname    string
	shared      Shared
	samples     monitor.Samples
	root        string
	mon         *monitor.Monitor
	cancel      context.CancelFunc
	done        chan struct{}
}

// OpenCluster opens and claims the server state of a validated file.
func OpenCluster(sh Shared, f File) (*Cluster, error) {
	hostname, err := os.Hostname()
	if err != nil {
		return nil, fmt.Errorf("read server hostname: %w", err)
	}
	s, err := state.Open(f.Cluster.StateDir, f.Cluster.KeyFile, ClusterEnvironment)
	if err != nil {
		return nil, fmt.Errorf("open cluster state: %w", err)
	}
	cluster := &Cluster{
		store: s, alerts: f.Cluster.Alerts, alertConfig: Config{TelegramAPIURL: f.TelegramAPIURL, Listen: f.Listen}, hostname: hostname, shared: sh,
	}
	if sh.Collector != nil {
		cluster.samples = sh.Collector.Latest
	}
	if err := s.Claim(context.Background(), ClusterProject); err != nil {
		cluster.Close()
		return nil, fmt.Errorf("claim cluster state: %w", err)
	}
	return cluster, nil
}

// ChannelChecks reports whether the server can use each configured alert channel.
func (c *Cluster) ChannelChecks(ctx context.Context) []api.Check {
	if c.alerts == nil || c.alerts.Telegram == nil && c.alerts.Webhook == nil {
		return []api.Check{{Name: "cluster_alerts", Detail: "no cluster alert channel: host problems alert nobody; run berthy setup --cluster-alerts FILE"}}
	}
	checks := make([]api.Check, 0, 2)
	for _, channel := range c.publicAlertChannels(ctx) {
		detail := channel.okDetail
		if channel.err != nil {
			detail = channel.err.Error()
		}
		checks = append(checks, api.Check{Name: "cluster_" + channel.name, OK: channel.err == nil, Detail: detail})
	}
	return checks
}

// publicAlertChannels keeps server credential names out of the environment's read API and monitor detail.
func (c *Cluster) publicAlertChannels(ctx context.Context) []alertChannel {
	channels := alertChannels(ctx, c.alertConfig, c.store, c.alerts.manifestAlerts())
	for i := range channels {
		var named namedAlertCredentialError
		if errors.As(channels[i].err, &named) {
			channels[i].err = errors.New(named.public)
		}
	}
	return channels
}

type clusterCheckRow struct {
	Server string    `json:"server"`
	Name   string    `json:"name"`
	State  string    `json:"state"`
	Detail string    `json:"detail"`
	Since  time.Time `json:"since"`
}

// CheckRows returns the persisted cluster checks, in name order.
func (c *Cluster) CheckRows(ctx context.Context) ([]clusterCheckRow, error) {
	rows, err := c.store.Checks(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]clusterCheckRow, 0, len(rows))
	for _, row := range rows {
		server := c.hostname
		if row.Name == "telegram" || row.Name == "webhook" {
			server = ""
		}
		out = append(out, clusterCheckRow{Server: server, Name: row.Name, State: row.State, Detail: row.Detail, Since: row.Since})
	}
	return out, nil
}

// Start runs the server monitor until the host shuts down.
func (c *Cluster) Start(ctx context.Context) {
	if c == nil || c.done != nil {
		return
	}
	ctx, c.cancel = context.WithCancel(ctx)
	c.done = make(chan struct{})
	c.mon = &monitor.Monitor{Server: c.hostname, Store: c.store, Checks: c.checks, Notifier: c.notifier, Log: c.shared.Log}
	go func() {
		defer close(c.done)
		c.mon.Run(ctx)
	}()
}

func (c *Cluster) checks(ctx context.Context) []monitor.Check {
	samples := c.samples
	if samples == nil {
		samples = func() (metrics.Sample, bool) { return metrics.Sample{}, false }
	}
	sample, ok := samples()
	if !ok && c.samples != nil {
		return nil // The monitor retries once the collector can name the disks.
	}
	var mounts []string
	for _, mount := range sample.Host.Mounts {
		mounts = append(mounts, mount.Path)
	}
	root := c.dockerDataRoot(ctx)
	holder := monitor.MountFor(root, mounts)
	var out []monitor.Check
	for _, path := range monitor.DiskMounts(sample.Host.Mounts, root) {
		out = append(out, monitor.DiskCheck(samples, path, path == holder))
	}
	// Once Docker reports its data root, its mount's disk check carries the Docker label.
	if root == "" {
		out = append(out, monitor.DockerDiskCheck(samples, ""))
	}
	out = append(out, monitor.MemoryCheck(samples))
	out = append(out, hostChecks(c.shared.Engine, c.alertConfig.Listen)...)
	if c.alerts != nil {
		for _, channel := range c.publicAlertChannels(ctx) {
			out = append(out, monitor.AlertSecretCheck(channel.name, func(ctx context.Context) (bool, string) {
				return alertChannelStatus(c.publicAlertChannels(ctx), channel.name)
			}))
		}
	}
	return out
}

// WriteClusterSecret is the setup credential writer. The service must be stopped to release the store.
func WriteClusterSecret(ctx context.Context, c ClusterConfig, name string, value []byte) error {
	if _, err := os.Stat(filepath.Join(c.StateDir, "state.db")); err != nil {
		return err
	}
	s, err := state.Open(c.StateDir, c.KeyFile, ClusterEnvironment)
	if err != nil {
		return err
	}
	defer s.Close() // Release the owner lock before systemd starts the service.
	if err := s.CheckOwner(ctx, ClusterProject); err != nil {
		return err
	}
	return s.PutSecret(ctx, state.Actor{ID: "setup", Name: "setup"}, state.SecretInfo{Name: name}, value)
}

func (c *Cluster) dockerDataRoot(ctx context.Context) string {
	if c.root != "" || c.shared.Engine == nil {
		return c.root
	}
	dctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var info struct {
		DockerRootDir string
	}
	if err := c.shared.Engine.Get(dctx, "/info", &info); err == nil {
		c.root = info.DockerRootDir
	}
	return c.root
}

func (c *Cluster) notifier(ctx context.Context) (monitor.Notifier, time.Duration, error) {
	if c.alerts == nil {
		return nil, 0, nil
	}
	alerts := c.alerts.manifestAlerts()
	notifiers := alertNotifiers(ctx, c.alertConfig, c.store, alerts)
	if len(notifiers) == 0 {
		return nil, 0, nil
	}
	return notifiers, alerts.ReminderInterval(), nil
}

// Close stops the monitor before releasing its store.
func (c *Cluster) Close() {
	if c != nil {
		if c.done != nil {
			c.cancel()
			<-c.done
		}
		_ = c.store.Close() // No further operation can use the store during shutdown.
	}
}
