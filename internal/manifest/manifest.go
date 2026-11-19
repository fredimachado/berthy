package manifest

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"berthy/internal/version"

	"github.com/distribution/reference"
	"go.yaml.in/yaml/v3"
)

const maxInput = 1 << 20

// TunnelTokenSecret is the reserved secret name for the tunnel token Berthy fetches for the connector.
const TunnelTokenSecret = "TUNNEL_TOKEN"

// ConnectorAppName is the app Berthy runs for the managed ingress connector.
const ConnectorAppName = "ingress"

// berthyEnvPrefix starts the environment names Berthy sets in every image app's containers. An app may not declare them.
const berthyEnvPrefix = "BERTHY_"

// ConnectorMetricsPort is the port the connector serves metrics and its readiness check on, inside its container.
const ConnectorMetricsPort = 20241

// Manifest is the complete desired configuration sent to an agent.
type Manifest struct {
	Project      string                 `json:"project" yaml:"project"`
	Network      string                 `json:"network" yaml:"network"`
	Protected    bool                   `json:"protected" yaml:"protected"`
	Registries   map[string]Registry    `json:"registries,omitempty" yaml:"registries,omitempty"`
	Apps         map[string]App         `json:"apps" yaml:"apps"`
	Jobs         map[string]Job         `json:"jobs,omitempty" yaml:"jobs,omitempty"`
	Backups      map[string]Backup      `json:"backups,omitempty" yaml:"backups,omitempty"`
	Destinations map[string]Destination `json:"destinations,omitempty" yaml:"destinations,omitempty"`
	StateBackup  *StateBackup           `json:"state_backup,omitempty" yaml:"state_backup,omitempty"`
	Telemetry    *Telemetry             `json:"telemetry,omitempty" yaml:"telemetry,omitempty"`
	Probes       map[string]Probe       `json:"probes,omitempty" yaml:"probes,omitempty"`
	Peers        map[string]string      `json:"peers,omitempty" yaml:"peers,omitempty"`
	Alerts       *Alerts                `json:"alerts,omitempty" yaml:"alerts,omitempty"`
	Ingress      *Ingress               `json:"ingress,omitempty" yaml:"ingress,omitempty"`
	Aliases      map[string]string      `json:"aliases,omitempty" yaml:"aliases,omitempty"`

	// MinVersion is the oldest Berthy release this manifest works with, such as "0.9.0". The CLI and the agent refuse a manifest
	// that needs a newer release before they send or change anything.
	MinVersion string `json:"min_version,omitempty" yaml:"min_version,omitempty"`

	// Timezone is the IANA name of the zone every schedule of the manifest is read in, such as "Europe/Lisbon". It is UTC
	// when empty. Location gives the zone and ParseSchedule a schedule in it.
	Timezone string `json:"timezone,omitempty" yaml:"timezone,omitempty"`

	// Connector marks the manifest Berthy builds to run the ingress connector. It is never read from JSON or YAML.
	Connector bool `json:"-" yaml:"-"`
}
type Ingress struct {
	Cloudflare *Cloudflare `json:"cloudflare,omitempty" yaml:"cloudflare,omitempty"`
}
type Cloudflare struct {
	AccountID      string `json:"account_id" yaml:"account_id"`
	APITokenSecret string `json:"api_token_secret" yaml:"api_token_secret"`
	Image          string `json:"image" yaml:"image"`
	Replicas       *int   `json:"replicas,omitempty" yaml:"replicas,omitempty"`
	Tunnel         string `json:"tunnel,omitempty" yaml:"tunnel,omitempty"`
	DNS            *bool  `json:"dns,omitempty" yaml:"dns,omitempty"`
}
type Registry struct {
	Username       string `json:"username" yaml:"username"`
	PasswordSecret string `json:"password_secret" yaml:"password_secret"`
}
type App struct {
	Kind        string          `json:"kind" yaml:"kind"`
	Image       string          `json:"image,omitempty" yaml:"image,omitempty"`
	File        string          `json:"file,omitempty" yaml:"file,omitempty"`
	ProjectName string          `json:"project_name,omitempty" yaml:"project_name,omitempty"`
	Stateful    bool            `json:"stateful,omitempty" yaml:"stateful,omitempty"`
	Roles       map[string]Role `json:"roles,omitempty" yaml:"roles,omitempty"`
	Env         Env             `json:"env,omitempty" yaml:"env,omitempty"`
	Deploy      Deploy          `json:"deploy,omitempty" yaml:"deploy,omitempty"`
	Hooks       Hooks           `json:"hooks,omitempty" yaml:"hooks,omitempty"`
	Migrations  *Migrations     `json:"migrations,omitempty" yaml:"migrations,omitempty"`
	// DependsOn names the apps that a deploy of every app runs before this one. It orders `berthy deploy` without an app
	// and nothing else: a deploy of one app does not deploy or check its dependencies.
	DependsOn []string `json:"depends_on,omitempty" yaml:"depends_on,omitempty"`
}
type Role struct {
	Service    string     `json:"service,omitempty" yaml:"service,omitempty"`
	Command    []string   `json:"command,omitempty" yaml:"command,omitempty"`
	Entrypoint []string   `json:"entrypoint,omitempty" yaml:"entrypoint,omitempty"`
	User       string     `json:"user,omitempty" yaml:"user,omitempty"`
	Replicas   *int       `json:"replicas,omitempty" yaml:"replicas,omitempty"`
	Port       int        `json:"port,omitempty" yaml:"port,omitempty"`
	Health     *Health    `json:"health,omitempty" yaml:"health,omitempty"`
	Volumes    []string   `json:"volumes,omitempty" yaml:"volumes,omitempty"`
	Resources  *Resources `json:"resources,omitempty" yaml:"resources,omitempty"`
	Hostnames  []string   `json:"hostnames,omitempty" yaml:"hostnames,omitempty"`
	Logging    *Logging   `json:"logging,omitempty" yaml:"logging,omitempty"`
}
type Resources struct {
	CPU    string `json:"cpu,omitempty" yaml:"cpu,omitempty"`
	Memory string `json:"memory,omitempty" yaml:"memory,omitempty"`
}

