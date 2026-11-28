package agent

import (
	"context"
	"fmt"
	"os"

	"berthy/internal/state"
)

// Cluster owns the agent's server state and alert configuration.
type Cluster struct {
	store    *state.Store
	alerts   *ClusterAlerts
	hostname string
	shared   Shared
}

// OpenCluster opens and claims the server state of a validated file.
func OpenCluster(sh Shared, f File) (*Cluster, error) {
	hostname, err := os.Hostname()
	if err != nil {
		return nil, fmt.Errorf("read server hostname: %w", err)
	}
	s, err := state.Open(f.Cluster.StateDir, f.Cluster.KeyFile, "cluster")
	if err != nil {
		return nil, fmt.Errorf("open cluster state: %w", err)
	}
	cluster := &Cluster{store: s, alerts: f.Cluster.Alerts, hostname: hostname, shared: sh}
	if err := s.Claim(context.Background(), "(cluster)"); err != nil {
		cluster.Close()
		return nil, fmt.Errorf("claim cluster state: %w", err)
	}
	return cluster, nil
}

// Close releases the cluster store.
func (c *Cluster) Close() {
	if c != nil {
		_ = c.store.Close() // No further operation can use the store during shutdown.
	}
}
