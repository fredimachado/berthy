package monitor

import (
	"berthy/internal/dialtest"
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"berthy/internal/api"
	"berthy/internal/metrics"
)

// samplesWith returns a Samples function over one fixed sample.
func samplesWith(s metrics.Sample) Samples {
	return func() (metrics.Sample, bool) { return s, true }
}

func mount(path string, total, available uint64) metrics.Mount {
	return metrics.Mount{Path: path, Total: total, Available: available}
}

func TestDiskThresholdsUseUsedSpace(t *testing.T) {
	cases := []struct {
		name      string
		available uint64
		state     string
		detail    string
	}{
		{"well below", 75, OK, "25.0% used"},
		{"warning boundary", 20, Warning, "80.0% used"},
		{"warning", 15, Warning, "85.0% used"},
		{"critical boundary", 10, Critical, "90.0% used"},
		{"critical", 5, Critical, "95.0% used"},
		{"full", 0, Critical, "100.0% used"},
	}
	for _, tc := range cases {
		s := metrics.Sample{Host: metrics.Host{Mounts: []metrics.Mount{mount("/", 100, tc.available)}}}
		got := DiskCheck(samplesWith(s), "/", false).Run(context.Background())
		if got.State != tc.state || got.Detail != tc.detail {
			t.Errorf("%s: got %+v", tc.name, got)
		}
	}
	s := metrics.Sample{Host: metrics.Host{Mounts: []metrics.Mount{mount("/", 100, 15)}}}
	if got := DiskCheck(samplesWith(s), "/", true).Run(context.Background()); got.Detail != "85.0% used (Docker data)" {
		t.Fatalf("Docker data label: %+v", got)
	}
}

func TestDiskCheckUnknownWithoutData(t *testing.T) {
	ctx := context.Background()
	if got := DiskCheck(func() (metrics.Sample, bool) { return metrics.Sample{}, false }, "/", false).Run(ctx); got.State != Unknown {
		t.Fatalf("no sample: %+v", got)
	}
	s := metrics.Sample{Host: metrics.Host{Mounts: []metrics.Mount{mount("/", 0, 0)}}}
	if got := DiskCheck(samplesWith(s), "/", false).Run(ctx); got.State != Unknown {
		t.Fatalf("zero size: %+v", got)
	}
	if got := DiskCheck(samplesWith(s), "/data", false).Run(ctx); got.State != Unknown || got.Detail != "mount not reported" {
		t.Fatalf("missing mount: %+v", got)
	}
	if got := DiskCheck(samplesWith(s), "/", false).Name(); got != "disk:/" {
		t.Fatalf("name %q", got)
	}
}

func TestDockerDiskUsesTheMountHoldingTheDataRoot(t *testing.T) {
	cases := map[string]struct {
		root   string
		mounts []string
		want   string
	}{
		"own mount":          {"/var/lib/docker", []string{"/", "/var/lib/docker"}, "/var/lib/docker"},
		"root mount":         {"/var/lib/docker", []string{"/", "/data"}, "/"},
		"parent mount":       {"/mnt/dockerdata", []string{"/", "/mnt"}, "/mnt"},
		"partial name":       {"/mntx/docker", []string{"/mnt"}, ""},
		"no root":            {"", []string{"/"}, ""},
		"trailing separator": {"/srv/docker/data", []string{"/srv/"}, "/srv/"},
	}
	for name, tc := range cases {
		if got := MountFor(tc.root, tc.mounts); got != tc.want {
			t.Errorf("%s: MountFor = %q, want %q", name, got, tc.want)
		}
	}
	s := metrics.Sample{Host: metrics.Host{Mounts: []metrics.Mount{mount("/", 100, 5), mount("/data", 100, 50)}}}
	got := DockerDiskCheck(samplesWith(s), "/").Run(context.Background())
	if got.State != Critical || got.Detail != "95.0% used (Docker data)" {
		t.Fatalf("docker disk: %+v", got)
	}
	if got := DockerDiskCheck(samplesWith(s), "").Run(context.Background()); got.State != Unknown || got.Detail != "Docker data root unknown" {
		t.Fatalf("unknown root: %+v", got)
	}
	if DockerDiskCheck(samplesWith(s), "/").Name() != "docker_disk" {
		t.Fatal("docker disk name")
	}
}

