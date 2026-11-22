package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"berthy/internal/backup"
	"berthy/internal/manifest"
	"berthy/internal/state"
)

// fakeBackups is the agent's side of a backup hook. Its calls go into the order of the engine's, so a test sees where in the
// run of a deploy a backup happened.
type fakeBackups struct {
	f        *hookEngine
	result   BackupResult
	err      error
	ran      []string
	manifest manifest.Manifest
}

func (b *fakeBackups) RunBackup(_ context.Context, m manifest.Manifest, name string) (BackupResult, error) {
	b.manifest = m
	b.ran = append(b.ran, name)
	b.f.calls = append(b.f.calls, "backup:"+name)
	if _, ok := m.Backups[name]; !ok {
		return BackupResult{}, errors.New("the backup is not in the manifest it was given")
	}
	return b.result, b.err
}

// backupHookRig is hookFixture with the apps in deployed already applied, and a backup main-db of the app db. The hook
// operation holds the lock, so a hook that runs one-off commands can be driven too.
func backupHookRig(t *testing.T, result BackupResult, deployed ...string) (*Controller, *hookEngine, *fakeBackups, string, preparedPlan) {
	return backupHookRigWithApp(t, result, manifest.App{Kind: "image", Roles: map[string]manifest.Role{"db": {Volumes: []string{"dumps:/data"}}}}, deployed...)
}

func backupHookRigWithApp(t *testing.T, result BackupResult, target manifest.App, deployed ...string) (*Controller, *hookEngine, *fakeBackups, string, preparedPlan) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(dir, "key")
	if err := os.WriteFile(key, make([]byte, 32), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := state.Open(dir, key, "stage")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	ctx := context.Background()
	applied := manifest.Manifest{Project: "proj", Apps: map[string]manifest.App{"db": target}}
	encoded, err := json.Marshal(applied)
	if err != nil {
		t.Fatal(err)
	}
	for _, app := range deployed {
		op, _, err := s.Admit(ctx, state.Admission{RequestID: "deploy-" + app, ActorID: "actor", Kind: "deploy", App: app, PublicRequest: []byte(`{}`)})
		if err != nil {
			t.Fatal(err)
		}
		if err = s.Finish(ctx, op.ID, state.Outcome{State: "succeeded", Release: &state.Release{App: app, Manifest: encoded}}); err != nil {
			t.Fatal(err)
		}
	}
	claims := []state.Claim{{Kind: state.ClaimVolume, Name: applied.VolumeName("stage", "dumps")}, {Kind: state.ClaimComposeProject, Name: applied.ComposeProject("stage", "db")}}
	if err := s.AddClaims(ctx, claims); err != nil {
		t.Fatal(err)
	}
	op, _, err := s.Admit(ctx, state.Admission{RequestID: "req", ActorID: "actor", Kind: "deploy", PublicRequest: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Finish(context.Background(), op.ID, state.Outcome{State: "succeeded"}); err != nil {
			t.Error(err)
		}
	})
	f := &hookEngine{store: s, op: op.ID}
	fb := &fakeBackups{f: f, result: result}
	c := &Controller{Engine: f, Store: s, Environment: "stage", Backups: fb}
	m := manifest.Manifest{Project: "proj", Apps: map[string]manifest.App{"app": {}, "db": {Kind: "compose"}},
		Backups: map[string]manifest.Backup{"main-db": {App: "db", Volume: "dumps", Destination: "offsite"}}}
	p := preparedPlan{Image: "registry/app@sha256:pinned", NetworkID: "overlay", Request: Request{App: "app", Manifest: m}}
	return c, f, fb, op.ID, p
}

func setHooks(p preparedPlan, pre, post []manifest.Hook, m *manifest.Migrations) {
	a := p.Request.Manifest.Apps["app"]
	a.Hooks.PreDeploy, a.Hooks.PostDeploy, a.Migrations = pre, post, m
	p.Request.Manifest.Apps["app"] = a
}

