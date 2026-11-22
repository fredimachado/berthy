package tui

import (
	"context"
	"flag"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"berthy/internal/agentclient"
	"berthy/internal/api"
	"berthy/internal/logs"
	"berthy/internal/metrics"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

var update = flag.Bool("update", false, "rewrite the golden files")

// testNow is the fixed clock of the tests.
var testNow = time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)

func TestMain(m *testing.M) {
	time.Local = time.UTC
	os.Setenv("TERM", "xterm-256color") // huh switches a form to accessible mode when TERM=dumb
	os.Exit(m.Run())
}

func u64(v uint64) *uint64        { return &v }
func f64(v float64) *float64      { return &v }
func intp(v int) *int             { return &v }
func boolp(v bool) *bool          { return &v }
func tptr(t time.Time) *time.Time { return &t }

const digestA = "127.0.0.1:15001/app@sha256:abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"

func fixtureApps() []api.App {
	return []api.App{
		{
			Ref: api.AppRef{Kind: "image", Name: "api"},
			Roles: []api.Role{{Name: "web", Desired: u64(2), Running: 2, Replicas: []api.Replica{
				{ContainerID: "aaaaaaaaaaaa1111", Image: digestA, State: "running", Health: "healthy", RestartCount: intp(0), OOMKilled: boolp(false)},
				{ContainerID: "bbbbbbbbbbbb2222", Image: digestA, State: "running", Health: "healthy", RestartCount: intp(2), OOMKilled: boolp(false)},
			}}},
			Jobs: []api.JobStatus{{Name: "cleanup", App: "api", Target: "web", Schedule: "0 3 * * *", Timeout: "5m", Next: tptr(testNow.Add(15 * time.Hour)),
				Last: &api.JobRun{Status: "succeeded", Started: testNow.Add(-9 * time.Hour), ExitCode: intp(0)}}},
			Backups: []api.BackupStatus{{Name: "main-db", Volume: "data", Schedule: "0 */6 * * *", LastSuccess: tptr(testNow.Add(-3 * time.Hour)), Size: 3 << 20, LastStatus: "succeeded"}},
		},
		{
			Ref: api.AppRef{Kind: "image", Name: "worker"},
			Roles: []api.Role{{Name: "run", Desired: u64(1), Running: 1, Replicas: []api.Replica{
				{ContainerID: "cccccccccccc3333", Image: "registry.example/worker:v3", State: "running", Health: "starting", RestartCount: intp(1), OOMKilled: boolp(true)},
			}}},
			Backups: []api.BackupStatus{{Name: "queue", Volume: "q", LastSuccess: tptr(testNow.Add(-26 * time.Hour)), LastStatus: "failed", Detail: "object storage refused the upload"}},
		},
		{
			Ref: api.AppRef{Kind: "compose", Name: "blog"},
			Roles: []api.Role{
				{Name: "web", Running: 1, Replicas: []api.Replica{{ContainerID: "dddddddddddd4444", Image: "ghost:5", State: "running", Health: "unhealthy"}}},
				{Name: "db", Running: 1, Replicas: []api.Replica{{ContainerID: "eeeeeeeeeeee5555", Image: "mysql:8", State: "running", Health: "none"}}},
			},
		},
	}
}

func fixtureHistory() []historyRow {
	return []historyRow{
		{ID: 3, Tag: "v1.0.2", Digest: "sha256:abcdef0123456789", Commit: "c3", Created: testNow.Add(-10 * time.Minute)},
		{OperationID: "op1", Kind: "deploy", State: "rolled_back", Commit: "cbad", Created: testNow.Add(-40 * time.Minute),
			Reason: "replica web became unhealthy: health check failed 3 times, the container exited with code 1 and was rolled back"},
		{ID: 2, Digest: "sha256:0123456789abcdef", Commit: "c2", Created: testNow.Add(-2 * time.Hour)},
	}
}

func fixtureApprovals() []approvalRow {
	return []approvalRow{
		{ID: "a1b2c3d4e5f6", Actor: "claude", Kind: "deploy", Summary: "deploy api sha256:abc123456789", Status: "pending",
			Created: testNow.Add(-3 * time.Minute), Expires: testNow.Add(12 * time.Minute), Plan: []byte(`{"action":"deploy","app":"api","version":"sha256:abc123456789","roles":[{"name":"web","replicas":2}]}`)},
		{ID: "f6e5d4c3b2a1", Actor: "claude", Kind: "restart", Summary: "restart api/web", Status: "approved", DecidedBy: "fredi",
			Created: testNow.Add(-2 * time.Hour), Expires: testNow.Add(-110 * time.Minute)},
		{ID: "0a0a0a0a0a0a", Actor: "ci", Kind: "scale", Summary: "scale worker to 3", Status: "expired",
			Created: testNow.Add(-5 * time.Hour), Expires: testNow.Add(-295 * time.Minute)},
	}
}

func fixtureSample(at time.Time, cpu float64) metrics.Sample {
	return metrics.Sample{
		Time: at,
		Host: metrics.Host{
			CPUPercent: f64(cpu), MemoryTotal: u64(4 << 30), MemoryAvailable: u64(1 << 30), SwapTotal: u64(1 << 30), SwapFree: u64(1 << 30),
			Load: []float64{0.5, 0.4, 0.3}, DiskReadBytes: u64(1000), DiskWriteBytes: u64(2000), NetworkRXBytes: u64(5000), NetworkTXBytes: u64(6000),
			Mounts: []metrics.Mount{{Path: "/", Total: 100 << 30, Available: 40 << 30}, {Path: "/var/lib/docker", Total: 200 << 30, Available: 20 << 30}},
		},
		Containers: []metrics.Container{
			{ID: "aaaaaaaaaaaa1111ffff", CPUPercent: f64(12.5), MemoryUsage: 120 << 20, MemoryLimit: 512 << 20, NetworkRXBytes: 1 << 20, NetworkTXBytes: 2 << 20, BlockReadBytes: 4096, BlockWriteBytes: 8192},
			{ID: "cccccccccccc3333ffff", CPUPercent: f64(71.2), MemoryUsage: 300 << 20, MemoryLimit: 0, NetworkRXBytes: 100, NetworkTXBytes: 200},
			{ID: "ffffffffffff9999ffff", CPUPercent: f64(0.3), MemoryUsage: 20 << 20, MemoryLimit: 64 << 20},
		},
		ContainersFresh: true,
	}
}

