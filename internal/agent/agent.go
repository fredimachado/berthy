package agent

import (
	"berthy/internal/api"
	"berthy/internal/deploy"
	"berthy/internal/engine"
	"berthy/internal/manifest"
	"berthy/internal/metrics"
	"berthy/internal/monitor"
	"berthy/internal/scope"
	"berthy/internal/state"
	"berthy/internal/telemetry"
	"berthy/internal/version"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type Token struct {
	Name   string `json:"name,omitempty"`
	Digest string `json:"digest"`
	Scope  string `json:"scope"`
	// Agent marks the token of an AI agent. On a protected environment the agent does not carry out its changes: each one is
	// held as a pending approval until a person approves it.
	Agent bool `json:"agent,omitempty"`
}
type Config struct {
	Project      string `json:"-"`
	Environment  string `json:"environment"`
	Listen       string `json:"listen"`
	DockerSocket string `json:"docker_socket"`
	StateDir     string `json:"state_dir,omitempty"`
	KeyFile      string `json:"key_file,omitempty"`
	Protected    bool   `json:"protected,omitempty"`
	// Network is the shared overlay network the apps join; berthy setup writes it. When it is empty, the network of the
	// running manifest is used, and with neither, the network check is left out.
	Network string `json:"network,omitempty"`
	// CloudflareAPIURL overrides the Cloudflare API base URL. https is required, except for a loopback host, which tests use.
	CloudflareAPIURL string `json:"cloudflare_api_url,omitempty"`
	// TelegramAPIURL overrides the Telegram Bot API base URL that alerts use. The same rule applies: https, or http on a loopback host.
	TelegramAPIURL string `json:"telegram_api_url,omitempty"`
	// ComposeCommand runs the Compose CLI for Compose apps. It defaults to docker compose.
	ComposeCommand []string `json:"compose_command,omitempty"`
	// ApprovalTTL is how long a held request waits for a person before it expires, as a Go duration of at least a minute. It
	// defaults to 15 minutes. Tests of expiry shorten it.
	ApprovalTTL string  `json:"approval_ttl,omitempty"`
	Tokens      []Token `json:"tokens"`
	// clock is the time source of the approvals. Tests set it; nil means the system clock.
	clock func() time.Time
	// dial opens the agent's connections to other services: Cloudflare, registries, Telegram, webhooks, the heartbeat URL,
	// peers and probes. Nil means a net.Dialer and each client's default. Tests set it to refuse a connection without a
	// closed port.
	dial monitor.DialFunc
	// startupChecks reads the host's startup result. A standalone environment has no host result.
	startupChecks func() []api.Check
}

// outboundHTTP returns the HTTP client for those services: nil, which is each package's default, unless a test set dial.
func (c Config) outboundHTTP() *http.Client {
	if c.dial == nil {
		return nil
	}
	return &http.Client{Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true, DialContext: c.dial}}
}

const defaultApprovalTTL = 15 * time.Minute

// approvalTTL is how long a held request lives. Validate has checked the configured value.
func (c Config) approvalTTL() time.Duration {
	if d, err := time.ParseDuration(c.ApprovalTTL); err == nil && d >= time.Minute {
		return d
	}
	return defaultApprovalTTL
}

// now is the current time for approvals.
func (c Config) now() time.Time {
	if c.clock != nil {
		return c.clock()
	}
	return time.Now()
}

// composeCommand is the Compose CLI command the agent runs, or the default.
func (c Config) composeCommand() []string {
	if len(c.ComposeCommand) == 0 {
		return []string{"docker", "compose"}
	}
	return c.ComposeCommand
}

// ID is the hosted environment this configuration is for.
func (c Config) ID() scope.ID {
	return scope.ID{Project: c.Project, Environment: c.Environment}
}

// dockerHost is the DOCKER_HOST that points a child process at the agent's Docker socket.
func dockerHost(socket string) string {
	if socket == "" {
		socket = "/var/run/docker.sock"
	}
	return "unix://" + socket
}