func TestABackupHookRunsInOrderWithTheRunHooksAndBeforeTheMigration(t *testing.T) {
	c, f, fb, id, p := backupHookRig(t, BackupResult{RunID: 7, Status: "succeeded"}, "db")
	setHooks(p, []manifest.Hook{{Run: []string{"first"}}, {Backup: "main-db"}, {Run: []string{"third"}}}, nil, &manifest.Migrations{Version: []string{"version"}, Apply: []string{"apply"}})
	f.exits = []int{0, 0, 0, 0, 0}
	f.logs = [][]byte{[]byte("v1\n"), []byte("v2\n")}
	before, after, err := c.runPre(context.Background(), id, p)
	if err != nil || before != "v1" || after != "v2" {
		t.Fatalf("runPre: %q %q %v", before, after, err)
	}
	var order []string
	for _, call := range f.calls {
		if call == "create" || strings.HasPrefix(call, "backup:") {
			order = append(order, call)
		}
	}
	// first, the backup, third, schema before, apply, schema after.
	want := []string{"create", "backup:main-db", "create", "create", "create", "create"}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Fatalf("order %v, want %v", order, want)
	}
	if len(fb.ran) != 1 {
		t.Fatalf("backups run: %v", fb.ran)
	}
}

func TestAFailedPreDeployBackupStopsTheDeployBeforeTheMigration(t *testing.T) {
	c, f, _, id, p := backupHookRig(t, BackupResult{RunID: 9, Status: "failed", Detail: "upload failed: access denied"}, "db")
	setHooks(p, []manifest.Hook{{Backup: "main-db"}, {Run: []string{"never"}}}, nil, &manifest.Migrations{Version: []string{"version"}, Apply: []string{"apply"}})
	before, after, err := c.runPre(context.Background(), id, p)
	if err == nil || before != "" || after != "" {
		t.Fatalf("a failed backup did not stop the deploy: %q %q %v", before, after, err)
	}
	if strings.Contains(strings.Join(f.calls, ","), "create") || len(f.specs) != 0 {
		t.Fatalf("a command ran after the failed backup: %v", f.calls)
	}
	d := failureDiagnostics(err)
	if len(d) != 1 || d[0].Kind != "backup" || d[0].Name != "main-db" || d[0].Role != "pre_hook" || d[0].Index != 1 || d[0].Error != "upload failed: access denied" || d[0].Logs != "" {
		t.Fatalf("diagnostics: %+v", d)
	}
	if reason := failureReason(d, ""); reason != "pre-deploy backup main-db failed: upload failed" {
		t.Fatalf("reason %q", reason)
	}
	if got := hookFailureText(err, "pre"); got != "pre-deploy backup failed" || !PublicMessage(got) {
		t.Fatalf("operation error %q", got)
	}
	if got := hookFailureText(errors.New("x"), "pre"); got != "pre-deploy command failed" {
		t.Fatalf("a command failure: %q", got)
	}
}

func TestABackupThatCannotStartFailsTheHookWithItsRefusal(t *testing.T) {
	c, _, fb, id, p := backupHookRig(t, BackupResult{}, "db")
	fb.err = backup.ErrCredentials
	setHooks(p, []manifest.Hook{{Backup: "main-db"}}, nil, nil)
	_, _, err := c.runPre(context.Background(), id, p)
	d := failureDiagnostics(err)
	if len(d) != 1 || failureReason(d, "") != "pre-deploy backup main-db failed: backup credentials unavailable" {
		t.Fatalf("diagnostics %+v for %v", d, err)
	}
	fb.err = errors.New("dial tcp 10.0.0.5: connection refused")
	_, _, err = c.runPre(context.Background(), id, p)
	if reason := failureReason(failureDiagnostics(err), ""); strings.Contains(reason, "10.0.0.5") || reason != "pre-deploy backup main-db failed: backup could not run" {
		t.Fatalf("reason %q", reason)
	}
	c.Backups = nil
	_, _, err = c.runPre(context.Background(), id, p)
	if err == nil || failureReason(failureDiagnostics(err), "") != "pre-deploy backup main-db failed: backups unavailable" {
		t.Fatalf("without a runner: %v", err)
	}
}

func TestABackupHookOfAnAppThatIsNotDeployedIsSkipped(t *testing.T) {
	c, _, fb, id, p := backupHookRig(t, BackupResult{Status: "failed", Detail: "archive failed"})
	setHooks(p, []manifest.Hook{{Backup: "main-db"}}, nil, nil)
	// Nothing is deployed in the fixture's store, so the first deploy has no volume to back up.
	if _, _, err := c.runPre(context.Background(), id, p); err != nil {
		t.Fatalf("a first deploy was stopped by a backup of nothing: %v", err)
	}
	if len(fb.ran) != 0 {
		t.Fatalf("a backup of an app that is not deployed ran: %v", fb.ran)
	}
	events, _ := c.Store.EventsAfter(context.Background(), id, 0, 10)
	var steps []string
	for _, e := range events {
		steps = append(steps, string(e.Data))
	}
	if !strings.Contains(strings.Join(steps, " "), "skipped") {
		t.Fatalf("events: %v", steps)
	}
}

