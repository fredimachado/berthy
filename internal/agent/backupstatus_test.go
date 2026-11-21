package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"berthy/internal/api"
	"berthy/internal/backup"
	"berthy/internal/deploy"
	"berthy/internal/manifest"
	"berthy/internal/state"
)

// statusManifest has an image app with a scheduled backup and one that only runs on demand, and a Compose app with a backup.
const statusManifest = `{"project":"demo","network":"demo",
	"apps":{"api":{"kind":"image","image":"ghcr.io/acme/api","roles":{"web":{"volumes":["data:/data"]}}},"db":{"kind":"compose","file":"db/compose.yml"}},
	"destinations":{"offsite":{"type":"s3","bucket":"bkt","access_key_secret":"A","secret_key_secret":"B"}},
	"backups":{"data":{"app":"api","volume":"data","destination":"offsite","schedule":"0 * * * *"},
		"manual":{"app":"api","volume":"data","destination":"offsite"},
		"dumps":{"app":"db","volume":"dumps","destination":"offsite"}}}`

func finishedBackup(t *testing.T, s *state.Store, backupName, key string, started time.Time, status string, size int64, detail string) {
	t.Helper()
	ctx := context.Background()
	id, err := s.StartBackup(ctx, state.BackupRun{Backup: backupName, App: "api", Volume: "data", Destination: "offsite", ObjectKey: key, Trigger: "manual", Started: started})
	if err != nil {
		t.Fatal(err)
	}
	sum := ""
	if status == "succeeded" {
		sum = strings.Repeat("ab", 32)
	}
	if err = s.FinishBackup(ctx, id, status, size, sum, detail); err != nil {
		t.Fatal(err)
	}
}

func TestDescribeAndPsShowTheAgeOfAnAppsBackups(t *testing.T) {
	s := openIngressStore(t)
	recordBoundRelease(t, s, "release-backups", statusManifest, []state.Binding{{App: "api", Role: "web", Name: "lab-api-web", ServiceID: "svc-1"}})
	finishedBackup(t, s, "data", "k1", time.Now().Add(-3*time.Hour), "succeeded", 1536, "")
	finishedBackup(t, s, "data", "k2", time.Now().Add(-time.Minute), "failed", 0, "upload failed: access denied")
	h := NewDeploymentHandler(monitoringConfig(""), fakeDocker(t), nil, &deploy.Controller{Store: s, Environment: "lab"})
	w := serveRequest(t, h, http.MethodGet, "/v1/projects/demo/environments/lab/apps/api", monitoringToken("a"))
	var app api.App
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &app) != nil {
		t.Fatalf("describe: %d %s", w.Code, w.Body.String())
	}
	if len(app.Backups) != 2 || app.Backups[0].Name != "data" || app.Backups[1].Name != "manual" {
		t.Fatalf("backups: %+v", app.Backups)
	}
	d := app.Backups[0]
	if d.Volume != "data" || d.Schedule != "0 * * * *" || d.LastSuccess == nil || !strings.HasSuffix(d.Age, "s") || d.Size != 1536 || d.LastStatus != "failed" || d.Detail != "upload failed: access denied" {
		t.Fatalf("data: %+v", d)
	}
	if m := app.Backups[1]; m.LastSuccess != nil || m.Age != "" || m.LastStatus != "" {
		t.Fatalf("a backup that never ran: %+v", m)
	}
	// ps lists the same backups under each app.
	w = serveRequest(t, h, http.MethodGet, "/v1/projects/demo/environments/lab/apps", monitoringToken("a"))
	var apps []api.App
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &apps) != nil {
		t.Fatalf("ps: %d %s", w.Code, w.Body.String())
	}
	found := false
	for _, a := range apps {
		if a.Ref.Name == "api" && a.Ref.Kind == "image" {
			found = len(a.Backups) == 2 && a.Backups[0].Age != ""
		}
	}
	if !found {
		t.Fatalf("ps: %s", w.Body.String())
	}
	// An app without backups keeps its old shape.
	recordBoundRelease(t, s, "release-none", monitoredManifest(``), []state.Binding{{App: "api", Role: "web", Name: "lab-api-web", ServiceID: "svc-1"}})
	w = serveRequest(t, h, http.MethodGet, "/v1/projects/demo/environments/lab/apps/api", monitoringToken("a"))
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), `"backups"`) {
		t.Fatalf("app without backups: %d %s", w.Code, w.Body.String())
	}
}

func TestAppBackupsMatchesImageAndComposeApps(t *testing.T) {
	s := openIngressStore(t)
	ctx := context.Background()
	recordRelease(t, s, "release-backups", statusManifest)
	d := &deploy.Controller{Store: s, Environment: "lab"}
	names := func(app api.App) string {
		var out []string
		for _, b := range appBackups(ctx, d, app, time.Now()) {
			out = append(out, b.Name)
		}
		return strings.Join(out, ",")
	}
	if got := names(api.App{Ref: api.AppRef{Kind: "image", Name: "api"}}); got != "data,manual" {
		t.Errorf("image app: %q", got)
	}
	if got := names(api.App{Ref: api.AppRef{Kind: "compose", Name: "demo-lab-db"}}); got != "dumps" {
		t.Errorf("compose app: %q", got)
	}
	if got := names(api.App{Ref: api.AppRef{Kind: "swarm", Name: "api"}}); got != "" {
		t.Errorf("unmanaged service: %q", got)
	}
	if got := appBackups(ctx, nil, api.App{}, time.Now()); got != nil {
		t.Errorf("without state: %+v", got)
	}
}

