package agent

import (
	"berthy/internal/deploy"
	"berthy/internal/metrics"
	"berthy/internal/scope"
	"berthy/internal/state"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestGuardRejectsWrongEnvironmentBeforeDocker(t *testing.T) {
	h := Handler(Config{Project: "demo", Environment: "prod", Tokens: []Token{{Digest: Digest("bty.prod." + strings.Repeat("a", 64)), Scope: "read"}}}, nil)
	for _, tc := range []struct {
		path, token string
		want        int
	}{{"/v1/projects/demo/environments/dev/apps", "bty.prod." + strings.Repeat("a", 64), 401}, {"/v1/projects/demo/environments/prod/apps", "bad", 401}, {"/v1/projects/demo/environments/prod/agent", "bty.prod." + strings.Repeat("a", 64), 200}} {
		r := httptest.NewRequest(http.MethodGet, tc.path, nil)
		r.Header.Set("Authorization", "Bearer "+tc.token)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Errorf("%s: got %d want %d", tc.path, w.Code, tc.want)
		}
	}
}

func TestAgentInfoSaysWhetherTheEnvironmentIsProtected(t *testing.T) {
	token := "bty.prod." + strings.Repeat("a", 64)
	for _, protected := range []bool{true, false} {
		h := Handler(Config{Project: "demo", Environment: "prod", Protected: protected, Tokens: []Token{{Digest: Digest(token), Scope: "read"}}}, nil)
		r := httptest.NewRequest(http.MethodGet, "/v1/projects/demo/environments/prod/agent", nil)
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		var info struct {
			Environment string `json:"environment"`
			Protected   bool   `json:"protected"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &info); err != nil || info.Environment != "prod" || info.Protected != protected {
			t.Errorf("protected=%v: %d %s", protected, w.Code, w.Body.String())
		}
	}
}

func TestStatsRouteRequiresAuthentication(t *testing.T) {
	c := Config{Project: "demo", Environment: "lab", Tokens: []Token{{Digest: Digest("bty.lab." + strings.Repeat("a", 64)), Scope: "read"}}}
	req := httptest.NewRequest("GET", "/v1/projects/demo/environments/lab/stats", nil)
	w := httptest.NewRecorder()
	Handler(c, nil).ServeHTTP(w, req)
	if w.Code != 401 {
		t.Fatalf("got %d", w.Code)
	}
	req = httptest.NewRequest("GET", "/v1/projects/demo/environments/lab/stats", nil)
	req.Header.Set("Authorization", "Bearer bty.lab."+strings.Repeat("a", 64))
	w = httptest.NewRecorder()
	Handler(c, nil).ServeHTTP(w, req)
	if w.Code != 503 {
		t.Fatalf("stats: %d %s", w.Code, w.Body.String())
	}
}
func TestStatsWatchStreamsInitialSample(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := metrics.New(metrics.NewHostReader(), nilDocker{})
	go c.Run(ctx)
	deadline := time.Now().Add(time.Second)
	for {
		if _, ok := c.Latest(); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no sample")
		}
		runtime.Gosched()
	}
	cfg := Config{Project: "demo", Environment: "lab", Tokens: []Token{{Digest: Digest("bty.lab." + strings.Repeat("a", 64)), Scope: "read"}}}
	req := httptest.NewRequest("GET", "/v1/projects/demo/environments/lab/stats?watch=1", nil).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer bty.lab."+strings.Repeat("a", 64))
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { HandlerWithMetrics(cfg, nil, c).ServeHTTP(w, req); close(done) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done
	if !strings.Contains(w.Body.String(), "data: ") {
		t.Fatalf("missing event: %s", w.Body.String())
	}
}

type nilDocker struct{}

func (nilDocker) Containers(context.Context) ([]metrics.ContainerRef, error) { return nil, nil }
func (nilDocker) Stats(context.Context, string) (metrics.RawContainer, error) {
	return metrics.RawContainer{}, nil
}

type fixedHost struct{ host metrics.Host }

func (f fixedHost) Read(context.Context) metrics.Host { return f.host }

type labeledDocker struct {
	refs []metrics.ContainerRef
}

func (d labeledDocker) Containers(context.Context) ([]metrics.ContainerRef, error) {
	return d.refs, nil
}
func (labeledDocker) Stats(context.Context, string) (metrics.RawContainer, error) {
	return metrics.RawContainer{MemoryUsage: 50, MemoryLimit: 100}, nil
}

func sampledCollector(t *testing.T, host metrics.Host, refs []metrics.ContainerRef) *metrics.Collector {
	t.Helper()
	col := metrics.New(fixedHost{host}, labeledDocker{refs})
	sampled, stop := col.Subscribe()
	t.Cleanup(stop)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go col.Run(ctx)
	select {
	case <-sampled:
	case <-time.After(2 * time.Second):
		t.Fatal("no sample")
	}
	return col
}

func statsSample(t *testing.T, h http.Handler, path, token string) metrics.Sample {
	t.Helper()
	w := serve(h, "GET", path, token, "")
	if w.Code != http.StatusOK {
		t.Fatalf("stats: %d %s", w.Code, w.Body.String())
	}
	var s metrics.Sample
	if err := json.Unmarshal(w.Body.Bytes(), &s); err != nil {
		t.Fatal(err)
	}
	return s
}

func statsContainerIDs(s metrics.Sample) []string {
	ids := make([]string, len(s.Containers))
	for i, c := range s.Containers {
		ids[i] = c.ID
	}
	return ids
}

func TestStatsShowTheHostAndOnlyThisEnvironmentsContainers(t *testing.T) {
	lab, prod := scope.ID{Project: "demo", Environment: "lab"}, scope.ID{Project: "shop", Environment: "prod"}
	cpu := 42.0
	total := uint64(2048)
	sh, f := hostedFile(t, lab, prod)
	sh.Collector = sampledCollector(t, metrics.Host{CPUPercent: &cpu, MemoryTotal: &total}, []metrics.ContainerRef{
		{ID: "lab-web", Labels: lab.Labels()},
		{ID: "prod-web", Labels: prod.Labels()},
	})
	h := openHost(t, sh, f)
	for n, id := range []scope.ID{lab, prod} {
		s := statsSample(t, h, id.APIPath()+"stats", adminOf(f, n))
		if s.Host.CPUPercent == nil || *s.Host.CPUPercent != cpu || s.Host.MemoryTotal == nil || *s.Host.MemoryTotal != total {
			t.Fatalf("%s host: %+v", id, s.Host)
		}
		want := id.Environment + "-web"
		if ids := statsContainerIDs(s); len(ids) != 1 || ids[0] != want {
			t.Fatalf("%s containers %v, want [%s]", id, ids, want)
		}
	}
}

func TestStatsHideTheTasksOfAnUnlabelledServiceAnotherEnvironmentAdopted(t *testing.T) {
	lab, prod := scope.ID{Project: "demo", Environment: "lab"}, scope.ID{Project: "shop", Environment: "prod"}
	sh, f := hostedFile(t, lab, prod)
	sh.Collector = sampledCollector(t, metrics.Host{}, []metrics.ContainerRef{
		{ID: "lab-own", Labels: lab.Labels()},
		{ID: "adopted-task", Labels: map[string]string{"com.docker.swarm.service.name": "legacy-web"}},
	})
	h := openHost(t, sh, f)
	if err := h.runtimes[prod].deployment.Store.AddClaims(context.Background(), []state.Claim{{Kind: state.ClaimService, Name: "legacy-web"}}); err != nil {
		t.Fatal(err)
	}
	s := statsSample(t, h, lab.APIPath()+"stats", adminOf(f, 0))
	if ids := statsContainerIDs(s); len(ids) != 1 || ids[0] != "lab-own" {
		t.Fatalf("lab stats listed the adopted task: %v", ids)
	}
}

func TestVisibleFailsClosedWhenForeignClaimsCannotBeRead(t *testing.T) {
	s := openIngressStore(t)
	if err := s.AddClaims(context.Background(), []state.Claim{{Kind: state.ClaimService, Name: "ours"}}); err != nil {
		t.Fatal(err)
	}
	r := &Runtime{
		Config: Config{Project: "demo", Environment: "lab"},
		deployment: &deploy.Controller{
			Store: s,
			Foreign: func(context.Context) (deploy.Foreign, error) {
				return deploy.Foreign{}, errors.New("unavailable")
			},
		},
	}
	if !r.visible(scope.ID{Project: "demo", Environment: "lab"}.Labels()) {
		t.Fatal("own labels hidden when foreign claims cannot be read")
	}
	if !r.visible(map[string]string{"com.docker.swarm.service.name": "ours"}) {
		t.Fatal("own claimed service hidden when foreign claims cannot be read")
	}
	if r.visible(nil) {
		t.Fatal("unowned container listed when foreign claims cannot be read")
	}
	if r.visible(scope.ID{Project: "shop", Environment: "prod"}.Labels()) {
		t.Fatal("other environment listed when foreign claims cannot be read")
	}
	if r.visible(map[string]string{"com.docker.swarm.service.name": "legacy-web"}) {
		t.Fatal("unclaimed swarm task listed when foreign claims cannot be read")
	}
	lab := scope.ID{Project: "demo", Environment: "lab"}
	stolen := lab.Labels()
	stolen["com.docker.swarm.service.name"] = "legacy-web"
	if r.visible(stolen) {
		t.Fatal("lab-labelled task of a service this environment does not hold listed when foreign claims cannot be read")
	}
	stolen = lab.Labels()
	stolen["com.docker.compose.project"] = "shop-prod"
	if r.visible(stolen) {
		t.Fatal("lab-labelled compose container of a project this environment does not hold listed when foreign claims cannot be read")
	}
}
