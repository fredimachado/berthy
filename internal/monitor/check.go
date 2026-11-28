package monitor

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strings"
	"syscall"
	"time"

	"berthy/internal/metrics"
)

// Func is a Check whose run is a function.
type Func struct {
	name      string
	interval  time.Duration
	failures  int
	successes int
	run       func(context.Context) Result
}

// NewFunc returns a check named name that runs run every interval. failures is how many non-ok results in a row change
// its stored state; a value below one is treated as one.
func NewFunc(name string, interval time.Duration, failures int, run func(context.Context) Result) Check {
	if failures < 1 {
		failures = 1
	}
	return Func{name: name, interval: interval, failures: failures, successes: 1, run: run}
}

func newPressureCheck(name string, run func(context.Context) Result) Check {
	return Func{name: name, interval: 30 * time.Second, failures: 1, successes: 10, run: run}
}

func (f Func) Name() string            { return f.name }
func (f Func) Interval() time.Duration { return f.interval }
func (f Func) Failures() int           { return f.failures }
func (f Func) Successes() int          { return f.successes }
func (f Func) Run(ctx context.Context) Result {
	return f.run(ctx)
}

// probeUserAgent identifies the agent's probes to the services they check.
const probeUserAgent = "berthy-probe"

// DialFunc opens a network connection. Nil means a net.Dialer.
type DialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// HTTPProbe returns a probe that runs ProbeHTTP against url, with timeout as its deadline. A nil dial uses the default.
func HTTPProbe(name, url string, expect []int, interval, timeout time.Duration, failures int, dial DialFunc) Check {
	return NewFunc(name, interval, failures, func(ctx context.Context) Result {
		pctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		return ProbeHTTP(pctx, url, expect, dial)
	})
}

// ProbeHTTP sends GET to rawURL once, within ctx's deadline. It follows no redirects, so a 3xx is the status it sees. A
// status in expect is ok; without expect, any status from 200 to 399 is ok. Other statuses, timeouts and refused
// connections are critical. The response body is never read or reported. A nil dial uses the default.
func ProbeHTTP(ctx context.Context, rawURL string, expect []int, dial DialFunc) Result {
	transport := &http.Transport{Proxy: nil, DisableKeepAlives: true, DialContext: dial}
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Transport:     transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return Result{State: Critical, Detail: "invalid URL"}
	}
	req.Header.Set("User-Agent", probeUserAgent)
	resp, err := client.Do(req)
	if err != nil {
		return Result{State: Critical, Detail: FailureDetail(err)}
	}
	resp.Body.Close() //nolint:errcheck // The probe uses only the response status, not its body.
	detail := fmt.Sprintf("HTTP %d", resp.StatusCode)
	if statusExpected(resp.StatusCode, expect) {
		return Result{State: OK, Detail: detail}
	}
	return Result{State: Critical, Detail: detail}
}

// TCPProbe returns a probe that runs ProbeTCP against addr, with timeout as its deadline. A nil dial uses the default.
func TCPProbe(name, addr string, interval, timeout time.Duration, failures int, dial DialFunc) Check {
	return NewFunc(name, interval, failures, func(ctx context.Context) Result {
		pctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		return ProbeTCP(pctx, addr, dial)
	})
}

// ProbeTCP connects to addr (host:port) once, within ctx's deadline, and closes the connection. A failed connection is
// critical. A nil dial uses the default.
func ProbeTCP(ctx context.Context, addr string, dial DialFunc) Result {
	if dial == nil {
		dial = (&net.Dialer{}).DialContext
	}
	conn, err := dial(ctx, "tcp", addr)
	if err != nil {
		return Result{State: Critical, Detail: FailureDetail(err)}
	}
	conn.Close() //nolint:errcheck // The probe only checks whether the connection opens.
	return Result{State: OK, Detail: "connected"}
}

// PeerCheck returns the check for another agent, named by its base URL. It asks the peer's /healthz every 30 seconds
// with a 5-second timeout, and needs three misses in a row before it reports the peer silent. A nil dial uses the default.
func PeerCheck(name, base string, dial DialFunc) Check {
	return NewFunc(name, 30*time.Second, 3, func(ctx context.Context) Result {
		return ProbePeer(ctx, base, 5*time.Second, dial)
	})
}

