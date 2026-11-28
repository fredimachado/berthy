package telemetry

import (
	"context"
	"math"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"berthy/internal/metrics"
)

// instruments are the observable instruments of one meter provider. Each is read at export time from the collector's
// latest sample and the stored check states, so no value is recorded twice.
type instruments struct {
	cpuUtilization        metric.Float64ObservableGauge
	memoryUsage           metric.Int64ObservableGauge
	memoryLimit           metric.Int64ObservableGauge
	pagingUsage           metric.Int64ObservableGauge
	loadAverage1m         metric.Float64ObservableGauge
	loadAverage5m         metric.Float64ObservableGauge
	loadAverage15m        metric.Float64ObservableGauge
	filesystemUsage       metric.Int64ObservableGauge
	filesystemInodesTotal metric.Int64ObservableGauge
	filesystemInodesFree  metric.Int64ObservableGauge
	diskIO                metric.Int64ObservableCounter
	networkIO             metric.Int64ObservableCounter
	containerCPU          metric.Float64ObservableGauge
	containerMemory       metric.Int64ObservableGauge
	containerLimit        metric.Int64ObservableGauge
	containerNetwork      metric.Int64ObservableCounter
	containerDisk         metric.Int64ObservableCounter
	checkState            metric.Int64ObservableGauge
}

// newInstruments creates the instruments on meter. It returns them with the same instruments as observables, for a callback.
func newInstruments(meter metric.Meter) (*instruments, []metric.Observable, error) {
	var in instruments
	var err error
	if in.cpuUtilization, err = meter.Float64ObservableGauge("system.cpu.utilization", metric.WithUnit("1"), metric.WithDescription("Fraction of CPU time in use across all CPUs")); err != nil {
		return nil, nil, err
	}
	if in.memoryUsage, err = meter.Int64ObservableGauge("system.memory.usage", metric.WithUnit("By"), metric.WithDescription("Memory in use (state used) or available (state free)")); err != nil {
		return nil, nil, err
	}
	if in.memoryLimit, err = meter.Int64ObservableGauge("system.memory.limit", metric.WithUnit("By"), metric.WithDescription("Total physical memory")); err != nil {
		return nil, nil, err
	}
	if in.pagingUsage, err = meter.Int64ObservableGauge("system.paging.usage", metric.WithUnit("By"), metric.WithDescription("Swap in use (state used) or free (state free)")); err != nil {
		return nil, nil, err
	}
	if in.loadAverage1m, err = meter.Float64ObservableGauge("system.cpu.load_average.1m", metric.WithUnit("{thread}"), metric.WithDescription("Load average over one minute")); err != nil {
		return nil, nil, err
	}
	if in.loadAverage5m, err = meter.Float64ObservableGauge("system.cpu.load_average.5m", metric.WithUnit("{thread}"), metric.WithDescription("Load average over five minutes")); err != nil {
		return nil, nil, err
	}
	if in.loadAverage15m, err = meter.Float64ObservableGauge("system.cpu.load_average.15m", metric.WithUnit("{thread}"), metric.WithDescription("Load average over fifteen minutes")); err != nil {
		return nil, nil, err
	}
	if in.filesystemUsage, err = meter.Int64ObservableGauge("system.filesystem.usage", metric.WithUnit("By"), metric.WithDescription("Bytes used (state used) or available to users (state free) on each mount")); err != nil {
		return nil, nil, err
	}
	if in.filesystemInodesTotal, err = meter.Int64ObservableGauge("berthy.filesystem.inodes.total", metric.WithUnit("{inode}"), metric.WithDescription("Total inodes on each mount")); err != nil {
		return nil, nil, err
	}
	if in.filesystemInodesFree, err = meter.Int64ObservableGauge("berthy.filesystem.inodes.free", metric.WithUnit("{inode}"), metric.WithDescription("Free inodes on each mount")); err != nil {
		return nil, nil, err
	}
	if in.diskIO, err = meter.Int64ObservableCounter("system.disk.io", metric.WithUnit("By"), metric.WithDescription("Bytes read and written by whole physical disks since boot")); err != nil {
		return nil, nil, err
	}
	if in.networkIO, err = meter.Int64ObservableCounter("system.network.io", metric.WithUnit("By"), metric.WithDescription("Bytes received and transmitted by physical interfaces since boot")); err != nil {
		return nil, nil, err
	}
	if in.containerCPU, err = meter.Float64ObservableGauge("container.cpu.utilization", metric.WithUnit("1"), metric.WithDescription("CPU use of a container; 1 is one full CPU")); err != nil {
		return nil, nil, err
	}
	if in.containerMemory, err = meter.Int64ObservableGauge("container.memory.usage", metric.WithUnit("By"), metric.WithDescription("Memory in use by a container, without its page cache")); err != nil {
		return nil, nil, err
	}
	if in.containerLimit, err = meter.Int64ObservableGauge("container.memory.limit", metric.WithUnit("By"), metric.WithDescription("Memory limit of a container")); err != nil {
		return nil, nil, err
	}
	if in.containerNetwork, err = meter.Int64ObservableCounter("container.network.io", metric.WithUnit("By"), metric.WithDescription("Bytes received and transmitted by a container")); err != nil {
		return nil, nil, err
	}
	if in.containerDisk, err = meter.Int64ObservableCounter("container.disk.io", metric.WithUnit("By"), metric.WithDescription("Block I/O bytes read and written by a container")); err != nil {
		return nil, nil, err
	}
	if in.checkState, err = meter.Int64ObservableGauge("berthy.check.state", metric.WithDescription("State of a check: ok 0, warning 1, critical 2, unknown -1")); err != nil {
		return nil, nil, err
	}
	observables := []metric.Observable{
		in.cpuUtilization, in.memoryUsage, in.memoryLimit, in.pagingUsage, in.loadAverage1m, in.loadAverage5m, in.loadAverage15m,
		in.filesystemUsage, in.filesystemInodesTotal, in.filesystemInodesFree, in.diskIO, in.networkIO, in.containerCPU, in.containerMemory, in.containerLimit, in.containerNetwork,
		in.containerDisk, in.checkState,
	}
	return &in, observables, nil
}

