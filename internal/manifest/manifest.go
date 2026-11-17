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

	"berthy/internal/schedule"
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
	Service   string     `json:"service,omitempty" yaml:"service,omitempty"`
	Command   []string   `json:"command,omitempty" yaml:"command,omitempty"`
	Replicas  *int       `json:"replicas,omitempty" yaml:"replicas,omitempty"`
	Port      int        `json:"port,omitempty" yaml:"port,omitempty"`
	Health    *Health    `json:"health,omitempty" yaml:"health,omitempty"`
	Volumes   []string   `json:"volumes,omitempty" yaml:"volumes,omitempty"`
	Resources *Resources `json:"resources,omitempty" yaml:"resources,omitempty"`
	Hostnames []string   `json:"hostnames,omitempty" yaml:"hostnames,omitempty"`
	Logging   *Logging   `json:"logging,omitempty" yaml:"logging,omitempty"`
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
	Secret []string          `json:"secret,omitempty" yaml:"secret,omitempty"`
}
type Deploy struct {
	DeployTimeout string `json:"deploy_timeout,omitempty" yaml:"deploy_timeout,omitempty"`
	DrainTimeout  string `json:"drain_timeout,omitempty" yaml:"drain_timeout,omitempty"`
	Retain        *int   `json:"retain,omitempty" yaml:"retain,omitempty"`
	// StartAttempts is how many failed task starts a rollout tolerates before it gives up; default 3, from 1 to 10.
	StartAttempts *int `json:"start_attempts,omitempty" yaml:"start_attempts,omitempty"`
}
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