// ProbePeer asks another agent for /healthz once. A 200 is reachable; any other answer, or none, is critical and silent.
// A nil dial uses the default.
func ProbePeer(ctx context.Context, base string, timeout time.Duration, dial DialFunc) Result {
	client := &http.Client{
		Timeout:       timeout,
		Transport:     &http.Transport{Proxy: nil, DisableKeepAlives: true, DialContext: dial},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/healthz", nil)
	if err != nil {
		return Result{State: Critical, Detail: "silent: invalid URL"}
	}
	req.Header.Set("User-Agent", probeUserAgent)
	resp, err := client.Do(req)
	if err != nil {
		return Result{State: Critical, Detail: "silent: " + FailureDetail(err)}
	}
	resp.Body.Close() //nolint:errcheck // The peer check uses only the response status.
	if resp.StatusCode != http.StatusOK {
		return Result{State: Critical, Detail: fmt.Sprintf("silent: HTTP %d", resp.StatusCode)}
	}
	return Result{State: OK, Detail: "reachable"}
}

// statusExpected reports whether code is one the probe expects.
func statusExpected(code int, expect []int) bool {
	if len(expect) == 0 {
		return code >= 200 && code <= 399
	}
	for _, want := range expect {
		if want == code {
			return true
		}
	}
	return false
}

// FailureDetail says why a probe got no answer. It never includes the error text, which can echo the address.
func FailureDetail(err error) string {
	var netErr net.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded) || errors.As(err, &netErr) && netErr.Timeout():
		return "timeout"
	case refused(err):
		return "connection refused"
	default:
		return "unreachable"
	}
}

// refused reports whether a dial was refused. The message is matched because Windows reports the refusal with its own
// error number; the match only classifies the failure and is never shown.
func refused(err error) bool {
	var op *net.OpError
	if !errors.As(err, &op) || op.Op != "dial" {
		return false
	}
	return errors.Is(err, syscall.ECONNREFUSED) || op.Err != nil && strings.Contains(strings.ToLower(op.Err.Error()), "refused")
}

// Samples returns the latest host sample, and false until the first one has been taken.
type Samples func() (metrics.Sample, bool)

// Disk thresholds: used space warns from 80% and is critical from 90%.
const (
	diskWarning  = 80.0
	diskCritical = 90.0
	inodeWarning = 95.0
)

// Memory thresholds: available memory warns below 10% and is critical below 5%.
const (
	memoryWarning  = 10.0
	memoryCritical = 5.0
)

// DiskCheck returns the check of one mount, named disk:<mount>. dockerData adds "(Docker data)" to the detail for the
// mount that holds Docker's data root, so the Docker check can be skipped.
func DiskCheck(samples Samples, mount string, dockerData bool) Check {
	label := ""
	if dockerData {
		label = " (Docker data)"
	}
	return newPressureCheck("disk:"+mount, func(context.Context) Result {
		return diskResult(samples, mount, label)
	})
}

// DockerDiskCheck returns the check of the mount that holds Docker's data root, named docker_disk. mount is that
// mount's path (see MountFor), or empty when it is not known yet, in which case the check reports unknown.
func DockerDiskCheck(samples Samples, mount string) Check {
	return newPressureCheck("docker_disk", func(context.Context) Result {
		if mount == "" {
			return Result{State: Unknown, Detail: "Docker data root unknown"}
		}
		return diskResult(samples, mount, " (Docker data)")
	})
}

// MinDiskSize is the size from which a mount's disk check runs. Smaller mounts are skipped, except "/" and the mount that
// holds Docker's data. Tiny pseudo and read-only mounts report themselves full, which says nothing about the server.
const MinDiskSize = 1 << 30

// DiskMounts returns the paths whose disk checks run, sorted: "/", the mount that holds dockerRoot when the sample
// reports it, and every mount of at least MinDiskSize bytes.
func DiskMounts(mounts []metrics.Mount, dockerRoot string) []string {
	paths := make([]string, 0, len(mounts))
	for _, m := range mounts {
		paths = append(paths, m.Path)
	}
	holder := MountFor(dockerRoot, paths)
	seen := map[string]bool{}
	var out []string
	for _, m := range mounts {
		if seen[m.Path] || m.Path != "/" && m.Path != holder && m.Total < MinDiskSize {
			continue
		}
		seen[m.Path] = true
		out = append(out, m.Path)
	}
	sort.Strings(out)
	return out
}

