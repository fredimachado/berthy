package backup

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"berthy/internal/jobs"
	"berthy/internal/manifest"
)

// fakeCommands is the agent's way to run a command: it plays one run of the command of a backup or a restore.
type fakeCommands struct {
	mu    sync.Mutex
	calls []commandCall
	// out is what the command writes to its standard output, one write per element.
	out [][]byte
	// outcome is how the command ends, after it has written out.
	outcome jobs.Outcome
	// err is the error of the run, which is the writer's when an upload failed.
	err error
	// in is what the command read of its input.
	in []byte
	// started is closed once a run has begun.
	hold chan struct{}
}

type commandCall struct {
	Kind, Name, ID string
	Job            manifest.Job
	Deadline       time.Duration
}

func exited(code int) jobs.Outcome { return jobs.Outcome{Exit: &code} }

func (f *fakeCommands) RunCommand(ctx context.Context, _ manifest.Manifest, kind, name, id string, job manifest.Job, st jobs.Streams) (jobs.Outcome, error) {
	call := commandCall{Kind: kind, Name: name, ID: id, Job: job}
	if d, ok := ctx.Deadline(); ok {
		call.Deadline = time.Until(d)
	}
	f.mu.Lock()
	f.calls = append(f.calls, call)
	out, outcome, err, hold := f.out, f.outcome, f.err, f.hold
	f.mu.Unlock()
	if hold != nil {
		select {
		case <-hold:
		case <-ctx.Done():
			return jobs.Outcome{}, ctx.Err()
		}
	}
	if st.Stdin != nil {
		in, _ := io.ReadAll(st.Stdin)
		f.mu.Lock()
		f.in = in
		f.mu.Unlock()
	}
	if st.Stdout != nil {
		for _, chunk := range out {
			if _, werr := st.Stdout.Write(chunk); werr != nil {
				return jobs.Outcome{}, werr
			}
		}
	}
	if outcome.Exit == nil && !outcome.TimedOut && outcome.Detail == "" {
		outcome = exited(0)
	}
	return outcome, err
}

func (f *fakeCommands) ran() []commandCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// commandRig is the rig with a command backup of a Compose database added.
func commandRig(t *testing.T) (*rig, *fakeCommands) {
	t.Helper()
	g := newRig(t)
	g.m.Apps["db"] = manifest.App{Kind: "compose"}
	g.m.Backups["main-db"] = manifest.Backup{App: "db", Service: "postgres", Command: []string{"pg_dump", "-Fc", "app"}, Destination: "offsite", Timeout: "30m"}
	cmds := &fakeCommands{out: [][]byte{[]byte("PGDMP-part-one "), []byte("part-two")}}
	g.runner.Commands = cmds
	return g, cmds
}

func gunzip(t *testing.T, b []byte) string {
	t.Helper()
	r, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("not gzip: %v", err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("gunzip: %v", err)
	}
	return string(out)
}

