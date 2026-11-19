package deploy

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"berthy/internal/engine"
	"berthy/internal/jobs"
	"berthy/internal/manifest"
)

var dumpJob = manifest.Job{App: "web", Role: "web", Command: []string{"sh", "-c", "dump"}, Timeout: "1m"}

func TestRunCommandStreamsTheOutputOfAnImageAppFromAContainerOfItsOwn(t *testing.T) {
	c, f := jobFixture(t)
	f.streamOut = "the dump"
	var out bytes.Buffer
	res, err := c.RunCommand(context.Background(), imageJobManifest(), "backup", "main-db", "21", dumpJob, jobs.Streams{Stdout: &out})
	if err != nil || res.Exit == nil || *res.Exit != 0 || res.Output != "" || res.Container != "ctr-berthy-backup-test-21" || out.String() != "the dump" {
		t.Fatalf("outcome %+v %v, stdout %q", res, err, out.String())
	}
	// The container is attached before it starts, so its first words are not lost, and it always goes.
	if got := f.callList(); got != "create,attach,start,wait,remove" {
		t.Fatalf("calls %s", got)
	}
	var spec struct {
		Entrypoint []string
		Labels     map[string]string
		OpenStdin  bool
		StdinOnce  bool
		Tty        bool
		HostConfig struct{ LogConfig struct{ Type string } }
	}
	if err = json.Unmarshal(f.specs[0], &spec); err != nil {
		t.Fatal(err)
	}
	if !spec.OpenStdin || !spec.StdinOnce || spec.Tty || spec.Entrypoint[0] != "sh" {
		t.Errorf("stdin and terminal: %s", f.specs[0])
	}
	// What the command writes is the product, so Docker must not keep a copy of it in a log on the server's disk.
	if spec.HostConfig.LogConfig.Type != "none" {
		t.Errorf("the container logs its output: %s", f.specs[0])
	}
	if spec.Labels["berthy.environment"] != "test" || spec.Labels["berthy.backup"] != "main-db" || spec.Labels["berthy.oneoff"] != "backup" {
		t.Errorf("labels %v", spec.Labels)
	}
}

func TestRunCommandOfAnImageAppKeepsTheRedactedTailOfAFailedStreamedRun(t *testing.T) {
	c, f := jobFixture(t)
	f.exit = 3
	f.execTail = "pg_dump: error: boom token=s3cret\n"
	res, err := c.RunCommand(context.Background(), imageJobManifest(), "backup", "main-db", "22", dumpJob, jobs.Streams{Stdout: &bytes.Buffer{}})
	if err != nil || res.Exit == nil || *res.Exit != 3 || res.Output != "pg_dump: error: boom token=[redacted]\n" {
		t.Fatalf("outcome %+v %v", res, err)
	}
	if got := f.callList(); got != "create,attach,start,wait,remove" {
		t.Fatalf("calls %s", got)
	}
}

func TestRunCommandOfAnImageAppStopsTheContainerOfAStreamedRunThatTimedOut(t *testing.T) {
	c, f := jobFixture(t)
	f.block = true
	f.execTail = "still dumping token=s3cret\n"
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	res, err := c.RunCommand(ctx, imageJobManifest(), "backup", "main-db", "23", dumpJob, jobs.Streams{Stdout: &bytes.Buffer{}})
	if err != nil || !res.TimedOut || res.Exit != nil || res.Output != "still dumping token=[redacted]\n" {
		t.Fatalf("outcome %+v %v", res, err)
	}
	if got := f.callList(); got != "create,attach,start,stop,remove" {
		t.Fatalf("calls %s", got)
	}
}

func TestRunCommandGivesAnImageAppsCommandItsInput(t *testing.T) {
	c, f := jobFixture(t)
	res, err := c.RunCommand(context.Background(), imageJobManifest(), "restore", "op-1", "op-1", dumpJob, jobs.Streams{Stdin: strings.NewReader("the dump")})
	if err != nil || res.Exit == nil || *res.Exit != 0 || string(f.streamIn) != "the dump" {
		t.Fatalf("outcome %+v %v, input %q", res, err, f.streamIn)
	}
}

func TestRunCommandStreamsTheOutputOfAComposeAppFromItsServiceContainer(t *testing.T) {
	c, f := jobFixture(t)
	f.composeOf = []engine.ComposeContainer{{ID: "pg-1", Service: "postgres", State: "running", Number: 1}}
	f.streamOut = "the dump"
	var out bytes.Buffer
	job := manifest.Job{App: "db", Service: "postgres", Command: []string{"pg_dump"}, Timeout: "1m"}
	res, err := c.RunCommand(context.Background(), composeJobManifest(), "backup", "main-db", "24", job, jobs.Streams{Stdout: &out})
	if err != nil || res.Exit == nil || *res.Exit != 0 || res.Container != "pg-1" || out.String() != "the dump" || f.callList() != "exec" {
		t.Fatalf("outcome %+v %v, stdout %q, calls %s", res, err, out.String(), f.callList())
	}
	f.execExit, f.execTail = 1, "pg_dump: error: boom s3cret\n"
	res, err = c.RunCommand(context.Background(), composeJobManifest(), "backup", "main-db", "25", job, jobs.Streams{Stdout: &out})
	if err != nil || res.Exit == nil || *res.Exit != 1 || res.Output != "pg_dump: error: boom [redacted]\n" {
		t.Fatalf("failed outcome %+v %v", res, err)
	}
}

func TestRunCommandGivesAComposeAppsCommandItsInput(t *testing.T) {
	c, f := jobFixture(t)
	f.composeOf = []engine.ComposeContainer{{ID: "pg-1", Service: "postgres", State: "running", Number: 1}}
	job := manifest.Job{App: "db", Service: "postgres", Command: []string{"pg_restore"}, Timeout: "1m"}
	res, err := c.RunCommand(context.Background(), composeJobManifest(), "restore", "op-2", "op-2", job, jobs.Streams{Stdin: strings.NewReader("the dump")})
	if err != nil || res.Exit == nil || *res.Exit != 0 || string(f.streamIn) != "the dump" {
		t.Fatalf("outcome %+v %v, input %q", res, err, f.streamIn)
	}
}