// MountFor returns the mount that holds path: the longest mount that is path itself or one of its parents. It returns
// empty when no mount holds path.
func MountFor(path string, mounts []string) string {
	best := ""
	for _, m := range mounts {
		if m == "" || path == "" {
			continue
		}
		holds := m == "/" || path == m || strings.HasPrefix(path, strings.TrimRight(m, "/")+"/")
		if holds && len(m) > len(best) {
			best = m
		}
	}
	return best
}

// diskResult evaluates one mount of the latest sample.
func diskResult(samples Samples, mount, label string) Result {
	s, ok := samples()
	if !ok {
		return Result{State: Unknown, Detail: "no host sample yet"}
	}
	for _, m := range s.Host.Mounts {
		if m.Path != mount {
			continue
		}
		bytes, ok := usedPercent(m.Total, m.Available)
		if !ok {
			return Result{State: Unknown, Detail: "mount size not reported"}
		}
		inodes, inodeOK := usedPercent(m.InodesTotal, m.InodesFree)
		state, used, resource := OK, bytes, "bytes"
		switch {
		case bytes >= diskCritical:
			state = Critical
		case inodeOK && m.InodesFree == 0:
			state, used, resource = Critical, inodes, "inodes"
		case bytes >= diskWarning:
			state = Warning
		case inodeOK && inodes >= inodeWarning:
			state, used, resource = Warning, inodes, "inodes"
		}
		return Result{State: state, Detail: fmt.Sprintf("%.1f%% %s used%s", used, resource, label)}
	}
	return Result{State: Unknown, Detail: "mount not reported"}
}

// usedPercent is the share in use, from a total and free count.
func usedPercent(total, free uint64) (float64, bool) {
	if total == 0 || free > total {
		return 0, false
	}
	return 100 * float64(total-free) / float64(total), true
}

// MemoryCheck returns the check of host memory, named memory.
func MemoryCheck(samples Samples) Check {
	return newPressureCheck("memory", func(context.Context) Result {
		s, ok := samples()
		if !ok {
			return Result{State: Unknown, Detail: "no host sample yet"}
		}
		total, available := s.Host.MemoryTotal, s.Host.MemoryAvailable
		if total == nil || available == nil || *total == 0 {
			return Result{State: Unknown, Detail: "memory not reported"}
		}
		pct := 100 * float64(*available) / float64(*total)
		state := OK
		switch {
		case pct < memoryCritical:
			state = Critical
		case pct < memoryWarning:
			state = Warning
		}
		return Result{State: state, Detail: fmt.Sprintf("%.1f%% available", pct)}
	})
}

// TunnelCheck returns the check of the managed tunnel, named tunnel. status returns the tunnel's status as Cloudflare
// reports it, or "unavailable" when Cloudflare cannot be asked. It is run every two minutes and needs two misses.
func TunnelCheck(status func(context.Context) string) Check {
	return NewFunc("tunnel", 2*time.Minute, 2, func(ctx context.Context) Result {
		return TunnelResult(status(ctx))
	})
}

// TunnelResult maps a tunnel status to a result: healthy is ok, degraded a warning, down and inactive critical, and
// anything else, including an unavailable Cloudflare, unknown.
func TunnelResult(status string) Result {
	switch status {
	case "healthy":
		return Result{State: OK, Detail: "tunnel healthy"}
	case "degraded":
		return Result{State: Warning, Detail: "tunnel degraded"}
	case "down":
		return Result{State: Critical, Detail: "tunnel down"}
	case "inactive":
		return Result{State: Critical, Detail: "tunnel inactive"}
	default:
		return Result{State: Unknown, Detail: "Cloudflare unavailable"}
	}
}

// AlertSecretCheck returns the check of an alert channel, named after the channel, that warns while a secret the channel
// needs is not stored. status says whether the secrets are stored, and the detail to show either way.
func AlertSecretCheck(channel string, status func(context.Context) (bool, string)) Check {
	return NewFunc(channel, time.Minute, 1, func(ctx context.Context) Result {
		ok, detail := status(ctx)
		if !ok {
			return Result{State: Warning, Detail: detail}
		}
		return Result{State: OK, Detail: detail}
	})
}
