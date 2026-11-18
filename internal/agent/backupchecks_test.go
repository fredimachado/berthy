package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"berthy/internal/deploy"
	"berthy/internal/manifest"
	"berthy/internal/monitor"
	"berthy/internal/state"
)

func at(h, m, s int) time.Time { return time.Date(2026, 11, 16, h, m, s, 0, time.UTC) }

func success(finished time.Time, size int64) state.BackupRun {
	return state.BackupRun{Status: "succeeded", Finished: finished, Started: finished.Add(-5 * time.Second), Size: size}
}

func failure(detail string) *state.BackupRun {
	return &state.BackupRun{Status: "failed", Detail: detail, Finished: at(9, 0, 0)}
}

func TestBackupResultReportsAFailedRunWithItsDetail(t *testing.T) {
	b := manifest.Backup{Schedule: "0 * * * *"}
	for _, detail := range []string{"preparation failed: exit status 3", "upload failed: access denied"} {
		s := success(at(9, 0, 5), 100)
		got := backupResult(b, time.UTC, state.BackupHistory{Last: failure(detail), Successes: []state.BackupRun{s}}, at(1, 0, 0), at(9, 30, 0))
		if got.State != monitor.Critical || got.Detail != detail {
			t.Errorf("%s: %+v", detail, got)
		}
	}
	// A failure wins over an overdue backup, and a success after it clears it.
	got := backupResult(b, time.UTC, state.BackupHistory{Last: failure("archive failed"), Successes: []state.BackupRun{success(at(1, 0, 5), 100)}}, at(1, 0, 0), at(9, 30, 0))
	if got.Detail != "archive failed" {
		t.Fatalf("failure and overdue: %+v", got)
	}
	ok := success(at(9, 0, 5), 100)
	if got = backupResult(b, time.UTC, state.BackupHistory{Last: &ok, Successes: []state.BackupRun{ok}}, at(1, 0, 0), at(9, 30, 0)); got.State != monitor.OK {
		t.Fatalf("after a success: %+v", got)
	}
}

func TestBackupResultOverdueMathHourlyDailyAndEveryMinute(t *testing.T) {
	last := func(finished time.Time) state.BackupHistory {
		s := success(finished, 100)
		return state.BackupHistory{Last: &s, Successes: []state.BackupRun{s}}
	}
	cases := []struct {
		name    string
		cron    string
		success time.Time
		now     time.Time
		overdue bool
		by      string
	}{
		// Hourly: the next occurrence after 10:00:30 is 11:00, and the grace is the hour between occurrences.
		{"hourly, still allowed", "0 * * * *", at(10, 0, 30), at(11, 59, 59), false, ""},
		{"hourly, overdue", "0 * * * *", at(10, 0, 30), at(12, 0, 1), true, "2026-11-16T12:00:00Z"},
		// Daily at 02:30: the next occurrence is the next day and the grace is one hour, not a day.
		{"daily, within the grace", "30 2 * * *", time.Date(2026, 11, 15, 2, 30, 20, 0, time.UTC), at(3, 30, 0), false, ""},
		{"daily, overdue", "30 2 * * *", time.Date(2026, 11, 15, 2, 30, 20, 0, time.UTC), at(3, 31, 0), true, "2026-11-16T03:30:00Z"},
		// Every minute: the grace is the minute between occurrences.
		{"every minute, within the grace", "* * * * *", at(10, 0, 5), at(10, 2, 0), false, ""},
		{"every minute, overdue", "* * * * *", at(10, 0, 5), at(10, 2, 1), true, "2026-11-16T10:02:00Z"},
	}
	for _, c := range cases {
		got := backupResult(manifest.Backup{Schedule: c.cron}, time.UTC, last(c.success), at(0, 0, 0), c.now)
		if c.overdue {
			want := "overdue: last success " + c.success.UTC().Format(time.RFC3339) + " (expected by " + c.by + ")"
			if got.State != monitor.Critical || got.Detail != want {
				t.Errorf("%s: %+v, want %q", c.name, got, want)
			}
		} else if got.State != monitor.OK {
			t.Errorf("%s: %+v", c.name, got)
		}
	}
	// Without a schedule a backup is never overdue.
	if got := backupResult(manifest.Backup{}, time.UTC, last(at(1, 0, 0)), at(0, 0, 0), time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)); got.State != monitor.OK {
		t.Fatalf("no schedule: %+v", got)
	}
}

func TestBackupResultNeverRunHasAGraceFromTheFirstSighting(t *testing.T) {
	b := manifest.Backup{Schedule: "0 * * * *"}
	seen := at(10, 0, 10)
	if got := backupResult(b, time.UTC, state.BackupHistory{}, seen, at(11, 59, 0)); got.State != monitor.Unknown || got.Detail != "not run yet" {
		t.Fatalf("within the grace: %+v", got)
	}
	got := backupResult(b, time.UTC, state.BackupHistory{}, seen, at(12, 0, 1))
	if got.State != monitor.Critical || !strings.HasPrefix(got.Detail, "overdue: no success yet") || !strings.Contains(got.Detail, "expected by 2026-11-16T12:00:00Z") {
		t.Fatalf("overdue: %+v", got)
	}
	if got = backupResult(manifest.Backup{}, time.UTC, state.BackupHistory{}, seen, at(23, 0, 0)); got.State != monitor.Unknown {
		t.Fatalf("no schedule, never run: %+v", got)
	}
}

