package monitor

import (
	"testing"
	"time"

	"berthy/internal/api"
)

func TestChecksStayKnownBetweenTwoSlowReadsOfTheInventory(t *testing.T) {
	// While Docker's events are flowing the inventory is read every InventoryEvery, so a read that is a little older than
	// that is still the latest one, and the checks judge by it. They turn unknown only when a read has been missed. The
	// restart watch also stays unknown for one RestartWindow after its first inventory, so that warm-up must finish
	// before this check can prove a slow read is still known.
	ref := api.AppRef{Kind: "image", Name: "api"}
	var now time.Time
	down := NewDownWatch(func() time.Time { return now })
	restarts := NewRestartWatch(func() time.Time { return now })
	apps := []api.App{downRole(ref, "web", want(1), replica("t1", "running", "unknown"))}
	now = downT0
	down.Observe(now, apps, nil)
	restarts.Observe(now, apps, nil)
	now = downT0.Add(RestartWindow)
	down.Observe(now, apps, nil)
	restarts.Observe(now, apps, nil)
	now = downT0.Add(RestartWindow + InventoryEvery + time.Minute)
	expectDown(t, down.Check(ref, "web"), OK, "1 of 1 replicas running")
	if got := restarts.result(now, RoleKey(ref, "web")); got.State == Unknown {
		t.Fatalf("restart check unknown after one slow read: %+v", got)
	}
	now = downT0.Add(RestartWindow + 2*InventoryEvery + time.Minute)
	expectDown(t, down.Check(ref, "web"), Unknown, "no recent Docker inventory")
}