// register creates the instruments on meter and registers the callback that observes them.
func (e *Exporter) register(meter metric.Meter) error {
	in, observables, err := newInstruments(meter)
	if err != nil {
		return err
	}
	_, err = meter.RegisterCallback(func(ctx context.Context, o metric.Observer) error {
		e.observe(ctx, o, in)
		return nil
	}, observables...)
	return err
}

// observe records the latest host and container samples and the stored check states.
func (e *Exporter) observe(ctx context.Context, o metric.Observer, in *instruments) {
	if e.Collector != nil {
		if s, ok := e.Collector.Latest(); ok {
			var keep func(map[string]string) bool
			if e.Keep != nil {
				keep = e.Keep()
			}
			s = s.Only(keep)
			observeHost(o, in, s.Host)
			observeContainers(o, in, s.Containers)
		}
	}
	if e.Checks == nil {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	states, err := e.Checks(cctx)
	if err != nil {
		return
	}
	for _, c := range states {
		if v, ok := checkValue(c.State); ok {
			o.ObserveInt64(in.checkState, v, metric.WithAttributes(attribute.String("berthy.check", c.Name)))
		}
	}
}

// observeHost records the host sample. A value the sample does not have is left out rather than reported as zero.
func observeHost(o metric.Observer, in *instruments, h metrics.Host) {
	if h.CPUPercent != nil {
		o.ObserveFloat64(in.cpuUtilization, *h.CPUPercent/100)
	}
	if h.MemoryTotal != nil && h.MemoryAvailable != nil {
		total, available := *h.MemoryTotal, *h.MemoryAvailable
		o.ObserveInt64(in.memoryUsage, toInt64(used(total, available)), metric.WithAttributes(attribute.String("system.memory.state", "used")))
		o.ObserveInt64(in.memoryUsage, toInt64(available), metric.WithAttributes(attribute.String("system.memory.state", "free")))
		o.ObserveInt64(in.memoryLimit, toInt64(total))
	}
	if h.SwapTotal != nil && h.SwapFree != nil {
		total, free := *h.SwapTotal, *h.SwapFree
		o.ObserveInt64(in.pagingUsage, toInt64(used(total, free)), metric.WithAttributes(attribute.String("system.paging.state", "used")))
		o.ObserveInt64(in.pagingUsage, toInt64(free), metric.WithAttributes(attribute.String("system.paging.state", "free")))
	}
	if len(h.Load) > 0 {
		o.ObserveFloat64(in.loadAverage1m, h.Load[0])
	}
	if len(h.Load) > 1 {
		o.ObserveFloat64(in.loadAverage5m, h.Load[1])
	}
	if len(h.Load) > 2 {
		o.ObserveFloat64(in.loadAverage15m, h.Load[2])
	}
	for _, m := range h.Mounts {
		if m.Total == 0 {
			continue
		}
		mount := attribute.String("system.filesystem.mountpoint", m.Path)
		o.ObserveInt64(in.filesystemUsage, toInt64(used(m.Total, m.Available)), metric.WithAttributes(mount, attribute.String("system.filesystem.state", "used")))
		o.ObserveInt64(in.filesystemUsage, toInt64(m.Available), metric.WithAttributes(mount, attribute.String("system.filesystem.state", "free")))
		if m.InodesTotal > 0 {
			o.ObserveInt64(in.filesystemInodesTotal, toInt64(m.InodesTotal), metric.WithAttributes(mount))
			o.ObserveInt64(in.filesystemInodesFree, toInt64(m.InodesFree), metric.WithAttributes(mount))
		}
	}
	if h.DiskReadBytes != nil {
		o.ObserveInt64(in.diskIO, toInt64(*h.DiskReadBytes), metric.WithAttributes(attribute.String("disk.io.direction", "read")))
	}
	if h.DiskWriteBytes != nil {
		o.ObserveInt64(in.diskIO, toInt64(*h.DiskWriteBytes), metric.WithAttributes(attribute.String("disk.io.direction", "write")))
	}
	if h.NetworkRXBytes != nil {
		o.ObserveInt64(in.networkIO, toInt64(*h.NetworkRXBytes), metric.WithAttributes(attribute.String("network.io.direction", "receive")))
	}
	if h.NetworkTXBytes != nil {
		o.ObserveInt64(in.networkIO, toInt64(*h.NetworkTXBytes), metric.WithAttributes(attribute.String("network.io.direction", "transmit")))
	}
}

// observeContainers records each container that was sampled without error. Its ID is shortened to 12 characters, as
// Docker shows it. The inventory is not consulted, so no app or role is attached.
func observeContainers(o metric.Observer, in *instruments, containers []metrics.Container) {
	for _, c := range containers {
		if c.Error != "" {
			continue
		}
		id := attribute.String("container.id", shortID(c.ID))
		self := metric.WithAttributes(id)
		if c.CPUPercent != nil {
			o.ObserveFloat64(in.containerCPU, *c.CPUPercent/100, self)
		}
		o.ObserveInt64(in.containerMemory, toInt64(c.MemoryUsage), self)
		o.ObserveInt64(in.containerLimit, toInt64(c.MemoryLimit), self)
		o.ObserveInt64(in.containerNetwork, toInt64(c.NetworkRXBytes), metric.WithAttributes(id, attribute.String("network.io.direction", "receive")))
		o.ObserveInt64(in.containerNetwork, toInt64(c.NetworkTXBytes), metric.WithAttributes(id, attribute.String("network.io.direction", "transmit")))
		o.ObserveInt64(in.containerDisk, toInt64(c.BlockReadBytes), metric.WithAttributes(id, attribute.String("disk.io.direction", "read")))
		o.ObserveInt64(in.containerDisk, toInt64(c.BlockWriteBytes), metric.WithAttributes(id, attribute.String("disk.io.direction", "write")))
	}
}

// checkValue maps a stored check state to its gauge value.
func checkValue(state string) (int64, bool) {
	switch state {
	case "ok":
		return 0, true
	case "warning":
		return 1, true
	case "critical":
		return 2, true
	case "unknown":
		return -1, true
	}
	return 0, false
}

// used returns total minus free, or zero when free is larger, which a sample can report during a change.
func used(total, free uint64) uint64 {
	if free > total {
		return 0
	}
	return total - free
}

// toInt64 converts a count to the instrument's type, clamping values beyond its range.
func toInt64(v uint64) int64 {
	if v > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(v)
}

// shortID returns the first 12 characters of a container ID, or the whole ID when it is shorter.
func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
