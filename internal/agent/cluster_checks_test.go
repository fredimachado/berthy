package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"berthy/internal/api"
	"berthy/internal/manifest"
	"berthy/internal/monitor"
	"berthy/internal/scope"
	"berthy/internal/state"
)

func clusterRouteFile(t *testing.T) (Shared, File, [2]scope.ID, [2]string) {
	t.Helper()
	ids := [2]scope.ID{{Project: "demo", Environment: "lab"}, {Project: "shop", Environment: "lab"}}
	sh, f := hostedFile(t, ids[:]...)
	var readers [2]string
	for i, id := range ids {
		readers[i] = hostToken(id, "a")
		f.Environments[i].Tokens = append(f.Environments[i].Tokens, Token{Name: "reader", Scope: "read", Digest: Digest(readers[i])})
	}
	return sh, f, ids, readers
}

func openClusterRouteHost(t *testing.T, sh Shared, f File) *Host {
	t.Helper()
	// The handlers are wired by Open. Leave the workers stopped so the checks are only the ones each test seeds.
	h, err := Open(sh, f)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	return h
}

func TestClusterChecksAuthenticateUnderTheSelectedEnvironment(t *testing.T) {
	sh, f, ids, readers := clusterRouteFile(t)
	h := openClusterRouteHost(t, sh, f)
	for i, id := range ids {
		path := id.APIPath() + "cluster/checks"
		for _, tc := range []struct {
			method, token string
			status        int
			body          string
		}{
			{"GET", readers[i], 200, `[]`},
			{"GET", "", 401, `{"code":"unauthorized","message":"unauthorized"}`},
			{"GET", readers[1-i], 401, `{"code":"unauthorized","message":"unauthorized"}`},
			{"POST", readers[i], 405, `{"code":"method_not_allowed","message":"method not allowed"}`},
			{"HEAD", readers[i], 405, `{"code":"method_not_allowed","message":"method not allowed"}`},
		} {
			w := serve(h, tc.method, path, tc.token, "")
			if w.Code != tc.status || strings.TrimSpace(w.Body.String()) != tc.body {
				t.Errorf("%s %s: %d %s, want %d %s", tc.method, path, w.Code, w.Body.String(), tc.status, tc.body)
			}
		}
	}
	h.cluster.Close()
	w := serve(h, "GET", ids[0].APIPath()+"cluster/checks", readers[0], "")
	if w.Code != 503 || strings.TrimSpace(w.Body.String()) != `{"code":"state_unavailable","message":"state unavailable"}` {
		t.Errorf("closed store: %d %s", w.Code, w.Body.String())
	}
}

func TestClusterCheckRowsDistinguishHostAndChannel(t *testing.T) {
	sh, f, ids, readers := clusterRouteFile(t)
	h := openClusterRouteHost(t, sh, f)
	ctx := context.Background()
	since := time.Date(2026, 11, 28, 6, 0, 0, 0, time.UTC)
	for _, row := range []state.CheckState{
		{Name: "webhook", State: "warning", Detail: "url missing", Since: since},
		{Name: "telegram", State: "ok", Detail: "token present", Since: since},
		{Name: "public_ports", State: "warning", Detail: "published port", Since: since, NotifiedState: "warning", Notified: since},
	} {
		if err := h.cluster.store.PutCheck(ctx, row); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.runtimes[ids[0]].deployment.Store.PutCheck(ctx, state.CheckState{Name: "probe:env", State: "critical"}); err != nil {
		t.Fatal(err)
	}
	hostname, err := os.Hostname()
	if err != nil || hostname == "" {
		t.Fatalf("hostname unavailable: %v", err)
	}
	want := []map[string]any{
		{"server": hostname, "name": "public_ports", "state": "warning", "detail": "published port", "since": "2026-11-28T06:00:00Z"},
		{"server": "", "name": "telegram", "state": "ok", "detail": "token present", "since": "2026-11-28T06:00:00Z"},
		{"server": "", "name": "webhook", "state": "warning", "detail": "url missing", "since": "2026-11-28T06:00:00Z"},
	}
	for i, id := range ids {
		w := serve(h, "GET", id.APIPath()+"cluster/checks", readers[i], "")
		var got []map[string]any
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &got) != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("%s cluster rows: %d %s, want %v", id, w.Code, w.Body.String(), want)
		}
	}
	w := serve(h, "GET", ids[0].APIPath()+"checks", readers[0], "")
	var ordinary []map[string]any
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &ordinary) != nil || len(ordinary) != 1 || ordinary[0]["name"] != "probe:env" {
		t.Errorf("environment checks: %d %s", w.Code, w.Body.String())
	}
}