func TestMemoryThresholds(t *testing.T) {
	cases := []struct {
		available uint64
		state     string
	}{
		{150, OK}, {100, OK}, {99, Warning}, {50, Warning}, {49, Critical}, {0, Critical},
	}
	for _, tc := range cases {
		total, avail := uint64(1000), tc.available
		s := metrics.Sample{Host: metrics.Host{MemoryTotal: &total, MemoryAvailable: &avail}}
		if got := MemoryCheck(samplesWith(s)).Run(context.Background()); got.State != tc.state {
			t.Errorf("available %d: got %+v, want %s", tc.available, got, tc.state)
		}
	}
	empty := metrics.Sample{}
	if got := MemoryCheck(samplesWith(empty)).Run(context.Background()); got.State != Unknown {
		t.Fatalf("memory not reported: %+v", got)
	}
}

func TestTunnelStatusMapping(t *testing.T) {
	for status, want := range map[string]string{
		"healthy":     OK,
		"degraded":    Warning,
		"down":        Critical,
		"inactive":    Critical,
		"unavailable": Unknown,
		"":            Unknown,
		"surprising":  Unknown,
	} {
		if got := TunnelResult(status).State; got != want {
			t.Errorf("%q: %s, want %s", status, got, want)
		}
	}
	check := TunnelCheck(func(context.Context) string { return "degraded" })
	if check.Name() != "tunnel" || check.Interval() != 2*time.Minute || check.Failures() != 2 {
		t.Fatalf("tunnel check shape: %s %v %d", check.Name(), check.Interval(), check.Failures())
	}
	if got := check.Run(context.Background()); got.State != Warning {
		t.Fatalf("tunnel run: %+v", got)
	}
}

func TestAlertSecretCheckWarnsWhenMissing(t *testing.T) {
	present := false
	check := AlertSecretCheck("webhook", func(context.Context) (bool, string) {
		if present {
			return true, "url secret present"
		}
		return false, "webhook url secret missing"
	})
	if check.Name() != "webhook" {
		t.Fatalf("name %q", check.Name())
	}
	if got := check.Run(context.Background()); got.State != Warning || got.Detail != "webhook url secret missing" {
		t.Fatalf("missing secret: %+v", got)
	}
	present = true
	if got := check.Run(context.Background()); got.State != OK || got.Detail != "url secret present" {
		t.Fatalf("present secret: %+v", got)
	}
}

func TestHTTPProbeStatuses(t *testing.T) {
	var status atomic.Int32
	status.Store(http.StatusOK)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(int(status.Load()))
	}))
	defer srv.Close()
	ctx := context.Background()
	run := func(expect []int) Result {
		return HTTPProbe("probe:x", srv.URL, expect, time.Minute, time.Second, 1, nil).Run(ctx)
	}
	if got := run(nil); got.State != OK || got.Detail != "HTTP 200" {
		t.Fatalf("200 by default: %+v", got)
	}
	status.Store(int32(http.StatusNoContent))
	if got := run(nil); got.State != OK {
		t.Fatalf("204 by default: %+v", got)
	}
	status.Store(int32(http.StatusServiceUnavailable))
	if got := run(nil); got.State != Critical || got.Detail != "HTTP 503" {
		t.Fatalf("503 by default: %+v", got)
	}
	status.Store(int32(http.StatusNoContent))
	if got := run([]int{204}); got.State != OK {
		t.Fatalf("listed status: %+v", got)
	}
	status.Store(int32(http.StatusOK))
	if got := run([]int{204, 201}); got.State != Critical || got.Detail != "HTTP 200" {
		t.Fatalf("unlisted status: %+v", got)
	}
}

func TestHTTPProbeDoesNotFollowRedirects(t *testing.T) {
	var hits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer redirect.Close()
	got := HTTPProbe("probe:r", redirect.URL, nil, time.Minute, time.Second, 1, nil).Run(context.Background())
	if got.State != OK || got.Detail != "HTTP 302" {
		t.Fatalf("redirect not taken as the status: %+v", got)
	}
	if hits.Load() != 0 {
		t.Fatal("redirect target was requested")
	}
}

func TestHTTPProbeSendsItsUserAgentAndNeverEchoesBodies(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.UserAgent() != "berthy-probe" {
			w.WriteHeader(http.StatusTeapot)
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("private-body-text"))
	}))
	defer srv.Close()
	got := HTTPProbe("probe:ua", srv.URL, nil, time.Minute, time.Second, 1, nil).Run(context.Background())
	if got.Detail != "HTTP 500" || strings.Contains(got.Detail, "private") {
		t.Fatalf("detail: %+v", got)
	}
	ok := HTTPProbe("probe:ua", srv.URL, []int{500}, time.Minute, time.Second, 1, nil).Run(context.Background())
	if ok.State != OK {
		t.Fatalf("user agent not sent: %+v", ok)
	}
}

