package deploy

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"berthy/internal/engine"
	"berthy/internal/manifest"
	"berthy/internal/state"
)

// cancelFake is the diagnostics fake with the waits a cancel interrupts: a pull that never ends, a command that runs until it is
// released or its container is stopped, and tasks of the new version that never become healthy.
type cancelFake struct {
	*diagFake
	pullBlocks bool
	pulling    chan struct{}
	pullOnce   sync.Once
	waiting    chan string   // the name of each container whose wait began
	release    chan struct{} // closed to let the commands exit
}

func newCancelFake() *cancelFake {
	return &cancelFake{diagFake: newDiagFake(), pulling: make(chan struct{}), waiting: make(chan string, 16), release: make(chan struct{})}
}
func (f *cancelFake) InspectImage(ctx context.Context, ref string) (engine.Image, error) {
	if f.pullBlocks {
		return engine.Image{}, &engine.StatusError{Code: 404}
	}
	return f.diagFake.InspectImage(ctx, ref)
}
func (f *cancelFake) PullImage(ctx context.Context, _ string, _ *engine.RegistryAuth) error {
	f.pullOnce.Do(func() { close(f.pulling) })
	<-ctx.Done()
	return ctx.Err()
}
func (f *cancelFake) WaitContainer(ctx context.Context, id string) (int, error) {
	f.waiting <- id
	select {
	case <-ctx.Done():
		return 0, ctx.Err()
	case <-f.release:
	}
	return f.diagFake.WaitContainer(ctx, id)
}

func cancelController(t *testing.T, f *cancelFake) (*Controller, *state.Store) {
	t.Helper()
	s := openStore(t)
	return &Controller{rolloutHold: zeroTestRolloutHold(), Engine: f, Store: s, Environment: "test"}, s
}

var cancelAdmin = Actor{ID: "tok-id", Name: "tok", Admin: true}

// deployCancellable admits a deploy of web at the digest made of the character d.
func deployCancellable(t *testing.T, c *Controller, id, d string, m manifest.Manifest) state.Operation {
	t.Helper()
	op, _, err := admit(t, c, context.Background(), Actor{ID: "admin"}, Request{RequestID: id, App: "web", Version: "sha256:" + strings.Repeat(d, 64), Manifest: m})
	if err != nil {
		t.Fatal(err)
	}
	return op
}

func receive(t *testing.T, ch <-chan string) string {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(testWait):
		t.Fatal("the awaited step never began")
	}
	return ""
}

func requireRefused(t *testing.T, c *Controller, id, want string) {
	t.Helper()
	plan, err := c.Cancel(context.Background(), cancelAdmin, id, false)
	var refused CancelRefused
	if !errors.As(err, &refused) || string(refused) != want || plan.Accepted {
		t.Fatalf("cancel: plan %+v, error %v; want a refusal %q", plan, err, want)
	}
}

func requireNothingDeployed(t *testing.T, f *cancelFake, s *state.Store) {
	t.Helper()
	f.positiveEngine.mu.Lock()
	defer f.positiveEngine.mu.Unlock()
	if f.positiveEngine.spec != nil {
		t.Error("a service exists")
	}
	if _, err := s.Applied(context.Background(), "web"); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("a release is applied: %v", err)
	}
	if _, err := s.Active(context.Background()); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("the deploy lock is held: %v", err)
	}
}

func requireCancelled(t *testing.T, got state.Operation, state string) {
	t.Helper()
	if got.State != state || got.Reason != "cancelled by tok" || got.Err != "operation cancelled" || !PublicMessage(got.Err) {
		t.Fatalf("operation %s %q %q, want %s cancelled by tok", got.State, got.Err, got.Reason, state)
	}
	if got.NoReturn != "" {
		t.Errorf("a cancelled operation crossed its point of no return: %q", got.NoReturn)
	}
}

func TestCancelWhilePullingStopsAndChangesNothing(t *testing.T) {
	f := newCancelFake()
	f.pullBlocks = true
	c, s := cancelController(t, f)
	op := deployCancellable(t, c, "d1", "a", webManifest(testDeployTimeout, nil))
	select {
	case <-f.pulling:
	case <-time.After(testWait):
		t.Fatal("the pull never began")
	}
	plan, err := c.Cancel(context.Background(), cancelAdmin, op.ID, false)
	if err != nil || !plan.Accepted || plan.Kind != "deploy" || plan.App != "web" {
		t.Fatalf("cancel: %+v, %v", plan, err)
	}
	requireCancelled(t, waitOperation(t, s, op.ID), "failed")
	requireNothingDeployed(t, f, s)
}

