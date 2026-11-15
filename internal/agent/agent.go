package agent

import (
	"berthy/internal/api"
	"berthy/internal/deploy"
	"berthy/internal/engine"
	"berthy/internal/metrics"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
}
type Config struct {
	Environment  string `json:"environment"`
	Listen       string `json:"listen"`
	DockerSocket string `json:"docker_socket"`
	StateDir     string `json:"state_dir,omitempty"`
	KeyFile      string `json:"key_file,omitempty"`
	Protected    bool   `json:"protected,omitempty"`
	// CloudflareAPIURL overrides the Cloudflare API base URL. https is required, except for a loopback host, which tests use.
	CloudflareAPIURL string `json:"cloudflare_api_url,omitempty"`
	// ComposeCommand runs the Compose CLI for Compose apps. It defaults to docker compose.
	ComposeCommand []string `json:"compose_command,omitempty"`
	Tokens         []Token  `json:"tokens"`
}

// composeCommand is the Compose CLI command the agent runs, or the default.
func (c Config) composeCommand() []string {
	if len(c.ComposeCommand) == 0 {
		return []string{"docker", "compose"}
	}
	return c.ComposeCommand
}

// dockerHost is the DOCKER_HOST that points a child process at the agent's Docker socket.
func dockerHost(socket string) string {
	if socket == "" {
		socket = "/var/run/docker.sock"
	}
	return "unix://" + socket
}

var envPattern = regexp.MustCompile(`^[a-z0-9-]+$`)

// tokenPattern matches a bearer token issued for env: bty.<env>.<64 hex>.
func tokenPattern(env string) *regexp.Regexp {
	return regexp.MustCompile(`^bty\.` + regexp.QuoteMeta(env) + `\.[0-9a-f]{64}$`)
}