func TestACommandBackupStoresTheGzippedOutputWithItsDigestAndAFormatOfItsOwn(t *testing.T) {
	g, cmds := commandRig(t)
	res := g.run("main-db")
	wantKey := "demo/prod/db/command/main-db/20261116T100100Z-1.gz"
	if res.Key != wantKey || res.Run.Status != "succeeded" || res.Run.Detail != "" {
		t.Fatalf("result: %+v", res)
	}
	if got := g.objs.keys(); !slices.Equal(got, []string{wantKey, "demo/prod/db/command/main-db/20261116T100100Z-1.json"}) {
		t.Fatalf("objects: %v", got)
	}
	stored := g.objs.objects[wantKey]
	if got := gunzip(t, stored); got != "PGDMP-part-one part-two" {
		t.Fatalf("stored output %q", got)
	}
	sc := g.sidecarOf(wantKey)
	want := Sidecar{Format: CommandSidecarFormat, Project: "demo", Environment: "prod", Backup: "main-db", App: "db", Size: int64(len(stored)), SHA256: sum(string(stored)),
		Started: time.Date(2026, 11, 16, 10, 1, 0, 0, time.UTC), Finished: time.Date(2026, 11, 16, 10, 2, 0, 0, time.UTC), RunID: 1}
	if sc != want {
		t.Fatalf("sidecar\n got %+v\nwant %+v", sc, want)
	}
	rec := g.lastRun("main-db")
	if rec.Status != "succeeded" || rec.Volume != "" || rec.Size != sc.Size || rec.SHA256 != sc.SHA256 || rec.ObjectKey != wantKey {
		t.Fatalf("recorded run: %+v", rec)
	}
	calls := cmds.ran()
	if len(calls) != 1 || calls[0].Kind != "backup" || calls[0].Name != "main-db" || calls[0].ID != "1" ||
		calls[0].Job.App != "db" || calls[0].Job.Service != "postgres" || !slices.Equal(calls[0].Job.Command, []string{"pg_dump", "-Fc", "app"}) {
		t.Fatalf("command runs: %+v", calls)
	}
	// The command's time limit is the backup's.
	if d := calls[0].Deadline; d <= 29*time.Minute || d > 30*time.Minute {
		t.Fatalf("the command's time limit is %v", d)
	}
	// No helper container is made: nothing is archived from a volume.
	g.docker.assertNoContainerLeft(t)
}

func TestACommandThatExitsNonZeroHalfwayLeavesNoBackupAndKeepsItsStderr(t *testing.T) {
	g, cmds := commandRig(t)
	cmds.outcome = jobs.Outcome{Exit: new(int), Output: "pg_dump: error: connection lost\n"}
	*cmds.outcome.Exit = 2
	res := g.run("main-db")
	if res.Run.Status != "failed" || res.Run.Detail != "command failed: exited with code 2: pg_dump: error: connection lost" {
		t.Fatalf("result: %+v", res.Run)
	}
	if got := g.objs.keys(); len(got) != 0 {
		t.Fatalf("objects after a failed command: %v", got)
	}
	if rec := g.lastRun("main-db"); rec.Status != "failed" || rec.Detail != res.Run.Detail || rec.SHA256 != "" {
		t.Fatalf("recorded run: %+v", rec)
	}
	if list, err := g.runner.List(context.Background(), g.m, "main-db"); err != nil || len(list) != 0 {
		t.Fatalf("listed: %+v %v", list, err)
	}
}

func TestACommandThatWritesNothingFailsTheBackup(t *testing.T) {
	g, cmds := commandRig(t)
	cmds.out = nil
	res := g.run("main-db")
	if res.Run.Status != "failed" || res.Run.Detail != "command failed: wrote no output" {
		t.Fatalf("result: %+v", res.Run)
	}
	if got := g.objs.keys(); len(got) != 0 {
		t.Fatalf("objects after an empty output: %v", got)
	}
}

func TestACommandThatCouldNotRunOrTimedOutFailsTheBackupWithTheReason(t *testing.T) {
	g, cmds := commandRig(t)
	cmds.outcome = jobs.Outcome{Detail: "no running container for db/postgres"}
	cmds.out = nil
	if res := g.run("main-db"); res.Run.Detail != "command failed: no running container for db/postgres" {
		t.Fatalf("not run: %+v", res.Run)
	}
	cmds.outcome = jobs.Outcome{TimedOut: true, Output: "still dumping\n"}
	cmds.out = [][]byte{[]byte("partial")}
	if res := g.run("main-db"); res.Run.Detail != "command failed: timed out after 30m0s: still dumping" {
		t.Fatalf("timed out: %+v", res.Run)
	}
	if got := g.objs.keys(); len(got) != 0 {
		t.Fatalf("objects: %v", got)
	}
}

