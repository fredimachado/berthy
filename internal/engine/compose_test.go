package engine

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestComposeHelperProcess is the fake Compose CLI the runner tests execute. It runs only when the sentinel argument is
// present, and reports the arguments and environment it received as JSON.
func TestComposeHelperProcess(t *testing.T) {
	at := -1
	for i, a := range os.Args {
		if a == "helper-sentinel" {
			at = i
			break
		}
	}
	if at < 0 {
		return
	}
	rest := os.Args[at+1:]
	switch {
	case len(rest) > 0 && rest[len(rest)-1] == "version":
		os.Stdout.WriteString("Docker Compose version v9.9.9\nsecond line\n")
	case contains(rest, "fail"):
		os.Stderr.WriteString("password=SECRET-VALUE")
		os.Exit(3)
	case contains(rest, "flood"):
		os.Stdout.Write([]byte(strings.Repeat("a", 2<<20)))
	default:
		json.NewEncoder(os.Stdout).Encode(struct {
			Args []string `json:"args"`
			Env  []string `json:"env"`
		}{rest, os.Environ()})
	}
	os.Exit(0)
}

func contains(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}

// helperCompose runs this test binary as the Compose CLI.
func helperCompose() Compose {
	return Compose{Command: []string{os.Args[0], "-test.run=^TestComposeHelperProcess$", "--", "helper-sentinel"}, Host: "unix:///run/docker.sock"}
}

func TestComposeRunArgumentsAndMinimalEnvironment(t *testing.T) {
	t.Setenv("BERTHY_TEST_LEAK", "must-not-pass")
	dir := t.TempDir()
	auth := filepath.Join(dir, "auth")
	out, _, err := helperCompose().Run(context.Background(), ComposeRun{Project: "shop-db", Dir: dir, Args: []string{"config", "--format", "json"}, DockerConfig: auth})
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Args []string `json:"args"`
		Env  []string `json:"env"`
	}
	if err = json.Unmarshal(out, &got); err != nil {
		t.Fatalf("helper output %q: %v", out, err)
	}
	want := []string{"-p", "shop-db", "--project-directory", dir, "-f", filepath.Join(dir, "compose.yaml"), "--env-file", filepath.Join(dir, ".env"), "config", "--format", "json"}
	if strings.Join(got.Args, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("args %q", got.Args)
	}
	for _, entry := range []string{"HOME=" + dir, "DOCKER_HOST=unix:///run/docker.sock", "COMPOSE_STATUS_STDOUT=1", "DOCKER_CONFIG=" + auth} {
		if !contains(got.Env, entry) {
			t.Fatalf("environment lacks %s: %q", entry, got.Env)
		}
	}
	for _, entry := range got.Env {
		if strings.HasPrefix(entry, "BERTHY_TEST_LEAK=") {
			t.Fatal("parent environment leaked into the Compose command")
		}
	}
}

func TestComposeRunOmitsDockerConfigWhenNotGiven(t *testing.T) {
	out, _, err := helperCompose().Run(context.Background(), ComposeRun{Project: "shop-db", Dir: t.TempDir(), Args: []string{"restart"}})
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Env []string `json:"env"`
	}
	if json.Unmarshal(out, &got) != nil {
		t.Fatalf("helper output %q", out)
	}
	for _, entry := range got.Env {
		if strings.HasPrefix(entry, "DOCKER_CONFIG=") {
			t.Fatalf("DOCKER_CONFIG set without auths: %s", entry)
		}
	}
}

func TestComposeRunErrorNeverCarriesOutput(t *testing.T) {
	_, stderr, err := helperCompose().Run(context.Background(), ComposeRun{Project: "shop-db", Dir: t.TempDir(), Args: []string{"fail"}})
	if err == nil || err.Error() != "compose exited with status 3" {
		t.Fatalf("error %v", err)
	}
	if strings.Contains(err.Error(), "SECRET-VALUE") {
		t.Fatal("error carries command output")
	}
	if !strings.Contains(string(stderr), "SECRET-VALUE") {
		t.Fatal("stderr not captured for the caller")
	}
}