func fixtureDF() dfInfo {
	var d dfInfo
	d.HostStatus, d.DockerStatus = "available", "available"
	d.Docker.LayersSize = 1 << 30
	return d
}

// newTestModel makes a model with a client that goes nowhere, a fixed clock and no animation.
func newTestModel(t *testing.T, w, h int) *Model {
	t.Helper()
	base, _ := url.Parse("http://127.0.0.1:1")
	target := &agentclient.Target{Project: "demo", Env: "prod", Base: base, Prefix: "/v1/projects/demo/environments/prod/", Token: "secret-token-value"}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	m := NewModel(newClient(ctx, target), "prod", func() time.Time { return testNow })
	m.noAnim = true
	send(m, tea.WindowSizeMsg{Width: w, Height: h})
	return m
}

// send applies messages to the model and drops the commands.
func send(m *Model, msgs ...tea.Msg) {
	for _, msg := range msgs {
		m.Update(msg)
	}
}

// loaded is a model that has the fixture data.
func loadedModel(t *testing.T, w, h int) *Model {
	m := newTestModel(t, w, h)
	send(m, infoMsg{Environment: "prod", Hostname: "lab-1", Protected: true}, appsMsg{apps: fixtureApps()},
		lockMsg{Locked: true, Holder: "ci", Reason: "migrating"}, pendingMsg{n: 1})
	return m
}

func keypress(s string) tea.Msg {
	switch s {
	case "enter":
		return tea.KeyPressMsg{Code: tea.KeyEnter}
	case "esc":
		return tea.KeyPressMsg{Code: tea.KeyEscape}
	case "tab":
		return tea.KeyPressMsg{Code: tea.KeyTab}
	case "shift+tab":
		return tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift}
	case "down":
		return tea.KeyPressMsg{Code: tea.KeyDown}
	case "up":
		return tea.KeyPressMsg{Code: tea.KeyUp}
	case "ctrl+c":
		return tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}
	}
	r := []rune(s)
	return tea.KeyPressMsg{Code: r[0], Text: s}
}

func keys(m *Model, names ...string) {
	for _, n := range names {
		send(m, keypress(n))
	}
}

func stripANSIForTest(s string) string { return ansi.Strip(s) }

// plain is what the view shows, without colours.
func plain(m *Model) string {
	return ansi.Strip(m.View().Content)
}

// golden compares the view with testdata/NAME.golden. With -update it writes the file.
func golden(t *testing.T, name, got string) {
	t.Helper()
	got = strings.TrimRight(got, "\n") + "\n"
	path := filepath.Join("testdata", name+".golden")
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run the test with -update)", err)
	}
	if strings.ReplaceAll(string(want), "\r\n", "\n") != got {
		t.Errorf("view differs from %s:\n%s", path, got)
	}
}

func logRec(sec int, role, replica, stream, msg string) logs.Record {
	return logs.Record{Identity: logs.Identity{Role: role, Replica: replica}, Timestamp: testNow.Add(time.Duration(sec) * time.Second), Stream: stream, Message: msg}
}

// TestDeployLinesShowTheTagBeforeTheDigest: a release deployed by tag shows the tag first, and in a list that mixes tagged and
// untagged releases the commit column starts at the same cell on every row. A list with no tag keeps the 16-cell column.
func TestDeployLinesShowTheTagBeforeTheDigest(t *testing.T) {
	st := newStyles(newPalette(true))
	hist := []historyRow{
		{ID: 3, Tag: "v1.0.2", Digest: "sha256:111c870af40e0000", Commit: "c3", Created: testNow.Add(-10 * time.Minute)},
		{ID: 2, Digest: "sha256:0123456789abcdef", Commit: "c2", Created: testNow.Add(-2 * time.Hour)},
	}
	lines := deployHistoryLines(st, testNow, hist, 80)
	if len(lines) != 2 {
		t.Fatalf("%d lines for two releases", len(lines))
	}
	first, second := ansi.Strip(lines[0]), ansi.Strip(lines[1])
	if !strings.Contains(first, "v1.0.2 "+glyphDot+" sha256:111c870a") {
		t.Fatalf("a release with a tag: %q", first)
	}
	if strings.Contains(second, "v1.0.2") || !strings.Contains(second, "sha256:01234567") {
		t.Fatalf("a release without a tag: %q", second)
	}
	column := func(line string) int { return ansi.StringWidth(line[:strings.Index(line, "commit")]) }
	if column(first) != column(second) {
		t.Fatalf("the commit column starts at cell %d and %d:\n%s\n%s", column(first), column(second), first, second)
	}
	only := ansi.Strip(deployHistoryLines(st, testNow, hist[1:], 80)[0])
	if want := column(second) - (ansi.StringWidth("v1.0.2 "+glyphDot+" sha256:111c870a") - 16); column(only) != want {
		t.Fatalf("an untagged-only list lost its 16-cell column: commit at %d, want %d:\n%s", column(only), want, only)
	}
}
