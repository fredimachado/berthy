package state

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func backupRun(backup string, n int) BackupRun {
	return BackupRun{
		Backup:      backup,
		App:         "files",
		Volume:      "uploads",
		Destination: "offsite",
		ObjectKey:   "files/uploads/" + backup + "/" + minute(n).Format("150405") + ".tar.zst",
		Trigger:     "manual",
		Started:     minute(n),
	}
}

func TestStartBackupRecordsARunningRun(t *testing.T) {
	s, _, _ := testStore(t)
	ctx := context.Background()
	id, err := s.StartBackup(ctx, backupRun("uploads", 1))
	if err != nil || id == 0 {
		t.Fatalf("start: %d %v", id, err)
	}
	runs, err := s.BackupRuns(ctx, "uploads", 10)
	if err != nil || len(runs) != 1 {
		t.Fatalf("runs: %+v %v", runs, err)
	}
	r := runs[0]
	if r.ID != id || r.Status != "running" || !r.Finished.IsZero() || r.Size != 0 || r.SHA256 != "" || r.Detail != "" {
		t.Fatalf("running run: %+v", r)
	}
	if r.Backup != "uploads" || r.App != "files" || r.Volume != "uploads" || r.Destination != "offsite" || r.Trigger != "manual" || !r.Started.Equal(minute(1)) {
		t.Fatalf("fields: %+v", r)
	}
}

func TestStartBackupRefusesInvalidRuns(t *testing.T) {
	s, _, _ := testStore(t)
	ctx := context.Background()
	valid := backupRun("uploads", 1)
	cases := map[string]func(*BackupRun){
		"no backup":      func(b *BackupRun) { b.Backup = "" },
		"no app":         func(b *BackupRun) { b.App = "" },
		"no volume":      func(b *BackupRun) { b.Volume = "" },
		"no destination": func(b *BackupRun) { b.Destination = "" },
		"no object key":  func(b *BackupRun) { b.ObjectKey = "" },
		"unknown trig":   func(b *BackupRun) { b.Trigger = "cron" },
		"no started":     func(b *BackupRun) { b.Started = time.Time{} },
		"finished":       func(b *BackupRun) { b.Status = "succeeded" },
	}
	for name, mutate := range cases {
		b := valid
		mutate(&b)
		if _, err := s.StartBackup(ctx, b); err == nil || errors.Is(err, ErrConflict) {
			t.Errorf("%s accepted: %v", name, err)
		}
	}
	if runs, _ := s.BackupRuns(ctx, "", 10); len(runs) != 0 {
		t.Fatalf("invalid runs stored: %+v", runs)
	}
}

func TestFinishBackupRecordsTheOutcomeOnce(t *testing.T) {
	s, _, _ := testStore(t)
	ctx := context.Background()
	id, err := s.StartBackup(ctx, backupRun("uploads", 1))
	if err != nil {
		t.Fatal(err)
	}
	sum := strings.Repeat("ab", 32)
	if err = s.FinishBackup(ctx, id, "succeeded", 2048, sum, ""); err != nil {
		t.Fatalf("finish: %v", err)
	}
	runs, _ := s.BackupRuns(ctx, "uploads", 1)
	r := runs[0]
	if r.Status != "succeeded" || r.Size != 2048 || r.SHA256 != sum || r.Finished.IsZero() {
		t.Fatalf("finished run: %+v", r)
	}
	if err = s.FinishBackup(ctx, id, "failed", 0, "", "late"); !errors.Is(err, ErrConflict) {
		t.Fatalf("second finish: %v", err)
	}
	if err = s.FinishBackup(ctx, id+100, "failed", 0, "", "x"); !errors.Is(err, ErrConflict) {
		t.Fatalf("unknown run: %v", err)
	}
	pending, _ := s.StartBackup(ctx, backupRun("uploads", 2))
	if err = s.FinishBackup(ctx, pending, "running", 0, "", ""); err == nil || errors.Is(err, ErrConflict) {
		t.Fatalf("running is not a finish status: %v", err)
	}
	if err = s.FinishBackup(ctx, pending, "succeeded", 1, "not-a-checksum", ""); err == nil {
		t.Fatal("succeeded run without a sha256 accepted")
	}
	if err = s.FinishBackup(ctx, pending, "failed", 0, "", "upload failed: access denied"); err != nil {
		t.Fatalf("failed finish: %v", err)
	}
}