// tokenPattern matches a bearer token issued for id: bty.<project>.<environment>.<64 hex>.
func tokenPattern(id scope.ID) *regexp.Regexp {
	return regexp.MustCompile(`^` + id.TokenExpression() + `$`)
}

func Digest(s string) string { sum := sha256.Sum256([]byte(s)); return hex.EncodeToString(sum[:]) }

// NewToken makes a bearer token for id, bty.<project>.<environment>.<64 hex>, from 32 random bytes, and returns it with its
// digest. The agent's configuration holds only the digest.
func NewToken(id scope.ID) (token, digest string, err error) {
	b := make([]byte, 32)
	if _, err = rand.Read(b); err != nil {
		return "", "", err
	}
	token = "bty." + id.Project + "." + id.Environment + "." + hex.EncodeToString(b)
	return token, Digest(token), nil
}

func (c Config) Validate() error {
	if c.Project != "" && !scope.ValidProject(c.Project) {
		return fmt.Errorf("invalid project")
	}
	if !scope.ValidEnvironment(c.Environment) {
		return fmt.Errorf("invalid environment")
	}
	if err := (scope.ID{Project: c.Project, Environment: c.Environment}).CheckLength(); err != nil {
		return err
	}
	host, port, err := net.SplitHostPort(c.Listen)
	if err != nil {
		return fmt.Errorf("invalid listen address: %w", err)
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("invalid listen port")
	}
	ip := net.ParseIP(host)
	if ip == nil || ip.IsUnspecified() || ip.IsMulticast() || !privateAddress(ip) {
		return fmt.Errorf("listen address must be a loopback, private, or VPN IP")
	}
	if len(c.Tokens) == 0 {
		return fmt.Errorf("at least one token is required")
	}
	if (c.StateDir == "") != (c.KeyFile == "") {
		return fmt.Errorf("state_dir and key_file must be configured together")
	}
	if c.ApprovalTTL != "" {
		if d, err := time.ParseDuration(c.ApprovalTTL); err != nil || d < time.Minute {
			return fmt.Errorf("approval_ttl must be a duration of at least 1m")
		}
	}
	if c.Network != "" && !manifest.ValidNetwork(c.Network) {
		return fmt.Errorf("invalid network")
	}
	if c.CloudflareAPIURL != "" {
		if err := ValidateAPIURL("cloudflare_api_url", c.CloudflareAPIURL); err != nil {
			return err
		}
	}
	if c.TelegramAPIURL != "" {
		if err := ValidateAPIURL("telegram_api_url", c.TelegramAPIURL); err != nil {
			return err
		}
	}
	for _, arg := range c.ComposeCommand {
		if arg == "" || strings.ContainsRune(arg, 0) {
			return fmt.Errorf("invalid compose_command")
		}
	}
	seen := map[string]bool{}
	names := map[string]bool{}
	for _, t := range c.Tokens {
		if t.Name != "" {
			if !ValidTokenName(t.Name) {
				return fmt.Errorf("invalid token name")
			}
			if names[t.Name] {
				return fmt.Errorf("duplicate token name")
			}
			names[t.Name] = true
		}
		digestKey := strings.ToLower(t.Digest)
		if seen[digestKey] {
			return fmt.Errorf("duplicate token digest")
		}
		seen[digestKey] = true
		b, e := hex.DecodeString(t.Digest)
		if e != nil || len(b) != 32 || (t.Scope != "read" && t.Scope != "deploy" && t.Scope != "admin") {
			return fmt.Errorf("invalid token digest or scope")
		}
	}
	return nil
}

// ValidateAPIURL accepts an https base URL, or http for a loopback host, which the in-memory fakes use. It rejects
// userinfo, queries and fragments, so a token is never sent to an address with credentials in it. name is the config
// key the error names.
func ValidateAPIURL(name, raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || strings.ContainsAny(raw, "?#") {
		return fmt.Errorf("invalid %s", name)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		switch u.Hostname() {
		case "127.0.0.1", "localhost", "::1":
			return nil
		}
	}
	return fmt.Errorf("%s must use https, or http on a loopback host", name)
}

