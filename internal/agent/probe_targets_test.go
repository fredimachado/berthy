package agent

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"reflect"
	"runtime"
	"slices"
	"testing"

	"berthy/internal/api"
	"berthy/internal/deploy"
	"berthy/internal/manifest"
	"berthy/internal/monitor"
	"berthy/internal/state"
)

// probeTargetsManifest has an image app bound to the overlay, an image app that is not deployed yet, and a Compose app.
const probeTargetsManifest = `{"project":"demo","network":"demo",
	"apps":{
		"api":{"kind":"image","image":"ghcr.io/acme/api","roles":{"web":{}}},
		"worker":{"kind":"image","image":"ghcr.io/acme/worker","roles":{"job":{}}},
		"db":{"kind":"compose","file":"db/compose.yml"}},
	"probes":{
		"web":{"service":{"app":"api","port":8080,"path":"/health"},"expect_status":[200],"interval":"10s","timeout":"2s"},
		"job":{"service":{"app":"worker","role":"job","port":9000},"interval":"10s","timeout":"2s"},
		"pg":{"service":{"app":"db","port":5432},"interval":"10s","timeout":"2s"},
		"named":{"service":{"app":"db","role":"postgres","port":5432},"interval":"10s","timeout":"2s"}}}`

// recordBoundRelease stores a successful deploy of app api whose Swarm services are bindings, as a deploy does.
// releaseSettings are the environment settings that a deploy of manifestJSON applies.
func releaseSettings(t *testing.T, manifestJSON string) *state.Settings {
	t.Helper()
	m, err := manifest.DecodeRelease([]byte(manifestJSON), false)
	if err != nil {
		t.Fatal(err)
	}
	st, err := state.NewSettings(*m, "", false)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func recordBoundRelease(t *testing.T, s *state.Store, id, manifestJSON string, bindings []state.Binding) {
	t.Helper()
	ctx := context.Background()
	var m struct {
		Apps map[string]json.RawMessage `json:"apps"`
	}
	if err := json.Unmarshal([]byte(manifestJSON), &m); err != nil {
		t.Fatal(err)
	}
	// Every app the manifest names has an applied release, as it would after its own deploy.
	for _, app := range slices.Sorted(maps.Keys(m.Apps)) {
		var own []state.Binding
		for _, b := range bindings {
			if b.App == app {
				own = append(own, b)
			}
		}
		op, _, err := s.Admit(ctx, state.Admission{RequestID: id + "-" + app, ActorID: "ci", Kind: "deploy", App: app, PublicRequest: []byte(`{}`)})
		if err != nil {
			t.Fatal(err)
		}
		if err = s.Finish(ctx, op.ID, state.Outcome{State: "succeeded", Release: &state.Release{App: app, Manifest: []byte(manifestJSON), Bindings: own}, Settings: releaseSettings(t, manifestJSON)}); err != nil {
			t.Fatal(err)
		}
	}
}

// projectServicesFake returns a Compose service lister that answers for one project.
func projectServicesFake(project string, services []string, asked *[]string) func(context.Context, string) ([]string, error) {
	return func(_ context.Context, p string) ([]string, error) {
		*asked = append(*asked, p)
		if p != project {
			return nil, nil
		}
		return services, nil
	}
}

func TestServiceProbeTargetsNameTheOverlayService(t *testing.T) {
	s := openIngressStore(t)
	ctx := context.Background()
	recordBoundRelease(t, s, "release-probe-targets", probeTargetsManifest, []state.Binding{{App: "api", Role: "web", Name: "lab-api-web", ServiceID: "svc-1"}})
	m, err := manifest.DecodeJSON([]byte(probeTargetsManifest))
	if err != nil {
		t.Fatal(err)
	}
	w := NewMonitoring(monitoringConfig(""), nil, nil, &deploy.Controller{Store: s, Environment: "lab"}, func(string) {})
	var asked []string
	w.composeServices = projectServicesFake("demo-db", []string{"postgres"}, &asked)
	cases := map[string]struct {
		probe   manifest.ServiceProbe
		want    string
		wantErr string
	}{
		"bound image role":      {manifest.ServiceProbe{App: "api", Port: 8080}, "lab-api-web", ""},
		"role named":            {manifest.ServiceProbe{App: "api", Role: "web", Port: 8080}, "lab-api-web", ""},
		"not deployed yet":      {manifest.ServiceProbe{App: "worker", Role: "job", Port: 9000}, "demo-worker-job", ""},
		"compose, only service": {manifest.ServiceProbe{App: "db", Port: 5432}, "postgres", ""},
		"compose, role named":   {manifest.ServiceProbe{App: "db", Role: "postgres", Port: 5432}, "postgres", ""},
		"unknown app":           {manifest.ServiceProbe{App: "ghost", Port: 1}, "", "probe target unknown: app not in the manifest"},
	}
	for name, tc := range cases {
		got, err := w.serviceTarget(ctx, *m, tc.probe)
		if tc.wantErr != "" {
			if err == nil || err.Error() != tc.wantErr {
				t.Errorf("%s: error %v, want %q", name, err, tc.wantErr)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("%s: %q %v, want %q", name, got, err, tc.want)
		}
	}
	if !reflect.DeepEqual(asked, []string{"demo-db"}) {
		t.Errorf("Compose project asked for %v", asked)
	}
}

func TestServiceProbeNeedsAComposeServiceNamedWhenThereAreSeveral(t *testing.T) {
	s := openIngressStore(t)
	m, err := manifest.DecodeJSON([]byte(probeTargetsManifest))
	if err != nil {
		t.Fatal(err)
	}
	w := NewMonitoring(monitoringConfig(""), nil, nil, &deploy.Controller{Store: s, Environment: "lab"}, func(string) {})
	var asked []string
	w.composeServices = projectServicesFake("demo-db", []string{"postgres", "redis"}, &asked)
	_, err = w.serviceTarget(context.Background(), *m, manifest.ServiceProbe{App: "db", Port: 5432})
	if err == nil || err.Error() != "probe target unknown: name the Compose service as role" {
		t.Fatalf("two services: %v", err)
	}
	w.composeServices = func(context.Context, string) ([]string, error) { return nil, errors.New("unavailable") }
	_, err = w.serviceTarget(context.Background(), *m, manifest.ServiceProbe{App: "db", Port: 5432})
	if err == nil || err.Error() != "probe target unknown: Compose services unavailable" {
		t.Fatalf("services unavailable: %v", err)
	}
}

func TestServiceProbeChecksSendTheirProbeToTheHelper(t *testing.T) {
	s := openIngressStore(t)
	ctx := context.Background()
	recordBoundRelease(t, s, "release-probe-targets", probeTargetsManifest, []state.Binding{{App: "api", Role: "web", Name: "lab-api-web", ServiceID: "svc-1"}})
	var got []monitor.HelperProbe
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var req monitor.HelperProbe
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		got = append(got, req)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "detail": "answered"})
	}))
	defer srv.Close()
	w := NewMonitoring(monitoringConfig(""), nil, nil, &deploy.Controller{Store: s, Environment: "lab"}, func(string) {})
	w.helper.goos = "linux"
	var asked []string
	w.composeServices = projectServicesFake("demo-db", []string{"postgres"}, &asked)
	checks := map[string]monitor.Check{}
	for _, c := range w.checks(ctx) {
		checks[c.Name()] = c
	}
	// Docker is not there, so the run that makes the helper fails; the probes use the helper state set here.
	w.helper.token, w.helper.port = "tok", serverPort(t, srv)
	for _, name := range []string{"probe:web", "probe:job", "probe:pg", "probe:named"} {
		if checks[name] == nil {
			t.Fatalf("missing %s", name)
		}
	}
	if res := checks["probe:web"].Run(ctx); res.State != monitor.OK || res.Detail != "answered" {
		t.Fatalf("web probe: %+v", res)
	}
	if res := checks["probe:pg"].Run(ctx); res.State != monitor.OK {
		t.Fatalf("pg probe: %+v", res)
	}
	want := []monitor.HelperProbe{
		{Kind: monitor.KindHTTP, Target: "lab-api-web:8080", Path: "/health", TimeoutMS: 2000, Expect: []int{200}},
		{Kind: monitor.KindTCP, Target: "postgres:5432", TimeoutMS: 2000},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("probes sent:\n got %+v\nwant %+v", got, want)
	}
}

