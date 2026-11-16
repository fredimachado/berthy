package backup

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"berthy/internal/engine"
)

func TestRemoveLeftoversRemovesOnlyThisEnvironmentsBackupHelpers(t *testing.T) {
	f := newFakeDocker()
	f.inventory = []engine.InventoryContainer{
		{ID: "a1", State: "running", Names: []string{"/berthy-archive-prod-1"}, Labels: map[string]string{"berthy.environment": "prod", "berthy.helper": "archive"}},
		{ID: "r1", State: "exited", Labels: map[string]string{"berthy.environment": "prod", "berthy.helper": "restore"}},
		{ID: "x1", State: "running", Labels: map[string]string{"berthy.environment": "staging", "berthy.helper": "archive"}},
		{ID: "p1", State: "running", Labels: map[string]string{"berthy.environment": "prod", "berthy.helper": "probe"}},
		{ID: "u1", State: "running", Labels: map[string]string{"berthy.environment": "prod"}},
		{ID: "u2", State: "running"},
	}
	a, logged := newArchiver(f)
	for _, c := range f.inventory {
		f.containers[c.ID] = &fakeContainer{name: c.ID}
	}
	n, err := a.RemoveLeftovers(context.Background())
	if err != nil || n != 2 {
		t.Fatalf("removed %d: %v", n, err)
	}
	var gone []string
	for id, c := range f.containers {
		if c.removed {
			gone = append(gone, id)
		}
	}
	slices.Sort(gone)
	if !slices.Equal(gone, []string{"a1", "r1"}) {
		t.Fatalf("removed %v", gone)
	}
	if len(*logged) != 2 || !strings.Contains((*logged)[0], "berthy-archive-prod-1") {
		t.Fatalf("log %v", *logged)
	}
}

func TestRemoveLeftoversReportsFailuresAndKeepsGoing(t *testing.T) {
	f := newFakeDocker()
	f.inventory = []engine.InventoryContainer{
		{ID: "a1", Labels: map[string]string{"berthy.environment": "prod", "berthy.helper": "archive"}},
		{ID: "a2", Labels: map[string]string{"berthy.environment": "prod", "berthy.helper": "archive"}},
	}
	f.containers["a2"] = &fakeContainer{name: "a2"}
	a, _ := newArchiver(f)
	n, err := a.RemoveLeftovers(context.Background())
	// a1 is unknown to Docker (404, already gone); a2 is removed.
	if err != nil || n != 1 {
		t.Fatalf("removed %d: %v", n, err)
	}
	f.inventoryErr = errors.New("docker down")
	if _, err = a.RemoveLeftovers(context.Background()); err == nil {
		t.Fatal("an inventory failure was not reported")
	}
}
