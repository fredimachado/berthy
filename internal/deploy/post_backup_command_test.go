package deploy

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"berthy/internal/jobs"
	"berthy/internal/manifest"
	"berthy/internal/state"
)

type realCommandBackup struct{ c *Controller }

func (b realCommandBackup) RunBackup(ctx context.Context, m manifest.Manifest, name string) (BackupResult, error) {
	var out bytes.Buffer
	result, err := b.c.RunCommand(ctx, m, "backup", name, "1", m.Backups[name].Job(), jobs.Streams{Stdout: &out})
	if err != nil {
		return BackupResult{}, err
	}
	if result.Exit == nil || *result.Exit != 0 {
		return BackupResult{Status: "failed", Detail: result.Detail}, nil
	}
	return BackupResult{Status: "succeeded"}, nil
}

type heldJobBackup struct{ scheduler *jobs.Scheduler }

func (b heldJobBackup) RunBackup(ctx context.Context, m manifest.Manifest, name string) (BackupResult, error) {
	run, release, err := b.scheduler.RunHeld(ctx, m.Backups[name].Before)
	if err != nil {
		return BackupResult{}, err
	}
	defer release()
	return BackupResult{Status: run.Status, Detail: run.Detail}, nil
}

func TestPostDeployCommandBackupUsesVerifiedImageAndMounts(t *testing.T) {
	for _, prior := range []bool{false, true} {
		t.Run(map[bool]string{false: "first", true: "update"}[prior], func(t *testing.T) {
			old := manifest.Manifest{}
			if prior {
				old = runManifest("web")
				role := old.Apps["web"].Roles["web"]
				role.Volumes = []string{"old:/old"}
				old.Apps["web"].Roles["web"] = role
			}
			c, base, s := runFixture(t, old)
			f := &jobFake{runFake: base, streamOut: "dump"}
			c.Engine, c.Backups = f, realCommandBackup{c}
			m := runManifest("web")
			role := m.Apps["web"].Roles["web"]
			role.Volumes = []string{"newdata:/data"}
			role.Resources = &manifest.Resources{CPU: "1.5", Memory: "128M"}
			m.Apps["web"].Roles["web"] = role
			m.Backups = map[string]manifest.Backup{"dump": {App: "web", Role: "web", Command: []string{"dump"}, Destination: "offsite"}}
			op, _, err := s.Admit(context.Background(), state.Admission{RequestID: "post", ActorID: "admin", Kind: "deploy", App: "web", PublicRequest: []byte(`{}`)})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := s.Finish(context.Background(), op.ID, state.Outcome{State: "succeeded"}); err != nil {
					t.Error(err)
				}
			})
			image := "example/web@sha256:" + strings.Repeat("b", 64)
			p := preparedPlan{Image: image, Request: Request{App: "web", Manifest: m}}
			if err := c.runBackupHook(context.Background(), op.ID, p, "post_hook", 1, "dump"); err != nil {
				t.Fatalf("backup hook: %v", err)
			}
			if len(f.specs) != 1 {
				t.Fatalf("container specs: %d", len(f.specs))
			}
			var spec struct {
				Image      string
				HostConfig struct {
					NanoCpus, Memory int64
					Mounts           []struct{ Source, Target string }
				}
			}
			if err := json.Unmarshal(f.specs[0], &spec); err != nil {
				t.Fatal(err)
			}
			if spec.Image != image || spec.HostConfig.NanoCpus != 1500000000 || spec.HostConfig.Memory != 128*1024*1024 || len(spec.HostConfig.Mounts) != 1 || spec.HostConfig.Mounts[0].Source != "proj-test_newdata" || spec.HostConfig.Mounts[0].Target != "/data" {
				t.Fatalf("wrong image or mounts: %s", f.specs[0])
			}
		})
	}
}