func TestServiceProbeWithoutAHelperIsUnknownWithTheReason(t *testing.T) {
	s := openIngressStore(t)
	ctx := context.Background()
	recordBoundRelease(t, s, "release-probe-targets", probeTargetsManifest, []state.Binding{{App: "api", Role: "web", Name: "lab-api-web", ServiceID: "svc-1"}})
	w := NewMonitoring(monitoringConfig(""), nil, nil, &deploy.Controller{Store: s, Environment: "lab"}, func(string) {})
	w.helper.goos = "linux"
	var asked []string
	w.composeServices = projectServicesFake("demo-db", []string{"postgres"}, &asked)
	checks := map[string]monitor.Check{}
	for _, c := range w.checks(ctx) {
		checks[c.Name()] = c
	}
	if res := checks["probe:web"].Run(ctx); res.State != monitor.Unknown || res.Detail != "probe helper unavailable: Docker unavailable" {
		t.Fatalf("no helper: %+v", res)
	}
	// A target nobody can name is unknown too, and never reaches the helper.
	w.composeServices = projectServicesFake("demo-db", []string{"postgres", "redis"}, &asked)
	checks = map[string]monitor.Check{}
	for _, c := range w.checks(ctx) {
		checks[c.Name()] = c
	}
	if res := checks["probe:pg"].Run(ctx); res.State != monitor.Unknown || res.Detail != "probe target unknown: name the Compose service as role" {
		t.Fatalf("unnamed service: %+v", res)
	}
}

