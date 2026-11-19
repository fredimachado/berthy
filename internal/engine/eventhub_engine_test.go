package engine

import (
	"context"
	"testing"
)

func TestHubOfAnEngineKeepsTheEnvironmentsContainersAndEveryService(t *testing.T) {
	foreign := `{"Type":"container","Action":"die","Actor":{"ID":"x","Attributes":{"berthy.environment":"prod"}},"time":1,"timeNano":1000000000}`
	compose := `{"Type":"container","Action":"die","Actor":{"ID":"k","Attributes":{"com.docker.compose.project":"shop-db"}},"time":2,"timeNano":2000000000}`
	e, queries := eventsServer(t, []string{foreign, dieLine, compose, updateLine}, true)
	h := NewEventHub(e, "lab")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	defer func() { cancel(); <-done }()
	ch, stop := e.Watch(func(Event) bool { return true })
	defer stop()
	go func() { defer close(done); h.Run(ctx) }()
	var ids []string
	for i := 0; i < 3; i++ {
		ids = append(ids, recv(t, ch).ID)
	}
	if ids[0] != "c1" || ids[1] != "k" || ids[2] != "svc" {
		t.Fatalf("got %v, want the lab container, the Compose container and the service", ids)
	}
	if got := queries()[0].Get("filters"); got == "" {
		t.Fatal("the hub asked Docker for everything")
	}
}
