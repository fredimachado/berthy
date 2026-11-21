package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"berthy/internal/api"
	"berthy/internal/backup"
	"berthy/internal/deploy"
	"berthy/internal/engine"
	"berthy/internal/manifest"
	"berthy/internal/objectstore"
	"berthy/internal/state"

	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"
)

// fakeArchiveEngine is the Docker side of the archive and restore containers. A volume archives to its payload; a restore
// records the bytes it was fed.
type fakeArchiveEngine struct {
	mu         sync.Mutex
	volumes    map[string]string // volume -> what its archive container prints
	fed        map[string]string // volume -> what a restore container was fed
	containers map[string]fakeArchiveContainer
	archived   []string
	block      chan struct{} // an archive does not end until this is closed, when set
	inventory  []engine.InventoryContainer
	removedC   []string
	n          int
}

type fakeArchiveContainer struct {
	volume  string
	restore bool
}

func newFakeArchiveEngine() *fakeArchiveEngine {
	return &fakeArchiveEngine{volumes: map[string]string{}, fed: map[string]string{}, containers: map[string]fakeArchiveContainer{}}
}

func (f *fakeArchiveEngine) CreateContainer(_ context.Context, _ string, spec json.RawMessage) (string, error) {
	var s struct {
		Entrypoint []string
		HostConfig struct{ Mounts []struct{ Source string } }
	}
	if err := json.Unmarshal(spec, &s); err != nil || len(s.HostConfig.Mounts) != 1 {
		return "", fmt.Errorf("unexpected spec %s", spec)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n++
	id := fmt.Sprintf("c%d", f.n)
	f.containers[id] = fakeArchiveContainer{volume: s.HostConfig.Mounts[0].Source, restore: s.Entrypoint[1] == "volume-extract"}
	return id, nil
}

type waitReader struct {
	r     io.Reader
	block chan struct{}
	done  bool
}

func (w *waitReader) Read(p []byte) (int, error) {
	n, err := w.r.Read(p)
	if err == io.EOF && !w.done && w.block != nil {
		<-w.block
		w.done = true
	}
	return n, err
}

type captureStdin struct {
	f      *fakeArchiveEngine
	volume string
}

func (c captureStdin) Write(p []byte) (int, error) {
	c.f.mu.Lock()
	c.f.fed[c.volume] += string(p)
	c.f.mu.Unlock()
	return len(p), nil
}
func (captureStdin) Close() error { return nil }

func (f *fakeArchiveEngine) Attach(_ context.Context, id string, stdin bool) (io.ReadCloser, io.WriteCloser, *engine.BoundedBuffer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.containers[id]
	if stdin {
		return io.NopCloser(strings.NewReader("")), captureStdin{f, c.volume}, engine.NewBoundedBuffer(16), nil
	}
	f.archived = append(f.archived, c.volume)
	return io.NopCloser(&waitReader{r: strings.NewReader(f.volumes[c.volume]), block: f.block}), nil, engine.NewBoundedBuffer(16), nil
}

func (f *fakeArchiveEngine) StartContainer(context.Context, string) error { return nil }
func (f *fakeArchiveEngine) WaitContainer(context.Context, string) (int, error) {
	return 0, nil
}
func (f *fakeArchiveEngine) RemoveContainer(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removedC = append(f.removedC, id)
	return nil
}
func (f *fakeArchiveEngine) InventoryContainers(context.Context) ([]engine.InventoryContainer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.inventory, nil
}
func (f *fakeArchiveEngine) VolumeExists(_ context.Context, name string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.volumes[name]
	return ok, nil
}
func (f *fakeArchiveEngine) CreateVolume(_ context.Context, name string, _ map[string]string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.volumes[name] = ""
	return nil
}
func (f *fakeArchiveEngine) RemoveVolume(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.volumes, name)
	return nil
}
func (f *fakeArchiveEngine) has(volume string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.volumes[volume]
	return ok
}

// configCompose answers `compose config` with a fixed document.
type configCompose struct {
	mu   sync.Mutex
	out  string
	runs []engine.ComposeRun
}

func (c *configCompose) Run(_ context.Context, r engine.ComposeRun) ([]byte, []byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.runs = append(c.runs, r)
	return []byte(c.out), nil, nil
}

func backupToken(fill string) string { return "bty.lab." + strings.Repeat(fill, 64) }