func TestComposeRunCapsEachStream(t *testing.T) {
	out, _, err := helperCompose().Run(context.Background(), ComposeRun{Project: "shop-db", Dir: t.TempDir(), Args: []string{"flood"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != maxComposeOutput {
		t.Fatalf("stdout kept %d bytes, want %d", len(out), maxComposeOutput)
	}
}

func TestComposeRunRejectsInvalidCommands(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		name string
		c    Compose
		r    ComposeRun
	}{
		{"no command", Compose{Host: "unix:///x"}, ComposeRun{Project: "shop-db", Dir: dir, Args: []string{"up"}}},
		{"no host", Compose{Command: []string{"docker", "compose"}}, ComposeRun{Project: "shop-db", Dir: dir, Args: []string{"up"}}},
		{"bad project", Compose{Command: []string{"docker", "compose"}, Host: "unix:///x"}, ComposeRun{Project: "Shop DB", Dir: dir, Args: []string{"up"}}},
		{"no directory", Compose{Command: []string{"docker", "compose"}, Host: "unix:///x"}, ComposeRun{Project: "shop-db", Args: []string{"up"}}},
		{"no arguments", Compose{Command: []string{"docker", "compose"}, Host: "unix:///x"}, ComposeRun{Project: "shop-db", Dir: dir}},
	} {
		if _, _, err := tc.c.Run(context.Background(), tc.r); err == nil {
			t.Errorf("%s accepted", tc.name)
		}
	}
}

func TestComposeVersionIsFirstLine(t *testing.T) {
	got, err := helperCompose().Version(context.Background(), t.TempDir())
	if err != nil || got != "Docker Compose version v9.9.9" {
		t.Fatalf("version %q: %v", got, err)
	}
	if _, err = (Compose{Command: []string{"/nonexistent/compose"}, Host: "unix:///x"}).Version(context.Background(), t.TempDir()); err == nil {
		t.Fatal("missing command reported as available")
	}
}

func TestComposeContainersQueryTheProjectLabel(t *testing.T) {
	e := fakeEngine(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/v1.44/containers/json" || r.URL.Query().Get("all") != "1" || !strings.Contains(r.URL.Query().Get("filters"), "com.docker.compose.project=shop-db") {
			t.Errorf("request %s %s", r.URL.Path, r.URL.RawQuery)
		}
		return response(200, `[{"Id":"abc123","Labels":{"com.docker.compose.service":"db","com.docker.compose.config-hash":"hash-1","com.docker.compose.oneoff":"False"}},{"Id":"def456","Labels":{"com.docker.compose.service":"db","com.docker.compose.oneoff":"True"}}]`), nil
	})
	got, err := e.ComposeContainers(context.Background(), "shop-db")
	if err != nil || len(got) != 2 {
		t.Fatalf("containers %+v: %v", got, err)
	}
	if got[0].ID != "abc123" || got[0].Service != "db" || got[0].ConfigHash != "hash-1" || got[0].OneOff {
		t.Fatalf("first container %+v", got[0])
	}
	if !got[1].OneOff {
		t.Fatalf("one-off container not marked: %+v", got[1])
	}
	if _, err = e.ComposeContainers(context.Background(), "Shop DB"); err == nil {
		t.Fatal("invalid project accepted")
	}
}

func TestVolumeExistsTreatsNotFoundAsAbsent(t *testing.T) {
	e := fakeEngine(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/v1.44/volumes/shop-db_data" {
			return response(200, `{"Name":"shop-db_data"}`), nil
		}
		if r.URL.Path == "/v1.44/volumes/shop-db_new" {
			return response(404, `{"message":"no such volume"}`), nil
		}
		return response(500, `{}`), nil
	})
	if ok, err := e.VolumeExists(context.Background(), "shop-db_data"); err != nil || !ok {
		t.Fatalf("existing volume: %v %v", ok, err)
	}
	if ok, err := e.VolumeExists(context.Background(), "shop-db_new"); err != nil || ok {
		t.Fatalf("missing volume: %v %v", ok, err)
	}
	if _, err := e.VolumeExists(context.Background(), "shop-db_broken"); err == nil {
		t.Fatal("server error reported as absent")
	}
}

func TestComposeContainersReportStateAndNumber(t *testing.T) {
	e := fakeEngine(func(r *http.Request) (*http.Response, error) {
		return response(200, `[{"Id":"abc123","State":"running","Labels":{"com.docker.compose.service":"db","com.docker.compose.container-number":"2"}},{"Id":"def456","State":"exited","Labels":{"com.docker.compose.service":"db"}}]`), nil
	})
	got, err := e.ComposeContainers(context.Background(), "shop-db")
	if err != nil || len(got) != 2 {
		t.Fatalf("containers %+v: %v", got, err)
	}
	if got[0].State != "running" || got[0].Number != 2 || got[1].State != "exited" || got[1].Number != 0 {
		t.Fatalf("state and number: %+v", got)
	}
}

