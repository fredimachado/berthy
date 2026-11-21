package agent

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"debug/elf"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"berthy/internal/engine"
	"berthy/internal/monitor"
	"berthy/internal/scope"
)

// hfClock is the helper's clock. Tests move it forward to pass the one-minute limit between runs.
type hfClock struct{ t time.Time }

func (c *hfClock) now() time.Time { return c.t }

// hfSpec is the part of a container create request that the fake Docker reads back.
type hfSpec struct {
	Image  string
	User   string
	Env    []string
	Labels map[string]string
	// HostConfig and NetworkingConfig are the settings the helper asks for.
	HostConfig struct {
		NetworkMode    string
		ReadonlyRootfs bool
		CapDrop        []string
		SecurityOpt    []string
		Memory         int64
		RestartPolicy  struct{ Name string }
		PortBindings   map[string][]engine.PortBinding
	}
	NetworkingConfig struct {
		EndpointsConfig map[string]any
	}
}

// hfContainer is a container of the fake Docker.
type hfContainer struct {
	id, name, imageID string
	spec              hfSpec
	running           bool
	ports             map[string][]engine.PortBinding
	// netIDs are the IDs of the networks the container was attached to when it was created.
	netIDs map[string]string
}

// hfDocker is an in-memory Docker for the helper. It keeps networks, images and containers, and reports the settings a
// container was created with, the way Docker does, with the host port assigned when the container starts.
type hfDocker struct {
	networks   map[string]engine.Network
	images     map[string]string // reference to image ID
	containers map[string]*hfContainer
	names      map[string]string // container name to ID
	calls      []string
	specs      []json.RawMessage
	imported   []hfImport
	removed    []string // image references removed
	nextID     int
	nextPort   int
	failStart  bool
}

type hfImport struct {
	repo, tag string
	changes   []string
	archive   []byte
}

func newHfDocker() *hfDocker {
	return &hfDocker{
		networks:   map[string]engine.Network{"lab-net": {ID: "net-1", Name: "lab-net", Scope: "swarm", Attachable: true}},
		images:     map[string]string{},
		containers: map[string]*hfContainer{},
		names:      map[string]string{},
		nextPort:   40000,
	}
}

func (f *hfDocker) call(name string) { f.calls = append(f.calls, name) }

// createCount returns how many containers were created.
func (f *hfDocker) createCount() int {
	n := 0
	for _, c := range f.calls {
		if c == "CreateContainer" {
			n++
		}
	}
	return n
}

// only returns the one container, failing the test when there is not exactly one.
func (f *hfDocker) only(t *testing.T) *hfContainer {
	t.Helper()
	if len(f.containers) != 1 {
		t.Fatalf("%d containers, want 1", len(f.containers))
	}
	for _, c := range f.containers {
		return c
	}
	return nil
}

func (f *hfDocker) InspectNetwork(_ context.Context, name string) (engine.Network, error) {
	f.call("InspectNetwork")
	n, ok := f.networks[name]
	if !ok {
		return engine.Network{}, &engine.StatusError{Code: http.StatusNotFound}
	}
	return n, nil
}

func (f *hfDocker) InspectImage(_ context.Context, ref string) (engine.Image, error) {
	f.call("InspectImage")
	id, ok := f.images[ref]
	if !ok {
		return engine.Image{}, &engine.StatusError{Code: http.StatusNotFound}
	}
	return engine.Image{ID: id}, nil
}

func (f *hfDocker) ImportImage(_ context.Context, repo, tag string, changes []string, archive []byte) error {
	f.call("ImportImage")
	f.imported = append(f.imported, hfImport{repo: repo, tag: tag, changes: changes, archive: archive})
	f.images[repo+":"+tag] = fmt.Sprintf("sha256:%060x", len(f.imported))
	return nil
}

func (f *hfDocker) InventoryImages(context.Context) ([]engine.InventoryImage, error) {
	f.call("InventoryImages")
	byID := map[string][]string{}
	var order []string
	for ref, id := range f.images {
		if _, seen := byID[id]; !seen {
			order = append(order, id)
		}
		byID[id] = append(byID[id], ref)
	}
	var out []engine.InventoryImage
	for _, id := range order {
		out = append(out, engine.InventoryImage{ID: id, RepoTags: byID[id]})
	}
	return out, nil
}

// RemoveUnusedImage refuses an image that any container uses, as Docker does without force.
func (f *hfDocker) RemoveUnusedImage(_ context.Context, ref string) error {
	f.call("RemoveUnusedImage")
	id, ok := f.images[ref]
	if !ok {
		return &engine.StatusError{Code: http.StatusNotFound}
	}
	for _, c := range f.containers {
		if c.imageID == id {
			return &engine.StatusError{Code: http.StatusConflict}
		}
	}
	delete(f.images, ref)
	f.removed = append(f.removed, ref)
	return nil
}

func (f *hfDocker) CreateContainer(_ context.Context, name string, spec json.RawMessage) (string, error) {
	f.call("CreateContainer")
	if _, taken := f.names[name]; taken {
		return "", &engine.StatusError{Code: http.StatusConflict}
	}
	var s hfSpec
	if err := json.Unmarshal(spec, &s); err != nil {
		return "", err
	}
	imageID, ok := f.images[s.Image]
	if !ok {
		return "", &engine.StatusError{Code: http.StatusNotFound}
	}
	f.nextID++
	id := fmt.Sprintf("c%03d", f.nextID)
	netIDs := map[string]string{}
	for net := range s.NetworkingConfig.EndpointsConfig {
		netIDs[net] = f.networks[net].ID
	}
	f.containers[id] = &hfContainer{id: id, name: name, imageID: imageID, spec: s, netIDs: netIDs}
	f.names[name] = id
	f.specs = append(f.specs, spec)
	return id, nil
}

