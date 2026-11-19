package cli

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"berthy/internal/manifest"
	"berthy/internal/objectstore"
	"berthy/internal/state"
	"berthy/internal/statebackup"

	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"
)

// stateAgent is a fake agent for `state-backup`: it accepts the run, reports it running for a few polls and then as final.
type stateAgent struct {
	finishAt int
	final    string
	polls    int
	requests []string
}

func (a *stateAgent) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.requests = append(a.requests, r.Method+" "+r.URL.Path)
		switch {
		case r.Method == "POST" && r.URL.Path == "/v1/environments/dev/state-backup":
			w.WriteHeader(202)
			fmt.Fprint(w, `{"state_backup_run_id":7}`)
		case r.Method == "GET" && r.URL.Path == "/v1/environments/dev/state-backup/runs":
			a.polls++
			if a.polls < a.finishAt {
				fmt.Fprint(w, `[{"id":7,"trigger":"manual","started":"2026-11-16T10:00:00Z","status":"running"}]`)
				return
			}
			fmt.Fprint(w, a.final)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
			w.WriteHeader(404)
		}
	})
}

func runStateBackup(t *testing.T, a *stateAgent) (int, string, string) {
	t.Helper()
	old := backupPollInterval
	backupPollInterval = time.Millisecond
	defer func() { backupPollInterval = old }()
	server := httptest.NewServer(a.handler(t))
	defer server.Close()
	var out, errout bytes.Buffer
	code := Run([]string{"state-backup", "-e", "dev", "--config", ingressCLIConfig(t, server.URL)}, &out, &errout)
	return code, out.String(), errout.String()
}

const succeededStateBackup = `[{"id":7,"key":"berthy-state/dev/20261116T100000Z.db","trigger":"manual","started":"2026-11-16T10:00:00Z","finished":"2026-11-16T10:00:02Z","status":"succeeded","size":4096,"sha256":"` + backupSum + `"}]`

func TestStateBackupWaitsForTheSnapshotAndPrintsIt(t *testing.T) {
	a := &stateAgent{finishAt: 3, final: succeededStateBackup}
	code, out, errout := runStateBackup(t, a)
	if code != 0 {
		t.Fatalf("exit %d out %q err %q", code, out, errout)
	}
	for _, want := range []string{"run: 7", "status: succeeded", "key: berthy-state/dev/20261116T100000Z.db", "size: 4096", "sha256: " + backupSum} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if a.polls != 3 {
		t.Fatalf("polls: %d", a.polls)
	}
}

func TestStateBackupExitsNonZeroWhenTheSnapshotFails(t *testing.T) {
	failed := `[{"id":7,"key":"berthy-state/dev/x.db","trigger":"manual","started":"2026-11-16T10:00:00Z","finished":"2026-11-16T10:00:02Z","status":"failed","detail":"upload failed: access denied"}]`
	code, out, _ := runStateBackup(t, &stateAgent{finishAt: 1, final: failed})
	if code == 0 || !strings.Contains(out, "detail: upload failed: access denied") {
		t.Fatalf("exit %d out %q", code, out)
	}
}