func TestPostDeployVolumeBackupRunsNewBeforeJob(t *testing.T) {
	old := runManifest("web")
	c, base, s := runFixture(t, old)
	f := &jobFake{runFake: base}
	c.Engine = f
	sched := &jobs.Scheduler{Env: "test", Store: s, Manifest: func(context.Context) (manifest.Manifest, error) { return old, nil }, Runner: jobs.RunnerFunc(c.RunJob)}
	c.Backups = heldJobBackup{sched}
	m := runManifest("web")
	role := m.Apps["web"].Roles["web"]
	role.Volumes = []string{"newdata:/data"}
	m.Apps["web"].Roles["web"] = role
	m.Jobs = map[string]manifest.Job{"prepare": {App: "web", Role: "web", Command: []string{"prepare"}}}
	m.Backups = map[string]manifest.Backup{"data": {App: "web", Volume: "newdata", Before: "prepare", Destination: "offsite"}}
	if err := s.AddClaims(context.Background(), []state.Claim{{Kind: state.ClaimVolume, Name: "proj-test_newdata"}}); err != nil {
		t.Fatal(err)
	}
	op, _, err := s.Admit(context.Background(), state.Admission{RequestID: "post-job", ActorID: "admin", Kind: "deploy", App: "web", PublicRequest: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Finish(context.Background(), op.ID, state.Outcome{State: "succeeded"}); err != nil {
			t.Error(err)
		}
	})
	image := "example/web@sha256:" + strings.Repeat("b", 64)
	p := preparedPlan{Image: image, Request: Request{App: "web", Manifest: m}}
	if err := c.runBackupHook(context.Background(), op.ID, p, "post_hook", 1, "data"); err != nil {
		t.Fatalf("backup hook: %v", err)
	}
	if len(f.specs) != 1 || !strings.Contains(string(f.specs[0]), "proj-test_newdata") || !strings.Contains(string(f.specs[0]), image) {
		t.Fatalf("job specs: %s", f.specs)
	}
}

func TestPostDeployBackupOfAnotherAppUsesItsAppliedRelease(t *testing.T) {
	old := runManifest("web")
	role := old.Apps["web"].Roles["web"]
	role.Volumes = []string{"old:/old"}
	old.Apps["web"].Roles["web"] = role
	c, base, s := runFixture(t, old)
	f := &jobFake{runFake: base, streamOut: "dump"}
	c.Engine, c.Backups = f, realCommandBackup{c}
	m := runManifest("web")
	role = m.Apps["web"].Roles["web"]
	role.Volumes = []string{"peer:/peer"}
	m.Apps["web"].Roles["web"] = role
	m.Backups = map[string]manifest.Backup{"dump": {App: "web", Role: "web", Command: []string{"dump"}, Destination: "offsite"}}
	op, _, err := s.Admit(context.Background(), state.Admission{RequestID: "post-other", ActorID: "admin", Kind: "deploy", App: "other", PublicRequest: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Finish(context.Background(), op.ID, state.Outcome{State: "succeeded"}); err != nil {
			t.Error(err)
		}
	})
	p := preparedPlan{Image: "example/other@sha256:" + strings.Repeat("b", 64), Request: Request{App: "other", Manifest: m}}
	if err := c.runBackupHook(context.Background(), op.ID, p, "post_hook", 1, "dump"); err != nil {
		t.Fatal(err)
	}
	if len(f.specs) != 1 || !strings.Contains(string(f.specs[0]), runDigest) || !strings.Contains(string(f.specs[0]), "proj-test_old") || strings.Contains(string(f.specs[0]), "proj-test_peer") {
		t.Fatalf("specs: %s", f.specs)
	}
}

func TestDeployPostCommandBackupRunsNewRelease(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	f := &jobFake{runFake: newRunFake(), streamOut: "dump"}
	c := &Controller{rolloutHold: zeroTestRolloutHold(), Engine: f, Store: s, Environment: "test"}
	c.Backups = realCommandBackup{c}
	t.Cleanup(func() {
		if err := c.Drain(context.Background()); err != nil {
			t.Error(err)
		}
	})
	m := dockerManifest(nil)
	m.Destinations = map[string]manifest.Destination{"offsite": {Type: "s3", Bucket: "bkt", AccessKeySecret: "ACCESS", SecretKeySecret: "SECRET"}}
	m.Backups = map[string]manifest.Backup{"dump": {App: "web", Role: "web", Command: []string{"dump"}, Destination: "offsite"}}
	app := m.Apps["web"]
	app.Hooks.PostDeploy = []manifest.Hook{{Backup: "dump"}}
	m.Apps["web"] = app
	for i, mount := range []string{"first:/first", "second:/second"} {
		app := m.Apps["web"]
		role := app.Roles["web"]
		role.Volumes = []string{mount}
		app.Roles["web"] = role
		m.Apps["web"] = app
		digit := []string{"b", "c"}[i]
		req := Request{RequestID: "deploy-" + digit, App: "web", Version: "sha256:" + strings.Repeat(digit, 64), Manifest: m}
		op, _, err := admit(t, c, ctx, Actor{ID: "admin", Admin: true}, req)
		if err != nil {
			t.Fatal(err)
		}
		got := waitOperation(t, s, op.ID)
		if err := c.Drain(ctx); err != nil {
			t.Fatal(err)
		}
		if got.State != "succeeded" {
			t.Fatalf("deploy %d: %+v", i, got)
		}
		if len(f.specs) != i+1 {
			t.Fatalf("deploy %d specs: %d", i, len(f.specs))
		}
		spec := string(f.specs[i])
		if !strings.Contains(spec, "sha256:"+strings.Repeat(digit, 64)) || !strings.Contains(spec, "proj-test_"+strings.Split(mount, ":")[0]) {
			t.Fatalf("deploy %d backup spec: %s", i, spec)
		}
	}
}

