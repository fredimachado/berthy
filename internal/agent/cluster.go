package agent

import (
	"context"
	"fmt"
	"os"
	"time"

	"berthy/internal/api"
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
		store: s, alerts: f.Cluster.Alerts, alertConfig: Config{TelegramAPIURL: f.TelegramAPIURL}, hostname: hostname, shared: sh,
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
	alerts := c.alerts.manifestAlerts()
	checks := make([]api.Check, 0, 2)
	for _, channel := range []struct {
		name       string
		configured bool
	}{{"telegram", alerts.Telegram != nil}, {"webhook", alerts.Webhook != nil}} {
		if channel.configured {
			ok, detail := alertChannelStatus(ctx, c.alertConfig, c.store, alerts, channel.name)
			checks = append(checks, api.Check{Name: "cluster_" + channel.name, OK: ok, Detail: detail})
		}
	}
	return checks
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

// Close releases the cluster store.
func (c *Cluster) Close() {
	if c != nil {
		_ = c.store.Close() // No further operation can use the store during shutdown.
	}
}