func (f *hfDocker) StartContainer(_ context.Context, id string) error {
	f.call("StartContainer")
	c, err := f.get(id)
	if err != nil {
		return err
	}
	if f.failStart {
		return errors.New("start refused")
	}
	c.running = true
	c.ports = map[string][]engine.PortBinding{}
	for key, bindings := range c.spec.HostConfig.PortBindings {
		for _, b := range bindings {
			if b.HostPort == "" {
				f.nextPort++
				b.HostPort = strconv.Itoa(f.nextPort)
			}
			c.ports[key] = append(c.ports[key], b)
		}
	}
	return nil
}

func (f *hfDocker) InspectContainerDetail(_ context.Context, ref string) (engine.ContainerDetail, error) {
	f.call("InspectContainerDetail")
	c, err := f.get(ref)
	if err != nil {
		return engine.ContainerDetail{}, err
	}
	return engine.ContainerDetail{
		ID: c.id, ImageID: c.imageID, User: c.spec.User, Labels: c.spec.Labels, Running: c.running,
		ReadonlyRootfs: c.spec.HostConfig.ReadonlyRootfs, CapDrop: c.spec.HostConfig.CapDrop,
		SecurityOpt: c.spec.HostConfig.SecurityOpt, Memory: c.spec.HostConfig.Memory,
		RestartPolicy: c.spec.HostConfig.RestartPolicy.Name, PortBindings: c.spec.HostConfig.PortBindings,
		Ports: c.ports, Networks: c.netIDs,
	}, nil
}

func (f *hfDocker) StopContainer(_ context.Context, id string, _ int) error {
	f.call("StopContainer")
	c, err := f.get(id)
	if err != nil {
		return err
	}
	if !c.running {
		return &engine.StatusError{Code: http.StatusNotModified}
	}
	c.running = false
	return nil
}

func (f *hfDocker) RemoveContainer(_ context.Context, id string) error {
	f.call("RemoveContainer")
	c, err := f.get(id)
	if err != nil {
		return err
	}
	if c.running {
		return &engine.StatusError{Code: http.StatusConflict}
	}
	delete(f.containers, c.id)
	delete(f.names, c.name)
	return nil
}

// get returns the container with the given ID or name, or a 404.
func (f *hfDocker) get(ref string) (*hfContainer, error) {
	if c, ok := f.containers[ref]; ok {
		return c, nil
	}
	if id, ok := f.names[ref]; ok {
		return f.containers[id], nil
	}
	return nil, &engine.StatusError{Code: http.StatusNotFound}
}

// seed adds a container that Berthy did not create, with the given name, labels and image.
func (f *hfDocker) seed(name string, labels map[string]string, image string) {
	f.nextID++
	id := fmt.Sprintf("c%03d", f.nextID)
	c := &hfContainer{id: id, name: name, imageID: f.images[image], running: true, netIDs: map[string]string{}}
	c.spec.Labels = labels
	f.containers[id] = c
	f.names[name] = id
}

// hfELF returns a minimal little-endian ELF64 executable with one program header per type in progs.
func hfELF(progs ...elf.ProgType) []byte {
	const ehsize, phentsize = 64, 56
	b := make([]byte, ehsize+phentsize*len(progs))
	copy(b, []byte{0x7f, 'E', 'L', 'F', 2, 1, 1, 0})
	le := binary.LittleEndian
	le.PutUint16(b[16:], uint16(elf.ET_EXEC))
	le.PutUint16(b[18:], uint16(elf.EM_X86_64))
	le.PutUint32(b[20:], 1)
	le.PutUint64(b[32:], ehsize) // program header offset
	le.PutUint16(b[52:], ehsize)
	le.PutUint16(b[54:], phentsize)
	le.PutUint16(b[56:], uint16(len(progs)))
	for i, p := range progs {
		off := ehsize + phentsize*i
		le.PutUint32(b[off:], uint32(p))
		le.PutUint32(b[off+4:], uint32(elf.PF_R))
		le.PutUint64(b[off+16:], 0x400000)
		le.PutUint64(b[off+24:], 0x400000)
		le.PutUint64(b[off+48:], 8)
	}
	return b
}