// Logging is a role's log driver and options. It replaces Berthy's default log rotation for the role.
type Logging struct {
	Driver  string            `json:"driver" yaml:"driver"`
	Options map[string]string `json:"options,omitempty" yaml:"options,omitempty"`
}
type Health struct {
	Test          []string `json:"test" yaml:"test"`
	Interval      string   `json:"interval,omitempty" yaml:"interval,omitempty"`
	Timeout       string   `json:"timeout,omitempty" yaml:"timeout,omitempty"`
	StartPeriod   string   `json:"start_period,omitempty" yaml:"start_period,omitempty"`
	StartInterval string   `json:"start_interval,omitempty" yaml:"start_interval,omitempty"`
	Retries       *int     `json:"retries,omitempty" yaml:"retries,omitempty"`
}
type Env struct {
	Plain  map[string]string `json:"plain,omitempty" yaml:"plain,omitempty"`
	Secret []SecretVar       `json:"secret,omitempty" yaml:"secret,omitempty"`
	// Template holds variables whose value is text that refers to secrets as ${NAME}. The agent fills it in when it deploys,
	// and the filled value is a secret.
	Template map[string]string `json:"template,omitempty" yaml:"template,omitempty"`
}

// SecretVar is one entry of env.secret: the variable a container gets and the secret whose value it takes. In a manifest it
// is a name, for a variable of the same name as its secret, or a one-key map from the variable to the secret.
type SecretVar struct {
	Var    string
	Secret string
}

// MarshalJSON writes the bare name when the variable is the secret's name, so a manifest without a mapping keeps the form it
// was stored in, and {"VAR":"SECRET"} otherwise.
func (s SecretVar) MarshalJSON() ([]byte, error) {
	if s.Var == s.Secret {
		return json.Marshal(s.Secret)
	}
	return json.Marshal(map[string]string{s.Var: s.Secret})
}

// UnmarshalJSON reads a name, or an object with exactly one key, a variable, whose value is a secret's name. Anything else
// is refused here, so the rule that says what an entry is can be shown to the user.
func (s *SecretVar) UnmarshalJSON(b []byte) error {
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return errSecretEntry
	}
	switch x := v.(type) {
	case string:
		s.Var, s.Secret = x, x
	case map[string]any:
		if len(x) != 1 {
			return errSecretEntry
		}
		for name, value := range x {
			secret, ok := value.(string)
			if !ok || name == "" {
				return errSecretEntry
			}
			s.Var, s.Secret = name, secret
		}
	default:
		return errSecretEntry
	}
	return nil
}

var errSecretEntry = errors.New("env.secret entry must be a name or VARIABLE: SECRET")

