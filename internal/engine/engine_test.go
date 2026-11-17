package engine

import (
	"berthy/internal/api"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestSelect(t *testing.T) {
	a := []api.App{{Ref: api.AppRef{Kind: "swarm", Name: "web"}}, {Ref: api.AppRef{Kind: "compose", Name: "web"}}}
	for _, c := range []struct {
		s    string
		code int
	}{{"web", 409}, {"swarm:web", 200}, {"missing", 404}} {
		_, got := Select(a, c.s)
		if got != c.code {
			t.Errorf("%s: %d", c.s, got)
		}
	}
}

func TestStatsDecodesDockerCounters(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawQuery != "stream=false&one-shot=true" {
			t.Errorf("query %s", r.URL.RawQuery)
		}
		fmt.Fprint(w, `{"cpu_stats":{"cpu_usage":{"total_usage":100,"percpu_usage":[1,2]},"system_cpu_usage":500,"online_cpus":2},"memory_stats":{"usage":1000,"limit":2000,"stats":{"inactive_file":100}},"networks":{"eth0":{"rx_bytes":3,"tx_bytes":4}},"blkio_stats":{"io_service_bytes_recursive":[{"op":"Read","value":7}]}}`)
	}))
	defer s.Close()
	e := &Engine{client: &http.Client{Transport: rewriteTransport{base: s.URL}}}
	v, err := e.Stats(context.Background(), "abc")
	if err != nil || v.CPUUsage != 100 || v.SystemUsage != 500 || v.Cache != 100 || v.NetworkRXBytes != 3 || v.BlockReadBytes != 7 {
		t.Fatalf("stats %+v %v", v, err)
	}
}

type rewriteTransport struct{ base string }

func (t rewriteTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	u, _ := url.Parse(t.base)
	r.URL.Scheme = u.Scheme
	r.URL.Host = u.Host
	return http.DefaultTransport.RoundTrip(r)
}
func TestStatsEmptyResponseUnavailable(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{}`) }))
	defer s.Close()
	e := &Engine{client: &http.Client{Transport: rewriteTransport{base: s.URL}}}
	if _, err := e.Stats(context.Background(), "stopped"); err == nil {
		t.Fatal("empty stats accepted")
	}
}

// composeList returns n running containers, c0..c(n-1), all in compose project p, service s.
func composeList(n int) string {
	var b strings.Builder
	b.WriteString("[")
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"Id":"c%d","Image":"img","State":"running","Labels":{"com.docker.compose.project":"p","com.docker.compose.service":"s"}}`, i)
	}
	b.WriteString("]")
	return b.String()
}

func TestAppsKeepsRetiringRunningTask(t *testing.T) {
	e := fakeEngine(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/v1.44/containers/json":
			return response(200, `[{"Id":"c1","Labels":{"com.docker.swarm.task.id":"T1"}},{"Id":"c2","Labels":{"com.docker.swarm.task.id":"T2"}},{"Id":"c3","Labels":{"com.docker.swarm.task.id":"T3"}}]`), nil
		case "/v1.44/services":
			return response(200, `[{"ID":"web-id","Spec":{"Name":"web","Mode":{"Replicated":{"Replicas":1}}}}]`), nil
		case "/v1.44/tasks":
			return response(200, `[{"ID":"T1","ServiceID":"web-id","DesiredState":"running","Status":{"State":"running","ContainerStatus":{"ContainerID":"c1"}}},{"ID":"T2","ServiceID":"web-id","DesiredState":"shutdown","Status":{"State":"running","ContainerStatus":{"ContainerID":"c2"}}},{"ID":"T3","ServiceID":"web-id","DesiredState":"shutdown","Status":{"State":"complete","ContainerStatus":{"ContainerID":"c3"}}}]`), nil
		case "/v1.44/containers/c1/json", "/v1.44/containers/c2/json":
			return response(200, `{"State":{}}`), nil
		case "/v1.44/containers/c3/json":
			t.Errorf("terminal task container was inspected")
			return response(200, `{"State":{}}`), nil
		}
		return response(404, `{}`), nil
	})
	apps, err := e.Apps(context.Background())
	if err != nil || len(apps) != 1 || len(apps[0].Roles) != 1 {
		t.Fatalf("apps %#v err %v", apps, err)
	}
	role := apps[0].Roles[0]
	if role.Running != 2 || len(role.Replicas) != 2 || role.Replicas[0].TaskID != "T1" || role.Replicas[0].ContainerID != "c1" || role.Replicas[1].TaskID != "T2" || role.Replicas[1].ContainerID != "c2" {
		t.Fatalf("role %#v", role)
	}
}

func TestAppsFillsDetailsForManyContainers(t *testing.T) {
	const n = 50
	bodies := map[string]string{}
	for i := 0; i < n; i++ {
		bodies[fmt.Sprintf("/v1.44/containers/c%d/json", i)] = fmt.Sprintf(`{"RestartCount":%d,"State":{"OOMKilled":%t}}`, i, i%2 == 1)
	}
	// bodies is only read by the handler, so concurrent inspects are safe.
	e := fakeEngine(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/v1.44/containers/json":
			return response(200, composeList(n)), nil
		case "/v1.44/services":
			return response(200, `[]`), nil
		}
		if body, ok := bodies[r.URL.Path]; ok {
			return response(200, body), nil
		}
		return response(404, `{}`), nil
	})
	apps, err := e.Apps(context.Background())
	if err != nil || len(apps) != 1 || len(apps[0].Roles) != 1 {
		t.Fatalf("apps %#v err %v", apps, err)
	}
	role := apps[0].Roles[0]
	if role.Name != "s" || role.Running != n || len(role.Replicas) != n {
		t.Fatalf("role %s running %d replicas %d", role.Name, role.Running, len(role.Replicas))
	}
	for i, rep := range role.Replicas {
		if want := fmt.Sprintf("c%d", i); rep.ContainerID != want {
			t.Fatalf("replica %d is %s, want %s", i, rep.ContainerID, want)
		}
		if rep.RestartCount == nil || *rep.RestartCount != i {
			t.Fatalf("%s restart count %v, want %d", rep.ContainerID, rep.RestartCount, i)
		}
		if rep.OOMKilled == nil || *rep.OOMKilled != (i%2 == 1) {
			t.Fatalf("%s oom killed %v", rep.ContainerID, rep.OOMKilled)
		}
		if !rep.Local {
			t.Fatalf("%s not marked local", rep.ContainerID)
		}
		if rep.Health != "none" {
			t.Fatalf("%s health %q, want none", rep.ContainerID, rep.Health)
		}
	}
}

