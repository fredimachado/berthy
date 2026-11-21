package agent

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"berthy/internal/engine"
	"berthy/internal/monitor"
	"berthy/internal/state"
)

func runtimeConfig(t *testing.T) (Shared, Config) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(dir, "key")
	if err := os.WriteFile(key, make([]byte, 32), 0600); err != nil {
		t.Fatal(err)
	}
	c := Config{Environment: "lab", Listen: "127.0.0.1:19501", StateDir: dir, KeyFile: key,
		Tokens: []Token{{Name: "reader", Digest: Digest(apReadToken), Scope: "read"}}}
	return Shared{Engine: engine.New(filepath.Join(dir, "missing.sock"))}, c
}

func TestARuntimeServesItsEnvironmentAndCloseReleasesTheState(t *testing.T) {
	sh, c := runtimeConfig(t)
	r, err := OpenRuntime(sh, c)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Start(context.Background()); err != nil {
		r.Close()
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	r.Handler.ServeHTTP(w, newGet("/v1/environments/lab/agent", apReadToken))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"environment":"lab"`) {
		t.Errorf("agent: %d %s", w.Code, w.Body.String())
	}
	r.Close()
	s, err := state.Open(c.StateDir, c.KeyFile, c.Environment)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
}

func TestOpenRuntimeRefusesAConfigWithoutState(t *testing.T) {
	sh, c := runtimeConfig(t)
	for _, paths := range [][2]string{{"", ""}, {c.StateDir, ""}, {"", c.KeyFile}} {
		cfg := c
		cfg.StateDir, cfg.KeyFile = paths[0], paths[1]
		if r, err := OpenRuntime(sh, cfg); err == nil || err.Error() != "a hosted environment needs state_dir and key_file" {
			if r != nil {
				r.Close()
			}
			t.Fatalf("state paths %q: %v", paths, err)
		}
	}
}

type runtimeCheck struct{}

func (runtimeCheck) Name() string            { return "runtime-test" }
func (runtimeCheck) Interval() time.Duration { return time.Hour }
func (runtimeCheck) Failures() int           { return 1 }
func (runtimeCheck) Run(context.Context) monitor.Result {
	return monitor.Result{State: monitor.Warning}
}

func TestOpenRuntimeStartsNothing(t *testing.T) {
	sh, c := runtimeConfig(t)
	var mu sync.Mutex
	var lines []string
	sh.Log = func(line string) { mu.Lock(); defer mu.Unlock(); lines = append(lines, line) }
	r, err := OpenRuntime(sh, c)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	rows, err := r.deployment.Store.Checks(context.Background())
	if err != nil || len(rows) != 0 {
		t.Fatalf("checks before start: %+v %v", rows, err)
	}
	mu.Lock()
	if len(lines) != 0 {
		t.Errorf("log before start: %v", lines)
	}
	mu.Unlock()
	// The notifier runs only after the monitor has recorded this check's warning.
	recorded := make(chan struct{})
	var once sync.Once
	r.monitoring.mon.Checks = func(context.Context) []monitor.Check { return []monitor.Check{runtimeCheck{}} }
	r.monitoring.mon.Notifier = func(context.Context) (monitor.Notifier, time.Duration, error) {
		once.Do(func() { close(recorded) })
		return nil, 0, nil
	}
	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	<-recorded
	rows, err = r.deployment.Store.Checks(context.Background())
	if err != nil || len(rows) != 1 || rows[0].Name != "runtime-test" {
		t.Fatalf("checks after start: %+v %v", rows, err)
	}
}

func TestOpenRuntimeFailsOnAWrongKeyAndHoldsNoLock(t *testing.T) {
	sh, c := runtimeConfig(t)
	s, err := state.Open(c.StateDir, c.KeyFile, c.Environment)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	wrong := filepath.Join(c.StateDir, "wrong")
	if err := os.WriteFile(wrong, []byte(strings.Repeat("x", 32)), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := c
	cfg.KeyFile = wrong
	if r, err := OpenRuntime(sh, cfg); !errors.Is(err, state.ErrWrongKey) {
		if r != nil {
			r.Close()
		}
		t.Fatalf("wrong key: %v", err)
	}
	s, err = state.Open(c.StateDir, c.KeyFile, c.Environment)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
}

func TestRecoveryRetryOnlyForRequired(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls, recovered atomic.Int32
	active := func(context.Context) (state.Operation, error) {
		if calls.Add(1) == 1 {
			return state.Operation{State: "running"}, nil
		}
		return state.Operation{State: "recovery_required"}, nil
	}
	// The recovery ends the retrying and takes longer than the interval, so another tick is waiting when it returns: the loop
	// must still stop, and must not recover a second time.
	recover := func(context.Context) error {
		recovered.Add(1)
		cancel()
		time.Sleep(5 * time.Millisecond)
		return nil
	}
	finished := make(chan struct{})
	go func() {
		retryRecovery(ctx, time.Millisecond, active, recover)
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(10 * time.Second):
		t.Fatal("recovery not retried")
	}
	if calls.Load() < 2 {
		t.Fatal("retried a running operation")
	}
	if n := recovered.Load(); n != 1 {
		t.Fatalf("recovered %d times, want once", n)
	}
}

func TestRecoveryNoticeNamesResolveCommand(t *testing.T) {
	if got := recoveryNotice("op1"); !strings.Contains(got, "berthy recovery resolve op1 -m REASON") {
		t.Fatalf("notice with id: %q", got)
	}
	if got := recoveryNotice(""); !strings.Contains(got, "berthy recovery resolve OPERATION_ID -m REASON") || strings.Contains(got, "\n") {
		t.Fatalf("notice without id: %q", got)
	}
}
