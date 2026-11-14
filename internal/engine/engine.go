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
	"time"
)

type Engine struct {
	client *http.Client
	socket string
}

func New(socket string) *Engine {
	if socket == "" {
		socket = "/var/run/docker.sock"
	}
	tr := &http.Transport{Proxy: nil, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
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
		return fmt.Errorf("Docker API %s: %s", path, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(out)
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
	for _, s := range ss {
		a := &api.App{Ref: api.AppRef{Kind: "swarm", Name: s.Spec.Name}, Roles: []api.Role{{Name: s.Spec.Name, Replicas: []api.Replica{}}}}
		if s.Spec.Mode.Replicated != nil {
			a.Roles[0].Desired = &s.Spec.Mode.Replicated.Replicas
		}
		apps["swarm:"+s.Spec.Name] = a
		for _, t := range tasks {
			if t.ServiceID != s.ID || t.DesiredState != "running" {
				continue
			}
			r := api.Replica{TaskID: t.ID, ContainerID: t.Status.ContainerStatus.ContainerID, Image: t.Spec.ContainerSpec.Image, State: t.Status.State, Health: "unknown"}
			if r.State == "running" {
				a.Roles[0].Running++
			}
			e.details(ctx, &r)
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
		e.details(ctx, &r)
		a.Roles[i].Replicas = append(a.Roles[i].Replicas, r)
	}
	out := make([]api.App, 0, len(apps))
	for _, a := range apps {
		sort.Slice(a.Roles, func(i, j int) bool { return a.Roles[i].Name < a.Roles[j].Name })
		out = append(out, *a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ref.Kind+out[i].Ref.Name < out[j].Ref.Kind+out[j].Ref.Name })
	return out, nil
}
func (e *Engine) details(ctx context.Context, r *api.Replica) {
	if r.ContainerID == "" {
		return
	}
	var d inspect
	if e.Get(ctx, "/containers/"+url.PathEscape(r.ContainerID)+"/json", &d) == nil {
		r.RestartCount = &d.RestartCount
		r.OOMKilled = &d.State.OOMKilled
		if d.State.Health == nil {
			r.Health = "none"
		} else {
			r.Health = d.State.Health.Status
		}
	}
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
				TotalUsage  uint64   `json:"total_usage"`
				PercpuUsage []uint64 `json:"percpu_usage"`
			} `json:"cpu_usage"`
			SystemCPUUsage uint64 `json:"system_cpu_usage"`
			OnlineCPUs     uint32 `json:"online_cpus"`
		} `json:"cpu_stats"`
		MemoryStats struct {
			Usage, Limit uint64
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
	r := metrics.RawContainer{CPUUsage: v.CPUStats.CPUUsage.TotalUsage, SystemUsage: v.CPUStats.SystemCPUUsage, OnlineCPUs: v.CPUStats.OnlineCPUs, PerCPUs: uint32(len(v.CPUStats.CPUUsage.PercpuUsage)), MemoryUsage: v.MemoryStats.Usage, MemoryLimit: v.MemoryStats.Limit}
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