func TestNodeIDIsThisNodesSwarmID(t *testing.T) {
	e := fakeEngine(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/v1.44/info" {
			t.Errorf("request %s", r.URL.Path)
		}
		return response(200, `{"Swarm":{"NodeID":"node-1","LocalNodeState":"active"}}`), nil
	})
	if id, err := e.NodeID(context.Background()); err != nil || id != "node-1" {
		t.Fatalf("node id %q: %v", id, err)
	}
	inactive := fakeEngine(func(r *http.Request) (*http.Response, error) {
		return response(200, `{"Swarm":{"NodeID":"","LocalNodeState":"inactive"}}`), nil
	})
	if _, err := inactive.NodeID(context.Background()); err == nil {
		t.Fatal("node id of a node outside a swarm")
	}
}

func TestComposeRunWithoutFilesNamesOnlyTheProject(t *testing.T) {
	dir := t.TempDir()
	out, _, err := helperCompose().Run(context.Background(), ComposeRun{Project: "shop-db", Dir: dir, NoFiles: true, Args: []string{"down", "--remove-orphans"}})
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Args []string `json:"args"`
		Env  []string `json:"env"`
	}
	if err = json.Unmarshal(out, &got); err != nil {
		t.Fatalf("helper output %q: %v", out, err)
	}
	want := []string{"-p", "shop-db", "down", "--remove-orphans"}
	if strings.Join(got.Args, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("a project without files must not be told to read any: %q", got.Args)
	}
	if !contains(got.Env, "HOME="+dir) {
		t.Fatalf("environment lacks the private home: %q", got.Env)
	}
}

func TestProjectVolumesQueryTheProjectLabel(t *testing.T) {
	e := fakeEngine(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/v1.44/volumes" || !strings.Contains(r.URL.Query().Get("filters"), "com.docker.compose.project=shop-db") {
			t.Errorf("request %s %s", r.URL.Path, r.URL.RawQuery)
		}
		return response(200, `{"Volumes":[{"Name":"shop-db_data"},{"Name":"shop-db_cache"}],"Warnings":null}`), nil
	})
	got, err := e.ProjectVolumes(context.Background(), "shop-db")
	if err != nil || len(got) != 2 || got[0] != "shop-db_cache" || got[1] != "shop-db_data" {
		t.Fatalf("volumes %v: %v", got, err)
	}
	if _, err = e.ProjectVolumes(context.Background(), "Shop DB"); err == nil {
		t.Fatal("invalid project accepted")
	}
}

func TestVolumeLabelsReadsTheLabelsOfAnExistingVolume(t *testing.T) {
	e := fakeEngine(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/v1.44/volumes/restored-1":
			return response(200, `{"Name":"restored-1","Labels":{"berthy.restore-operation":"op1"}}`), nil
		case "/v1.44/volumes/plain":
			return response(200, `{"Name":"plain","Labels":null}`), nil
		case "/v1.44/volumes/gone":
			return response(404, `{"message":"no such volume"}`), nil
		}
		return response(500, `{}`), nil
	})
	labels, ok, err := e.VolumeLabels(context.Background(), "restored-1")
	if err != nil || !ok || labels["berthy.restore-operation"] != "op1" {
		t.Fatalf("labelled volume: %v %v %v", labels, ok, err)
	}
	if labels, ok, err = e.VolumeLabels(context.Background(), "plain"); err != nil || !ok || len(labels) != 0 {
		t.Fatalf("volume without labels: %v %v %v", labels, ok, err)
	}
	if _, ok, err = e.VolumeLabels(context.Background(), "gone"); err != nil || ok {
		t.Fatalf("missing volume: %v %v", ok, err)
	}
	if _, _, err = e.VolumeLabels(context.Background(), "broken"); err == nil {
		t.Fatal("server error reported as absent")
	}
}

func TestProjectVolumesRefusesPartialInventoryWarnings(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		wantErr    bool
	}{
		{"warning with no volumes", `{"Volumes":[],"Warnings":["driver private detail"]}`, true},
		{"warning with volumes", `{"Volumes":[{"Name":"present"}],"Warnings":["driver private detail"]}`, true},
		{"empty warning string", `{"Volumes":[],"Warnings":[""]}`, true},
		{"empty warnings", `{"Volumes":[{"Name":"present"}],"Warnings":[]}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := fakeEngine(func(*http.Request) (*http.Response, error) { return response(200, tc.body), nil })
			got, err := e.ProjectVolumes(context.Background(), "shop-db")
			if tc.wantErr {
				if err == nil || err.Error() != "volume inventory unavailable" || got != nil {
					t.Fatalf("partial inventory accepted: %v %v", got, err)
				}
			} else if err != nil || len(got) != 1 || got[0] != "present" {
				t.Fatalf("complete inventory: %v %v", got, err)
			}
		})
	}
}
