package jobs

import (
	"context"
	"errors"
	"testing"
	"time"

	"berthy/internal/api"
	"berthy/internal/engine"
	"berthy/internal/manifest"
	"berthy/internal/state"
)

func targetManifest() manifest.Manifest {
	return manifest.Manifest{
		Project: "demo",
		Apps: map[string]manifest.App{
			"api":  {Kind: "image", Roles: map[string]manifest.Role{"web": {}, "worker": {}}},
			"solo": {Kind: "image", Roles: map[string]manifest.Role{"main": {}}},
			"db":   {Kind: "compose"},
		},
	}
}

func TestStatusesListJobsWithNextAndLastRun(t *testing.T) {
	store, _, _ := openTestStore(t)
	ctx := context.Background()
	m := targetManifest()
	m.Jobs = map[string]manifest.Job{
		"tick":   {App: "solo", Schedule: "*/5 * * * *", Timeout: "30s", Command: []string{"date"}},
		"manual": {App: "api", Role: "web", Command: []string{"date"}},
		"dump":   {App: "db", Service: "postgres", Schedule: "30 2 * * *", Command: []string{"date"}},
	}
	run, err := store.StartJobRun(ctx, state.JobRun{Job: "tick", App: "solo", Trigger: "manual", Started: time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	three := 3
	if err = store.FinishJobRun(ctx, run, "failed", &three, "exit status 3"); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 16, 10, 2, 30, 0, time.UTC)
	got, err := Statuses(ctx, store, m, "", now)
	if err != nil || len(got) != 3 {
		t.Fatalf("statuses: %+v %v", got, err)
	}
	dump, manual, tick := got[0], got[1], got[2]
	if dump.Name != "dump" || manual.Name != "manual" || tick.Name != "tick" {
		t.Fatalf("not sorted by name: %v %v %v", dump.Name, manual.Name, tick.Name)
	}
	if tick.App != "solo" || tick.Target != "main" || tick.Schedule != "*/5 * * * *" || tick.Timeout != "30s" || tick.Next == nil || !tick.Next.Equal(time.Date(2026, 11, 16, 10, 5, 0, 0, time.UTC)) {
		t.Fatalf("tick: %+v", tick)
	}
	if tick.Last == nil || tick.Last.Status != "failed" || tick.Last.Detail != "exit status 3" || tick.Last.ExitCode == nil || *tick.Last.ExitCode != 3 || tick.Last.Trigger != "manual" || tick.Last.Finished == nil || tick.Last.Scheduled != nil {
		t.Fatalf("tick last run: %+v", tick.Last)
	}
	if manual.Next != nil || manual.Last != nil || manual.Target != "web" || manual.Schedule != "" || manual.Timeout != "1h0m0s" {
		t.Fatalf("manual: %+v", manual)
	}
	if dump.Target != "postgres" {
		t.Fatalf("dump: %+v", dump)
	}
	one, err := Statuses(ctx, store, m, "solo", now)
	if err != nil || len(one) != 1 || one[0].Name != "tick" {
		t.Fatalf("filtered statuses: %+v %v", one, err)
	}
	var _ []api.JobStatus = one
}

// TestPickComposeContainerChoosesWhatAJobOrAnExecRunsIn: the running, non-one-off container of the named service, or of the
// only service when none is named, the lowest replica first and then the lowest id.
func TestPickComposeContainerChoosesWhatAJobOrAnExecRunsIn(t *testing.T) {
	pg := []engine.ComposeContainer{
		{ID: "pg-3", Service: "postgres", State: "running", Number: 3},
		{ID: "pg-2", Service: "postgres", State: "running", Number: 2},
		{ID: "pg-2b", Service: "postgres", State: "running", Number: 2},
	}
	web := engine.ComposeContainer{ID: "web-1", Service: "web", State: "running", Number: 1}
	oneOff := engine.ComposeContainer{ID: "pg-oneoff", Service: "postgres", State: "running", Number: 1, OneOff: true}
	stopped := engine.ComposeContainer{ID: "pg-stopped", Service: "postgres", State: "exited", Number: 1}
	for _, tc := range []struct {
		name       string
		containers []engine.ComposeContainer
		service    string
		want       string
	}{
		{"named service", append([]engine.ComposeContainer{web, oneOff, stopped}, pg...), "postgres", "pg-2"},
		{"only service", append([]engine.ComposeContainer{oneOff, stopped}, pg...), "", "pg-2"},
		{"lowest id among replicas of one number", []engine.ComposeContainer{{ID: "b", Service: "x", State: "running", Number: 1}, {ID: "a", Service: "x", State: "running", Number: 1}}, "x", "a"},
		{"one-off container is never chosen", []engine.ComposeContainer{oneOff, {ID: "pg-run", Service: "postgres", State: "running", Number: 4}}, "postgres", "pg-run"},
	} {
		got, err := PickComposeContainer(tc.containers, tc.service)
		if err != nil || got != tc.want {
			t.Errorf("%s: container %q, %v; want %q", tc.name, got, err, tc.want)
		}
	}
	if _, err := PickComposeContainer([]engine.ComposeContainer{web, {ID: "pg", Service: "postgres", State: "running", Number: 1}}, ""); !errors.Is(err, ErrSeveralServices) {
		t.Errorf("two services without a name: %v", err)
	}
	if _, err := PickComposeContainer([]engine.ComposeContainer{web, {ID: "pg", Service: "postgres", State: "running", Number: 1}}, "postgres"); err != nil {
		t.Errorf("named service among two: %v", err)
	}
	for _, tc := range []struct {
		name       string
		containers []engine.ComposeContainer
		service    string
	}{
		{"no containers", nil, ""},
		{"only one-off and stopped", []engine.ComposeContainer{oneOff, stopped}, "postgres"},
		{"named service not running", []engine.ComposeContainer{web}, "postgres"},
	} {
		if _, err := PickComposeContainer(tc.containers, tc.service); !errors.Is(err, ErrNoRunningContainer) {
			t.Errorf("%s: %v", tc.name, err)
		}
	}
}
