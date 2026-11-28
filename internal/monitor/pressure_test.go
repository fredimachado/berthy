package monitor

import (
	"context"
	"fmt"
	"testing"
	"time"

	"berthy/internal/metrics"
)

func TestHostChecksRecoverAfterTenConsecutivePasses(t *testing.T) {
	cases := []struct {
		name                                      string
		makeCheck                                 func(Samples) Check
		passing, warning, critical                uint64
		passDetail, warningDetail, criticalDetail string
	}{
		{"disk", func(s Samples) Check { return DiskCheck(s, "/", false) }, 210, 199, 99, "79.0% used", "80.1% used", "90.1% used"},
		{"docker disk", func(s Samples) Check { return DockerDiskCheck(s, "/") }, 210, 199, 99, "79.0% used (Docker data)", "80.1% used (Docker data)", "90.1% used (Docker data)"},
		{"memory", MemoryCheck, 101, 99, 49, "10.1% available", "9.9% available", "4.9% available"},
	}
	for _, tc := range cases {
		for _, severity := range []string{Warning, Critical} {
			t.Run(tc.name+"/"+severity, func(t *testing.T) {
				r := newRig(t, openStore(t))
				total, available := uint64(1000), tc.passing
				samples := func() (metrics.Sample, bool) {
					return metrics.Sample{Host: metrics.Host{Mounts: []metrics.Mount{mount("/", total, available)}, MemoryTotal: &total, MemoryAvailable: &available}}, true
				}
				r.m.Checks = func(context.Context) []Check { return []Check{tc.makeCheck(samples)} }
				name := tc.makeCheck(samples).Name()
				r.step(0)
				if got := r.stored(name); got.State != OK || got.Detail != tc.passDetail {
					t.Fatalf("healthy startup: %+v", got)
				}
				available, detail, icon := tc.warning, tc.warningDetail, "🟠"
				if severity == Critical {
					available, detail, icon = tc.critical, tc.criticalDetail, "🔴"
				}
				failure := available
				r.step(30 * time.Second)
				initial := r.stored(name)
				if initial.State != severity || initial.Detail != detail {
					t.Fatalf("first failure: %+v", initial)
				}
				alert := fmt.Sprintf("%s demo/lab %s: %s", icon, name, detail)
				expectMessages(t, r.notifier.messages(), alert)
				assertHeld := func() {
					t.Helper()
					got := r.stored(name)
					if got.State != severity || got.Detail != detail || !got.Since.Equal(initial.Since) {
						t.Fatalf("failure not held: %+v; initial %+v", got, initial)
					}
					expectMessages(t, r.notifier.messages(), alert)
				}
				if severity == Warning {
					for i := 0; i < 3; i++ {
						available = tc.passing
						r.step(30 * time.Second)
						assertHeld()
						available = failure
						r.step(30 * time.Second)
						assertHeld()
					}
					available = tc.passing
					for i := 0; i < 9; i++ {
						r.step(30 * time.Second)
						assertHeld()
					}
					available = failure
					r.step(30 * time.Second)
					assertHeld()
				}
				available = tc.passing
				for i := 0; i < 9; i++ {
					r.step(30 * time.Second)
					assertHeld()
				}
				r.step(30 * time.Second)
				got := r.stored(name)
				if got.State != OK || got.Detail != tc.passDetail || !got.Since.Equal(r.now) || !got.Since.After(initial.Since) {
					t.Fatalf("tenth pass: %+v", got)
				}
				recovery := fmt.Sprintf("✅ demo/lab %s recovered", name)
				expectMessages(t, r.notifier.messages(), alert, recovery)
				r.step(30 * time.Second)
				expectMessages(t, r.notifier.messages(), alert, recovery)
			})
		}
	}
}

func TestDiskMountsRecoverIndependently(t *testing.T) {
	r := newRig(t, openStore(t))
	a, b := uint64(199), uint64(199)
	samples := func() (metrics.Sample, bool) {
		return metrics.Sample{Host: metrics.Host{Mounts: []metrics.Mount{mount("/a", 1000, a), mount("/b", 1000, b)}}}, true
	}
	r.m.Checks = func(context.Context) []Check {
		return []Check{DiskCheck(samples, "/a", false), DiskCheck(samples, "/b", false)}
	}
	r.step(0)
	initialB := r.stored("disk:/b")
	a, b = 210, 210
	for i := 0; i < 9; i++ {
		r.step(30 * time.Second)
	}
	b = 199
	r.step(30 * time.Second)
	if got := r.stored("disk:/a"); got.State != OK || got.Detail != "79.0% used" {
		t.Fatalf("first mount: %+v", got)
	}
	if got := r.stored("disk:/b"); got.State != Warning || got.Detail != initialB.Detail || !got.Since.Equal(initialB.Since) {
		t.Fatalf("second mount: %+v", got)
	}
	expectMessages(t, r.notifier.messages(), "🟠 demo/lab disk:/a: 80.1% used", "🟠 demo/lab disk:/b: 80.1% used", "✅ demo/lab disk:/a recovered")
}
