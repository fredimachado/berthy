package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"berthy/internal/deploy"
	"berthy/internal/engine"
	"berthy/internal/jobs"
	"berthy/internal/manifest"
	"berthy/internal/state"
)

// runEngine is the Docker side of a run that never ends by itself: its container runs until it is stopped or the wait is
// interrupted.
type runEngine struct {
	planEngine
	mu      sync.Mutex
	started chan struct{}
	release chan struct{}
	once    sync.Once
	stopped bool
}

func (e *runEngine) InspectImage(context.Context, string) (engine.Image, error) {
	return engine.Image{ID: "image"}, nil
}
func (e *runEngine) CreateContainer(_ context.Context, name string, _ json.RawMessage) (string, error) {
	return "ctr-" + name, nil
}
func (e *runEngine) StartContainer(context.Context, string) error { return nil }
func (e *runEngine) WaitContainer(ctx context.Context, _ string) (int, error) {
	close(e.started)
	select {
	case <-ctx.Done():
		return 0, ctx.Err()
	case <-e.release:
		return 0, nil
	}
}
func (e *runEngine) ContainerLogs(context.Context, string) ([]byte, error) {
	return []byte("so far\n"), nil
}
func (e *runEngine) InspectContainer(context.Context, string) (engine.ContainerSnapshot, error) {
	return engine.ContainerSnapshot{}, &engine.StatusError{Code: 404}
}
func (e *runEngine) StopContainer(context.Context, string, int) error {
	e.mu.Lock()
	e.stopped = true
	e.mu.Unlock()
	return nil
}
func (e *runEngine) RemoveContainer(context.Context, string) error { return nil }

// runningRun starts a run that does not end, and returns its operation once its container is waiting.
func runningRun(t *testing.T, f *approvalFixture) (string, *runEngine) {
	t.Helper()
	seedRunApp(t, f.s)
	e := &runEngine{started: make(chan struct{}), release: make(chan struct{})}
	f.ctrl.Engine = e
	w := f.call("POST", "/run", apAdminToken, `{"app":"api","command":["sleep","60"],"request_id":"r1"}`)
	var accepted struct {
		ID string `json:"operation_id"`
	}
	if w.Code != http.StatusAccepted || json.Unmarshal(w.Body.Bytes(), &accepted) != nil || accepted.ID == "" {
		t.Fatalf("run: %d %s", w.Code, w.Body.String())
	}
	t.Cleanup(func() {
		e.once.Do(func() { close(e.release) })
		_ = f.ctrl.Drain(context.Background())
	})
	<-e.started
	return accepted.ID, e
}

func (f *approvalFixture) waitTerminal(id string) string {
	f.t.Helper()
	return f.call("GET", "/operations/"+id+"/events", apReadToken, "").Body.String()
}

func TestAnAgentsCancelWithNoBodyIsHeldAndApprovalCarriesItOut(t *testing.T) {
	f := newApprovalFixture(t, true, nil)
	id, _ := runningRun(t, f)
	h := f.hold("POST", "/operations/"+id+"/cancel", apAgentDeploy, "")
	op, err := f.s.Operation(t.Context(), id)
	if err != nil || op.State != "running" {
		t.Errorf("operation after hold: %+v %v", op, err)
	}
	for _, line := range f.audit() {
		if strings.Contains(line, "cancel:"+id) {
			t.Errorf("cancel audited during hold: %s", line)
		}
	}
	w := f.call("POST", "/approvals/"+h.ID+"/approve", apAdminToken, "")
	if w.Code != http.StatusOK {
		t.Fatalf("approve: %d %s", w.Code, w.Body.String())
	}
	if stream := f.waitTerminal(id); !strings.Contains(stream, "cancelled by claude (approved by alice)") {
		t.Fatalf("terminal: %s", stream)
	}
}

