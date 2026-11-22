package metrics

import (
	"context"
	"errors"
	"testing"
	"time"
)

type fakeHost struct{}

func (fakeHost) Read(context.Context) Host { return Host{} }

type fakeDocker struct {
	raw RawContainer
	err error
}

func (f *fakeDocker) Containers(context.Context) ([]string, error)        { return []string{"abc"}, f.err }
func (f *fakeDocker) Stats(context.Context, string) (RawContainer, error) { return f.raw, f.err }
func TestCollectorRatesAndRetention(t *testing.T) {
	now := time.Unix(10000, 0)
	d := &fakeDocker{raw: RawContainer{CPUUsage: 100, SystemUsage: 1000, OnlineCPUs: 2, MemoryUsage: 100, MemoryLimit: 200, Cache: 20}}
	c := New(fakeHost{}, d)
	c.now = func() time.Time { return now }
	c.collect(context.Background())
	s, _ := c.Latest()
	if s.Containers[0].CPUPercent != nil || s.Containers[0].MemoryUsage != 80 {
		t.Fatalf("first sample: %+v", s)
	}
	now = now.Add(10 * time.Second)
	d.raw.CPUUsage = 150
	d.raw.SystemUsage = 1200
	c.collect(context.Background())
	s, _ = c.Latest()
	if s.Containers[0].CPUPercent == nil || *s.Containers[0].CPUPercent != 50 {
		t.Fatalf("rate: %+v", s)
	}
	now = now.Add(time.Hour)
	if len(c.Since(time.Hour)) != 1 {
		t.Fatal("expired history retained")
	}
}

func TestFailureRecoveryAndSnapshotsAreIndependent(t *testing.T) {
	now := time.Unix(10000, 0)
	d := &fakeDocker{raw: RawContainer{CPUUsage: 10, SystemUsage: 10, OnlineCPUs: 1}}
	c := New(fakeHost{}, d)
	c.now = func() time.Time { return now }
	c.collect(context.Background())
	d.err = errors.New("offline")
	now = now.Add(interval)
	c.collect(context.Background())
	s, _ := c.Latest()
	if s.DockerError == "" || s.ContainersFresh {
		t.Fatal("outage hidden")
	}
	d.err = nil
	now = now.Add(interval)
	d.raw.CPUUsage = 20
	d.raw.SystemUsage = 20
	c.collect(context.Background())
	s, _ = c.Latest()
	if s.Containers[0].CPUPercent != nil {
		t.Fatal("outage retained baseline")
	}
	s.Containers[0].ID = "changed"
	s2, _ := c.Latest()
	if s2.Containers[0].ID != "abc" {
		t.Fatal("caller changed snapshot")
	}
}
func TestWatchIsBounded(t *testing.T) {
	c := New(fakeHost{}, &fakeDocker{})
	ch, stop := c.Subscribe()
	defer stop()
	c.collect(context.Background())
	c.collect(context.Background())
	if len(ch) != 1 {
		t.Fatal("notification not bounded")
	}
}
func TestInvalidAndResetIntervals(t *testing.T) {
	now := time.Unix(10000, 0)
	d := &fakeDocker{raw: RawContainer{CPUUsage: 100, SystemUsage: 100, OnlineCPUs: 1}}
	c := New(fakeHost{}, d)
	c.now = func() time.Time { return now }
	c.collect(context.Background())
	now = now.Add(interval)
	d.raw.CPUUsage = 5
	d.raw.SystemUsage = 101
	c.collect(context.Background())
	s, _ := c.Latest()
	if s.Containers[0].CPUPercent != nil {
		t.Fatal("reset produced a rate")
	}
	now = now.Add(3 * interval)
	d.raw.CPUUsage = 10
	d.raw.SystemUsage = 110
	c.collect(context.Background())
	s, _ = c.Latest()
	if s.Containers[0].CPUPercent != nil {
		t.Fatal("gap produced a rate")
	}
}
func TestPartialContainerFailureMarksSampleStale(t *testing.T) {
	c := New(fakeHost{}, partialDocker{})
	c.collect(context.Background())
	s, _ := c.Latest()
	if s.ContainersFresh || len(s.Containers) != 1 || s.Containers[0].Error == "" {
		t.Fatalf("partial failure hidden: %+v", s)
	}
}

type partialDocker struct{}

func (partialDocker) Containers(context.Context) ([]string, error) { return []string{"x"}, nil }
func (partialDocker) Stats(context.Context, string) (RawContainer, error) {
	return RawContainer{}, errors.New("gone")
}

func TestOnlyKeepsTheHostAndTheMatchingContainers(t *testing.T) {
	cpu := 12.5
	total := uint64(1000)
	s := Sample{
		Host:            Host{CPUPercent: &cpu, MemoryTotal: &total, Load: []float64{1, 2, 3}},
		Containers:      []Container{{ID: "keep-a", Labels: map[string]string{"app": "web"}}, {ID: "drop", Labels: map[string]string{"app": "db"}}, {ID: "keep-b", Labels: map[string]string{"app": "web"}}},
		DockerError:     "docker busy",
		ContainersFresh: true,
	}
	got := s.Only(func(labels map[string]string) bool { return labels["app"] == "web" })
	if got.Host.CPUPercent == nil || *got.Host.CPUPercent != cpu || got.Host.MemoryTotal == nil || *got.Host.MemoryTotal != total || len(got.Host.Load) != 3 {
		t.Fatalf("host changed: %+v", got.Host)
	}
	if got.DockerError != s.DockerError || !got.ContainersFresh {
		t.Fatalf("sample flags changed: %+v", got)
	}
	if len(got.Containers) != 2 || got.Containers[0].ID != "keep-a" || got.Containers[1].ID != "keep-b" {
		t.Fatalf("containers: %+v", got.Containers)
	}
	if len(s.Containers) != 3 {
		t.Fatal("Only mutated the original sample")
	}
}