// writeAgentBinary writes a static executable whose content is version, and returns its path. Each version has its own
// digest, so each names its own helper image.
func writeAgentBinary(t *testing.T, version string, progs ...elf.ProgType) string {
	t.Helper()
	if len(progs) == 0 {
		progs = []elf.ProgType{elf.PT_LOAD}
	}
	path := filepath.Join(t.TempDir(), "berthy")
	if err := os.WriteFile(path, append(hfELF(progs...), []byte(version)...), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// newHfHelper returns the helper of environment lab, on the fake Docker f, for the agent binary at exe.
func newHfHelper(f *hfDocker, exe string, clk *hfClock) *helperManager {
	m := newHelperManager(scope.ID{Environment: "lab"}, nil, nil)
	m.engine = f
	m.exe = func() (string, error) { return exe, nil }
	m.goos = "linux"
	m.now = clk.now
	// The fake Docker publishes ports that no server listens on, so a new helper is taken as listening.
	m.await = func(context.Context, string, string) error { return nil }
	return m
}

func TestHelperOfAnotherEnvironmentIsNeverTouched(t *testing.T) {
	for _, labels := range []map[string]string{
		{"berthy.project": "shop", "berthy.environment": "lab", "berthy.helper": "probe"},
		{"berthy.environment": "prod", "berthy.helper": "probe"},
	} {
		f := newHfDocker()
		m := newHfHelper(f, writeAgentBinary(t, "one"), &hfClock{t: time.Unix(1_700_000_000, 0)})
		f.seed(m.name(), labels, "")
		if _, _, err := m.lookup(context.Background()); err == nil {
			t.Errorf("accepted foreign helper: %v", labels)
		}
	}
}

func TestHelperNameOfTheLongestEnvironmentIsAValidDockerName(t *testing.T) {
	id := scope.ID{Project: strings.Repeat("p", 30), Environment: strings.Repeat("e", scope.MaxIDLength-31)}
	if name := newHelperManager(id, nil, nil).name(); !engine.ValidName(name) {
		t.Errorf("name %q (%d characters) is not a valid Docker identifier", name, len(name))
	}
}

func TestHelperNameCarriesProjectAndEnvironment(t *testing.T) {
	for _, id := range []scope.ID{{Project: "shop", Environment: "lab"}, {Project: "blog", Environment: "lab"}, {Project: "shop", Environment: "prod"}} {
		m := newHelperManager(id, nil, nil)
		if want := "berthy-helper-" + id.Project + "." + id.Environment; m.name() != want {
			t.Errorf("name %q, want %q", m.name(), want)
		}
	}
}

// tarEntries returns the regular files of a tar archive by name, with their modes and contents.
func tarEntries(t *testing.T, archive []byte) map[string]struct {
	mode int64
	data string
} {
	t.Helper()
	out := map[string]struct {
		mode int64
		data string
	}{}
	tr := tar.NewReader(bytes.NewReader(archive))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		out[h.Name] = struct {
			mode int64
			data string
		}{h.Mode, string(data)}
	}
}

func TestEnsureBuildsTheHelperFromTheAgentBinary(t *testing.T) {
	f := newHfDocker()
	exe := writeAgentBinary(t, "one")
	clk := &hfClock{t: time.Unix(1_700_000_000, 0)}
	m := newHfHelper(f, exe, clk)
	ctx := context.Background()
	if err := m.ensure(ctx, "lab-net"); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	tag := hex.EncodeToString(sum[:])[:12]

	if len(f.imported) != 1 || f.imported[0].repo != "berthy-helper" || f.imported[0].tag != tag {
		t.Fatalf("imports: %+v", f.imported)
	}
	instructions := strings.Join(f.imported[0].changes, "\n")
	for _, want := range []string{`ENTRYPOINT ["/berthy","probe-helper"]`, "USER 65532:65532", "EXPOSE 7079/tcp"} {
		if !strings.Contains(instructions, want) {
			t.Errorf("image instructions lack %q: %v", want, f.imported[0].changes)
		}
	}
	entries := tarEntries(t, f.imported[0].archive)
	if len(entries) != 1 || entries["berthy"].mode != 0o755 || entries["berthy"].data != string(content) {
		t.Fatalf("image archive holds %d entries, not the binary alone", len(entries))
	}

	c := f.only(t)
	if c.name != "berthy-helper-.lab" || !c.running || c.imageID != f.images["berthy-helper:"+tag] {
		t.Fatalf("container %+v", c)
	}
	s := c.spec
	if s.Image != "berthy-helper:"+tag || s.User != "65532:65532" {
		t.Errorf("image %q user %q", s.Image, s.User)
	}
	if !reflect.DeepEqual(s.Labels, map[string]string{"berthy.project": "", "berthy.environment": "lab", "berthy.helper": "probe"}) {
		t.Errorf("labels %v", s.Labels)
	}
	if s.HostConfig.NetworkMode != "lab-net" || !reflect.DeepEqual(s.NetworkingConfig.EndpointsConfig, map[string]any{"lab-net": map[string]any{}}) {
		t.Errorf("network %q %v", s.HostConfig.NetworkMode, s.NetworkingConfig.EndpointsConfig)
	}
	hc := s.HostConfig
	if !hc.ReadonlyRootfs || !reflect.DeepEqual(hc.CapDrop, []string{"ALL"}) || !reflect.DeepEqual(hc.SecurityOpt, []string{"no-new-privileges"}) {
		t.Errorf("security: readonly %v capdrop %v secopt %v", hc.ReadonlyRootfs, hc.CapDrop, hc.SecurityOpt)
	}
	if hc.Memory != 64<<20 || hc.RestartPolicy.Name != "unless-stopped" {
		t.Errorf("memory %d restart %q", hc.Memory, hc.RestartPolicy.Name)
	}
	if b := hc.PortBindings["7079/tcp"]; len(hc.PortBindings) != 1 || len(b) != 1 || b[0].HostIP != "127.0.0.1" || b[0].HostPort != "" {
		t.Errorf("port bindings %+v", hc.PortBindings)
	}
	if len(s.Env) != 1 || !strings.HasPrefix(s.Env[0], "BERTHY_HELPER_TOKEN=") {
		t.Fatalf("env %v", s.Env)
	}
	token := strings.TrimPrefix(s.Env[0], "BERTHY_HELPER_TOKEN=")
	if len(token) != 64 || token != m.token || !m.holds(c.id) {
		t.Errorf("token %d characters, held %v", len(token), m.holds(c.id))
	}
	if got := c.ports["7079/tcp"]; len(got) != 1 || got[0].HostIP != "127.0.0.1" || got[0].HostPort != m.port || m.port == "" {
		t.Errorf("published %+v, helper port %q", got, m.port)
	}
	if !strings.Contains(string(f.specs[0]), token) {
		t.Errorf("the token is not in the create request")
	}
}

func TestEnsureKeepsAHelperItMadeWithItsToken(t *testing.T) {
	f := newHfDocker()
	clk := &hfClock{t: time.Unix(1_700_000_000, 0)}
	m := newHfHelper(f, writeAgentBinary(t, "one"), clk)
	ctx := context.Background()
	if err := m.ensure(ctx, "lab-net"); err != nil {
		t.Fatal(err)
	}
	id := m.id
	calls := len(f.calls)
	clk.t = clk.t.Add(30 * time.Second)
	if err := m.ensure(ctx, "lab-net"); err != nil || len(f.calls) != calls {
		t.Fatalf("ensure inside a minute: %v, %d new calls", err, len(f.calls)-calls)
	}
	clk.t = clk.t.Add(31 * time.Second)
	if err := m.ensure(ctx, "lab-net"); err != nil {
		t.Fatal(err)
	}
	if m.id != id || f.createCount() != 1 || len(f.imported) != 1 {
		t.Fatalf("a matching helper was recreated: id %s -> %s, creates %d, imports %d", id, m.id, f.createCount(), len(f.imported))
	}
}

func TestEnsureRecreatesWhatItCannotVouchFor(t *testing.T) {
	ctx := context.Background()
	cases := map[string]struct {
		// change alters the state between two runs.
		change func(t *testing.T, f *hfDocker, exe string)
	}{
		"agent restarted, token gone": {
			change: func(t *testing.T, f *hfDocker, exe string) {}, // a new manager has no token
		},
		"container stopped": {
			change: func(t *testing.T, f *hfDocker, exe string) {
				for _, c := range f.containers {
					c.running = false
				}
			},
		},
		"settings drifted": {
			change: func(t *testing.T, f *hfDocker, exe string) {
				for _, c := range f.containers {
					c.spec.HostConfig.ReadonlyRootfs = false
				}
			},
		},
		"published on every address": {
			change: func(t *testing.T, f *hfDocker, exe string) {
				for _, c := range f.containers {
					c.spec.HostConfig.PortBindings["7079/tcp"][0].HostIP = ""
					c.ports["7079/tcp"][0].HostIP = "0.0.0.0"
				}
			},
		},
		"network replaced": {
			change: func(t *testing.T, f *hfDocker, exe string) {
				f.networks["lab-net"] = engine.Network{ID: "net-2", Name: "lab-net", Attachable: true}
			},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := newHfDocker()
			exe := writeAgentBinary(t, "one")
			clk := &hfClock{t: time.Unix(1_700_000_000, 0)}
			m := newHfHelper(f, exe, clk)
			if err := m.ensure(ctx, "lab-net"); err != nil {
				t.Fatal(err)
			}
			before := f.only(t).id
			tc.change(t, f, exe)
			clk.t = clk.t.Add(time.Minute)
			m2 := m
			if name == "agent restarted, token gone" {
				m2 = newHfHelper(f, exe, clk)
			}
			if err := m2.ensure(ctx, "lab-net"); err != nil {
				t.Fatal(err)
			}
			after := f.only(t)
			if after.id == before || !after.running {
				t.Fatalf("helper %s kept or not running (was %s)", after.id, before)
			}
		})
	}
}

