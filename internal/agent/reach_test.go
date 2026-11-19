package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
	"time"

	"berthy/internal/deploy"
	"berthy/internal/monitor"
)

// reachHelper is a helper of the fake Docker whose published port is served by an httptest server that answers each probe
// with answer, and records the probes it got.
type reachHelper struct {
	*helperManager
	docker *hfDocker
	clock  *hfClock
	mu     sync.Mutex
	probes []monitor.HelperProbe
	answer map[string]any
}

func newReachHelper(t *testing.T) *reachHelper {
	t.Helper()
	r := &reachHelper{docker: newHfDocker(), clock: &hfClock{t: time.Unix(1_700_000_000, 0)}, answer: map[string]any{"ok": true, "detail": "connected"}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var p monitor.HelperProbe
		if err := json.NewDecoder(req.Body).Decode(&p); err != nil {
			t.Error(err)
		}
		r.mu.Lock()
		r.probes = append(r.probes, p)
		answer := r.answer
		r.mu.Unlock()
		_ = json.NewEncoder(w).Encode(answer)
	}))
	t.Cleanup(srv.Close)
	r.helperManager = newHfHelper(r.docker, writeAgentBinary(t, "one"), r.clock)
	r.client = &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, srv.Listener.Addr().String())
	}}}
	return r
}

func TestReachStartsTheHelperAndProbesTheServicePort(t *testing.T) {
	r := newReachHelper(t)
	if err := r.Reach(context.Background(), "lab-net", "demo-api-web", 8080); err != nil {
		t.Fatal(err)
	}
	want := []monitor.HelperProbe{{Kind: monitor.KindTCP, Target: "demo-api-web:8080", TimeoutMS: 3000}}
	if !reflect.DeepEqual(r.probes, want) {
		t.Fatalf("probes %+v, want %+v", r.probes, want)
	}
	if c := r.docker.only(t); !c.running {
		t.Fatal("the helper is not running")
	}
}

func TestReachReportsAPortThatDoesNotAnswerAsNotAnswering(t *testing.T) {
	r := newReachHelper(t)
	r.answer = map[string]any{"ok": false, "detail": "connection refused"}
	err := r.Reach(context.Background(), "lab-net", "demo-api-web", 8080)
	if err == nil || errors.Is(err, deploy.ErrReachUnavailable) {
		t.Fatalf("err %v: want a plain error for a port that does not answer", err)
	}
}

func TestReachSaysWhenTheHelperCannotRun(t *testing.T) {
	r := newReachHelper(t)
	delete(r.docker.networks, "lab-net")
	err := r.Reach(context.Background(), "lab-net", "demo-api-web", 8080)
	if !errors.Is(err, deploy.ErrReachUnavailable) {
		t.Fatalf("err %v: want ErrReachUnavailable", err)
	}
	if len(r.probes) != 0 {
		t.Fatalf("probes %+v with no helper", r.probes)
	}
}

func TestReachIgnoresTheOneMinuteLimitOfTheMonitor(t *testing.T) {
	r := newReachHelper(t)
	ctx := context.Background()
	// The monitor just found no service probe and removed nothing; its next run is a minute away.
	if err := r.remove(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.Reach(ctx, "lab-net", "demo-api-web", 8080); err != nil {
		t.Fatalf("a deploy right after a monitor run: %v", err)
	}
	if r.docker.createCount() != 1 {
		t.Fatalf("%d helpers created, want 1", r.docker.createCount())
	}
}

func TestTheHelperThatACheckStartedStaysForTheLeaseAndIsRemovedAfterIt(t *testing.T) {
	r := newReachHelper(t)
	w := &Monitoring{helper: r.helperManager}
	ctx := context.Background()
	if err := r.Reach(ctx, "lab-net", "demo-api-web", 8080); err != nil {
		t.Fatal(err)
	}
	// The monitor sees no service probe in the manifest. It leaves the helper while a deploy's check may still need it.
	r.clock.t = r.clock.t.Add(2 * time.Minute)
	w.maintainHelper(ctx, nil)
	if len(r.docker.containers) != 1 {
		t.Fatal("the helper was removed inside its lease")
	}
	r.clock.t = r.clock.t.Add(helperLease)
	w.maintainHelper(ctx, nil)
	if len(r.docker.containers) != 0 {
		t.Fatal("the helper stayed after its lease with no service probe")
	}
}

func TestACheckRenewsTheLeaseOfTheHelper(t *testing.T) {
	r := newReachHelper(t)
	ctx := context.Background()
	if err := r.Reach(ctx, "lab-net", "demo-api-web", 8080); err != nil {
		t.Fatal(err)
	}
	r.clock.t = r.clock.t.Add(helperLease - time.Minute)
	if err := r.Reach(ctx, "lab-net", "demo-api-web", 8080); err != nil {
		t.Fatal(err)
	}
	r.clock.t = r.clock.t.Add(helperLease - time.Minute)
	if err := r.remove(ctx); err != nil || len(r.docker.containers) != 1 {
		t.Fatalf("remove: %v, %d containers: the second check renewed the lease", err, len(r.docker.containers))
	}
	if r.docker.createCount() != 1 {
		t.Fatalf("%d helpers created, want 1", r.docker.createCount())
	}
}
