package deploy

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"berthy/internal/state"
)

// driftController deploys app web and returns the controller with its fake Docker.
func driftController(t *testing.T) (*Controller, *boundFake) {
	t.Helper()
	f := &boundFake{}
	c := &Controller{Engine: f, Store: openStore(t), Environment: "test"}
	redeployWeb(t, c, "first", "a", webWith(map[string]string{"MODE": "fast"}, 1))
	return c, f
}

// editLive changes the service the fake keeps, as a person would with docker service update.
func editLive(t *testing.T, f *boundFake, edit func(spec, container map[string]any)) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.services[0].Spec = edited(t, f.services[0].Spec, edit)
}

func TestDriftOfAFreshDeployIsEmpty(t *testing.T) {
	c, _ := driftController(t)
	got, err := c.Drift(context.Background(), "web")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Lines) != 0 || len(got.Scaled) != 0 || got.Release != "sha256:aaaaaaaaaaaa…" {
		t.Fatalf("report %+v", got)
	}
}

func TestDriftNamesWhatWasChangedByHandWithoutAValue(t *testing.T) {
	c, f := driftController(t)
	editLive(t, f, func(spec, container map[string]any) {
		spec["Mode"] = map[string]any{"Replicated": map[string]any{"Replicas": 5}}
		container["Env"] = append(container["Env"].([]any), "EXTRA=hand-value")
	})
	got, err := c.Drift(context.Background(), "web")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"web/web: replicas 5, release says 1", "web/web: env EXTRA differs"}
	if !reflect.DeepEqual(got.Lines, want) || len(got.Scaled) != 0 {
		t.Fatalf("report %+v", got)
	}
}

func TestDriftReportsARoleScaledWithBerthyScaleAsScaled(t *testing.T) {
	c, f := driftController(t)
	op, _, err := c.Store.Admit(context.Background(), state.Admission{RequestID: "scale", ActorID: "admin", Kind: "scale", App: "web", PublicRequest: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	editLive(t, f, func(spec, container map[string]any) {
		spec["Mode"] = map[string]any{"Replicated": map[string]any{"Replicas": 3}}
		container["Labels"].(map[string]any)["berthy.operation"] = op.ID
	})
	got, err := c.Drift(context.Background(), "web")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Lines) != 0 || !reflect.DeepEqual(got.Scaled, []string{"web/web: scaled to 3 with berthy scale"}) {
		t.Fatalf("report %+v", got)
	}
}

func TestDriftNamesAServiceThatIsGone(t *testing.T) {
	c, f := driftController(t)
	f.mu.Lock()
	f.services = nil
	f.mu.Unlock()
	got, err := c.Drift(context.Background(), "web")
	if err != nil || len(got.Lines) != 1 || !strings.HasPrefix(got.Lines[0], "web/web: service ") || !strings.HasSuffix(got.Lines[0], " is gone") {
		t.Fatalf("report %+v: %v", got, err)
	}
}

func TestDriftIsAnErrorWhenDockerCannotBeRead(t *testing.T) {
	c, f := driftController(t)
	f.mu.Lock()
	f.failures = map[string]error{"proj-web-web": errors.New("daemon down")}
	f.mu.Unlock()
	if _, err := c.Drift(context.Background(), "web"); err == nil {
		t.Fatal("no error")
	}
}

func TestDriftOfAnAppThatIsNotDeployedIsNoRows(t *testing.T) {
	c := &Controller{Engine: &boundFake{}, Store: openStore(t), Environment: "test"}
	if _, err := c.Drift(context.Background(), "web"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("error %v", err)
	}
}

func TestDriftKeepsARoleScaledAfterARestartAsScaled(t *testing.T) {
	c, f := driftController(t)
	ctx := context.Background()
	scale, _, err := c.Store.Admit(ctx, state.Admission{RequestID: "scale", ActorID: "admin", Kind: "scale", App: "web", PublicRequest: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Store.Finish(ctx, scale.ID, state.Outcome{State: "succeeded"}); err != nil {
		t.Fatal(err)
	}
	restart, _, err := c.Store.Admit(ctx, state.Admission{RequestID: "restart", ActorID: "admin", Kind: "restart", App: "web", PublicRequest: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	// The restart keeps the scaled count and labels the service with its own operation.
	editLive(t, f, func(spec, container map[string]any) {
		spec["Mode"] = map[string]any{"Replicated": map[string]any{"Replicas": 3}}
		container["Labels"].(map[string]any)["berthy.operation"] = restart.ID
	})
	got, err := c.Drift(ctx, "web")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Lines) != 0 || !reflect.DeepEqual(got.Scaled, []string{"web/web: scaled to 3 with berthy scale"}) {
		t.Fatalf("report %+v", got)
	}
}
