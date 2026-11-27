package deploy

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"berthy/internal/engine"
	"berthy/internal/state"
)

// healthyTask is a task of the generation that runs healthy in the given slot.
func healthyTask(id, generation string, slot uint64) engine.ServiceTask {
	t := runningTask(id, generation)
	t.ServiceID = "s"
	t.Slot = slot
	return t
}

// A hand update that scales a service to 2 and a deploy that scales it back to 1 can leave the one replica in slot 2:
// Swarm keeps the task that started last. The replica count is how many slots run healthy, not which slot numbers.
func TestHealthCountsAReplicaInAnySlot(t *testing.T) {
	e := healthFake{tasks: []engine.ServiceTask{healthyTask("a", "op", 2)}, status: "healthy"}
	if err := healthyOnce(context.Background(), e, "s", "op", 1); err != nil {
		t.Fatalf("one healthy replica in slot 2 of 1: %v", err)
	}
	two := healthFake{tasks: []engine.ServiceTask{healthyTask("a", "op", 1), healthyTask("b", "op", 3)}, status: "healthy"}
	if err := healthyOnce(context.Background(), two, "s", "op", 2); err != nil {
		t.Fatalf("two healthy replicas in slots 1 and 3 of 2: %v", err)
	}
	if healthyOnce(context.Background(), e, "s", "op", 2) == nil {
		t.Fatal("one healthy replica counted as two")
	}
	// Two tasks in one slot are one replica.
	same := healthFake{tasks: []engine.ServiceTask{healthyTask("a", "op", 2), healthyTask("b", "op", 2)}, status: "healthy"}
	if healthyOnce(context.Background(), same, "s", "op", 2) == nil {
		t.Fatal("two tasks of one slot counted as two replicas")
	}
}

func TestAdoptedHealthCountsAReplicaInAnySlot(t *testing.T) {
	e := healthFake{tasks: []engine.ServiceTask{{ServiceID: "s", ContainerID: "c", State: "running", DesiredState: "running", Slot: 2, Image: "repo@sha256:old"}}, status: "healthy"}
	if err := healthyImageOnce(context.Background(), e, "s", "repo@sha256:old", 1); err != nil {
		t.Fatalf("one healthy replica in slot 2 of 1: %v", err)
	}
}

// updateRole over a service that someone scaled and changed with docker service update right before: the hand update's new
// task of slot 2 is the one that stays, the old slot 1 task is gone, and the deploy's own generation runs in slot 2 only.
func TestUpdateRoleSucceedsWhenTheSurvivingReplicaIsInSlotTwo(t *testing.T) {
	before := json.RawMessage(`{"Name":"svc","version":"hand"}`)
	desired := json.RawMessage(`{"Name":"svc","version":"new"}`)
	retired := engine.ServiceTask{ID: "r", ServiceID: "id", State: "shutdown", DesiredState: "shutdown", Slot: 1, Labels: map[string]string{"berthy.operation": "hand"}}
	survivor := healthyTask("n", "op", 2)
	survivor.ServiceID = "id"
	m := &mutationFake{live: engine.ServiceSnapshot{ID: "id", Version: 9, Spec: before}, tasks: []engine.ServiceTask{retired, survivor}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := updateRole(ctx, &journalFake{}, m, "op", state.Intent{Kind: "update", Name: "svc", ServiceID: "id", Generation: "op", Before: before, Desired: desired}, nil, 1, startBudget{limit: 3}, 0)
	if err != nil {
		t.Fatalf("update over a service changed by hand: %v", err)
	}
}