// SecretNames returns the names of the secrets the entries and the templates read, sorted and without duplicates. A malformed
// template adds nothing: validation refuses it.
func (e Env) SecretNames() []string {
	seen := map[string]bool{}
	var names []string
	add := func(name string) {
		if !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	for _, s := range e.Secret {
		add(s.Secret)
	}
	for _, t := range e.Template {
		refs, _ := TemplateRefs(t)
		for _, name := range refs {
			add(name)
		}
	}
	sort.Strings(names)
	return names
}

type Deploy struct {
	DeployTimeout string `json:"deploy_timeout,omitempty" yaml:"deploy_timeout,omitempty"`
	DrainTimeout  string `json:"drain_timeout,omitempty" yaml:"drain_timeout,omitempty"`
	Retain        *int   `json:"retain,omitempty" yaml:"retain,omitempty"`
	// StartAttempts is how many failed task starts a rollout tolerates before it gives up; default 3, from 1 to 10.
	StartAttempts *int `json:"start_attempts,omitempty" yaml:"start_attempts,omitempty"`
	// Order is how an update replaces each role's old version: StartFirst or StopFirst. Empty means UpdateOrder decides per role.
	Order string `json:"order,omitempty" yaml:"order,omitempty"`
	// Verify turns off the checks, at the end of a deploy, that the roles can be reached. Nil is on: use Verifies.
	Verify *bool `json:"verify,omitempty" yaml:"verify,omitempty"`
}

// Verifies reports whether a deploy checks, once it is done, that its roles can be reached. It is true unless the manifest
// says verify: false, and for a stored manifest that predates the setting.
func (d Deploy) Verifies() bool { return d.Verify == nil || *d.Verify }

type Hooks struct {
	PreDeploy  []Hook `json:"pre_deploy,omitempty" yaml:"pre_deploy,omitempty"`
	PostDeploy []Hook `json:"post_deploy,omitempty" yaml:"post_deploy,omitempty"`
}
type Hook struct {
	Backup string   `json:"backup,omitempty" yaml:"backup,omitempty"`
	Run    []string `json:"run,omitempty" yaml:"run,omitempty"`
	// Timeout and Retries are for run hooks only: the time limit of each attempt (default 5m, from 1s to 24h) and how
	// many more times a command that exited non-zero is run (default 0, up to 5).
	Timeout string `json:"timeout,omitempty" yaml:"timeout,omitempty"`
	Retries *int   `json:"retries,omitempty" yaml:"retries,omitempty"`
}
type Migrations struct {
	Version []string `json:"version" yaml:"version"`
	Apply   []string `json:"apply" yaml:"apply"`
	Revert  []string `json:"revert,omitempty" yaml:"revert,omitempty"`
	// Timeout limits each migration command (default 5m, from 1s to 24h). Retries runs a failed apply again (default 0, up
	// to 5); version and revert are never retried.
	Timeout string `json:"timeout,omitempty" yaml:"timeout,omitempty"`
	Retries *int   `json:"retries,omitempty" yaml:"retries,omitempty"`
}
type Job struct {
	App      string   `json:"app" yaml:"app"`
	Role     string   `json:"role,omitempty" yaml:"role,omitempty"`
	Service  string   `json:"service,omitempty" yaml:"service,omitempty"`
	Schedule string   `json:"schedule,omitempty" yaml:"schedule,omitempty"`
	Command  []string `json:"command" yaml:"command"`
	Timeout  string   `json:"timeout,omitempty" yaml:"timeout,omitempty"` // default "1h"; from 1s to 24h
}

// Job timeouts: a job that sets none gets defaultJobTimeout, which stored manifests show. A timeout runs from
// minJobTimeout to maxJobTimeout.
const (
	defaultJobTimeout = "1h"
	minJobTimeout     = time.Second
	maxJobTimeout     = 24 * time.Hour
)

// Rollout and command settings. A setting left out takes its default, which stored manifests show.
const (
	defaultStartAttempts  = 3
	maxStartAttempts      = 10
	defaultCommandTimeout = "5m"
	minCommandTimeout     = time.Second
	maxCommandTimeout     = 24 * time.Hour
	maxCommandRetries     = 5
)

// StartAttemptLimit is how many failed task starts a rollout of the app tolerates. A manifest stored before the setting
// existed answers with the default.
func (d Deploy) StartAttemptLimit() int {
	if d.StartAttempts == nil {
		return defaultStartAttempts
	}
	return *d.StartAttempts
}

func commandTimeout(s string) time.Duration {
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		d, _ = time.ParseDuration(defaultCommandTimeout)
	}
	return d
}

func commandRetries(n *int) int {
	if n == nil {
		return 0
	}
	return *n
}

// validCommandTimeout accepts an empty timeout, which takes the default, or a duration from 1s to 24h.
func validCommandTimeout(s string) bool {
	if s == "" {
		return true
	}
	d, err := time.ParseDuration(s)
	return err == nil && d >= minCommandTimeout && d <= maxCommandTimeout
}

func validCommandRetries(n *int) bool { return n == nil || *n >= 0 && *n <= maxCommandRetries }

// TimeoutDuration is the time limit of each attempt of a run hook.
func (h Hook) TimeoutDuration() time.Duration { return commandTimeout(h.Timeout) }

// RetryCount is how many more times a run hook is run after it exits non-zero.
func (h Hook) RetryCount() int { return commandRetries(h.Retries) }

// TimeoutDuration is the time limit of each migration command.
func (x Migrations) TimeoutDuration() time.Duration { return commandTimeout(x.Timeout) }

