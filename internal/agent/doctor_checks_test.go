package agent

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"berthy/internal/deploy"
	"berthy/internal/engine"
	"berthy/internal/state"
	"berthy/internal/version"
)

const (
	attachableOverlay = `{"Id":"n1","Name":"NAME","Driver":"overlay","Scope":"swarm","Attachable":true}`
	bridgeNetwork     = `{"Id":"n2","Name":"NAME","Driver":"bridge","Scope":"local","Attachable":false}`
	plainOverlay      = `{"Id":"n3","Name":"NAME","Driver":"overlay","Scope":"swarm","Attachable":false}`
)

// doctorDocker serves, over a Unix socket, the Docker routes the doctor reads: the API version, a Swarm manager, and the
// networks, each answered with its JSON or with a 404 when it is not in networks. It returns the engine and a function that
// lists the network names asked for so far.
func doctorDocker(t *testing.T, networks map[string]string) (*engine.Engine, func() []string) {
	t.Helper()
	return doctorDockerContainers(t, networks, "[]")
}

// doctorDockerContainers is doctorDocker whose containers route answers containers, the JSON of the running containers.
func doctorDockerContainers(t *testing.T, networks map[string]string, containers string) (*engine.Engine, func() []string) {
	t.Helper()
	// The directory is short on purpose: a Unix socket path holds about 108 bytes, and a test's temporary directory can be longer.
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
	var mu sync.Mutex
	var asked []string
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/version":
			w.Write([]byte(`{"ApiVersion":"1.50","MinAPIVersion":"1.24"}`))
		case r.URL.Path == "/v1.44/info":
			w.Write([]byte(`{"LoggingDriver":"json-file","Swarm":{"ControlAvailable":true,"LocalNodeState":"active"}}`))
		case r.URL.Path == "/v1.44/containers/json":
			w.Write([]byte(containers))
		case strings.HasPrefix(r.URL.Path, "/v1.44/networks/"):
			name := strings.TrimPrefix(r.URL.Path, "/v1.44/networks/")
			mu.Lock()
			asked = append(asked, name)
			mu.Unlock()
			body, ok := networks[name]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				w.Write([]byte(`{"message":"network not found"}`))
				return
			}
			w.Write([]byte(strings.ReplaceAll(body, "NAME", name)))
		default:
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"message":"no such route"}`))
		}
	})}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })
	return engine.New(sock), func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), asked...)
	}
}

// doctorConfig is the test environment with the given configured network. Its Compose command cannot run, so the
// doctor's Compose check fails quickly.
func doctorConfig(network string) Config {
	cfg := monitoringConfig("")
	cfg.Network = network
	return cfg
}

func TestAgentInfoReportsItsVersion(t *testing.T) {
	saved := version.Version
	defer func() { version.Version = saved }()
	version.Version = "v1.2.3"
	w := serveRequest(t, Handler(monitoringConfig(""), nil), http.MethodGet, "/v1/projects/demo/environments/lab/agent", monitoringToken("a"))
	var info struct {
		Version string `json:"version"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &info) != nil || info.Version != "v1.2.3" {
		t.Fatalf("agent: %d %s", w.Code, w.Body.String())
	}
}

func TestDoctorStartsWithTheAgentVersion(t *testing.T) {
	saved := version.Version
	defer func() { version.Version = saved }()
	version.Version = "v1.2.3"
	checks := doctorChecks(t, Handler(monitoringConfig(""), nil))
	if len(checks) == 0 || checks[0].Name != "agent" || !checks[0].OK || !checks[0].Critical || checks[0].Detail != "v1.2.3" {
		t.Fatalf("first check: %+v", checks)
	}
}

func TestDoctorNetworkIsOKForAnAttachableSwarmOverlay(t *testing.T) {
	e, _ := doctorDocker(t, map[string]string{"lab-net": attachableOverlay})
	c, ok := findCheck(doctorChecks(t, NewDeploymentHandler(doctorConfig("lab-net"), e, nil, nil)), "network")
	if !ok || !c.OK || !c.Critical || c.Detail != "lab-net" {
		t.Fatalf("network: %+v found %v", c, ok)
	}
}