func TestTheDetailOfAFailedCommandKeepsTheEndOfItsStderr(t *testing.T) {
	g, cmds := commandRig(t)
	cmds.out = nil
	cmds.outcome = jobs.Outcome{Exit: new(int), Output: strings.Repeat("noise noise noise\n", 100) + "the real reason\n"}
	*cmds.outcome.Exit = 1
	res := g.run("main-db")
	if len(res.Run.Detail) > 512 || !strings.HasSuffix(res.Run.Detail, "the real reason") || !strings.HasPrefix(res.Run.Detail, "command failed: exited with code 1: ") {
		t.Fatalf("detail (%d bytes): %q", len(res.Run.Detail), res.Run.Detail)
	}
	if rec := g.lastRun("main-db"); rec.Detail != res.Run.Detail {
		t.Fatalf("recorded %q", rec.Detail)
	}
}

func TestAFailedUploadOfACommandBackupEndsTheCommandAndLeavesNothing(t *testing.T) {
	g, cmds := commandRig(t)
	g.objs.failPut = ".gz"
	cmds.out = [][]byte{bytes.Repeat([]byte("x"), 1000)}
	res := g.run("main-db")
	if res.Run.Status != "failed" || res.Run.Detail != "upload failed: access denied" {
		t.Fatalf("result: %+v", res.Run)
	}
	if got := g.objs.keys(); len(got) != 0 {
		t.Fatalf("objects: %v", got)
	}
}

func TestACommandBackupFollowsTheSameRetentionAsAVolumeBackup(t *testing.T) {
	g, _ := commandRig(t)
	b := g.m.Backups["main-db"]
	b.Retention = &manifest.Retention{Keep: 1}
	g.m.Backups["main-db"] = b
	first := g.run("main-db")
	second := g.run("main-db")
	keys := g.objs.keys()
	if !slices.Equal(keys, []string{sidecarKeyOf(second.Key), second.Key}) && !slices.Equal(keys, []string{second.Key, sidecarKeyOf(second.Key)}) {
		t.Fatalf("after retention: %v (first %s, second %s)", keys, first.Key, second.Key)
	}
	list, err := g.runner.List(context.Background(), g.m, "main-db")
	if err != nil || len(list) != 1 || list[0].Key != second.Key || list[0].Format != CommandSidecarFormat || list[0].Volume != "" || list[0].SHA256 != second.Run.SHA256 {
		t.Fatalf("list: %+v %v", list, err)
	}
}

func TestACommandBackupRunsOneAtATime(t *testing.T) {
	g, cmds := commandRig(t)
	cmds.hold = make(chan struct{})
	p, err := g.runner.Begin(context.Background(), g.m, "main-db", "manual")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan Result)
	go func() { done <- p.Execute(context.Background()) }()
	if _, err = g.runner.Begin(context.Background(), g.m, "main-db", "manual"); !errors.Is(err, ErrBackupRunning) {
		t.Fatalf("a second run: %v", err)
	}
	close(cmds.hold)
	if res := <-done; res.Run.Status != "succeeded" {
		t.Fatalf("result %+v", res.Run)
	}
	if _, err = g.runner.Begin(context.Background(), g.m, "main-db", "manual"); err != nil {
		t.Fatalf("after the run: %v", err)
	}
}

func TestACommandBackupThatCannotRunStartsNothingWithoutCommands(t *testing.T) {
	g, _ := commandRig(t)
	g.runner.Commands = nil
	if _, err := g.runner.Begin(context.Background(), g.m, "main-db", "manual"); err == nil {
		t.Fatal("a command backup began without a way to run commands")
	}
}

// restoreRig is the command rig with one successful command backup in storage.
func restoreRig(t *testing.T) (*rig, *fakeCommands, Result) {
	t.Helper()
	g, cmds := commandRig(t)
	res := g.run("main-db")
	if res.Run.Status != "succeeded" {
		t.Fatalf("setup: %+v", res.Run)
	}
	cmds.mu.Lock()
	cmds.calls = nil
	cmds.mu.Unlock()
	return g, cmds, res
}

var restoreJob = manifest.Job{App: "db", Service: "postgres", Command: []string{"pg_restore", "-d", "scratch"}, Timeout: "1h"}

