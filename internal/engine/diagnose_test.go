package engine

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestServiceTasksParseFailureDetails(t *testing.T) {
	body := `[
 {"ID":"t1","ServiceID":"svc1","DesiredState":"shutdown","Slot":1,"Status":{"State":"failed","Err":"task: non-zero exit (1)","Message":"started","Timestamp":"2026-11-16T10:04:00Z","ContainerStatus":{"ContainerID":"c1","ExitCode":1}}},
 {"ID":"t2","ServiceID":"svc1","DesiredState":"shutdown","Slot":2,"Status":{"State":"rejected","Err":"No such image: x","Message":"preparing","Timestamp":"2026-11-16T10:05:00Z"}},
 {"ID":"t3","ServiceID":"svc1","DesiredState":"running","Slot":3,"Status":{"State":"running","Message":"started","ContainerStatus":{"ContainerID":"c3","ExitCode":0}}}
]`
	tasks, err := tasksEngine(t, body).ServiceTasks(context.Background(), "svc1")
	if err != nil || len(tasks) != 3 {
		t.Fatalf("tasks %d: %v", len(tasks), err)
	}
	if tasks[0].Err != "task: non-zero exit (1)" || tasks[0].Message != "started" || tasks[0].ExitCode == nil || *tasks[0].ExitCode != 1 {
		t.Errorf("failed task: %+v", tasks[0])
	}
	if tasks[1].Err != "No such image: x" || tasks[1].ExitCode != nil {
		t.Errorf("rejected task has no exit code: %+v", tasks[1])
	}
	if tasks[2].ExitCode == nil || *tasks[2].ExitCode != 0 {
		t.Errorf("a reported exit code 0 stays distinct from none: %+v", tasks[2])
	}
}

func inspectEngine(t *testing.T, path, body string) *Engine {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != path {
			t.Errorf("request %s", r.URL.Path)
		}
		fmt.Fprint(w, body)
	}))
	t.Cleanup(s.Close)
	return &Engine{client: &http.Client{Transport: rewriteTransport{base: s.URL}}}
}

func TestInspectContainerHealthRequiresRunning(t *testing.T) {
	for _, tc := range []struct{ name, state, want string }{
		{"stopped", `{"Running":false,"Health":{"Status":"healthy"}}`, ""},
		{"missing running", `{"Health":{"Status":"healthy"}}`, ""},
		{"healthy", `{"Running":true,"Health":{"Status":"healthy"}}`, "healthy"},
		{"starting", `{"Running":true,"Health":{"Status":"starting"}}`, "starting"},
		{"no health", `{"Running":true}`, "none"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != http.MethodGet || r.URL.Path != "/v1.44/containers/c1/json" {
					t.Errorf("request %s %s", r.Method, r.URL.Path)
				}
				fmt.Fprint(w, `{"State":`+tc.state+`}`)
			}))
			defer s.Close()
			e := &Engine{client: &http.Client{Transport: rewriteTransport{base: s.URL}}}
			got, err := e.InspectContainerHealth(context.Background(), "c1")
			if err != nil || got != tc.want {
				t.Fatalf("health = %q, %v; want %q", got, err, tc.want)
			}
			if calls.Load() != 1 {
				t.Fatalf("inspect calls = %d, want 1", calls.Load())
			}
		})
	}
}

func TestDiagnoseContainerParsesStateAndHealthLog(t *testing.T) {
	long := strings.Repeat("x", 3000)
	var probes []string
	for i := 1; i <= 7; i++ {
		probes = append(probes, fmt.Sprintf(`{"Start":"2026-11-16T10:00:0%dZ","End":"2026-11-16T10:00:0%dZ","ExitCode":1,"Output":"probe %d %s"}`, i, i, i, long))
	}
	body := `{"Id":"c1","State":{"Status":"exited","ExitCode":137,"OOMKilled":true,"Error":"oom","Health":{"Status":"unhealthy","FailingStreak":7,"Log":[` + strings.Join(probes, ",") + `]}}}`
	d, err := inspectEngine(t, "/v1.44/containers/c1/json", body).DiagnoseContainer(context.Background(), "c1")
	if err != nil {
		t.Fatal(err)
	}
	if d.State != "exited" || d.ExitCode != 137 || !d.OOMKilled || d.Error != "oom" || d.Health != "unhealthy" {
		t.Fatalf("diagnosis %+v", d)
	}
	if len(d.HealthLog) != 5 || !strings.HasPrefix(d.HealthLog[0].Output, "probe 3 ") || !strings.HasPrefix(d.HealthLog[4].Output, "probe 7 ") {
		t.Fatalf("health log keeps the last five probes: %d", len(d.HealthLog))
	}
	for _, p := range d.HealthLog {
		if len(p.Output) > 1024 || p.ExitCode != 1 || !p.Start.After(time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)) {
			t.Fatalf("probe %+v", p)
		}
	}
}