func TestCancelDuringAPreDeployCommandStopsItsContainer(t *testing.T) {
	f := newCancelFake()
	c, s := cancelController(t, f)
	op := deployCancellable(t, c, "d1", "a", webManifest(testDeployTimeout, func(a *manifest.App) {
		a.Hooks.PreDeploy = []manifest.Hook{{Run: []string{"sleep", "60"}}}
	}))
	name := receive(t, f.waiting)
	if _, err := c.Cancel(context.Background(), cancelAdmin, op.ID, false); err != nil {
		t.Fatal(err)
	}
	requireCancelled(t, waitOperation(t, s, op.ID), "failed")
	if f.callIndex("StopContainer:"+name) < 0 {
		t.Errorf("the command's container %s was not stopped: %v", name, f.calls)
	}
	requireNothingDeployed(t, f, s)
}

func TestCancelDuringAMigrationIsRefusedAndTheDeployGoesOn(t *testing.T) {
	f := newCancelFake()
	f.exits, f.versions = []int{0, 0, 0}, []string{"1\n", "2\n"}
	c, s := cancelController(t, f)
	op := deployCancellable(t, c, "d1", "a", webManifest(testDeployTimeout, func(a *manifest.App) {
		a.Migrations = &manifest.Migrations{Version: []string{"version"}, Apply: []string{"apply"}}
	}))
	receive(t, f.waiting)
	requireRefused(t, c, op.ID, "a migration is running")
	close(f.release)
	got := waitOperation(t, s, op.ID)
	if got.State != "succeeded" || got.Reason != "" {
		t.Fatalf("the refused cancel changed the deploy: %s %q %q", got.State, got.Err, got.Reason)
	}
}

func TestCancelAfterTheMigrationStillRestoresWhileWaitingForHealth(t *testing.T) {
	f := newCancelFake()
	f.exits, f.versions = []int{0, 0, 0}, []string{"1\n", "2\n"}
	close(f.release)
	c, s := cancelController(t, f)
	first := deployWeb(t, c, s, "first", "a", webManifest(testDeployTimeout, nil))
	if first.State != "succeeded" {
		t.Fatalf("first deploy: %s %s", first.State, first.Err)
	}
	waiting := make(chan struct{})
	var once sync.Once
	f.tasksFor = func(generation, image string) []engine.ServiceTask {
		if generation == first.ID {
			return []engine.ServiceTask{{ID: "old", ContainerID: "old-c", State: "running", DesiredState: "running", Slot: 1, Labels: map[string]string{"berthy.operation": generation}, Image: image}}
		}
		once.Do(func() { close(waiting) })
		return nil
	}
	op := deployCancellable(t, c, "second", "b", webManifest(testDeployTimeout, func(a *manifest.App) {
		a.Migrations = &manifest.Migrations{Version: []string{"version"}, Apply: []string{"apply"}}
	}))
	select {
	case <-waiting:
	case <-time.After(testWait):
		t.Fatal("the health wait never began")
	}
	if _, err := c.Cancel(context.Background(), cancelAdmin, op.ID, false); err != nil {
		t.Fatalf("a migration that has finished does not refuse a cancel: %v", err)
	}
	got := waitOperation(t, s, op.ID)
	requireCancelled(t, got, "rolled_back")
	if got.SchemaBefore != "1" || got.SchemaAfter != "2" {
		t.Errorf("the schema states of the finished migration are lost: %q %q", got.SchemaBefore, got.SchemaAfter)
	}
}

