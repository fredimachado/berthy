package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"testing/synctest"

	"berthy/internal/deploy"
	"berthy/internal/engine"
	"berthy/internal/manifest"
	"berthy/internal/state"
)

type finalReasonEngine struct {
	planEngine
	mu        sync.Mutex
	store     *state.Store
	spec      json.RawMessage
	triggered bool
	foreign   bool
}

func (f *finalReasonEngine) InspectService(ctx context.Context, _ string) (engine.ServiceSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.spec == nil {
		return engine.ServiceSnapshot{}, &engine.StatusError{Code: 404}
	}
	if !f.triggered {
		op, err := f.store.Active(ctx)
		if err == nil {
			intents, err := f.store.Intents(ctx, op.ID)
			if err == nil {
				for _, i := range intents {
					if i.Kind == "create" && i.Phase == "verified" {
						f.triggered = true
						return engine.ServiceSnapshot{}, errors.New("injected final inspection failure")
					}
				}
			}
		}
	}
	if f.triggered && f.foreign {
		return engine.ServiceSnapshot{ID: "foreign", Spec: json.RawMessage(`{"Name":"foreign"}`)}, nil
	}
	return engine.ServiceSnapshot{ID: "id", Spec: append(json.RawMessage(nil), f.spec...)}, nil
}

func (f *finalReasonEngine) CreateService(_ context.Context, spec json.RawMessage, _ *engine.RegistryAuth) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.spec = append(json.RawMessage(nil), spec...)
	return "id", nil
}

func (f *finalReasonEngine) UpdateService(context.Context, string, uint64, json.RawMessage, *engine.RegistryAuth) error {
	return errors.New("unexpected update")
}

func (f *finalReasonEngine) DeleteService(context.Context, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.spec = nil
	return nil
}

func (f *finalReasonEngine) ResolveImage(context.Context, string, *engine.RegistryAuth) (string, error) {
	return "", errors.New("unexpected image resolution")
}
func (f *finalReasonEngine) PullImage(context.Context, string, *engine.RegistryAuth) error {
	return nil
}
func (f *finalReasonEngine) InspectImage(context.Context, string) (engine.Image, error) {
	return engine.Image{ID: "image"}, nil
}

func (f *finalReasonEngine) ServiceTasks(context.Context, string) ([]engine.ServiceTask, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.triggered || f.spec == nil {
		return nil, nil
	}
	var spec struct {
		TaskTemplate struct {
			ContainerSpec struct{ Labels map[string]string }
		}
	}
	if err := json.Unmarshal(f.spec, &spec); err != nil {
		return nil, err
	}
	return []engine.ServiceTask{{ServiceID: "id", ContainerID: "container", State: "running", DesiredState: "running", Slot: 1, Labels: spec.TaskTemplate.ContainerSpec.Labels}}, nil
}

func (f *finalReasonEngine) InspectContainerHealth(context.Context, string) (string, error) {
	return "healthy", nil
}

func TestOperationExposesFinalVerificationReason(t *testing.T) {
	for _, tc := range []struct {
		name, state, operationError string
		foreign                     bool
	}{
		{"confirmed cleanup", "failed", "rollout failed and was cleaned up", false},
		{"unconfirmed cleanup", "recovery_required", "rollout recovery required", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := openIngressStore(t)
			f := &finalReasonEngine{store: s, foreign: tc.foreign}
			c := &deploy.Controller{Store: s, Engine: f, Environment: "lab"}
			one := 1
			m := manifest.Manifest{Project: "demo", Network: "overlay", Apps: map[string]manifest.App{"web": {Kind: "image", Image: "example/web", Roles: map[string]manifest.Role{"web": {Replicas: &one, Health: &manifest.Health{Test: []string{"CMD", "true"}}}}}}}
			synctest.Test(t, func(t *testing.T) {
				ctx := context.Background()
				op, _, err := c.Admit(ctx, deploy.Actor{ID: "a", Admin: true}, deploy.Request{RequestID: "candidate", App: "web", Version: "sha256:" + strings.Repeat("a", 64), Manifest: m})
				if err != nil {
					t.Fatal(err)
				}
				defer func() {
					if err := c.Drain(ctx); err != nil {
						t.Error(err)
					}
				}()
				if err := c.Drain(ctx); err != nil {
					t.Fatal(err)
				}
				got, err := s.Operation(ctx, op.ID)
				if err != nil || !f.triggered || got.State != tc.state {
					t.Fatalf("operation %+v, final inspection reached=%v, error=%v", got, f.triggered, err)
				}
				body, err := json.Marshal(publicOperation(got))
				if err != nil {
					t.Fatal(err)
				}
				var public struct {
					Reason string `json:"reason"`
					Error  string `json:"error"`
				}
				if err := json.Unmarshal(body, &public); err != nil {
					t.Fatal(err)
				}
				if public.Reason != "final verification unavailable" || public.Error != tc.operationError {
					t.Fatalf("operation JSON = %s; want reason %q and error %q", body, "final verification unavailable", tc.operationError)
				}
			})
		})
	}
}
