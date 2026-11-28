package manifest

import (
	"errors"
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

// Alerts configures where alerts go and what they are about. Reminder and Deploys apply to every channel.
type Alerts struct {
	Reminder string `json:"reminder,omitempty" yaml:"reminder,omitempty"`
	// Deploys says which deploys and rollbacks the channels hear about: failed (the default), all, or none.
	Deploys   string     `json:"deploys,omitempty" yaml:"deploys,omitempty"`
	Telegram  *Telegram  `json:"telegram,omitempty" yaml:"telegram,omitempty"`
	Webhook   *Webhook   `json:"webhook,omitempty" yaml:"webhook,omitempty"`
	Heartbeat *Heartbeat `json:"heartbeat,omitempty" yaml:"heartbeat,omitempty"`
}

// Telegram sends alerts to one chat. The bot token is a secret; the chat is named in the manifest.
type Telegram struct {
	BotTokenSecret string `json:"bot_token_secret" yaml:"bot_token_secret"`
	ChatID         string `json:"chat_id" yaml:"chat_id"`
}

// Webhook sends each alert as an HTTP POST of a JSON document. The URL is often the credential, so it is a secret, and so
// are the optional headers.
type Webhook struct {
	URLSecret     string `json:"url_secret" yaml:"url_secret"`
	HeadersSecret string `json:"headers_secret,omitempty" yaml:"headers_secret,omitempty"`
}

// Heartbeat has the agent send one GET to a URL every interval while it runs and Docker answers, so a service that watches for
// the pings can raise the alarm when they stop. The URL is a secret. The heartbeat is not an alert channel: it never alerts.
type Heartbeat struct {
	URLSecret string `json:"url_secret" yaml:"url_secret"`
	Interval  string `json:"interval,omitempty" yaml:"interval,omitempty"`
}

const (
	defaultHeartbeatInterval = time.Minute
	minHeartbeatInterval     = 10 * time.Second
	defaultProbeInterval     = 30 * time.Second
	minProbeInterval         = 5 * time.Second
	defaultProbeTimeout      = 5 * time.Second
	defaultProbeFailures     = 3
	defaultReminder          = 4 * time.Hour
	// The values of alerts.deploys.
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

// probeTimes returns the interval and timeout of a probe, with the defaults applied. When they are invalid it returns an
// error, and the name of the field to blame.
func probeTimes(p Probe) (time.Duration, time.Duration, string, error) {
	interval := defaultProbeInterval
	if p.Interval != "" {
		d, err := time.ParseDuration(p.Interval)
		if err != nil || d < minProbeInterval {
			return 0, 0, "interval", errors.New("interval must be a duration of at least 5s")
		}
		interval = d
	}
	timeout := defaultProbeTimeout
	if p.Timeout != "" {
		d, err := time.ParseDuration(p.Timeout)
		if err != nil || d <= 0 {
			return 0, 0, "timeout", errors.New("timeout must be a positive duration")
		}
		timeout = d
	}
	if timeout >= interval {
		return 0, 0, "timeout", errors.New("timeout must be shorter than interval")
	}
	return interval, timeout, "", nil
}

// Durations returns the probe's interval and timeout, with the defaults applied. The manifest is validated before use,
// so the values are the configured ones.
func (p Probe) Durations() (time.Duration, time.Duration) {
	interval, timeout, _, err := probeTimes(p)
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

// Every returns how often the heartbeat pings, with the default applied.
func (h Heartbeat) Every() time.Duration {
	if h.Interval == "" {
		return defaultHeartbeatInterval
	}
	d, err := time.ParseDuration(h.Interval)
	if err != nil || d < minHeartbeatInterval {
		return defaultHeartbeatInterval
	}
	return d
}

// ReminderInterval returns how often an alert repeats while its check keeps failing. Zero disables reminders.
func (a Alerts) ReminderInterval() time.Duration {
	if a.Reminder == "" {
		return defaultReminder
	}
	d, err := time.ParseDuration(a.Reminder)
	if err != nil || d < 0 {
		return defaultReminder
	}
	return d
}

// checkMonitoring checks probes, peers and alerts. Names must be identifiers, and every value must be usable without
// further lookups.
func (m *Manifest) checkMonitoring(p *Problems) {
	for _, name := range sortedKeys(m.Probes) {
		if !identifier.MatchString(name) {
			p.add(join("probes", name), "invalid probe %q", name)
			continue
		}
		m.checkProbe(p, name, m.Probes[name])
	}
	for _, name := range sortedKeys(m.Peers) {
		if !identifier.MatchString(name) {
			p.add(join("peers", name), "invalid peer %q", name)
			continue
		}
		if !validPeerURL(m.Peers[name]) {
			p.add(join("peers", name), "peer %s: URL must be http or https with a host and a port, and no path, query or user", name)
		}
	}
	if m.Alerts != nil {
		m.Alerts.check(p)
	}
}

func (m *Manifest) checkProbe(p *Problems, name string, pr Probe) {
	path := join("probes", name)
	kinds := 0
	for _, set := range []bool{pr.HTTP != "", pr.TCP != "", pr.Service != nil} {
		if set {
			kinds++
		}
	}
	if kinds != 1 {
		p.add(path, "probe %s: exactly one of http, tcp or service is required", name)
		return
	}
	switch {
	case pr.HTTP != "":
		if !validHTTPURL(pr.HTTP) {
			p.add(path+".http", "probe %s: http must be an http or https URL with a host", name)
		}
	case pr.TCP != "":
		if !validTCPAddress(pr.TCP) {
			p.add(path+".tcp", "probe %s: tcp must be host:port with a port from 1 to 65535", name)
		}
	default:
		m.checkServiceProbe(p, name, *pr.Service)
	}
	for i, code := range pr.ExpectStatus {
		if code < minProbeExpectStatus || code > maxProbeExpectStatus {
			p.add(path+".expect_status."+strconv.Itoa(i), "probe %s: expect_status entries must be from 100 to 599", name)
			break
		}
	}
	_, timeout, field, err := probeTimes(pr)
	if err != nil {
		p.add(path+"."+field, "probe %s: %v", name, err)
	} else if pr.Service != nil && (timeout < minServiceProbeTimeout || timeout > maxServiceProbeTimeout) {
		p.add(path+".timeout", "probe %s: service probe timeout must be from 1s to 25s", name)
	}
	if pr.Failures != nil && *pr.Failures < 1 {
		p.add(path+".failures", "probe %s: failures must be at least 1", name)
	}
}

func (m *Manifest) checkServiceProbe(p *Problems, name string, s ServiceProbe) {
	path := join("probes", name) + ".service"
	a, ok := m.Apps[s.App]
	if !ok {
		p.add(path+".app", "probe %s: service app is not in the manifest", name)
		return
	}
	if a.Kind == "image" {
		_, known := a.Roles[s.Role]
		switch {
		case s.Role == "" && len(a.Roles) != 1:
			p.add(path, "probe %s: service role is required when the app has more than one role", name)
		case s.Role != "" && !known:
			p.add(path+".role", "probe %s: service role is not a role of the app", name)
		}
	} else if s.Role != "" && !identifier.MatchString(s.Role) {
		p.add(path+".role", "probe %s: invalid service role", name)
	}
	if s.Port < 1 || s.Port > 65535 {
		p.add(path+".port", "probe %s: service port must be from 1 to 65535", name)
	}
	if s.Path != "" && !strings.HasPrefix(s.Path, "/") {
		p.add(path+".path", "probe %s: service path must start with /", name)
	}
}

// check validates the alert settings that every channel shares, then each channel that is set.
func (a Alerts) check(p *Problems) {
	a.checkReminder(p)
	switch a.Deploys {
	case "", deploysFailed, deploysAll, deploysNone:
	default:
		p.add("alerts.deploys", "alerts: deploys must be failed, all or none")
	}
	a.checkDestinations(p)
	if a.Heartbeat != nil {
		checkHeartbeat(p, *a.Heartbeat)
	}
}

// CheckAlertChannels validates reminder and destination settings shared with server alerts.
func CheckAlertChannels(a Alerts) error {
	var p Problems
	a.checkReminder(&p)
	a.checkDestinations(&p)
	if len(p) > 0 {
		return p
	}
	return nil
}

func (a Alerts) checkReminder(p *Problems) {
	if !duration(a.Reminder, true) {
		p.add("alerts.reminder", "alerts: reminder must be a duration such as 4h, or 0s to disable")
	}
}

func (a Alerts) checkDestinations(p *Problems) {
	if a.Telegram != nil {
		checkTelegram(p, *a.Telegram)
	}
	if a.Webhook != nil {
		checkWebhook(p, *a.Webhook)
	}
}

func checkTelegram(p *Problems, t Telegram) {
	const path = "alerts.telegram"
	if t.BotTokenSecret == TunnelTokenSecret {
		p.add(path+".bot_token_secret", "alerts.telegram: %s is reserved for managed ingress", TunnelTokenSecret)
	} else if !envName.MatchString(t.BotTokenSecret) {
		p.add(path+".bot_token_secret", "alerts.telegram: bot_token_secret must name a secret")
	}
	if !chatIDPattern.MatchString(t.ChatID) {
		p.add(path+".chat_id", "alerts.telegram: chat_id must be a numeric chat ID or a @channel name")
	}
}

func checkWebhook(p *Problems, w Webhook) {
	const path = "alerts.webhook"
	checkSecretName(p, path, "url_secret", w.URLSecret, false)
	checkSecretName(p, path, "headers_secret", w.HeadersSecret, true)
}

func checkHeartbeat(p *Problems, h Heartbeat) {
	const path = "alerts.heartbeat"
	checkSecretName(p, path, "url_secret", h.URLSecret, false)
	if h.Interval != "" {
		if d, err := time.ParseDuration(h.Interval); err != nil || d < minHeartbeatInterval {
			p.add(path+".interval", "alerts.heartbeat: interval must be a duration of at least 10s")
		}
	}
}

// checkSecretName reports the key of a block that does not name a secret, or names the reserved tunnel token. An empty name
// is accepted when optional is true.
func checkSecretName(p *Problems, block, key, name string, optional bool) {
	switch {
	case name == "" && optional:
	case name == TunnelTokenSecret:
		p.add(block+"."+key, "%s: %s is reserved for managed ingress", block, TunnelTokenSecret)
	case !envName.MatchString(name):
		p.add(block+"."+key, "%s: %s must name a secret", block, key)
	}
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
	if m.Alerts != nil && m.Alerts.Reminder == "" {
		m.Alerts.Reminder = defaultReminder.String()
	}
	if m.Alerts != nil && m.Alerts.Deploys == "" {
		m.Alerts.Deploys = deploysFailed
	}
	if m.Alerts != nil && m.Alerts.Heartbeat != nil && m.Alerts.Heartbeat.Interval == "" {
		m.Alerts.Heartbeat.Interval = defaultHeartbeatInterval.String()
	}
}