func TestCancelWhileWaitingForHealthRestoresThePreviousVersion(t *testing.T) {
	f := newCancelFake()
	c, s := cancelController(t, f)
	first := deployWeb(t, c, s, "first", "a", webManifest(testDeployTimeout, nil))
	if first.State != "succeeded" {
		t.Fatalf("first deploy: %s %s", first.State, first.Err)
	}
	waiting := make(chan struct{})
	var once sync.Once
	f.tasksFor = func(generation, image string) []engine.ServiceTask {
		if generation == first.ID {
			return []engine.ServiceTask{{ID: "old", ContainerID: "old-c", State: "running", DesiredState: "running", Slot: 1, Labels: map[string]string{"berthy.operation": generation}, Image: image}}
		}
		once.Do(func() { close(waiting) })
		return nil
	}
	op := deployCancellable(t, c, "second", "b", webManifest(testDeployTimeout, nil))
	select {
	case <-waiting:
	case <-time.After(testWait):
		t.Fatal("the health wait never began")
	}
	if _, err := c.Cancel(context.Background(), cancelAdmin, op.ID, false); err != nil {
		t.Fatal(err)
	}
	requireCancelled(t, waitOperation(t, s, op.ID), "rolled_back")
	f.positiveEngine.mu.Lock()
	spec := string(f.positiveEngine.spec)
	f.positiveEngine.mu.Unlock()
	if !strings.Contains(spec, first.ID) || strings.Contains(spec, op.ID) {
		t.Errorf("the previous version is not back: %s", spec)
	}
	applied, err := s.Applied(context.Background(), "web")
	if err != nil || applied.OperationID != first.ID {
		t.Fatalf("applied %+v, %v", applied, err)
	}
	if _, err = s.Active(context.Background()); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("the deploy lock is held: %v", err)
	}
}

func TestCancelAfterTrafficSwitchedIsRefusedAndChangesNothing(t *testing.T) {
	f := newCancelFake()
	f.exits = []int{0}
	c, s := cancelController(t, f)
	op := deployCancellable(t, c, "d1", "a", webManifest(testDeployTimeout, func(a *manifest.App) {
		a.Hooks.PostDeploy = []manifest.Hook{{Run: []string{"notify"}}}
	}))
	receive(t, f.waiting)
	requireRefused(t, c, op.ID, "the deploy already switched traffic")
	crossed, err := s.Operation(context.Background(), op.ID)
	if err != nil || crossed.State != "running" || crossed.NoReturn != "the deploy already switched traffic" {
		t.Fatalf("operation %+v, %v", crossed, err)
	}
	close(f.release)
	got := waitOperation(t, s, op.ID)
	if got.State != "succeeded" || got.Reason != "" {
		t.Fatalf("the refused cancel changed the deploy: %s %q %q", got.State, got.Err, got.Reason)
	}
	if _, err = s.Applied(context.Background(), "web"); err != nil {
		t.Errorf("the release is not applied: %v", err)
	}
}

func TestCancelAnEndedOperationIsRefused(t *testing.T) {
	f := newCancelFake()
	close(f.release)
	c, s := cancelController(t, f)
	op := deployWeb(t, c, s, "first", "a", webManifest(testDeployTimeout, nil))
	if op.State != "succeeded" {
		t.Fatalf("deploy: %s %s", op.State, op.Err)
	}
	requireRefused(t, c, op.ID, "the operation already ended: succeeded")
	if _, err := c.Cancel(context.Background(), cancelAdmin, "no-such-operation", false); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("an unknown operation: %v", err)
	}
	// A refused cancel is audited, and so is the one that was asked.
	audit, err := s.AuditLog(context.Background(), 50)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range audit {
		if a.Action == "cancel:"+op.ID && a.Outcome == "refused" && a.ActorName == "tok" {
			found = true
		}
	}
	if !found {
		t.Errorf("no audit row for the refused cancel: %+v", audit)
	}
}

func TestCancelRefusesTheKindsThatCannotBeUndone(t *testing.T) {
	for kind, want := range map[string]string{
		"restore":  "a restore cannot be cancelled",
		"remove":   "a remove cannot be cancelled",
		"destroy":  "a destroy cannot be cancelled",
		"exec":     "an exec cannot be cancelled",
		"ingress":  "an ingress cannot be cancelled",
		"restart":  "a restart cannot be cancelled",
		"scale":    "a scale cannot be cancelled",
		"prune":    "a prune cannot be cancelled",
		"deploy-x": "a deploy-x cannot be cancelled",
	} {
		t.Run(kind, func(t *testing.T) {
			s := openStore(t)
			c := &Controller{Store: s, Environment: "test"}
			op, _, err := s.Admit(context.Background(), state.Admission{RequestID: "r", ActorID: "a", Kind: kind, PublicRequest: []byte(`{}`)})
			if err != nil {
				t.Fatal(err)
			}
			requireRefused(t, c, op.ID, want)
			if got, _ := s.Operation(context.Background(), op.ID); got.State != "running" {
				t.Errorf("the refused cancel ended the operation: %s", got.State)
			}
		})
	}
}

