package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"berthy/internal/engine"
	"berthy/internal/manifest"
	"berthy/internal/state"
)

const fixtureProbeSource = "/usr/local/bin/berthy"

func testProbeSource() (string, error) { return fixtureProbeSource, nil }

func uptimeManifest(threshold, budget string) manifest.Manifest {
	m := dockerManifest(nil)
	app := m.Apps["web"]
	app.Deploy.DeployTimeout = budget
	role := app.Roles["web"]
	retries := 3
	role.Health = &manifest.Health{Uptime: &threshold, Timeout: "30s", Retries: &retries}
	app.Roles["web"] = role
	m.Apps["web"] = app
	return m
}

func TestUptimeRoleProducesExecCheckAndOneReadOnlyBind(t *testing.T) {
	m := uptimeManifest("20000ms", "1m")
	app := m.Apps["web"]
	worker := app.Roles["web"]
	worker.Volumes = []string{"data:/data"}
	normal := dockerManifest(nil).Apps["web"].Roles["web"]
	defaultHealth := `{"Test":["CMD","/.berthy/berthy","probe","uptime","20s"],"Interval":20100000000,"Timeout":30000000000,"Retries":3}`
	workerMounts := `[{"Type":"volume","Source":"data","Target":"/data"},{"Type":"bind","Source":"/usr/local/bin/berthy","Target":"/.berthy/berthy","ReadOnly":true}]`
	for _, tc := range []struct {
		name, interval, startInterval, health, mounts string
		role                                          manifest.Role
	}{
		{"default", "", "", defaultHealth, workerMounts, worker},
		{"positive grace", "7s", "7000ms", `{"Test":["CMD","/.berthy/berthy","probe","uptime","20s"],"Interval":7000000000,"Timeout":30000000000,"Retries":3,"StartPeriod":20100000000}`, workerMounts, worker},
		{name: "ordinary sibling", health: `{"Test":["CMD","true"]}`, role: normal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			health := *tc.role.Health
			health.Interval, health.StartInterval = tc.interval, tc.startInterval
			tc.role.Health = &health
			spec, err := serviceSpec("test", "proj", "web", tc.name, "service", "operation", "net", "image", func(s string) string { return s }, nil, tc.role, "start-first", "1s", nil, "", fixtureProbeSource)
			if err != nil {
				t.Fatal(err)
			}
			var stored struct {
				TaskTemplate struct {
					ContainerSpec struct{ Mounts, Healthcheck json.RawMessage }
				}
			}
			if err := json.Unmarshal(storedAsDocker(spec), &stored); err != nil {
				t.Fatal(err)
			}
			container := stored.TaskTemplate.ContainerSpec
			if !sameSpec(container.Healthcheck, json.RawMessage(tc.health)) || (tc.mounts != "" && !sameSpec(container.Mounts, json.RawMessage(tc.mounts))) || (tc.mounts == "" && len(container.Mounts) != 0) {
				t.Fatalf("stored health %s, mounts %s, want %s and %s", container.Healthcheck, container.Mounts, tc.health, tc.mounts)
			}
		})
	}
}

type uptimeImageEngine struct {
	boundFake
	image engine.Image
}

func (f *uptimeImageEngine) InspectImage(context.Context, string) (engine.Image, error) {
	return f.image, nil
}

func runUptimeRequest(t *testing.T, c *Controller, req Request) state.Operation {
	t.Helper()
	ctx := context.Background()
	op, _, err := c.Admit(ctx, Actor{ID: "admin", Admin: true}, req)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	op, err = c.Store.Operation(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	return op
}

func TestUptimeImageTimingPreflight(t *testing.T) {
	health := uptimeManifest("20s", "1m").Apps["web"].Roles["web"].Health
	timing, err := health.UptimeTiming()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, health, budget, want string }{
		{"no metadata", "", "21s", "succeeded"},
		{"short image grace", `{"StartPeriod":20000000000}`, "1m", "failed"},
		{"inherited grace just below the scheduling margin", fmt.Sprintf(`{"StartPeriod":%d}`, timing.Interval-1), "1m", "failed"},
		{"image cadence misses", `{"StartPeriod":40000000000,"StartInterval":30000000000}`, "25s", "failed"},
		{"fallback cadence misses", `{"StartPeriod":40000000000,"StartInterval":500000000}`, "21s", "failed"},
		{"portable inherited cadence", `{"StartPeriod":40000000000,"StartInterval":2000000000}`, "26s", "succeeded"},
		{"malformed", `{"StartPeriod":"bad"}`, "1m", "failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &uptimeImageEngine{image: engine.Image{ID: "image", ConfigPresent: true, Healthcheck: json.RawMessage(tc.health)}}
			c := &Controller{Store: openStore(t), Engine: f, Environment: "test", ProbeSource: testProbeSource, rolloutHold: zeroTestRolloutHold()}
			op := runUptimeRequest(t, c, Request{RequestID: "candidate", App: "web", Version: digestOfLetter("a"), Manifest: uptimeManifest("20s", tc.budget)})
			if op.State != tc.want || (tc.want == "failed" && len(f.createdSpecs()) != 0) {
				t.Fatalf("state %s, error %q, service writes %d, want %s", op.State, op.Err, len(f.createdSpecs()), tc.want)
			}
		})
	}
}