func TestRestoreIntoACommandStreamsTheDecompressedBackupAfterCheckingItsDigest(t *testing.T) {
	g, cmds, res := restoreRig(t)
	out, err := g.runner.RestoreCommand(context.Background(), g.m, res.Key, restoreJob, "op-7")
	if err != nil || out.Exit == nil || *out.Exit != 0 {
		t.Fatalf("restore: %+v %v", out, err)
	}
	if string(cmds.in) != "PGDMP-part-one part-two" {
		t.Fatalf("the command read %q", cmds.in)
	}
	calls := cmds.ran()
	if len(calls) != 1 || calls[0].Kind != "restore" || calls[0].ID != "op-7" || calls[0].Job.App != "db" || !slices.Equal(calls[0].Job.Command, restoreJob.Command) {
		t.Fatalf("command runs: %+v", calls)
	}
}

func TestRestoreIntoACommandRefusesAWrongDigestBeforeTheCommandStarts(t *testing.T) {
	g, cmds, res := restoreRig(t)
	g.objs.mu.Lock()
	g.objs.objects[res.Key] = append([]byte{}, g.objs.objects[res.Key]...)
	g.objs.objects[res.Key][len(g.objs.objects[res.Key])/2] ^= 0xff
	g.objs.mu.Unlock()
	_, err := g.runner.RestoreCommand(context.Background(), g.m, res.Key, restoreJob, "op-8")
	if !errors.Is(err, ErrChecksumMismatch) {
		t.Fatalf("error %v", err)
	}
	if calls := cmds.ran(); len(calls) != 0 || len(cmds.in) != 0 {
		t.Fatalf("the command was started: %+v, read %q", calls, cmds.in)
	}
}

func TestRestoreIntoACommandReportsAFailedCommandWithItsStderr(t *testing.T) {
	g, cmds, res := restoreRig(t)
	cmds.outcome = jobs.Outcome{Exit: new(int), Output: "pg_restore: error: could not connect\n"}
	*cmds.outcome.Exit = 1
	out, err := g.runner.RestoreCommand(context.Background(), g.m, res.Key, restoreJob, "op-9")
	var failure *CommandError
	if !errors.As(err, &failure) || failure.Outcome.Output != "pg_restore: error: could not connect\n" || out.Exit == nil || *out.Exit != 1 {
		t.Fatalf("error %v, outcome %+v", err, out)
	}
	if failure.Error() != "exited with code 1: pg_restore: error: could not connect" {
		t.Fatalf("message %q", failure.Error())
	}
}

func TestEachKindOfRestoreRefusesTheOtherKindOfBackup(t *testing.T) {
	g, _, command := restoreRig(t)
	g.archive("the-archive-bytes")
	volume := g.run("uploads")
	var refusal Refusal
	if _, err := g.runner.Check(context.Background(), g.m, command.Key, "restored"); !errors.As(err, &refusal) || refusal != ErrCommandBackup {
		t.Fatalf("restore --into of a command backup: %v", err)
	}
	if _, err := g.runner.CheckCommand(context.Background(), g.m, volume.Key); !errors.As(err, &refusal) || refusal != ErrVolumeBackup {
		t.Fatalf("restore --exec of a volume backup: %v", err)
	}
	if err := g.runner.Restore(context.Background(), g.m, command.Key, "restored", "op-1"); !errors.As(err, &refusal) || refusal != ErrCommandBackup {
		t.Fatalf("a restore into a volume of a command backup: %v", err)
	}
	if _, err := g.runner.RestoreCommand(context.Background(), g.m, volume.Key, restoreJob, "op-2"); !errors.As(err, &refusal) || refusal != ErrVolumeBackup {
		t.Fatalf("a restore into a command of a volume backup: %v", err)
	}
	sc, err := g.runner.CheckCommand(context.Background(), g.m, command.Key)
	if err != nil || sc.Backup != "main-db" || sc.Format != CommandSidecarFormat {
		t.Fatalf("a valid check: %+v %v", sc, err)
	}
	if !PublicRefusal(string(ErrCommandBackup)) || !PublicRefusal(string(ErrVolumeBackup)) {
		t.Fatal("the refusals of the wrong kind are not public")
	}
}