type crossAppJobBackup struct {
	scheduler *jobs.Scheduler
	manifest  manifest.Manifest
}

func (b *crossAppJobBackup) RunBackup(ctx context.Context, m manifest.Manifest, name string) (BackupResult, error) {
	b.manifest = m
	run, release, err := b.scheduler.RunHeld(ctx, m.Backups[name].Before)
	if err != nil {
		return BackupResult{}, err
	}
	defer release()
	return BackupResult{Status: run.Status, Detail: run.Detail}, nil
}

func TestPostDeployBackupOfAnotherAppRunsNewBeforeJob(t *testing.T) {
	ctx := context.Background()
	old := runManifest("web")
	old.Apps["db"] = manifest.App{Kind: "image", Roles: map[string]manifest.Role{"db": {Volumes: []string{"dbdata:/db"}}}}
	c, base, s := runFixture(t, old)
	dbManifest, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	dbOp, _, err := s.Admit(ctx, state.Admission{RequestID: "db-deployed", ActorID: "admin", Kind: "deploy", App: "db", PublicRequest: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Finish(ctx, dbOp.ID, state.Outcome{State: "succeeded", Release: &state.Release{App: "db", Manifest: dbManifest}}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddClaims(ctx, []state.Claim{{Kind: state.ClaimVolume, Name: "proj-test_dbdata"}}); err != nil {
		t.Fatal(err)
	}
	f := &jobFake{runFake: base}
	c.Engine = f
	scheduler := &jobs.Scheduler{Env: "test", Store: s, Manifest: func(context.Context) (manifest.Manifest, error) { return old, nil }, Runner: jobs.RunnerFunc(c.RunJob)}
	backup := &crossAppJobBackup{scheduler: scheduler}
	c.Backups = backup
	m := runManifest("web")
	role := m.Apps["web"].Roles["web"]
	role.Volumes = []string{"newdata:/data"}
	m.Apps["web"].Roles["web"] = role
	m.Apps["db"] = manifest.App{Kind: "image", Roles: map[string]manifest.Role{"db": {Volumes: []string{"other:/db"}}}}
	m.Jobs = map[string]manifest.Job{"prepare": {App: "web", Role: "web", Command: []string{"prepare"}}}
	m.Backups = map[string]manifest.Backup{"data": {App: "db", Volume: "dbdata", Before: "prepare", Destination: "offsite"}}
	op, _, err := s.Admit(ctx, state.Admission{RequestID: "cross-app", ActorID: "admin", Kind: "deploy", App: "web", PublicRequest: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Finish(ctx, op.ID, state.Outcome{State: "succeeded"}); err != nil {
			t.Error(err)
		}
	})
	image := "example/web@sha256:" + strings.Repeat("b", 64)
	p := preparedPlan{Image: image, Request: Request{App: "web", Manifest: m}}
	if err := c.runBackupHook(ctx, op.ID, p, "post_hook", 1, "data"); err != nil {
		t.Fatalf("cross-app backup: %v", err)
	}
	if got := backup.manifest.Apps["db"].Roles["db"].Volumes; len(got) != 1 || got[0] != "dbdata:/db" {
		t.Fatalf("backup did not use applied db mounts: %v", got)
	}
	if len(f.specs) != 1 || !strings.Contains(string(f.specs[0]), image) || !strings.Contains(string(f.specs[0]), "proj-test_newdata") {
		t.Fatalf("before job did not use verified web release: %s", f.specs)
	}
}