// vpnRange is the range of the VPN that the agent may listen on, and that the host checks do not call public.
var vpnRange = &net.IPNet{IP: net.ParseIP("100.64.0.0"), Mask: net.CIDRMask(10, 32)}

// privateAddress reports whether ip is loopback, private (RFC 1918 or ULA) or on the VPN range. It is the rule for the
// agent's listen address, and the host checks count anything else that listens as public.
func privateAddress(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || vpnRange.Contains(ip)
}

func Handler(c Config, e *engine.Engine) http.Handler {
	return HandlerWithMetrics(c, e, nil)
}
func HandlerWithMetrics(c Config, e *engine.Engine, collector *metrics.Collector) http.Handler {
	return serveAPI(c, handlerWithState(c, e, collector, nil, nil))
}
func handlerWithState(c Config, e *engine.Engine, collector *metrics.Collector, deployment *deploy.Controller, tel *telemetry.Exporter) http.Handler {
	return handlerWithTokens(c, e, collector, deployment, tel, newTokenSource(c, storeOf(deployment)), newLimits(), nil)
}

// snapshotKeep is one claims snapshot for a stats response or stream emission. A nil keep leaves every container.
func snapshotKeep(keep func() func(map[string]string) bool) func(map[string]string) bool {
	if keep == nil {
		return nil
	}
	return keep()
}

// storeOf is the state of a controller, or nil for an agent without state.
func storeOf(d *deploy.Controller) *state.Store {
	if d == nil {
		return nil
	}
	return d.Store
}

// handlerWithTokens is handlerWithState with the tokens the agent authenticates against, which the deployment routes share.
// The handler sees the path after the environment, as mount gives it.
func handlerWithTokens(c Config, e *engine.Engine, collector *metrics.Collector, deployment *deploy.Controller, tel *telemetry.Exporter, tokens *tokenSource, lim *limits, keep func() func(map[string]string) bool) http.Handler {
	rt := &routeRuntime{c: c, e: e, collector: collector, d: deployment, tel: tel, tokens: tokens, lim: lim, keep: keep}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rest := restOf(r.URL.Path)
		rt.serveRead(routeCall{w: w, r: r, rest: rest, row: findEnvRoute(rest, r.Method, false)})
	})
}

func (rt *routeRuntime) serveRead(call routeCall) {
	authorize(rt.tokens, "read", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if call.row == nil || call.row.lane != legacyReadRoute {
			writeError(w, 404, "not_found", "not found")
			return
		}
		if r.Method != call.row.method {
			writeError(w, 405, "method_not_allowed", "method not allowed")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		call.readCtx = ctx
		call.row.handle(rt, call)
	})).ServeHTTP(call.w, call.r)
}

func (rt *routeRuntime) handleAgent(call routeCall) {
	c, deployment := rt.c, rt.d
	host, _ := os.Hostname()
	var effective any
	if c.Protected || (deployment != nil && deployment.Protected) {
		effective = true
	} else if deployment != nil && deployment.Store != nil {
		if value, err := deployment.EffectiveProtected(call.readCtx); err == nil {
			effective = value
		}
	}
	writeReadResult(call.w, map[string]any{"project": c.Project, "environment": c.Environment, "hostname": host,
		"protected": c.Protected, "version": version.String(), "effective_protected": effective}, nil)
}

func (rt *routeRuntime) handleApps(call routeCall) {
	if rt.e == nil {
		writeReadResult(call.w, nil, fmt.Errorf("Docker unavailable"))
		return
	}
	apps, err := appInventory(call.readCtx, rt.e, rt.d)
	// ps shows how old each app's backups are.
	for i := range apps {
		apps[i].Backups = appBackups(call.readCtx, rt.d, apps[i], time.Now())
	}
	writeReadResult(call.w, apps, err)
}