func TestCancelDryRunSaysWhetherAndWhyAndChangesNothing(t *testing.T) {
	f := newCancelFake()
	c, s := cancelController(t, f)
	op := deployCancellable(t, c, "d1", "a", webManifest(testDeployTimeout, func(a *manifest.App) {
		a.Hooks.PreDeploy = []manifest.Hook{{Run: []string{"sleep", "60"}}}
	}))
	receive(t, f.waiting)
	plan, err := c.Cancel(context.Background(), cancelAdmin, op.ID, true)
	if err != nil || !plan.Accepted || plan.Reason != "" || plan.OperationID != op.ID {
		t.Fatalf("dry run while a command runs: %+v, %v", plan, err)
	}
	if got, _ := s.Operation(context.Background(), op.ID); got.State != "running" {
		t.Fatalf("the dry run ended the operation: %s", got.State)
	}
	// Only a real cancel ends it; the dry run left it running.
	if _, err = c.Cancel(context.Background(), cancelAdmin, op.ID, false); err != nil {
		t.Fatal(err)
	}
	requireCancelled(t, waitOperation(t, s, op.ID), "failed")

	g := newCancelFake()
	g.exits = []int{0}
	c2, s2 := cancelController(t, g)
	post := deployCancellable(t, c2, "d2", "a", webManifest(testDeployTimeout, func(a *manifest.App) {
		a.Hooks.PostDeploy = []manifest.Hook{{Run: []string{"notify"}}}
	}))
	receive(t, g.waiting)
	plan, err = c2.Cancel(context.Background(), cancelAdmin, post.ID, true)
	if err != nil || plan.Accepted || plan.Reason != "the deploy already switched traffic" {
		t.Fatalf("dry run after the switch: %+v, %v", plan, err)
	}
	close(g.release)
	if got := waitOperation(t, s2, post.ID); got.State != "succeeded" {
		t.Fatalf("operation %s %q", got.State, got.Err)
	}
	for _, a := range auditRows(t, s) {
		if a.Action == "cancel:"+post.ID {
			t.Errorf("a dry run was audited: %+v", a)
		}
	}
}

