package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"berthy/internal/backup"
	"berthy/internal/engine"
	"berthy/internal/jobs"
	"berthy/internal/manifest"
	"berthy/internal/state"
)

const dumpKey = "db/command/main-db/20261116T100000Z-1.gz"

// execRestoreController is a controller that runs the image app web and the Compose app db, with a running postgres container,
// and a restorer that stands for the agent's.
func execRestoreController(t *testing.T, r *fakeRestorer) (*Controller, *jobFake) {
	t.Helper()
	m := runManifest("web")
	m.Apps["db"] = manifest.App{Kind: "compose"}
	c, f, s := runFixture(t, m)
	raw, _ := json.Marshal(m)
	op, _, err := s.Admit(context.Background(), state.Admission{RequestID: "seed-db", ActorID: "admin", Kind: "deploy", App: "db", PublicRequest: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Finish(context.Background(), op.ID, state.Outcome{State: "succeeded", Release: &state.Release{App: "db", Digest: runDigest, Tag: "v1", Manifest: raw}}); err != nil {
		t.Fatal(err)
	}
	jf := &jobFake{runFake: f}
	jf.composeOf = []engine.ComposeContainer{{ID: "pg-1", Service: "postgres", State: "running", Number: 1}}
	c.Engine = jf
	c.Restorer = r
	return c, jf
}

func execRestoreRequest(id string) Request {
	return Request{RequestID: id, Kind: "restore", RestoreKey: dumpKey, App: "db", Service: "postgres", Command: []string{"pg_restore", "--clean", "-d", "scratch"}}
}

func TestRestoreIntoACommandRunsUnderTheLockAndAuditedAndFeedsTheCommand(t *testing.T) {
	r := &fakeRestorer{block: make(chan struct{})}
	c, _ := execRestoreController(t, r)
	op, created, err := admit(t, c, context.Background(), restoreAdmin, execRestoreRequest("x1"))
	if err != nil || !created || op.Kind != "restore" {
		t.Fatalf("admit: %+v %v %v", op, created, err)
	}
	if _, _, err = admit(t, c, context.Background(), restoreAdmin, Request{RequestID: "x2", Kind: "prune"}); !errors.Is(err, state.ErrConflict) {
		t.Fatalf("a second operation during the restore: %v", err)
	}
	close(r.block)
	done := waitComposeOperation(t, c.Store, op.ID)
	if done.State != "succeeded" || done.Err != "" {
		t.Fatalf("operation: %+v", done)
	}
	if len(r.execJobs) != 1 || r.execJobs[0].App != "db" || r.execJobs[0].Service != "postgres" || r.execJobs[0].Role != "" ||
		!slices.Equal(r.execJobs[0].Command, []string{"pg_restore", "--clean", "-d", "scratch"}) || r.execJobs[0].Timeout != "" {
		t.Fatalf("jobs: %+v", r.execJobs)
	}
	if !slices.Equal(r.opIDs, []string{op.ID}) {
		t.Fatalf("operations %v", r.opIDs)
	}
	steps := stepsOf(t, c.Store, op.ID)
	if len(steps) != 2 || !strings.Contains(steps[0], "restoring") || !strings.Contains(steps[0], "db") || !strings.Contains(steps[1], "restored") {
		t.Fatalf("steps: %v", steps)
	}
	audit, _ := c.Store.AuditLog(context.Background(), 10)
	found := false
	for _, a := range audit {
		found = found || a.Action == "restore" && a.ActorName == "ops"
	}
	if !found {
		t.Fatalf("audit: %+v", audit)
	}
}

func TestRestoreIntoACommandOfAnImageAppNamesItsRole(t *testing.T) {
	r := &fakeRestorer{}
	c, _ := execRestoreController(t, r)
	req := Request{RequestID: "x1", Kind: "restore", RestoreKey: dumpKey, App: "web", Role: "web", Command: []string{"restore-tool"}, Timeout: "2h"}
	op, _, err := admit(t, c, context.Background(), restoreAdmin, req)
	if err != nil {
		t.Fatal(err)
	}
	if done := waitComposeOperation(t, c.Store, op.ID); done.State != "succeeded" {
		t.Fatalf("operation: %+v", done)
	}
	if len(r.execJobs) != 1 || r.execJobs[0].Role != "web" || r.execJobs[0].Service != "" || r.execJobs[0].Timeout != "2h" {
		t.Fatalf("jobs: %+v", r.execJobs)
	}
}

func TestRestoreIntoACommandRefusesWhatCannotWorkBeforeAnythingIsLocked(t *testing.T) {
	r := &fakeRestorer{}
	c, jf := execRestoreController(t, r)
	for name, tc := range map[string]struct {
		mutate func(*Request)
		actor  Actor
		want   string
	}{
		"not an administrator":    {func(*Request) {}, Actor{ID: "ci", Name: "ci"}, string(errRestoreAdmin)},
		"into and a command":      {func(q *Request) { q.RestoreInto = "restored" }, restoreAdmin, string(errRestoreArgs)},
		"no key":                  {func(q *Request) { q.RestoreKey = "" }, restoreAdmin, string(errRestoreArgs)},
		"no app":                  {func(q *Request) { q.App = "" }, restoreAdmin, string(errRestoreArgs)},
		"unknown app":             {func(q *Request) { q.App = "ghost" }, restoreAdmin, string(errRunNotDeployed)},
		"role of a Compose app":   {func(q *Request) { q.Service, q.Role = "", "web" }, restoreAdmin, "use service for a Compose app"},
		"service of an image app": {func(q *Request) { q.App, q.Role, q.Service = "web", "", "web" }, restoreAdmin, "use role for an image app"},
		"role and service":        {func(q *Request) { q.Role = "web" }, restoreAdmin, "name a role or a service, not both"},
		"bad timeout":             {func(q *Request) { q.Timeout = "soon" }, restoreAdmin, string(errRunTimeout)},
	} {
		req := execRestoreRequest("x-" + name)
		tc.mutate(&req)
		_, _, err := admit(t, c, context.Background(), tc.actor, req)
		var refusal Refusal
		if !errors.As(err, &refusal) || string(refusal) != tc.want {
			t.Errorf("%s: got %v, want %q", name, err, tc.want)
		}
	}
	jf.composeOf = nil
	if _, _, err := admit(t, c, context.Background(), restoreAdmin, execRestoreRequest("x-none")); err == nil || err.Error() != "no running container for db/postgres" {
		t.Errorf("no running container: %v", err)
	}
	if len(r.execChecks) != 0 && len(r.execJobs) != 0 {
		t.Fatalf("a refused restore reached the command: %v", r.execJobs)
	}
	if active, err := c.Store.Active(context.Background()); err == nil {
		t.Fatalf("a refused restore holds the lock: %+v", active)
	}
}

func TestRestoreIntoACommandRefusesAVolumeBackup(t *testing.T) {
	r := &fakeRestorer{checkErr: backup.ErrVolumeBackup}
	c, _ := execRestoreController(t, r)
	_, _, err := admit(t, c, context.Background(), restoreAdmin, execRestoreRequest("x1"))
	var refusal Refusal
	if !errors.As(err, &refusal) || string(refusal) != string(backup.ErrVolumeBackup) {
		t.Fatalf("admission: %v", err)
	}
	// And restore into a volume refuses a command backup.
	r.checkErr = backup.ErrCommandBackup
	_, _, err = admit(t, c, context.Background(), restoreAdmin, restoreRequest("x2"))
	if !errors.As(err, &refusal) || string(refusal) != string(backup.ErrCommandBackup) {
		t.Fatalf("restore into a volume: %v", err)
	}
}

func TestRestoreIntoACommandDryRunShowsWhereItRunsAndChangesNothing(t *testing.T) {
	r := &fakeRestorer{}
	c, _ := execRestoreController(t, r)
	req := execRestoreRequest("x1")
	req.DryRun = true
	plan, err := c.Plan(context.Background(), restoreAdmin, req)
	if err != nil || plan.Restore == nil {
		t.Fatalf("plan: %+v %v", plan, err)
	}
	p := plan.Restore
	if p.Key != dumpKey || p.Into != "" || p.Backup != "main-db" || p.Target != "db service postgres" || !slices.Equal(p.Command, req.Command) || p.Size != 12 {
		t.Fatalf("restore plan: %+v", p)
	}
	if len(r.execJobs) != 0 {
		t.Fatalf("a dry run restored: %v", r.execJobs)
	}
	req.App, req.Service, req.Role = "web", "", "web"
	if plan, err = c.Plan(context.Background(), restoreAdmin, req); err != nil || plan.Restore.Target != "web role web" {
		t.Fatalf("plan of an image app: %+v %v", plan.Restore, err)
	}
}

func TestAFailedRestoreCommandReportsItsExitCodeAndTheEndOfItsStderr(t *testing.T) {
	code := 1
	r := &fakeRestorer{
		execOutput: jobs.Outcome{Exit: &code, Output: "pg_restore: error: first\npg_restore: error: could not connect, café\n"},
	}
	r.execErr = &backup.CommandError{Outcome: r.execOutput, Timeout: time.Hour}
	c, _ := execRestoreController(t, r)
	op, _, err := admit(t, c, context.Background(), restoreAdmin, execRestoreRequest("x1"))
	if err != nil {
		t.Fatal(err)
	}
	done := waitComposeOperation(t, c.Store, op.ID)
	if done.State != "failed" || done.Err != string(errRestoreCommandFailed) || !PublicMessage(done.Err) {
		t.Fatalf("operation: %+v", done)
	}
	if done.Reason != "command exited with code 1: pg_restore: error: could not connect, caf?" || safeReasonOK(done.Reason) == false {
		t.Fatalf("reason %q", done.Reason)
	}
	steps := stepsOf(t, c.Store, op.ID)
	if len(steps) < 3 || steps[len(steps)-2] != "pg_restore: error: first" || steps[len(steps)-1] != "pg_restore: error: could not connect, café" {
		t.Fatalf("steps: %q", steps)
	}
}

// safeReasonOK is the rule of the reason the API shows: printable ASCII, 300 bytes at most.
func safeReasonOK(s string) bool {
	if len(s) > 300 {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r > 0x7e {
			return false
		}
	}
	return true
}

func TestARestoreIntoACommandThatTimedOutOrCouldNotRunSaysSo(t *testing.T) {
	for name, tc := range map[string]struct {
		err    error
		want   string
		reason string
	}{
		"timed out":  {&backup.CommandError{Outcome: jobs.Outcome{TimedOut: true}, Timeout: time.Hour}, string(errRestoreCommandFailed), "command timed out after 1h0m0s"},
		"not run":    {&backup.CommandError{Outcome: jobs.Outcome{Detail: "no running container for db/postgres"}}, string(errRestoreCommandFailed), "no running container for db/postgres"},
		"checksum":   {backup.ErrChecksumMismatch, "backup checksum mismatch", ""},
		"unexpected": {errors.New("/secret/path exploded"), "restore failed", ""},
	} {
		r := &fakeRestorer{execErr: tc.err}
		c, _ := execRestoreController(t, r)
		op, _, err := admit(t, c, context.Background(), restoreAdmin, execRestoreRequest("x1"))
		if err != nil {
			t.Fatal(err)
		}
		done := waitComposeOperation(t, c.Store, op.ID)
		if done.State != "failed" || done.Err != tc.want || done.Reason != tc.reason {
			t.Errorf("%s: %+v", name, done)
		}
	}
}

func TestAnInterruptedRestoreIntoACommandSaysTheCommandMayHaveLoadedPartOfTheBackup(t *testing.T) {
	r := &fakeRestorer{block: make(chan struct{})}
	c, _ := execRestoreController(t, r)
	op, _, err := admit(t, c, context.Background(), restoreAdmin, execRestoreRequest("x1"))
	if err != nil {
		t.Fatal(err)
	}
	releaseAtEnd(t, r)
	waitForRestore(t, r)
	if err = c.Recover(context.Background()); err != nil {
		t.Fatalf("recover: %v", err)
	}
	got, _ := c.Store.Operation(context.Background(), op.ID)
	if got.State != "interrupted" || got.Err != string(errRestoreExecInterrupted) || !PublicMessage(got.Err) {
		t.Fatalf("recovered: %+v", got)
	}
	if _, err = c.Store.Active(context.Background()); err == nil {
		t.Fatal("the lock is still held")
	}
}