func (rt *routeRuntime) handleApp(call routeCall) {
	w, ctx, name := call.w, call.readCtx, call.rest[1]
	if rt.e == nil {
		writeReadResult(w, nil, fmt.Errorf("Docker unavailable"))
		return
	}
	apps, err := appInventory(ctx, rt.e, rt.d)
	if err != nil {
		writeReadResult(w, nil, err)
		return
	}
	app, code := engine.Select(apps, name)
	if code != 200 {
		message := http.StatusText(code)
		if code == 409 {
			message = "ambiguous app; use image:" + name + ", swarm:" + name + " or compose:" + name
		}
		writeError(w, code, strings.ToLower(strings.ReplaceAll(http.StatusText(code), " ", "_")), message)
		return
	}
	app.Jobs = appJobs(ctx, rt.d, app, time.Now())
	app.Backups = appBackups(ctx, rt.d, app, time.Now())
	app.Drift, app.Scaled = appDrift(ctx, rt.d, app)
	writeReadResult(w, app, nil)
}

func (rt *routeRuntime) handleAppLogs(call routeCall) {
	serveLogs(call.w, call.r, rt.e, call.rest[1], rt.d, rt.lim)
}

func (rt *routeRuntime) handleStats(call routeCall) {
	w, r := call.w, call.r
	var result any
	if r.URL.Query().Get("watch") == "1" && rt.collector != nil {
		streamStats(w, r, rt.collector, rt.lim, rt.keep)
		return
	}
	if rt.collector == nil {
		writeError(w, 503, "sample_not_ready", "sample not ready")
		return
	} else if value := r.URL.Query().Get("since"); value != "" {
		d, parseErr := time.ParseDuration(value)
		if parseErr != nil || d <= 0 || d > time.Hour {
			writeError(w, 400, "bad_request", "invalid since duration")
			return
		}
		samples := rt.collector.Since(d)
		filter := snapshotKeep(rt.keep)
		for i := range samples {
			samples[i] = samples[i].Only(filter)
		}
		result = samples
	} else {
		var ready bool
		var sample metrics.Sample
		sample, ready = rt.collector.Latest()
		if !ready {
			writeError(w, 503, "sample_not_ready", "sample not ready")
			return
		}
		result = sample.Only(snapshotKeep(rt.keep))
	}
	writeReadResult(w, result, nil)
}

func (rt *routeRuntime) handleDF(call routeCall) {
	ctx, e := call.readCtx, rt.e
	disk, diskErr := hostDisk()
	var d dockerDisk
	var dockerErr error
	if e == nil {
		dockerErr = fmt.Errorf("docker unavailable")
	} else {
		dockerErr = e.Get(ctx, "/system/df", &d)
	}
	status := "available"
	if dockerErr != nil {
		status = "unavailable"
	}
	hostStatus := diskStatus(disk, diskErr)
	result := map[string]any{"host": disk, "host_status": hostStatus, "docker": d, "docker_status": status}
	if diskErr != nil {
		result["host_error"] = diskErr.Error()
	}
	writeReadResult(call.w, result, nil)
}