func TestClusterReadPathsHideMalformedCredentialNames(t *testing.T) {
	const urlName, headersName = "SERVER_PRIVATE_URL", "SERVER_PRIVATE_HEADERS"
	for _, tc := range []struct {
		name, url, headers, detail string
	}{
		{"url", "bad-fixture-url", "X-Test=value", "webhook url secret is not an http or https URL"},
		{"headers", "http://fixture.example.test", "bad-fixture-headers", "webhook headers secret is malformed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				_, f, ids, readers := clusterRouteFile(t)
				f.Cluster.Alerts = &ClusterAlerts{Webhook: &manifest.Webhook{URLSecret: urlName, HeadersSecret: headersName}}
				h := openClusterRouteHost(t, Shared{}, f)
				ctx := context.Background()
				for name, value := range map[string]string{urlName: tc.url, headersName: tc.headers} {
					if err := h.cluster.store.PutSecret(ctx, state.Actor{ID: "setup"}, state.SecretInfo{Name: name}, []byte(value)); err != nil {
						t.Fatal(err)
					}
				}
				h.cluster.Start(ctx)
				synctest.Wait()
				for i, id := range ids {
					doctor := serve(h, "GET", id.APIPath()+"doctor", readers[i], "")
					var diagnosis struct{ Checks []api.Check }
					if doctor.Code != 200 || json.Unmarshal(doctor.Body.Bytes(), &diagnosis) != nil {
						t.Fatalf("doctor: %d %s", doctor.Code, doctor.Body.String())
					}
					check, found := findCheck(diagnosis.Checks, "cluster_webhook")
					if !found || check.OK || check.Critical || check.Detail != tc.detail {
						t.Errorf("doctor webhook = %+v, want noncritical %q", check, tc.detail)
					}
					reply := serve(h, "GET", id.APIPath()+"cluster/checks", readers[i], "")
					var rows []clusterCheckRow
					if reply.Code != 200 || json.Unmarshal(reply.Body.Bytes(), &rows) != nil {
						t.Fatalf("cluster checks: %d %s", reply.Code, reply.Body.String())
					}
					var webhook clusterCheckRow
					for _, row := range rows {
						if row.Name == "webhook" {
							webhook = row
						}
					}
					if webhook.State != monitor.Warning || webhook.Detail != tc.detail {
						t.Errorf("persisted webhook = %+v, want warning %q", webhook, tc.detail)
					}
				}
			})
		})
	}
}

func TestNoTokenCanManageOrTestClusterAlerts(t *testing.T) {
	var sends atomic.Int32
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		sends.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer fake.Close()
	sh, f, ids, readers := clusterRouteFile(t)
	ctx := context.Background()
	f.Cluster.Alerts = &ClusterAlerts{Webhook: &manifest.Webhook{URLSecret: "HOOK"}}
	f.Environments[0].Protected = true
	agentToken := hostToken(ids[0], "b")
	f.Environments[0].Tokens = append(f.Environments[0].Tokens, Token{Name: "agent", Scope: "admin", Digest: Digest(agentToken), Agent: true})
	h := openClusterRouteHost(t, sh, f)
	env := h.runtimes[ids[0]].deployment.Store
	for _, s := range []*state.Store{h.cluster.store, env} {
		if err := s.PutSecret(ctx, state.Actor{ID: "fixture"}, state.SecretInfo{Name: "HOOK"}, []byte(fake.URL)); err != nil {
			t.Fatal(err)
		}
	}
	recordRelease(t, env, "environment-alert", monitoredManifest(`,"alerts":{"webhook":{"url_secret":"HOOK"}}`))
	before, found, err := env.Settings(ctx)
	if err != nil || !found {
		t.Fatalf("environment settings unavailable: %v", err)
	}
	for _, token := range []string{adminOf(f, 0), agentToken} {
		for _, prefix := range []string{ids[0].APIPath() + "cluster/", "/v1/cluster/", "/cluster/"} {
			for _, req := range []struct{ method, path, body string }{
				{"GET", "secrets", ""}, {"POST", "secrets", `{"name":"HOOK","value":"mutated"}`}, {"DELETE", "secrets/HOOK", `{}`},
				{"POST", "apply", `{"alerts":{}}`}, {"PUT", "settings", `{"alerts":{}}`}, {"POST", "alerts/test", ""},
			} {
				w := serve(h, req.method, prefix+req.path, token, req.body)
				if w.Code != http.StatusNotFound {
					t.Errorf("%s %s: %d %s", req.method, prefix+req.path, w.Code, w.Body.String())
				}
			}
		}
	}
	if sends.Load() != 0 {
		t.Error("a refused request sent a notification")
	}
	if after, found, err := env.Settings(ctx); err != nil || !found || !reflect.DeepEqual(before, after) {
		t.Fatalf("environment settings changed: %v", err)
	}
	if _, found, err := h.cluster.store.Settings(ctx); err != nil || found {
		t.Fatalf("a refused request stored cluster settings: %v", err)
	}
	for _, s := range []*state.Store{h.cluster.store, env} {
		secrets, err := s.Secrets(ctx)
		if err != nil || len(secrets) != 1 || secrets[0].Name != "HOOK" {
			t.Fatalf("secrets changed: %v %v", secrets, err)
		}
		if _, value, err := s.Secret(ctx, "HOOK"); err != nil || string(value) != fake.URL {
			t.Fatalf("webhook credential changed: %v", err)
		}
		if rows, err := s.Approvals(ctx, "", time.Time{}); err != nil || len(rows) != 0 {
			t.Fatalf("approval records: %v %v", rows, err)
		}
	}
	if w := serve(h, "POST", ids[0].APIPath()+"alerts/test", adminOf(f, 0), ""); w.Code != 200 || sends.Load() != 1 {
		t.Errorf("ordinary test alert: %d %s, calls %d", w.Code, w.Body.String(), sends.Load())
	}
	if w := serve(h, "GET", ids[0].APIPath()+"cluster/checks", readers[0], ""); w.Code != 200 {
		t.Errorf("cluster read: %d %s", w.Code, w.Body.String())
	}
}

