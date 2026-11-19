package deploy

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"berthy/internal/manifest"
	"berthy/internal/state"
)

// fakeReacher answers every Reach with err and records what it was asked.
type fakeReacher struct {
	mu    sync.Mutex
	err   error
	calls []string
}

func (r *fakeReacher) Reach(_ context.Context, network, host string, port int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, fmt.Sprintf("%s %s:%d", network, host, port))
	return r.err
}

// verifyLines returns the text of the verify events of an operation, in order.
func verifyLines(t *testing.T, s *state.Store, opID string) []string {
	t.Helper()
	events, err := s.EventsAfter(context.Background(), opID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, ev := range events {
		if ev.Kind != "verify" {
			continue
		}
		var data struct {
			Role   string `json:"role"`
			Check  string `json:"check"`
			Status string `json:"status"`
			Step   string `json:"step"`
		}
		if err = json.Unmarshal(ev.Data, &data); err != nil {
			t.Fatal(err)
		}
		if data.Role == "" || data.Check == "" || data.Status == "" {
			t.Fatalf("verify event %s lacks role, check or status", ev.Data)
		}
		out = append(out, data.Step)
	}
	return out
}

// verifyManifest is a manifest with one image app, api, whose web role has the given JSON settings and the app's extra
// members, such as `,"deploy":{"verify":false}`.
func verifyManifest(t *testing.T, role, extra string) manifest.Manifest {
	t.Helper()
	m, err := manifest.DecodeJSON([]byte(`{"project":"demo","network":"overlay",` + apiApps(role, extra) + `}`))
	if err != nil {
		t.Fatal(err)
	}
	return *m
}

const (
	warnNetwork = "role web: healthy, but port 8080 does not answer on the network; is the app listening on 127.0.0.1?"
	okNetwork   = "role web: port 8080 answers on the network"
)

func TestDeployReportsThatThePortAnswersOnTheNetwork(t *testing.T) {
	f := newIngressFixture(t)
	r := &fakeReacher{}
	f.ctrl.Reacher = r
	op := f.deploy("d1", "api", apiFirst, verifyManifest(t, webRole(), ""))
	if op.State != "succeeded" {
		t.Fatalf("deploy %s: %s", op.State, op.Err)
	}
	if got := verifyLines(t, f.store, op.ID); !reflect.DeepEqual(got, []string{okNetwork}) {
		t.Fatalf("events %q", got)
	}
	if want := []string{"overlay demo-api-web:8080"}; !reflect.DeepEqual(r.calls, want) {
		t.Fatalf("reach calls %q, want %q", r.calls, want)
	}
}

func TestDeployWarnsWhenThePortDoesNotAnswerAndStillSucceeds(t *testing.T) {
	f := newIngressFixture(t)
	f.ctrl.Reacher = &fakeReacher{err: errors.New("connection refused")}
	op := f.deploy("d1", "api", apiFirst, verifyManifest(t, webRole(), ""))
	if op.State != "succeeded" || op.Err != "" {
		t.Fatalf("deploy %s: %s", op.State, op.Err)
	}
	if got := verifyLines(t, f.store, op.ID); !reflect.DeepEqual(got, []string{warnNetwork}) {
		t.Fatalf("events %q", got)
	}
}

func TestDeploySaysWhenThePortCouldNotBeChecked(t *testing.T) {
	f := newIngressFixture(t)
	f.ctrl.Reacher = &fakeReacher{err: fmt.Errorf("%w: probe helper unavailable: not running", ErrReachUnavailable)}
	op := f.deploy("d1", "api", apiFirst, verifyManifest(t, webRole(), ""))
	if op.State != "succeeded" {
		t.Fatalf("deploy %s: %s", op.State, op.Err)
	}
	want := []string{"role web: port 8080 could not be checked on the network (probe helper unavailable: not running)"}
	if got := verifyLines(t, f.store, op.ID); !reflect.DeepEqual(got, want) {
		t.Fatalf("events %q, want %q", got, want)
	}
}

func TestDeployWithVerifyOffChecksNothing(t *testing.T) {
	f := newIngressFixture(t)
	r := &fakeReacher{err: errors.New("refused")}
	f.ctrl.Reacher = r
	hits := f.outside(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	m := withIngress(t, apiApps(webRole("api.example.com"), `,"deploy":{"verify":false}`))
	f.applyIngress("apply", m)
	op := f.deploy("d1", "api", apiFirst, m)
	if op.State != "succeeded" {
		t.Fatalf("deploy %s: %s", op.State, op.Err)
	}
	if got := verifyLines(t, f.store, op.ID); len(got) != 0 || len(r.calls) != 0 || hits.Load() != 0 {
		t.Fatalf("events %q, reach calls %q, requests %d: want none", got, r.calls, hits.Load())
	}
}

func TestDeployChecksOnlyRolesWithAPort(t *testing.T) {
	f := newIngressFixture(t)
	r := &fakeReacher{}
	f.ctrl.Reacher = r
	op := f.deploy("d1", "api", apiFirst, verifyManifest(t, `{"health":{"test":["CMD","true"]}}`, ""))
	if op.State != "succeeded" {
		t.Fatalf("deploy %s: %s", op.State, op.Err)
	}
	if got := verifyLines(t, f.store, op.ID); len(got) != 0 || len(r.calls) != 0 {
		t.Fatalf("events %q, reach calls %q: a role without a port is not checked", got, r.calls)
	}
}

func TestDeployWithoutAReacherSaysNothingAboutTheNetwork(t *testing.T) {
	f := newIngressFixture(t)
	op := f.deploy("d1", "api", apiFirst, verifyManifest(t, webRole(), ""))
	if op.State != "succeeded" {
		t.Fatalf("deploy %s: %s", op.State, op.Err)
	}
	if got := verifyLines(t, f.store, op.ID); len(got) != 0 {
		t.Fatalf("events %q", got)
	}
}

// outside points the controller's outside checks at a TLS server that runs handler, whatever hostname they ask for. It returns
// how many requests the server got.
func (f *ingressFixture) outside(t *testing.T, handler http.HandlerFunc) *atomic.Int32 {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	addr := srv.Listener.Addr().String()
	f.ctrl.Outside = &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		},
	}}
	f.ctrl.outsidePause = time.Millisecond
	return &hits
}