func TestAnAppliedReleaseMakesTheBackupAppDeployed(t *testing.T) {
	c, _, fb, id, p := backupHookRig(t, BackupResult{RunID: 1, Status: "succeeded"}, "db")
	setHooks(p, []manifest.Hook{{Backup: "main-db"}}, nil, nil)
	if _, _, err := c.runPre(context.Background(), id, p); err != nil {
		t.Fatal(err)
	}
	if len(fb.ran) != 1 {
		t.Fatalf("the backup of a deployed app did not run: %v", fb.ran)
	}
}

func TestAFailedPostDeployBackupIsReportedLikeAPostCommand(t *testing.T) {
	c, _, fb, id, p := backupHookRig(t, BackupResult{RunID: 3, Status: "failed", Detail: "preparation failed: exit status 3"}, "db")
	setHooks(p, nil, []manifest.Hook{{Backup: "main-db"}}, nil)
	err := c.runPost(context.Background(), id, p)
	d := failureDiagnostics(err)
	if len(d) != 1 || d[0].Role != "post_hook" || failureReason(d, "") != "post-deploy backup main-db failed: preparation failed" {
		t.Fatalf("diagnostics %+v for %v", d, err)
	}
	if got := hookFailureText(err, "post"); got != "post-deploy backup failed" || !PublicMessage(got) {
		t.Fatalf("operation error %q", got)
	}
	fb.result = BackupResult{RunID: 4, Status: "succeeded"}
	if err = c.runPost(context.Background(), id, p); err != nil {
		t.Fatalf("a successful post-deploy backup: %v", err)
	}
}

func TestBackupHookUsesAppliedComposeProject(t *testing.T) {
	c, _, fb, id, p := backupHookRigWithApp(t, BackupResult{Status: "succeeded"}, manifest.App{Kind: "compose", ProjectName: "owned-project"}, "db")
	ctx := context.Background()
	app := p.Request.Manifest.Apps["db"]
	app.ProjectName = "peer-project"
	p.Request.Manifest.Apps["db"] = app
	p.Request.Manifest.Backups["main-db"] = manifest.Backup{App: "db", Command: []string{"dump"}, Destination: "offsite"}
	if err := c.runBackupHook(ctx, id, p, "pre_hook", 1, "main-db"); err != nil {
		t.Fatal(err)
	}
	if got := fb.manifest.ComposeProject("stage", "db"); got != "owned-project" {
		t.Fatalf("backup ran against project %q", got)
	}
	if p.Request.Manifest.ComposeProject("stage", "db") != "peer-project" {
		t.Fatal("incoming manifest was changed")
	}
}

func TestBackupHookRefusesUnownedAppliedTargets(t *testing.T) {
	for _, tc := range []struct {
		name   string
		app    manifest.App
		backup manifest.Backup
		claim  state.Claim
	}{
		{"project", manifest.App{Kind: "compose", ProjectName: "owned-project"}, manifest.Backup{App: "db", Command: []string{"dump"}}, state.Claim{Kind: state.ClaimComposeProject, Name: "owned-project"}},
		{"volume", manifest.App{Kind: "image", Roles: map[string]manifest.Role{"db": {Volumes: []string{"dumps:/data"}}}}, manifest.Backup{App: "db", Volume: "dumps"}, state.Claim{Kind: state.ClaimVolume, Name: "proj-stage_dumps"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _, fb, id, p := backupHookRigWithApp(t, BackupResult{Status: "succeeded"}, tc.app, "db")
			if err := c.Store.ReleaseClaims(context.Background(), []state.Claim{tc.claim}); err != nil {
				t.Fatal(err)
			}
			p.Request.Manifest.Backups["main-db"] = tc.backup
			if err := c.runBackupHook(context.Background(), id, p, "pre_hook", 1, "main-db"); err == nil || len(fb.ran) != 0 {
				t.Fatalf("err %v; backups %v", err, fb.ran)
			}
		})
	}
}