func backupConfig() Config {
	return Config{Project: "demo", Environment: "lab", Tokens: []Token{
		{Name: "reader", Digest: Digest(backupToken("a")), Scope: "read"},
		{Name: "ci", Digest: Digest(backupToken("b")), Scope: "deploy"},
		{Name: "ops", Digest: Digest(backupToken("c")), Scope: "admin"},
	}}
}

const (
	readToken   = "a"
	deployToken = "b"
	adminToken  = "c"
)

type backupRig struct {
	t       *testing.T
	store   *state.Store
	ctrl    *deploy.Controller
	docker  *fakeArchiveEngine
	compose *configCompose
	backups *Backups
	h       http.Handler
	s3      objectstore.Store
	url     string
	ctx     context.Context
	cancel  context.CancelFunc
}

func backupManifest(endpoint string) string {
	return `{"project":"demo","network":"demo",
	"apps":{"api":{"kind":"image","image":"ghcr.io/acme/api","roles":{"web":{"volumes":["uploads:/data","cache:/cache:ro"]}}},
		"files":{"kind":"compose","file":"files/compose.yaml"},
		"gone":{"kind":"image","image":"ghcr.io/acme/gone","roles":{"web":{"volumes":["old:/old"]}}}},
	"destinations":{"offsite":{"type":"s3","endpoint":"` + endpoint + `","bucket":"bkt","access_key_secret":"S3_ACCESS_KEY","secret_key_secret":"S3_SECRET_KEY"}},
	"backups":{
		"uploads":{"app":"api","volume":"uploads","destination":"offsite"},
		"docs":{"app":"files","volume":"docs","destination":"offsite","prefix":"docs/archive","retention":{"keep":2}},
		"lost":{"app":"api","volume":"ghost","destination":"offsite"},
		"old":{"app":"gone","volume":"old","destination":"offsite"}}}`
}

func newBackupRig(t *testing.T) *backupRig {
	t.Helper()
	backend := s3mem.New()
	if err := backend.CreateBucket("bkt"); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(gofakes3.New(backend).Server())
	t.Cleanup(srv.Close)
	s := openIngressStore(t)
	g := &backupRig{t: t, url: srv.URL, store: s, docker: newFakeArchiveEngine(), compose: &configCompose{out: `{"volumes":{"docs":{"name":"files_docs"},"shared":{"name":"team-shared","external":true}}}`}}
	g.docker.volumes["uploads"] = "uploads-payload"
	g.docker.volumes["files_docs"] = "docs-payload"
	recordAppRelease(t, s, "r-api", "api", backupManifest(srv.URL))
	recordAppRelease(t, s, "r-files", "files", backupManifest(srv.URL))
	g.ctrl = &deploy.Controller{Store: s, Environment: "lab", Compose: g.compose, StateDir: t.TempDir()}
	arch := &backup.Archiver{Engine: g.docker, Image: func(context.Context) (string, error) { return "berthy-helper:aabbccddeeff", nil }, Env: "lab"}
	g.backups = newBackups("lab", g.ctrl, arch, nil, func(string) {})
	g.ctx, g.cancel = context.WithCancel(context.Background())
	g.backups.Start(g.ctx)
	t.Cleanup(func() { g.cancel(); g.backups.Wait() })
	ctx := context.Background()
	for name, value := range map[string]string{"S3_ACCESS_KEY": "AKIATESTACCESS", "S3_SECRET_KEY": "test-secret-key-0123456789"} {
		if err := s.PutSecret(ctx, state.Actor{ID: "ops", Name: "ops"}, state.SecretInfo{Name: name}, []byte(value)); err != nil {
			t.Fatal(err)
		}
	}
	var err error
	g.s3, err = objectstore.New(objectstore.Config{Endpoint: srv.URL, Bucket: "bkt", AccessKey: "AKIATESTACCESS", SecretKey: "test-secret-key-0123456789"})
	if err != nil {
		t.Fatal(err)
	}
	g.h = NewDeploymentHandlerWithBackups(backupConfig(), nil, nil, g.ctrl, nil, nil, g.backups)
	return g
}

func (g *backupRig) call(method, path, token, body string) *httptest.ResponseRecorder {
	g.t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+backupToken(token))
	}
	w := httptest.NewRecorder()
	g.h.ServeHTTP(w, r)
	return w
}

