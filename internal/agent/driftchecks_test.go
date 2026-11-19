package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"berthy/internal/deploy"
	"berthy/internal/monitor"
	"berthy/internal/state"
)

func driftStore(t *testing.T) *state.Store {
	t.Helper()
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
	t.Cleanup(func() { s.Close() })
	return s
}

func reporting(report deploy.DriftReport, err error) driftFunc {
	return func(context.Context, string) (deploy.DriftReport, error) { return report, err }
}

func TestDriftCheckWarnsWithTheLinesAndIsOkOnceTheyAreGone(t *testing.T) {
	s := driftStore(t)
	ctx := context.Background()
	lines := []string{"api/web: replicas 3, release says 2", "api/web: env X differs"}
	got := driftResult(ctx, s, reporting(deploy.DriftReport{Release: "v1", Lines: lines}, nil), "api")
	if got.State != monitor.Warning || got.Detail != "api/web: replicas 3, release says 2; api/web: env X differs" {
		t.Fatalf("with drift: %+v", got)
	}
	got = driftResult(ctx, s, reporting(deploy.DriftReport{Release: "v1"}, nil), "api")
	if got.State != monitor.OK || got.Detail != "matches release v1" {
		t.Fatalf("after a deploy: %+v", got)
	}
}

func TestDriftCheckSaysScaledAndStaysOk(t *testing.T) {
	s := driftStore(t)
	got := driftResult(context.Background(), s, reporting(deploy.DriftReport{Release: "v1", Scaled: []string{"api/web: scaled to 3 with berthy scale"}}, nil), "api")
	if got.State != monitor.OK || got.Detail != "matches release v1; api/web: scaled to 3 with berthy scale" {
		t.Fatalf("scaled: %+v", got)
	}
}

func TestDriftCheckIsUnknownWhileAnOperationRunsOnTheApp(t *testing.T) {
	s := driftStore(t)
	ctx := context.Background()
	op, _, err := s.Admit(ctx, state.Admission{RequestID: "deploy", ActorID: "a", Kind: "deploy", App: "api", PublicRequest: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	asked := false
	drift := func(context.Context, string) (deploy.DriftReport, error) {
		asked = true
		return deploy.DriftReport{Lines: []string{"api/web: replicas 3, release says 2"}}, nil
	}
	if got := driftResult(ctx, s, drift, "api"); got.State != monitor.Unknown || asked {
		t.Fatalf("during the operation: %+v, asked %v", got, asked)
	}
	if err = s.Finish(ctx, op.ID, state.Outcome{State: "failed", Err: "x"}); err != nil {
		t.Fatal(err)
	}
	if got := driftResult(ctx, s, drift, "api"); got.State != monitor.Warning {
		t.Fatalf("after the operation: %+v", got)
	}
}

func TestDriftCheckJudgesAnAppWhileAnOperationRunsOnAnotherApp(t *testing.T) {
	s := driftStore(t)
	ctx := context.Background()
	if _, _, err := s.Admit(ctx, state.Admission{RequestID: "deploy", ActorID: "a", Kind: "deploy", App: "other", PublicRequest: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if got := driftResult(ctx, s, reporting(deploy.DriftReport{Release: "v1"}, nil), "api"); got.State != monitor.OK {
		t.Fatalf("%+v", got)
	}
}

func TestDriftCheckIsUnknownWhenDockerCannotBeRead(t *testing.T) {
	s := driftStore(t)
	got := driftResult(context.Background(), s, reporting(deploy.DriftReport{}, errors.New("Docker unavailable")), "api")
	if got.State != monitor.Unknown || !strings.Contains(got.Detail, "Docker unavailable") {
		t.Fatalf("%+v", got)
	}
}

func TestDriftCheckCapsALongDetail(t *testing.T) {
	s := driftStore(t)
	var lines []string
	for range 60 {
		lines = append(lines, "api/web: env SOME_LONG_VARIABLE_NAME differs")
	}
	got := driftResult(context.Background(), s, reporting(deploy.DriftReport{Lines: lines}, nil), "api")
	if got.State != monitor.Warning || len([]rune(got.Detail)) > maxDriftDetail+3 || !strings.HasSuffix(got.Detail, "...") {
		t.Fatalf("%d characters: %s", len(got.Detail), got.Detail)
	}
}