func Digest(s string) string { sum := sha256.Sum256([]byte(s)); return hex.EncodeToString(sum[:]) }
func Load(path string) (Config, error) {
	var c Config
	st, err := os.Stat(path)
	if err != nil {
		return c, err
	}
	if st.Mode().Perm()&0077 != 0 {
		return c, fmt.Errorf("agent config must not be accessible by group or others")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err = dec.Decode(&c); err != nil {
		return c, err
	}
	if err = dec.Decode(&struct{}{}); err != io.EOF {
		return c, fmt.Errorf("invalid trailing JSON")
	}
	return c, c.Validate()
}
func (c Config) Validate() error {
	if !envPattern.MatchString(c.Environment) {
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
	if ip == nil || ip.IsUnspecified() || ip.IsMulticast() || !(ip.IsLoopback() || ip.IsPrivate() || (&net.IPNet{IP: net.ParseIP("100.64.0.0"), Mask: net.CIDRMask(10, 32)}).Contains(ip)) {
		return fmt.Errorf("listen address must be a loopback, private, or VPN IP")
	}
	if len(c.Tokens) == 0 {
		return fmt.Errorf("at least one token is required")
	}
	if (c.StateDir == "") != (c.KeyFile == "") {
		return fmt.Errorf("state_dir and key_file must be configured together")
	}
	if c.CloudflareAPIURL != "" {
		if err := validateCloudflareURL(c.CloudflareAPIURL); err != nil {
			return err
		}
	}
	for _, arg := range c.ComposeCommand {
		if arg == "" || strings.ContainsRune(arg, 0) {
			return fmt.Errorf("invalid compose_command")
		}
	}
	seen := map[string]bool{}
	for _, t := range c.Tokens {
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

// validateCloudflareURL accepts an https base URL, or http for a loopback host, which the in-memory Cloudflare fake uses.
// It rejects userinfo, queries and fragments, so the API token is never sent to an address with credentials in it.
func validateCloudflareURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || strings.ContainsAny(raw, "?#") {
		return fmt.Errorf("invalid cloudflare_api_url")
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
	return fmt.Errorf("cloudflare_api_url must use https, or http on a loopback host")
}

func Handler(c Config, e *engine.Engine) http.Handler {
	return HandlerWithMetrics(c, e, nil)
}
func HandlerWithMetrics(c Config, e *engine.Engine, collector *metrics.Collector) http.Handler {
	return handlerWithState(c, e, collector, nil)
}
func handlerWithState(c Config, e *engine.Engine, collector *metrics.Collector, deployment *deploy.Controller) http.Handler {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if r.Method != "GET" {
			writeError(w, 405, "method_not_allowed", "method not allowed")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		var result any
		var err error
		switch {
		case len(parts) == 6 && parts[3] == "apps" && parts[5] == "logs":
			serveLogs(w, r, e, parts[4], deployment)
			return
		case len(parts) == 4 && parts[3] == "stats":
			if r.URL.Query().Get("watch") == "1" && collector != nil {
				streamStats(w, r, collector)
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
		case len(parts) == 4 && parts[3] == "agent":
			host, _ := os.Hostname()
			result = map[string]any{"environment": c.Environment, "hostname": host}
		case len(parts) == 4 && parts[3] == "apps":
			if e == nil {
				err = fmt.Errorf("Docker unavailable")
			} else {
				result, err = appInventory(ctx, e, deployment)
			}
		case len(parts) == 5 && parts[3] == "apps":
			if e == nil {
				err = fmt.Errorf("Docker unavailable")
			} else {
				var apps []api.App
				apps, err = appInventory(ctx, e, deployment)
				if err == nil {
					var code int
					result, code = engine.Select(apps, parts[4])
					if code != 200 {
						message := http.StatusText(code)
						if code == 409 {
							message = "ambiguous app; use image:" + parts[4] + ", swarm:" + parts[4] + " or compose:" + parts[4]
						}
						writeError(w, code, strings.ToLower(strings.ReplaceAll(http.StatusText(code), " ", "_")), message)
						return
					}
				}
			}
		case len(parts) == 4 && parts[3] == "df":
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

		case len(parts) == 4 && parts[3] == "doctor":
			host, _ := os.Hostname()
			checks := []api.Check{{Name: "host", OK: host != "", Critical: true, Detail: host}}
			mounts, _ := hostDisk()
			checks = append(checks, rootDiskCheck(mounts))
			var version struct {
				APIVersion    string
				MinAPIVersion string
			}
			if e == nil {
				err = fmt.Errorf("Docker unavailable")
			} else {
				err = e.Get(ctx, "/version", &version)
			}
			if err != nil {
				checks = append(checks, api.Check{Name: "docker", Critical: true, Detail: "unavailable"})
				err = nil
			} else {
				checks = append(checks, api.Check{Name: "docker", OK: true, Critical: true, Detail: "available"})
				compatible := compareVersion(version.MinAPIVersion, "1.44") <= 0 && compareVersion(version.APIVersion, "1.44") >= 0
				checks = append(checks, api.Check{Name: "docker_api", OK: compatible, Critical: true, Detail: version.MinAPIVersion + " - " + version.APIVersion})
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
			if deployment != nil {
				ingressCtx, cancelIngress := context.WithTimeout(r.Context(), 10*time.Second)
				configured, status := deployment.TunnelHealth(ingressCtx)
				cancelIngress()
				if configured {
					checks = append(checks, api.Check{Name: "ingress_tunnel", OK: status == "healthy", Detail: status})
				}
				composeCtx, cancelCompose := context.WithTimeout(r.Context(), 5*time.Second)
				version, composeErr := engine.Compose{Command: c.composeCommand(), Host: dockerHost(c.DockerSocket)}.Version(composeCtx, c.StateDir)
				cancelCompose()
				if composeErr != nil {
					checks = append(checks, api.Check{Name: "compose", Detail: "unavailable"})
				} else {
					checks = append(checks, api.Check{Name: "compose", OK: true, Detail: version})
				}
			}
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
	readAuthorized := authorize(c, tokenPattern(c.Environment), "read", handler)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(parts) < 4 || parts[0] != "v1" || parts[1] != "environments" {
			writeError(w, 404, "not_found", "not found")
			return
		}
		routed := false
		if len(parts) == 4 {
			switch parts[3] {
			case "agent", "apps", "df", "doctor", "stats":
				routed = true
			}
		}
		if len(parts) == 5 && parts[3] == "apps" {
			routed = true
		}
		if len(parts) == 6 && parts[3] == "apps" && parts[5] == "logs" {
			routed = true
		}
		if !routed {
			writeError(w, 404, "not_found", "not found")
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

func streamStats(w http.ResponseWriter, r *http.Request, c *metrics.Collector) {
	select {
	case followSlots <- struct{}{}:
		defer func() { <-followSlots }()
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

func authorize(c Config, pattern *regexp.Regexp, required string, next http.Handler) http.Handler {
	levels := map[string]int{"read": 1, "deploy": 2, "admin": 3}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		raw := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		sum := sha256.Sum256([]byte(raw))
		scope := 0
		for _, t := range c.Tokens {
			digest, err := hex.DecodeString(t.Digest)
			if err == nil && subtle.ConstantTimeCompare(sum[:], digest) == 1 {
				if levels[t.Scope] > scope {
					scope = levels[t.Scope]
				}
			}
		}
		if len(parts) < 3 || parts[2] != c.Environment || !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") || !pattern.MatchString(raw) || scope == 0 {
			writeError(w, 401, "unauthorized", "unauthorized")
			return
		}
		if levels[required] == 0 || scope < levels[required] {
			writeError(w, 403, "forbidden", "forbidden")
			return
		}
		next.ServeHTTP(w, r)
	})
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
