package engine

import (
	"berthy/internal/api"
	"berthy/internal/metrics"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

type Engine struct {
	client *http.Client
	socket string
}

// inspectWorkers is the number of concurrent container inspects in fillDetails.
// It also sizes the idle connection pool per host so each worker can reuse a
// connection. The metrics collector's four concurrent stats calls fit within it too.
const inspectWorkers = 8

func New(socket string) *Engine {
	if socket == "" {
		socket = "/var/run/docker.sock"
	}
	tr := &http.Transport{Proxy: nil, MaxIdleConnsPerHost: inspectWorkers, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	return &Engine{client: &http.Client{Transport: tr, Timeout: 5 * time.Second}, socket: socket}
}
func (e *Engine) Get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, "GET", "http://docker"+func() string {
		if path == "/version" {
			return ""
		}
		return "/v1.44"
	}()+path, nil)
	if err != nil {
		return err
	}
	resp, err := e.client.Do(req)
	if err != nil {
		return fmt.Errorf("Docker unavailable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return &StatusError{Code: resp.StatusCode}
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

type StatusError struct{ Code int }

func (e *StatusError) Error() string { return fmt.Sprintf("Docker API status %d", e.Code) }

// PublishedPort is a port that a running container publishes on the host: the host address it is bound to, the host port and
// the protocol.
type PublishedPort struct {
	IP         string
	PublicPort uint16
	Type       string
}

// PublishedPorts returns the ports that the running containers publish on the host. A port that a container exposes without
// publishing has no PublicPort, and is left out.
func (e *Engine) PublishedPorts(ctx context.Context) ([]PublishedPort, error) {
	var containers []struct {
		Ports []PublishedPort
	}
	if err := e.Get(ctx, "/containers/json", &containers); err != nil {
		return nil, err
	}
	var out []PublishedPort
	for _, c := range containers {
		for _, p := range c.Ports {
			if p.PublicPort != 0 {
				out = append(out, p)
			}
		}
	}
	return out, nil
}

type container struct {
	ID, Image, State string
	Labels           map[string]string
}
type service struct {
	ID   string
	Spec struct {
		Name string
		Mode struct{ Replicated *struct{ Replicas uint64 } }
	}
}
type task struct {
	ID, ServiceID, DesiredState string
	Spec                        struct{ ContainerSpec struct{ Image string } }
	Status                      struct {
		State           string
		ContainerStatus struct{ ContainerID string }
	}
}
type inspect struct {
	RestartCount int
	State        struct {
		OOMKilled bool
		Health    *struct{ Status string }
	}
}

func (e *Engine) Apps(ctx context.Context) ([]api.App, error) {
	var cs []container
	if err := e.Get(ctx, "/containers/json?all=1", &cs); err != nil {
		return nil, err
	}
	var ss []service
	if err := e.Get(ctx, "/services", &ss); err != nil {
		var info struct {
			Swarm struct{ ControlAvailable bool }
		}
		if x := e.Get(ctx, "/info", &info); x != nil {
			return nil, x
		}
		if !info.Swarm.ControlAvailable {
			return nil, fmt.Errorf("swarm_manager_unavailable")
		}
		return nil, err
	}
	var tasks []task
	if len(ss) > 0 {
		if err := e.Get(ctx, "/tasks", &tasks); err != nil {
			return nil, err
		}
	}
	apps := map[string]*api.App{}
	localTasks := map[string]string{}
	for _, c := range cs {
		if id := c.Labels["com.docker.swarm.task.id"]; id != "" {
			localTasks[id] = c.ID
		}
	}
	for _, s := range ss {
		a := &api.App{Ref: api.AppRef{Kind: "swarm", Name: s.Spec.Name}, Roles: []api.Role{{Name: s.Spec.Name, Replicas: []api.Replica{}}}}
		if s.Spec.Mode.Replicated != nil {
			a.Roles[0].Desired = &s.Spec.Mode.Replicated.Replicas
		}
		apps["swarm:"+s.Spec.Name] = a
		for _, t := range tasks {
			if t.ServiceID != s.ID || !activeTask(t) {
				continue
			}
			r := api.Replica{TaskID: t.ID, ContainerID: t.Status.ContainerStatus.ContainerID, Image: t.Spec.ContainerSpec.Image, State: t.Status.State, Health: "unknown"}
			if r.ContainerID == "" {
				r.ContainerID = localTasks[t.ID]
			}
			if r.State == "running" {
				a.Roles[0].Running++
			}
			a.Roles[0].Replicas = append(a.Roles[0].Replicas, r)
		}
	}
	for _, c := range cs {
		p, n := c.Labels["com.docker.compose.project"], c.Labels["com.docker.compose.service"]
		if p == "" || n == "" || c.Labels["com.docker.compose.oneoff"] == "True" || c.Labels["com.docker.compose.oneoff"] == "true" {
			continue
		}
		key := "compose:" + p
		a := apps[key]
		if a == nil {
			a = &api.App{Ref: api.AppRef{Kind: "compose", Name: p}, Roles: []api.Role{}}
			apps[key] = a
		}
		i := -1
		for j := range a.Roles {
			if a.Roles[j].Name == n {
				i = j
				break
			}
		}
		if i < 0 {
			a.Roles = append(a.Roles, api.Role{Name: n, Replicas: []api.Replica{}})
			i = len(a.Roles) - 1
		}
		r := api.Replica{ContainerID: c.ID, Image: c.Image, State: c.State, Health: "unknown"}
		if c.State == "running" {
			a.Roles[i].Running++
		}
		a.Roles[i].Replicas = append(a.Roles[i].Replicas, r)
	}
	// Pointers are taken only now, after every append, so they cannot go stale.
	var targets []*api.Replica
	for _, a := range apps {
		for ri := range a.Roles {
			for pi := range a.Roles[ri].Replicas {
				targets = append(targets, &a.Roles[ri].Replicas[pi])
			}
		}
	}
	if err := e.fillDetails(ctx, targets); err != nil {
		return nil, err
	}
	out := make([]api.App, 0, len(apps))
	for _, a := range apps {
		sort.Slice(a.Roles, func(i, j int) bool { return a.Roles[i].Name < a.Roles[j].Name })
		out = append(out, *a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ref.Kind+out[i].Ref.Name < out[j].Ref.Kind+out[j].Ref.Name })
	return out, nil
}

// activeTask keeps tasks that are running (even if being retired during a rollout) or about to run.
func activeTask(t task) bool {
	switch t.Status.State {
	case "complete", "failed", "shutdown", "rejected", "orphaned", "remove":
		return false
	}
	return t.Status.State == "running" || t.DesiredState == "running" || t.DesiredState == "ready"
}

// fillDetails inspects every target with a fixed pool of workers. Each worker
// writes only the replica it was handed, so no locking is needed. The first
// error wins and cancels the inspects still in flight. Targets are still handed
// out after a failure or a dead context: details then fails fast in e.Get, so a
// parent deadline is reported as an error instead of being skipped.
func (e *Engine) fillDetails(ctx context.Context, targets []*api.Replica) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan *api.Replica)
	var (
		wg       sync.WaitGroup
		once     sync.Once
		firstErr error
	)
	for i := 0; i < inspectWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for r := range jobs {
				if err := e.details(ctx, r); err != nil {
					once.Do(func() {
						firstErr = err
						cancel()
					})
				}
			}
		}()
	}
	for _, r := range targets {
		jobs <- r
	}
	close(jobs)
	wg.Wait()
	return firstErr
}
func (e *Engine) details(ctx context.Context, r *api.Replica) error {
	if r.ContainerID == "" {
		return nil
	}
	var d inspect
	if err := e.Get(ctx, "/containers/"+url.PathEscape(r.ContainerID)+"/json", &d); err != nil {
		if status, ok := err.(*StatusError); ok && status.Code == 404 {
			return nil
		}
		return err
	} else {
		r.Local = true
		r.RestartCount = &d.RestartCount
		r.OOMKilled = &d.State.OOMKilled
		if d.State.Health == nil {
			r.Health = "none"
		} else {
			r.Health = d.State.Health.Status
		}
	}
	return nil
}
func Select(apps []api.App, name string) (api.App, int) {
	var found []api.App
	for _, a := range apps {
		if name == a.Ref.Kind+":"+a.Ref.Name || (!strings.Contains(name, ":") && name == a.Ref.Name) {
			found = append(found, a)
		}
	}
	if len(found) == 0 {
		return api.App{}, 404
	}
	if len(found) > 1 {
		return api.App{}, 409
	}
	return found[0], 200
}

