package agent

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"debug/elf"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"sync"
	"time"

	"berthy/internal/deploy"
	"berthy/internal/engine"
	"berthy/internal/monitor"
	"berthy/internal/scope"
)

// The overlay helper is one container per environment, built locally from the agent's own static binary. It runs the
// service probes from inside the shared network. The agent reaches it only through a port published on 127.0.0.1, with a
// bearer token that stays in the agent's memory.
const (
	helperRepo      = engine.HelperRepo
	helperUser      = "65532:65532"
	helperMemory    = 64 << 20
	helperTagLength = 12
	// helperEvery is the least time between two runs that change Docker.
	helperEvery = time.Minute
	// helperTimeout bounds one run that changes Docker.
	helperTimeout = 15 * time.Second
	// helperStopSeconds is how long a helper container may take to stop before it is killed. The helper keeps no state.
	helperStopSeconds = 1
	// helperSlack is how much longer than a probe's own timeout the agent waits for the helper to answer.
	helperSlack = 5 * time.Second
	// helperAttempts and helperPoll bound the wait for a new helper to listen: about five seconds in all.
	helperAttempts = 50
	helperPoll     = 100 * time.Millisecond
	// helperLease is how long the helper that a deploy's check started is kept when no service probe needs it. Every check
	// renews it.
	helperLease = 5 * time.Minute
	// reachTimeout is how long a check waits for a TCP connection to a role's port.
	reachTimeout = 3 * time.Second
)

var (
	helperPortKey    = strconv.Itoa(monitor.HelperPort) + "/tcp"
	helperTagPattern = regexp.MustCompile(`^` + helperRepo + `:[0-9a-f]{12}$`)
	// helperChanges are the Dockerfile instructions that give the imported image its entrypoint, user and port.
	helperChanges = []string{
		`ENTRYPOINT ["/berthy","probe-helper"]`,
		"USER " + helperUser,
		"EXPOSE " + helperPortKey,
	}
)

// helperEngine is the part of the Docker API the helper uses. *engine.Engine implements it.
type helperEngine interface {
	InspectNetwork(ctx context.Context, name string) (engine.Network, error)
	InspectImage(ctx context.Context, ref string) (engine.Image, error)
	ImportImage(ctx context.Context, repo, tag string, changes []string, archive []byte) error
	InventoryImages(ctx context.Context) ([]engine.InventoryImage, error)
	RemoveUnusedImage(ctx context.Context, ref string) error
	CreateContainer(ctx context.Context, name string, spec json.RawMessage) (string, error)
	StartContainer(ctx context.Context, id string) error
	InspectContainerDetail(ctx context.Context, id string) (engine.ContainerDetail, error)
	StopContainer(ctx context.Context, id string, seconds int) error
	RemoveContainer(ctx context.Context, id string) error
}

var (
	_ helperEngine        = (*engine.Engine)(nil)
	_ monitor.ProbeHelper = (*helperManager)(nil)
	_ deploy.Reacher      = (*helperManager)(nil)
)

// helperBinary is the agent's executable: the digest that names the helper image, and whether the file is static.
type helperBinary struct {
	path   string
	size   int64
	mtime  time.Time
	digest string
	static bool
}

// tag is the helper image's tag: the first twelve hex digits of the digest.
func (b helperBinary) tag() string { return b.digest[:helperTagLength] }

// helperManager creates, repairs and removes the overlay helper of one environment, and runs the service probes on it.
// ensure and remove change Docker and run at most once a minute. Probe runs from the checks, whose goroutines call it
// concurrently with those runs.
type helperManager struct {
	owner  scope.ID
	engine helperEngine // nil without Docker
	exe    func() (string, error)
	goos   string
	now    func() time.Time
	log    func(string)
	client *http.Client
	// await waits until a new helper answers on port with its token.
	await func(ctx context.Context, port, token string) error

	// runMu serializes the runs that change Docker.
	runMu sync.Mutex

	// mu guards the fields below. Probe reads the running helper's token, port and error under it.
	mu      sync.Mutex
	nextRun time.Time
	err     error // why the helper is not running; nil while it is
	binary  *helperBinary
	token   string // bearer token of the running helper, held in memory only
	id      string // container ID of the helper this process created
	port    string // host port the helper is published on, on 127.0.0.1
	// imagesClean is set once no helper image is left that no container uses.
	imagesClean bool
	// leaseUntil is when the lease that a deploy's check took on the helper ends.
	leaseUntil time.Time
}