func TestBackupResultWarnsWhenTheBackupShrankBelowHalf(t *testing.T) {
	b := manifest.Backup{}
	hist := func(newest, previous int64) state.BackupHistory {
		n, p := success(at(10, 0, 0), newest), success(at(9, 0, 0), previous)
		return state.BackupHistory{Last: &n, Successes: []state.BackupRun{n, p}}
	}
	got := backupResult(b, time.UTC, hist(40, 100), at(0, 0, 0), at(10, 5, 0))
	if got.State != monitor.Warning || got.Detail != "size dropped to 40% of the previous backup" {
		t.Fatalf("40%%: %+v", got)
	}
	if got = backupResult(b, time.UTC, hist(271, 3074018), at(0, 0, 0), at(10, 5, 0)); got.State != monitor.Warning || got.Detail != "size dropped to under 1% of the previous backup" {
		t.Fatalf("a volume that was emptied: %+v", got)
	}
	for name, h := range map[string]state.BackupHistory{"exactly half": hist(50, 100), "grew": hist(300, 100), "previous empty": hist(0, 0)} {
		if got = backupResult(b, time.UTC, h, at(0, 0, 0), at(10, 5, 0)); got.State != monitor.OK {
			t.Errorf("%s: %+v", name, got)
		}
	}
	one := success(at(10, 0, 0), 5)
	if got = backupResult(b, time.UTC, state.BackupHistory{Last: &one, Successes: []state.BackupRun{one}}, at(0, 0, 0), at(10, 5, 0)); got.State != monitor.OK {
		t.Fatalf("one success: %+v", got)
	}
	// Overdue is critical and outranks the warning.
	got = backupResult(manifest.Backup{Schedule: "0 * * * *"}, time.UTC, hist(40, 100), at(0, 0, 0), at(14, 0, 0))
	if got.State != monitor.Critical {
		t.Fatalf("overdue and shrunk: %+v", got)
	}
}

func TestBackupResultOkSaysHowOldAndHowBig(t *testing.T) {
	s := success(at(10, 0, 0), 1536)
	got := backupResult(manifest.Backup{Schedule: "0 * * * *"}, time.UTC, state.BackupHistory{Last: &s, Successes: []state.BackupRun{s}}, at(0, 0, 0), at(10, 5, 30))
	if got.State != monitor.OK || got.Detail != "last success 5m ago, 1.5 KiB" {
		t.Fatalf("%+v", got)
	}
}

func TestBackupChecksAreOnePerBackupOfTheDeployedManifest(t *testing.T) {
	s := openIngressStore(t)
	ctx := context.Background()
	recordRelease(t, s, "release-backups", `{"project":"demo","network":"demo",
		"apps":{"api":{"kind":"image","image":"ghcr.io/acme/api","roles":{"web":{"volumes":["data:/data"]}}},"gone":{"kind":"image","image":"ghcr.io/acme/gone","roles":{"web":{}}}},
		"destinations":{"offsite":{"type":"s3","bucket":"bkt","access_key_secret":"A","secret_key_secret":"B"}},
		"backups":{"data":{"app":"api","volume":"data","destination":"offsite","schedule":"0 * * * *"},"manual":{"app":"api","volume":"data","destination":"offsite"},"old":{"app":"gone","volume":"x","destination":"offsite"}}}`)
	w := NewMonitoring(monitoringConfig(""), nil, nil, &deploy.Controller{Store: s, Environment: "lab"}, func(string) {})
	now := time.Now().UTC()
	w.now = func() time.Time { return time.Now().UTC() }
	checks := map[string]monitor.Check{}
	for _, c := range w.checks(ctx) {
		checks[c.Name()] = c
	}
	// recordRelease deploys every app, so "old" is a backup of a deployed app here; the filter itself is tested elsewhere.
	data := checks["backup:data"]
	if data == nil || checks["backup:manual"] == nil {
		t.Fatalf("checks: %d", len(checks))
	}
	if data.Interval() != time.Minute || data.Failures() != 1 {
		t.Fatalf("check settings: %v %d", data.Interval(), data.Failures())
	}
	if got := data.Run(ctx); got.State != monitor.Unknown || got.Detail != "not run yet" {
		t.Fatalf("before any run: %+v", got)
	}
	id, _ := s.StartBackup(ctx, state.BackupRun{Backup: "data", App: "api", Volume: "data", Destination: "offsite", ObjectKey: "k1", Trigger: "manual", Started: now.Add(-time.Hour)})
	if err := s.FinishBackup(ctx, id, "failed", 0, "", "upload failed: access denied"); err != nil {
		t.Fatal(err)
	}
	if got := data.Run(ctx); got.State != monitor.Critical || got.Detail != "upload failed: access denied" {
		t.Fatalf("after a failed run: %+v", got)
	}
	id, _ = s.StartBackup(ctx, state.BackupRun{Backup: "data", App: "api", Volume: "data", Destination: "offsite", ObjectKey: "k2", Trigger: "manual", Started: now.Add(-time.Minute)})
	if err := s.FinishBackup(ctx, id, "succeeded", 2048, strings.Repeat("ab", 32), ""); err != nil {
		t.Fatal(err)
	}
	if got := data.Run(ctx); got.State != monitor.OK || !strings.HasPrefix(got.Detail, "last success ") || !strings.Contains(got.Detail, "2.0 KiB") {
		t.Fatalf("after a success: %+v", got)
	}
}
