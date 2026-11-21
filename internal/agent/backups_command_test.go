package agent

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"berthy/internal/api"
	"berthy/internal/deploy"
	"berthy/internal/engine"
)

// commandEngine is the Docker side of a Compose service's exec: it plays the dump and the restore commands.
type commandEngine struct {
	deploy.Engine
	mu     sync.Mutex
	dump   string // what the command writes to its standard output
	exit   int
	stderr string
	fed    string   // what a restore command was given
	ran    []string // the commands run, joined
}

func (e *commandEngine) ComposeContainers(context.Context, string) ([]engine.ComposeContainer, error) {
	return []engine.ComposeContainer{{ID: "db-1", Service: "db", State: "running", Number: 1}}, nil
}

func (e *commandEngine) ExecStream(_ context.Context, _ string, command []string, stdin io.Reader, stdout io.Writer, _ int) (int, string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.ran = append(e.ran, strings.Join(command, " "))
	if stdin != nil {
		b, _ := io.ReadAll(stdin)
		e.fed = string(b)
	}
	if stdout != nil {
		io.WriteString(stdout, e.dump)
	}
	return e.exit, e.stderr, nil
}

func commandRig(t *testing.T) (*backupRig, *commandEngine) {
	t.Helper()
	g := newBackupRig(t)
	e := &commandEngine{dump: "PGDMP the dump"}
	g.ctrl.Engine = e
	withDump := strings.Replace(backupManifest(g.url), `"backups":{`, `"backups":{"dump":{"app":"files","service":"db","command":["pg_dump","-Fc"],"destination":"offsite"},`, 1)
	recordAppRelease(t, g.store, "r-files-2", "files", withDump)
	return g, e
}

func TestACommandBackupRunsListsAndRestoresIntoACommandThroughTheAPI(t *testing.T) {
	g, e := commandRig(t)
	run := g.run("dump")
	if run.Status != "succeeded" || run.Volume != "" || run.Size == 0 || len(run.SHA256) != 64 || !strings.HasPrefix(run.Key, "files/command/dump/") || !strings.HasSuffix(run.Key, fmt.Sprintf("-%d.gz", run.ID)) {
		t.Fatalf("run: %+v", run)
	}
	if len(e.ran) != 1 || e.ran[0] != "pg_dump -Fc" {
		t.Fatalf("commands run: %v", e.ran)
	}
	entries := g.list("?backup=dump")
	if len(entries) != 1 || entries[0].Key != run.Key || entries[0].Volume != "" || entries[0].Format != "berthy-command-gzip-1" || entries[0].SHA256 != run.SHA256 {
		t.Fatalf("entries: %+v", entries)
	}
	// What is stored is the gzip of the dump, with nothing staged by the agent.
	body, _, err := g.s3.Get(context.Background(), run.Key)
	if err != nil {
		t.Fatal(err)
	}
	stored, _ := io.ReadAll(body)
	body.Close()
	zr, err := gzip.NewReader(bytes.NewReader(stored))
	if err != nil {
		t.Fatal(err)
	}
	if text, _ := io.ReadAll(zr); string(text) != "PGDMP the dump" {
		t.Fatalf("stored %q", text)
	}

	// A restore into a volume refuses it, and a restore into a command takes it.
	w := g.call("POST", "/v1/projects/demo/environments/lab/restore", adminToken, fmt.Sprintf(`{"key":%q,"into":"restored-1"}`, run.Key))
	if w.Code != 400 || !strings.Contains(w.Body.String(), "command backup") {
		t.Fatalf("restore into a volume: %d %s", w.Code, w.Body.String())
	}
	body2 := fmt.Sprintf(`{"key":%q,"app":"files","service":"db","command":["pg_restore","-d","scratch"],"dry_run":true}`, run.Key)
	w = g.call("POST", "/v1/projects/demo/environments/lab/restore", adminToken, body2)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"target":"files service db"`) || !strings.Contains(w.Body.String(), `"command":["pg_restore","-d","scratch"]`) {
		t.Fatalf("dry run: %d %s", w.Code, w.Body.String())
	}
	if len(e.ran) != 1 {
		t.Fatalf("a dry run ran a command: %v", e.ran)
	}
	if w = g.call("POST", "/v1/projects/demo/environments/lab/restore", deployToken, strings.Replace(body2, `"dry_run":true`, `"dry_run":false`, 1)); w.Code != 403 {
		t.Fatalf("restore with a deploy token: %d %s", w.Code, w.Body.String())
	}
	w = g.call("POST", "/v1/projects/demo/environments/lab/restore", adminToken, strings.Replace(body2, `"dry_run":true`, `"dry_run":false`, 1))
	var accepted struct {
		ID string `json:"operation_id"`
	}
	if w.Code != 202 || json.Unmarshal(w.Body.Bytes(), &accepted) != nil {
		t.Fatalf("restore: %d %s", w.Code, w.Body.String())
	}
	if op := g.waitOperation(accepted.ID); op.State != "succeeded" || op.Kind != "restore" {
		t.Fatalf("operation: %+v", op)
	}
	if e.fed != "PGDMP the dump" || e.ran[len(e.ran)-1] != "pg_restore -d scratch" {
		t.Fatalf("the restore command was given %q (ran %v)", e.fed, e.ran)
	}
	// A volume backup is refused by restore --exec.
	vol := g.run("uploads")
	w = g.call("POST", "/v1/projects/demo/environments/lab/restore", adminToken, fmt.Sprintf(`{"key":%q,"app":"files","service":"db","command":["pg_restore"]}`, vol.Key))
	if w.Code != 400 || !strings.Contains(w.Body.String(), "volume backup") {
		t.Fatalf("restore --exec of a volume backup: %d %s", w.Code, w.Body.String())
	}
}

func TestACommandThatFailsLeavesNoBackupAndItsCheckShowsTheError(t *testing.T) {
	g, e := commandRig(t)
	e.exit, e.stderr = 3, "pg_dump: error: server closed the connection\n"
	run := g.run("dump")
	want := "command failed: exited with code 3: pg_dump: error: server closed the connection"
	if run.Status != "failed" || run.Detail != want || run.SHA256 != "" {
		t.Fatalf("run: %+v", run)
	}
	if entries := g.list("?backup=dump"); len(entries) != 0 {
		t.Fatalf("a failed command left a listed backup: %+v", entries)
	}
	objects, err := g.s3.List(context.Background(), "files/command/dump/")
	if err != nil || len(objects) != 0 {
		t.Fatalf("a failed command left objects: %+v %v", objects, err)
	}
	m, _ := runningManifest(context.Background(), g.store)
	statuses, err := backupStatuses(context.Background(), g.store, m, "files", time.Now())
	var got *api.BackupStatus
	for i := range statuses {
		if statuses[i].Name == "dump" {
			got = &statuses[i]
		}
	}
	if err != nil || got == nil || got.Volume != "" || got.LastStatus != "failed" || got.Detail != want {
		t.Fatalf("status: %+v %v", got, err)
	}
	// The same command with empty output is a failure too.
	e.exit, e.stderr, e.dump = 0, "", ""
	if run = g.run("dump"); run.Status != "failed" || run.Detail != "command failed: wrote no output" {
		t.Fatalf("empty output: %+v", run)
	}
}
