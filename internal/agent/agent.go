package agent

import (
	"berthy/internal/api"
	"berthy/internal/engine"
	"berthy/internal/metrics"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type Token struct {
	Digest string `json:"digest"`
	Scope  string `json:"scope"`
}
type Config struct {
	Environment  string  `json:"environment"`
	Listen       string  `json:"listen"`
	DockerSocket string  `json:"docker_socket"`
	Tokens       []Token `json:"tokens"`
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
	if err = json.Unmarshal(b, &c); err != nil {
		return c, err
	}
	return c, c.Validate()
}
func (c Config) Validate() error {
	if !regexp.MustCompile(`^[a-z0-9-]+$`).MatchString(c.Environment) {
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
	for _, t := range c.Tokens {
		b, e := hex.DecodeString(t.Digest)
		if e != nil || len(b) != 32 || (t.Scope != "read" && t.Scope != "deploy" && t.Scope != "admin") {
			return fmt.Errorf("invalid token digest or scope")
		}
	}
	return nil
}
func Handler(c Config, e *engine.Engine) http.Handler {
	return HandlerWithMetrics(c, e, nil)
}
func HandlerWithMetrics(c Config, e *engine.Engine, collector *metrics.Collector) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(parts) < 4 || parts[0] != "v1" || parts[1] != "environments" {
			writeError(w, 404, "not_found", "not found")
			return
		}

		raw := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		digest := Digest(raw)
		ok := false
		for _, t := range c.Tokens {
			if subtle.ConstantTimeCompare([]byte(digest), []byte(t.Digest)) == 1 && (t.Scope == "read" || t.Scope == "deploy" || t.Scope == "admin") {
				ok = true
			}
		}
		if !ok || parts[2] != c.Environment || !regexp.MustCompile(`^bty\.`+regexp.QuoteMeta(c.Environment)+`\.[0-9a-f]{64}$`).MatchString(raw) || !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			writeError(w, 401, "unauthorized", "unauthorized")
			return
		}
		if r.Method != "GET" {
			writeError(w, 405, "method_not_allowed", "method not allowed")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		var result any
		var err error
		switch {
		case len(parts) == 4 && parts[3] == "stats":
			if r.URL.Query().Get("watch") == "1" && collector != nil {
				streamStats(w, r, collector)
				return
			}
			if collector == nil {
				result = metrics.Sample{}
			} else if value := r.URL.Query().Get("since"); value != "" {
				d, parseErr := time.ParseDuration(value)
				if parseErr != nil || d <= 0 || d > time.Hour {
					writeError(w, 400, "bad_request", "invalid since duration")
					return
				}
				result = collector.Since(d)
			} else {
				result, _ = collector.Latest()
			}
		case len(parts) == 4 && parts[3] == "agent":
			host, _ := os.Hostname()
			result = map[string]any{"environment": c.Environment, "hostname": host}
		case len(parts) == 4 && parts[3] == "apps":
			if e == nil {
				err = fmt.Errorf("Docker unavailable")
			} else {
				result, err = e.Apps(ctx)
			}
		case len(parts) == 5 && parts[3] == "apps":
			if e == nil {
				err = fmt.Errorf("Docker unavailable")
			} else {
				var apps []api.App
				apps, err = e.Apps(ctx)
				if err == nil {
					var code int
					result, code = engine.Select(apps, parts[4])
					if code != 200 {
						message := http.StatusText(code)
						if code == 409 {
							message = "ambiguous app; use swarm:" + parts[4] + " or compose:" + parts[4]
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
			hostStatus := "available"
			if diskErr != nil {
				hostStatus = "unavailable"
			}
			result = map[string]any{"host": disk, "host_status": hostStatus, "docker": d, "docker_status": status}

		case len(parts) == 4 && parts[3] == "doctor":
			host, _ := os.Hostname()
			checks := []api.Check{{Name: "host", OK: host != "", Critical: true, Detail: host}}
			_, diskErr := hostDisk()
			checks = append(checks, api.Check{Name: "disk", OK: diskErr == nil, Critical: true, Detail: func() string {
				if diskErr != nil {
					return "unavailable"
				}
				return "available"
			}()})
			var info struct {
				Swarm struct {
					ControlAvailable bool
					LocalNodeState   string
				}
			}
			if e == nil {
				err = fmt.Errorf("Docker unavailable")
			} else {
				err = e.Get(ctx, "/info", &info)
			}
			if err != nil {
				checks = append(checks, api.Check{Name: "docker", Critical: true, Detail: "unavailable"})
				err = nil
			} else {
				checks = append(checks, api.Check{Name: "docker", OK: true, Critical: true, Detail: "available"}, api.Check{Name: "swarm_manager", OK: info.Swarm.ControlAvailable, Critical: true, Detail: info.Swarm.LocalNodeState})
				var version struct {
					APIVersion    string
					MinAPIVersion string
				}
				versionErr := e.Get(ctx, "/version", &version)
				compatible := versionErr == nil && compareVersion(version.MinAPIVersion, "1.44") <= 0 && compareVersion(version.APIVersion, "1.44") >= 0
				checks = append(checks, api.Check{Name: "docker_api", OK: compatible, Critical: true, Detail: version.MinAPIVersion + " - " + version.APIVersion})
			}
			result = map[string]any{"checks": checks}
		default:
			writeError(w, 404, "not_found", "not found")
			return
		}
		if err != nil {
			code := "docker_unavailable"
			if errors.Is(err, context.DeadlineExceeded) {
				code = "docker_deadline"
			} else if strings.Contains(err.Error(), "swarm_manager_unavailable") {
				code = "swarm_manager_unavailable"
			}
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]string{"code": code, "message": strings.ReplaceAll(code, "_", " ")})
			return
		}
		_ = json.NewEncoder(w).Encode(result)
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
	send := func() bool {
		s, ok := c.Latest()
		if !ok {
			return true
		}
		b, err := json.Marshal(s)
		if err != nil {
			return false
		}
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(5 * time.Second))
		if _, err = fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
			return false
		}
		f.Flush()
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
		}
	}
}