// snapshotInBucket takes a state backup of a state with one secret into a fake bucket, with the Runner the agent uses. It
// returns the endpoint, the key of the snapshot and the saved key file.
func snapshotInBucket(t *testing.T) (endpoint, object, keyPath string) {
	t.Helper()
	ctx := context.Background()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	keyPath = filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(keyPath, key, 0600); err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(dir, keyPath, "prod")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if err = store.PutSecret(ctx, state.Actor{ID: "ops", Name: "ops"}, state.SecretInfo{Name: "DB_PASSWORD"}, []byte("cli-secret-value")); err != nil {
		t.Fatal(err)
	}
	backend := s3mem.New()
	if err = backend.CreateBucket("bkt"); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(gofakes3.New(backend).Server())
	t.Cleanup(srv.Close)
	objects, err := objectstore.New(objectstore.Config{Endpoint: srv.URL, Bucket: "bkt", AccessKey: "AKIATESTACCESS", SecretKey: "test-secret-key-0123456789"})
	if err != nil {
		t.Fatal(err)
	}
	m := manifest.Manifest{
		Destinations: map[string]manifest.Destination{"offsite": {Type: "s3", Endpoint: srv.URL, Bucket: "bkt", AccessKeySecret: "S3_ACCESS_KEY", SecretKeySecret: "S3_SECRET_KEY"}},
		StateBackup:  &manifest.StateBackup{Destination: "offsite"},
	}
	r := &statebackup.Runner{Store: store, Env: "prod", Dir: dir,
		Objects: func(context.Context, manifest.Destination) (objectstore.Store, error) { return objects, nil }}
	p, err := r.Begin(ctx, m, "manual", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	res := p.Execute(ctx)
	if res.Run.Status != "succeeded" {
		t.Fatalf("snapshot: %+v", res.Run)
	}
	return srv.URL, res.Run.Key, keyPath
}

// stateRestoreArgs are the arguments of a restore of object from the bucket at endpoint into out.
func stateRestoreArgs(endpoint, object, out, keyPath string) []string {
	return []string{"state-restore", "--endpoint", endpoint, "--bucket", "bkt", "--object", object, "--out", out, "--key-file", keyPath}
}

func TestStateRestoreTakesCredentialsOnlyFromTheEnvironment(t *testing.T) {
	endpoint, object, keyPath := snapshotInBucket(t)
	out := filepath.Join(t.TempDir(), "restored")
	var stdout, stderr bytes.Buffer

	t.Setenv("BERTHY_S3_ACCESS_KEY", "")
	t.Setenv("BERTHY_S3_SECRET_KEY", "")
	if code := Run(stateRestoreArgs(endpoint, object, out, keyPath), &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), "BERTHY_S3_ACCESS_KEY") {
		t.Fatalf("without the environment: exit %d, %q", code, stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	args := append(stateRestoreArgs(endpoint, object, out, keyPath), "--access-key", "AKIATESTACCESS")
	t.Setenv("BERTHY_S3_ACCESS_KEY", "AKIATESTACCESS")
	t.Setenv("BERTHY_S3_SECRET_KEY", "test-secret-key-0123456789")
	if code := Run(args, &stdout, &stderr); code != 2 {
		t.Fatalf("a credential flag was accepted: exit %d", code)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatal("a refused command wrote the output directory")
	}
}

func TestStateRestoreRebuildsTheStateAndPrintsWhatItHolds(t *testing.T) {
	endpoint, object, keyPath := snapshotInBucket(t)
	out := filepath.Join(t.TempDir(), "restored")
	t.Setenv("BERTHY_S3_ACCESS_KEY", "AKIATESTACCESS")
	t.Setenv("BERTHY_S3_SECRET_KEY", "test-secret-key-0123456789")
	var stdout, stderr bytes.Buffer
	if code := Run(stateRestoreArgs(endpoint, object, out, keyPath), &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d, err %q", code, stderr.String())
	}
	for _, want := range []string{"environment: prod", "schema: 18", "secrets: 1", "DB_PASSWORD"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, stdout.String())
		}
	}
	for _, secret := range []string{"cli-secret-value", "test-secret-key-0123456789", "AKIATESTACCESS"} {
		if strings.Contains(stdout.String()+stderr.String(), secret) {
			t.Fatalf("output shows %q", secret)
		}
	}
}

func TestStateRestoreWithAWrongKeyExitsNonZero(t *testing.T) {
	endpoint, object, saved := snapshotInBucket(t)
	need, err := state.KeyIDOfFile(saved)
	if err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(other, bytes.Repeat([]byte{3}, 32), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BERTHY_S3_ACCESS_KEY", "AKIATESTACCESS")
	t.Setenv("BERTHY_S3_SECRET_KEY", "test-secret-key-0123456789")
	var stdout, stderr bytes.Buffer
	code := Run(stateRestoreArgs(endpoint, object, filepath.Join(t.TempDir(), "restored"), other), &stdout, &stderr)
	have, err := state.KeyIDOfFile(other)
	if err != nil {
		t.Fatal(err)
	}
	if want := "the snapshot needs key " + need + "; " + other + " is key " + have; code == 0 || strings.TrimSpace(stderr.String()) != want {
		t.Fatalf("wrong key: exit %d, %q, want %q", code, stderr.String(), want)
	}
}