func TestEnsureReplacesTheHelperOfAnotherAgentBinary(t *testing.T) {
	f := newHfDocker()
	exe := writeAgentBinary(t, "one")
	clk := &hfClock{t: time.Unix(1_700_000_000, 0)}
	m := newHfHelper(f, exe, clk)
	ctx := context.Background()
	if err := m.ensure(ctx, "lab-net"); err != nil {
		t.Fatal(err)
	}
	oldTag := m.binary.tag()
	if err := os.WriteFile(exe, append(hfELF(elf.PT_LOAD), []byte("version-two")...), 0o755); err != nil {
		t.Fatal(err)
	}
	clk.t = clk.t.Add(time.Minute)
	if err := m.ensure(ctx, "lab-net"); err != nil {
		t.Fatal(err)
	}
	newTag := m.binary.tag()
	if newTag == oldTag || len(f.imported) != 2 || f.createCount() != 2 {
		t.Fatalf("tags %s -> %s, imports %d, creates %d", oldTag, newTag, len(f.imported), f.createCount())
	}
	if _, ok := f.images["berthy-helper:"+oldTag]; ok || !contains(f.removed, "berthy-helper:"+oldTag) {
		t.Fatalf("the old helper image was kept: removed %v", f.removed)
	}
	if _, ok := f.images["berthy-helper:"+newTag]; !ok {
		t.Fatal("the new helper image is missing")
	}
	if c := f.only(t); c.imageID != f.images["berthy-helper:"+newTag] {
		t.Fatal("the helper does not run the new image")
	}
}