func (rt *routeRuntime) handleDoctor(call routeCall) {
	w, r, ctx := call.w, call.r, call.readCtx
	c, e, deployment, tel := rt.c, rt.e, rt.d, rt.tel
	var err error
	host, _ := os.Hostname()
	checks := []api.Check{{Name: "agent", OK: true, Critical: true, Detail: version.String()}, {Name: "host", OK: host != "", Critical: true, Detail: host}}
	mounts, _ := hostDisk()
	checks = append(checks, rootDiskCheck(mounts))
	var dockerVersion struct {
		APIVersion    string
		MinAPIVersion string
	}
	if e == nil {
		err = fmt.Errorf("Docker unavailable")
	} else {
		err = e.Get(ctx, "/version", &dockerVersion)
	}
	if err != nil {
		checks = append(checks, api.Check{Name: "docker", Critical: true, Detail: "unavailable"})
	} else {
		checks = append(checks, api.Check{Name: "docker", OK: true, Critical: true, Detail: "available"})
		compatible := compareVersion(dockerVersion.MinAPIVersion, "1.44") <= 0 && compareVersion(dockerVersion.APIVersion, "1.44") >= 0
		checks = append(checks, api.Check{Name: "docker_api", OK: compatible, Critical: true, Detail: dockerVersion.MinAPIVersion + " - " + dockerVersion.APIVersion})
		if compatible {
			var info struct {
				Swarm struct {
					ControlAvailable bool
					LocalNodeState   string
				}
			}
			if infoErr := e.Get(ctx, "/info", &info); infoErr != nil {
				checks = append(checks, api.Check{Name: "swarm_manager", Critical: true, Detail: "unavailable"})
			} else {
				checks = append(checks, api.Check{Name: "swarm_manager", OK: info.Swarm.ControlAvailable, Critical: true, Detail: info.Swarm.LocalNodeState})
			}
		}
	}
	if netName, ok := doctorNetwork(ctx, c, deployment); ok {
		checks = append(checks, networkCheck(ctx, e, netName))
	}
	// The credential questions run while the tunnel and Compose checks do, so the doctor takes no longer for them.
	var credentials chan []api.Check
	if deployment != nil && deployment.Store != nil {
		credentials = make(chan []api.Check, 1)
		go func() { credentials <- credentialDoctorChecks(r.Context(), c, deployment.Store, time.Now()) }()
	}
	if deployment != nil {
		ingressCtx, cancelIngress := context.WithTimeout(r.Context(), 10*time.Second)
		configured, status := deployment.TunnelHealth(ingressCtx)
		cancelIngress()
		if configured {
			checks = append(checks, api.Check{Name: "ingress_tunnel", OK: status == "healthy", Detail: status})
		}
		composeCtx, cancelCompose := context.WithTimeout(r.Context(), 5*time.Second)
		composeVersion, composeErr := engine.Compose{Command: c.composeCommand(), Host: dockerHost(c.DockerSocket)}.Version(composeCtx, c.StateDir)
		cancelCompose()
		if composeErr != nil {
			checks = append(checks, api.Check{Name: "compose", Detail: "unavailable"})
		} else {
			checks = append(checks, api.Check{Name: "compose", OK: true, Detail: composeVersion})
		}
	}
	if credentials != nil {
		checks = append(checks, <-credentials...)
	}
	if deployment != nil && deployment.Store != nil {
		checks = append(checks, lockCheck(ctx, deployment.Store, time.Now()))
		if check, ok := stateKeyCheck(deployment.Store); ok {
			checks = append(checks, check)
		}
		checks = append(checks, monitoringChecks(r.Context(), c, deployment.Store, e, c.Environment)...)
		checks = append(checks, driftDoctorChecks(r.Context(), deployment)...)
		if m, mErr := runningManifest(r.Context(), deployment.Store); mErr == nil {
			logCtx, cancelLog := context.WithTimeout(r.Context(), 10*time.Second)
			if check, ok := composeLogCheck(logCtx, deployment, e, m); ok {
				checks = append(checks, check)
			}
			cancelLog()
		}
	}
	if tel != nil {
		if check, ok := telemetryDoctorCheck(tel, time.Now()); ok {
			checks = append(checks, check)
		}
	}
	checks = append(checks, hostDoctorChecks(r.Context(), e, c.Listen)...)
	if c.startupChecks != nil {
		for _, check := range c.startupChecks() {
			if !check.OK {
				checks = append(checks, check)
			}
		}
	}
	writeReadResult(w, map[string]any{"checks": checks}, nil)
}

func writeReadResult(w http.ResponseWriter, result any, err error) {
	if err != nil {
		code := dockerErrorCode(err)
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]string{"code": code, "message": strings.ReplaceAll(code, "_", " ")})
		return
	}
	_ = json.NewEncoder(w).Encode(result)
}