// newHelperManager returns the helper of environment env. e is nil without Docker. log receives the message of a run
// that fails, once until the outcome changes; messages never contain secrets.
func newHelperManager(id scope.ID, e *engine.Engine, log func(string)) *helperManager {
	m := &helperManager{
		owner:  id,
		exe:    os.Executable,
		goos:   runtime.GOOS,
		now:    time.Now,
		log:    log,
		client: &http.Client{Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true}},
	}
	m.await = m.awaitHelper
	if e != nil {
		m.engine = e
	}
	return m
}

// ensure creates or repairs the helper on network, which the service probes need. It runs at most once a minute, and a
// call within that minute returns the outcome of the run before. Its error is the detail that the probes report.
func (m *helperManager) ensure(ctx context.Context, network string) error {
	return m.run(ctx, func(ctx context.Context) error { return m.ensureNow(ctx, network) })
}

// remove deletes the helper container and the helper images that no container uses. It runs at most once a minute. It
// changes nothing while a deploy's check holds the lease on the helper.
func (m *helperManager) remove(ctx context.Context) error {
	if m.leased() {
		return nil
	}
	return m.run(ctx, func(ctx context.Context) error {
		// A check may have taken the lease while this run waited for runMu.
		if m.leased() {
			return nil
		}
		return m.removeNow(ctx)
	})
}

// leased reports whether a deploy's check has the helper until a time that has not come yet.
func (m *helperManager) leased() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.now().Before(m.leaseUntil)
}

// Reach opens a TCP connection to host:port from the shared network, on the helper, and implements deploy.Reacher. It starts
// the helper when none runs, now and not at the monitor's next run, and takes a lease on it for helperLease: the monitor
// removes a helper that no service probe needs only after the lease, so the checks of the next roles and the next deploy
// find it. An error that wraps deploy.ErrReachUnavailable means the helper could not run the check; any other error means
// the port did not answer.
func (m *helperManager) Reach(ctx context.Context, network, host string, port int) error {
	if err := m.lease(ctx, network); err != nil {
		return fmt.Errorf("%w: %s", deploy.ErrReachUnavailable, err)
	}
	ok, detail, err := m.Probe(ctx, monitor.HelperProbe{Kind: monitor.KindTCP, Target: host + ":" + strconv.Itoa(port), TimeoutMS: int(reachTimeout / time.Millisecond)})
	switch {
	case err != nil:
		return fmt.Errorf("%w: %s", deploy.ErrReachUnavailable, err)
	case !ok:
		return errors.New(detail)
	}
	return nil
}

// lease makes sure the helper runs on network and holds the lease on it. It does not wait for the monitor's minute between
// two runs, and it leaves that minute alone.
func (m *helperManager) lease(ctx context.Context, network string) error {
	m.runMu.Lock()
	defer m.runMu.Unlock()
	rctx, cancel := context.WithTimeout(ctx, helperTimeout)
	defer cancel()
	if err := m.ensureNow(rctx, network); err != nil {
		return err
	}
	m.mu.Lock()
	m.leaseUntil = m.now().Add(helperLease)
	m.mu.Unlock()
	return nil
}

// run calls do unless the last run was less than helperEvery ago, and records the outcome.
func (m *helperManager) run(ctx context.Context, do func(context.Context) error) error {
	m.runMu.Lock()
	defer m.runMu.Unlock()
	now := m.now()
	m.mu.Lock()
	if now.Before(m.nextRun) {
		err := m.err
		m.mu.Unlock()
		return err
	}
	m.nextRun = now.Add(helperEvery)
	m.mu.Unlock()

	rctx, cancel := context.WithTimeout(ctx, helperTimeout)
	defer cancel()
	err := do(rctx)

	m.mu.Lock()
	prev := m.err
	m.err = err
	m.mu.Unlock()
	if err != nil && (prev == nil || prev.Error() != err.Error()) && m.log != nil {
		m.log(err.Error())
	}
	return err
}

// preflight checks what the helper needs before Docker is changed: Linux, Docker, and a static agent binary. It returns
// the binary.
func (m *helperManager) preflight() (helperBinary, error) {
	if m.goos != "linux" {
		return helperBinary{}, errors.New("service probes need a Linux agent")
	}
	if m.engine == nil {
		return helperBinary{}, helperUnavailable("Docker unavailable")
	}
	bin, err := m.binaryInfo()
	if err != nil {
		return helperBinary{}, err
	}
	if !bin.static {
		return helperBinary{}, errors.New("service probes need a statically linked berthy binary")
	}
	return bin, nil
}

