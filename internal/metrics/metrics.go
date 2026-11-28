package metrics

import (
	"context"
	"maps"
	"sort"
	"sync"
	"time"
)

const interval = 10 * time.Second

type HostReader interface{ Read(context.Context) Host }

// ContainerRef is a running container the collector samples: its id and the labels Docker lists for it.
type ContainerRef struct {
	ID     string
	Labels map[string]string
}

type DockerReader interface {
	Containers(context.Context) ([]ContainerRef, error)
	Stats(context.Context, string) (RawContainer, error)
}
type Mount struct {
	Path        string `json:"path"`
	Total       uint64 `json:"total"`
	Available   uint64 `json:"available"`
	InodesTotal uint64 `json:"inodes_total"`
	InodesFree  uint64 `json:"inodes_free"`
}
type Host struct {
	CPUPercent      *float64  `json:"cpu_percent"`
	MemoryTotal     *uint64   `json:"memory_total"`
	MemoryAvailable *uint64   `json:"memory_available"`
	SwapTotal       *uint64   `json:"swap_total"`
	SwapFree        *uint64   `json:"swap_free"`
	Load            []float64 `json:"load"`
	DiskReadBytes   *uint64   `json:"disk_read_bytes"`
	DiskWriteBytes  *uint64   `json:"disk_write_bytes"`
	NetworkRXBytes  *uint64   `json:"network_rx_bytes"`
	NetworkTXBytes  *uint64   `json:"network_tx_bytes"`
	Mounts          []Mount   `json:"mounts"`
	Errors          []string  `json:"errors,omitempty"`
}
type RawContainer struct {
	CPUUsage, SystemUsage                                           uint64
	OnlineCPUs, PerCPUs                                             uint32
	MemoryUsage, MemoryLimit, Cache                                 uint64
	NetworkRXBytes, NetworkTXBytes, BlockReadBytes, BlockWriteBytes uint64
}
type Container struct {
	ID              string            `json:"id"`
	CPUPercent      *float64          `json:"cpu_percent"`
	MemoryUsage     uint64            `json:"memory_usage"`
	MemoryLimit     uint64            `json:"memory_limit"`
	NetworkRXBytes  uint64            `json:"network_rx_bytes"`
	NetworkTXBytes  uint64            `json:"network_tx_bytes"`
	BlockReadBytes  uint64            `json:"block_read_bytes"`
	BlockWriteBytes uint64            `json:"block_write_bytes"`
	Error           string            `json:"error,omitempty"`
	Labels          map[string]string `json:"-"`
}
type Sample struct {
	Time            time.Time   `json:"time"`
	Host            Host        `json:"host"`
	Containers      []Container `json:"containers"`
	DockerError     string      `json:"docker_error,omitempty"`
	ContainersFresh bool        `json:"containers_fresh"`
}
type baseline struct {
	raw RawContainer
	at  time.Time
}
type Collector struct {
	host        HostReader
	docker      DockerReader
	now         func() time.Time
	mu          sync.Mutex
	samples     []Sample
	previous    map[string]baseline
	subscribers map[chan struct{}]struct{}
}

