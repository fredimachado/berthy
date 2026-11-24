package agent

import (
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/synctest"

	"berthy/internal/deploy"
	"berthy/internal/state"
)

type gatedStreamWriter struct {
	*httptest.ResponseRecorder
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (w *gatedStreamWriter) Flush() {
	w.ResponseRecorder.Flush()
	w.once.Do(func() { close(w.entered); <-w.release })
}

func TestStreamOperationDrainsCommittedFinalEvents(t *testing.T) {
	for _, progress := range []bool{false, true} {
		t.Run(fmt.Sprintf("progress=%t", progress), func(t *testing.T) {
			dir := t.TempDir()
			if err := os.Chmod(dir, 0700); err != nil {
				t.Fatal(err)
			}
			key := filepath.Join(dir, "key")
			if err := os.WriteFile(key, make([]byte, 32), 0600); err != nil {
				t.Fatal(err)
			}
			s, err := state.Open(dir, key, "lab")
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			ctx := context.Background()
			op, _, err := s.Admit(ctx, state.Admission{RequestID: "r", ActorID: "a", Kind: "deploy", App: "app", PublicRequest: []byte(`{}`)})
			if err != nil {
				t.Fatal(err)
			}
			token := "bty.demo.lab." + strings.Repeat("a", 64)
			cfg := Config{Project: "demo", Environment: "lab", Tokens: []Token{{Digest: Digest(token), Scope: "read"}}}
			h := NewDeploymentHandler(cfg, nil, nil, &deploy.Controller{Store: s})
			synctest.Test(t, func(t *testing.T) {
				path := "/v1/projects/demo/environments/lab/operations/" + op.ID + "/events"
				if progress {
					path += "?progress=1"
				}
				req := httptest.NewRequest("GET", path, nil)
				req.Header.Set("Authorization", "Bearer "+token)
				streamCtx, cancel := context.WithCancel(ctx)
				req = req.WithContext(streamCtx)
				w := &gatedStreamWriter{ResponseRecorder: httptest.NewRecorder(), entered: make(chan struct{}), release: make(chan struct{})}
				done := make(chan struct{})
				var release sync.Once
				unblock := func() { release.Do(func() { close(w.release) }) }
				go func() { defer close(done); h.ServeHTTP(w, req) }()
				defer func() { cancel(); unblock(); <-done }()
				synctest.Wait()
				select {
				case <-w.entered:
				default:
					t.Fatal("stream did not emit accepted")
				}
				for i := 0; i < 201; i++ {
					status := "started"
					if i == 200 {
						status = "ready"
					}
					if err := s.AppendEvent(ctx, op.ID, state.Event{Kind: "rollout", Data: []byte(fmt.Sprintf(`{"role":"web","status":%q}`, status))}); err != nil {
						t.Fatal(err)
					}
				}
				if err := s.Finish(ctx, op.ID, state.Outcome{State: "succeeded"}); err != nil {
					t.Fatal(err)
				}
				unblock()
				<-done
				body := w.Body.String()
				events, err := s.EventsAfter(ctx, op.ID, 0, 1000)
				if err != nil {
					t.Fatal(err)
				}
				var expected strings.Builder
				for _, event := range events {
					if event.Kind == "rollout" && !progress {
						continue
					}
					fmt.Fprintf(&expected, "id: %d\nevent: %s\ndata: %s\n\n", event.ID, event.Kind, event.Data)
				}
				if !strings.HasPrefix(body, expected.String()+"event: terminal\n") {
					t.Error("persisted events do not precede terminal in their original order")
				}
				wantRollouts := 0
				if progress {
					wantRollouts = 201
				}
				if got := strings.Count(body, "event: rollout\n"); got != wantRollouts {
					t.Errorf("rollout count = %d, want %d", got, wantRollouts)
				}
				accepted, final, terminal := strings.Index(body, "event: accepted\n"), strings.Index(body, "event: succeeded\n"), strings.Index(body, "event: terminal\n")
				if accepted < 0 || final <= accepted || terminal <= final {
					t.Errorf("missing or unordered accepted, final, terminal: %s", body)
				}
				if progress && (strings.Index(body, `"status":"ready"`) < accepted || strings.Index(body, `"status":"ready"`) > final) {
					t.Error("ready missing or out of order")
				}
			})
		})
	}
}