func TestEnsureReportsWhatBlocksTheHelper(t *testing.T) {
	ctx := context.Background()
	t.Run("network missing", func(t *testing.T) {
		f := newHfDocker()
		delete(f.networks, "lab-net")
		m := newHfHelper(f, writeAgentBinary(t, "one"), &hfClock{t: time.Unix(1_700_000_000, 0)})
		if err := m.ensure(ctx, "lab-net"); err == nil || err.Error() != "probe helper unavailable: network unavailable" {
			t.Fatalf("error %v", err)
		}
	})
	t.Run("network not attachable", func(t *testing.T) {
		f := newHfDocker()
		f.networks["lab-net"] = engine.Network{ID: "net-1", Attachable: false}
		m := newHfHelper(f, writeAgentBinary(t, "one"), &hfClock{t: time.Unix(1_700_000_000, 0)})
		if err := m.ensure(ctx, "lab-net"); err == nil || err.Error() != "probe helper unavailable: network is not attachable" {
			t.Fatalf("error %v", err)
		}
		if f.createCount() != 0 {
			t.Fatal("a container was created on a network that cannot take one")
		}
	})
	t.Run("name held by another container", func(t *testing.T) {
		f := newHfDocker()
		f.seed("berthy-helper-.lab", map[string]string{"berthy.helper": "other"}, "")
		m := newHfHelper(f, writeAgentBinary(t, "one"), &hfClock{t: time.Unix(1_700_000_000, 0)})
		if err := m.ensure(ctx, "lab-net"); err == nil || err.Error() != "probe helper unavailable: name is used by another container" {
			t.Fatalf("error %v", err)
		}
		if _, ok := f.names["berthy-helper-.lab"]; !ok || f.createCount() != 0 {
			t.Fatal("another container's name was taken over")
		}
	})
	t.Run("container does not start", func(t *testing.T) {
		f := newHfDocker()
		f.failStart = true
		m := newHfHelper(f, writeAgentBinary(t, "one"), &hfClock{t: time.Unix(1_700_000_000, 0)})
		if err := m.ensure(ctx, "lab-net"); err == nil || err.Error() != "probe helper unavailable: container start failed" {
			t.Fatalf("error %v", err)
		}
		if len(f.containers) != 0 || m.token != "" || m.holds("c001") {
			t.Fatal("a container that did not start was kept")
		}
	})
	t.Run("binary not static", func(t *testing.T) {
		f := newHfDocker()
		m := newHfHelper(f, writeAgentBinary(t, "one", elf.PT_PHDR, elf.PT_INTERP, elf.PT_LOAD), &hfClock{t: time.Unix(1_700_000_000, 0)})
		if err := m.ensure(ctx, "lab-net"); err == nil || err.Error() != "service probes need a statically linked berthy binary" {
			t.Fatalf("error %v", err)
		}
		if len(f.calls) != 0 {
			t.Fatalf("Docker was asked %v for a binary that cannot run", f.calls)
		}
	})
	t.Run("not Linux", func(t *testing.T) {
		f := newHfDocker()
		m := newHfHelper(f, writeAgentBinary(t, "one"), &hfClock{t: time.Unix(1_700_000_000, 0)})
		m.goos = "windows"
		if err := m.ensure(ctx, "lab-net"); err == nil || err.Error() != "service probes need a Linux agent" {
			t.Fatalf("error %v", err)
		}
		if len(f.calls) != 0 {
			t.Fatal("Docker was asked on a non-Linux agent")
		}
	})
	t.Run("no Docker", func(t *testing.T) {
		m := newHelperManager(scope.ID{Environment: "lab"}, nil, nil)
		m.exe = func() (string, error) { return writeAgentBinary(t, "one"), nil }
		m.goos = "linux"
		if err := m.ensure(ctx, "lab-net"); err == nil || err.Error() != "probe helper unavailable: Docker unavailable" {
			t.Fatalf("error %v", err)
		}
	})
}

func TestEnsureRunsOncePerMinute(t *testing.T) {
	f := newHfDocker()
	f.failStart = true
	clk := &hfClock{t: time.Unix(1_700_000_000, 0)}
	m := newHfHelper(f, writeAgentBinary(t, "one"), clk)
	ctx := context.Background()
	first := m.ensure(ctx, "lab-net")
	calls := len(f.calls)
	clk.t = clk.t.Add(59 * time.Second)
	if err := m.ensure(ctx, "lab-net"); err == nil || err.Error() != first.Error() || len(f.calls) != calls {
		t.Fatalf("a second run inside the minute: %v, %d calls", err, len(f.calls)-calls)
	}
}

