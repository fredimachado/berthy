package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"berthy/internal/api"
	"berthy/internal/deploy"
	"berthy/internal/engine"
	"berthy/internal/manifest"
	"berthy/internal/state"
)

// downDocker serves over a Unix socket the Docker routes of one inventory read: a Compose project with one healthy
// container, and a Swarm service of two replicas with one running task, which is the image app api's role web. It returns
// the engine and a function that counts the reads of the container list.
func downDocker(t *testing.T) (*engine.Engine, func() int) {
	t.Helper()
	d := newDownFake(t)
	return d.eng, d.containerReads
}

// downFake is that Docker, with a Docker event feed whose events the test pushes, and a switch that makes the Compose
// container report it was killed for memory.
type downFake struct {
	eng *engine.Engine
	mu  sync.Mutex
	// reads counts the requests for the container list; oom is what the container's inspect reports.
	reads   int
	oom     bool
	down    bool // the container list answers an error
	events  chan string
	filters chan map[string][]string
}

func (d *downFake) setDown(v bool) {
	d.mu.Lock()
	d.down = v
	d.mu.Unlock()
}

func (d *downFake) containerReads() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.reads
}

func (d *downFake) setOOM(v bool) {
	d.mu.Lock()
	d.oom = v
	d.mu.Unlock()
}

// push sends one line to the open event feed.
func (d *downFake) push(line string) { d.events <- line }

func newDownFake(t *testing.T) *downFake {
	t.Helper()
	return newDownFakeWithService(t, "shop-api-web", "svc-1")
}

