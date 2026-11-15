package manifest

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/distribution/reference"
	"go.yaml.in/yaml/v3"
)

const maxInput = 1 << 20

// TunnelTokenSecret is the reserved secret name for the tunnel token Berthy fetches for the connector.
const TunnelTokenSecret = "TUNNEL_TOKEN"

// ConnectorAppName is the app Berthy runs for the managed ingress connector.
const ConnectorAppName = "ingress"

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
	Telemetry    *Telemetry             `json:"telemetry,omitempty" yaml:"telemetry,omitempty"`
	Probes       map[string]Probe       `json:"probes,omitempty" yaml:"probes,omitempty"`
	Peers        map[string]string      `json:"peers,omitempty" yaml:"peers,omitempty"`
	Alerts       *Alerts                `json:"alerts,omitempty" yaml:"alerts,omitempty"`
	Ingress      *Ingress               `json:"ingress,omitempty" yaml:"ingress,omitempty"`

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
}
type Resources struct {
	CPU    string `json:"cpu,omitempty" yaml:"cpu,omitempty"`
	Memory string `json:"memory,omitempty" yaml:"memory,omitempty"`
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
}
type Hooks struct {
	PreDeploy  []Hook `json:"pre_deploy,omitempty" yaml:"pre_deploy,omitempty"`
	PostDeploy []Hook `json:"post_deploy,omitempty" yaml:"post_deploy,omitempty"`
}
type Hook struct {
	Backup string   `json:"backup,omitempty" yaml:"backup,omitempty"`
	Run    []string `json:"run,omitempty" yaml:"run,omitempty"`
}
type Migrations struct {
	Version []string `json:"version" yaml:"version"`
	Apply   []string `json:"apply" yaml:"apply"`
	Revert  []string `json:"revert,omitempty" yaml:"revert,omitempty"`
}
type Job struct {
	App      string   `json:"app" yaml:"app"`
	Role     string   `json:"role,omitempty" yaml:"role,omitempty"`
	Service  string   `json:"service,omitempty" yaml:"service,omitempty"`
	Schedule string   `json:"schedule,omitempty" yaml:"schedule,omitempty"`
	Command  []string `json:"command" yaml:"command"`
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
	Bucket          string `json:"bucket" yaml:"bucket"`
	AccessKeySecret string `json:"access_key_secret" yaml:"access_key_secret"`
	SecretKeySecret string `json:"secret_key_secret" yaml:"secret_key_secret"`
}
type Telemetry struct {
	OTLPEndpoint  string `json:"otlp_endpoint" yaml:"otlp_endpoint"`
	HeadersSecret string `json:"headers_secret,omitempty" yaml:"headers_secret,omitempty"`
}

var identifier = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)
var hostnamePattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)+$`)
var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
var digest = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

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
func Load(dir, env string) (*Manifest, error) {
	if !identifier.MatchString(env) {
		return nil, fmt.Errorf("invalid environment %q", env)
	}
	base, err := readYAML(filepath.Join(dir, "berthy.yml"), false)
	if err != nil {
		return nil, err
	}
	overlay, err := readYAML(filepath.Join(dir, "berthy."+env+".yml"), true)
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
	return DecodeJSON(data)
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
		for _, k := range sortedKeys(a.Env.Plain) {
			if k == TunnelTokenSecret {
				return fmt.Errorf("app %s: %s is reserved for managed ingress", name, TunnelTokenSecret)
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
			if !identifier.MatchString(rn) || r.Service != "" && !identifier.MatchString(r.Service) || r.Replicas != nil && *r.Replicas < 1 || r.Port < 0 || r.Port > 65535 || len(r.Command) > 0 && !argv(r.Command) {
				return fmt.Errorf("app %s role %s: invalid settings", name, rn)
			}
			if h := r.Health; h != nil {
				if !argv(h.Test) || (h.Test[0] != "CMD" && h.Test[0] != "CMD-SHELL") || !duration(h.Interval, false) || !duration(h.Timeout, false) || !duration(h.StartPeriod, true) || !duration(h.StartInterval, false) || h.Retries != nil && *h.Retries < 1 {
					return fmt.Errorf("app %s role %s: invalid health", name, rn)
				}
			}
		}
		for _, h := range append(append([]Hook{}, a.Hooks.PreDeploy...), a.Hooks.PostDeploy...) {
			if (h.Backup == "") == (len(h.Run) == 0) || h.Backup != "" && !identifier.MatchString(h.Backup) || len(h.Run) > 0 && !argv(h.Run) {
				return fmt.Errorf("app %s: invalid hook", name)
			}
		}
		if x := a.Migrations; x != nil {
			if !argv(x.Version) || !argv(x.Apply) || len(x.Revert) > 0 && !argv(x.Revert) {
				return fmt.Errorf("app %s: invalid migrations", name)
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
	for _, name := range sortedKeys(m.Jobs) {
		j := m.Jobs[name]
		if !identifier.MatchString(name) || !identifier.MatchString(j.App) || !argv(j.Command) || j.Role != "" && !identifier.MatchString(j.Role) || j.Service != "" && !identifier.MatchString(j.Service) || j.Role != "" && j.Service != "" {
			return fmt.Errorf("invalid job %q", name)
		}
		if _, ok := m.Apps[j.App]; !ok {
			return fmt.Errorf("job %s: unknown app", name)
		}
	}
	for _, name := range sortedKeys(m.Destinations) {
		d := m.Destinations[name]
		if d.AccessKeySecret == TunnelTokenSecret || d.SecretKeySecret == TunnelTokenSecret {
			return fmt.Errorf("destination %s: %s is reserved for managed ingress", name, TunnelTokenSecret)
		}
		if !identifier.MatchString(name) || d.Type != "s3" || d.Bucket == "" || !envName.MatchString(d.AccessKeySecret) || !envName.MatchString(d.SecretKeySecret) {
			return fmt.Errorf("invalid destination %q", name)
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
	if m.Telemetry != nil && m.Telemetry.HeadersSecret == TunnelTokenSecret {
		return fmt.Errorf("telemetry: %s is reserved for managed ingress", TunnelTokenSecret)
	}
	if m.Telemetry != nil && (m.Telemetry.OTLPEndpoint == "" || m.Telemetry.HeadersSecret != "" && !envName.MatchString(m.Telemetry.HeadersSecret)) {
		return fmt.Errorf("invalid telemetry")
	}
	return m.validateMonitoring()
}
func (m *Manifest) defaults() {
	m.defaultsMonitoring()
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
	for _, h := range append(append([]Hook{}, a.Hooks.PreDeploy...), a.Hooks.PostDeploy...) {
		if h.Backup != "" {
			return "", fmt.Errorf("backup hooks are not supported yet")
		}
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
		Deploy: Deploy{DeployTimeout: "180s", DrainTimeout: "30s", Retain: &retain},
	}
	return app, version, nil
}

// Link SHA-256 so reference can validate sha256 digests.
var _ = sha256.Size
