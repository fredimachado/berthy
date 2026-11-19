package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"berthy/internal/backup"
	"berthy/internal/engine"
	"berthy/internal/jobs"
	"berthy/internal/manifest"
	"berthy/internal/state"
)

// fakeRestorer is the agent's side of a restore.
type fakeRestorer struct {
	mu       sync.Mutex
	checks   []string
	restores []string
	opIDs    []string
	checkErr error
	err      error
	block    chan struct{}
	// The restore into a command: the checks, the commands that ran, and how the restore ends.
	execChecks []string
	execJobs   []manifest.Job
	execOutput jobs.Outcome
	execErr    error
}

func (f *fakeRestorer) CheckRestoreCommand(_ context.Context, key string) (RestorePlan, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.execChecks = append(f.execChecks, key)
	if f.checkErr != nil {
		return RestorePlan{}, f.checkErr
	}
	return RestorePlan{Key: key, Backup: "main-db", App: "db", Size: 12, SHA256: strings.Repeat("ab", 32), Finished: time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)}, nil
}

func (f *fakeRestorer) RestoreCommand(_ context.Context, opID, key string, job manifest.Job) (jobs.Outcome, error) {
	f.mu.Lock()
	f.execJobs = append(f.execJobs, job)
	f.restores = append(f.restores, key+" (command)")
	f.opIDs = append(f.opIDs, opID)
	block, out, err := f.block, f.execOutput, f.execErr
	f.mu.Unlock()
	if block != nil {
		<-block
	}
	return out, err
}

func (f *fakeRestorer) CheckRestore(_ context.Context, key, into string) (RestorePlan, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checks = append(f.checks, key+" "+into)
	if f.checkErr != nil {
		return RestorePlan{}, f.checkErr
	}
	return RestorePlan{Key: key, Backup: "uploads", App: "files", Volume: "uploads", Into: into, Size: 12, SHA256: strings.Repeat("ab", 32), Finished: time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)}, nil
}

func (f *fakeRestorer) Restore(_ context.Context, opID, key, into string) error {
	f.mu.Lock()
	f.restores = append(f.restores, key+" "+into)
	f.opIDs = append(f.opIDs, opID)
	block, err := f.block, f.err
	f.mu.Unlock()
	if block != nil {
		<-block
	}
	return err
}

func restoreController(t *testing.T, r *fakeRestorer) *Controller {
	t.Helper()
	return &Controller{Engine: &positiveEngine{}, Store: openStore(t), Environment: "test", Restorer: r}
}

var restoreAdmin = Actor{ID: "admin", Name: "ops", Admin: true}

func restoreRequest(id string) Request {
	return Request{RequestID: id, Kind: "restore", RestoreKey: "files/uploads/uploads/20261116T100000Z-1.tar.gz", RestoreInto: "restored-1"}
}