func newDownFakeWithService(t *testing.T, name, id string) *downFake {
	t.Helper()
	d := &downFake{events: make(chan string, 64), filters: make(chan map[string][]string, 1)}
	// The directory is short on purpose: a Unix socket path holds about 108 bytes.
	dir, err := os.MkdirTemp("", "bd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Skipf("no Unix sockets here: %v", err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		service := fmt.Sprintf(`{"ID":%q,"Version":{"Index":1},"Spec":{"Name":%q,"Mode":{"Replicated":{"Replicas":2}}}}`, id, name)
		switch r.URL.Path {
		case "/version":
			w.Write([]byte(`{"ApiVersion":"1.50","MinAPIVersion":"1.24"}`))
		case "/v1.44/events":
			var filters map[string][]string
			if err := json.Unmarshal([]byte(r.URL.Query().Get("filters")), &filters); err != nil {
				t.Errorf("event filters: %v", err)
				return
			}
			select {
			case d.filters <- filters:
			default:
			}
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			for {
				select {
				case line := <-d.events:
					var event struct{ Type, Action string }
					if err := json.Unmarshal([]byte(line), &event); err != nil {
						t.Errorf("event: %v", err)
						continue
					}
					action := strings.SplitN(event.Action, ":", 2)[0]
					if !slices.Contains(filters["type"], event.Type) || !slices.Contains(filters["event"], action) {
						continue
					}
					w.Write([]byte(line + "\n"))
					w.(http.Flusher).Flush()
				case <-r.Context().Done():
					return
				}
			}
		case "/v1.44/containers/json":
			d.mu.Lock()
			d.reads++
			down := d.down
			d.mu.Unlock()
			if down {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			w.Write([]byte(`[{"Id":"c1","Image":"img","State":"running","Labels":{"com.docker.compose.project":"shop-lab-db","com.docker.compose.service":"db"}}]`))
		case "/v1.44/containers/c1/json":
			d.mu.Lock()
			oom := d.oom
			d.mu.Unlock()
			fmt.Fprintf(w, `{"RestartCount":0,"State":{"OOMKilled":%t,"Health":{"Status":"healthy"}}}`, oom)
		case "/v1.44/services":
			w.Write([]byte("[" + service + "]"))
		case "/v1.44/services/" + name:
			w.Write([]byte(service))
		case "/v1.44/tasks":
			fmt.Fprintf(w, `[{"ID":"t1","ServiceID":%q,"DesiredState":"running","Status":{"State":"running"}}]`, id)
		default:
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"message":"no such route"}`))
		}
	})}
	// A client inside synctest must not keep a socket read waiting after a response.
	srv.SetKeepAlivesEnabled(false)
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })
	d.eng = engine.New(sock)
	return d
}

func TestDownAndRestartChecksShareOneInventoryRead(t *testing.T) {
	e, containerReads := downDocker(t)
	s := openIngressStore(t)
	recordBoundRelease(t, s, "rel-shop", `{"project":"shop","network":"shop","apps":{"api":{"kind":"image","image":"ghcr.io/acme/api","roles":{"web":{}}}}}`,
		[]state.Binding{{App: "api", Role: "web", Name: "shop-api-web", ServiceID: "svc-1"}})
	m := &manifest.Manifest{Project: "shop", Apps: map[string]manifest.App{"api": {Kind: "image"}, "db": {Kind: "compose"}}}
	w := NewMonitoring(monitoringConfig(""), e, nil, &deploy.Controller{Store: s, Environment: "lab"}, func(string) {})

	names := map[string]bool{}
	for _, c := range w.appChecks(context.Background(), m) {
		names[c.Name()] = true
	}
	for _, want := range []string{"down:api/web", "down:shop-lab-db/db", "restarts:api/web", "restarts:shop-lab-db/db"} {
		if !names[want] {
			t.Errorf("missing check %s; have %v", want, names)
		}
	}
	if names["down:shop-api-web/web"] || names["down:shop-lab-db/web"] {
		t.Errorf("a check named by a service instead of its app: %v", names)
	}
	if got := containerReads(); got != 1 {
		t.Fatalf("the container list was read %d times for one rebuild, want 1", got)
	}
	// Without a manifest no Compose project is managed, so its services get no check.
	for _, c := range w.appChecks(context.Background(), nil) {
		if strings.HasPrefix(c.Name(), "down:shop-lab-db/") {
			t.Fatalf("unmanaged Compose service checked: %s", c.Name())
		}
	}
}

func TestDownQuietFollowsTheOperationsOfAnApp(t *testing.T) {
	s := openIngressStore(t)
	ctx := context.Background()
	w := NewMonitoring(monitoringConfig(""), nil, nil, &deploy.Controller{Store: s, Environment: "lab"}, func(string) {})
	m := &manifest.Manifest{Project: "shop", Apps: map[string]manifest.App{"api": {Kind: "image"}, "db": {Kind: "compose"}, "tools": {Kind: "image"}}}
	api1 := api.AppRef{Kind: "image", Name: "api"}
	db := api.AppRef{Kind: "compose", Name: "shop-lab-db"}
	tools := api.AppRef{Kind: "image", Name: "tools"}

	quietAt := func(at time.Time) func(api.AppRef) bool {
		t.Helper()
		quiet, err := w.quietApps(ctx, m, at)
		if err != nil {
			t.Fatal(err)
		}
		return quiet
	}
	if quietAt(time.Now())(api1) {
		t.Fatal("an app with no operation is quiet")
	}

	// A deploy of api has just ended: api is quiet, the other apps are not.
	admitAndFinish(t, s, "deploy-api", "deploy", "api", "succeeded")
	quiet := quietAt(time.Now())
	if !quiet(api1) || quiet(db) || quiet(tools) {
		t.Fatal("a deploy quiets its own app only")
	}
	// Three minutes later the deploy is long over.
	if quietAt(time.Now().Add(3 * time.Minute))(api1) {
		t.Fatal("an app is quiet long after its operation ended")
	}

	// A restart of db runs: the Compose project of db is quiet while it runs.
	op, _, err := s.Admit(ctx, state.Admission{RequestID: "restart-db", ActorID: "admin", ActorName: "ops", Kind: "restart", App: "db", PublicRequest: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if quiet = quietAt(time.Now()); !quiet(db) || quiet(tools) {
		t.Fatal("a running restart of a Compose app does not quiet its project")
	}
	if err = s.Finish(ctx, op.ID, state.Outcome{State: "succeeded"}); err != nil {
		t.Fatal(err)
	}

	// An exec changes no container, so it quiets nothing.
	admitAndFinish(t, s, "exec-tools", "exec", "tools", "succeeded")
	if quietAt(time.Now())(tools) {
		t.Fatal("an exec quiets its app")
	}

	// A destroy quiets every app while it runs.
	op, _, err = s.Admit(ctx, state.Admission{RequestID: "destroy", ActorID: "admin", ActorName: "ops", Kind: "destroy", PublicRequest: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if quiet = quietAt(time.Now()); !quiet(tools) || !quiet(db) {
		t.Fatal("a running destroy leaves an app judged")
	}
	if err = s.Finish(ctx, op.ID, state.Outcome{State: "succeeded"}); err != nil {
		t.Fatal(err)
	}
}

// admitAndFinish records one finished operation of kind on app.
func admitAndFinish(t *testing.T, s *state.Store, request, kind, app, outcome string) {
	t.Helper()
	ctx := context.Background()
	op, _, err := s.Admit(ctx, state.Admission{RequestID: request, ActorID: "admin", ActorName: "ops", Kind: kind, App: app, PublicRequest: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Finish(ctx, op.ID, state.Outcome{State: outcome}); err != nil {
		t.Fatal(err)
	}
}

func TestDownAppsAddTheManifestProjectsThatHaveNoContainers(t *testing.T) {
	m := &manifest.Manifest{Project: "shop", Apps: map[string]manifest.App{
		"db":     {Kind: "compose"},
		"legacy": {Kind: "compose", ProjectName: "old"},
		"api":    {Kind: "image"},
	}}
	managed := []api.App{
		{Ref: api.AppRef{Kind: "compose", Name: "shop-lab-db"}, Roles: []api.Role{{Name: "db"}}},
		{Ref: api.AppRef{Kind: "image", Name: "api"}},
	}
	got := downApps(managed, m, "lab")
	if len(got) != 3 {
		t.Fatalf("apps %v, want the two managed ones and the project without containers", got)
	}
	last := got[2]
	if last.Ref != (api.AppRef{Kind: "compose", Name: "old"}) || len(last.Roles) != 0 {
		t.Fatalf("project without containers: %+v", last)
	}
	if got := downApps(managed, nil, "lab"); len(got) != 2 {
		t.Fatalf("without a manifest: %v", got)
	}
}