func auditRows(t *testing.T, s *state.Store) []state.Audit {
	t.Helper()
	rows, err := s.AuditLog(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func TestCancelAcceptedIsAuditedWithTheTokenName(t *testing.T) {
	f := newCancelFake()
	c, s := cancelController(t, f)
	op := deployCancellable(t, c, "d1", "a", webManifest(testDeployTimeout, func(a *manifest.App) {
		a.Hooks.PreDeploy = []manifest.Hook{{Run: []string{"sleep", "60"}}}
	}))
	receive(t, f.waiting)
	if _, err := c.Cancel(context.Background(), cancelAdmin, op.ID, false); err != nil {
		t.Fatal(err)
	}
	waitOperation(t, s, op.ID)
	found := false
	for _, a := range auditRows(t, s) {
		if a.Action == "cancel:"+op.ID && a.Outcome == "accepted" && a.ActorName == "tok" {
			found = true
		}
	}
	if !found {
		t.Errorf("no audit row: %+v", auditRows(t, s))
	}
}

// A second cancel of an operation that is already being cancelled is accepted and changes nothing more.
func TestCancelTwiceIsHarmless(t *testing.T) {
	f := newCancelFake()
	c, s := cancelController(t, f)
	op := deployCancellable(t, c, "d1", "a", webManifest(testDeployTimeout, func(a *manifest.App) {
		a.Hooks.PreDeploy = []manifest.Hook{{Run: []string{"sleep", "60"}}}
	}))
	receive(t, f.waiting)
	for range 2 {
		if _, err := c.Cancel(context.Background(), cancelAdmin, op.ID, false); err != nil {
			if got := waitOperation(t, s, op.ID); got.State == "failed" {
				break
			}
			t.Fatal(err)
		}
	}
	requireCancelled(t, waitOperation(t, s, op.ID), "failed")
}

// The cancel races the step that crosses the point of no return. Whichever the operation learns first decides, and the two
// never disagree: an accepted cancel ends the operation as failed or rolled_back with no release, and a refused one leaves a
// deploy that succeeds.
func TestCancelRacesThePointOfNoReturn(t *testing.T) {
	f := newCancelFake()
	close(f.release)
	c, s := cancelController(t, f)
	first := deployWeb(t, c, s, "first", "a", webManifest(testDeployTimeout, nil))
	if first.State != "succeeded" {
		t.Fatalf("first deploy: %s %s", first.State, first.Err)
	}
	// How long a deploy takes, so the rounds that follow cancel at every point of it.
	started := time.Now()
	if op := deployWeb(t, c, s, "calibrate", "b", webManifest(testDeployTimeout, nil)); op.State != "succeeded" {
		t.Fatalf("calibration deploy: %s %s", op.State, op.Err)
	}
	took := time.Since(started)
	var accepted, refused atomic.Int32
	for i := range 60 {
		before, err := s.Applied(context.Background(), "web")
		if err != nil {
			t.Fatal(err)
		}
		// Each round deploys a digest the app does not run, so a deploy that succeeds is told from one that was cancelled.
		op := deployCancellable(t, c, "race-"+strconv.Itoa(i), hexDigit(i+1), webManifest(testDeployTimeout, nil))
		time.Sleep(took * time.Duration(i) / 30)
		plan, cancelErr := c.Cancel(context.Background(), cancelAdmin, op.ID, false)
		got := waitOperation(t, s, op.ID)
		after, err := s.Applied(context.Background(), "web")
		if err != nil {
			t.Fatal(err)
		}
		switch {
		case cancelErr == nil && plan.Accepted:
			accepted.Add(1)
			if got.State != "failed" && got.State != "rolled_back" || got.Reason != "cancelled by tok" || got.NoReturn != "" {
				t.Fatalf("round %d: an accepted cancel ended as %s %q %q crossed %q", i, got.State, got.Err, got.Reason, got.NoReturn)
			}
			if after.ID != before.ID {
				t.Fatalf("round %d: an accepted cancel applied release %d", i, after.ID)
			}
		default:
			refused.Add(1)
			var r CancelRefused
			if !errors.As(cancelErr, &r) {
				t.Fatalf("round %d: %+v, %v", i, plan, cancelErr)
			}
			if got.State != "succeeded" || got.Reason != "" || after.ID == before.ID {
				t.Fatalf("round %d: a refused cancel (%s) left %s %q, release %d -> %d", i, r, got.State, got.Reason, before.ID, after.ID)
			}
		}
	}
	t.Logf("accepted %d, refused %d", accepted.Load(), refused.Load())
}

func hexDigit(i int) string { return string("0123456789abcdef"[i%16]) }

func TestCancelAPendingRunStopsItsContainer(t *testing.T) {
	c, f, s := runFixture(t, runManifest("main"))
	f.block, f.logs = true, []byte("partial\n")
	op, _, err := admit(t, c, context.Background(), runAdmin, runRequest("r1", "sleep", "60"))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(testWait)
	for !strings.Contains(f.callList(), "wait") {
		if time.Now().After(deadline) {
			t.Fatal("the run never began")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err = c.Cancel(context.Background(), cancelAdmin, op.ID, false); err != nil {
		t.Fatal(err)
	}
	got := waitOperation(t, s, op.ID)
	if got.State != "failed" || got.Reason != "cancelled by tok" || got.Err != "operation cancelled" {
		t.Fatalf("outcome %s %q %q", got.State, got.Err, got.Reason)
	}
	calls := f.callList()
	if !strings.Contains(calls, "stop") || !strings.HasSuffix(calls, "logs,remove") {
		t.Errorf("calls %s: the container is stopped and then removed once its output is kept", calls)
	}
	if output, _ := runOutput(t, s, op.ID); output != "partial\n" {
		t.Errorf("output %q", output)
	}
	if _, err = s.Active(context.Background()); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("the lock is held: %v", err)
	}
}

// A Compose deploy has no compensation, so its point of no return is the first change to the project: once up has begun a cancel
// is refused, and the deploy goes on to its end.
func TestCancelOfAComposeDeployIsRefusedOnceTheProjectChanges(t *testing.T) {
	runner := &fakeCompose{config: configOne, hashes: hashOne, upStarted: make(chan struct{}), upRelease: make(chan struct{})}
	c, s := composeController(t, runner, newComposeDocker())
	op, _, err := admit(t, c, context.Background(), Actor{ID: "ci", Name: "ci"}, composeReq("one", composeFileOne, composeApp(true)))
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-runner.upStarted:
	case <-time.After(testWait):
		t.Fatal("up never began")
	}
	requireRefused(t, c, op.ID, "the Compose project is being changed")
	close(runner.upRelease)
	if got := waitComposeOperation(t, s, op.ID); got.State != "succeeded" || got.NoReturn != "the Compose project is being changed" {
		t.Fatalf("operation %s %q crossed %q", got.State, got.Err, got.NoReturn)
	}
}