func TestDiagnoseContainerWithoutHealthCheck(t *testing.T) {
	d, err := inspectEngine(t, "/v1.44/containers/c1/json", `{"State":{"Status":"exited","ExitCode":1}}`).DiagnoseContainer(context.Background(), "c1")
	if err != nil || d.State != "exited" || d.ExitCode != 1 || d.Health != "" || len(d.HealthLog) != 0 {
		t.Fatalf("diagnosis %+v: %v", d, err)
	}
}

func TestLogTailInterleavesStreamsAndCapsBytes(t *testing.T) {
	var logBody bytes.Buffer
	logBody.WriteString(frame(1, "2026-11-16T10:00:01.000000000Z starting\n"))
	logBody.WriteString(frame(2, "2026-11-16T10:00:03.000000000Z config: REQUIRED_SETTING is missing\n"))
	logBody.WriteString(frame(1, "2026-11-16T10:00:02.000000000Z listening\n"))
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1.44/containers/c1/json":
			fmt.Fprint(w, `{"Config":{"Tty":false}}`)
		case "/v1.44/containers/c1/logs":
			if r.URL.Query().Get("tail") != "200" || r.URL.Query().Get("stdout") != "1" || r.URL.Query().Get("stderr") != "1" {
				t.Errorf("query %s", r.URL.RawQuery)
			}
			w.Write(logBody.Bytes())
		default:
			t.Errorf("request %s", r.URL.Path)
		}
	}))
	defer s.Close()
	e := &Engine{client: &http.Client{Transport: rewriteTransport{base: s.URL}}}
	text, truncated, err := e.LogTail(context.Background(), "c1", 200, 64<<10)
	if err != nil || truncated {
		t.Fatalf("tail %q %v: %v", text, truncated, err)
	}
	want := "2026-11-16T10:00:01Z stdout starting\n2026-11-16T10:00:02Z stdout listening\n2026-11-16T10:00:03Z stderr config: REQUIRED_SETTING is missing\n"
	if text != want {
		t.Fatalf("tail %q, want %q", text, want)
	}
	// A cap that fits only the newest line drops the older ones and says so.
	last := "2026-11-16T10:00:03Z stderr config: REQUIRED_SETTING is missing\n"
	text, truncated, err = e.LogTail(context.Background(), "c1", 200, len(last))
	if err != nil || !truncated || text != last {
		t.Fatalf("capped tail %q %v: %v", text, truncated, err)
	}
}

func TestLogTailKeepsNewerEvidenceAfterLongLine(t *testing.T) {
	stamp := "2026-11-16T10:00:01Z "
	body := frame(1, stamp+"before\n"+stamp+strings.Repeat("x", 300<<10)+"\n"+stamp+"after\n")
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/json") {
			fmt.Fprint(w, `{"Config":{"Tty":false}}`)
			return
		}
		fmt.Fprint(w, body)
	}))
	defer s.Close()
	e := &Engine{client: &http.Client{Transport: rewriteTransport{base: s.URL}}}
	text, truncated, err := e.LogTail(context.Background(), "c1", 200, 64<<10)
	if err != nil || !truncated || text != "2026-11-16T10:00:01Z stdout after\n" {
		t.Fatalf("tail %q truncated %v: %v", text, truncated, err)
	}
}

func TestLogTailReportsDockerDaemonError(t *testing.T) {
	body := frame(3, "log reader failed\n")
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/json") {
			fmt.Fprint(w, `{"Config":{"Tty":false}}`)
			return
		}
		fmt.Fprint(w, body)
	}))
	defer s.Close()
	e := &Engine{client: &http.Client{Transport: rewriteTransport{base: s.URL}}}
	text, truncated, err := e.LogTail(context.Background(), "c1", 200, 64<<10)
	if err != nil || truncated || !strings.Contains(text, " stderr Docker daemon error: log reader failed\n") {
		t.Fatalf("tail %q truncated %v: %v", text, truncated, err)
	}
}

func TestDiagnoseContainerReadsRunningWithHealth(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		running    bool
	}{
		{"running", `{"State":{"Status":"running","Running":true,"Health":{"Status":"healthy"}}}`, true},
		{"exited", `{"State":{"Status":"exited","Running":false,"Health":{"Status":"healthy"}}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			d, err := inspectEngine(t, "/v1.44/containers/c1/json", tc.body).DiagnoseContainer(ctx, "c1")
			if err != nil || d.Running != tc.running || d.Health != "healthy" {
				t.Fatalf("running=%v health=%q, error %v, want running=%v and healthy", d.Running, d.Health, err, tc.running)
			}
		})
	}
}