func TestHTTPProbeTimeoutAndRefusedConnection(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}))
	defer slow.Close()
	got := HTTPProbe("probe:slow", slow.URL, nil, time.Minute, 50*time.Millisecond, 1, nil).Run(context.Background())
	if got.State != Critical || got.Detail != "timeout" {
		t.Fatalf("timeout: %+v", got)
	}
	closed := dialtest.Refused
	got = HTTPProbe("probe:closed", "http://"+closed+"/", nil, time.Minute, time.Second, 1, dialtest.Dial).Run(context.Background())
	if got.State != Critical || got.Detail != "connection refused" {
		t.Fatalf("refused: %+v", got)
	}
	if strings.Contains(got.Detail, closed) {
		t.Fatalf("error text echoed the address: %q", got.Detail)
	}
}

func TestTCPProbe(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	ctx := context.Background()
	if got := TCPProbe("probe:db", l.Addr().String(), time.Minute, time.Second, 1, nil).Run(ctx); got.State != OK || got.Detail != "connected" {
		t.Fatalf("open port: %+v", got)
	}
	if got := TCPProbe("probe:db", dialtest.Refused, time.Minute, time.Second, 1, dialtest.Dial).Run(ctx); got.State != Critical || got.Detail != "connection refused" {
		t.Fatalf("closed port: %+v", got)
	}
}

// fakeProbeHelper answers every probe with the same result and remembers the last request.
type fakeProbeHelper struct {
	ok     bool
	detail string
	err    error
	last   HelperProbe
}

func (f *fakeProbeHelper) Probe(_ context.Context, req HelperProbe) (bool, string, error) {
	f.last = req
	return f.ok, f.detail, f.err
}

func TestServiceProbeReportsTheHelperAnswer(t *testing.T) {
	ctx := context.Background()
	req := HelperProbe{Kind: KindHTTP, Target: "web:8080", Path: "/health", TimeoutMS: 5000, Expect: []int{200}}
	h := &fakeProbeHelper{ok: true, detail: "HTTP 200"}
	if got := ServiceProbe("probe:web", 30*time.Second, 3, h, req).Run(ctx); got.State != OK || got.Detail != "HTTP 200" {
		t.Fatalf("passing probe: %+v", got)
	}
	if h.last.Target != "web:8080" || h.last.Path != "/health" || h.last.TimeoutMS != 5000 || len(h.last.Expect) != 1 {
		t.Fatalf("request sent: %+v", h.last)
	}
	h.ok, h.detail = false, "HTTP 503"
	if got := ServiceProbe("probe:web", 30*time.Second, 3, h, req).Run(ctx); got.State != Critical || got.Detail != "HTTP 503" {
		t.Fatalf("failing probe: %+v", got)
	}
	h.err = errors.New("probe helper unavailable: not running")
	if got := ServiceProbe("probe:web", 30*time.Second, 3, h, req).Run(ctx); got.State != Unknown || got.Detail != "probe helper unavailable: not running" {
		t.Fatalf("helper unavailable: %+v", got)
	}
	if got := ServiceProbe("probe:web", 30*time.Second, 3, nil, req).Run(ctx); got.State != Unknown || got.Detail != "probe helper unavailable: not configured" {
		t.Fatalf("no helper: %+v", got)
	}
}

func TestPeerProbe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	ctx := context.Background()
	if got := ProbePeer(ctx, srv.URL+"/", time.Second, nil); got.State != OK {
		t.Fatalf("healthy peer: %+v", got)
	}
	if got := ProbePeer(ctx, srv.URL+"/other", time.Second, nil); got.State != Critical || got.Detail != "silent: HTTP 404" {
		t.Fatalf("peer without /healthz: %+v", got)
	}
	if got := ProbePeer(ctx, "http://"+dialtest.Refused, time.Second, dialtest.Dial); got.State != Critical || got.Detail != "silent: connection refused" {
		t.Fatalf("silent peer: %+v", got)
	}
	check := PeerCheck("peer:prod", srv.URL, nil)
	if check.Name() != "peer:prod" || check.Interval() != 30*time.Second || check.Failures() != 3 {
		t.Fatalf("peer check shape: %s %v %d", check.Name(), check.Interval(), check.Failures())
	}
}