func TestRemoveDeletesTheHelperAndItsUnusedImages(t *testing.T) {
	f := newHfDocker()
	clk := &hfClock{t: time.Unix(1_700_000_000, 0)}
	m := newHfHelper(f, writeAgentBinary(t, "one"), clk)
	ctx := context.Background()
	if err := m.ensure(ctx, "lab-net"); err != nil {
		t.Fatal(err)
	}
	current := m.binary.tag()
	// An older helper image nothing uses, and one a container still runs from.
	f.images["berthy-helper:aaaaaaaaaaaa"] = "sha256:old"
	f.images["berthy-helper:bbbbbbbbbbbb"] = "sha256:busy"
	f.seed("other", map[string]string{"berthy.environment": "other"}, "berthy-helper:bbbbbbbbbbbb")
	clk.t = clk.t.Add(time.Minute)
	if err := m.remove(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.names["berthy-helper-.lab"]; ok {
		t.Fatal("the helper container was kept")
	}
	if _, ok := f.images["berthy-helper:"+current]; ok || !contains(f.removed, "berthy-helper:"+current) {
		t.Fatalf("the helper's image was kept: removed %v", f.removed)
	}
	if !contains(f.removed, "berthy-helper:aaaaaaaaaaaa") {
		t.Fatalf("the unused old image was kept: removed %v", f.removed)
	}
	if _, ok := f.images["berthy-helper:bbbbbbbbbbbb"]; !ok || contains(f.removed, "berthy-helper:bbbbbbbbbbbb") {
		t.Fatal("an image a running container uses was removed")
	}
	if _, ok := f.names["other"]; !ok {
		t.Fatal("another container was removed")
	}
	// Once no helper is left, a run inside the minute does nothing, and a later one finds nothing to clean.
	calls := len(f.calls)
	if err := m.remove(ctx); err != nil || len(f.calls) != calls {
		t.Fatalf("remove inside a minute: %v, %d calls", err, len(f.calls)-calls)
	}
}

func TestRemoveWithoutAHelperOnlyCleansOnce(t *testing.T) {
	f := newHfDocker()
	clk := &hfClock{t: time.Unix(1_700_000_000, 0)}
	m := newHfHelper(f, writeAgentBinary(t, "one"), clk)
	ctx := context.Background()
	if err := m.remove(ctx); err != nil {
		t.Fatal(err)
	}
	calls := len(f.calls)
	clk.t = clk.t.Add(time.Minute)
	if err := m.remove(ctx); err != nil {
		t.Fatal(err)
	}
	if got := f.calls[calls:]; !reflect.DeepEqual(got, []string{"InspectContainerDetail"}) {
		t.Fatalf("calls after the first clean: %v", got)
	}
}

func TestProbeRunsOnTheHelperWithItsToken(t *testing.T) {
	var got monitor.HelperProbe
	var seenAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth = r.Header.Get("Authorization")
		if seenAuth != "Bearer tok-123" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Error(err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "detail": "HTTP 503"})
	}))
	defer srv.Close()
	m := newHelperManager(scope.ID{Environment: "lab"}, nil, nil)
	m.token, m.port = "tok-123", serverPort(t, srv)
	req := monitor.HelperProbe{Kind: "http", Target: "web:8080", Path: "/health", TimeoutMS: 2000, Expect: []int{200}}
	ok, detail, err := m.Probe(context.Background(), req)
	if err != nil || ok || detail != "HTTP 503" {
		t.Fatalf("probe: %v %v %q", ok, err, detail)
	}
	if !reflect.DeepEqual(got, req) || seenAuth != "Bearer tok-123" {
		t.Fatalf("request %+v with %q", got, seenAuth)
	}
}

func TestProbeMarksAHelperItCannotReachOrTrustLost(t *testing.T) {
	ctx := context.Background()
	refuse := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer refuse.Close()
	m := newHelperManager(scope.ID{Environment: "lab"}, nil, nil)
	m.token, m.port = "stale", serverPort(t, refuse)
	req := monitor.HelperProbe{Kind: "tcp", Target: "web:80", TimeoutMS: 1000}
	if _, _, err := m.Probe(ctx, req); err == nil || err.Error() != "probe helper unavailable: token rejected" {
		t.Fatalf("refused token: %v", err)
	}
	if m.token != "" {
		t.Fatal("a helper that refused the token is still trusted")
	}
	if _, _, err := m.Probe(ctx, req); err == nil || err.Error() != "probe helper unavailable: token rejected" {
		t.Fatalf("after the refusal: %v", err)
	}

	gone := newHelperManager(scope.ID{Environment: "lab"}, nil, nil)
	gone.token, gone.port = "tok", unansweringPort(t)
	if _, _, err := gone.Probe(ctx, req); err == nil || err.Error() != "probe helper unavailable: not answering" || gone.token != "" {
		t.Fatalf("no answer: %v, token kept %v", err, gone.token != "")
	}
}

func TestProbeWithoutAHelperSaysWhy(t *testing.T) {
	ctx := context.Background()
	req := monitor.HelperProbe{Kind: "tcp", Target: "web:80", TimeoutMS: 1000}
	m := newHelperManager(scope.ID{Environment: "lab"}, nil, nil)
	if _, _, err := m.Probe(ctx, req); err == nil || err.Error() != "probe helper unavailable: not running" {
		t.Fatalf("never started: %v", err)
	}
	f := newHfDocker()
	f.failStart = true
	failed := newHfHelper(f, writeAgentBinary(t, "one"), &hfClock{t: time.Unix(1_700_000_000, 0)})
	_ = failed.ensure(ctx, "lab-net")
	if _, _, err := failed.Probe(ctx, req); err == nil || err.Error() != "probe helper unavailable: container start failed" {
		t.Fatalf("after a failed run: %v", err)
	}
}

