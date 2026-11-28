package monitor

import (
	"context"
	"testing"
	"time"

	"berthy/internal/api"
)

var downT0 = time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)

// want is a desired replica count, as a Swarm service spec holds it.
func want(n uint64) *uint64 { return &n }

// downRole returns one app with one role. A nil desired count is a Compose service.
func downRole(ref api.AppRef, role string, desired *uint64, replicas ...api.Replica) api.App {
	return api.App{Ref: ref, Roles: []api.Role{{Name: role, Desired: desired, Replicas: replicas}}}
}

func replica(id, state, health string) api.Replica {
	return api.Replica{ContainerID: id, State: state, Health: health}
}

// exitedWith returns a stopped Compose container that exited with code.
func exitedWith(id string, code int) api.Replica {
	return api.Replica{ContainerID: id, State: "exited", Health: "none", ExitCode: &code}
}

// expectDown runs check and compares its state and detail.
func expectDown(t *testing.T, check Check, state, detail string) {
	t.Helper()
	got := check.Run(context.Background())
	if got.State != state || got.Detail != detail {
		t.Fatalf("%s: got %s %q, want %s %q", check.Name(), got.State, got.Detail, state, detail)
	}
}

func TestDownCheckIsCriticalOnlyAfterTwoMinutes(t *testing.T) {
	ref := api.AppRef{Kind: "image", Name: "api"}
	var now time.Time
	w := NewDownWatch(func() time.Time { return now })
	check := w.Check(ref, "web")
	short := []api.App{downRole(ref, "web", want(2))}

	now = downT0
	w.Observe(now, short, nil)
	expectDown(t, check, Unknown, "0 of 2 replicas running, not yet confirmed")

	now = downT0.Add(119 * time.Second)
	w.Observe(now, short, nil)
	expectDown(t, check, Unknown, "0 of 2 replicas running, not yet confirmed")

	now = downT0.Add(2 * time.Minute)
	w.Observe(now, short, nil)
	expectDown(t, check, Critical, "0 of 2 replicas running for 2m")
}

func TestDownCheckRecoversWhenTheReplicasRun(t *testing.T) {
	ref := api.AppRef{Kind: "image", Name: "api"}
	var now time.Time
	w := NewDownWatch(func() time.Time { return now })
	check := w.Check(ref, "web")
	now = downT0
	w.Observe(now, []api.App{downRole(ref, "web", want(2))}, nil)
	now = downT0.Add(3 * time.Minute)
	w.Observe(now, []api.App{downRole(ref, "web", want(2), replica("a", "running", "unknown"), replica("b", "running", "unknown"))}, nil)
	expectDown(t, check, OK, "2 of 2 replicas running")
}

func TestDownCheckIsOKForAScaleToZero(t *testing.T) {
	ref := api.AppRef{Kind: "image", Name: "api"}
	var now time.Time
	w := NewDownWatch(func() time.Time { return now })
	check := w.Check(ref, "web")
	now = downT0.Add(5 * time.Minute)
	w.Observe(now, []api.App{downRole(ref, "web", want(0))}, nil)
	expectDown(t, check, OK, "no replicas wanted")
}

func TestDownCheckIsUnknownDuringAnOperationAndStartsAgain(t *testing.T) {
	ref := api.AppRef{Kind: "image", Name: "api"}
	var now time.Time
	w := NewDownWatch(func() time.Time { return now })
	check := w.Check(ref, "web")
	short := []api.App{downRole(ref, "web", want(2))}
	quiet := func(r api.AppRef) bool { return r == ref }

	now = downT0
	w.Observe(now, short, nil)
	now = downT0.Add(time.Minute)
	w.Observe(now, short, quiet)
	expectDown(t, check, Unknown, "operation on api running or just ended")

	now = downT0.Add(2 * time.Minute)
	w.Observe(now, short, quiet)
	expectDown(t, check, Unknown, "operation on api running or just ended")

	// The operation has ended. The two minutes of confirmation count from now, not from before the operation.
	now = downT0.Add(3 * time.Minute)
	w.Observe(now, short, nil)
	expectDown(t, check, Unknown, "0 of 2 replicas running, not yet confirmed")
	now = downT0.Add(5 * time.Minute)
	w.Observe(now, short, nil)
	expectDown(t, check, Critical, "0 of 2 replicas running for 2m")
}

