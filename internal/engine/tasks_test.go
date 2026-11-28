package engine

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// cannedTasks is Docker's task list for one service, in the shape the Docker API returns it. It holds one task of each
// kind the failure count must tell apart: tasks Swarm marked failed or rejected, shutdowns with exit codes 1, 0 and none,
// a running task, a task without a timestamp, an old failure, and a failure whose status timestamp is missing.
const cannedTasks = `[
 {"ID":"t-failed","ServiceID":"svc1","NodeID":"n1","DesiredState":"shutdown","Slot":1,"UpdatedAt":"2026-11-16T10:00:00Z","Status":{"State":"failed","Timestamp":"2026-11-16T10:04:00Z","ContainerStatus":{"ContainerID":"c-failed","ExitCode":1}}},
 {"ID":"t-rejected","ServiceID":"svc1","NodeID":"n1","DesiredState":"shutdown","Slot":2,"Status":{"State":"rejected","Timestamp":"2026-11-16T10:06:00Z"}},
 {"ID":"t-exit1","ServiceID":"svc1","NodeID":"n1","DesiredState":"shutdown","Slot":3,"Status":{"State":"shutdown","Timestamp":"2026-11-16T10:07:00Z","ContainerStatus":{"ContainerID":"c-exit1","ExitCode":1}}},
 {"ID":"t-planned","ServiceID":"svc1","NodeID":"n1","DesiredState":"shutdown","Slot":4,"Status":{"State":"shutdown","Timestamp":"2026-11-16T10:07:30Z","ContainerStatus":{"ContainerID":"c-planned","ExitCode":0}}},
 {"ID":"t-unknown-exit","ServiceID":"svc1","NodeID":"n1","DesiredState":"shutdown","Slot":5,"Status":{"State":"shutdown","Timestamp":"2026-11-16T10:08:00Z"}},
 {"ID":"t-running","ServiceID":"svc1","NodeID":"n1","DesiredState":"running","Slot":6,"Status":{"State":"running","Timestamp":"2026-11-16T10:09:00Z","ContainerStatus":{"ContainerID":"c-run"}}},
 {"ID":"t-untimed","ServiceID":"svc1","NodeID":"n1","DesiredState":"shutdown","Slot":7,"Status":{"State":"failed"}},
 {"ID":"t-old","ServiceID":"svc1","NodeID":"n1","DesiredState":"shutdown","Slot":8,"Status":{"State":"failed","Timestamp":"2026-11-16T09:00:00Z","ContainerStatus":{"ContainerID":"c-old","ExitCode":2}}},
 {"ID":"t-fallback","ServiceID":"svc1","NodeID":"n1","DesiredState":"shutdown","Slot":9,"UpdatedAt":"2026-11-16T10:02:00Z","Status":{"State":"failed"}}
]`

// cannedRollingUpdate is the task list of a service during a rolling update: three old tasks that Docker shut down with
// exit code 143 (SIGTERM), and the new task, running.
const cannedRollingUpdate = `[
 {"ID":"old-1","ServiceID":"svc1","NodeID":"n1","DesiredState":"shutdown","Slot":1,"Status":{"State":"shutdown","Timestamp":"2026-11-16T10:03:00Z","ContainerStatus":{"ContainerID":"c-old-1","ExitCode":143}}},
 {"ID":"old-2","ServiceID":"svc1","NodeID":"n1","DesiredState":"shutdown","Slot":2,"Status":{"State":"shutdown","Timestamp":"2026-11-16T10:05:00Z","ContainerStatus":{"ContainerID":"c-old-2","ExitCode":143}}},
 {"ID":"old-3","ServiceID":"svc1","NodeID":"n1","DesiredState":"shutdown","Slot":3,"Status":{"State":"shutdown","Timestamp":"2026-11-16T10:07:00Z","ContainerStatus":{"ContainerID":"c-old-3","ExitCode":143}}},
 {"ID":"new-1","ServiceID":"svc1","NodeID":"n1","DesiredState":"running","Slot":1,"Status":{"State":"running","Timestamp":"2026-11-16T10:07:05Z","ContainerStatus":{"ContainerID":"c-new-1"}}}
]`