// RetryCount is how many more times a failed apply is run.
func (x Migrations) RetryCount() int { return commandRetries(x.Retries) }

// TimeoutDuration returns the job's timeout. The manifest is validated and defaulted before use, so the fallback only
// covers a Job built in code.
func (j Job) TimeoutDuration() time.Duration {
	d, err := time.ParseDuration(j.Timeout)
	if err != nil {
		d, _ = time.ParseDuration(defaultJobTimeout)
	}
	return d
}

// Backup is a copy of one thing an app holds, kept in object storage. It has exactly one source: a volume, which is archived,
// or a command, whose standard output is stored compressed. A command backup names where the command runs as a job does:
// service for a Compose app, role for an image app.
type Backup struct {
	App         string     `json:"app" yaml:"app"`
	Volume      string     `json:"volume,omitempty" yaml:"volume,omitempty"`
	Command     []string   `json:"command,omitempty" yaml:"command,omitempty"`
	Role        string     `json:"role,omitempty" yaml:"role,omitempty"`
	Service     string     `json:"service,omitempty" yaml:"service,omitempty"`
	Timeout     string     `json:"timeout,omitempty" yaml:"timeout,omitempty"` // a command backup only; default "1h"; from 1s to 24h
	Schedule    string     `json:"schedule,omitempty" yaml:"schedule,omitempty"`
	Destination string     `json:"destination" yaml:"destination"`
	Prefix      string     `json:"prefix,omitempty" yaml:"prefix,omitempty"`
	Before      string     `json:"before,omitempty" yaml:"before,omitempty"`
	Retention   *Retention `json:"retention,omitempty" yaml:"retention,omitempty"`
}

// TimeoutDuration is the time limit of a command backup.
func (b Backup) TimeoutDuration() time.Duration {
	d, err := time.ParseDuration(b.Timeout)
	if err != nil {
		d, _ = time.ParseDuration(defaultJobTimeout)
	}
	return d
}

// Job is the job a command backup runs as: the same app, target, command and time limit.
func (b Backup) Job() Job {
	return Job{App: b.App, Role: b.Role, Service: b.Service, Command: b.Command, Timeout: b.Timeout}
}

type Retention struct {
	Keep any `json:"keep,omitempty" yaml:"keep,omitempty"`
}
type Destination struct {
	Type            string `json:"type" yaml:"type"`
	Endpoint        string `json:"endpoint,omitempty" yaml:"endpoint,omitempty"`
	Region          string `json:"region,omitempty" yaml:"region,omitempty"`
	Bucket          string `json:"bucket" yaml:"bucket"`
	AccessKeySecret string `json:"access_key_secret" yaml:"access_key_secret"`
	SecretKeySecret string `json:"secret_key_secret" yaml:"secret_key_secret"`
}

// StateBackup is the environment's backup of the agent's SQLite state. Destination names a destination of the manifest.
// Prefix is the object prefix; empty means berthy-state/<environment>, which the agent resolves because it knows the
// environment. Schedule is a UTC cron expression and Keep the number of snapshots that are retained.
type StateBackup struct {
	Destination string `json:"destination" yaml:"destination"`
	Prefix      string `json:"prefix,omitempty" yaml:"prefix,omitempty"`
	Schedule    string `json:"schedule,omitempty" yaml:"schedule,omitempty"`
	Keep        *int   `json:"keep,omitempty" yaml:"keep,omitempty"`
}

const (
	// DefaultStateBackupSchedule runs a state backup daily at 03:17 UTC.
	DefaultStateBackupSchedule = "17 3 * * *"
	// DefaultStateBackupKeep is how many snapshots a state backup retains.
	DefaultStateBackupKeep = 14
)

// stateBackupPrefixSegment is one segment of a state backup prefix. Like a volume backup's prefix, it allows no dot segments.
var stateBackupPrefixSegment = regexp.MustCompile(`^[A-Za-z0-9._@+=-]+$`)

type Telemetry struct {
	OTLPEndpoint  string `json:"otlp_endpoint" yaml:"otlp_endpoint"`
	HeadersSecret string `json:"headers_secret,omitempty" yaml:"headers_secret,omitempty"`
}

var identifier = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)
var hostnamePattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)+$`)
var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
var digest = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

// bucketName is an S3 bucket name: 3 to 63 characters of lower-case letters, digits, dots and hyphens, starting and
// ending with a letter or digit.
var bucketName = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9.-]{1,61}[a-z0-9])$`)
var regionName = regexp.MustCompile(`^[a-z0-9-]{1,32}$`)