// ensureNow makes the helper container match the agent's binary and the network. A helper that matches and that this
// process created, with its token in memory, is kept. Any other helper is replaced, with a new token.
func (m *helperManager) ensureNow(ctx context.Context, network string) error {
	bin, err := m.preflight()
	if err != nil {
		return err
	}
	netID, err := m.networkID(ctx, network)
	if err != nil {
		return err
	}
	img, err := m.image(ctx, bin)
	if err != nil {
		return err
	}
	current, found, err := m.lookup(ctx)
	if err != nil {
		return err
	}
	if found {
		if reason := m.mismatch(current, img.ID, network, netID); reason == "" && m.holds(current.ID) {
			port, _ := publishedPort(current)
			m.mu.Lock()
			m.port = port
			m.mu.Unlock()
			return nil
		}
		if err = m.removeContainer(ctx, current.ID); err != nil {
			return err
		}
	}
	return m.create(ctx, bin, network, netID, img.ID)
}

// removeNow deletes the helper container, if there is one, and then the older helper images that no container uses.
func (m *helperManager) removeNow(ctx context.Context) error {
	if m.engine == nil {
		return nil
	}
	current, found, err := m.lookup(ctx)
	if err != nil {
		return err
	}
	if found {
		if err = m.removeContainer(ctx, current.ID); err != nil {
			return err
		}
	} else {
		m.mu.Lock()
		clean := m.imagesClean
		m.mu.Unlock()
		if clean {
			return nil
		}
	}
	// The image of the agent's own binary stays: every hosted environment shares it, and another one's backup may have
	// resolved it and not yet created its container, which is all that would protect it. When the binary cannot be read the
	// current tag is unknown, so no tag can be called stale: nothing is pruned, and the next run tries again.
	bin, err := m.binaryInfo()
	if err != nil {
		return nil
	}
	m.pruneImages(ctx, bin.tag())
	m.mu.Lock()
	m.imagesClean = true
	m.mu.Unlock()
	return nil
}

// networkID returns the ID of the network the helper attaches to. Only an attachable network can take a standalone
// container.
func (m *helperManager) networkID(ctx context.Context, name string) (string, error) {
	n, err := m.engine.InspectNetwork(ctx, name)
	if err != nil {
		return "", helperUnavailable("network unavailable")
	}
	if !n.Attachable {
		return "", helperUnavailable("network is not attachable")
	}
	return n.ID, nil
}

// Image makes sure the helper image of the agent's binary exists, importing it when Docker does not have it, and returns
// its reference. The backup containers run from it: it holds the agent's binary alone, and each of them overrides its
// entrypoint and user. It creates no container. Pruning of old helper images skips the image of any container.
func (m *helperManager) Image(ctx context.Context) (string, error) {
	bin, err := m.preflight()
	if err != nil {
		return "", err
	}
	if _, err = m.image(ctx, bin); err != nil {
		return "", err
	}
	return helperRepo + ":" + bin.tag(), nil
}

var imageMu sync.Mutex

// image returns the helper image for bin, importing it from the agent's binary when Docker does not have it.
func (m *helperManager) image(ctx context.Context, bin helperBinary) (engine.Image, error) {
	imageMu.Lock()
	defer imageMu.Unlock()
	ref := helperRepo + ":" + bin.tag()
	img, err := m.engine.InspectImage(ctx, ref)
	if err == nil {
		return img, nil
	}
	if !notFound(err) {
		return engine.Image{}, helperUnavailable("image unavailable")
	}
	data, err := readBinary(bin)
	if err != nil {
		return engine.Image{}, err
	}
	archive, err := helperArchive(data)
	if err != nil {
		return engine.Image{}, helperUnavailable("image archive failed")
	}
	if err = m.engine.ImportImage(ctx, helperRepo, bin.tag(), helperChanges, archive); err != nil {
		return engine.Image{}, helperUnavailable("image import failed")
	}
	img, err = m.engine.InspectImage(ctx, ref)
	if err != nil {
		return engine.Image{}, helperUnavailable("image unavailable")
	}
	return img, nil
}

// lookup returns the helper container of this environment, and false when there is none. A container that holds the
// helper's name but is not this environment's helper is an error, and is never touched.
func (m *helperManager) lookup(ctx context.Context) (engine.ContainerDetail, bool, error) {
	c, err := m.engine.InspectContainerDetail(ctx, m.name())
	if notFound(err) {
		return engine.ContainerDetail{}, false, nil
	}
	if err != nil {
		return engine.ContainerDetail{}, false, helperUnavailable("container inspection failed")
	}
	if c.Labels["berthy.helper"] != "probe" || !m.owner.Owns(c.Labels) {
		return engine.ContainerDetail{}, false, helperUnavailable("name is used by another container")
	}
	return c, true, nil
}