func TestCheckNamesAndShapes(t *testing.T) {
	probe := HTTPProbe("probe:site", "http://127.0.0.1:1/", nil, 5*time.Second, time.Second, 2, nil)
	if probe.Name() != "probe:site" || probe.Interval() != 5*time.Second || probe.Failures() != 2 {
		t.Fatalf("probe shape: %s %v %d", probe.Name(), probe.Interval(), probe.Failures())
	}
	if NewFunc("x", time.Second, 0, nil).Failures() != 1 {
		t.Fatal("failure limit below one not raised")
	}
}

func TestRestartLoopOverTime(t *testing.T) {
	t0 := time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)
	ref := api.AppRef{Kind: "image", Name: "api"}
	key := RoleKey(ref, "web")
	w := NewRestartWatch(nil)
	at := func(m int) time.Time { return t0.Add(time.Duration(m) * time.Minute) }
	observe := func(m, count int, oom bool) {
		w.Observe(at(m), inventory(ref, "web", "c1", count, oom), nil)
	}
	observe(0, 0, false)
	if got := w.result(at(0), key); got.State != OK || got.Detail != "1 replicas, no restart loop" {
		t.Fatalf("first observation: %+v", got)
	}
	observe(1, 1, false)
	observe(2, 2, false)
	if got := w.result(at(2), key); got.State != OK {
		t.Fatalf("two restarts: %+v", got)
	}
	observe(3, 3, false)
	if got := w.result(at(3), key); got.State != Warning || got.Detail != "a replica restarted 3 times in 10 minutes" {
		t.Fatalf("three restarts in three minutes: %+v", got)
	}
	observe(14, 3, false) // the loop has stopped and the window has passed the restarts
	if got := w.result(at(14), key); got.State != OK {
		t.Fatalf("loop still reported after the window: %+v", got)
	}
}

func TestSlowRestartsDoNotMakeALoop(t *testing.T) {
	t0 := time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)
	ref := api.AppRef{Kind: "compose", Name: "shop"}
	key := RoleKey(ref, "db")
	w := NewRestartWatch(nil)
	// One restart every five minutes is three restarts in fifteen minutes, but only two inside any ten-minute window.
	for i, m := range []int{0, 5, 10, 15} {
		w.Observe(t0.Add(time.Duration(m)*time.Minute), inventory(ref, "db", "c1", i, false), nil)
	}
	if got := w.result(t0.Add(15*time.Minute), key); got.State != OK {
		t.Fatalf("slow restarts reported as a loop: %+v", got)
	}
}

func TestRestartLoopCountsTasksWithoutContainers(t *testing.T) {
	t0 := time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)
	ref := api.AppRef{Kind: "swarm", Name: "legacy"}
	key := RoleKey(ref, "legacy")
	w := NewRestartWatch(nil)
	for i := 0; i < 4; i++ {
		count := i
		w.Observe(t0.Add(time.Duration(i)*time.Minute), []api.App{{Ref: ref, Roles: []api.Role{{Name: "legacy", Replicas: []api.Replica{{TaskID: "t1", RestartCount: &count}}}}}}, nil)
	}
	if got := w.result(t0.Add(3*time.Minute), key); got.State != Warning {
		t.Fatalf("task restarts not counted: %+v", got)
	}
}

func TestOOMKillIsAWarningAndStaleInventoryIsUnknown(t *testing.T) {
	t0 := time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)
	ref := api.AppRef{Kind: "image", Name: "worker"}
	key := RoleKey(ref, "main")
	w := NewRestartWatch(nil)
	if got := w.result(t0, key); got.State != Unknown {
		t.Fatalf("no inventory yet: %+v", got)
	}
	w.Observe(t0, inventory(ref, "main", "c1", 0, true), nil)
	if got := w.result(t0, key); got.State != Warning || got.Detail != "a replica was OOM killed" {
		t.Fatalf("OOM: %+v", got)
	}
	if got := w.result(t0.Add(inventoryStale+time.Minute), key); got.State != Unknown || got.Detail != "no recent Docker inventory" {
		t.Fatalf("stale inventory: %+v", got)
	}
	w.Observe(t0.Add(5*time.Minute), nil, nil)
	if got := w.result(t0.Add(5*time.Minute), RoleKey(ref, "main")); got.State != OK || got.Detail != "no replicas" {
		t.Fatalf("role gone: %+v", got)
	}
}