// tasksEngine returns an engine whose task list for service svc1 is body.
func tasksEngine(t *testing.T, body string) *Engine {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1.44/tasks" || !strings.Contains(r.URL.Query().Get("filters"), `"svc1"`) {
			t.Errorf("request %s?%s", r.URL.Path, r.URL.RawQuery)
		}
		fmt.Fprint(w, body)
	}))
	t.Cleanup(s.Close)
	return &Engine{client: &http.Client{Transport: rewriteTransport{base: s.URL}}}
}

func TestServiceTasksFeedFailureCounts(t *testing.T) {
	tasks, err := tasksEngine(t, cannedTasks).ServiceTasks(context.Background(), "svc1")
	if err != nil || len(tasks) != 9 {
		t.Fatalf("tasks: %d %v", len(tasks), err)
	}
	byID := map[string]ServiceTask{}
	for _, task := range tasks {
		byID[task.ID] = task
	}
	if got := byID["t-failed"]; !got.Timestamp.Equal(time.Date(2026, 11, 16, 10, 4, 0, 0, time.UTC)) {
		t.Errorf("failed task timestamp: %v", got.Timestamp)
	}
	if got := byID["t-fallback"]; !got.Timestamp.Equal(time.Date(2026, 11, 16, 10, 2, 0, 0, time.UTC)) {
		t.Errorf("status timestamp not replaced by the update time: %v", got.Timestamp)
	}
	// Failed and rejected tasks count, including the one whose status timestamp is missing, which its update time places.
	// Shutdowns do not count, whatever their exit code. Tasks outside the window and tasks without a timestamp do not count.
	if n := FailedTasksSince(tasks, time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)); n != 3 {
		t.Errorf("failures in the window: %d, want 3", n)
	}
	// A failure exactly at the start of the window counts; one a moment earlier does not.
	if n := FailedTasksSince(tasks, time.Date(2026, 11, 16, 10, 4, 0, 0, time.UTC)); n != 2 {
		t.Errorf("failures from 10:04: %d, want 2", n)
	}
	if n := FailedTasksSince(tasks, time.Date(2026, 11, 16, 10, 10, 0, 0, time.UTC)); n != 0 {
		t.Errorf("failures after the window: %d, want 0", n)
	}
}

// TestRollingUpdateShutdownsAreNotFailures: the three old tasks of a rolling update are shut down with exit code 143. They
// are not failures, so they count as zero and raise no task-failure warning.
func TestRollingUpdateShutdownsAreNotFailures(t *testing.T) {
	tasks, err := tasksEngine(t, cannedRollingUpdate).ServiceTasks(context.Background(), "svc1")
	if err != nil || len(tasks) != 4 {
		t.Fatalf("tasks: %d %v", len(tasks), err)
	}
	if n := FailedTasksSince(tasks, time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)); n != 0 {
		t.Fatalf("rolling update counted %d failures, want 0", n)
	}
}

func TestServiceTasksReportsRunningSince(t *testing.T) {
	body := strings.Replace(cannedTasks, `"Slot":9,"UpdatedAt":"2026-11-16T10:02:00Z","Status":{"State":"failed"}`, `"Slot":9,"UpdatedAt":"2026-11-16T10:02:00Z","Status":{"State":"running"}`, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tasks, err := tasksEngine(t, body).ServiceTasks(ctx, "svc1")
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range tasks {
		switch task.ID {
		case "t-running":
			want := time.Date(2026, 11, 16, 10, 9, 0, 0, time.UTC)
			if !task.RunningSince.Equal(want) {
				t.Errorf("running since %v, want %v", task.RunningSince, want)
			}
		default:
			if !task.RunningSince.IsZero() {
				t.Errorf("task %s has unexpected running-since evidence %v", task.ID, task.RunningSince)
			}
		}
		if task.ID == "t-fallback" && task.Timestamp.IsZero() {
			t.Error("missing fallback timestamp for diagnostics")
		}
	}
}