// create starts a helper container from the image, with a new token, and keeps its token and port. A container that
// does not come up as specified is removed.
func (m *helperManager) create(ctx context.Context, bin helperBinary, network, networkID, imageID string) error {
	token, err := newHelperToken()
	if err != nil {
		return helperUnavailable("token unavailable")
	}
	spec, err := helperSpec(m.owner, network, helperRepo+":"+bin.tag(), token)
	if err != nil {
		return helperUnavailable("container spec unavailable")
	}
	id, err := m.engine.CreateContainer(ctx, m.name(), spec)
	if err != nil || id == "" {
		return helperUnavailable("container create failed")
	}
	if err = m.engine.StartContainer(ctx, id); err != nil {
		m.discard(ctx, id)
		return helperUnavailable("container start failed")
	}
	c, err := m.engine.InspectContainerDetail(ctx, id)
	if err != nil {
		m.discard(ctx, id)
		return helperUnavailable("container inspection failed")
	}
	if reason := m.mismatch(c, imageID, network, networkID); reason != "" {
		m.discard(ctx, id)
		return helperUnavailable(reason)
	}
	port, _ := publishedPort(c)
	if err = m.await(ctx, port, token); err != nil {
		m.discard(ctx, id)
		return err
	}
	m.mu.Lock()
	m.token, m.id, m.port = token, id, port
	m.imagesClean = false
	m.mu.Unlock()
	m.pruneImages(ctx, bin.tag())
	return nil
}

// awaitHelper waits for a new helper to answer on port with its token, so the first probe does not race its start. An
// empty probe is refused with 400, which shows that the helper listens and knows the token.
func (m *helperManager) awaitHelper(ctx context.Context, port, token string) error {
	for attempt := 0; attempt < helperAttempts; attempt++ {
		resp, err := m.post(ctx, port, token, []byte("{}"))
		if err == nil {
			resp.Body.Close()
			switch resp.StatusCode {
			case http.StatusBadRequest:
				return nil
			case http.StatusUnauthorized:
				return helperUnavailable("token rejected")
			}
		}
		select {
		case <-ctx.Done():
			return helperUnavailable("not answering")
		case <-time.After(helperPoll):
		}
	}
	return helperUnavailable("not answering")
}

// post sends body to the helper's probe endpoint on port with the token. The caller closes the response body.
func (m *helperManager) post(ctx context.Context, port, token string, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://127.0.0.1:"+port+"/probe", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	return m.client.Do(req)
}

// discard removes a container that did not come up as specified. A failure here is not reported: the next run removes
// the container again.
func (m *helperManager) discard(ctx context.Context, id string) {
	_ = m.removeContainer(ctx, id)
}

// removeContainer stops and removes a helper container. A container that is already stopped or gone is removed all the
// same.
func (m *helperManager) removeContainer(ctx context.Context, id string) error {
	if err := m.engine.StopContainer(ctx, id, helperStopSeconds); err != nil && !notFound(err) && !statusIs(err, http.StatusNotModified) {
		return helperUnavailable("container stop failed")
	}
	if err := m.engine.RemoveContainer(ctx, id); err != nil && !notFound(err) {
		return helperUnavailable("container removal failed")
	}
	m.mu.Lock()
	if m.id == id {
		m.token, m.id, m.port = "", "", ""
	}
	m.mu.Unlock()
	return nil
}

// pruneImages removes the helper images other than keep that no container uses. Docker refuses the ones in use, and a
// refusal is not an error: the next run tries again.
func (m *helperManager) pruneImages(ctx context.Context, keep string) {
	if keep == "" {
		return // the current tag is unknown, so no tag is stale
	}
	images, err := m.engine.InventoryImages(ctx)
	if err != nil {
		return
	}
	for _, img := range images {
		for _, tag := range img.RepoTags {
			if helperTagPattern.MatchString(tag) && tag != helperRepo+":"+keep {
				_ = m.engine.RemoveUnusedImage(ctx, tag)
			}
		}
	}
}