func TestRestartCheckNames(t *testing.T) {
	if got := RestartCheckName(api.AppRef{Kind: "image", Name: "api"}, "web"); got != "restarts:api/web" {
		t.Fatalf("image app: %s", got)
	}
	if got := RestartCheckName(api.AppRef{Kind: "swarm", Name: "legacy"}, "legacy"); got != "restarts:swarm:legacy/legacy" {
		t.Fatalf("swarm service: %s", got)
	}
	check := NewRestartWatch(nil).Check(api.AppRef{Kind: "image", Name: "api"}, "web")
	if check.Name() != "restarts:api/web" || check.Interval() != time.Minute || check.Failures() != 1 {
		t.Fatalf("restart check shape: %s %v %d", check.Name(), check.Interval(), check.Failures())
	}
}

// inventory returns one app with one role holding one replica.
func inventory(ref api.AppRef, role, id string, count int, oom bool) []api.App {
	return []api.App{{Ref: ref, Roles: []api.Role{{Name: role, Replicas: []api.Replica{{ContainerID: id, RestartCount: &count, OOMKilled: &oom}}}}}}
}

func TestDiskMountsFollowTheSizeRule(t *testing.T) {
	const gib = 1 << 30
	mounts := []metrics.Mount{
		{Path: "/", Total: 512 << 20, Available: 1 << 20},
		{Path: "/usr/src/linux-headers", Total: 101527552, Available: 0},
		{Path: "/data", Total: 2 * gib, Available: gib},
		{Path: "/boot", Total: gib, Available: gib / 2},
		{Path: "/small", Total: gib - 1, Available: 0},
		{Path: "/var/lib/docker", Total: 512 << 20, Available: 1 << 20},
		{Path: "/data", Total: 2 * gib, Available: gib},
	}
	same := func(got []string, want ...string) {
		t.Helper()
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("disk mounts %v, want %v", got, want)
		}
	}
	// The root and Docker's mount are checked even when small; a full 96 MB mount, a 1 GiB-minus-one mount and a
	// duplicate path are not added twice or at all.
	same(DiskMounts(mounts, "/var/lib/docker/overlay2"), "/", "/boot", "/data", "/var/lib/docker")
	// Without Docker's data root, its small mount is not known to matter.
	same(DiskMounts(mounts, ""), "/", "/boot", "/data")
	same(DiskMounts(nil, "/var/lib/docker"))
	same(DiskMounts([]metrics.Mount{{Path: "/", Total: 100 << 20}}, ""), "/")
	if MinDiskSize != gib {
		t.Fatalf("MinDiskSize %d", MinDiskSize)
	}
}

func TestSwarmTaskFailuresWarnLikeRestartLoops(t *testing.T) {
	t0 := time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)
	ref := api.AppRef{Kind: "image", Name: "worker"}
	key := RoleKey(ref, "main")
	zero, healthy, killed := 0, false, true
	apps := []api.App{{Ref: ref, Roles: []api.Role{{Name: "main", Replicas: []api.Replica{{TaskID: "t9", RestartCount: &zero}}}}}}
	w := NewRestartWatch(nil)
	w.Observe(t0, apps, map[string]int{key: 2})
	if got := w.result(t0, key); got.State != OK || got.Detail != "1 replicas, no restart loop" {
		t.Fatalf("two failed tasks: %+v", got)
	}
	w.Observe(t0.Add(time.Minute), apps, map[string]int{key: 3})
	if got := w.result(t0.Add(time.Minute), key); got.State != Warning || got.Detail != "3 task failures in 10m" {
		t.Fatalf("three failed tasks: %+v", got)
	}
	oom := []api.App{{Ref: ref, Roles: []api.Role{{Name: "main", Replicas: []api.Replica{{TaskID: "t9", RestartCount: &zero, OOMKilled: &healthy}, {TaskID: "t10", RestartCount: &zero, OOMKilled: &killed}}}}}}
	w.Observe(t0.Add(2*time.Minute), oom, map[string]int{key: 4})
	if got := w.result(t0.Add(2*time.Minute), key); got.State != Warning || got.Detail != "4 task failures in 10m; a replica was OOM killed" {
		t.Fatalf("failures and an OOM kill: %+v", got)
	}
	// Failures leave the count when the window passes them: the next inventory counts none.
	w.Observe(t0.Add(13*time.Minute), apps, nil)
	if got := w.result(t0.Add(13*time.Minute), key); got.State != OK {
		t.Fatalf("failures still reported after the window: %+v", got)
	}
}