func TestDownCheckNamesAnUnhealthyComposeService(t *testing.T) {
	ref := api.AppRef{Kind: "compose", Name: "shop-db"}
	var now time.Time
	w := NewDownWatch(func() time.Time { return now })
	check := w.Check(ref, "db")
	// Two containers of the service: one runs healthy, the other runs unhealthy.
	apps := []api.App{downRole(ref, "db", nil, replica("c1", "running", "healthy"), replica("c2", "running", "unhealthy"))}
	now = downT0
	w.Observe(now, apps, nil)
	now = downT0.Add(2 * time.Minute)
	w.Observe(now, apps, nil)
	expectDown(t, check, Critical, "1 of 2 replicas healthy (1 unhealthy) for 2m")
}

func TestDownCheckSaysAStoppedSingleContainerExited(t *testing.T) {
	ref := api.AppRef{Kind: "compose", Name: "shop-db"}
	var now time.Time
	w := NewDownWatch(func() time.Time { return now })
	check := w.Check(ref, "db")
	stopped := []api.App{downRole(ref, "db", nil, replica("c1", "exited", "none"))}
	now = downT0
	w.Observe(now, stopped, nil)
	now = downT0.Add(2 * time.Minute)
	w.Observe(now, stopped, nil)
	expectDown(t, check, Critical, "container exited for 2m")
}

func TestDownCheckCountsAVanishedComposeServiceAsStopped(t *testing.T) {
	ref := api.AppRef{Kind: "compose", Name: "shop-db"}
	var now time.Time
	w := NewDownWatch(func() time.Time { return now })
	check := w.Check(ref, "db")
	now = downT0
	w.Observe(now, []api.App{downRole(ref, "db", nil, replica("c1", "running", "none"), replica("c2", "running", "none"))}, nil)
	// The service has no containers any more, and neither has the project: both count as stopped, two wanted.
	now = downT0.Add(time.Minute)
	w.Observe(now, []api.App{{Ref: ref}}, nil)
	now = downT0.Add(3 * time.Minute)
	w.Observe(now, []api.App{{Ref: ref}}, nil)
	expectDown(t, check, Critical, "0 of 2 replicas running for 2m")
}

func TestDownCheckForgetsAVanishedServiceDuringAnOperation(t *testing.T) {
	ref := api.AppRef{Kind: "compose", Name: "shop-db"}
	var now time.Time
	w := NewDownWatch(func() time.Time { return now })
	check := w.Check(ref, "db")
	now = downT0
	w.Observe(now, []api.App{downRole(ref, "db", nil, replica("c1", "running", "none"))}, nil)
	// A remove of the service runs. Once it has ended, nothing is expected of the service any more.
	quiet := func(r api.AppRef) bool { return r == ref }
	now = downT0.Add(time.Minute)
	w.Observe(now, []api.App{{Ref: ref}}, quiet)
	now = downT0.Add(5 * time.Minute)
	w.Observe(now, []api.App{{Ref: ref}}, nil)
	expectDown(t, check, OK, "no replicas")
}

func TestDownCheckKeepsTheCountOfAMissingSwarmService(t *testing.T) {
	ref := api.AppRef{Kind: "image", Name: "api"}
	var now time.Time
	w := NewDownWatch(func() time.Time { return now })
	check := w.Check(ref, "web")
	now = downT0
	w.Observe(now, []api.App{downRole(ref, "web", want(2), replica("t1", "running", "unknown"), replica("t2", "running", "unknown"))}, nil)
	// The binding stays, but its Swarm service is gone: the role has no desired count and no replicas.
	now = downT0.Add(time.Minute)
	w.Observe(now, []api.App{downRole(ref, "web", nil)}, nil)
	now = downT0.Add(3 * time.Minute)
	w.Observe(now, []api.App{downRole(ref, "web", nil)}, nil)
	expectDown(t, check, Critical, "0 of 2 replicas running for 2m")
}

func TestDownCheckIsUnknownWithoutARecentInventory(t *testing.T) {
	ref := api.AppRef{Kind: "image", Name: "api"}
	var now time.Time
	w := NewDownWatch(func() time.Time { return now })
	check := w.Check(ref, "web")
	now = downT0
	expectDown(t, check, Unknown, "no recent Docker inventory")
	w.Observe(now, []api.App{downRole(ref, "web", want(1), replica("t1", "running", "unknown"))}, nil)
	now = downT0.Add(inventoryStale + time.Minute)
	expectDown(t, check, Unknown, "no recent Docker inventory")
}

func TestDownCheckShape(t *testing.T) {
	check := NewDownWatch(nil).Check(api.AppRef{Kind: "compose", Name: "shop-db"}, "db")
	if check.Name() != "down:shop-db/db" || check.Interval() != 30*time.Second || check.Failures() != 1 {
		t.Fatalf("down check shape: %s %v %d", check.Name(), check.Interval(), check.Failures())
	}
}