func TestStatusIsTheDoctorsView(t *testing.T) {
	ctx := context.Background()
	f := newHfDocker()
	clk := &hfClock{t: time.Unix(1_700_000_000, 0)}
	m := newHfHelper(f, writeAgentBinary(t, "one"), clk)
	if ok, detail := m.status(ctx, "lab-net"); ok || detail != "probe helper unavailable: image not built yet" {
		t.Fatalf("before any run: %v %q", ok, detail)
	}
	if err := m.ensure(ctx, "lab-net"); err != nil {
		t.Fatal(err)
	}
	if ok, detail := m.status(ctx, "lab-net"); !ok || detail != "running" {
		t.Fatalf("running: %v %q", ok, detail)
	}
	for _, c := range f.containers {
		c.running = false
	}
	if ok, detail := m.status(ctx, "lab-net"); ok || detail != "probe helper unavailable: container not running" {
		t.Fatalf("stopped: %v %q", ok, detail)
	}
	f.containers = map[string]*hfContainer{}
	f.names = map[string]string{}
	if ok, detail := newHfHelper(f, writeAgentBinary(t, "one"), clk).status(ctx, "lab-net"); ok || detail != "probe helper unavailable: not created yet" {
		t.Fatalf("missing: %v %q", ok, detail)
	}
	if ok, _ := m.status(ctx, "absent-net"); ok {
		t.Fatal("a missing network reported running")
	}
}