func TestDoctorNetworkSaysWhatIsWrongWithIt(t *testing.T) {
	for _, tc := range []struct {
		name, body, detail string
	}{
		{"missing", "", "lab-net: missing"},
		{"bridge", bridgeNetwork, "lab-net: not an attachable overlay"},
		{"overlay that is not attachable", plainOverlay, "lab-net: not an attachable overlay"},
	} {
		networks := map[string]string{}
		if tc.body != "" {
			networks["lab-net"] = tc.body
		}
		e, _ := doctorDocker(t, networks)
		c, ok := findCheck(doctorChecks(t, NewDeploymentHandler(doctorConfig("lab-net"), e, nil, nil)), "network")
		if !ok || c.OK || !c.Critical || c.Detail != tc.detail {
			t.Errorf("%s: %+v found %v", tc.name, c, ok)
		}
	}
}

func TestDoctorNetworkWithoutDockerIsNotOK(t *testing.T) {
	c, ok := findCheck(doctorChecks(t, NewDeploymentHandler(doctorConfig("lab-net"), nil, nil, nil)), "network")
	if !ok || c.OK || !c.Critical || c.Detail != "lab-net: unavailable" {
		t.Fatalf("network: %+v found %v", c, ok)
	}
}

func TestDoctorNetworkConfiguredNameWinsOverTheManifest(t *testing.T) {
	e, asked := doctorDocker(t, map[string]string{"cfg-net": attachableOverlay})
	s := openIngressStore(t)
	recordRelease(t, s, "release-network", monitoredManifest(""))
	h := NewDeploymentHandler(doctorConfig("cfg-net"), e, nil, &deploy.Controller{Store: s, Environment: "lab"})
	c, ok := findCheck(doctorChecks(t, h), "network")
	if !ok || !c.OK || c.Detail != "cfg-net" {
		t.Fatalf("network: %+v found %v", c, ok)
	}
	if got := asked(); len(got) != 1 || got[0] != "cfg-net" {
		t.Fatalf("networks asked: %v", got)
	}
}

func TestDoctorNetworkFallsBackToTheLastAppliedManifest(t *testing.T) {
	e, asked := doctorDocker(t, map[string]string{"demo": attachableOverlay})
	s := openIngressStore(t)
	recordRelease(t, s, "release-network", monitoredManifest(""))
	h := NewDeploymentHandler(monitoringConfig(""), e, nil, &deploy.Controller{Store: s, Environment: "lab"})
	c, ok := findCheck(doctorChecks(t, h), "network")
	if !ok || !c.OK || c.Detail != "demo" {
		t.Fatalf("network: %+v found %v", c, ok)
	}
	if got := asked(); len(got) != 1 || got[0] != "demo" {
		t.Fatalf("networks asked: %v", got)
	}
}

func TestDoctorLeavesTheNetworkOutWhenNoneIsKnown(t *testing.T) {
	e, asked := doctorDocker(t, map[string]string{})
	if _, ok := findCheck(doctorChecks(t, NewDeploymentHandler(monitoringConfig(""), e, nil, nil)), "network"); ok {
		t.Error("network check without a configured or applied network")
	}
	h := NewDeploymentHandler(monitoringConfig(""), e, nil, &deploy.Controller{Store: openIngressStore(t), Environment: "lab"})
	if _, ok := findCheck(doctorChecks(t, h), "network"); ok {
		t.Error("network check on a fresh agent with no release")
	}
	if got := asked(); len(got) != 0 {
		t.Errorf("networks asked: %v", got)
	}
}