func TestDoctorShowsEachBackupsCheckAndNeverFailsTheExitCode(t *testing.T) {
	s := openIngressStore(t)
	ctx := context.Background()
	recordRelease(t, s, "release-backups", statusManifest)
	byName := func() map[string]api.Check {
		out := map[string]api.Check{}
		for _, c := range monitoringChecks(ctx, monitoringConfig(""), s, nil, "lab") {
			out[c.Name] = c
		}
		return out
	}
	if c := byName()["backup:data"]; !c.OK || c.Critical || c.Detail != "not checked yet" {
		t.Fatalf("before the monitor ran: %+v", c)
	}
	for name, st := range map[string]state.CheckState{
		"backup:data":   {Name: "backup:data", State: "critical", Detail: "overdue: last success x (expected by y)", Since: time.Now()},
		"backup:manual": {Name: "backup:manual", State: "ok", Detail: "last success 5m ago, 2.0 KiB", Since: time.Now()},
		"backup:dumps":  {Name: "backup:dumps", State: "warning", Detail: "size dropped to 40% of the previous backup", Since: time.Now()},
	} {
		if err := s.PutCheck(ctx, st); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	got := byName()
	if c := got["backup:data"]; c.OK || c.Critical || c.Detail != "critical: overdue: last success x (expected by y)" {
		t.Errorf("critical backup: %+v", c)
	}
	if c := got["backup:manual"]; !c.OK || c.Critical || !strings.Contains(c.Detail, "last success 5m ago") {
		t.Errorf("ok backup: %+v", c)
	}
	if c := got["backup:dumps"]; c.OK || c.Critical {
		t.Errorf("warning backup: %+v", c)
	}
}

func TestTheAgentsSchedulerRunsADueBackupAsTheBackup(t *testing.T) {
	g := newBackupRig(t)
	ctx := context.Background()
	// The schedule matches the fake clock below (10:00 UTC on 16 November) and practically never the real clock, which the rig's own scheduler follows.
	scheduled := strings.Replace(backupManifest(g.url), `"uploads":{"app":"api","volume":"uploads","destination":"offsite"}`, `"uploads":{"app":"api","volume":"uploads","destination":"offsite","schedule":"0 10 16 11 *"}`, 1)
	recordAppRelease(t, g.store, "r-sched-api", "api", scheduled)
	// The rig's own scheduler runs on the real clock, so the test drives a second set of backups on a clock of its own.
	arch := &backup.Archiver{Engine: g.docker, Image: func(context.Context) (string, error) { return "berthy-helper:aabbccddeeff", nil }, Env: "lab"}
	b := newBackups("lab", g.ctrl, arch, nil, func(string) {})
	b.Scheduler.Now = func() time.Time { return time.Date(2026, 11, 16, 10, 0, 30, 0, time.UTC) }
	b.Scheduler.Step(ctx)
	b.Scheduler.Wait()
	runs, _ := g.store.BackupRuns(ctx, "uploads", 10)
	if len(runs) != 1 || runs[0].Trigger != "schedule" || runs[0].Status != "succeeded" || !runs[0].Scheduled.Equal(time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("runs: %+v", runs)
	}
	if m, _ := runningManifest(ctx, g.store); m.Backups["docs"].Schedule != "" {
		t.Fatal("a backup without a schedule has one")
	}
	audit, _ := g.store.AuditLog(ctx, 10)
	var actors []string
	for _, a := range audit {
		if a.Action == "backup_run:uploads" {
			actors = append(actors, a.ActorName+":"+a.Outcome)
		}
	}
	if strings.Join(actors, ",") != "backup:uploads:succeeded,backup:uploads:started" {
		t.Fatalf("audit: %v", actors)
	}
}

func TestADeployHookBacksUpThroughTheAgent(t *testing.T) {
	g := newBackupRig(t)
	if g.ctrl.Backups == nil {
		t.Fatal("the controller cannot run backup hooks")
	}
	var m manifest.Manifest
	m, _ = runningManifest(context.Background(), g.store)
	res, err := g.ctrl.Backups.RunBackup(context.Background(), m, "uploads")
	if err != nil || res.Status != "succeeded" || res.RunID == 0 || res.Detail != "" {
		t.Fatalf("hook backup: %+v %v", res, err)
	}
	runs, _ := g.store.BackupRuns(context.Background(), "uploads", 5)
	if len(runs) != 1 || runs[0].Trigger != "hook" {
		t.Fatalf("runs: %+v", runs)
	}
	if _, err = g.ctrl.Backups.RunBackup(context.Background(), m, "nope"); err == nil {
		t.Fatal("an unknown backup ran")
	}
	res, err = g.ctrl.Backups.RunBackup(context.Background(), m, "lost")
	if err == nil || !strings.Contains(err.Error(), "not mounted") || res.RunID != 0 {
		t.Fatalf("a backup that cannot start: %+v %v", res, err)
	}
}

func TestBackupRunsShowTheOccurrenceOfAScheduledRun(t *testing.T) {
	g := newBackupRig(t)
	ctx := context.Background()
	at := time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)
	if _, err := g.store.StartBackup(ctx, state.BackupRun{Backup: "uploads", App: "api", Volume: "uploads", Destination: "offsite", ObjectKey: "k-sched", Trigger: "schedule", Scheduled: at, Started: at}); err != nil {
		t.Fatal(err)
	}
	w := g.call("GET", "/v1/projects/demo/environments/lab/backups/uploads/runs", readToken, "")
	var runs []api.BackupRun
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &runs) != nil || len(runs) != 1 || runs[0].Scheduled == nil || !runs[0].Scheduled.Equal(at) || runs[0].Trigger != "schedule" {
		t.Fatalf("runs: %d %s", w.Code, w.Body.String())
	}
}