// logDriverName is a Docker log driver: a built-in name such as json-file, or a plugin image such as
// grafana/loki-docker-driver:latest.
// userName is a container user: a name or number, with a group after a colon.
var userName = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,63}(:[A-Za-z0-9_][A-Za-z0-9_.-]{0,63})?$`)
var logDriverName = regexp.MustCompile(`^[a-z0-9][a-z0-9._/:-]{0,127}$`)
var logOptionKey = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// ValidNetwork reports whether name is a valid network name, by the rule the manifest applies to its network.
func ValidNetwork(name string) bool { return identifier.MatchString(name) }

// ValidEndpoint checks a destination endpoint. It accepts an empty endpoint, which means the AWS default, or
// https://host[:port]. It accepts http:// only for a loopback address or localhost, which tests and the WSL fixture use.
// An endpoint has no path, query, fragment or userinfo, so credentials can go only to the host named. The error never
// repeats the endpoint.
func ValidEndpoint(raw string) error {
	if raw == "" {
		return nil
	}
	if strings.ContainsAny(raw, "?#") {
		return errors.New("must not have a query or fragment")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return errors.New("not a URL")
	}
	if u.User != nil {
		return errors.New("must not contain credentials")
	}
	if u.Path != "" {
		return errors.New("must not have a path")
	}
	host := u.Hostname()
	if host == "" || strings.HasSuffix(u.Host, ":") {
		return errors.New("must be scheme://host[:port]")
	}
	if p := u.Port(); p != "" {
		if n, err := strconv.Atoi(p); err != nil || n < 1 || n > 65535 {
			return errors.New("port must be 1 to 65535")
		}
	}
	if !validHost(host) {
		return errors.New("host is not a valid name or address")
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if isLoopback(host) {
			return nil
		}
		return errors.New("http is allowed only for a loopback address or localhost")
	default:
		return errors.New("scheme must be https, or http for a loopback address")
	}
}

// endpointHost is a DNS name whose labels may be a single label, such as a service name on a private network.
var endpointHost = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)*$`)

func validHost(host string) bool {
	return isLoopback(host) || endpointHost.MatchString(strings.ToLower(host)) || net.ParseIP(host) != nil
}

func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// readYAML reads one manifest file. An overlay is optional: it is nil when it is not there. What is wrong with its content is
// returned as Problems of that file, each with its line.
func readYAML(path string, overlay bool) (*yaml.Node, error) {
	f, err := os.Open(path)
	if overlay && os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxInput+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxInput {
		return nil, fmt.Errorf("%s exceeds size limit", path)
	}
	n, err := parseYAML(b, overlay)
	if err != nil {
		var ps Problems
		if !errors.As(err, &ps) {
			ps = Problems{{Message: err.Error()}}
		}
		for i := range ps {
			ps[i].File = filepath.Base(path)
		}
		return nil, ps
	}
	return n, nil
}

// parseYAML parses a manifest, a mapping of one document without aliases, anchors or duplicate keys. Null is refused, except
// in an overlay, where it is the value of a key of a mapping and removes that key. What it refuses of the content is returned
// as Problems, each with the line and column in b.
func parseYAML(b []byte, overlay bool) (*yaml.Node, error) {
	if len(b) > maxInput {
		return nil, fmt.Errorf("manifest exceeds size limit")
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	var doc yaml.Node
	if err := dec.Decode(&doc); err != nil {
		return nil, err
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("multiple documents are not allowed")
		}
		return nil, err
	}
	if len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("manifest must be a mapping")
	}
	var ps Problems
	checkNode(doc.Content[0], 0, "", overlay, &ps)
	if len(ps) > 0 {
		return nil, ps
	}
	return doc.Content[0], nil
}

// checkNode records every alias, anchor, null, key that is not a string or is repeated, and level of nesting beyond the limit.
// removal is true for a value that a null may remove a key with: a value of an overlay's mappings. Such a null is written null
// or ~, because an empty value could be a key left empty by mistake, and removing it would not show.
func checkNode(n *yaml.Node, depth int, path string, removal bool, p *Problems) {
	if depth > 64 {
		p.addAt(n, path, "manifest nesting too deep")
		return
	}
	if n.Kind == yaml.AliasNode || n.Anchor != "" || (n.Tag == "!!null" && !removal) {
		p.addAt(n, path, "aliases, anchors and null are not allowed")
		return
	}
	if n.Tag == "!!null" && n.Value == "" {
		p.addAt(n, path, "an empty value is not allowed: write null to remove a key")
		return
	}
	if n.Kind == yaml.MappingNode {
		seen := map[string]bool{}
		for i := 0; i+1 < len(n.Content); i += 2 {
			k := n.Content[i]
			if k.Kind != yaml.ScalarNode || k.Tag != "!!str" || k.Anchor != "" || seen[k.Value] {
				p.addAt(k, join(path, k.Value), "invalid or duplicate key %q", k.Value)
			}
			seen[k.Value] = true
			checkNode(n.Content[i+1], depth+1, join(path, k.Value), removal, p)
		}
		return
	}
	// A list's items are never removals: an overlay's list replaces the base list as a whole.
	for i, c := range n.Content {
		checkNode(c, depth+1, join(path, strconv.Itoa(i)), false, p)
	}
}