func TestDownAlertsOnceAndRecovers(t *testing.T) {
	r := newRig(t, openStore(t))
	ref := api.AppRef{Kind: "compose", Name: "e2e-db"}
	w := NewDownWatch(func() time.Time { return r.now })
	r.checks = []Check{w.Check(ref, "side")}
	stopped := []api.App{downRole(ref, "side", nil, replica("c1", "exited", "none"))}
	running := []api.App{downRole(ref, "side", nil, replica("c1", "running", "none"))}

	w.Observe(r.now, running, nil)
	r.step(0)
	w.Observe(r.now, stopped, nil)
	for i := 0; i < 4; i++ {
		r.step(30 * time.Second)
		w.Observe(r.now, stopped, nil)
	}
	expectMessages(t, r.notifier.messages(), "🔴 demo/lab down:e2e-db/side: container exited for 2m")

	// Still down: nothing more is sent without reminders.
	r.step(30 * time.Second)
	expectMessages(t, r.notifier.messages(), "🔴 demo/lab down:e2e-db/side: container exited for 2m")

	w.Observe(r.now, running, nil)
	r.step(30 * time.Second)
	expectMessages(t, r.notifier.messages(), "🔴 demo/lab down:e2e-db/side: container exited for 2m", "✅ demo/lab down:e2e-db/side recovered")
}

func TestDownSaysTheExitCodeAndSkipsFinishedOneShots(t *testing.T) {
	ref := api.AppRef{Kind: "compose", Name: "shop-db"}
	var now time.Time
	w := NewDownWatch(func() time.Time { return now })
	apps := []api.App{
		downRole(ref, "db", nil, exitedWith("c1", 137)),
		// A one-shot that finished with code 0 is neither wanted nor down.
		downRole(ref, "job", nil, exitedWith("c2", 0)),
		// A service with a running container and a finished one-shot wants the running one only.
		downRole(ref, "web", nil, replica("c3", "running", "none"), exitedWith("c4", 0)),
	}
	now = downT0
	w.Observe(now, apps, nil)
	now = downT0.Add(2 * time.Minute)
	w.Observe(now, apps, nil)
	expectDown(t, w.Check(ref, "db"), Critical, "exited with code 137 for 2m")
	expectDown(t, w.Check(ref, "job"), OK, "no replicas wanted")
	expectDown(t, w.Check(ref, "web"), OK, "1 of 1 replicas running")
}

func TestDownNotifiedFailureStaysCriticalAcrossAnAgentRestart(t *testing.T) {
	r := newRig(t, openStore(t))
	ref := api.AppRef{Kind: "compose", Name: "e2e-db"}
	w := NewDownWatch(func() time.Time { return r.now })
	r.checks = []Check{w.Check(ref, "side")}
	stopped := []api.App{downRole(ref, "side", nil, exitedWith("c1", 137))}
	w.Observe(r.now, stopped, nil)
	for i := 0; i < 4; i++ {
		r.step(30 * time.Second)
		w.Observe(r.now, stopped, nil)
	}
	expectMessages(t, r.notifier.messages(), "🔴 demo/lab down:e2e-db/side: exited with code 137 for 2m")

	// The agent restarts: a new monitor reads the stored states, and a new watch has seen nothing yet. The service is still
	// down, so for two minutes the check is unknown. That sends no recovered message, and the critical state that was
	// alerted is not sent again once it is confirmed.
	r.attach()
	w = NewDownWatch(func() time.Time { return r.now })
	r.checks = []Check{w.Check(ref, "side")}
	for i := 0; i < 2; i++ {
		w.Observe(r.now, stopped, nil)
		r.step(30 * time.Second)
	}
	expectMessages(t, r.notifier.messages(), "🔴 demo/lab down:e2e-db/side: exited with code 137 for 2m")
	if got := r.stored("down:e2e-db/side"); got.State != Unknown {
		t.Fatalf("state after the restart: %+v, want unknown while confirming", got)
	}
	for i := 0; i < 4; i++ {
		w.Observe(r.now, stopped, nil)
		r.step(30 * time.Second)
	}
	if got := r.stored("down:e2e-db/side"); got.State != Critical {
		t.Fatalf("state after the restart and two minutes: %+v", got)
	}
	expectMessages(t, r.notifier.messages(), "🔴 demo/lab down:e2e-db/side: exited with code 137 for 2m")
}