func TestAppsReturnsInspectFailure(t *testing.T) {
	e := fakeEngine(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/v1.44/containers/json":
			return response(200, composeList(3)), nil
		case "/v1.44/services":
			return response(200, `[]`), nil
		case "/v1.44/containers/c1/json":
			return response(500, `{}`), nil
		}
		return response(200, `{"State":{}}`), nil
	})
	if apps, err := e.Apps(context.Background()); err == nil || apps != nil {
		t.Fatalf("apps %#v err %v", apps, err)
	}
}

func TestAppsStopsAfterContextEndsDuringInspect(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e := fakeEngine(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/v1.44/containers/json":
			return response(200, composeList(20)), nil
		case "/v1.44/services":
			return response(200, `[]`), nil
		case "/v1.44/containers/c0/json":
			// c0 is always handed out first, so this cancels the whole Apps call.
			cancel()
			return response(200, `{"State":{}}`), nil
		}
		if strings.HasPrefix(r.URL.Path, "/v1.44/containers/c") {
			<-r.Context().Done()
			return nil, r.Context().Err()
		}
		return response(404, `{}`), nil
	})
	if apps, err := e.Apps(ctx); err == nil || apps != nil {
		t.Fatalf("apps %#v err %v", apps, err)
	}
}

func TestAppsMarksUninspectableReplicaNotLocal(t *testing.T) {
	e := fakeEngine(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/v1.44/containers/json":
			return response(200, composeList(2)), nil
		case "/v1.44/services":
			return response(200, `[]`), nil
		case "/v1.44/containers/c0/json":
			return response(200, `{"RestartCount":1,"State":{}}`), nil
		case "/v1.44/containers/c1/json":
			return response(404, `{}`), nil
		}
		return response(404, `{}`), nil
	})
	apps, err := e.Apps(context.Background())
	if err != nil || len(apps) != 1 || len(apps[0].Roles) != 1 {
		t.Fatalf("apps %#v err %v", apps, err)
	}
	byID := map[string]api.Replica{}
	for _, rep := range apps[0].Roles[0].Replicas {
		byID[rep.ContainerID] = rep
	}
	c0, ok0 := byID["c0"]
	c1, ok1 := byID["c1"]
	if len(byID) != 2 || !ok0 || !ok1 {
		t.Fatalf("replicas %+v", byID)
	}
	if !c0.Local || c0.RestartCount == nil || *c0.RestartCount != 1 {
		t.Fatalf("c0 %+v", c0)
	}
	if c1.Local || c1.RestartCount != nil {
		t.Fatalf("c1 %+v", c1)
	}
}

func TestNewTransportKeepsIdleConnections(t *testing.T) {
	e := New("/nonexistent.sock")
	tr, ok := e.client.Transport.(*http.Transport)
	if !ok || tr.MaxIdleConnsPerHost != inspectWorkers {
		t.Fatalf("transport %#v, want *http.Transport with MaxIdleConnsPerHost %d", e.client.Transport, inspectWorkers)
	}
}

func TestReplicaJSONOmitsLocal(t *testing.T) {
	b, err := json.Marshal(api.Replica{ContainerID: "c", Local: true})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(b, &fields); err != nil {
		t.Fatal(err)
	}
	for key := range fields {
		if strings.EqualFold(key, "local") {
			t.Fatalf("Local leaked into JSON: %s", b)
		}
	}
}

func TestLoggingDriverIsTheDaemonsDefault(t *testing.T) {
	e := fakeEngine(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/v1.44/info" {
			t.Errorf("request %s", r.URL.Path)
		}
		return response(200, `{"LoggingDriver":"local","Swarm":{"NodeID":"node-1"}}`), nil
	})
	if got, err := e.LoggingDriver(context.Background()); err != nil || got != "local" {
		t.Fatalf("logging driver %q: %v", got, err)
	}
}

func TestLoggingDriverFailsWhenTheDaemonDoesNotSayOrFails(t *testing.T) {
	silent := fakeEngine(func(r *http.Request) (*http.Response, error) {
		return response(200, `{"Swarm":{"NodeID":"node-1"}}`), nil
	})
	if _, err := silent.LoggingDriver(context.Background()); err == nil {
		t.Fatal("a daemon that reports no log driver was accepted")
	}
	down := fakeEngine(func(r *http.Request) (*http.Response, error) {
		return nil, context.DeadlineExceeded
	})
	if _, err := down.LoggingDriver(context.Background()); err == nil {
		t.Fatal("an unreachable daemon was accepted")
	}
	failing := fakeEngine(func(r *http.Request) (*http.Response, error) {
		return response(500, `{}`), nil
	})
	if _, err := failing.LoggingDriver(context.Background()); err == nil {
		t.Fatal("a failed /info was accepted")
	}
}