func TestDoctorReportsClusterChannelsWithoutChangingHostChecks(t *testing.T) {
	files := updateNotifierFiles(procNetTCP)
	files["var/run/reboot-required"] = ""
	files["var/run/reboot-required.pkgs"] = "libc6\n"
	setHostFiles(t, files)
	sh, f, ids, readers := clusterRouteFile(t)
	f.Cluster.Alerts = &ClusterAlerts{Telegram: &manifest.Telegram{BotTokenSecret: "TG", ChatID: "1"}, Webhook: &manifest.Webhook{URLSecret: "HOOK"}}
	h := openClusterRouteHost(t, sh, f)
	if err := h.cluster.store.PutCheck(context.Background(), state.CheckState{Name: "reboot_required", State: "ok"}); err != nil {
		t.Fatal(err)
	}
	readDoctors := func(want ...api.Check) [2][]string {
		t.Helper()
		var critical [2][]string
		for i, id := range ids {
			w := serve(h, "GET", id.APIPath()+"doctor", readers[i], "")
			var body struct{ Checks []api.Check }
			if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &body) != nil {
				t.Fatalf("doctor for %s: %d %s", id, w.Code, w.Body.String())
			}
			var channelChecks []api.Check
			for _, check := range body.Checks {
				if check.Critical {
					critical[i] = append(critical[i], fmt.Sprintf("%s:%t", check.Name, check.OK))
				}
				if strings.HasPrefix(check.Name, "cluster_") {
					channelChecks = append(channelChecks, check)
				}
			}
			if !reflect.DeepEqual(channelChecks, want) {
				t.Errorf("%s cluster checks: %+v, want %+v", id, channelChecks, want)
			}
			if check, found := findCheck(body.Checks, "public_ports"); !found || check.Critical {
				t.Errorf("%s public ports: %+v", id, check)
			}
			if check, found := findCheck(body.Checks, "reboot_required"); !found || check.Critical || check.OK || check.Detail != "reboot required for libc6" {
				t.Errorf("%s live reboot check: %+v", id, check)
			}
		}
		return critical
	}
	missing := readDoctors(api.Check{Name: "cluster_telegram", Detail: "telegram bot token secret missing"}, api.Check{Name: "cluster_webhook", Detail: "webhook url secret missing"})
	for name, value := range map[string]string{"TG": "123:test-token", "HOOK": "https://example.test/hooks"} {
		if err := h.cluster.store.PutSecret(context.Background(), state.Actor{ID: "fixture"}, state.SecretInfo{Name: name}, []byte(value)); err != nil {
			t.Fatal(err)
		}
	}
	ready := readDoctors(api.Check{Name: "cluster_telegram", OK: true, Detail: "bot token secret present"}, api.Check{Name: "cluster_webhook", OK: true, Detail: "url secret present"})
	h.cluster.alerts = nil
	clear := readDoctors(api.Check{Name: "cluster_alerts", Detail: "no cluster alert channel: host problems alert nobody; run berthy setup --cluster-alerts FILE"})
	if !reflect.DeepEqual(missing, ready) || !reflect.DeepEqual(missing, clear) {
		t.Errorf("cluster readiness changed critical checks: %v, %v, %v", missing, ready, clear)
	}
}