func TestBackupRunsAreNewestFirstAndFilterByBackup(t *testing.T) {
	s, _, _ := testStore(t)
	ctx := context.Background()
	var ids []int64
	for n, backup := range []string{"uploads", "uploads", "uploads", "other"} {
		id, err := s.StartBackup(ctx, backupRun(backup, n+1))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	runs, err := s.BackupRuns(ctx, "uploads", 10)
	if err != nil || len(runs) != 3 || runs[0].ID != ids[2] || runs[2].ID != ids[0] {
		t.Fatalf("uploads newest first: %+v %v", runs, err)
	}
	if all, _ := s.BackupRuns(ctx, "", 10); len(all) != 4 || all[0].ID != ids[3] {
		t.Fatalf("all runs: %+v", all)
	}
	if two, _ := s.BackupRuns(ctx, "uploads", 2); len(two) != 2 || two[0].ID != ids[2] || two[1].ID != ids[1] {
		t.Fatalf("limit: %+v", two)
	}
	if _, err := s.BackupRuns(ctx, "uploads", 0); err == nil {
		t.Fatal("limit 0 accepted")
	}
}

func TestLastSuccessfulBackupsIgnoresFailedAndRunningRuns(t *testing.T) {
	s, _, _ := testStore(t)
	ctx := context.Background()
	sum := strings.Repeat("cd", 32)
	first, _ := s.StartBackup(ctx, backupRun("uploads", 1))
	if err := s.FinishBackup(ctx, first, "succeeded", 10, sum, ""); err != nil {
		t.Fatal(err)
	}
	second, _ := s.StartBackup(ctx, backupRun("uploads", 2))
	if err := s.FinishBackup(ctx, second, "failed", 0, "", "upload failed: object storage unavailable"); err != nil {
		t.Fatal(err)
	}
	s.StartBackup(ctx, backupRun("uploads", 3))
	onlyFailed, _ := s.StartBackup(ctx, backupRun("other", 4))
	if err := s.FinishBackup(ctx, onlyFailed, "failed", 0, "", "archive failed"); err != nil {
		t.Fatal(err)
	}
	s.StartBackup(ctx, backupRun("pending", 5))

	last, err := s.LastSuccessfulBackups(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(last) != 1 || last["uploads"].ID != first || last["uploads"].Size != 10 || last["uploads"].SHA256 != sum {
		t.Fatalf("last successful: %+v", last)
	}

	newer, _ := s.StartBackup(ctx, backupRun("uploads", 6))
	if err := s.FinishBackup(ctx, newer, "succeeded", 20, sum, ""); err != nil {
		t.Fatal(err)
	}
	last, _ = s.LastSuccessfulBackups(ctx)
	if last["uploads"].ID != newer {
		t.Fatalf("newer success not preferred: %+v", last["uploads"])
	}
}

func TestAbandonRunningBackupsFailsOnlyRunningRuns(t *testing.T) {
	s, _, _ := testStore(t)
	ctx := context.Background()
	crashed, _ := s.StartBackup(ctx, backupRun("uploads", 1))
	done, _ := s.StartBackup(ctx, backupRun("uploads", 2))
	if err := s.FinishBackup(ctx, done, "succeeded", 5, strings.Repeat("ef", 32), ""); err != nil {
		t.Fatal(err)
	}
	other, _ := s.StartBackup(ctx, backupRun("other", 3))

	n, err := s.AbandonRunningBackups(ctx)
	if err != nil || n != 2 {
		t.Fatalf("abandon: %d %v", n, err)
	}
	runs, _ := s.BackupRuns(ctx, "", 10)
	byID := map[int64]BackupRun{}
	for _, r := range runs {
		byID[r.ID] = r
	}
	for _, id := range []int64{crashed, other} {
		r := byID[id]
		if r.Status != "failed" || r.Detail != AbandonedBackupDetail || r.Finished.IsZero() {
			t.Fatalf("abandoned run %d: %+v", id, r)
		}
	}
	if byID[done].Status != "succeeded" {
		t.Fatalf("finished run changed: %+v", byID[done])
	}
	if n, err = s.AbandonRunningBackups(ctx); err != nil || n != 0 {
		t.Fatalf("second abandon: %d %v", n, err)
	}
	if r := byID[crashed]; r.Detail != "agent restarted during the backup" {
		t.Fatalf("detail: %q", r.Detail)
	}
}

func TestStartBackupRefusesAnObjectKeyAlreadyRecorded(t *testing.T) {
	s, dir, key := testStore(t)
	ctx := context.Background()
	run := backupRun("uploads", 1)
	id, err := s.StartBackup(ctx, run)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.StartBackup(ctx, run); !errors.Is(err, ErrConflict) {
		t.Fatalf("same key while running: %v", err)
	}
	if err = s.FinishBackup(ctx, id, "failed", 0, "", "archive failed"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.StartBackup(ctx, run); !errors.Is(err, ErrConflict) {
		t.Fatalf("same key after the run finished: %v", err)
	}
	s.Close()

	other, err := Open(dir, key, "other")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = other.StartBackup(ctx, run); err != nil {
		t.Fatalf("same key in another environment: %v", err)
	}
	other.Close()

	again, err := Open(dir, key, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	if _, err = again.StartBackup(ctx, run); !errors.Is(err, ErrConflict) {
		t.Fatalf("same key after reopening: %v", err)
	}
}

func TestBackupRunsAreScopedToTheEnvironment(t *testing.T) {
	s, dir, key := testStore(t)
	ctx := context.Background()
	if _, err := s.StartBackup(ctx, backupRun("uploads", 1)); err != nil {
		t.Fatal(err)
	}
	s.Close()
	other, err := Open(dir, key, "other")
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if runs, _ := other.BackupRuns(ctx, "", 10); len(runs) != 0 {
		t.Fatalf("another environment sees runs: %+v", runs)
	}
}

func TestUpgradeVersionSixAddsBackupsTable(t *testing.T) {
	s, dir, key := testStore(t)
	if _, err := s.db.Exec(`DROP TABLE backups; PRAGMA user_version=6`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	upgraded, err := Open(dir, key, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()
	var version int
	if err = upgraded.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 10 {
		t.Fatalf("migration: %d %v", version, err)
	}
	ctx := context.Background()
	id, err := upgraded.StartBackup(ctx, backupRun("uploads", 1))
	if err != nil {
		t.Fatalf("backups table unusable after upgrade: %v", err)
	}
	if err = upgraded.FinishBackup(ctx, id, "failed", 0, "", "x"); err != nil {
		t.Fatalf("finish after upgrade: %v", err)
	}
	if _, err = upgraded.StartBackup(ctx, backupRun("uploads", 1)); !errors.Is(err, ErrConflict) {
		t.Fatalf("object key index missing after upgrade: %v", err)
	}
}

func TestSetBackupObjectKeyRenamesARunningRunOnly(t *testing.T) {
	s, _, _ := testStore(t)
	ctx := context.Background()
	id, err := s.StartBackup(ctx, backupRun("uploads", 1))
	if err != nil {
		t.Fatal(err)
	}
	other, _ := s.StartBackup(ctx, backupRun("uploads", 2))
	if err = s.SetBackupObjectKey(ctx, id, "files/uploads/uploads/final-1.tar.gz"); err != nil {
		t.Fatalf("set: %v", err)
	}
	runs, _ := s.BackupRuns(ctx, "uploads", 10)
	var got BackupRun
	for _, r := range runs {
		if r.ID == id {
			got = r
		}
	}
	if got.ObjectKey != "files/uploads/uploads/final-1.tar.gz" {
		t.Fatalf("key: %q", got.ObjectKey)
	}
	if err = s.SetBackupObjectKey(ctx, other, "files/uploads/uploads/final-1.tar.gz"); !errors.Is(err, ErrConflict) {
		t.Fatalf("a key that is taken: %v", err)
	}
	if err = s.SetBackupObjectKey(ctx, other, ""); err == nil {
		t.Fatal("empty key accepted")
	}
	if err = s.FinishBackup(ctx, id, "failed", 0, "", "x"); err != nil {
		t.Fatal(err)
	}
	if err = s.SetBackupObjectKey(ctx, id, "files/uploads/uploads/late.tar.gz"); !errors.Is(err, ErrConflict) {
		t.Fatalf("a finished run: %v", err)
	}
}

func scheduledRun(backup string, n int) BackupRun {
	r := backupRun(backup, n)
	r.Trigger = "schedule"
	r.Scheduled = minute(n)
	return r
}

func TestAScheduledOccurrenceIsRecordedOnce(t *testing.T) {
	s, _, _ := testStore(t)
	ctx := context.Background()
	first := scheduledRun("uploads", 1)
	id, err := s.StartBackup(ctx, first)
	if err != nil || id == 0 {
		t.Fatalf("first: %d %v", id, err)
	}
	// The same occurrence again, whatever object key it brings.
	again := first
	again.ObjectKey = "files/uploads/uploads/another.tar.gz"
	if _, err = s.StartBackup(ctx, again); !errors.Is(err, ErrConflict) {
		t.Fatalf("second run of the occurrence: %v", err)
	}
	// Another occurrence, another backup's occurrence, and manual runs are unaffected.
	if _, err = s.StartBackup(ctx, scheduledRun("uploads", 2)); err != nil {
		t.Fatalf("next occurrence: %v", err)
	}
	other := scheduledRun("other", 1)
	other.ObjectKey = "files/uploads/other/x.tar.gz"
	if _, err = s.StartBackup(ctx, other); err != nil {
		t.Fatalf("other backup: %v", err)
	}
	for i := 0; i < 2; i++ {
		m := backupRun("uploads", 10+i)
		m.Trigger = "manual"
		if _, err = s.StartBackup(ctx, m); err != nil {
			t.Fatalf("manual run %d: %v", i, err)
		}
	}
	runs, _ := s.BackupRuns(ctx, "uploads", 10)
	var found bool
	for _, r := range runs {
		if r.ID == id {
			found = r.Scheduled.Equal(minute(1)) && r.Trigger == "schedule"
		}
	}
	if !found {
		t.Fatalf("the scheduled run lost its occurrence: %+v", runs)
	}
}

func TestAScheduledRunNeedsItsOccurrenceAndOnlyItDoes(t *testing.T) {
	s, _, _ := testStore(t)
	ctx := context.Background()
	noOccurrence := backupRun("uploads", 1)
	noOccurrence.Trigger = "schedule"
	if _, err := s.StartBackup(ctx, noOccurrence); err == nil || errors.Is(err, ErrConflict) {
		t.Fatalf("a scheduled run without an occurrence: %v", err)
	}
	manual := backupRun("uploads", 2)
	manual.Scheduled = minute(2)
	if _, err := s.StartBackup(ctx, manual); err == nil || errors.Is(err, ErrConflict) {
		t.Fatalf("a manual run with an occurrence: %v", err)
	}
}

func TestBackupHistoryNamesTheLastFinishedRunAndTheLastTwoSuccesses(t *testing.T) {
	s, _, _ := testStore(t)
	ctx := context.Background()
	h, err := s.BackupHistory(ctx, "uploads")
	if err != nil || h.Last != nil || len(h.Successes) != 0 {
		t.Fatalf("empty history: %+v %v", h, err)
	}
	sum := strings.Repeat("ab", 32)
	finish := func(n int, status string, size int64, detail string) {
		t.Helper()
		id, err := s.StartBackup(ctx, backupRun("uploads", n))
		if err != nil {
			t.Fatal(err)
		}
		sh := ""
		if status == "succeeded" {
			sh = sum
		}
		if err = s.FinishBackup(ctx, id, status, size, sh, detail); err != nil {
			t.Fatal(err)
		}
	}
	finish(1, "succeeded", 100, "")
	finish(2, "succeeded", 200, "")
	finish(3, "failed", 0, "upload failed: access denied")
	s.StartBackup(ctx, backupRun("uploads", 4)) // still running
	h, _ = s.BackupHistory(ctx, "uploads")
	if h.Last == nil || h.Last.Status != "failed" || h.Last.Detail != "upload failed: access denied" || len(h.Successes) != 2 || h.Successes[0].Size != 200 || h.Successes[1].Size != 100 {
		t.Fatalf("history: %+v", h)
	}
	// A run that the agent's stop or restart cut short says nothing about the backup, as an interrupted job does.
	finish(5, "failed", 0, StoppedBackupDetail)
	finish(6, "failed", 0, AbandonedBackupDetail)
	h, _ = s.BackupHistory(ctx, "uploads")
	if h.Last == nil || h.Last.Detail != "upload failed: access denied" {
		t.Fatalf("history after interrupted runs: %+v", h.Last)
	}
	finish(7, "succeeded", 300, "")
	h, _ = s.BackupHistory(ctx, "uploads")
	if h.Last.Status != "succeeded" || h.Successes[0].Size != 300 || h.Successes[1].Size != 200 {
		t.Fatalf("history after a success: %+v", h)
	}
	if other, _ := s.BackupHistory(ctx, "other"); other.Last != nil {
		t.Fatalf("another backup's history: %+v", other)
	}
}

func TestUpgradeVersionNineAddsTheOccurrenceOfBackups(t *testing.T) {
	s, dir, key := testStore(t)
	ctx := context.Background()
	id, err := s.StartBackup(ctx, backupRun("uploads", 1))
	if err != nil {
		t.Fatal(err)
	}
	// A version 9 database: no occurrence column and no occurrence index.
	if _, err = s.db.Exec(`DROP INDEX backups_occurrence; ALTER TABLE backups DROP COLUMN scheduled; PRAGMA user_version=9`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	upgraded, err := Open(dir, key, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()
	var version int
	if err = upgraded.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 10 {
		t.Fatalf("migration: %d %v", version, err)
	}
	runs, _ := upgraded.BackupRuns(ctx, "uploads", 10)
	if len(runs) != 1 || runs[0].ID != id || !runs[0].Scheduled.IsZero() {
		t.Fatalf("the old run after the upgrade: %+v", runs)
	}
	if _, err = upgraded.StartBackup(ctx, scheduledRun("uploads", 3)); err != nil {
		t.Fatalf("scheduled run after the upgrade: %v", err)
	}
	if _, err = upgraded.StartBackup(ctx, scheduledRun("uploads", 3)); !errors.Is(err, ErrConflict) {
		t.Fatalf("occurrence index missing after the upgrade: %v", err)
	}
	upgraded.Close()
	again, err := Open(dir, key, "test")
	if err != nil {
		t.Fatalf("a second open after the upgrade: %v", err)
	}
	again.Close()
}
