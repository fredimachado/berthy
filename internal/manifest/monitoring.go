package manifest

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Probe is a check the agent runs on a schedule. Exactly one of HTTP, TCP and Service is set. The agent runs HTTP and TCP
// probes itself; a Service probe runs from the overlay helper, on the shared network.
type Probe struct {
	HTTP         string        `json:"http,omitempty" yaml:"http,omitempty"`
	TCP          string        `json:"tcp,omitempty" yaml:"tcp,omitempty"`
	Service      *ServiceProbe `json:"service,omitempty" yaml:"service,omitempty"`
	ExpectStatus []int         `json:"expect_status,omitempty" yaml:"expect_status,omitempty"`
	Interval     string        `json:"interval,omitempty" yaml:"interval,omitempty"`
	Timeout      string        `json:"timeout,omitempty" yaml:"timeout,omitempty"`
	Failures     *int          `json:"failures,omitempty" yaml:"failures,omitempty"`
}

// ServiceProbe names a role or Compose service of an app in this manifest, and the port and path to probe on it.
type ServiceProbe struct {
	App  string `json:"app" yaml:"app"`
	Role string `json:"role,omitempty" yaml:"role,omitempty"`
	Port int    `json:"port" yaml:"port"`
	Path string `json:"path,omitempty" yaml:"path,omitempty"`
}

// Alerts configures where alerts go. Telegram is the only channel in v1.
type Alerts struct {
	Telegram *Telegram `json:"telegram,omitempty" yaml:"telegram,omitempty"`
}

// Telegram sends alerts to one chat. The bot token is a secret; the chat is named in the manifest.
type Telegram struct {
	BotTokenSecret string `json:"bot_token_secret" yaml:"bot_token_secret"`
	ChatID         string `json:"chat_id" yaml:"chat_id"`
	Reminder       string `json:"reminder,omitempty" yaml:"reminder,omitempty"`
	// Deploys says which deploys and rollbacks the channel hears about: failed (the default), all, or none.
	Deploys string `json:"deploys,omitempty" yaml:"deploys,omitempty"`
}

const (
	defaultProbeInterval = 30 * time.Second
	minProbeInterval     = 5 * time.Second
	defaultProbeTimeout  = 5 * time.Second
	defaultProbeFailures = 3
	defaultReminder      = 4 * time.Hour
	// The values of alerts.telegram.deploys.
	deploysFailed = "failed"
	deploysAll    = "all"
	deploysNone   = "none"
	// minProbeExpectStatus and maxProbeExpectStatus bound the status codes a probe may expect.
	minProbeExpectStatus = 100
	maxProbeExpectStatus = 599
	// minServiceProbeTimeout is the least timeout the overlay helper accepts. maxServiceProbeTimeout keeps a service probe
	// inside the monitor's run budget.
	minServiceProbeTimeout = time.Second
	maxServiceProbeTimeout = 25 * time.Second
)

// chatIDPattern accepts a numeric chat ID or a public channel's @username.
var chatIDPattern = regexp.MustCompile(`^-?[0-9]{1,20}$|^@[A-Za-z0-9_]{5,32}$`)

var digitsPattern = regexp.MustCompile(`^[0-9]+$`)

// probeTimes returns the interval and timeout of a probe, with the defaults applied, and an error when they are invalid.
func probeTimes(p Probe) (time.Duration, time.Duration, error) {
	interval := defaultProbeInterval
	if p.Interval != "" {
		d, err := time.ParseDuration(p.Interval)
		if err != nil || d < minProbeInterval {
			return 0, 0, errors.New("interval must be a duration of at least 5s")
		}
		interval = d
	}
	timeout := defaultProbeTimeout
	if p.Timeout != "" {
		d, err := time.ParseDuration(p.Timeout)
		if err != nil || d <= 0 {
			return 0, 0, errors.New("timeout must be a positive duration")
		}
		timeout = d
	}
	if timeout >= interval {
		return 0, 0, errors.New("timeout must be shorter than interval")
	}
	return interval, timeout, nil
}

// Durations returns the probe's interval and timeout, with the defaults applied. The manifest is validated before use,
// so the values are the configured ones.
func (p Probe) Durations() (time.Duration, time.Duration) {
	interval, timeout, err := probeTimes(p)
	if err != nil {
		return defaultProbeInterval, defaultProbeTimeout
	}
	return interval, timeout
}

// FailureLimit returns how many consecutive failed runs a probe needs before its state changes.
func (p Probe) FailureLimit() int {
	if p.Failures != nil && *p.Failures >= 1 {
		return *p.Failures
	}
	return defaultProbeFailures
}

// ReminderInterval returns how often an alert repeats while its check keeps failing. Zero disables reminders.
func (t Telegram) ReminderInterval() time.Duration {
	if t.Reminder == "" {
		return defaultReminder
	}
	d, err := time.ParseDuration(t.Reminder)
	if err != nil || d < 0 {
		return defaultReminder
	}
	return d
}

// validateMonitoring checks probes, peers and alerts. Names must be identifiers, and every value must be usable without
// further lookups.
func (m *Manifest) validateMonitoring() error {
	for _, name := range sortedKeys(m.Probes) {
		if !identifier.MatchString(name) {
			return fmt.Errorf("invalid probe %q", name)
		}
		if err := m.validateProbe(name, m.Probes[name]); err != nil {
			return err
		}
	}
	for _, name := range sortedKeys(m.Peers) {
		if !identifier.MatchString(name) {
			return fmt.Errorf("invalid peer %q", name)
		}
		if !validPeerURL(m.Peers[name]) {
			return fmt.Errorf("peer %s: URL must be http or https with a host and a port, and no path, query or user", name)
		}
	}
	if m.Alerts != nil && m.Alerts.Telegram != nil {
		return validateTelegram(*m.Alerts.Telegram)
	}
	return nil
}