func TestAnEmptyBodyIsZeroBytesOrJSONWhiteSpaceOnly(t *testing.T) {
	for _, body := range []string{"", " \r\n\t"} {
		t.Run("cancel/"+body, func(t *testing.T) {
			f := newApprovalFixture(t, false, nil)
			id, _ := runningRun(t, f)
			w := f.call("POST", "/operations/"+id+"/cancel", apAdminToken, body)
			if w.Code != 200 || !strings.Contains(w.Body.String(), `"accepted":true`) {
				t.Fatalf("cancel: %d %s", w.Code, w.Body.String())
			}
			f.waitTerminal(id)
		})
	}
	t.Run("cancel/non-json-space", func(t *testing.T) {
		f := newApprovalFixture(t, false, nil)
		id, _ := runningRun(t, f)
		w := f.call("POST", "/operations/"+id+"/cancel", apAdminToken, "\u00a0")
		op, err := f.s.Operation(t.Context(), id)
		if w.Code != 400 || !strings.Contains(w.Body.String(), `"message":"invalid request"`) || err != nil || op.State != "running" {
			t.Fatalf("cancel: %d %s operation=%+v %v", w.Code, w.Body.String(), op, err)
		}
	})
	t.Run("reject", func(t *testing.T) {
		f := newApprovalFixture(t, true, nil)
		h := f.hold("POST", "/lock/acquire", apAgentDeploy, `{"message":"x"}`)
		w := f.call("POST", "/approvals/"+h.ID+"/reject", apAdminToken, "\u00a0")
		if w.Code != 400 || !strings.Contains(w.Body.String(), `"message":"invalid request"`) || f.status(h.ID) != "pending" {
			t.Fatalf("reject: %d %s status=%s", w.Code, w.Body.String(), f.status(h.ID))
		}
		w = f.call("POST", "/approvals/"+h.ID+"/reject", apAdminToken, "")
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"status":"rejected"`) {
			t.Fatalf("reject empty: %d %s", w.Code, w.Body.String())
		}
	})
}

func (f *approvalFixture) waitEnded(id string) state.Operation {
	f.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		op, err := f.s.Operation(context.Background(), id)
		if err != nil {
			f.t.Fatal(err)
		}
		if op.State != "running" {
			return op
		}
		if time.Now().After(deadline) {
			f.t.Fatal("the operation did not end")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestCancelNeedsTheDeployScope(t *testing.T) {
	f := newApprovalFixture(t, false, nil)
	id, _ := runningRun(t, f)
	path := "/operations/" + id + "/cancel"
	if w := f.call("POST", path, apReadToken, `{}`); w.Code != http.StatusForbidden {
		t.Fatalf("a read token: %d %s", w.Code, w.Body.String())
	}
	if w := f.call("POST", path, "", `{}`); w.Code != http.StatusUnauthorized {
		t.Fatalf("no token: %d", w.Code)
	}
	if w := f.call("GET", path, apDeployToken, ""); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET: %d", w.Code)
	}
	w := f.call("POST", path, apDeployToken, `{}`)
	var plan struct {
		ID       string `json:"operation_id"`
		Kind     string `json:"kind"`
		Accepted bool   `json:"accepted"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &plan) != nil || !plan.Accepted || plan.ID != id || plan.Kind != "run" {
		t.Fatalf("a deploy token: %d %s", w.Code, w.Body.String())
	}
	op := f.waitEnded(id)
	if op.State != "failed" || op.Reason != "cancelled by ci" {
		t.Fatalf("operation %s %q %q", op.State, op.Err, op.Reason)
	}
	if w = f.call("GET", "/operations/"+id, apReadToken, ""); !strings.Contains(w.Body.String(), `"reason":"cancelled by ci"`) || !strings.Contains(w.Body.String(), `"error":"operation cancelled"`) {
		t.Fatalf("what the person sees: %s", w.Body.String())
	}
}

func TestCancelOfARunStopsItsContainerAndIsAudited(t *testing.T) {
	f := newApprovalFixture(t, false, nil)
	id, e := runningRun(t, f)
	if w := f.call("POST", "/operations/"+id+"/cancel", apAdminToken, `{}`); w.Code != http.StatusOK {
		t.Fatalf("cancel: %d %s", w.Code, w.Body.String())
	}
	f.waitEnded(id)
	e.mu.Lock()
	stopped := e.stopped
	e.mu.Unlock()
	if !stopped {
		t.Error("the container was not stopped")
	}
	var found bool
	for _, line := range f.audit() {
		found = found || line == "alice cancel:"+id+" accepted"
	}
	if !found {
		t.Errorf("audit: %v", f.audit())
	}
}

func TestCancelRefusalsAreAConflictWithTheReason(t *testing.T) {
	f := newApprovalFixture(t, false, nil)
	op, _, err := f.s.Admit(context.Background(), state.Admission{RequestID: "d", ActorID: "a", Kind: "destroy", PublicRequest: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	w := f.call("POST", "/operations/"+op.ID+"/cancel", apDeployToken, `{}`)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "a destroy cannot be cancelled") {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if w = f.call("POST", "/operations/nothing/cancel", apDeployToken, `{}`); w.Code != http.StatusNotFound {
		t.Fatalf("unknown operation: %d %s", w.Code, w.Body.String())
	}
	if got, _ := f.s.Operation(context.Background(), op.ID); got.State != "running" {
		t.Errorf("the refusal changed the operation: %s", got.State)
	}
}

func TestCancelDryRunAnswersWithoutCancelling(t *testing.T) {
	f := newApprovalFixture(t, false, nil)
	id, _ := runningRun(t, f)
	w := f.call("POST", "/operations/"+id+"/cancel", apDeployToken, `{"dry_run":true}`)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"accepted":true`) {
		t.Fatalf("dry run: %d %s", w.Code, w.Body.String())
	}
	if got, _ := f.s.Operation(context.Background(), id); got.State != "running" {
		t.Fatalf("the dry run cancelled the operation: %s", got.State)
	}
	if w = f.call("POST", "/operations/"+id+"/cancel", apDeployToken, `{}`); w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}
	f.waitEnded(id)
	for _, line := range f.audit() {
		if strings.Contains(line, "cancel:") && !strings.HasSuffix(line, "accepted") {
			t.Errorf("audit: %s", line)
		}
	}
}

func TestAnAgentTokensCancelOnAProtectedEnvironmentIsHeldAndNothingStops(t *testing.T) {
	f := newApprovalFixture(t, true, nil)
	id, _ := runningRun(t, f)
	h := f.hold("POST", "/operations/"+id+"/cancel", apAgentDeploy, `{}`)
	if !strings.Contains(string(h.Plan), `"accepted":true`) || !strings.Contains(string(h.Plan), id) {
		t.Errorf("the plan a person reviews is the cancel's own dry run: %s", h.Plan)
	}
	a, err := f.s.Approval(context.Background(), h.ID)
	if err != nil || a.Summary != "cancel "+id {
		t.Errorf("approval %+v %v", a, err)
	}
	time.Sleep(50 * time.Millisecond)
	if got, _ := f.s.Operation(context.Background(), id); got.State != "running" {
		t.Fatalf("a held cancel stopped the operation: %s", got.State)
	}
	// A person approves it and the cancel is carried out as the agent that asked.
	if w := f.call("POST", "/approvals/"+h.ID+"/approve", apAdminToken, ""); w.Code != http.StatusOK {
		t.Fatalf("approve: %d %s", w.Code, w.Body.String())
	}
	if op := f.waitEnded(id); op.State != "failed" || op.Reason != "cancelled by claude (approved by alice)" {
		t.Fatalf("operation %s %q", op.State, op.Reason)
	}
}

// The plan of a held cancel is the cancel's own dry run, which says when it would be refused, so the person who reviews it knows.
func TestTheHeldCancelOfAnOperationThatCannotBeCancelledSaysItWouldBeRefused(t *testing.T) {
	f := newApprovalFixture(t, true, nil)
	op, _, err := f.s.Admit(context.Background(), state.Admission{RequestID: "d", ActorID: "a", Kind: "destroy", PublicRequest: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	h := f.hold("POST", "/operations/"+op.ID+"/cancel", apAgentDeploy, `{}`)
	if !strings.Contains(string(h.Plan), `"accepted":false`) || !strings.Contains(string(h.Plan), "a destroy cannot be cancelled") {
		t.Errorf("the plan says it would be refused: %s", h.Plan)
	}
}

func TestJobCancelStopsTheRunningJobAndNeedsTheDeployScope(t *testing.T) {
	s := openIngressStore(t)
	ctx := context.Background()
	recordRelease(t, s, "release-jobs", jobsManifest)
	started := make(chan struct{}, 1)
	exec := jobs.RunnerFunc(func(c context.Context, _ manifest.Manifest, _ string, _ manifest.Job, _ int64) (jobs.Outcome, error) {
		started <- struct{}{}
		<-c.Done()
		return jobs.Outcome{Container: "container-1"}, c.Err()
	})
	sched := jobScheduler(s, exec)
	h := NewDeploymentHandlerWithJobs(Config{Project: "demo", Environment: "lab", Tokens: []Token{{Name: "reader", Digest: Digest(monitoringToken("a")), Scope: "read"}, {Name: "ci", Digest: Digest(monitoringToken("b")), Scope: "deploy"}}}, nil, nil, &deploy.Controller{Store: s, Environment: "lab"}, nil, sched)
	cancelPath := "/v1/projects/demo/environments/lab/jobs/manual/cancel"
	if w := serveRequest(t, h, http.MethodPost, cancelPath, monitoringToken("b")); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "job is not running") {
		t.Fatalf("a job that is not running: %d %s", w.Code, w.Body.String())
	}
	if w := serveRequest(t, h, http.MethodPost, "/v1/projects/demo/environments/lab/jobs/nothing/cancel", monitoringToken("b")); w.Code != http.StatusNotFound {
		t.Fatalf("an unknown job: %d", w.Code)
	}
	if w := serveRequest(t, h, http.MethodPost, "/v1/projects/demo/environments/lab/jobs/manual/run", monitoringToken("b")); w.Code != http.StatusAccepted {
		t.Fatalf("run: %d %s", w.Code, w.Body.String())
	}
	<-started
	if w := serveRequest(t, h, http.MethodPost, cancelPath, monitoringToken("a")); w.Code != http.StatusForbidden {
		t.Fatalf("a read token: %d", w.Code)
	}
	if w := serveRequest(t, h, http.MethodGet, cancelPath, monitoringToken("b")); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET: %d", w.Code)
	}
	if w := serveRequest(t, h, http.MethodPost, cancelPath, monitoringToken("b")); w.Code != http.StatusOK {
		t.Fatalf("cancel: %d %s", w.Code, w.Body.String())
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		runs, _ := s.JobRuns(ctx, "manual", 5)
		if len(runs) == 1 && runs[0].Status != "running" {
			if runs[0].Status != "failed" || runs[0].Detail != "cancelled by ci" {
				t.Fatalf("run %+v", runs[0])
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the job did not stop")
		}
		time.Sleep(5 * time.Millisecond)
	}
	rows, _ := s.AuditLog(ctx, 20)
	var asked bool
	for _, a := range rows {
		asked = asked || a.Action == "job_cancel:manual" && a.ActorName == "ci" && a.Outcome == "accepted"
	}
	if !asked {
		t.Errorf("audit: %+v", rows)
	}
}

func TestHeldJobCancelGetsADescriptionPlan(t *testing.T) {
	f := newApprovalFixture(t, true, nil)
	h := f.hold("POST", "/jobs/nightly/cancel", apAgentDeploy, "")
	a, err := f.s.Approval(context.Background(), h.ID)
	if err != nil || a.Summary != "job cancel nightly" || !sameJSON(t, h.Plan, []byte(`{"action":"job cancel","target":"nightly"}`)) {
		t.Errorf("%+v %s %v", a, h.Plan, err)
	}
}
