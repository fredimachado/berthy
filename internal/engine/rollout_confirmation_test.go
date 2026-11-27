package engine

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestInspectStoppedContainerAtRolloutConfirmation(t *testing.T) {
	var calls atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1.44/containers/c1/json" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		running := calls.Add(1) == 1
		fmt.Fprintf(w, `{"State":{"Running":%t,"Health":{"Status":"healthy"}}}`, running)
	}))
	defer s.Close()
	e := &Engine{client: &http.Client{Transport: rewriteTransport{base: s.URL}}}
	if health, err := e.InspectContainerHealth(context.Background(), "c1"); err != nil || health != "healthy" {
		t.Fatalf("initial health = %q, %v", health, err)
	}
	if health, err := e.InspectContainerHealth(context.Background(), "c1"); err != nil || health == "healthy" {
		t.Fatalf("stopped container at confirmation = %q, %v", health, err)
	}
	if calls.Load() != 2 {
		t.Fatalf("inspect calls = %d, want 2", calls.Load())
	}
}