func (m *Manifest) validateProbe(name string, p Probe) error {
	kinds := 0
	for _, set := range []bool{p.HTTP != "", p.TCP != "", p.Service != nil} {
		if set {
			kinds++
		}
	}
	if kinds != 1 {
		return fmt.Errorf("probe %s: exactly one of http, tcp or service is required", name)
	}
	switch {
	case p.HTTP != "":
		if !validHTTPURL(p.HTTP) {
			return fmt.Errorf("probe %s: http must be an http or https URL with a host", name)
		}
	case p.TCP != "":
		if !validTCPAddress(p.TCP) {
			return fmt.Errorf("probe %s: tcp must be host:port with a port from 1 to 65535", name)
		}
	default:
		if err := m.validateServiceProbe(name, *p.Service); err != nil {
			return err
		}
	}
	for _, code := range p.ExpectStatus {
		if code < minProbeExpectStatus || code > maxProbeExpectStatus {
			return fmt.Errorf("probe %s: expect_status entries must be from 100 to 599", name)
		}
	}
	_, timeout, err := probeTimes(p)
	if err != nil {
		return fmt.Errorf("probe %s: %w", name, err)
	}
	if p.Service != nil && (timeout < minServiceProbeTimeout || timeout > maxServiceProbeTimeout) {
		return fmt.Errorf("probe %s: service probe timeout must be from 1s to 25s", name)
	}
	if p.Failures != nil && *p.Failures < 1 {
		return fmt.Errorf("probe %s: failures must be at least 1", name)
	}
	return nil
}

func (m *Manifest) validateServiceProbe(name string, s ServiceProbe) error {
	a, ok := m.Apps[s.App]
	if !ok {
		return fmt.Errorf("probe %s: service app is not in the manifest", name)
	}
	if a.Kind == "image" {
		_, known := a.Roles[s.Role]
		switch {
		case s.Role == "" && len(a.Roles) != 1:
			return fmt.Errorf("probe %s: service role is required when the app has more than one role", name)
		case s.Role != "" && !known:
			return fmt.Errorf("probe %s: service role is not a role of the app", name)
		}
	} else if s.Role != "" && !identifier.MatchString(s.Role) {
		return fmt.Errorf("probe %s: invalid service role", name)
	}
	if s.Port < 1 || s.Port > 65535 {
		return fmt.Errorf("probe %s: service port must be from 1 to 65535", name)
	}
	if s.Path != "" && !strings.HasPrefix(s.Path, "/") {
		return fmt.Errorf("probe %s: service path must start with /", name)
	}
	return nil
}

func validateTelegram(t Telegram) error {
	if t.BotTokenSecret == TunnelTokenSecret {
		return fmt.Errorf("alerts.telegram: %s is reserved for managed ingress", TunnelTokenSecret)
	}
	if !envName.MatchString(t.BotTokenSecret) {
		return fmt.Errorf("alerts.telegram: bot_token_secret must name a secret")
	}
	if !chatIDPattern.MatchString(t.ChatID) {
		return fmt.Errorf("alerts.telegram: chat_id must be a numeric chat ID or a @channel name")
	}
	if !duration(t.Reminder, true) {
		return fmt.Errorf("alerts.telegram: reminder must be a duration such as 4h, or 0s to disable")
	}
	switch t.Deploys {
	case "", deploysFailed, deploysAll, deploysNone:
	default:
		return fmt.Errorf("alerts.telegram: deploys must be failed, all or none")
	}
	return nil
}

// validHTTPURL accepts an absolute http or https URL with a host and no user information.
func validHTTPURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.Hostname() == "" {
		return false
	}
	return u.Scheme == "http" || u.Scheme == "https"
}

// validTCPAddress accepts host:port with a port from 1 to 65535.
func validTCPAddress(addr string) bool {
	host, port, err := net.SplitHostPort(addr)
	if err != nil || host == "" || !digitsPattern.MatchString(port) {
		return false
	}
	n, err := strconv.Atoi(port)
	return err == nil && n >= 1 && n <= 65535
}

// validPeerURL accepts the base URL of another agent: http or https, a host and a port, and no path other than "/",
// query, fragment or user information.
func validPeerURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return false
	}
	if u.Scheme != "http" && u.Scheme != "https" || u.Path != "" && u.Path != "/" {
		return false
	}
	return validTCPAddress(u.Host)
}

// defaultsMonitoring fills the probe and reminder defaults, so the stored manifest shows the values in effect.
func (m *Manifest) defaultsMonitoring() {
	for name, p := range m.Probes {
		if p.Interval == "" {
			p.Interval = defaultProbeInterval.String()
		}
		if p.Timeout == "" {
			p.Timeout = defaultProbeTimeout.String()
		}
		if p.Failures == nil {
			n := defaultProbeFailures
			p.Failures = &n
		}
		m.Probes[name] = p
	}
	if m.Alerts != nil && m.Alerts.Telegram != nil && m.Alerts.Telegram.Reminder == "" {
		m.Alerts.Telegram.Reminder = defaultReminder.String()
	}
	if m.Alerts != nil && m.Alerts.Telegram != nil && m.Alerts.Telegram.Deploys == "" {
		m.Alerts.Telegram.Deploys = deploysFailed
	}
}