// outsideNothing makes every outside request fail to connect.
func (f *ingressFixture) outsideNothing() *atomic.Int32 {
	var tries atomic.Int32
	f.ctrl.Outside = &http.Client{Transport: &http.Transport{
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			tries.Add(1)
			return nil, errors.New("no such host")
		},
	}}
	f.ctrl.outsidePause = time.Millisecond
	return &tries
}

func ingressDeploy(t *testing.T, f *ingressFixture) state.Operation {
	t.Helper()
	m := ingressManifest(t, "")
	f.applyIngress("apply", m)
	op := f.deploy("d1", "api", apiFirst, m)
	if op.State != "succeeded" {
		t.Fatalf("deploy %s: %s", op.State, op.Err)
	}
	return op
}

func TestDeployReportsTheStatusAHostnameAnswersWith(t *testing.T) {
	f := newIngressFixture(t)
	hits := f.outside(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	op := ingressDeploy(t, f)
	want := []string{"role web: https://api.example.com answers with status 200 from outside"}
	if got := verifyLines(t, f.store, op.ID); !reflect.DeepEqual(got, want) {
		t.Fatalf("events %q, want %q", got, want)
	}
	if hits.Load() != 1 {
		t.Fatalf("%d requests, want 1: a hostname that answers is not asked again", hits.Load())
	}
}

func TestDeployRetriesAHostnameThatAnswersWithAnErrorThenWarns(t *testing.T) {
	f := newIngressFixture(t)
	hits := f.outside(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(502) })
	op := ingressDeploy(t, f)
	want := []string{"role web: https://api.example.com answers with status 502 from outside; the route or the app is not ready"}
	if got := verifyLines(t, f.store, op.ID); !reflect.DeepEqual(got, want) {
		t.Fatalf("events %q, want %q", got, want)
	}
	if hits.Load() != outsideTries {
		t.Fatalf("%d requests, want %d", hits.Load(), outsideTries)
	}
}

func TestDeployStopsAskingOnceTheRouteSettles(t *testing.T) {
	f := newIngressFixture(t)
	var n atomic.Int32
	hits := f.outside(t, func(w http.ResponseWriter, _ *http.Request) {
		if n.Add(1) < 3 {
			w.WriteHeader(530)
			return
		}
		w.WriteHeader(404)
	})
	op := ingressDeploy(t, f)
	want := []string{"role web: https://api.example.com answers with status 404 from outside"}
	if got := verifyLines(t, f.store, op.ID); !reflect.DeepEqual(got, want) || hits.Load() != 3 {
		t.Fatalf("events %q after %d requests", got, hits.Load())
	}
}

func TestDeploySaysWhenNothingAnswersFromOutside(t *testing.T) {
	f := newIngressFixture(t)
	tries := f.outsideNothing()
	op := ingressDeploy(t, f)
	want := []string{"role web: nothing answers at https://api.example.com from outside; DNS or the route may still be settling"}
	if got := verifyLines(t, f.store, op.ID); !reflect.DeepEqual(got, want) {
		t.Fatalf("events %q, want %q", got, want)
	}
	if tries.Load() != outsideTries {
		t.Fatalf("%d tries, want %d", tries.Load(), outsideTries)
	}
}

func TestOutsideCheckFollowsNoRedirect(t *testing.T) {
	f := newIngressFixture(t)
	f.outside(t, func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/login", http.StatusFound) })
	op := ingressDeploy(t, f)
	want := []string{"role web: https://api.example.com answers with status 302 from outside"}
	if got := verifyLines(t, f.store, op.ID); !reflect.DeepEqual(got, want) {
		t.Fatalf("events %q, want %q", got, want)
	}
}