// run starts a manual run through the API and waits until it has ended.
func (g *backupRig) run(name string) api.BackupRun {
	g.t.Helper()
	w := g.call("POST", "/v1/projects/demo/environments/lab/backups/"+name+"/run", deployToken, "")
	var accepted struct {
		ID int64 `json:"backup_run_id"`
	}
	if w.Code != http.StatusAccepted || json.Unmarshal(w.Body.Bytes(), &accepted) != nil || accepted.ID == 0 {
		g.t.Fatalf("run %s: %d %s", name, w.Code, w.Body.String())
	}
	return g.wait(name, accepted.ID)
}

func (g *backupRig) wait(name string, id int64) api.BackupRun {
	g.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		w := g.call("GET", "/v1/projects/demo/environments/lab/backups/"+name+"/runs?limit=100", readToken, "")
		var runs []api.BackupRun
		if err := json.Unmarshal(w.Body.Bytes(), &runs); err != nil {
			g.t.Fatalf("runs: %d %s", w.Code, w.Body.String())
		}
		for _, r := range runs {
			if r.ID == id && r.Status != "running" {
				return r
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	g.t.Fatal("the run did not end")
	return api.BackupRun{}
}

func (g *backupRig) list(query string) []api.BackupEntry {
	g.t.Helper()
	w := g.call("GET", "/v1/projects/demo/environments/lab/backups"+query, readToken, "")
	var out []api.BackupEntry
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil {
		g.t.Fatalf("list: %d %s", w.Code, w.Body.String())
	}
	return out
}

func (g *backupRig) waitOperation(id string) state.Operation {
	g.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		op, err := g.store.Operation(context.Background(), id)
		if err != nil {
			g.t.Fatal(err)
		}
		if op.State != "running" {
			return op
		}
		time.Sleep(5 * time.Millisecond)
	}
	g.t.Fatal("operation did not finish")
	return state.Operation{}
}

func TestBackupOfAnImageAppVolumeRunsListsAndAudits(t *testing.T) {
	g := newBackupRig(t)
	run := g.run("uploads")
	if run.Status != "succeeded" || run.Trigger != "manual" || run.Size != int64(len("uploads-payload")) || len(run.SHA256) != 64 || run.Finished == nil {
		t.Fatalf("run: %+v", run)
	}
	if !strings.HasPrefix(run.Key, "api/uploads/uploads/") || !strings.HasSuffix(run.Key, fmt.Sprintf("-%d.tar.gz", run.ID)) {
		t.Fatalf("key %q", run.Key)
	}
	if len(g.docker.archived) != 1 || g.docker.archived[0] != "uploads" {
		t.Fatalf("archived volumes: %v", g.docker.archived)
	}
	entries := g.list("")
	if len(entries) != 1 || entries[0].Key != run.Key || entries[0].Backup != "uploads" || entries[0].App != "api" || entries[0].Volume != "uploads" || entries[0].SHA256 != run.SHA256 || entries[0].Size != run.Size {
		t.Fatalf("entries: %+v", entries)
	}
	if only := g.list("?backup=docs"); len(only) != 0 {
		t.Fatalf("filtered entries: %+v", only)
	}
	if w := g.call("GET", "/v1/projects/demo/environments/lab/backups?backup=nope", readToken, ""); w.Code != 404 {
		t.Fatalf("unknown filter: %d", w.Code)
	}
	if _, err := g.s3.Stat(context.Background(), strings.TrimSuffix(run.Key, ".tar.gz")+".json"); err != nil {
		t.Fatalf("sidecar: %v", err)
	}
	audit, _ := g.store.AuditLog(context.Background(), 20)
	var outcomes []string
	for _, a := range audit {
		if a.Action == "backup_run:uploads" && a.ActorName == "ci" {
			outcomes = append(outcomes, a.Outcome)
		}
	}
	if len(outcomes) != 2 || outcomes[0] != "succeeded" || outcomes[1] != "started" {
		t.Fatalf("audit outcomes (newest first): %v", outcomes)
	}
}

func TestBackupOfAComposeVolumeResolvesTheProjectsDockerName(t *testing.T) {
	g := newBackupRig(t)
	run := g.run("docs")
	if run.Status != "succeeded" || !strings.HasPrefix(run.Key, "docs/archive/docs/") {
		t.Fatalf("run: %+v", run)
	}
	if len(g.docker.archived) != 1 || g.docker.archived[0] != "files_docs" {
		t.Fatalf("archived volumes: %v", g.docker.archived)
	}
	if len(g.compose.runs) == 0 || g.compose.runs[0].Project != "demo-files" {
		t.Fatalf("compose runs: %+v", g.compose.runs)
	}
	// An external volume resolves to its own name, and a key the Compose file lacks is not mounted.
	m, err := runningManifest(context.Background(), g.store)
	if err != nil {
		t.Fatal(err)
	}
	name, err := resolveVolume(context.Background(), g.ctrl, m, manifestBackup(m, "docs", "shared"))
	if err != nil || name != "team-shared" {
		t.Fatalf("external volume: %q %v", name, err)
	}
	if _, err = resolveVolume(context.Background(), g.ctrl, m, manifestBackup(m, "docs", "nothing")); err == nil || err.Error() != backup.ErrNotMounted.Error() {
		t.Fatalf("missing key: %v", err)
	}
}

func TestBackupVolumeResolutionForImageApps(t *testing.T) {
	g := newBackupRig(t)
	m, _ := runningManifest(context.Background(), g.store)
	for volume, want := range map[string]string{"uploads": "uploads", "cache": "cache"} {
		got, err := resolveVolume(context.Background(), g.ctrl, m, manifestBackup(m, "uploads", volume))
		if err != nil || got != want {
			t.Errorf("%s: %q %v", volume, got, err)
		}
	}
	for _, volume := range []string{"ghost", "/data", "uploads:/data"} {
		if _, err := resolveVolume(context.Background(), g.ctrl, m, manifestBackup(m, "uploads", volume)); err == nil {
			t.Errorf("%q resolved", volume)
		}
	}
}

// manifestBackup returns the backup `name` of m with its volume replaced.
func manifestBackup(m manifest.Manifest, name, volume string) manifest.Backup {
	b := m.Backups[name]
	b.Volume = volume
	return b
}

func TestBackupRoutesNeedTheScopesOfTheTokenTable(t *testing.T) {
	g := newBackupRig(t)
	for _, tc := range []struct {
		name, method, path, token, body string
		want                            int
	}{
		{"list needs a token", "GET", "/v1/projects/demo/environments/lab/backups", "", "", 401},
		{"read lists", "GET", "/v1/projects/demo/environments/lab/backups", readToken, "", 200},
		{"read reads runs", "GET", "/v1/projects/demo/environments/lab/backups/uploads/runs", readToken, "", 200},
		{"read cannot run", "POST", "/v1/projects/demo/environments/lab/backups/uploads/run", readToken, "", 403},
		{"run needs POST", "GET", "/v1/projects/demo/environments/lab/backups/uploads/run", deployToken, "", 405},
		{"list needs GET", "POST", "/v1/projects/demo/environments/lab/backups", deployToken, "", 405},
		{"read cannot restore", "POST", "/v1/projects/demo/environments/lab/restore", readToken, `{"key":"k","into":"v"}`, 403},
		{"deploy cannot restore", "POST", "/v1/projects/demo/environments/lab/restore", deployToken, `{"key":"k","into":"v"}`, 403},
		{"restore needs POST", "GET", "/v1/projects/demo/environments/lab/restore", adminToken, "", 405},
		{"unknown backup runs", "GET", "/v1/projects/demo/environments/lab/backups/nope/runs", readToken, "", 404},
		{"unknown backup run", "POST", "/v1/projects/demo/environments/lab/backups/nope/run", deployToken, "", 404},
		{"bad limit", "GET", "/v1/projects/demo/environments/lab/backups/uploads/runs?limit=0", readToken, "", 400},
	} {
		if w := g.call(tc.method, tc.path, tc.token, tc.body); w.Code != tc.want {
			t.Errorf("%s: %d %s", tc.name, w.Code, w.Body.String())
		}
	}
	if runs, _ := g.store.BackupRuns(context.Background(), "", 10); len(runs) != 0 {
		t.Fatalf("a refused request started a run: %+v", runs)
	}
}

func TestBackupRunRefusalsRecordNothing(t *testing.T) {
	g := newBackupRig(t)
	w := g.call("POST", "/v1/projects/demo/environments/lab/backups/lost/run", deployToken, "")
	if w.Code != 400 || !strings.Contains(w.Body.String(), "backup lost: volume ghost is not mounted by app api") {
		t.Fatalf("unresolved volume: %d %s", w.Code, w.Body.String())
	}
	// A backup of an app that is not deployed is not part of the configuration.
	if w = g.call("POST", "/v1/projects/demo/environments/lab/backups/old/run", deployToken, ""); w.Code != 404 {
		t.Fatalf("backup of an app that was removed: %d %s", w.Code, w.Body.String())
	}
	if err := g.store.DeleteSecret(context.Background(), state.Actor{ID: "ops", Name: "ops"}, "S3_SECRET_KEY"); err != nil {
		t.Fatal(err)
	}
	w = g.call("POST", "/v1/projects/demo/environments/lab/backups/uploads/run", deployToken, "")
	if w.Code != 400 || !strings.Contains(w.Body.String(), "backup credentials unavailable") {
		t.Fatalf("missing credentials: %d %s", w.Code, w.Body.String())
	}
	if runs, _ := g.store.BackupRuns(context.Background(), "", 10); len(runs) != 0 {
		t.Fatalf("refusals were recorded: %+v", runs)
	}
	unavailable := NewDeploymentHandlerWithBackups(backupConfig(), nil, nil, g.ctrl, nil, nil, nil)
	r := httptest.NewRequest("GET", "/v1/projects/demo/environments/lab/backups", nil)
	r.Header.Set("Authorization", "Bearer "+backupToken(readToken))
	rec := httptest.NewRecorder()
	unavailable.ServeHTTP(rec, r)
	if rec.Code != 503 {
		t.Fatalf("without backups: %d", rec.Code)
	}
}

func TestASecondBackupOfTheSameVolumeIsAConflict(t *testing.T) {
	g := newBackupRig(t)
	g.docker.block = make(chan struct{})
	w := g.call("POST", "/v1/projects/demo/environments/lab/backups/uploads/run", deployToken, "")
	var accepted struct {
		ID int64 `json:"backup_run_id"`
	}
	if w.Code != 202 || json.Unmarshal(w.Body.Bytes(), &accepted) != nil {
		t.Fatalf("first run: %d %s", w.Code, w.Body.String())
	}
	if w = g.call("POST", "/v1/projects/demo/environments/lab/backups/uploads/run", deployToken, ""); w.Code != 409 || !strings.Contains(w.Body.String(), "already running") {
		t.Fatalf("second run: %d %s", w.Code, w.Body.String())
	}
	runs, _ := g.store.BackupRuns(context.Background(), "uploads", 10)
	if len(runs) != 1 || runs[0].Status != "running" {
		t.Fatalf("runs while one runs: %+v", runs)
	}
	close(g.docker.block)
	if run := g.wait("uploads", accepted.ID); run.Status != "succeeded" {
		t.Fatalf("first run: %+v", run)
	}
}

func TestStoppingTheAgentRecordsTheRunAsStopped(t *testing.T) {
	g := newBackupRig(t)
	g.docker.block = make(chan struct{})
	w := g.call("POST", "/v1/projects/demo/environments/lab/backups/uploads/run", deployToken, "")
	var accepted struct {
		ID int64 `json:"backup_run_id"`
	}
	if w.Code != 202 || json.Unmarshal(w.Body.Bytes(), &accepted) != nil {
		t.Fatalf("run: %d %s", w.Code, w.Body.String())
	}
	time.Sleep(50 * time.Millisecond)
	g.cancel()
	close(g.docker.block)
	g.backups.Wait()
	runs, _ := g.store.BackupRuns(context.Background(), "uploads", 10)
	if len(runs) != 1 || runs[0].Status != "failed" || runs[0].Detail != "agent stopped during the backup" {
		t.Fatalf("run after the stop: %+v", runs)
	}
}

func TestStartFailsRunsAPreviousProcessLeftRunning(t *testing.T) {
	g := newBackupRig(t)
	ctx := context.Background()
	id, err := g.store.StartBackup(ctx, state.BackupRun{Backup: "uploads", App: "api", Volume: "uploads", Destination: "offsite", ObjectKey: "api/uploads/uploads/crashed.tar.gz", Trigger: "manual", Started: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	g.backups.Start(ctx)
	runs, _ := g.store.BackupRuns(ctx, "uploads", 10)
	if len(runs) != 1 || runs[0].ID != id || runs[0].Status != "failed" || runs[0].Detail != "agent restarted during the backup" {
		t.Fatalf("abandoned run: %+v", runs)
	}
}

func TestRestoreThroughTheAPIFillsANewVolume(t *testing.T) {
	g := newBackupRig(t)
	run := g.run("uploads")
	body := fmt.Sprintf(`{"key":%q,"into":"restored-1"}`, run.Key)
	// A dry run shows the plan and changes nothing.
	w := g.call("POST", "/v1/projects/demo/environments/lab/restore", adminToken, strings.TrimSuffix(body, "}")+`,"dry_run":true}`)
	var plan struct {
		Restore deploy.RestorePlan `json:"restore"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &plan) != nil || plan.Restore.Backup != "uploads" || plan.Restore.Into != "restored-1" || plan.Restore.SHA256 != run.SHA256 || plan.Restore.Size != run.Size {
		t.Fatalf("dry run: %d %s", w.Code, w.Body.String())
	}
	if g.docker.has("restored-1") {
		t.Fatal("a dry run created the volume")
	}
	w = g.call("POST", "/v1/projects/demo/environments/lab/restore", adminToken, body)
	var accepted struct {
		ID     string `json:"operation_id"`
		Events string `json:"events_url"`
	}
	if w.Code != 202 || json.Unmarshal(w.Body.Bytes(), &accepted) != nil || accepted.ID == "" || accepted.Events != "/v1/projects/demo/environments/lab/operations/"+accepted.ID+"/events" {
		t.Fatalf("restore: %d %s", w.Code, w.Body.String())
	}
	op := g.waitOperation(accepted.ID)
	if op.State != "succeeded" || op.Kind != "restore" {
		t.Fatalf("operation: %+v", op)
	}
	if got := g.docker.fed["restored-1"]; got != "uploads-payload" || !g.docker.has("restored-1") {
		t.Fatalf("restored volume: fed %q", got)
	}
	// An existing volume is refused when the request arrives.
	w = g.call("POST", "/v1/projects/demo/environments/lab/restore", adminToken, fmt.Sprintf(`{"key":%q,"into":"uploads"}`, run.Key))
	if w.Code != 400 || !strings.Contains(w.Body.String(), "target volume already exists") {
		t.Fatalf("existing volume: %d %s", w.Code, w.Body.String())
	}
	w = g.call("POST", "/v1/projects/demo/environments/lab/restore", adminToken, `{"key":"api/uploads/uploads/20200101T000000Z-1.tar.gz","into":"restored-2"}`)
	if w.Code != 400 || !strings.Contains(w.Body.String(), "backup not found") {
		t.Fatalf("unknown key: %d %s", w.Code, w.Body.String())
	}
}

func TestRestoreOfATamperedArchiveFailsAndRemovesTheVolume(t *testing.T) {
	g := newBackupRig(t)
	run := g.run("uploads")
	if _, err := g.s3.Put(context.Background(), run.Key, strings.NewReader("not the archive that was backed up"), nil); err != nil {
		t.Fatal(err)
	}
	w := g.call("POST", "/v1/projects/demo/environments/lab/restore", adminToken, fmt.Sprintf(`{"key":%q,"into":"restored-1"}`, run.Key))
	var accepted struct {
		ID string `json:"operation_id"`
	}
	if w.Code != 202 || json.Unmarshal(w.Body.Bytes(), &accepted) != nil {
		t.Fatalf("restore: %d %s", w.Code, w.Body.String())
	}
	op := g.waitOperation(accepted.ID)
	if op.State != "failed" || safeOperationError(op) != "backup checksum mismatch" {
		t.Fatalf("operation: %+v (%q)", op, safeOperationError(op))
	}
	if g.docker.has("restored-1") {
		t.Fatal("the new volume was kept after the checksum mismatch")
	}
}

func TestDeployedManifestDropsBackupsOfAppsThatAreNotDeployed(t *testing.T) {
	g := newBackupRig(t)
	m, err := runningManifest(context.Background(), g.store)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := m.Backups["old"]; ok || len(m.Backups) != 3 || len(m.Destinations) != 1 {
		t.Fatalf("backups %v destinations %v", m.Backups, m.Destinations)
	}
}

func TestBackupsNeedStateAndDocker(t *testing.T) {
	if b := NewBackups(Config{Project: "demo", Environment: "lab"}, nil, nil, nil, nil, nil); b != nil {
		t.Fatal("backups without Docker or state")
	}
	if b := NewBackups(Config{Project: "demo", Environment: "lab"}, engine.New("/nonexistent.sock"), &deploy.Controller{}, nil, nil, nil); b != nil {
		t.Fatal("backups without state")
	}
	var none *Backups
	none.Start(context.Background())
	none.Wait()
}

// removeApps ends the deployed state of the named apps, as berthy remove does: no applied release and no bindings remain, and
// the release history stays.
func (g *backupRig) removeApps(apps ...string) {
	g.t.Helper()
	ctx := context.Background()
	op, _, err := g.store.Admit(ctx, state.Admission{RequestID: "remove-all", ActorID: "ops", ActorName: "ops", Kind: "remove", PublicRequest: []byte(`{}`)})
	if err != nil {
		g.t.Fatal(err)
	}
	for _, app := range apps {
		if err = g.store.ForgetApp(ctx, op.ID, app); err != nil {
			g.t.Fatal(err)
		}
	}
	if err = g.store.Finish(ctx, op.ID, state.Outcome{State: "succeeded"}); err != nil {
		g.t.Fatal(err)
	}
}

func TestListAndRestoreStillWorkAfterTheAppsAreRemoved(t *testing.T) {
	g := newBackupRig(t)
	uploads := g.run("uploads")
	docs := g.run("docs")
	g.removeApps("api", "files")
	if m, err := runningManifest(context.Background(), g.store); err == nil && len(m.Backups) != 0 {
		t.Fatalf("backups of removed apps are still configured: %v", m.Backups)
	}
	// Running a backup needs the app.
	if w := g.call("POST", "/v1/projects/demo/environments/lab/backups/uploads/run", deployToken, ""); w.Code != 404 {
		t.Fatalf("run after the removal: %d %s", w.Code, w.Body.String())
	}
	entries := g.list("")
	if len(entries) != 2 || entries[0].Key != docs.Key && entries[1].Key != docs.Key {
		t.Fatalf("entries after the removal: %+v", entries)
	}
	if one := g.list("?backup=uploads"); len(one) != 1 || one[0].Key != uploads.Key {
		t.Fatalf("filtered entries: %+v", one)
	}
	for _, key := range []string{uploads.Key, docs.Key} {
		w := g.call("POST", "/v1/projects/demo/environments/lab/restore", adminToken, fmt.Sprintf(`{"key":%q,"into":"back-%d"}`, key, len(key)))
		var accepted struct {
			ID string `json:"operation_id"`
		}
		if w.Code != 202 || json.Unmarshal(w.Body.Bytes(), &accepted) != nil {
			t.Fatalf("restore of %s: %d %s", key, w.Code, w.Body.String())
		}
		if op := g.waitOperation(accepted.ID); op.State != "succeeded" {
			t.Fatalf("restore of %s: %+v", key, op)
		}
	}
	if g.docker.fed["back-"+fmt.Sprint(len(uploads.Key))] != "uploads-payload" {
		t.Fatalf("restored volumes: %v", g.docker.fed)
	}
}

func TestStartRemovesTheHelperContainersAKilledAgentLeft(t *testing.T) {
	g := newBackupRig(t)
	g.docker.inventory = []engine.InventoryContainer{
		{ID: "left-archive", State: "running", Labels: map[string]string{"berthy.environment": "lab", "berthy.helper": "archive"}},
		{ID: "left-restore", State: "exited", Labels: map[string]string{"berthy.environment": "lab", "berthy.helper": "restore"}},
		{ID: "other-env", State: "running", Labels: map[string]string{"berthy.environment": "prod", "berthy.helper": "archive"}},
		{ID: "probe", State: "running", Labels: map[string]string{"berthy.environment": "lab", "berthy.helper": "probe"}},
	}
	g.backups.Start(context.Background())
	if len(g.docker.removedC) != 2 || g.docker.removedC[0] != "left-archive" || g.docker.removedC[1] != "left-restore" {
		t.Fatalf("removed containers: %v", g.docker.removedC)
	}
}
