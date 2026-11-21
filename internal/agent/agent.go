package agent

import (
	"berthy/internal/api"
	"berthy/internal/deploy"
	"berthy/internal/engine"
	"berthy/internal/manifest"
	"berthy/internal/metrics"
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

// tokenPattern matches a bearer token issued for env: bty.<env>.<64 hex>.
func tokenPattern(env string) *regexp.Regexp {
	return regexp.MustCompile(`^bty\.` + regexp.QuoteMeta(env) + `\.[0-9a-f]{64}$`)
}

func Digest(s string) string { sum := sha256.Sum256([]byte(s)); return hex.EncodeToString(sum[:]) }

// NewToken makes a bearer token for env, bty.<env>.<64 hex>, from 32 random bytes, and returns it with its digest. The agent's
// configuration holds only the digest.
func NewToken(env string) (token, digest string, err error) {
	b := make([]byte, 32)
	if _, err = rand.Read(b); err != nil {
		return "", "", err
	}
	token = "bty." + env + "." + hex.EncodeToString(b)
	return token, Digest(token), nil
}

func (c Config) Validate() error {
	if c.Project != "" && !scope.ValidProject(c.Project) {
		return fmt.Errorf("invalid project")
	}
	if !scope.ValidEnvironment(c.Environment) {
		return fmt.Errorf("invalid environment")
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
	return handlerWithTokens(c, e, collector, deployment, tel, newTokenSource(c, storeOf(deployment)), newLimits())
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
func handlerWithTokens(c Config, e *engine.Engine, collector *metrics.Collector, deployment *deploy.Controller, tel *telemetry.Exporter, tokens *tokenSource, lim *limits) http.Handler {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rest := restOf(r.URL.Path)
		if r.Method != "GET" {
			writeError(w, 405, "method_not_allowed", "method not allowed")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		var result any
		var err error
		switch {
		case len(rest) == 3 && rest[0] == "apps" && rest[2] == "logs":
			serveLogs(w, r, e, rest[1], deployment, lim)
			return
		case len(rest) == 1 && rest[0] == "stats":
			if r.URL.Query().Get("watch") == "1" && collector != nil {
				streamStats(w, r, collector, lim)
				return
			}
			if collector == nil {
				writeError(w, 503, "sample_not_ready", "sample not ready")
				return
			} else if value := r.URL.Query().Get("since"); value != "" {
				d, parseErr := time.ParseDuration(value)
				if parseErr != nil || d <= 0 || d > time.Hour {
					writeError(w, 400, "bad_request", "invalid since duration")
					return
				}
				result = collector.Since(d)
			} else {
				var ready bool
				result, ready = collector.Latest()
				if !ready {
					writeError(w, 503, "sample_not_ready", "sample not ready")
					return
				}
			}
		case len(rest) == 1 && rest[0] == "agent":
			host, _ := os.Hostname()
			result = map[string]any{"project": c.Project, "environment": c.Environment, "hostname": host, "protected": c.Protected, "version": version.String()}
		case len(rest) == 1 && rest[0] == "apps":
			if e == nil {
				err = fmt.Errorf("Docker unavailable")
			} else {
				var apps []api.App
				apps, err = appInventory(ctx, e, deployment)
				// ps shows how old each app's backups are.
				for i := range apps {
					apps[i].Backups = appBackups(ctx, deployment, apps[i], time.Now())
				}
				result = apps
			}
		case len(rest) == 2 && rest[0] == "apps":
			if e == nil {
				err = fmt.Errorf("Docker unavailable")
			} else {
				var apps []api.App
				apps, err = appInventory(ctx, e, deployment)
				if err == nil {
					var code int
					var app api.App
					app, code = engine.Select(apps, rest[1])
					result = app
					if code == 200 {
						app.Jobs = appJobs(ctx, deployment, app, time.Now())
						app.Backups = appBackups(ctx, deployment, app, time.Now())
						app.Drift, app.Scaled = appDrift(ctx, deployment, app)
						result = app
					}
					if code != 200 {
						message := http.StatusText(code)
						if code == 409 {
							message = "ambiguous app; use image:" + rest[1] + ", swarm:" + rest[1] + " or compose:" + rest[1]
						}
						writeError(w, code, strings.ToLower(strings.ReplaceAll(http.StatusText(code), " ", "_")), message)
						return
					}
				}
			}
		case len(rest) == 1 && rest[0] == "df":
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
			result = map[string]any{"host": disk, "host_status": hostStatus, "docker": d, "docker_status": status}
			if diskErr != nil {
				result.(map[string]any)["host_error"] = diskErr.Error()
			}

		case len(rest) == 1 && rest[0] == "doctor":
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
				err = nil
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
			result = map[string]any{"checks": checks}
		default:
			writeError(w, 404, "not_found", "not found")
			return
		}
		if err != nil {
			code := dockerErrorCode(err)
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]string{"code": code, "message": strings.ReplaceAll(code, "_", " ")})
			return
		}
		_ = json.NewEncoder(w).Encode(result)
	})
	// Every route the router accepts requires read scope, so one authorizer is built once.
	readAuthorized := authorize(tokens, "read", handler)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		rest := restOf(r.URL.Path)
		routed := false
		if len(rest) == 1 {
			switch rest[0] {
			case "agent", "apps", "df", "doctor", "stats":
				routed = true
			}
		}
		if len(rest) == 2 && rest[0] == "apps" {
			routed = true
		}
		if len(rest) == 3 && rest[0] == "apps" && rest[2] == "logs" {
			routed = true
		}
		if !routed {
			// Whether a path is a route is for a client with a token to learn: without one the answer is a hosted environment's
			// 401 for every path, as refuseOutside gives for an environment that is not hosted.
			authorize(tokens, "read", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				writeError(w, 404, "not_found", "not found")
			})).ServeHTTP(w, r)
			return
		}
		readAuthorized.ServeHTTP(w, r)
	})
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

func streamStats(w http.ResponseWriter, r *http.Request, c *metrics.Collector, lim *limits) {
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
		b, err := json.Marshal(s)
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
