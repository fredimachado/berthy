package deploy

import (
	"berthy/internal/engine"
	"berthy/internal/state"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRuntimeChangePreservesSpecAndSetsGeneration(t *testing.T) {
	original := json.RawMessage(`{"Name":"app-web","Labels":{"berthy.app":"app","custom":"keep"},"TaskTemplate":{"ForceUpdate":7,"ContainerSpec":{"Image":"image@sha256:aa","Env":["TOKEN=hidden"],"Labels":{"berthy.app":"app"}}},"Mode":{"Replicated":{"Replicas":2}}}`)
	binding := state.Binding{App: "app", Role: "web", Name: "app-web", ServiceID: "id"}
	desired, n, err := runtimeSpec(original, binding, "stage", "op", "restart", nil)
	if err != nil || n != 2 {
		t.Fatalf("restart: %d %v", n, err)
	}
	var v map[string]any
	if json.Unmarshal(desired, &v) != nil {
		t.Fatal("invalid spec")
	}
	task := v["TaskTemplate"].(map[string]any)
	if task["ForceUpdate"] != float64(8) || task["ContainerSpec"].(map[string]any)["Labels"].(map[string]any)["berthy.operation"] != "op" || v["Labels"].(map[string]any)["custom"] != "keep" {
		t.Fatalf("lost values: %s", desired)
	}
	count := 3
	desired, n, err = runtimeSpec(original, binding, "stage", "op", "scale", &count)
	if err != nil || n != 3 || !json.Valid(desired) {
		t.Fatalf("scale %d %v", n, err)
	}
}
func TestRuntimeRejectsForeignIdentityAndBadCount(t *testing.T) {
	b := state.Binding{App: "app", Role: "web", Name: "name"}
	spec := json.RawMessage(`{"Name":"other","TaskTemplate":{"ContainerSpec":{}},"Mode":{"Replicated":{"Replicas":1}}}`)
	n := 0
	if _, _, err := runtimeSpec(spec, b, "stage", "op", "scale", &n); err == nil {
		t.Fatal("accepted invalid service/count")
	}
}

type runtimeEngine struct {
	Engine
	store   *state.Store
	op      string
	spec    json.RawMessage
	updates int
	fail    bool
}

func (f *runtimeEngine) InspectService(context.Context, string) (engine.ServiceSnapshot, error) {
	return engine.ServiceSnapshot{ID: "id", Version: 9, Spec: f.spec}, nil
}
func (f *runtimeEngine) UpdateService(ctx context.Context, id string, v uint64, spec json.RawMessage, auth *engine.RegistryAuth) error {
	intents, err := f.store.Intents(ctx, f.op)
	if err != nil || len(intents) != 1 || string(intents[0].Before) != string(f.spec) {
		return errors.New("update without durable preimage")
	}
	f.updates++
	if f.fail {
		return errors.New("lost response")
	}
	f.spec = spec
	return nil
}
func (f *runtimeEngine) CreateService(context.Context, json.RawMessage, *engine.RegistryAuth) (string, error) {
	return "", errors.New("unexpected create")
}
func (f *runtimeEngine) DeleteService(context.Context, string) error {
	return errors.New("unexpected delete")
}
func (f *runtimeEngine) ServiceTasks(context.Context, string) ([]engine.ServiceTask, error) {
	return []engine.ServiceTask{{ServiceID: "id", DesiredState: "running", State: "running", Slot: 1, ContainerID: "container", Labels: map[string]string{"berthy.operation": f.op}}}, nil
}
func (f *runtimeEngine) InspectContainerHealth(context.Context, string) (string, error) {
	return "healthy", nil
}
func TestRestartJournalAndRecoveryLock(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint(fail), func(t *testing.T) {
			dir := t.TempDir()
			os.Chmod(dir, 0700)
			key := filepath.Join(dir, "key")
			os.WriteFile(key, make([]byte, 32), 0600)
			s, err := state.Open(dir, key, "stage")
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			ctx := context.Background()
			first, _, err := s.Admit(ctx, state.Admission{RequestID: "first", ActorID: "a", Kind: "deploy", App: "app", PublicRequest: []byte(`{}`)})
			if err != nil {
				t.Fatal(err)
			}
			if err = s.Finish(ctx, first.ID, state.Outcome{State: "succeeded", Release: &state.Release{App: "app", Manifest: []byte(`{}`), Bindings: []state.Binding{{App: "app", Role: "web", Name: "app-web", ServiceID: "id"}}}}); err != nil {
				t.Fatal(err)
			}
			op, _, err := s.Admit(ctx, state.Admission{RequestID: "restart", ActorID: "a", Kind: "restart", App: "app", PublicRequest: []byte(`{}`)})
			if err != nil {
				t.Fatal(err)
			}
			f := &runtimeEngine{store: s, op: op.ID, fail: fail, spec: json.RawMessage(`{"Name":"app-web","TaskTemplate":{"ContainerSpec":{"Image":"example/app:tag"}},"Mode":{"Replicated":{"Replicas":1}}}`)}
			c := Controller{Store: s, Engine: f, Environment: "stage"}
			out := c.runOperation(ctx, op, Actor{}, Request{Kind: "restart", App: "app"})
			want := "succeeded"
			if fail {
				want = "recovery_required"
			}
			if out.State != want || f.updates != 1 {
				t.Fatalf("%s updates=%d", out.State, f.updates)
			}
			if err = s.Finish(ctx, op.ID, out); err != nil {
				t.Fatal(err)
			}
			_, err = s.Active(ctx)
			if fail && err != nil {
				t.Fatal("recovery lock released")
			}
			if !fail && err == nil {
				t.Fatal("completed lock remains")
			}
		})
	}
}
func TestRedactExecOutputBeforeTruncation(t *testing.T) {
	text := strings.Repeat("x", 65530) + "supersecret"
	got := redactOutput([]byte(text), [][]byte{[]byte("supersecret")})
	if strings.Contains(got, "super") || len(got) > 65536 {
		t.Fatal("secret or oversized output")
	}
}