func stepsOf(t *testing.T, s *state.Store, id string) []string {
	t.Helper()
	events, err := s.EventsAfter(context.Background(), id, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var steps []string
	for _, e := range events {
		if e.Kind == "step" {
			var v struct{ Step string }
			if json.Unmarshal(e.Data, &v) == nil {
				steps = append(steps, v.Step)
			}
		}
	}
	return steps
}

func TestRestoreRunsUnderTheDeployLockAndReportsItsSteps(t *testing.T) {
	r := &fakeRestorer{block: make(chan struct{})}
	c := restoreController(t, r)
	op, created, err := admit(t, c, context.Background(), restoreAdmin, restoreRequest("r1"))
	if err != nil || !created || op.Kind != "restore" {
		t.Fatalf("admit: %+v %v %v", op, created, err)
	}
	// The environment is locked while the restore runs.
	if _, _, err = admit(t, c, context.Background(), restoreAdmin, Request{RequestID: "r2", Kind: "prune"}); !errors.Is(err, state.ErrConflict) {
		t.Fatalf("a second operation during the restore: %v", err)
	}
	close(r.block)
	done := waitComposeOperation(t, c.Store, op.ID)
	if done.State != "succeeded" || done.Err != "" {
		t.Fatalf("operation: %+v", done)
	}
	if !slices.Equal(r.restores, []string{"files/uploads/uploads/20261116T100000Z-1.tar.gz restored-1"}) || !slices.Equal(r.opIDs, []string{op.ID}) {
		t.Fatalf("restores: %v operations %v", r.restores, r.opIDs)
	}
	steps := stepsOf(t, c.Store, op.ID)
	if len(steps) != 2 || !strings.Contains(steps[0], "restoring") || !strings.Contains(steps[1], "restored") {
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

func TestRestoreNeedsAnAdministratorAndBothNames(t *testing.T) {
	r := &fakeRestorer{}
	c := restoreController(t, r)
	deployer := Actor{ID: "ci", Name: "ci"}
	if _, _, err := admit(t, c, context.Background(), deployer, restoreRequest("r1")); !errors.Is(err, errRestoreAdmin) {
		t.Fatalf("a deploy token: %v", err)
	}
	if _, err := c.Plan(context.Background(), deployer, restoreRequest("r1")); !errors.Is(err, errRestoreAdmin) {
		t.Fatalf("plan with a deploy token: %v", err)
	}
	for name, mutate := range map[string]func(*Request){
		"no key":  func(q *Request) { q.RestoreKey = "" },
		"no into": func(q *Request) { q.RestoreInto = "" },
	} {
		req := restoreRequest("r2")
		mutate(&req)
		if _, _, err := admit(t, c, context.Background(), restoreAdmin, req); !errors.Is(err, errRestoreArgs) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if len(r.checks)+len(r.restores) != 0 {
		t.Fatalf("a refused restore reached the restorer: %v %v", r.checks, r.restores)
	}
	c.Restorer = nil
	if _, _, err := admit(t, c, context.Background(), restoreAdmin, restoreRequest("r3")); !errors.Is(err, errRestoreUnavailable) {
		t.Fatalf("without a restorer: %v", err)
	}
}

func TestRestoreRefusesAtAdmissionWhatTheCheckRefuses(t *testing.T) {
	r := &fakeRestorer{checkErr: backup.ErrVolumeExists}
	c := restoreController(t, r)
	_, _, err := admit(t, c, context.Background(), restoreAdmin, restoreRequest("r1"))
	var refusal Refusal
	if !errors.As(err, &refusal) || string(refusal) != backup.ErrVolumeExists.Error() || !PublicMessage(string(refusal)) {
		t.Fatalf("admission: %v", err)
	}
	if active, err := c.Store.Active(context.Background()); err == nil {
		t.Fatalf("a refused restore holds the lock: %+v", active)
	}
	r.checkErr = errors.New("secret-bearing storage detail")
	if _, _, err = admit(t, c, context.Background(), restoreAdmin, restoreRequest("r2")); !errors.As(err, &refusal) || strings.Contains(err.Error(), "secret") {
		t.Fatalf("an unexpected check failure: %v", err)
	}
}

func TestRestoreDryRunShowsThePlanAndChangesNothing(t *testing.T) {
	r := &fakeRestorer{}
	c := restoreController(t, r)
	req := restoreRequest("r1")
	req.DryRun = true
	plan, err := c.Plan(context.Background(), restoreAdmin, req)
	if err != nil || plan.Restore == nil {
		t.Fatalf("plan: %+v %v", plan, err)
	}
	p := plan.Restore
	if p.Key != req.RestoreKey || p.Into != "restored-1" || p.Backup != "uploads" || p.App != "files" || p.Volume != "uploads" || p.Size != 12 {
		t.Fatalf("restore plan: %+v", p)
	}
	if len(r.restores) != 0 {
		t.Fatalf("a dry run restored: %v", r.restores)
	}
	r.checkErr = backup.ErrNotFound
	if _, err = c.Plan(context.Background(), restoreAdmin, req); !errors.Is(err, Refusal(backup.ErrNotFound)) {
		t.Fatalf("dry run of a missing backup: %v", err)
	}
}

func TestAFailedRestoreIsReportedWithAPublicReason(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		want string
	}{
		"checksum": {fmt.Errorf("volume restore verification failed: %w", backup.ErrChecksumMismatch), "backup checksum mismatch"},
		"refusal":  {backup.ErrNotFound, "backup not found"},
		"other":    {errors.New("helper exited 1: /source/secret.txt"), "restore failed"},
	} {
		r := &fakeRestorer{err: tc.err}
		c := restoreController(t, r)
		op, _, err := admit(t, c, context.Background(), restoreAdmin, restoreRequest("r1"))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		done := waitComposeOperation(t, c.Store, op.ID)
		if done.State != "failed" || done.Err != tc.want || !PublicMessage(done.Err) {
			t.Errorf("%s: %+v", name, done)
		}
	}
}

// restoreDocker is the Docker of a restore recovery: containers, and volumes with their labels.
type restoreDocker struct {
	*positiveEngine
	mu         sync.Mutex
	containers []engine.InventoryContainer
	volumes    map[string]map[string]string
	removedC   []string
	removedV   []string
	volumeErr  error
}

func (d *restoreDocker) InventoryContainers(context.Context) ([]engine.InventoryContainer, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]engine.InventoryContainer(nil), d.containers...), nil
}

func (d *restoreDocker) RemoveContainer(_ context.Context, id string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.removedC = append(d.removedC, id)
	for _, c := range d.containers {
		if c.ID == id {
			return nil
		}
	}
	return &engine.StatusError{Code: 404}
}

func (d *restoreDocker) VolumeLabels(_ context.Context, name string) (map[string]string, bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	l, ok := d.volumes[name]
	return l, ok, nil
}

func (d *restoreDocker) RemoveVolume(_ context.Context, name string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.volumeErr != nil {
		return d.volumeErr
	}
	d.removedV = append(d.removedV, name)
	delete(d.volumes, name)
	return nil
}

// interruptedRestore starts a restore that never ends, as an agent that dies leaves it, and recovers it with docker.
func interruptedRestore(t *testing.T, docker *restoreDocker, label func(opID string) map[string]string) (*Controller, state.Operation) {
	t.Helper()
	r := &fakeRestorer{block: make(chan struct{})}
	// The worker stays blocked, as the worker of a dead agent does, until the test ends.
	c := restoreController(t, r)
	c.Engine = docker
	op, _, err := admit(t, c, context.Background(), restoreAdmin, restoreRequest("r1"))
	if err != nil {
		t.Fatal(err)
	}
	releaseAtEnd(t, r)
	waitForRestore(t, r)
	if labels := label(op.ID); labels != nil {
		docker.volumes["restored-1"] = labels
	}
	if err = c.Recover(context.Background()); err != nil {
		t.Fatalf("recover: %v", err)
	}
	got, _ := c.Store.Operation(context.Background(), op.ID)
	if _, err = c.Store.Active(context.Background()); err == nil {
		t.Fatal("the lock is still held")
	}
	return c, got
}

func TestAnInterruptedRestoreRemovesItsPartialVolumeAndHelperAtRecovery(t *testing.T) {
	docker := &restoreDocker{positiveEngine: &positiveEngine{}, volumes: map[string]map[string]string{"other": {"berthy.restore-operation": "someone-else"}, "plain": nil}, containers: []engine.InventoryContainer{
		{ID: "h1", State: "running", Labels: map[string]string{"berthy.environment": "test", "berthy.helper": "restore"}},
		{ID: "h2", State: "running", Labels: map[string]string{"berthy.environment": "other-env", "berthy.helper": "restore"}},
		{ID: "h3", State: "running", Labels: map[string]string{"berthy.environment": "test", "berthy.helper": "archive"}},
		{ID: "app", State: "running", Labels: map[string]string{"berthy.environment": "test"}},
	}}
	_, got := interruptedRestore(t, docker, func(id string) map[string]string {
		return map[string]string{"berthy.restore-operation": id}
	})
	if got.State != "interrupted" || got.Err != string(errRestoreCleaned) || !PublicMessage(got.Err) {
		t.Fatalf("recovered: %+v", got)
	}
	if !slices.Equal(docker.removedV, []string{"restored-1"}) || !slices.Equal(docker.removedC, []string{"h1"}) {
		t.Fatalf("removed volumes %v containers %v", docker.removedV, docker.removedC)
	}
	if _, ok := docker.volumes["other"]; !ok {
		t.Fatal("another volume was removed")
	}
}

func TestRecoveryOfARestoreNeverRemovesAVolumeItDidNotCreate(t *testing.T) {
	for name, label := range map[string]func(string) map[string]string{
		"no labels":      func(string) map[string]string { return map[string]string{} },
		"another op":     func(string) map[string]string { return map[string]string{"berthy.restore-operation": "someone-else"} },
		"no such volume": func(string) map[string]string { return nil },
	} {
		docker := &restoreDocker{positiveEngine: &positiveEngine{}, volumes: map[string]map[string]string{}}
		_, got := interruptedRestore(t, docker, label)
		if got.State != "interrupted" || len(docker.removedV) != 0 {
			t.Errorf("%s: %+v removed %v", name, got, docker.removedV)
		}
		want := string(errRestoreInterrupted)
		if name == "no such volume" {
			want = string(errRestoreCleaned) // nothing was left to remove
		}
		if got.Err != want {
			t.Errorf("%s: error %q, want %q", name, got.Err, want)
		}
	}
}

func TestRecoveryOfARestoreSaysWhenTheVolumeCouldNotBeRemoved(t *testing.T) {
	docker := &restoreDocker{positiveEngine: &positiveEngine{}, volumes: map[string]map[string]string{}, volumeErr: &engine.StatusError{Code: 409}}
	_, got := interruptedRestore(t, docker, func(id string) map[string]string { return map[string]string{"berthy.restore-operation": id} })
	if got.State != "interrupted" || got.Err != string(errRestoreInterrupted) {
		t.Fatalf("recovered: %+v", got)
	}
}

func TestARestoreThatJournaledItsSuccessIsNotUndoneByRecovery(t *testing.T) {
	r := &fakeRestorer{}
	docker := &restoreDocker{positiveEngine: &positiveEngine{}, volumes: map[string]map[string]string{}}
	c := restoreController(t, r)
	c.Engine = docker
	ctx := context.Background()
	// The worker ran to the end of the restore and died before it recorded the outcome.
	op, _, err := c.Store.Admit(ctx, state.Admission{RequestID: "r1", ActorID: "admin", ActorName: "ops", Kind: "restore", PublicRequest: []byte(`{"restore_key":"k","restore_into":"restored-1"}`)})
	if err != nil {
		t.Fatal(err)
	}
	c.Store.RecordIntent(ctx, op.ID, state.Intent{Ordinal: 1, Kind: "restore", Name: "restored-1", Desired: []byte(`{"key":"k","into":"restored-1"}`)})
	c.Store.MarkStep(ctx, op.ID, 1, "finished", []byte(`{"ok":true}`))
	docker.volumes["restored-1"] = map[string]string{"berthy.restore-operation": op.ID}
	if err = c.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	got, _ := c.Store.Operation(ctx, op.ID)
	if got.State != "succeeded" || len(docker.removedV) != 0 {
		t.Fatalf("recovered: %+v removed %v", got, docker.removedV)
	}
}

func TestAnInterruptedRestoreWithoutVolumeAccessEndsInterrupted(t *testing.T) {
	r := &fakeRestorer{block: make(chan struct{})}
	c := restoreController(t, r)
	op, _, err := admit(t, c, context.Background(), restoreAdmin, restoreRequest("r1"))
	if err != nil {
		t.Fatal(err)
	}
	// The agent died: the worker is gone, and the operation is still running.
	releaseAtEnd(t, r)
	waitForRestore(t, r)
	if err = c.Recover(context.Background()); err != nil {
		t.Fatalf("recover: %v", err)
	}
	got, _ := c.Store.Operation(context.Background(), op.ID)
	if got.State != "interrupted" || got.Err != string(errRestoreInterrupted) || !PublicMessage(got.Err) {
		t.Fatalf("recovered: %+v", got)
	}
	if _, err = c.Store.Active(context.Background()); err == nil {
		t.Fatal("the lock is still held")
	}
}

// releaseAtEnd lets a blocked restore worker go when the test ends, so that the operation's goroutine ends before the store closes.
// Registered after admit, it runs before admit's wait. The operation was ended by recovery, so the worker's write is refused.
func releaseAtEnd(t *testing.T, r *fakeRestorer) {
	t.Helper()
	t.Cleanup(func() { close(r.block) })
}

func waitForRestore(t *testing.T, r *fakeRestorer) {
	t.Helper()
	deadline := time.Now().Add(testWait)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		n := len(r.restores)
		r.mu.Unlock()
		if n > 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the restore did not start")
}

// composeVolumesJSON is the volumes of `compose config --format json` for a project with a plain, a renamed and an external volume.
const composeVolumesJSON = `{"name":"shop","services":{"db":{"image":"postgres"}},"volumes":{
	"uploads":{"name":"shop_uploads"},
	"dumps":{"name":"custom-dumps"},
	"shared":{"name":"team-shared","external":true}}}`

func TestComposeVolumeNamesResolvesKeysThroughTheStoredProject(t *testing.T) {
	runner := &fakeCompose{config: composeVolumesJSON}
	c, _ := composeController(t, runner, newComposeDocker())
	names, err := c.ComposeVolumeNames(context.Background(), "shop")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"uploads": "shop_uploads", "dumps": "custom-dumps", "shared": "team-shared"}
	if fmt.Sprint(names) != fmt.Sprint(want) {
		t.Fatalf("names %v", names)
	}
	if len(runner.runs) != 1 || runner.runs[0].Project != "shop" || runner.runs[0].Dir != c.composeDir("shop") || runner.runs[0].Args[0] != "config" {
		t.Fatalf("runs: %+v", runner.runs)
	}
	runner.config = `not json`
	if _, err = c.ComposeVolumeNames(context.Background(), "shop"); err == nil {
		t.Fatal("an unreadable config was accepted")
	}
	c.Compose = nil
	if _, err = c.ComposeVolumeNames(context.Background(), "shop"); err == nil {
		t.Fatal("no Compose runner was accepted")
	}
}

var _ = engine.ComposeRun{}
