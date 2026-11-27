package deploy

import (
	"context"
	"encoding/json"
	"testing"
	"testing/synctest"
	"time"

	"berthy/internal/engine"
	"berthy/internal/state"
)

type cutoffReader struct {
	block string
	late  bool
}

func (f cutoffReader) ServiceTasks(ctx context.Context, _ string) ([]engine.ServiceTask, error) {
	if f.block == "tasks" {
		<-ctx.Done()
		if !f.late {
			return nil, ctx.Err()
		}
	}
	return []engine.ServiceTask{runningTask("one", "op")}, nil
}

func (f cutoffReader) InspectContainerHealth(ctx context.Context, _ string) (string, error) {
	if f.block == "health" {
		<-ctx.Done()
		if !f.late {
			return "", ctx.Err()
		}
	}
	return "healthy", nil
}

func TestStartupReadCannotOutliveTheCutoff(t *testing.T) {
	for _, tc := range []struct {
		name, block string
		late        bool
	}{
		{"tasks", "tasks", false}, {"health", "health", false},
		{"late tasks", "tasks", true}, {"late health", "health", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				done, exited := make(chan error, 1), make(chan struct{})
				go func() {
					defer close(exited)
					done <- waitHealthy(ctx, cutoffReader{tc.block, tc.late}, "s", "op", 1, 1, 5*time.Second)
				}()
				defer func() { cancel(); <-exited }()
				synctest.Wait()
				time.Sleep(10 * time.Second)
				synctest.Wait()
				select {
				case err := <-done:
					if err == nil || err.Error() != "service health deadline exceeded" {
						t.Fatalf("error = %v", err)
					}
				default:
					t.Fatal("startup read still running at cutoff")
				}
			})
		})
	}
}

type slowIntentJournal struct{ journalFake }

func (j *slowIntentJournal) RecordIntent(ctx context.Context, op string, i state.Intent) error {
	time.Sleep(11 * time.Second)
	return j.journalFake.RecordIntent(ctx, op, i)
}

type slowUpdateInspect struct {
	mutationFake
	inspections int
}

func (f *slowUpdateInspect) InspectService(ctx context.Context, id string) (engine.ServiceSnapshot, error) {
	f.inspections++
	if f.inspections == 1 {
		time.Sleep(11 * time.Second)
	}
	return f.mutationFake.InspectService(ctx, id)
}

func TestExpiredStartupPreventsServiceMutation(t *testing.T) {
	for _, kind := range []string{"create", "update"} {
		t.Run(kind, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				before := json.RawMessage(`{"Name":"svc","version":"old"}`)
				desired := json.RawMessage(`{"Name":"svc","version":"new"}`)
				i := state.Intent{Kind: kind, Name: "svc", ServiceID: "id", Before: before, Desired: desired, Generation: "op"}
				m := &slowUpdateInspect{mutationFake: mutationFake{live: engine.ServiceSnapshot{ID: "id", Version: 9, Spec: before}}}
				done, exited := make(chan error, 1), make(chan struct{})
				go func() {
					defer close(exited)
					if kind == "create" {
						_, err := createRole(ctx, &slowIntentJournal{}, &m.mutationFake, "op", i, nil, 1, 1, 5*time.Second)
						done <- err
						return
					}
					done <- updateRole(ctx, &journalFake{}, m, "op", i, nil, 1, 1, 5*time.Second)
				}()
				defer func() { cancel(); <-exited }()
				synctest.Wait()
				time.Sleep(11 * time.Second)
				synctest.Wait()
				if m.creates != 0 || m.updateVersion != 0 {
					t.Fatalf("mutation after cutoff: creates=%d update version=%d", m.creates, m.updateVersion)
				}
				select {
				case err := <-done:
					if err == nil {
						t.Fatal("expected cutoff error")
					}
				default:
					cancel()
					<-done
					t.Fatal("role did not stop after cutoff")
				}
			})
		})
	}
}