type dockerDisk struct {
	LayersSize int64 `json:"LayersSize"`
	Images     []struct {
		Size       int64 `json:"Size"`
		SharedSize int64 `json:"SharedSize"`
		Containers int64 `json:"Containers"`
	} `json:"Images"`
	Containers []struct {
		SizeRw int64 `json:"SizeRw"`
	} `json:"Containers"`
	Volumes []struct {
		UsageData struct {
			Size     int64 `json:"Size"`
			RefCount int64 `json:"RefCount"`
		} `json:"UsageData"`
	} `json:"Volumes"`
	BuildCache []struct {
		Size  int64 `json:"Size"`
		InUse bool  `json:"InUse"`
	} `json:"BuildCache"`
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"code": code, "message": message})
}
func compareVersion(a, b string) int {
	var am, an, bm, bn int
	fmt.Sscanf(a, "%d.%d", &am, &an)
	fmt.Sscanf(b, "%d.%d", &bm, &bn)
	if am != bm {
		return am - bm
	}
	return an - bn
}

func streamStats(w http.ResponseWriter, r *http.Request, c *metrics.Collector, lim *limits, keep func() func(map[string]string) bool) {
	select {
	case lim.followers <- struct{}{}:
		defer func() { <-lim.followers }()
	default:
		writeError(w, 503, "too_many_followers", "too many followers")
		return
	}
	f, ok := w.(http.Flusher)
	if !ok {
		writeError(w, 500, "stream_unavailable", "stream unavailable")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	ch, stop := c.Subscribe()
	defer stop()
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	var last time.Time
	send := func() bool {
		s, ok := c.Latest()
		if !ok || !shouldEmitSample(s.Time, last) {
			return true
		}
		b, err := json.Marshal(s.Only(snapshotKeep(keep)))
		if err != nil {
			return false
		}
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(5 * time.Second))
		if _, err = fmt.Fprintf(w, "event: stats\ndata: %s\n\n", b); err != nil {
			return false
		}
		f.Flush()
		last = s.Time
		http.NewResponseController(w).SetWriteDeadline(time.Time{})
		return true
	}
	if !send() {
		return
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ch:
			if !send() {
				return
			}
		case <-ticker.C:
			_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(5 * time.Second))
			if _, err := fmt.Fprint(w, ": heartbeat\n\n"); err != nil {
				return
			}
			f.Flush()
			http.NewResponseController(w).SetWriteDeadline(time.Time{})
		}
	}
}

func authorize(tokens *tokenSource, required string, next http.Handler) http.Handler {
	levels := map[string]int{"read": 1, "deploy": 2, "admin": 3}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		token, status := tokens.identify(r)
		if status != http.StatusOK {
			refuseToken(w, status)
			return
		}
		if levels[required] == 0 || levels[token.Scope] < levels[required] {
			writeError(w, 403, "forbidden", "forbidden")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// refuseToken answers a request whose token tokenSource.identify did not accept.
func refuseToken(w http.ResponseWriter, status int) {
	if status == http.StatusServiceUnavailable {
		writeError(w, 503, "state_unavailable", "state unavailable")
		return
	}
	writeError(w, 401, "unauthorized", "unauthorized")
}

func diskStatus(mounts []metrics.Mount, err error) string {
	if len(mounts) == 0 {
		return "unavailable"
	}
	if err != nil {
		return "partial"
	}
	return "available"
}
func rootDiskCheck(mounts []metrics.Mount) api.Check {
	c := api.Check{Name: "disk", Critical: true, Detail: "root mount unavailable"}
	for _, m := range mounts {
		if m.Path == "/" && m.Total > 0 {
			c.OK = float64(m.Available)/float64(m.Total) > 0.1
			c.Detail = fmt.Sprintf("root %.1f%% available", 100*float64(m.Available)/float64(m.Total))
			break
		}
	}
	return c
}

func dockerErrorCode(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "docker_deadline"
	}
	if strings.Contains(err.Error(), "swarm_manager_unavailable") {
		return "swarm_manager_unavailable"
	}
	return "docker_unavailable"
}
func shouldEmitSample(current, last time.Time) bool { return last.IsZero() || current.After(last) }
