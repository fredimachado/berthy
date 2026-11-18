package deploy

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"berthy/internal/manifest"
	"berthy/internal/state"
)

// recordingObserver keeps every event it hears, in order.
type recordingObserver struct {
	mu      sync.Mutex
	started []OperationEvent
	ended   []OperationEvent
}

func (r *recordingObserver) OperationStarted(_ context.Context, e OperationEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.started = append(r.started, e)
}

func (r *recordingObserver) OperationEnded(_ context.Context, e OperationEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ended = append(r.ended, e)
}

// waitEnded returns the first n end events, waiting for them: the observer is told just after the state is written.
func (r *recordingObserver) waitEnded(t *testing.T, n int) []OperationEvent {
	t.Helper()
	deadline := time.Now().Add(testWait)
	for {
		r.mu.Lock()
		got := append([]OperationEvent(nil), r.ended...)
		r.mu.Unlock()
		if len(got) >= n {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("observer heard %d ends, want %d", len(got), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (r *recordingObserver) starts() []OperationEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]OperationEvent(nil), r.started...)
}

func TestObserverHearsASuccessfulDeploy(t *testing.T) {
	s := openStore(t)
	digest := "sha256:" + strings.Repeat("c", 64)
	obs := &recordingObserver{}
	c := &Controller{Engine: tagResolver{Engine: &boundFake{}, digest: digest}, Store: s, Environment: "prod", Observer: obs}
	op, _, err := admit(t, c, context.Background(), Actor{ID: "id-ci", Name: "ci"}, Request{RequestID: "ok", App: "web", Version: "v1.4.2", Manifest: dockerManifest(nil)})
	if err != nil {
		t.Fatal(err)
	}
	if got := waitOperation(t, s, op.ID); got.State != "succeeded" {
		t.Fatalf("state %s: %s", got.State, got.Err)
	}
	ends := obs.waitEnded(t, 1)
	starts := obs.starts()
	if len(starts) != 1 || len(ends) != 1 {
		t.Fatalf("starts %d ends %d", len(starts), len(ends))
	}
	start, end := starts[0], ends[0]
	if start.OperationID != op.ID || start.State != "running" || start.Kind != "deploy" || start.App != "web" || start.Environment != "prod" || start.Actor != "ci" || start.Version != "v1.4.2" || !start.Ended.IsZero() || start.Started.IsZero() {
		t.Fatalf("start %+v", start)
	}
	if end.OperationID != op.ID || end.State != "succeeded" || end.Version != "v1.4.2" || end.Digest != digest || end.Actor != "ci" || end.Reason != "" || end.Resumed {
		t.Fatalf("end %+v", end)
	}
	if !end.Started.Equal(start.Started) || end.Ended.Before(end.Started) || end.Manifest == nil || end.Manifest.Project != "proj" {
		t.Fatalf("end times or manifest: %+v", end)
	}
}

func TestObserverHearsADeployByDigestWithTheShortDigestAsItsVersion(t *testing.T) {
	s := openStore(t)
	digest := "sha256:" + strings.Repeat("d", 64)
	obs := &recordingObserver{}
	c := &Controller{Engine: &boundFake{}, Store: s, Environment: "prod", Observer: obs}
	runBound(t, s, c, Request{RequestID: "by-digest", App: "web", Version: digest, Manifest: dockerManifest(nil)})
	end := obs.waitEnded(t, 1)[0]
	if end.Version != "sha256:"+strings.Repeat("d", 12) || end.Digest != digest {
		t.Fatalf("end %+v", end)
	}
}

func TestObserverHearsAFailedDeployWithItsPublicReason(t *testing.T) {
	s := openStore(t)
	obs := &recordingObserver{}
	one := 1
	m := manifest.Manifest{Project: "proj", Network: "overlay", Apps: map[string]manifest.App{"web": {Kind: "image", Image: "example/web", Deploy: manifest.Deploy{DeployTimeout: "100ms", DrainTimeout: "1s"}, Roles: map[string]manifest.Role{"web": {Replicas: &one, Health: &manifest.Health{Test: []string{"CMD", "true"}}}}}}}
	c := &Controller{Engine: &positiveEngine{unhealthy: true}, Store: s, Environment: "prod", Observer: obs}
	op, _, err := admit(t, c, context.Background(), Actor{ID: "id-ci", Name: "ci"}, Request{RequestID: "bad", App: "web", Version: "sha256:" + strings.Repeat("a", 64), Manifest: m})
	if err != nil {
		t.Fatal(err)
	}
	got := waitOperation(t, s, op.ID)
	end := obs.waitEnded(t, 1)[0]
	if end.State != got.State || (end.State != "failed" && end.State != "rolled_back") {
		t.Fatalf("event state %s, operation %s", end.State, got.State)
	}
	if end.Reason != got.Reason || end.Err != got.Err || end.Err == "" {
		t.Fatalf("event reason %q err %q, operation reason %q err %q", end.Reason, end.Err, got.Reason, got.Err)
	}
	if end.Digest != "sha256:"+strings.Repeat("a", 64) {
		t.Fatalf("digest %q", end.Digest)
	}
}

func TestObserverHearsARollbackWithTheVersionItReturnsTo(t *testing.T) {
	s := openStore(t)
	seedTaggedRelease(t, s, "older", "sha256:"+strings.Repeat("a", 64), "v1.0.1")
	seedTaggedRelease(t, s, "current", "sha256:"+strings.Repeat("b", 64), "v1.0.2", boundBinding("legacy-web", "old-id"))
	obs := &recordingObserver{}
	c := &Controller{Engine: &boundFake{}, Store: s, Environment: "prod", Observer: obs}
	op := runBound(t, s, c, Request{RequestID: "rollback", App: "web", Kind: "rollback"})
	if op.State != "succeeded" {
		t.Fatalf("state %s: %s", op.State, op.Err)
	}
	end := obs.waitEnded(t, 1)[0]
	start := obs.starts()[0]
	if start.Kind != "rollback" || end.Kind != "rollback" || end.State != "succeeded" || end.Version != "v1.0.1" || end.Digest != "sha256:"+strings.Repeat("a", 64) {
		t.Fatalf("start %+v end %+v", start, end)
	}
	if end.Manifest != nil {
		t.Fatalf("a rollback is judged by the environment's settings, not by the release it returns to: %+v", end.Manifest)
	}
}

func TestObserverHearsEveryKindOfOperation(t *testing.T) {
	s := openStore(t)
	obs := &recordingObserver{}
	c := &Controller{Engine: &boundFake{}, Store: s, Environment: "prod", Observer: obs}
	// Nothing is deployed, so the restart ends as failed: its start and end are still told.
	op := runBound(t, s, c, Request{RequestID: "restart", App: "web", Kind: "restart", Manifest: dockerManifest(nil)})
	ends := obs.waitEnded(t, 1)
	starts := obs.starts()
	if len(starts) != 1 || starts[0].Kind != "restart" || starts[0].OperationID != op.ID || starts[0].State != "running" {
		t.Fatalf("starts %+v", starts)
	}
	if ends[0].Kind != "restart" || ends[0].OperationID != op.ID || ends[0].State != op.State || ends[0].Manifest != nil {
		t.Fatalf("end %+v, operation %+v", ends[0], op)
	}
}

func TestObserverHearsAnOperationThatRecoveryInterrupted(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	op, _, err := s.Admit(ctx, state.Admission{RequestID: "r", ActorID: "a", ActorName: "ci", Kind: "deploy", App: "web", PublicRequest: json.RawMessage(`{"version":"v2"}`)})
	if err != nil {
		t.Fatal(err)
	}
	spec := json.RawMessage(`{"Name":"svc"}`)
	if err = s.RecordIntent(ctx, op.ID, state.Intent{Kind: "create", Name: "svc", Desired: spec}); err != nil {
		t.Fatal(err)
	}
	if err = s.MarkStep(ctx, op.ID, 0, "observed", json.RawMessage(`{"id":"id","spec":{"Name":"svc"}}`)); err != nil {
		t.Fatal(err)
	}
	obs := &recordingObserver{}
	c := &Controller{Engine: &positiveEngine{spec: spec}, Store: s, Environment: "prod", Observer: obs}
	if err = c.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	ends := obs.waitEnded(t, 1)
	e := ends[0]
	if len(ends) != 1 || e.OperationID != op.ID || e.State != "interrupted" || e.Actor != "ci" || e.Version != "v2" || e.Resumed || e.Manifest != nil || e.Err == "" {
		t.Fatalf("ends %+v", ends)
	}
	if len(obs.starts()) != 0 {
		t.Fatal("recovery told the observer of a start: the process that admitted the operation did")
	}
}

// An operation that ends as recovery_required and is later decided by a person is told twice, and the second end says it
// resumes the first, so the alert channel stays quiet.
func TestObserverMarksTheLaterEndOfARecoveryRequiredOperation(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	op, _, err := s.Admit(ctx, state.Admission{RequestID: "r", ActorID: "a", Kind: "deploy", App: "web", PublicRequest: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RecordIntent(ctx, op.ID, state.Intent{Kind: "update", Name: "svc", ServiceID: "id", Before: json.RawMessage(`{"Name":"old"}`), Desired: json.RawMessage(`{"Name":"new"}`)}); err != nil {
		t.Fatal(err)
	}
	obs := &recordingObserver{}
	e := &positiveEngine{spec: json.RawMessage(`{"Name":"foreign"}`)}
	c := &Controller{Engine: e, Store: s, Environment: "prod", Observer: obs}
	if err = c.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if first := obs.waitEnded(t, 1)[0]; first.State != "recovery_required" || first.Resumed {
		t.Fatalf("first end %+v", first)
	}
	if _, err = c.ResolveRecovery(ctx, Actor{ID: "admin", Name: "admin", Admin: true}, op.ID, "service removed", false); err != nil {
		t.Fatal(err)
	}
	ends := obs.waitEnded(t, 2)
	if ends[1].State != "failed" || !ends[1].Resumed || ends[1].OperationID != op.ID || ends[1].Err != "recovery resolved by admin" {
		t.Fatalf("second end %+v", ends[1])
	}
}

func TestRecoveryThatLaterFinishesARecoveryRequiredOperationIsMarkedResumed(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	op, _, err := s.Admit(ctx, state.Admission{RequestID: "r", ActorID: "a", Kind: "deploy", App: "web", PublicRequest: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	spec := json.RawMessage(`{"Name":"svc"}`)
	if err = s.RecordIntent(ctx, op.ID, state.Intent{Kind: "create", Name: "svc", Desired: spec}); err != nil {
		t.Fatal(err)
	}
	if err = s.MarkStep(ctx, op.ID, 0, "observed", json.RawMessage(`{"id":"id","spec":{"Name":"svc"}}`)); err != nil {
		t.Fatal(err)
	}
	// An earlier recovery attempt gave up.
	if err = s.Finish(ctx, op.ID, state.Outcome{State: "recovery_required", Err: "service recovery required"}); err != nil {
		t.Fatal(err)
	}
	obs := &recordingObserver{}
	c := &Controller{Engine: &positiveEngine{spec: spec}, Store: s, Environment: "prod", Observer: obs}
	if err = c.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if e := obs.waitEnded(t, 1)[0]; e.State != "interrupted" || !e.Resumed {
		t.Fatalf("end %+v", e)
	}
}

func TestAnUnwrittenFinishIsNotReported(t *testing.T) {
	s := openStore(t)
	obs := &recordingObserver{}
	c := &Controller{Store: s, Observer: obs}
	// The operation does not hold the lock, so the store refuses the write.
	if err := c.finish(context.Background(), state.Operation{ID: "unknown"}, state.Outcome{State: "failed"}, target{}); err == nil {
		t.Fatal("finish of an unknown operation succeeded")
	}
	obs.mu.Lock()
	defer obs.mu.Unlock()
	if len(obs.ended) != 0 {
		t.Fatalf("observer heard %+v", obs.ended)
	}
}

func TestVersionParts(t *testing.T) {
	d := "sha256:" + strings.Repeat("e", 64)
	for _, tc := range []struct{ in, tag, digest string }{
		{"", "", ""},
		{"v1.4.2", "v1.4.2", ""},
		{d, "", d},
		{"example/web@" + d, "", d},
		{"example/web:v3", "v3", ""},
		{"example/web:v3@" + d, "v3", d},
	} {
		tag, digest := versionParts(tc.in)
		if tag != tc.tag || digest != tc.digest {
			t.Errorf("versionParts(%q) = %q, %q; want %q, %q", tc.in, tag, digest, tc.tag, tc.digest)
		}
	}
}

// Every terminal write of an operation goes through finish, which tells the observer. A behavioural test cannot see a write
// that skips it, so this one reads the package's sources.
func TestNoTerminalWriteBypassesFinish(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for n, line := range strings.Split(string(b), "\n") {
			if strings.Contains(line, "Store.Finish(") && !(f == "observer.go" && strings.Contains(line, "c.Store.Finish(ctx, op.ID, o)")) {
				t.Errorf("%s:%d writes an operation's end without finish: %s", f, n+1, strings.TrimSpace(line))
			}
			if strings.Contains(line, "Store.ResolveRecovery(") && f != "resolve.go" {
				t.Errorf("%s:%d resolves recovery outside ResolveRecovery", f, n+1)
			}
		}
	}
}
