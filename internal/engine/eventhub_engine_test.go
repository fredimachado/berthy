package engine

import (
	"context"
	"testing"
)

func TestHubKeepsEventsOfEveryHostedEnvironment(t *testing.T) {
	prod := `{"Type":"container","Action":"die","Actor":{"ID":"x","Attributes":{"berthy.environment":"prod"}},"time":1,"timeNano":1000000000}`
	compose := `{"Type":"container","Action":"die","Actor":{"ID":"k","Attributes":{"com.docker.compose.project":"shop-prod-db"}},"time":2,"timeNano":2000000000}`
	stranger := `{"Type":"container","Action":"die","Actor":{"ID":"s","Attributes":{"name":"somebody-else"}},"time":3,"timeNano":3000000000}`
	e, queries := eventsServer(t, []string{stranger, prod, dieLine, compose, updateLine}, true)
	h := NewEventHub(e)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	defer func() { cancel(); <-done }()
	ch, stop := e.Watch(func(Event) bool { return true })
	defer stop()
	go func() { defer close(done); h.Run(ctx) }()
	var ids []string
	for i := 0; i < 4; i++ {
		ids = append(ids, recv(t, ch).ID)
	}
	if ids[0] != "x" || ids[1] != "c1" || ids[2] != "k" || ids[3] != "svc" {
		t.Fatalf("got %v, want the containers of both environments, the Compose container and the service, and not the stranger", ids)
	}
	if got := queries()[0].Get("filters"); got == "" {
		t.Fatal("the hub asked Docker for everything")
	}
}
