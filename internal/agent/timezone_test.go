package agent

import (
	"strings"
	"testing"
	"time"

	"berthy/internal/manifest"
	"berthy/internal/monitor"
	"berthy/internal/state"
)

// A backup at 02:00 Lisbon time is due at 01:00 UTC on 2026-03-29, because the clocks went forward that day, so it is overdue an
// hour after that and not an hour after 02:00 UTC.
func TestBackupOverdueFollowsTheManifestsZone(t *testing.T) {
	lisbon, err := time.LoadLocation("Europe/Lisbon")
	if err != nil {
		t.Fatal(err)
	}
	last := time.Date(2026, 3, 28, 2, 0, 5, 0, time.UTC)
	hist := state.BackupHistory{Successes: []state.BackupRun{success(last, 100)}}
	hist.Last = &hist.Successes[0]
	b := manifest.Backup{Schedule: "0 2 * * *"}
	for _, tc := range []struct {
		now  time.Time
		want string
	}{
		{time.Date(2026, 3, 29, 1, 30, 0, 0, time.UTC), monitor.OK},
		{time.Date(2026, 3, 29, 2, 30, 0, 0, time.UTC), monitor.Critical},
	} {
		if got := backupResult(b, lisbon, hist, last, tc.now); got.State != tc.want {
			t.Errorf("at %v in Lisbon: %v %q, want %v", tc.now, got.State, got.Detail, tc.want)
		}
	}
	if got := backupResult(b, time.UTC, hist, last, time.Date(2026, 3, 29, 2, 30, 0, 0, time.UTC)); got.State != monitor.OK {
		t.Errorf("in UTC at 02:30: %v %q, want ok", got.State, got.Detail)
	}
}

func TestStateBackupOverdueFollowsTheManifestsZone(t *testing.T) {
	lisbon, err := time.LoadLocation("Europe/Lisbon")
	if err != nil {
		t.Fatal(err)
	}
	last := time.Date(2026, 3, 28, 2, 0, 5, 0, time.UTC)
	run := &state.StateBackupRun{Status: "succeeded", Finished: last, Size: 2048}
	h := state.StateBackupHistory{Last: run, Success: run}
	sb := manifest.StateBackup{Destination: "offsite", Schedule: "0 2 * * *"}
	now := time.Date(2026, 3, 29, 3, 30, 0, 0, time.UTC) // two and a half hours after 02:00 local, which is 01:00 UTC
	if got := stateBackupResult(sb, lisbon, h, last, now); got.State != monitor.Critical || !strings.Contains(got.Detail, "overdue") {
		t.Errorf("in Lisbon: %v %q, want an overdue critical", got.State, got.Detail)
	}
	if got := stateBackupResult(sb, time.UTC, h, last, now); got.State != monitor.OK {
		t.Errorf("in UTC: %v %q, want ok", got.State, got.Detail)
	}
}
