package state

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// openKeyed opens a state directory with a random key, which the snapshot tests need: a zero key could match zero bytes by chance.
func openKeyed(t *testing.T, dir string, key []byte) *Store {
	t.Helper()
	keyPath := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(keyPath, key, 0600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir, keyPath, "prod")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestSnapshotOpensWithTheSameKeyAndHoldsNoKeyOrPlaintext(t *testing.T) {
	ctx := context.Background()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	s := openKeyed(t, dir, key)
	if err := s.PutSecret(ctx, Actor{ID: "ops", Name: "ops"}, SecretInfo{Name: "DB_PASSWORD"}, []byte("plaintext-secret-value")); err != nil {
		t.Fatal(err)
	}
	copyDir := filepath.Join(t.TempDir(), "copy")
	if err := os.Mkdir(copyDir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(copyDir, "state.db")
	if _, err := s.Snapshot(ctx, path); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, key) || bytes.Contains(raw, []byte("plaintext-secret-value")) {
		t.Fatal("the snapshot holds the key or a plaintext secret")
	}
	if runtime.GOOS != "windows" {
		if info, _ := os.Stat(path); info.Mode().Perm() != 0600 {
			t.Fatalf("snapshot mode %v", info.Mode().Perm())
		}
	}
	if _, err = s.Snapshot(ctx, path); err == nil {
		t.Fatal("a snapshot over an existing file was written")
	}
	keyPath := filepath.Join(t.TempDir(), "key")
	if err = os.WriteFile(keyPath, key, 0600); err != nil {
		t.Fatal(err)
	}
	copied, err := Open(copyDir, keyPath, "prod")
	if err != nil {
		t.Fatalf("open the snapshot: %v", err)
	}
	defer copied.Close()
	if _, value, err := copied.Secret(ctx, "DB_PASSWORD"); err != nil || string(value) != "plaintext-secret-value" {
		t.Fatalf("secret in the snapshot: %q %v", value, err)
	}
	if version, err := copied.SchemaVersion(ctx); err != nil || version != 18 {
		t.Fatalf("schema version %d %v", version, err)
	}
}

func TestAWrongKeyIsReportedAsWrongKey(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	key := bytes.Repeat([]byte{7}, 32)
	openKeyed(t, dir, key).Close()
	other := bytes.Repeat([]byte{9}, 32)
	otherPath := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(otherPath, other, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir, otherPath, "prod"); !errors.Is(err, ErrWrongKey) {
		t.Fatalf("open with another key: %v", err)
	}
}

func TestStateBackupOccurrenceIsRecordedOnce(t *testing.T) {
	s, _, _ := testStore(t)
	ctx := context.Background()
	at := time.Date(2026, 11, 16, 3, 17, 0, 0, time.UTC)
	scheduled := StateBackupRun{Trigger: "schedule", Scheduled: at, Started: at.Add(5 * time.Second), Key: "berthy-state/test/a.db"}
	id, err := s.StartStateBackup(ctx, scheduled)
	if err != nil || id == 0 {
		t.Fatalf("first occurrence: %d %v", id, err)
	}
	again := scheduled
	again.Key = "berthy-state/test/b.db"
	if _, err = s.StartStateBackup(ctx, again); !errors.Is(err, ErrConflict) {
		t.Fatalf("second record of one occurrence: %v", err)
	}
	for i := range 2 {
		manual := StateBackupRun{Trigger: "manual", Started: at.Add(time.Duration(i+1) * time.Minute), Key: "berthy-state/test/m" + string(rune('0'+i)) + ".db"}
		if _, err = s.StartStateBackup(ctx, manual); err != nil {
			t.Fatalf("manual run %d: %v", i, err)
		}
	}
	for _, bad := range []StateBackupRun{
		{Trigger: "schedule", Started: at, Key: "k.db"},
		{Trigger: "manual", Scheduled: at, Started: at, Key: "k.db"},
		{Trigger: "hook", Started: at, Key: "k.db"},
		{Trigger: "manual", Started: at},
	} {
		if _, err = s.StartStateBackup(ctx, bad); err == nil {
			t.Errorf("invalid run accepted: %+v", bad)
		}
	}
	if err = s.FinishStateBackup(ctx, id, "succeeded", 42, "", ""); err == nil {
		t.Fatal("a succeeded run without a checksum was finished")
	}
	sum := "ab" + string(bytes.Repeat([]byte("0"), 62))
	if err = s.FinishStateBackup(ctx, id, "succeeded", 42, sum, ""); err != nil {
		t.Fatal(err)
	}
	if err = s.FinishStateBackup(ctx, id, "failed", 0, "", "late"); !errors.Is(err, ErrConflict) {
		t.Fatalf("a run finished twice: %v", err)
	}
	h, err := s.StateBackupHistory(ctx)
	if err != nil || h.Success == nil || h.Success.Size != 42 || h.Success.SHA256 != sum || h.Last == nil || h.Last.Status != "succeeded" {
		t.Fatalf("history: %+v %v", h, err)
	}
	runs, err := s.StateBackupRuns(ctx, 10)
	if err != nil || len(runs) != 3 || runs[0].Trigger != "manual" || runs[2].ID != id {
		t.Fatalf("runs newest first: %+v %v", runs, err)
	}
}

func TestAbandonedAndStoppedStateBackupsAreNotTheLastRun(t *testing.T) {
	s, _, _ := testStore(t)
	ctx := context.Background()
	at := time.Date(2026, 11, 16, 3, 17, 0, 0, time.UTC)
	ok, err := s.StartStateBackup(ctx, StateBackupRun{Trigger: "manual", Started: at, Key: "one.db"})
	if err != nil {
		t.Fatal(err)
	}
	sum := bytes.Repeat([]byte("1"), 64)
	if err = s.FinishStateBackup(ctx, ok, "succeeded", 10, string(sum), ""); err != nil {
		t.Fatal(err)
	}
	if _, err = s.StartStateBackup(ctx, StateBackupRun{Trigger: "manual", Started: at.Add(time.Minute), Key: "two.db"}); err != nil {
		t.Fatal(err)
	}
	if n, err := s.AbandonRunningStateBackups(ctx); err != nil || n != 1 {
		t.Fatalf("abandon: %d %v", n, err)
	}
	stopped, _ := s.StartStateBackup(ctx, StateBackupRun{Trigger: "manual", Started: at.Add(2 * time.Minute), Key: "three.db"})
	if err = s.FinishStateBackup(ctx, stopped, "failed", 0, "", StoppedStateBackupDetail); err != nil {
		t.Fatal(err)
	}
	h, err := s.StateBackupHistory(ctx)
	if err != nil || h.Last == nil || h.Last.Status != "succeeded" || h.Success == nil || h.Success.Key != "one.db" {
		t.Fatalf("history after interruptions: %+v %v", h, err)
	}
}

func TestSchemaTenUpgradesToElevenWithTheStateBackupTable(t *testing.T) {
	s, dir, key := testStore(t)
	if _, err := s.db.Exec(`DROP TABLE state_backups; PRAGMA user_version=10`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	upgraded, err := Open(dir, key, "test")
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer upgraded.Close()
	if version, err := upgraded.SchemaVersion(context.Background()); err != nil || version != 18 {
		t.Fatalf("version after upgrade: %d %v", version, err)
	}
	if _, err = upgraded.StartStateBackup(context.Background(), StateBackupRun{Trigger: "manual", Started: time.Now(), Key: "x.db"}); err != nil {
		t.Fatalf("table after upgrade: %v", err)
	}
}

func TestStateBackupHistoryKeepsTheNewest500RunsAndTheNewestSuccessAndRunningRun(t *testing.T) {
	s, _, _ := testStore(t)
	ctx := context.Background()
	start := func(n int) int64 {
		t.Helper()
		id, err := s.StartStateBackup(ctx, StateBackupRun{Trigger: "manual", Started: minute(n), Key: "berthy-state/test/" + minute(n).Format("150405") + ".db"})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	running := start(0)
	success := start(1)
	if err := s.FinishStateBackup(ctx, success, "succeeded", 10, strings.Repeat("a", 64), ""); err != nil {
		t.Fatal(err)
	}
	for n := 2; n < 2+maxRunsKept+10; n++ {
		id := start(n)
		if err := s.FinishStateBackup(ctx, id, "failed", 0, "", "refused"); err != nil {
			t.Fatal(err)
		}
	}
	runs, err := s.StateBackupRuns(ctx, 5000)
	if err != nil || len(runs) != maxRunsKept+2 {
		t.Fatalf("kept %d runs, want %d: %v", len(runs), maxRunsKept+2, err)
	}
	statuses := map[int64]string{}
	for _, r := range runs {
		statuses[r.ID] = r.Status
	}
	if statuses[running] != "running" {
		t.Fatalf("the running run was pruned or changed: %q", statuses[running])
	}
	if statuses[success] != "succeeded" {
		t.Fatalf("the newest success was pruned: %q", statuses[success])
	}
	if h, err := s.StateBackupHistory(ctx); err != nil || h.Success == nil || h.Success.ID != success {
		t.Fatalf("history success: %+v %v", h.Success, err)
	}
}