func TestDoctorReportsTheOverlayHelper(t *testing.T) {
	s := openIngressStore(t)
	ctx := context.Background()
	recordBoundRelease(t, s, "release-probe-targets", probeTargetsManifest, []state.Binding{{App: "api", Role: "web", Name: "lab-api-web", ServiceID: "svc-1"}})
	wantDetail := "probe helper unavailable: Docker unavailable"
	if runtime.GOOS != "linux" {
		wantDetail = "service probes need a Linux agent"
	}
	var found *api.Check
	for _, c := range monitoringChecks(ctx, monitoringConfig(""), s, nil, "lab") {
		if c.Name == "probe_helper" {
			c := c
			found = &c
		}
	}
	if found == nil || found.OK || found.Critical || found.Detail != wantDetail {
		t.Fatalf("probe_helper check: %+v", found)
	}

	// Every app is deployed again from a manifest whose only probe is an HTTP one: each app's own probes go back with its release.
	recordBoundRelease(t, s, "release-doctor-http", `{"project":"demo","network":"demo","apps":{
		"api":{"kind":"image","image":"ghcr.io/acme/api","roles":{"web":{}}},
		"worker":{"kind":"image","image":"ghcr.io/acme/worker","roles":{"job":{}}},
		"db":{"kind":"compose","file":"db/compose.yml"}},
		"probes":{"site":{"http":"http://127.0.0.1:1/"}}}`, nil)
	for _, c := range monitoringChecks(ctx, monitoringConfig(""), s, nil, "lab") {
		if c.Name == "probe_helper" {
			t.Fatalf("probe_helper without a service probe: %+v", c)
		}
	}
}

// TestHasServiceProbeMatchesOnlyServiceProbes checks which probes need the helper.
func TestHasServiceProbeMatchesOnlyServiceProbes(t *testing.T) {
	if hasServiceProbe(map[string]manifest.Probe{"site": {HTTP: "https://example.com/"}, "pg": {TCP: "10.0.0.5:5432"}}) {
		t.Fatal("HTTP and TCP probes need no helper")
	}
	if !hasServiceProbe(map[string]manifest.Probe{"web": {Service: &manifest.ServiceProbe{App: "api", Port: 8080}}}) {
		t.Fatal("a service probe needs the helper")
	}
}