type Backup struct {
	App         string     `json:"app" yaml:"app"`
	Volume      string     `json:"volume" yaml:"volume"`
	Schedule    string     `json:"schedule,omitempty" yaml:"schedule,omitempty"`
	Destination string     `json:"destination" yaml:"destination"`
	Prefix      string     `json:"prefix,omitempty" yaml:"prefix,omitempty"`
	Before      string     `json:"before,omitempty" yaml:"before,omitempty"`
	Retention   *Retention `json:"retention,omitempty" yaml:"retention,omitempty"`
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

func readYAML(path string, optional bool) (*yaml.Node, error) {
	f, err := os.Open(path)
	if optional && os.IsNotExist(err) {
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
	return parseYAML(b)
}
func parseYAML(b []byte) (*yaml.Node, error) {
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
	if err := checkNode(doc.Content[0], 0); err != nil {
		return nil, err
	}
	return doc.Content[0], nil
}
func checkNode(n *yaml.Node, depth int) error {
	if depth > 64 {
		return fmt.Errorf("manifest nesting too deep")
	}
	if n.Kind == yaml.AliasNode || n.Anchor != "" || n.Tag == "!!null" {
		return fmt.Errorf("aliases, anchors and null are not allowed")
	}
	if n.Kind == yaml.MappingNode {
		seen := map[string]bool{}
		for i := 0; i < len(n.Content); i += 2 {
			k := n.Content[i]
			if k.Kind != yaml.ScalarNode || k.Tag != "!!str" || seen[k.Value] {
				return fmt.Errorf("invalid or duplicate key %q", k.Value)
			}
			seen[k.Value] = true
		}
	}
	for _, c := range n.Content {
		if err := checkNode(c, depth+1); err != nil {
			return err
		}
	}
	return nil
}
func merge(base, overlay *yaml.Node) {
	for i := 0; i < len(overlay.Content); i += 2 {
		k, v := overlay.Content[i], overlay.Content[i+1]
		found := false
		for j := 0; j < len(base.Content); j += 2 {
			if base.Content[j].Value == k.Value {
				if base.Content[j+1].Kind == yaml.MappingNode && v.Kind == yaml.MappingNode {
					merge(base.Content[j+1], v)
				} else {
					base.Content[j+1] = v
				}
				found = true
				break
			}
		}
		if !found {
			base.Content = append(base.Content, k, v)
		}
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

// Files are the names, relative to the manifest directory, of the files Load reads for env: the base manifest, and the overlay
// of env, which is optional.
func Files(env string) []string {
	return []string{"berthy.yml", "berthy." + env + ".yml"}
}

func Load(dir, env string) (*Manifest, error) {
	if !identifier.MatchString(env) {
		return nil, fmt.Errorf("invalid environment %q", env)
	}
	names := Files(env)
	base, err := readYAML(filepath.Join(dir, names[0]), false)
	if err != nil {
		return nil, err
	}
	overlay, err := readYAML(filepath.Join(dir, names[1]), true)
	if err != nil {
		return nil, err
	}
	if overlay != nil {
		merge(base, overlay)
	}
	var value any
	if err = base.Decode(&value); err != nil {
		return nil, err
	}
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	m, err := DecodeJSON(data)
	if err != nil {
		return nil, err
	}
	if err := m.CheckVersion(version.Version); err != nil {
		var old *TooOld
		if errors.As(err, &old) {
			return nil, fmt.Errorf("this manifest needs Berthy %s or newer; this CLI is %s", old.Need, old.Have)
		}
		return nil, err
	}
	return m, nil
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
	// Parse once for duplicate keys and explicit null before strict JSON decoding.
	node, err := parseYAML(data)
	if err != nil {
		return nil, err
	}
	if err := exactKeys(node, reflect.TypeOf(Manifest{})); err != nil {
		return nil, err
	}
	var m Manifest
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	m.Connector = connector
	if err := d.Decode(&m); err != nil {
		return nil, err
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	m.defaults()
	return &m, nil
}
func exactKeys(n *yaml.Node, t reflect.Type) error {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.Struct:
		if n.Kind != yaml.MappingNode {
			return nil
		}
		fields := map[string]reflect.Type{}
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			name := strings.Split(f.Tag.Get("json"), ",")[0]
			if name != "" && name != "-" {
				fields[name] = f.Type
			}
		}
		for i := 0; i < len(n.Content); i += 2 {
			name := n.Content[i].Value
			ft, ok := fields[name]
			if !ok {
				return fmt.Errorf("unknown field %q", name)
			}
			if err := exactKeys(n.Content[i+1], ft); err != nil {
				return err
			}
		}
	case reflect.Map:
		if n.Kind == yaml.MappingNode {
			for i := 1; i < len(n.Content); i += 2 {
				if err := exactKeys(n.Content[i], t.Elem()); err != nil {
					return err
				}
			}
		}
	case reflect.Slice:
		if n.Kind == yaml.SequenceNode {
			for _, v := range n.Content {
				if err := exactKeys(v, t.Elem()); err != nil {
					return err
				}
			}
		}
	}
	return nil
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
func (m *Manifest) Validate() error {
	if m.Ingress != nil {
		c := m.Ingress.Cloudflare
		if c == nil || c.AccountID == "" || !envName.MatchString(c.APITokenSecret) || c.Image == "" || c.Replicas != nil && *c.Replicas < 1 || c.Tunnel != "" && !identifier.MatchString(c.Tunnel) {
			return fmt.Errorf("invalid Cloudflare ingress")
		}
		if c.APITokenSecret == TunnelTokenSecret {
			return fmt.Errorf("%s is reserved for managed ingress", TunnelTokenSecret)
		}
		ref, err := reference.ParseNormalizedNamed(c.Image)
		if err != nil {
			return fmt.Errorf("invalid Cloudflare image")
		}
		if reference.IsNameOnly(ref) {
			return fmt.Errorf("invalid Cloudflare image: a tag or digest is required")
		}
	}
	hostnames := map[string]bool{}
	if !identifier.MatchString(m.Project) || !identifier.MatchString(m.Network) {
		return fmt.Errorf("invalid project or network")
	}
	if len(m.Apps) == 0 {
		return fmt.Errorf("at least one app is required")
	}
	for _, name := range sortedKeys(m.Registries) {
		r := m.Registries[name]
		if r.PasswordSecret == TunnelTokenSecret {
			return fmt.Errorf("registry %s: %s is reserved for managed ingress", name, TunnelTokenSecret)
		}
		if name == "" || strings.ContainsAny(name, "/ @") || r.Username == "" || !envName.MatchString(r.PasswordSecret) {
			return fmt.Errorf("invalid registry %q", name)
		}
	}
	for _, name := range sortedKeys(m.Apps) {
		a := m.Apps[name]
		if !identifier.MatchString(name) {
			return fmt.Errorf("invalid app %q", name)
		}
		if m.Ingress != nil && name == ConnectorAppName {
			return fmt.Errorf("app name %s is reserved for the managed ingress connector", ConnectorAppName)
		}
		if a.ProjectName != "" && !identifier.MatchString(a.ProjectName) {
			return fmt.Errorf("app %s: invalid project name", name)
		}
		if a.Kind != "image" && a.Kind != "compose" {
			return fmt.Errorf("app %s: invalid kind", name)
		}
		if a.Kind == "image" {
			ref, e := reference.ParseNormalizedNamed(a.Image)
			if e != nil || reference.IsNameOnly(ref) == false || len(a.Roles) == 0 {
				return fmt.Errorf("app %s: image must be a repository and roles required", name)
			}
		} else if a.File == "" || filepath.IsAbs(a.File) || strings.Contains(strings.ReplaceAll(a.File, `\`, `/`), "../") {
			return fmt.Errorf("app %s: invalid compose file", name)
		}
		if err := m.checkDependsOn(name, a); err != nil {
			return err
		}
		for _, k := range sortedKeys(a.Env.Plain) {
			if k == TunnelTokenSecret {
				return fmt.Errorf("app %s: %s is reserved for managed ingress", name, TunnelTokenSecret)
			}
			if strings.HasPrefix(k, berthyEnvPrefix) {
				return fmt.Errorf("app %s: names starting with BERTHY_ are reserved for Berthy", name)
			}
			if !envName.MatchString(k) || strings.ContainsRune(a.Env.Plain[k], 0) {
				return fmt.Errorf("app %s: invalid environment name or value", name)
			}
		}
		seen := map[string]bool{}
		for _, s := range a.Env.Secret {
			if s == TunnelTokenSecret && !(m.Connector && name == ConnectorAppName) {
				return fmt.Errorf("app %s: %s is reserved for managed ingress", name, TunnelTokenSecret)
			}
			if strings.HasPrefix(s, berthyEnvPrefix) {
				return fmt.Errorf("app %s: names starting with BERTHY_ are reserved for Berthy", name)
			}
			if !envName.MatchString(s) || seen[s] {
				return fmt.Errorf("app %s: invalid secret", name)
			}
			if _, ok := a.Env.Plain[s]; ok {
				return fmt.Errorf("app %s: duplicate environment name", name)
			}
			seen[s] = true
		}
		if !duration(a.Deploy.DeployTimeout, false) || !duration(a.Deploy.DrainTimeout, false) || a.Deploy.Retain != nil && *a.Deploy.Retain < 1 {
			return fmt.Errorf("app %s: invalid deploy settings", name)
		}
		if n := a.Deploy.StartAttempts; n != nil && (*n < 1 || *n > maxStartAttempts) {
			return fmt.Errorf("app %s: invalid start_attempts", name)
		}
		for _, rn := range sortedKeys(a.Roles) {
			r := a.Roles[rn]
			for _, host := range r.Hostnames {
				if a.Kind != "image" || m.Ingress == nil || r.Port == 0 || len(host) > 253 || !hostnamePattern.MatchString(host) || hostnames[host] {
					return fmt.Errorf("app %s role %s: invalid or duplicate hostname", name, rn)
				}
				hostnames[host] = true
			}
			for _, v := range r.Volumes {
				if _, err := ParseMount(v); err != nil {
					return fmt.Errorf("app %s role %s: invalid volume", name, rn)
				}
			}
			if r.Resources != nil {
				if r.Resources.CPU != "" {
					if _, err := ParseCPU(r.Resources.CPU); err != nil {
						return fmt.Errorf("app %s role %s: invalid CPU", name, rn)
					}
				}
				if r.Resources.Memory != "" {
					if _, err := ParseMemory(r.Resources.Memory); err != nil {
						return fmt.Errorf("app %s role %s: invalid memory", name, rn)
					}
				}
			}
			if r.Logging != nil {
				if a.Kind != "image" {
					return fmt.Errorf("app %s role %s: logging is for image apps", name, rn)
				}
				if !logDriverName.MatchString(r.Logging.Driver) || !validLogOptions(r.Logging.Options) {
					return fmt.Errorf("app %s role %s: invalid logging", name, rn)
				}
			}
			if !identifier.MatchString(rn) || r.Service != "" && !identifier.MatchString(r.Service) || r.Replicas != nil && *r.Replicas < 1 || r.Port < 0 || r.Port > 65535 || len(r.Command) > 0 && !argv(r.Command) {
				return fmt.Errorf("app %s role %s: invalid settings", name, rn)
			}
			if h := r.Health; h != nil {
				if !argv(h.Test) || (h.Test[0] != "CMD" && h.Test[0] != "CMD-SHELL") || !duration(h.Interval, false) || !duration(h.Timeout, false) || !duration(h.StartPeriod, true) || !duration(h.StartInterval, false) || h.Retries != nil && *h.Retries < 1 {
					return fmt.Errorf("app %s role %s: invalid health", name, rn)
				}
			}
		}
		for _, lh := range []struct {
			list  string
			hooks []Hook
		}{{"pre_deploy", a.Hooks.PreDeploy}, {"post_deploy", a.Hooks.PostDeploy}} {
			for n, h := range lh.hooks {
				if (h.Backup == "") == (len(h.Run) == 0) || h.Backup != "" && !identifier.MatchString(h.Backup) || len(h.Run) > 0 && !argv(h.Run) {
					return fmt.Errorf("app %s: invalid hook", name)
				}
				switch {
				case h.Backup != "" && (h.Timeout != "" || h.Retries != nil):
					return fmt.Errorf("app %s: %s hook %d: timeout and retries apply to run hooks only", name, lh.list, n+1)
				case !validCommandTimeout(h.Timeout):
					return fmt.Errorf("app %s: %s hook %d: invalid timeout", name, lh.list, n+1)
				case !validCommandRetries(h.Retries):
					return fmt.Errorf("app %s: %s hook %d: invalid retries", name, lh.list, n+1)
				}
				if _, ok := m.Backups[h.Backup]; h.Backup != "" && !ok {
					return fmt.Errorf("app %s: %s hook %d: unknown backup", name, lh.list, n+1)
				}
			}
		}
		if x := a.Migrations; x != nil {
			if !argv(x.Version) || !argv(x.Apply) || len(x.Revert) > 0 && !argv(x.Revert) {
				return fmt.Errorf("app %s: invalid migrations", name)
			}
			if !validCommandTimeout(x.Timeout) {
				return fmt.Errorf("app %s: invalid migrations timeout", name)
			}
			if !validCommandRetries(x.Retries) {
				return fmt.Errorf("app %s: invalid migrations retries", name)
			}
			n := 0
			for _, s := range x.Revert {
				if strings.Contains(s, "{target}") && s != "{target}" {
					return fmt.Errorf("app %s: target must be a standalone argument", name)
				}
				n += strings.Count(s, "{target}")
			}
			if len(x.Revert) > 0 && n != 1 {
				return fmt.Errorf("app %s: revert requires one {target}", name)
			}
		}
	}
	if err := m.checkDependsCycle(); err != nil {
		return err
	}
	for _, name := range sortedKeys(m.Jobs) {
		j := m.Jobs[name]
		if !identifier.MatchString(name) || !identifier.MatchString(j.App) || !argv(j.Command) || j.Role != "" && !identifier.MatchString(j.Role) || j.Service != "" && !identifier.MatchString(j.Service) || j.Role != "" && j.Service != "" {
			return fmt.Errorf("invalid job %q", name)
		}
		if err := m.validateJob(name, j); err != nil {
			return err
		}
	}
	for _, name := range sortedKeys(m.Destinations) {
		d := m.Destinations[name]
		if d.AccessKeySecret == TunnelTokenSecret || d.SecretKeySecret == TunnelTokenSecret {
			return fmt.Errorf("destination %s: %s is reserved for managed ingress", name, TunnelTokenSecret)
		}
		if !identifier.MatchString(name) || d.Type != "s3" || !envName.MatchString(d.AccessKeySecret) || !envName.MatchString(d.SecretKeySecret) {
			return fmt.Errorf("invalid destination %q", name)
		}
		if !bucketName.MatchString(d.Bucket) {
			return fmt.Errorf("destination %s: invalid bucket name", name)
		}
		if err := ValidEndpoint(d.Endpoint); err != nil {
			return fmt.Errorf("destination %s: invalid endpoint: %w", name, err)
		}
		if d.Region != "" && !regionName.MatchString(d.Region) {
			return fmt.Errorf("destination %s: invalid region", name)
		}
	}
	for _, name := range sortedKeys(m.Backups) {
		b := m.Backups[name]
		if !identifier.MatchString(name) || !identifier.MatchString(b.App) || b.Volume == "" || !identifier.MatchString(b.Destination) || b.Before != "" && !identifier.MatchString(b.Before) {
			return fmt.Errorf("invalid backup %q", name)
		}
		if _, ok := m.Apps[b.App]; !ok {
			return fmt.Errorf("backup %s: unknown app", name)
		}
		if _, ok := m.Destinations[b.Destination]; !ok {
			return fmt.Errorf("backup %s: unknown destination", name)
		}
		if b.Schedule != "" {
			if _, err := schedule.Parse(b.Schedule); err != nil {
				return fmt.Errorf("backup %s: invalid schedule: %w", name, err)
			}
		}
		if b.Before != "" {
			if _, ok := m.Jobs[b.Before]; !ok {
				return fmt.Errorf("backup %s: unknown job", name)
			}
		}
		if b.Retention != nil && b.Retention.Keep != nil {
			switch v := b.Retention.Keep.(type) {
			case string:
				if v != "all" {
					return fmt.Errorf("backup %s: invalid retention", name)
				}
			case float64:
				if v < 1 || v != float64(int64(v)) {
					return fmt.Errorf("backup %s: invalid retention", name)
				}
			default:
				return fmt.Errorf("backup %s: invalid retention", name)
			}
		}
	}
	if sb := m.StateBackup; sb != nil {
		if err := m.validateStateBackup(*sb); err != nil {
			return err
		}
	}
	if m.Telemetry != nil && m.Telemetry.HeadersSecret == TunnelTokenSecret {
		return fmt.Errorf("telemetry: %s is reserved for managed ingress", TunnelTokenSecret)
	}
	if m.Telemetry != nil && (m.Telemetry.OTLPEndpoint == "" || m.Telemetry.HeadersSecret != "" && !envName.MatchString(m.Telemetry.HeadersSecret)) {
		return fmt.Errorf("invalid telemetry")
	}
	if m.MinVersion != "" && !minVersionPattern.MatchString(m.MinVersion) {
		return fmt.Errorf("invalid min_version %q: use a version such as 0.9.0", m.MinVersion)
	}
	if err := validateAliases(m.Aliases); err != nil {
		return err
	}
	return m.validateMonitoring()
}

// maxAliases is the most aliases a manifest may hold.
const maxAliases = 64

// validateAliases checks the shape of the aliases. Each name is an identifier, and each value has at least one word. Words
// are split on spaces and never quoted, so a value with a quote, an apostrophe or a NUL byte is refused rather than misread.
// What an alias means is checked by the CLI, which owns the command list.
func validateAliases(aliases map[string]string) error {
	if len(aliases) > maxAliases {
		return fmt.Errorf("at most %d aliases", maxAliases)
	}
	for _, name := range sortedKeys(aliases) {
		value := aliases[name]
		switch {
		case !identifier.MatchString(name):
			return fmt.Errorf("alias %s: invalid name", name)
		case len(strings.Fields(value)) == 0:
			return fmt.Errorf("alias %s: a command is required", name)
		case strings.ContainsAny(value, "'\""):
			return fmt.Errorf("alias %s: quotes are not interpreted; separate the words with spaces", name)
		case strings.ContainsRune(value, 0):
			return fmt.Errorf("alias %s: invalid value", name)
		}
	}
	return nil
}

// validLogOptions reports whether every option key and value of a role's logging is one Docker takes: a key of lower-case
// letters, digits, dots, underscores or hyphens, and a value of at most 1024 bytes with no NUL.
func validLogOptions(options map[string]string) bool {
	for k, v := range options {
		if !logOptionKey.MatchString(k) || len(v) > 1024 || strings.ContainsRune(v, 0) {
			return false
		}
	}
	return true
}

// validateStateBackup checks the environment's state backup: its destination, schedule, prefix and retention.
func (m *Manifest) validateStateBackup(sb StateBackup) error {
	if _, ok := m.Destinations[sb.Destination]; !ok {
		return fmt.Errorf("state_backup: unknown destination")
	}
	if sb.Schedule != "" {
		if _, err := schedule.Parse(sb.Schedule); err != nil {
			return fmt.Errorf("state_backup: invalid schedule: %w", err)
		}
	}
	if sb.Keep != nil && (*sb.Keep < 1 || *sb.Keep > 1000) {
		return fmt.Errorf("state_backup: keep must be from 1 to 1000")
	}
	if p := strings.Trim(sb.Prefix, "/"); p != "" {
		for _, seg := range strings.Split(p, "/") {
			if seg == "." || seg == ".." || !stateBackupPrefixSegment.MatchString(seg) {
				return fmt.Errorf("state_backup: invalid prefix")
			}
		}
	}
	return nil
}

// validateJob checks a job's schedule, timeout and target. An image app's job names a role, which may be left out when
// the app has one role. A Compose job names a service, which the manifest cannot check, so only its shape is checked.
func (m *Manifest) validateJob(name string, j Job) error {
	if m.Ingress != nil && j.App == ConnectorAppName {
		return fmt.Errorf("job %s: app %s is reserved for the managed ingress connector", name, ConnectorAppName)
	}
	app, ok := m.Apps[j.App]
	if !ok {
		return fmt.Errorf("job %s: unknown app", name)
	}
	if j.Schedule != "" {
		if _, err := schedule.Parse(j.Schedule); err != nil {
			return fmt.Errorf("job %s: invalid schedule: %w", name, err)
		}
	}
	if !validJobTimeout(j.Timeout) {
		return fmt.Errorf("job %s: invalid timeout", name)
	}
	if app.Kind != "image" {
		if j.Role != "" {
			return fmt.Errorf("job %s: use service for a Compose app", name)
		}
		return nil
	}
	switch {
	case j.Service != "":
		return fmt.Errorf("job %s: use role for an image app", name)
	case j.Role != "":
		if _, known := app.Roles[j.Role]; !known {
			return fmt.Errorf("job %s: role is not a role of the app", name)
		}
	case len(app.Roles) != 1:
		return fmt.Errorf("job %s: role required for an app with several roles", name)
	}
	return nil
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
		Env:    Env{Secret: []string{TunnelTokenSecret}},
		Deploy: Deploy{DeployTimeout: "180s", DrainTimeout: "30s", Retain: &retain, StartAttempts: &attempts},
	}
	return app, version, nil
}

// Link SHA-256 so reference can validate sha256 digests.
var _ = sha256.Size