// mismatch names the first way a helper container differs from the one ensure creates, or returns "" when it matches.
func (m *helperManager) mismatch(c engine.ContainerDetail, imageID, network, networkID string) string {
	_, published := publishedPort(c)
	switch {
	case !c.Running:
		return "container not running"
	case c.ImageID != imageID:
		return "image differs"
	case len(c.Networks) != 1 || c.Networks[network] != networkID:
		return "network differs"
	case c.User != helperUser || !c.ReadonlyRootfs || !slices.Contains(c.CapDrop, "ALL") || !hasOption(c.SecurityOpt, "no-new-privileges") ||
		c.Memory != helperMemory || c.RestartPolicy != "unless-stopped":
		return "settings differ"
	case !requestedOnLoopback(c.PortBindings) || !published:
		return "port not published on 127.0.0.1"
	}
	return ""
}

// holds reports whether this process created container id and still holds its token.
func (m *helperManager) holds(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.id == id && m.token != ""
}

// name is the helper container's name.
func (m *helperManager) name() string { return "berthy-helper-" + m.owner.Slug() }

// Probe runs one probe on the running helper. The helper is marked lost when it does not answer or refuses the token,
// and the next run recreates it.
func (m *helperManager) Probe(ctx context.Context, req monitor.HelperProbe) (bool, string, error) {
	m.mu.Lock()
	token, port, err := m.token, m.port, m.err
	m.mu.Unlock()
	if token == "" || port == "" {
		if err == nil {
			err = helperUnavailable("not running")
		}
		return false, "", err
	}
	body, err := json.Marshal(req)
	if err != nil {
		return false, "", helperUnavailable("probe invalid")
	}
	pctx, cancel := context.WithTimeout(ctx, time.Duration(req.TimeoutMS)*time.Millisecond+helperSlack)
	defer cancel()
	resp, err := m.post(pctx, port, token, body)
	if err != nil {
		return false, "", m.lost(ctx, "not answering")
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized:
		return false, "", m.lost(ctx, "token rejected")
	default:
		return false, "", helperUnavailable("probe refused")
	}
	var out struct {
		OK     bool   `json:"ok"`
		Detail string `json:"detail"`
	}
	if err = json.NewDecoder(io.LimitReader(resp.Body, 4<<10)).Decode(&out); err != nil {
		return false, "", helperUnavailable("answer invalid")
	}
	return out.OK, out.Detail, nil
}

// lost marks the running helper unusable, so the next run recreates it, and returns why. A probe that ended because its
// own context did was not the helper's fault, and changes nothing.
func (m *helperManager) lost(ctx context.Context, reason string) error {
	err := helperUnavailable(reason)
	if ctx.Err() == nil {
		m.mu.Lock()
		m.token, m.port, m.err = "", "", err
		m.mu.Unlock()
	}
	return err
}

// status reports whether the helper runs as ensure would leave it, and changes nothing. It is the doctor's view. The
// detail is the reason when it does not run.
func (m *helperManager) status(ctx context.Context, network string) (bool, string) {
	bin, err := m.preflight()
	if err != nil {
		return false, err.Error()
	}
	netID, err := m.networkID(ctx, network)
	if err != nil {
		return false, err.Error()
	}
	img, err := m.engine.InspectImage(ctx, helperRepo+":"+bin.tag())
	switch {
	case notFound(err):
		return false, helperUnavailable("image not built yet").Error()
	case err != nil:
		return false, helperUnavailable("image unavailable").Error()
	}
	current, found, err := m.lookup(ctx)
	if err != nil {
		return false, err.Error()
	}
	if !found {
		return false, helperUnavailable("not created yet").Error()
	}
	if reason := m.mismatch(current, img.ID, network, netID); reason != "" {
		return false, helperUnavailable(reason).Error()
	}
	return true, "running"
}

// binaryInfo describes the agent's executable. The digest and the static check are read again only when the file's size
// or modification time changes.
func (m *helperManager) binaryInfo() (helperBinary, error) {
	path, err := m.exe()
	if err != nil {
		return helperBinary{}, helperUnavailable("agent binary unavailable")
	}
	st, err := os.Stat(path)
	if err != nil || st.IsDir() {
		return helperBinary{}, helperUnavailable("agent binary unavailable")
	}
	m.mu.Lock()
	memo := m.binary
	m.mu.Unlock()
	if memo != nil && memo.path == path && memo.size == st.Size() && memo.mtime.Equal(st.ModTime()) {
		return *memo, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return helperBinary{}, helperUnavailable("agent binary unavailable")
	}
	defer f.Close()
	h := sha256.New()
	if _, err = io.Copy(h, f); err != nil {
		return helperBinary{}, helperUnavailable("agent binary unavailable")
	}
	bin := helperBinary{path: path, size: st.Size(), mtime: st.ModTime(), digest: hex.EncodeToString(h.Sum(nil)), static: staticELF(f)}
	m.mu.Lock()
	m.binary = &bin
	m.mu.Unlock()
	return bin, nil
}

