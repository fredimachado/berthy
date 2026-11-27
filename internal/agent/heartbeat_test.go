package agent

import (
	"berthy/internal/dialtest"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"berthy/internal/api"
	"berthy/internal/deploy"
	"berthy/internal/monitor"
	"berthy/internal/state"
)

// heartbeatStore returns a store whose manifest has a heartbeat every interval, and that holds the URL secret when url is not
// empty.
func heartbeatStore(t *testing.T, url, interval string) *state.Store {
	t.Helper()
	s := openIngressStore(t)
	if url != "" {
		if err := s.PutSecret(context.Background(), state.Actor{ID: "admin"}, state.SecretInfo{Name: "HEARTBEAT_URL"}, []byte(url)); err != nil {
			t.Fatal(err)
		}
	}
	recordRelease(t, s, "heartbeat", monitoredManifest(`,"alerts":{"heartbeat":{"url_secret":"HEARTBEAT_URL","interval":"`+interval+`"}}`))
	return s
}

// heartbeatCheck returns the heartbeat check of the stored manifest, and the monitoring that made it, whose Docker answers.
func heartbeatCheck(t *testing.T, s *state.Store, dial monitor.DialFunc) (monitor.Check, *Monitoring) {
	t.Helper()
	cfg := monitoringConfig("")
	cfg.dial = dial
	w := NewMonitoring(cfg, nil, nil, &deploy.Controller{Store: s, Environment: "lab"}, func(string) {})
	w.beat.Alive = func(context.Context) bool { return true }
	for _, c := range w.checks(context.Background()) {
		if c.Name() == "heartbeat" {
			return c, w
		}
	}
	t.Fatal("no heartbeat check")
	return nil, nil
}

func TestHeartbeatPingsTheURLOfTheSecretEveryIntervalOfTheManifest(t *testing.T) {
	hook := newHookReceiver(t)
	s := heartbeatStore(t, hook.url(), "10s")
	check, w := heartbeatCheck(t, s, nil)
	if check.Interval() != 10*time.Second || check.Failures() != 1 {
		t.Fatalf("check %v %d", check.Interval(), check.Failures())
	}
	got := check.Run(context.Background())
	if got.State != monitor.OK || !strings.HasPrefix(got.Detail, "last ping accepted at 20") {
		t.Fatalf("result %+v", got)
	}
	calls := hook.received()
	if len(calls) != 1 || calls[0].method != http.MethodGet || calls[0].path != hookPath {
		t.Fatalf("requests %+v", calls)
	}

	// A new interval in the manifest is picked up when the checks are made again, which the monitor does every minute.
	recordRelease(t, s, "heartbeat-2", monitoredManifest(`,"alerts":{"heartbeat":{"url_secret":"HEARTBEAT_URL","interval":"20s"}}`))
	for _, c := range w.checks(context.Background()) {
		if c.Name() == "heartbeat" && c.Interval() != 20*time.Second {
			t.Fatalf("interval %v after the manifest changed", c.Interval())
		}
	}
}

func TestHeartbeatIsUnknownAndSendsNothingWithoutADockerThatAnswers(t *testing.T) {
	hook := newHookReceiver(t)
	s := heartbeatStore(t, hook.url(), "10s")
	// This monitoring has no engine, so Docker does not answer.
	w := NewMonitoring(monitoringConfig(""), nil, nil, &deploy.Controller{Store: s, Environment: "lab"}, func(string) {})
	var check monitor.Check
	for _, c := range w.checks(context.Background()) {
		if c.Name() == "heartbeat" {
			check = c
		}
	}
	if check == nil {
		t.Fatal("no heartbeat check")
	}
	got := check.Run(context.Background())
	if got.State != monitor.Unknown || got.Detail != "Docker unavailable: no ping sent; no ping accepted yet" || len(hook.received()) != 0 {
		t.Fatalf("result %+v, %d requests", got, len(hook.received()))
	}
}

func TestHeartbeatNamesTheSecretThatCannotBeUsed(t *testing.T) {
	hook := newHookReceiver(t)
	check, _ := heartbeatCheck(t, heartbeatStore(t, "", "10s"), nil)
	if got := check.Run(context.Background()); got.State != monitor.Unknown || got.Detail != "heartbeat url secret missing; no ping accepted yet" {
		t.Fatalf("missing: %+v", got)
	}
	check, _ = heartbeatCheck(t, heartbeatStore(t, "ftp://hooks.example.com/s3cr3t", "10s"), nil)
	got := check.Run(context.Background())
	if got.State != monitor.Unknown || got.Detail != "heartbeat url secret HEARTBEAT_URL is not an http or https URL; no ping accepted yet" || strings.Contains(got.Detail, "s3cr3t") {
		t.Fatalf("bad URL: %+v", got)
	}
	if len(hook.received()) != 0 {
		t.Fatal("a ping was sent")
	}
}

func TestHeartbeatIsUnknownWhenTheURLCannotBeReachedAndNeverLeaksIt(t *testing.T) {
	check, _ := heartbeatCheck(t, heartbeatStore(t, "http://"+dialtest.Refused+hookPath, "10s"), refusalDial(t))
	got := check.Run(context.Background())
	if got.State != monitor.Unknown || got.Detail != "ping failed: unreachable; no ping accepted yet" {
		t.Fatalf("result %+v", got)
	}
}

func TestDoctorShowsTheHeartbeatFromItsStoredCheck(t *testing.T) {
	hook := newHookReceiver(t)
	s := heartbeatStore(t, hook.url(), "10s")
	ctx := context.Background()
	doctor := func() api.Check {
		t.Helper()
		h := NewDeploymentHandler(monitoringConfig(""), nil, nil, &deploy.Controller{Store: s, Environment: "lab"})
		w := serveRequest(t, h, http.MethodGet, "/v1/projects/demo/environments/lab/doctor", monitoringToken("a"))
		var body struct {
			Checks []api.Check `json:"checks"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(w.Body.String(), "s3cr3t") {
			t.Fatalf("doctor leaks the URL: %s", w.Body.String())
		}
		for _, c := range body.Checks {
			if c.Name == "heartbeat" {
				return c
			}
		}
		t.Fatal("no heartbeat in the doctor")
		return api.Check{}
	}
	if c := doctor(); !c.OK || c.Critical || c.Detail != "not checked yet" {
		t.Errorf("before the first ping: %+v", c)
	}
	if err := s.PutCheck(ctx, state.CheckState{Name: "heartbeat", State: "ok", Detail: "last ping accepted at 2026-11-19T10:00:00Z", Since: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if c := doctor(); !c.OK || c.Detail != "ok: last ping accepted at 2026-11-19T10:00:00Z" {
		t.Errorf("after a ping: %+v", c)
	}
	if err := s.PutCheck(ctx, state.CheckState{Name: "heartbeat", State: "unknown", Detail: "ping failed: unreachable; last ping accepted at 2026-11-19T10:00:00Z", Since: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if c := doctor(); !c.OK || c.Critical || c.Detail != "unknown: ping failed: unreachable; last ping accepted at 2026-11-19T10:00:00Z" {
		t.Errorf("the URL cannot be reached: %+v", c)
	}
	if err := s.DeleteSecret(ctx, state.Actor{ID: "admin"}, "HEARTBEAT_URL"); err != nil {
		t.Fatal(err)
	}
	if c := doctor(); c.OK || c.Critical || c.Detail != "heartbeat url secret missing" {
		t.Errorf("the secret is gone: %+v", c)
	}
}