func (e *Engine) Containers(ctx context.Context) ([]string, error) {
	var cs []struct{ ID string }
	if err := e.Get(ctx, "/containers/json", &cs); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(cs))
	for _, c := range cs {
		ids = append(ids, c.ID)
	}
	return ids, nil
}
func (e *Engine) Stats(ctx context.Context, id string) (metrics.RawContainer, error) {
	var v struct {
		CPUStats struct {
			CPUUsage struct {
				TotalUsage  *uint64  `json:"total_usage"`
				PercpuUsage []uint64 `json:"percpu_usage"`
			} `json:"cpu_usage"`
			SystemCPUUsage *uint64 `json:"system_cpu_usage"`
			OnlineCPUs     uint32  `json:"online_cpus"`
		} `json:"cpu_stats"`
		MemoryStats struct {
			Usage, Limit *uint64
			Stats        map[string]uint64
		} `json:"memory_stats"`
		Networks map[string]struct {
			RxBytes uint64 `json:"rx_bytes"`
			TxBytes uint64 `json:"tx_bytes"`
		}
		BlkioStats struct {
			IOServiceBytesRecursive []struct {
				Op    string
				Value uint64
			} `json:"io_service_bytes_recursive"`
		} `json:"blkio_stats"`
	}
	if err := e.Get(ctx, "/containers/"+url.PathEscape(id)+"/stats?stream=false&one-shot=true", &v); err != nil {
		return metrics.RawContainer{}, err
	}
	if v.CPUStats.CPUUsage.TotalUsage == nil || v.CPUStats.SystemCPUUsage == nil || v.MemoryStats.Usage == nil || v.MemoryStats.Limit == nil {
		return metrics.RawContainer{}, fmt.Errorf("incomplete Docker stats")
	}
	r := metrics.RawContainer{CPUUsage: *v.CPUStats.CPUUsage.TotalUsage, SystemUsage: *v.CPUStats.SystemCPUUsage, OnlineCPUs: v.CPUStats.OnlineCPUs, PerCPUs: uint32(len(v.CPUStats.CPUUsage.PercpuUsage)), MemoryUsage: *v.MemoryStats.Usage, MemoryLimit: *v.MemoryStats.Limit}
	if cache, ok := v.MemoryStats.Stats["total_inactive_file"]; ok {
		r.Cache = cache
	} else {
		r.Cache = v.MemoryStats.Stats["inactive_file"]
	}
	for _, n := range v.Networks {
		r.NetworkRXBytes += n.RxBytes
		r.NetworkTXBytes += n.TxBytes
	}
	for _, b := range v.BlkioStats.IOServiceBytesRecursive {
		switch strings.ToLower(b.Op) {
		case "read":
			r.BlockReadBytes += b.Value
		case "write":
			r.BlockWriteBytes += b.Value
		}
	}
	return r, nil
}

// NodeID returns the Swarm node ID of the node the daemon runs on. It fails when the node is not in a swarm.
func (e *Engine) NodeID(ctx context.Context) (string, error) {
	var info struct{ Swarm struct{ NodeID string } }
	if err := e.Get(ctx, "/info", &info); err != nil {
		return "", err
	}
	if info.Swarm.NodeID == "" {
		return "", fmt.Errorf("node is not in a swarm")
	}
	return info.Swarm.NodeID, nil
}

// LoggingDriver returns the daemon's default log driver, as /info reports it ("json-file", "local", "journald", ...). A
// daemon that reports none is an error, so a caller never takes a missing driver for Docker's default.
func (e *Engine) LoggingDriver(ctx context.Context) (string, error) {
	var info struct{ LoggingDriver string }
	if err := e.Get(ctx, "/info", &info); err != nil {
		return "", err
	}
	if info.LoggingDriver == "" {
		return "", fmt.Errorf("daemon reports no log driver")
	}
	return info.LoggingDriver, nil
}