// readBinary reads the agent's binary, and fails when it is no longer the file bin describes.
func readBinary(bin helperBinary) ([]byte, error) {
	data, err := os.ReadFile(bin.path)
	if err != nil {
		return nil, helperUnavailable("agent binary unavailable")
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != bin.digest {
		return nil, helperUnavailable("agent binary changed")
	}
	return data, nil
}

// staticELF reports whether r is an ELF file with loadable segments and no program interpreter. Such a file runs without
// a dynamic loader, so it can be the only file in an image. Any other file is not static.
func staticELF(r io.ReaderAt) bool {
	f, err := elf.NewFile(r)
	if err != nil {
		return false
	}
	loadable := false
	for _, p := range f.Progs {
		switch p.Type {
		case elf.PT_INTERP:
			return false
		case elf.PT_LOAD:
			loadable = true
		}
	}
	return loadable
}

// helperArchive is the tar archive the helper image is imported from: the binary alone, at /berthy.
func helperArchive(binary []byte) ([]byte, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Name: "berthy", Mode: 0o755, Size: int64(len(binary)), Typeflag: tar.TypeReg}); err != nil {
		return nil, err
	}
	if _, err := tw.Write(binary); err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// helperSpec is the helper container. It is attached to the shared network, with its port published on 127.0.0.1 only,
// a read-only root, no capabilities, no way to gain privileges, a memory limit, and a restart policy.
func helperSpec(id scope.ID, network, image, token string) (json.RawMessage, error) {
	labels := id.Labels()
	labels["berthy.helper"] = "probe"
	return json.Marshal(map[string]any{
		"Image":        image,
		"User":         helperUser,
		"Env":          []string{monitor.HelperTokenEnv + "=" + token},
		"Labels":       labels,
		"ExposedPorts": map[string]any{helperPortKey: map[string]any{}},
		"HostConfig": map[string]any{
			"NetworkMode":    network,
			"ReadonlyRootfs": true,
			"CapDrop":        []string{"ALL"},
			"SecurityOpt":    []string{"no-new-privileges"},
			"Memory":         helperMemory,
			"RestartPolicy":  map[string]any{"Name": "unless-stopped"},
			"PortBindings":   map[string]any{helperPortKey: []any{map[string]string{"HostIp": "127.0.0.1", "HostPort": ""}}},
		},
		"NetworkingConfig": map[string]any{"EndpointsConfig": map[string]any{network: map[string]any{}}},
	})
}

// publishedPort returns the host port the helper is published on. The helper's port must be the only publication, and it
// must be on 127.0.0.1.
func publishedPort(c engine.ContainerDetail) (string, bool) {
	if len(c.Ports) != 1 {
		return "", false
	}
	bindings := c.Ports[helperPortKey]
	if len(bindings) != 1 || bindings[0].HostIP != "127.0.0.1" {
		return "", false
	}
	n, err := strconv.Atoi(bindings[0].HostPort)
	if err != nil || n < 1 || n > 65535 {
		return "", false
	}
	return bindings[0].HostPort, true
}

// requestedOnLoopback reports whether the container asked for exactly one publication, of the helper's port on 127.0.0.1.
func requestedOnLoopback(requested map[string][]engine.PortBinding) bool {
	return len(requested) == 1 && len(requested[helperPortKey]) == 1 && requested[helperPortKey][0].HostIP == "127.0.0.1"
}

// hasOption reports whether a security option is set, in any of the forms Docker reports it.
func hasOption(options []string, name string) bool {
	return slices.ContainsFunc(options, func(o string) bool {
		return o == name || o == name+":true" || o == name+"=true"
	})
}

// newHelperToken returns a new bearer token: 32 random bytes in hex.
func newHelperToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// helperUnavailable is the error for a helper that cannot run probes. Its text is the detail that the probes report, so
// it names the reason and nothing else.
func helperUnavailable(reason string) error {
	return errors.New("probe helper unavailable: " + reason)
}

// statusIs reports whether err is a Docker refusal with the given status.
func statusIs(err error, code int) bool {
	var se *engine.StatusError
	return errors.As(err, &se) && se.Code == code
}

func notFound(err error) bool { return statusIs(err, http.StatusNotFound) }