func TestUptimeImageConfigThroughDockerInspect(t *testing.T) {
	for _, tc := range []struct {
		name, reply string
		valid       bool
	}{
		{"missing", `{"Id":"image"}`, false},
		{"null", `{"Id":"image","Config":null}`, false},
		{"empty", `{"Id":"image","Config":{}}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Keep the socket path short enough for sockaddr_un even when the test name is long.
			dir, err := os.MkdirTemp("", "berthy-image-")
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(dir)
			path := filepath.Join(dir, "d.sock")
			listener, err := net.Listen("unix", path)
			if err != nil {
				t.Fatal(err)
			}
			server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/v1.44/images/worker/json" {
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
				}
				_, _ = w.Write([]byte(tc.reply))
			})}
			done := make(chan struct{})
			go func() { defer close(done); _ = server.Serve(listener) }()
			ctx, cancel := context.WithCancel(context.Background())
			defer func() { cancel(); _ = server.Close(); <-done }()
			image, err := engine.New(path).InspectImage(ctx, "worker")
			if err != nil {
				t.Fatal(err)
			}
			m := uptimeManifest("20s", "1m")
			f := &uptimeImageEngine{image: image}
			c := &Controller{Store: openStore(t), Engine: f, Environment: "test", ProbeSource: testProbeSource, rolloutHold: zeroTestRolloutHold()}
			op := runUptimeRequest(t, c, Request{RequestID: "candidate", App: "web", Version: digestOfLetter("a"), Manifest: m})
			if tc.valid && op.State != "succeeded" || !tc.valid && (op.State != "failed" || op.Err != string(errUptimeImageTiming) || len(f.createdSpecs()) != 0) {
				t.Errorf("deploy state %s, error %q, service writes %d", op.State, op.Err, len(f.createdSpecs()))
			}
			f.add("manual", adoptedService(t, adoptionRole(), ""))
			role := m.Apps["web"].Roles["web"]
			role.Service = "existing"
			m.Apps["web"].Roles["web"] = role
			_, err = c.Plan(ctx, Actor{Admin: true}, Request{App: "web", Manifest: m, Adopt: true, DryRun: true})
			if tc.valid && err != nil || !tc.valid && !errors.Is(err, errUptimeImageTiming) {
				t.Errorf("adoption error %v", err)
			}
		})
	}
}

func TestUptimeSourceFailurePrecedesMutation(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	seedUptimeRelease(t, s, dockerManifest(nil))
	previous, err := s.Applied(ctx, "web")
	if err != nil {
		t.Fatal(err)
	}
	f := &boundFake{}
	c := &Controller{Store: s, Engine: f, Environment: "test", rolloutHold: zeroTestRolloutHold()}
	req := Request{RequestID: "candidate", App: "web", Version: digestOfLetter("b"), Manifest: uptimeManifest("20s", "1m")}
	_, err = c.Plan(ctx, Actor{Admin: true}, req)
	var refusal Refusal
	if !errors.As(err, &refusal) || !PublicMessage(string(refusal)) {
		t.Errorf("plan returned %v, want public source refusal", err)
	}
	op := runUptimeRequest(t, c, req)
	applied, err := s.Applied(ctx, "web")
	if err != nil || applied.ID != previous.ID || op.State != "failed" || !PublicMessage(op.Err) || len(f.createdSpecs()) != 0 {
		t.Fatalf("state %s, error %q, release %d (was %d), service writes %d, lookup %v", op.State, op.Err, applied.ID, previous.ID, len(f.createdSpecs()), err)
	}
}

func TestUptimeImageVolumeConflict(t *testing.T) {
	for _, tc := range []struct {
		target    string
		conflicts bool
	}{
		{"/", true}, {"/.berthy", true}, {"/.berthy/berthy", true}, {"/.berthy/other", true}, {"/.berthy-data", false}, {"/work", false},
	} {
		t.Run(tc.target, func(t *testing.T) {
			f := &uptimeImageEngine{image: engine.Image{ID: "image", ConfigPresent: true, Volumes: map[string]struct{}{tc.target: {}}}}
			c := &Controller{Store: openStore(t), Engine: f, Environment: "test", ProbeSource: testProbeSource, rolloutHold: zeroTestRolloutHold()}
			op := runUptimeRequest(t, c, Request{RequestID: "volume", App: "web", Version: digestOfLetter("a"), Manifest: uptimeManifest("20s", "1m")})
			if tc.conflicts {
				if op.State != "failed" || !PublicMessage(op.Err) || len(f.createdSpecs()) != 0 {
					t.Fatalf("state %s, error %q, writes %d", op.State, op.Err, len(f.createdSpecs()))
				}
			} else if op.State != "succeeded" {
				t.Fatalf("state %s, error %q", op.State, op.Err)
			}
		})
	}
}

func seedUptimeRelease(t *testing.T, s *state.Store, m manifest.Manifest, bindings ...state.Binding) state.Operation {
	t.Helper()
	ctx := context.Background()
	prior, _, err := s.Admit(ctx, state.Admission{RequestID: "prior", ActorID: "admin", Kind: "deploy", App: "web", PublicRequest: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Finish(ctx, prior.ID, state.Outcome{State: "succeeded", Release: &state.Release{App: "web", Digest: "docker.io/example/web@" + digestOfLetter("a"), Manifest: raw, Bindings: bindings}}); err != nil {
		t.Fatal(err)
	}
	return prior
}