func New(host HostReader, docker DockerReader) *Collector {
	return &Collector{host: host, docker: docker, now: time.Now, previous: map[string]baseline{}, subscribers: map[chan struct{}]struct{}{}}
}
func (c *Collector) Run(ctx context.Context) {
	c.collect(ctx)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.collect(ctx)
		}
	}
}
func (c *Collector) collect(ctx context.Context) {
	now := c.now()
	s := Sample{Time: now, Host: c.host.Read(ctx)}
	next := map[string]baseline{}
	work, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	refs, err := c.docker.Containers(work)
	if err != nil {
		s.DockerError = err.Error()
	} else {
		sort.Slice(refs, func(i, j int) bool { return refs[i].ID < refs[j].ID })
		s.Containers = make([]Container, len(refs))
		s.ContainersFresh = true
		jobs := make(chan int)
		var wg sync.WaitGroup
		for i := 0; i < 4; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for n := range jobs {
					id := refs[n].ID
					raw, e := c.docker.Stats(work, id)
					item := Container{ID: id, Labels: maps.Clone(refs[n].Labels)}
					if e != nil {
						item.Error = e.Error()
					} else {
						item.MemoryUsage = raw.MemoryUsage
						if raw.Cache < raw.MemoryUsage {
							item.MemoryUsage -= raw.Cache
						}
						item.MemoryLimit = raw.MemoryLimit
						item.NetworkRXBytes = raw.NetworkRXBytes
						item.NetworkTXBytes = raw.NetworkTXBytes
						item.BlockReadBytes = raw.BlockReadBytes
						item.BlockWriteBytes = raw.BlockWriteBytes
						if old, ok := c.previous[id]; ok && now.Sub(old.at) > 0 && now.Sub(old.at) <= 2*interval && raw.CPUUsage >= old.raw.CPUUsage && raw.SystemUsage > old.raw.SystemUsage {
							cpus := raw.OnlineCPUs
							if cpus == 0 {
								cpus = raw.PerCPUs
							}
							if cpus > 0 {
								v := float64(raw.CPUUsage-old.raw.CPUUsage) / float64(raw.SystemUsage-old.raw.SystemUsage) * float64(cpus) * 100
								item.CPUPercent = &v
							}
						}
						c.mu.Lock()
						next[id] = baseline{raw, now}
						c.mu.Unlock()
					}
					s.Containers[n] = item
				}
			}()
		}
		for i := range refs {
			jobs <- i
		}
		close(jobs)
		wg.Wait()
		for _, item := range s.Containers {
			if item.Error != "" {
				s.ContainersFresh = false
			}
		}
	}
	c.previous = next
	c.mu.Lock()
	defer c.mu.Unlock()
	c.samples = append(c.samples, clone(s))
	if len(c.samples) > 360 {
		c.samples = c.samples[len(c.samples)-360:]
	}
	for ch := range c.subscribers {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}
func clone(s Sample) Sample {
	s.Host.Load = append([]float64(nil), s.Host.Load...)
	s.Host.Mounts = append([]Mount(nil), s.Host.Mounts...)
	s.Host.Errors = append([]string(nil), s.Host.Errors...)
	s.Containers = append([]Container(nil), s.Containers...) // copy pointed-to values so callers cannot mutate stored samples
	for i := range s.Containers {
		s.Containers[i].Labels = maps.Clone(s.Containers[i].Labels)
		if p := s.Containers[i].CPUPercent; p != nil {
			v := *p
			s.Containers[i].CPUPercent = &v
		}
	}
	if p := s.Host.CPUPercent; p != nil {
		v := *p
		s.Host.CPUPercent = &v
	}
	for _, p := range []**uint64{&s.Host.MemoryTotal, &s.Host.MemoryAvailable, &s.Host.SwapTotal, &s.Host.SwapFree, &s.Host.DiskReadBytes, &s.Host.DiskWriteBytes, &s.Host.NetworkRXBytes, &s.Host.NetworkTXBytes} {
		if *p != nil {
			v := **p
			*p = &v
		}
	}
	return s
}

// Only returns a copy of s whose containers are those keep accepts. A nil keep leaves every container. The host part is
// unchanged.
func (s Sample) Only(keep func(labels map[string]string) bool) Sample {
	if keep == nil {
		return s
	}
	kept := make([]Container, 0, len(s.Containers))
	for _, c := range s.Containers {
		if keep(c.Labels) {
			kept = append(kept, c)
		}
	}
	s.Containers = kept
	return s
}

func (c *Collector) Latest() (Sample, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.samples) == 0 {
		return Sample{}, false
	}
	return clone(c.samples[len(c.samples)-1]), true
}
func (c *Collector) Since(d time.Duration) []Sample {
	if d > time.Hour {
		d = time.Hour
	}
	cutoff := c.now().Add(-d)
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []Sample
	for _, s := range c.samples {
		if !s.Time.Before(cutoff) {
			out = append(out, clone(s))
		}
	}
	return out
}
func (c *Collector) Subscribe() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	c.mu.Lock()
	c.subscribers[ch] = struct{}{}
	c.mu.Unlock()
	var once sync.Once
	return ch, func() { once.Do(func() { c.mu.Lock(); delete(c.subscribers, ch); c.mu.Unlock() }) }
}