func TestBinaryInfoRereadsWhatChanged(t *testing.T) {
	exe := writeAgentBinary(t, "one")
	m := newHelperManager(scope.ID{Environment: "lab"}, nil, nil)
	m.exe = func() (string, error) { return exe, nil }
	first, err := m.binaryInfo()
	if err != nil || !first.static || len(first.digest) != 64 {
		t.Fatalf("binary %+v: %v", first, err)
	}
	if again, _ := m.binaryInfo(); again.digest != first.digest {
		t.Fatal("the same file gave another digest")
	}
	// Same size, different bytes, older time: the cached digest must not be trusted.
	st, err := os.Stat(exe)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(exe, append(hfELF(elf.PT_LOAD), []byte("ONE")...), 0o755); err != nil {
		t.Fatal(err)
	}
	if err = os.Chtimes(exe, st.ModTime().Add(-time.Hour), st.ModTime().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := readBinary(first); err == nil || err.Error() != "probe helper unavailable: agent binary changed" {
		t.Fatalf("a changed binary was accepted: %v", err)
	}
}

func TestStaticELFTellsStaticFromDynamic(t *testing.T) {
	cases := map[string]struct {
		data   []byte
		static bool
	}{
		"loadable only":        {hfELF(elf.PT_LOAD, elf.PT_GNU_STACK), true},
		"with an interpreter":  {hfELF(elf.PT_PHDR, elf.PT_INTERP, elf.PT_LOAD), false},
		"no loadable segments": {hfELF(elf.PT_NOTE), false},
		"a script":             {[]byte("#!/bin/sh\necho hi\n"), false},
		"nothing":              {nil, false},
	}
	for name, tc := range cases {
		if got := staticELF(bytes.NewReader(tc.data)); got != tc.static {
			t.Errorf("%s: static %v, want %v", name, got, tc.static)
		}
	}
}

func TestStaticELFAgreesWithTheTestBinary(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(exe)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	ef, err := elf.NewFile(f)
	if err != nil {
		t.Skip("the test binary is not ELF on this platform")
	}
	interpreter := false
	for _, p := range ef.Progs {
		interpreter = interpreter || p.Type == elf.PT_INTERP
	}
	if got := staticELF(f); got == interpreter {
		t.Fatalf("the test binary: static %v with an interpreter %v", got, interpreter)
	}
}

func TestAwaitHelperWaitsForTheHelperToListen(t *testing.T) {
	ctx := context.Background()
	m := newHelperManager(scope.ID{Environment: "lab"}, nil, nil)
	// A helper that answers the empty probe with 400 is up and knows the token.
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer up.Close()
	if err := m.awaitHelper(ctx, serverPort(t, up), "tok"); err != nil {
		t.Fatalf("listening helper: %v", err)
	}
	if err := m.awaitHelper(ctx, serverPort(t, up), "other"); err == nil || err.Error() != "probe helper unavailable: token rejected" {
		t.Fatalf("helper with another token: %v", err)
	}
	// Nothing listens on the port: the wait gives up when its context ends.
	short, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	if err := m.awaitHelper(short, unansweringPort(t), "tok"); err == nil || err.Error() != "probe helper unavailable: not answering" {
		t.Fatalf("silent helper: %v", err)
	}
}

func TestHelperArchiveHoldsTheBinaryAlone(t *testing.T) {
	archive, err := helperArchive([]byte("binary"))
	if err != nil {
		t.Fatal(err)
	}
	entries := tarEntries(t, archive)
	if len(entries) != 1 || entries["berthy"].data != "binary" || entries["berthy"].mode != 0o755 {
		t.Fatalf("entries %+v", entries)
	}
}

func TestHelperSpecAsksForTheHardenedSettings(t *testing.T) {
	raw, err := helperSpec(scope.ID{Environment: "lab"}, "lab-net", "berthy-helper:0123456789ab", strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	var spec map[string]any
	if err = json.Unmarshal(raw, &spec); err != nil {
		t.Fatal(err)
	}
	hc := spec["HostConfig"].(map[string]any)
	if hc["ReadonlyRootfs"] != true || hc["NetworkMode"] != "lab-net" || hc["Memory"].(float64) != 64<<20 {
		t.Fatalf("host config %v", hc)
	}
	if _, ok := hc["Privileged"]; ok {
		t.Fatal("privileged is set")
	}
	if !reflect.DeepEqual(hc["CapDrop"], []any{"ALL"}) || !reflect.DeepEqual(hc["SecurityOpt"], []any{"no-new-privileges"}) {
		t.Fatalf("capabilities %v security %v", hc["CapDrop"], hc["SecurityOpt"])
	}
	if !reflect.DeepEqual(hc["RestartPolicy"], map[string]any{"Name": "unless-stopped"}) {
		t.Fatalf("restart %v", hc["RestartPolicy"])
	}
}

// serverPort returns the port a test server listens on.
func serverPort(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return u.Port()
}

// unansweringPort returns a loopback port whose server never answers a request: it accepts each connection and closes it at
// once. The listener stays open until the test ends, so no other test's server can take the port, which a port that was
// freed again could be under load.
func unansweringPort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	_, port, _ := net.SplitHostPort(l.Addr().String())
	return port
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func TestImageBuildsTheHelperImageForTheBackupContainers(t *testing.T) {
	f := newHfDocker()
	m := newHfHelper(f, writeAgentBinary(t, "one"), &hfClock{t: time.Unix(1_700_000_000, 0)})
	ctx := context.Background()
	ref, err := m.Image(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if want := "berthy-helper:" + m.binary.tag(); ref != want || len(f.imported) != 1 {
		t.Fatalf("ref %q (want %q), %d imports", ref, want, len(f.imported))
	}
	if _, ok := f.images[ref]; !ok {
		t.Fatal("the image is not in Docker")
	}
	// The probe helper is not involved: no container, no network, and the same image is reused.
	if f.createCount() != 0 || contains(f.calls, "InspectNetwork") {
		t.Fatalf("calls %v", f.calls)
	}
	if again, err := m.Image(ctx); err != nil || again != ref || len(f.imported) != 1 {
		t.Fatalf("second call: %q %v, %d imports", again, err, len(f.imported))
	}
	// The probe helper then runs from the very same image.
	if err = m.ensure(ctx, "lab-net"); err != nil || len(f.imported) != 1 || f.only(t).imageID != f.images[ref] {
		t.Fatalf("ensure after Image: %v, %d imports", err, len(f.imported))
	}
}

func TestImageReportsWhatBlocksIt(t *testing.T) {
	ctx := context.Background()
	f := newHfDocker()
	m := newHfHelper(f, writeAgentBinary(t, "one", elf.PT_PHDR, elf.PT_INTERP, elf.PT_LOAD), &hfClock{t: time.Unix(1_700_000_000, 0)})
	if _, err := m.Image(ctx); err == nil || err.Error() != "service probes need a statically linked berthy binary" {
		t.Fatalf("error %v", err)
	}
	m = newHfHelper(f, writeAgentBinary(t, "one"), &hfClock{t: time.Unix(1_700_000_000, 0)})
	m.goos = "windows"
	if _, err := m.Image(ctx); err == nil || err.Error() != "service probes need a Linux agent" {
		t.Fatalf("error %v", err)
	}
	if len(f.calls) != 0 {
		t.Fatalf("Docker was asked %v", f.calls)
	}
	m = newHelperManager(scope.ID{Environment: "lab"}, nil, nil)
	m.exe = func() (string, error) { return writeAgentBinary(t, "one"), nil }
	m.goos = "linux"
	if _, err := m.Image(ctx); err == nil || err.Error() != "probe helper unavailable: Docker unavailable" {
		t.Fatalf("error %v", err)
	}
}

// An archive or restore container is created from a helper image and may outlive the agent binary that built it. The
// pruning of old helper images must leave the image it runs from, whether the probe helper is ensured or removed.
func TestPruningKeepsTheImageOfABackupContainer(t *testing.T) {
	f := newHfDocker()
	exe := writeAgentBinary(t, "one")
	clk := &hfClock{t: time.Unix(1_700_000_000, 0)}
	m := newHfHelper(f, exe, clk)
	ctx := context.Background()
	oldRef, err := m.Image(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// A backup container, created but not started yet, then the agent is upgraded.
	f.seed("berthy-archive-lab-1a2b3c4d", map[string]string{"berthy.environment": "lab", "berthy.helper": "archive"}, oldRef)
	f.containers[f.names["berthy-archive-lab-1a2b3c4d"]].running = false
	if err = os.WriteFile(exe, append(hfELF(elf.PT_LOAD), []byte("version-two")...), 0o755); err != nil {
		t.Fatal(err)
	}
	if err = m.ensure(ctx, "lab-net"); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.images[oldRef]; !ok || contains(f.removed, oldRef) {
		t.Fatalf("the image of a backup container was removed: %v", f.removed)
	}
	clk.t = clk.t.Add(time.Minute)
	if err = m.remove(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.images[oldRef]; !ok || contains(f.removed, oldRef) {
		t.Fatalf("remove deleted the image of a backup container: %v", f.removed)
	}
	// Once the backup container is gone, the old image is unused and goes with the next cleanup.
	delete(f.containers, f.names["berthy-archive-lab-1a2b3c4d"])
	delete(f.names, "berthy-archive-lab-1a2b3c4d")
	clk.t = clk.t.Add(time.Minute)
	if err = m.ensure(ctx, "lab-net"); err != nil {
		t.Fatal(err)
	}
	clk.t = clk.t.Add(time.Minute)
	if err = m.remove(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.images[oldRef]; ok {
		t.Fatalf("the unused old image was kept: %v", f.removed)
	}
}