// minVersionPattern is a release version as min_version takes it: a version such as 0.9.0, with an optional "v" and an optional
// pre-release.
var minVersionPattern = regexp.MustCompile(`^v?[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$`)

// TooOld is the error of a manifest whose min_version is above the running release: Need is the oldest release the manifest
// works with, and Have is the running one.
type TooOld struct{ Need, Have string }

func (e *TooOld) Error() string {
	return fmt.Sprintf("manifest needs Berthy %s or newer; running %s", e.Need, e.Have)
}

// CheckVersion reports a *TooOld when running is a release below the manifest's min_version. A manifest without one, or a
// development build, passes.
func (m *Manifest) CheckVersion(running string) error {
	if m.MinVersion == "" {
		return nil
	}
	satisfied, ok := version.Satisfies(running, m.MinVersion)
	if !ok || satisfied {
		return nil
	}
	return &TooOld{Need: m.MinVersion, Have: running}
}

// FilePath is the path, on the OS that runs this, of a file the manifest names relative to its directory, such as a Compose
// app's file: a backslash is a separator on every OS, as the manifest check reads it.
func FilePath(file string) string { return filepath.FromSlash(strings.ReplaceAll(file, `\`, "/")) }

// Files are the names, relative to the manifest directory, of the files Load reads for env: the base manifest, and the overlay
// of env, which is optional.
func Files(env string) []string {
	return []string{"berthy.yml", "berthy." + env + ".yml"}
}

// Load reads the manifest of env: the base file, and the overlay of env when there is one. A manifest that is not valid is
// returned as Problems, every one of them with its file and its line, which say whether the base or the overlay is at fault.
func Load(dir, env string) (*Manifest, error) {
	l, err := LoadDetailed(dir, env)
	if err != nil {
		return nil, err
	}
	return l.Manifest, nil
}

// Loaded is a valid manifest of one environment, with what Load found about it: its warnings, and where each value is written.
type Loaded struct {
	Manifest *Manifest
	Warnings Problems
	where    locator
}

// LoadDetailed is Load, and also returns the warnings of a valid manifest, placed in its files like the problems, and the file
// and line of every value (Origins). A manifest with problems returns them and no warnings.
func LoadDetailed(dir, env string) (*Loaded, error) {
	if !identifier.MatchString(env) {
		return nil, fmt.Errorf("invalid environment %q", env)
	}
	names := Files(env)
	var parse Problems
	read := func(name string, overlay bool) (*yaml.Node, error) {
		n, err := readYAML(filepath.Join(dir, name), overlay)
		var ps Problems
		if errors.As(err, &ps) {
			parse = append(parse, ps...)
			return nil, nil
		}
		return n, err
	}
	base, err := read(names[0], false)
	if err != nil {
		return nil, err
	}
	overlay, err := read(names[1], true)
	if err != nil {
		return nil, err
	}
	if len(parse) > 0 {
		return nil, parse
	}
	// Merge into a copy, so the files' own trees keep their positions to place the problems by.
	where := locator{base: source{names[0], base}}
	merged := cloneNode(base)
	var warnings Problems
	if overlay != nil {
		where.overlay = &source{names[1], overlay}
		o := overlayMerge{baseName: names[0]}
		o.merge(merged, overlay, "")
		warnings = o.warnings
	}
	var value any
	if err = merged.Decode(&value); err != nil {
		return nil, err
	}
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	m, err := DecodeJSON(data)
	if err != nil {
		var ps Problems
		if errors.As(err, &ps) {
			where.place(ps)
		}
		return nil, err
	}
	if err := m.CheckVersion(version.Version); err != nil {
		var old *TooOld
		if errors.As(err, &old) {
			return nil, fmt.Errorf("this manifest needs Berthy %s or newer; this CLI is %s", old.Need, old.Have)
		}
		return nil, err
	}
	warnings = append(warnings, m.Warnings()...)
	where.place(warnings)
	return &Loaded{Manifest: m, Warnings: warnings, where: where}, nil
}

// DecodeJSON decodes and validates a manifest sent to the agent. It never sets the connector flag.
func DecodeJSON(data []byte) (*Manifest, error) {
	return decodeManifest(data, false)
}

// DecodeRelease decodes and validates a manifest that Berthy stored with a release. connector is true only for the
// connector's own release, the one manifest that may carry the tunnel token.
func DecodeRelease(data []byte, connector bool) (*Manifest, error) {
	return decodeManifest(data, connector)
}

func decodeManifest(data []byte, connector bool) (*Manifest, error) {
	if !json.Valid(data) {
		return nil, fmt.Errorf("invalid JSON manifest")
	}
	// Parse once for duplicate keys and explicit null, then walk the tree against the types for unknown fields and values of
	// the wrong type, before the strict JSON decoding.
	node, err := parseYAML(data, false)
	if err != nil {
		return nil, err
	}
	var w shapeWalk
	w.walk(node, reflect.TypeOf(Manifest{}), "", "", "")
	var m Manifest
	d := json.NewDecoder(bytes.NewReader(data))
	found := w.found()
	if len(found) == 0 {
		d.DisallowUnknownFields()
	}
	m.Connector = connector
	if err := d.Decode(&m); err != nil {
		// The walk names what it finds; the decoder's own error is only a net for what the walk does not know.
		if len(found) == 0 {
			found.add("", "%s", err.Error())
		}
		return nil, found
	}
	// Unknown fields are ignored by the decoder, so the manifest it read can still be checked. A value of the wrong type
	// leaves the manifest unreliable, so it is not.
	problems := found
	if len(w.wrong) == 0 {
		problems = append(problems, m.Check()...)
	}
	if len(problems) > 0 {
		return nil, problems
	}
	m.defaults()
	return &m, nil
}
func sortedKeys[T any](m map[string]T) []string {
	k := make([]string, 0, len(m))
	for x := range m {
		k = append(k, x)
	}
	sort.Strings(k)
	return k
}
func argv(a []string) bool {
	if len(a) == 0 || a[0] == "" {
		return false
	}
	for _, s := range a {
		if strings.ContainsRune(s, 0) {
			return false
		}
	}
	return true
}
func duration(s string, zero bool) bool {
	if s == "" {
		return true
	}
	d, e := time.ParseDuration(s)
	return e == nil && (d > 0 || zero && d == 0)
}

// ParseCommandTimeout parses the time limit of a job or of a run: "" is the default (1h); otherwise from 1s to 24h.
func ParseCommandTimeout(s string) (time.Duration, error) {
	if s == "" {
		s = defaultJobTimeout
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < minJobTimeout || d > maxJobTimeout {
		return 0, fmt.Errorf("invalid timeout")
	}
	return d, nil
}

func validJobTimeout(s string) bool {
	_, err := ParseCommandTimeout(s)
	return err == nil
}

// Defaulted returns a copy of m with the defaults a stored manifest takes, without checking it. Two manifests compare by what
// they run once both are defaulted, whether or not they were when they were written.
func (m Manifest) Defaulted() Manifest {
	b, err := json.Marshal(m)
	if err != nil {
		return m
	}
	var out Manifest
	if json.Unmarshal(b, &out) != nil {
		return m
	}
	out.Connector = m.Connector
	out.defaults()
	return out
}

func (m *Manifest) defaults() {
	m.defaultsMonitoring()
	if sb := m.StateBackup; sb != nil {
		if sb.Schedule == "" {
			sb.Schedule = DefaultStateBackupSchedule
		}
		if sb.Keep == nil {
			n := DefaultStateBackupKeep
			sb.Keep = &n
		}
	}
	for name, j := range m.Jobs {
		if j.Timeout == "" {
			j.Timeout = defaultJobTimeout
		}
		m.Jobs[name] = j
	}
	for name, b := range m.Backups {
		if len(b.Command) > 0 && b.Timeout == "" {
			b.Timeout = defaultJobTimeout
			m.Backups[name] = b
		}
	}
	for k, a := range m.Apps {
		if a.Deploy.DeployTimeout == "" {
			a.Deploy.DeployTimeout = "120s"
		}
		if a.Deploy.DrainTimeout == "" {
			a.Deploy.DrainTimeout = "30s"
		}
		if a.Deploy.Retain == nil {
			n := 5
			a.Deploy.Retain = &n
		}
		if a.Deploy.StartAttempts == nil {
			n := defaultStartAttempts
			a.Deploy.StartAttempts = &n
		}
		for _, hooks := range [][]Hook{a.Hooks.PreDeploy, a.Hooks.PostDeploy} {
			for i, h := range hooks {
				if h.Backup != "" {
					continue
				}
				if h.Timeout == "" {
					hooks[i].Timeout = defaultCommandTimeout
				}
				if h.Retries == nil {
					hooks[i].Retries = new(int)
				}
			}
		}
		if x := a.Migrations; x != nil {
			if x.Timeout == "" {
				x.Timeout = defaultCommandTimeout
			}
			if x.Retries == nil {
				x.Retries = new(int)
			}
		}
		for rn, r := range a.Roles {
			if r.Replicas == nil {
				n := 1
				r.Replicas = &n
			}
			if h := r.Health; h != nil {
				if h.Interval == "" {
					h.Interval = "30s"
				}
				if h.Timeout == "" {
					h.Timeout = "30s"
				}
				if h.StartPeriod == "" {
					h.StartPeriod = "0s"
				}
				if h.Retries == nil {
					n := 3
					h.Retries = &n
				}
			}
			a.Roles[rn] = r
		}
		m.Apps[k] = a
	}
}
func (m *Manifest) CheckImageDeploy(app, version string, protected bool) (string, error) {
	a, ok := m.Apps[app]
	if !ok || a.Kind != "image" {
		return "", fmt.Errorf("app %q is not an image app", app)
	}
	repo, e := reference.ParseNormalizedNamed(a.Image)
	if e != nil {
		return "", e
	}
	if digest.MatchString(version) {
		version = reference.TrimNamed(repo).String() + "@" + version
	} else if !strings.ContainsAny(version, "/@") {
		version = reference.TrimNamed(repo).String() + ":" + version
	}
	selected, e := reference.ParseNormalizedNamed(version)
	if e != nil {
		return "", fmt.Errorf("invalid image reference: %w", e)
	}
	if reference.TrimNamed(selected).Name() != reference.TrimNamed(repo).Name() {
		return "", fmt.Errorf("image repository does not match app")
	}
	if _, ok := selected.(reference.Canonical); (protected || m.Protected) && !ok {
		return "", fmt.Errorf("protected environment requires a digest")
	}
	if _, tagged := selected.(reference.NamedTagged); (protected || m.Protected) && tagged {
		return "", fmt.Errorf("protected environment requires digest without tag")
	}
	if canonical, ok := selected.(reference.Canonical); ok && !digest.MatchString(canonical.Digest().String()) {
		return "", fmt.Errorf("image requires a sha256 digest")
	}
	if reference.IsNameOnly(selected) {
		return "", fmt.Errorf("image version required")
	}
	return selected.String(), nil
}

// TunnelName is the tunnel's name in Cloudflare: the configured name, or <project>-<environment>.
func (m *Manifest) TunnelName(environment string) string {
	if m.Ingress != nil && m.Ingress.Cloudflare != nil && m.Ingress.Cloudflare.Tunnel != "" {
		return m.Ingress.Cloudflare.Tunnel
	}
	return m.Project + "-" + environment
}

// ImageRepos returns the sorted, distinct repositories ("acme/api") of the image apps whose images are on registry.
func (m *Manifest) ImageRepos(registry string) []string {
	var repos []string
	for _, app := range m.Apps {
		if app.Image == "" {
			continue
		}
		named, err := reference.ParseNormalizedNamed(app.Image)
		if err != nil || reference.Domain(named) != registry {
			continue
		}
		repos = append(repos, reference.Path(named))
	}
	slices.Sort(repos)
	return slices.Compact(repos)
}

// ConnectorApp returns the image app that runs the managed ingress connector, and the version to deploy:
// the connector image's digest when it is pinned by digest, otherwise its tag.
func (m *Manifest) ConnectorApp() (App, string, error) {
	if m.Ingress == nil || m.Ingress.Cloudflare == nil {
		return App{}, "", errors.New("managed ingress is not configured")
	}
	c := m.Ingress.Cloudflare
	ref, err := reference.ParseNormalizedNamed(c.Image)
	if err != nil {
		return App{}, "", errors.New("invalid Cloudflare image")
	}
	var version string
	switch v := ref.(type) {
	case reference.Canonical:
		version = v.Digest().String()
	case reference.NamedTagged:
		version = v.Tag()
	default:
		return App{}, "", errors.New("invalid Cloudflare image: a tag or digest is required")
	}
	replicas := 1
	if c.Replicas != nil {
		replicas = *c.Replicas
	}
	retries := 3
	retain := 3
	attempts := defaultStartAttempts
	app := App{
		Kind:  "image",
		Image: reference.TrimNamed(ref).String(),
		Roles: map[string]Role{"connector": {
			Replicas: &replicas,
			Command:  []string{"tunnel", "--no-autoupdate", "--metrics", fmt.Sprintf("0.0.0.0:%d", ConnectorMetricsPort), "run"},
			Health: &Health{
				Test:        []string{"CMD", "cloudflared", "tunnel", "--metrics", fmt.Sprintf("127.0.0.1:%d", ConnectorMetricsPort), "ready"},
				Interval:    "10s",
				Timeout:     "5s",
				StartPeriod: "30s",
				Retries:     &retries,
			},
		}},
		Env:    Env{Secret: []SecretVar{{Var: TunnelTokenSecret, Secret: TunnelTokenSecret}}},
		Deploy: Deploy{DeployTimeout: "180s", DrainTimeout: "30s", Retain: &retain, StartAttempts: &attempts},
	}
	return app, version, nil
}

// Link SHA-256 so reference can validate sha256 digests.
var _ = sha256.Size