func TestNetworkConfigUsesTheManifestNameRule(t *testing.T) {
	base := Config{Project: "demo", Environment: "lab", Listen: "127.0.0.1:9999", Tokens: []Token{{Digest: Digest(monitoringToken("a")), Scope: "read"}}}
	for name, valid := range map[string]bool{
		"lab-net":               true,
		"a":                     true,
		strings.Repeat("a", 63): true,
		"":                      true,
		"Lab":                   false,
		"-lab":                  false,
		"lab.net":               false,
		"lab net":               false,
		strings.Repeat("a", 64): false,
		"lab/net":               false,
		"../escape":             false,
	} {
		c := base
		c.Network = name
		if err := c.Validate(); (err == nil) != valid {
			t.Errorf("network %q: valid %v, error %v", name, err == nil, err)
		}
	}
}

func TestNetworkConfigFieldIsNamedNetwork(t *testing.T) {
	var c Config
	if err := json.Unmarshal([]byte(`{"network":"lab-net"}`), &c); err != nil || c.Network != "lab-net" {
		t.Fatalf("decoded %+v: %v", c, err)
	}
}

func TestDoctorReportsTheDeployLockWithoutItsToken(t *testing.T) {
	e, _ := doctorDocker(t, map[string]string{"demo": attachableOverlay})
	s := openIngressStore(t)
	recordRelease(t, s, "release-lock", monitoredManifest(""))
	h := NewDeploymentHandler(monitoringConfig(""), e, nil, &deploy.Controller{Store: s, Environment: "lab"})
	c, ok := findCheck(doctorChecks(t, h), "lock")
	if !ok || !c.OK || c.Critical || c.Detail != "free" {
		t.Fatalf("free lock: %+v found %v", c, ok)
	}

	if err := s.AcquireManual(context.Background(), state.Actor{ID: "admin", Name: "admin"}, "ci-bot", "freeze for release"); err != nil {
		t.Fatal(err)
	}
	w := serveRequest(t, h, http.MethodGet, "/v1/projects/demo/environments/lab/doctor", monitoringToken("a"))
	c, ok = findCheck(doctorChecks(t, h), "lock")
	if !ok || !c.OK || c.Critical || !strings.HasPrefix(c.Detail, "held by ci-bot for ") || !strings.HasSuffix(c.Detail, "freeze for release") {
		t.Fatalf("held lock: %+v found %v", c, ok)
	}
	if strings.Contains(w.Body.String(), monitoringToken("a")) || strings.Contains(w.Body.String(), monitoringToken("b")) {
		t.Fatal("doctor printed a token")
	}
}

func TestDoctorKeepsItsOrderWithTheAgentFirst(t *testing.T) {
	e, _ := doctorDocker(t, map[string]string{"demo": attachableOverlay})
	s := openIngressStore(t)
	recordRelease(t, s, "release-order", monitoredManifest(""))
	h := NewDeploymentHandler(monitoringConfig(""), e, nil, &deploy.Controller{Store: s, Environment: "lab"})
	var names []string
	for _, c := range doctorChecks(t, h) {
		names = append(names, c.Name)
	}
	want := []string{"agent", "host", "disk", "docker", "docker_api", "swarm_manager", "network", "compose", "lock"}
	if len(names) < len(want) {
		t.Fatalf("checks: %v", names)
	}
	for i, name := range want {
		if names[i] != name {
			t.Fatalf("check %d is %q, want %q (all %v)", i, names[i], name, names)
		}
	}
}

func TestDoctorChecksTheDefaultNetwork(t *testing.T) {
	e, asked := doctorDocker(t, map[string]string{"demo-lab": attachableOverlay})
	s := openIngressStore(t)
	recordRelease(t, s, "default-network", strings.Replace(monitoredManifest(""), `,"network":"demo"`, "", 1))
	h := NewDeploymentHandler(monitoringConfig(""), e, nil, &deploy.Controller{Store: s, Environment: "lab"})
	c, ok := findCheck(doctorChecks(t, h), "network")
	if !ok || !c.OK || c.Detail != "demo-lab" {
		t.Fatalf("network: %+v found %v, asked %v", c, ok, asked())
	}
}
