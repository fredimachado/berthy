package deploy

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"berthy/internal/engine"
	"berthy/internal/manifest"
	"berthy/internal/state"
)

// ComposeRunner runs the Compose CLI for one project. engine.Compose implements it.
type ComposeRunner interface {
	Run(ctx context.Context, r engine.ComposeRun) ([]byte, []byte, error)
}

// composeEngine is the part of the Docker API a Compose plan reads. The Engine of a Controller may implement it.
type composeEngine interface {
	ComposeContainers(context.Context, string) ([]engine.ComposeContainer, error)
	VolumeExists(context.Context, string) (bool, error)
}

// ComposeService is one planned change to a Compose service: create, recreate, keep or remove.
type ComposeService struct {
	Name   string `json:"name"`
	Action string `json:"action"`
}

// ComposeVolume is one planned change to a named volume: create or keep.
type ComposeVolume struct {
	Name   string `json:"name"`
	Action string `json:"action"`
}

// ComposePlan is what a Compose deploy would change. Blockers stop a deploy before anything changes.
type ComposePlan struct {
	Project  string           `json:"project"`
	Services []ComposeService `json:"services"`
	Volumes  []ComposeVolume  `json:"volumes"`
	Blockers []string         `json:"blockers,omitempty"`
}

const (
	composeSourceOrdinal  = 9001
	composeRunOrdinal     = 9002
	composeDefaultTimeout = 120 * time.Second
	composeUpWindow       = 30 * time.Second
	composeRestartLimit   = 2 * time.Minute
)

const (
	errAllowRecreateAdmin   Refusal = "recreating a stateful app requires administrator access"
	errComposeFile          Refusal = "compose file must be non-empty UTF-8 text of at most 512 KiB without NUL"
	errComposeNotCompose    Refusal = "compose file is for compose apps only"
	errComposeUnavailable   Refusal = "Compose unavailable"
	errComposeFiles         Refusal = "compose project files unavailable"
	errComposeConfig        Refusal = "compose configuration invalid"
	errComposePlan          Refusal = "compose plan failed"
	errComposeJournal       Refusal = "compose journal failed"
	errComposeUp            Refusal = "compose up failed"
	errComposeRestart       Refusal = "compose restart failed"
	errComposeFailed        Refusal = "compose deploy failed"
	errComposeNoFiles       Refusal = "compose project has no deployed files"
	errComposeSourceMissing Refusal = "compose source unavailable for rollback"
	errComposeInterrupted   Refusal = "compose deploy interrupted"
	errComposeRegistry      Refusal = "registry credential unavailable"
	errComposeEnv           Refusal = "environment value cannot be written to a Compose environment file"
	errComposeSecret        Refusal = "secret value cannot be written to a Compose environment file"
	errScaleCompose         Refusal = "scale is for image apps"
	errExecServiceImage     Refusal = "use --role for an image app"
	errExecServices         Refusal = "the app has several services: name one with --service"
	errComposeRole          Refusal = "compose apps have no roles"
)

// composeMessage matches the refusals a Compose plan, an adoption or an exec can name a service, port or app in. Only these
// dynamic texts are public besides the fixed Refusal values.
var composeMessage = regexp.MustCompile(`^(service [A-Za-z0-9][A-Za-z0-9._-]{0,127} (publishes port [0-9]{1,5}(-[0-9]{1,5})? on all interfaces; bind it to a private address|builds an image; Compose apps deploy images only|binds a path inside the agent's Compose directory; use a named volume or a path outside it|(runs privileged|uses the host (network|pid|ipc|uts|user|cgroup) namespace|adds capabilities|maps host devices|relaxes security options|reads an env_file from the host|mounts host path [ -~]{1,200}|shares container [ -~]{1,200}|reads a host file through (secret|config) [A-Za-z0-9][A-Za-z0-9._-]{0,127}); deploy it with an admin token)|stateful app [a-z0-9][a-z0-9_-]{0,62} would (recreate|remove) [A-Za-z0-9][A-Za-z0-9._-]{0,127}; deploy with --allow-recreate and an admin token|removing stateful app [a-z0-9][a-z0-9_-]{0,62} needs --allow-recreate|adoption would change (service|volume) [A-Za-z0-9][A-Za-z0-9._-]{0,127}; align the manifest first|no running container for [a-z0-9][a-z0-9_-]{0,62}(/[A-Za-z0-9][A-Za-z0-9._-]{0,127})?)$`)

// composeHash matches one config hash printed by `compose config --hash`.
var composeHash = regexp.MustCompile(`^[A-Za-z0-9:._-]{8,256}$`)

// composeEnvEscaper escapes the characters that keep their meaning inside a double-quoted environment value.
var composeEnvEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, `$`, `\$`)

// composeRequest reports whether req targets a Compose app.
func composeRequest(req Request) bool {
	a, ok := req.Manifest.Apps[req.App]
	return ok && a.Kind == "compose"
}

// checkComposeRequest refuses a Compose file that a Compose app cannot run without, and a Compose file sent for any other app.
func checkComposeRequest(req Request) error {
	if !composeRequest(req) {
		if req.ComposeFile != "" {
			return errComposeNotCompose
		}
		return nil
	}
	if manifest.CheckComposeFile([]byte(req.ComposeFile)) != nil {
		return errComposeFile
	}
	return nil
}

// composeReady reports whether the controller can run Compose commands and read the Docker API they depend on.
func (c *Controller) composeReady() error {
	if c.Compose == nil || c.StateDir == "" || c.Store == nil {
		return errComposeUnavailable
	}
	if _, ok := c.Engine.(composeEngine); !ok {
		return errComposeUnavailable
	}
	return nil
}

// composeRoot is the directory that holds every Compose project's files.
func (c *Controller) composeRoot() string {
	return filepath.Join(c.StateDir, "compose")
}

// composeDir is the working directory of a deployed Compose project. Its files are replaced by each deploy.
func (c *Controller) composeDir(project string) string {
	return filepath.Join(c.composeRoot(), project)
}

// newComposeDir creates a new private directory under the Compose root.
func (c *Controller) newComposeDir(prefix string) (string, error) {
	if err := os.MkdirAll(c.composeRoot(), 0700); err != nil {
		return "", errComposeFiles
	}
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", errComposeFiles
	}
	dir := filepath.Join(c.composeRoot(), prefix+hex.EncodeToString(b[:]))
	if err := os.Mkdir(dir, 0700); err != nil {
		return "", errComposeFiles
	}
	return dir, nil
}

// composeEnvValue quotes v for a Compose environment file. A value with no single quote is single-quoted, which Compose
// reads literally. Any other value is double-quoted with backslash, double quote and dollar escaped. A value with a line
// break or NUL cannot be written and reports false.
func composeEnvValue(v string) (string, bool) {
	if strings.ContainsAny(v, "\x00\r\n") {
		return "", false
	}
	if !strings.Contains(v, "'") {
		return "'" + v + "'", true
	}
	return `"` + composeEnvEscaper.Replace(v) + `"`, true
}

// stageCompose writes compose.yaml and .env (plain and secret values, 0600) into dir. When the manifest lists registries,
// it writes their credentials as DOCKER_CONFIG/config.json in a new directory and returns that directory; the caller
// removes it once the command that needs it has run. It returns an empty string when no registry is listed.
func (c *Controller) stageCompose(ctx context.Context, dir string, req Request) (string, error) {
	app := req.Manifest.Apps[req.App]
	if err := os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte(req.ComposeFile), 0600); err != nil {
		return "", errComposeFiles
	}
	var env strings.Builder
	// The Berthy identity comes first, so a Compose file can pass it on as ${BERTHY_ENVIRONMENT} and the like.
	composeIdentity := map[string]string{"BERTHY_APP": req.App, "BERTHY_ENVIRONMENT": c.Environment, "BERTHY_PROJECT": req.Manifest.Project}
	for _, k := range sortedKeys(composeIdentity) {
		v, ok := composeEnvValue(composeIdentity[k])
		if !ok {
			return "", errComposeEnv
		}
		env.WriteString(k + "=" + v + "\n")
	}
	vars, err := c.resolveEnv(ctx, app.Env)
	if err != nil {
		var r Refusal
		if errors.As(err, &r) {
			return "", r
		}
		return "", errComposeEnv
	}
	for _, e := range vars {
		v, ok := composeEnvValue(e.Value)
		switch {
		case !ok && e.Sensitive:
			return "", errComposeSecret
		case !ok:
			return "", errComposeEnv
		}
		env.WriteString(e.Name + "=" + v + "\n")
	}
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(env.String()), 0600); err != nil {
		return "", errComposeFiles
	}
	if len(req.Manifest.Registries) == 0 {
		return "", nil
	}
	type auth struct {
		Auth string `json:"auth"`
	}
	config := struct {
		Auths map[string]auth `json:"auths"`
	}{Auths: map[string]auth{}}
	for _, host := range sortedKeys(req.Manifest.Registries) {
		reg := req.Manifest.Registries[host]
		info, value, err := c.Store.Secret(ctx, reg.PasswordSecret)
		if err != nil || info.Registry != host {
			return "", errComposeRegistry
		}
		config.Auths[host] = auth{Auth: base64.StdEncoding.EncodeToString([]byte(reg.Username + ":" + string(value)))}
	}
	b, err := json.Marshal(config)
	if err != nil {
		return "", errComposeFiles
	}
	authDir, err := c.newComposeDir(".auth-")
	if err != nil {
		return "", err
	}
	if err = os.WriteFile(filepath.Join(authDir, "config.json"), b, 0600); err != nil {
		os.RemoveAll(authDir)
		return "", errComposeFiles
	}
	return authDir, nil
}

// composeConfigPort is a published or unpublished port in `compose config --format json`.
type composeConfigPort struct {
	HostIP    string `json:"host_ip,omitempty"`
	Published string `json:"published,omitempty"`
}

// composeConfigMount is a volume mount of a service in `compose config --format json`.
type composeConfigMount struct {
	Type   string `json:"type,omitempty"`
	Source string `json:"source,omitempty"`
}

// composeConfigService is the part of a service the plan reads. Each field is one setting that a deploy must not use
// unless it comes from an administrator, or is a fixed rule of its own.
type composeConfigService struct {
	Build             json.RawMessage      `json:"build,omitempty"`
	Ports             []composeConfigPort  `json:"ports,omitempty"`
	Volumes           []composeConfigMount `json:"volumes,omitempty"`
	VolumesFrom       []string             `json:"volumes_from,omitempty"`
	Privileged        bool                 `json:"privileged,omitempty"`
	NetworkMode       string               `json:"network_mode,omitempty"`
	Pid               string               `json:"pid,omitempty"`
	Ipc               string               `json:"ipc,omitempty"`
	Uts               string               `json:"uts,omitempty"`
	UsernsMode        string               `json:"userns_mode,omitempty"`
	Cgroup            string               `json:"cgroup,omitempty"`
	CapAdd            []string             `json:"cap_add,omitempty"`
	Devices           json.RawMessage      `json:"devices,omitempty"`
	DeviceCgroupRules []string             `json:"device_cgroup_rules,omitempty"`
	SecurityOpt       []string             `json:"security_opt,omitempty"`
	EnvFile           json.RawMessage      `json:"env_file,omitempty"`
	Secrets           []composeServiceRef  `json:"secrets,omitempty"`
	Configs           []composeServiceRef  `json:"configs,omitempty"`
	Logging           *composeLogging      `json:"logging,omitempty"`
}

// composeLogging is a service's log driver and options in `compose config --format json`.
type composeLogging struct {
	Driver  string            `json:"driver,omitempty"`
	Options map[string]string `json:"options,omitempty"`
}

// composeLogUnlimited reports whether a Compose service's logs grow without a size limit. daemonDriver is the daemon's
// default log driver; daemonMaxSize is log-opts.max-size from the daemon's configuration, "" when none. Only json-file
// can be unlimited: local rotates by default, and any other driver is the operator's choice, which Berthy does not judge.
// Docker merges the daemon's log-opts into a container's log config, key by key, when the container's driver is the
// daemon's default driver. So the service's max-size, or else the daemon's when the drivers match, decides, and the
// service is unlimited only when that is empty. Other log-opts do not matter here.
func composeLogUnlimited(l *composeLogging, daemonDriver, daemonMaxSize string) bool {
	driver, options := daemonDriver, map[string]string(nil)
	if l != nil {
		if l.Driver != "" {
			driver = l.Driver
		}
		options = l.Options
	}
	if driver != "json-file" {
		return false
	}
	maxSize := options["max-size"]
	if maxSize == "" && driver == daemonDriver {
		maxSize = daemonMaxSize
	}
	return maxSize == ""
}

// composeServiceRef is a secret or config a service uses, by its top-level name.
type composeServiceRef struct {
	Source string `json:"source"`
}

// composeTopFile is a top-level secret or config. A file is a path on the host.
type composeTopFile struct {
	File string `json:"file,omitempty"`
}

// composeConfigVolume is a named volume in `compose config --format json`.
type composeConfigVolume struct {
	Name     string          `json:"name"`
	External json.RawMessage `json:"external,omitempty"`
}

// composeConfigFile is the part of `compose config --format json` the plan reads.
type composeConfigFile struct {
	Services map[string]composeConfigService `json:"services"`
	Volumes  map[string]composeConfigVolume  `json:"volumes,omitempty"`
	Secrets  map[string]composeTopFile       `json:"secrets,omitempty"`
	Configs  map[string]composeTopFile       `json:"configs,omitempty"`
}

func (v composeConfigVolume) external() bool {
	s := string(v.External)
	return s != "" && s != "null" && s != "false"
}

// ComposeServicesWithoutLogLimit returns the sorted names of the services of a deployed Compose project whose logs grow
// without a size limit, by composeLogUnlimited. It reads the project's stored files and changes nothing.
func (c *Controller) ComposeServicesWithoutLogLimit(ctx context.Context, project, daemonDriver, daemonMaxSize string) ([]string, error) {
	if c.Compose == nil || c.StateDir == "" {
		return nil, errComposeUnavailable
	}
	out, _, err := c.Compose.Run(ctx, engine.ComposeRun{Project: project, Dir: c.composeDir(project), Args: []string{"config", "--format", "json", "--no-env-resolution"}})
	if err != nil {
		return nil, errComposeConfig
	}
	var cfg composeConfigFile
	if json.Unmarshal(out, &cfg) != nil {
		return nil, errComposeConfig
	}
	var names []string
	for name, svc := range cfg.Services {
		if composeLogUnlimited(svc.Logging, daemonDriver, daemonMaxSize) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names, nil
}

// composeHashes reads the `<service> <hash>` lines of `compose config --hash *`.
func composeHashes(out []byte) (map[string]string, error) {
	hashes := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if len(fields) != 2 || !composeHash.MatchString(fields[1]) {
			return nil, errComposeConfig
		}
		hashes[fields[0]] = fields[1]
	}
	return hashes, nil
}

// composeInputs is everything buildComposePlan needs, so the plan can be tested without Docker or the Compose CLI.
type composeInputs struct {
	Project, App  string
	Root          string // the Compose root; a bind mount inside it would be replaced by the next deploy
	Stateful      bool
	AllowRecreate bool // the request carries --allow-recreate and an administrator token
	Adopt         bool
	Admin         bool // the request carries an administrator token; settings that reach the host need one
	Config        composeConfigFile
	Hashes        map[string]string
	Containers    []engine.ComposeContainer
	VolumeExists  map[string]bool // by resolved volume name, for every non-external volume
}

func portBlocker(service, port string) string {
	return fmt.Sprintf("service %s publishes port %s on all interfaces; bind it to a private address", service, port)
}

func buildBlocker(service string) string {
	return fmt.Sprintf("service %s builds an image; Compose apps deploy images only", service)
}

func bindBlocker(service string) string {
	return fmt.Sprintf("service %s binds a path inside the agent's Compose directory; use a named volume or a path outside it", service)
}

func statefulBlocker(app, service, action string) string {
	verb := "recreate"
	if action == "remove" {
		verb = "remove"
	}
	return fmt.Sprintf("stateful app %s would %s %s; deploy with --allow-recreate and an admin token", app, verb, service)
}

// buildComposePlan compares the configured services and volumes with what exists and lists the blockers. It makes no
// calls, so it is the same whether the plan is a dry run or a deploy.
func buildComposePlan(in composeInputs) ComposePlan {
	plan := ComposePlan{Project: in.Project, Services: []ComposeService{}, Volumes: []ComposeVolume{}}
	byService := map[string][]engine.ComposeContainer{}
	orphans := map[string]bool{}
	for _, ctr := range in.Containers {
		if ctr.OneOff || ctr.Service == "" {
			continue
		}
		if _, ok := in.Config.Services[ctr.Service]; ok {
			byService[ctr.Service] = append(byService[ctr.Service], ctr)
		} else {
			orphans[ctr.Service] = true
		}
	}
	var blockers []string
	root := filepath.Clean(in.Root) + string(filepath.Separator)
	for _, name := range sortedKeys(in.Config.Services) {
		svc := in.Config.Services[name]
		if len(svc.Build) > 0 && string(svc.Build) != "null" {
			blockers = append(blockers, buildBlocker(name))
		}
		for _, p := range svc.Ports {
			if p.Published != "" && (p.HostIP == "" || p.HostIP == "0.0.0.0" || p.HostIP == "::") {
				blockers = append(blockers, portBlocker(name, p.Published))
				break
			}
		}
		for _, m := range svc.Volumes {
			// A bind mount inside the Compose root is replaced by the next deploy. This applies to administrators too.
			if m.Type == "bind" && (!strings.HasPrefix(m.Source, "/") && !filepath.IsAbs(m.Source) || strings.HasPrefix(filepath.Clean(m.Source), root)) {
				blockers = append(blockers, bindBlocker(name))
				break
			}
		}
		action := "create"
		if containers := byService[name]; len(containers) > 0 {
			action = "keep"
			for _, ctr := range containers {
				if ctr.ConfigHash != in.Hashes[name] {
					action = "recreate"
					break
				}
			}
		}
		plan.Services = append(plan.Services, ComposeService{Name: name, Action: action})
	}
	// --remove-orphans removes the containers of services the file no longer defines.
	for _, name := range sortedKeys(orphans) {
		plan.Services = append(plan.Services, ComposeService{Name: name, Action: "remove"})
	}
	for _, key := range sortedKeys(in.Config.Volumes) {
		v := in.Config.Volumes[key]
		if v.external() {
			continue
		}
		action := "keep"
		if !in.VolumeExists[v.Name] {
			action = "create"
		}
		plan.Volumes = append(plan.Volumes, ComposeVolume{Name: v.Name, Action: action})
	}
	// Settings that reach the host need an administrator's token, for a deploy and for an adoption alike.
	if !in.Admin {
		for _, name := range sortedKeys(in.Config.Services) {
			blockers = append(blockers, hostAccessBlockers(name, in.Config.Services[name], in.Config)...)
		}
	}
	if in.Stateful && !in.Adopt && !in.AllowRecreate {
		for _, s := range plan.Services {
			if s.Action == "recreate" || s.Action == "remove" {
				blockers = append(blockers, statefulBlocker(in.App, s.Name, s.Action))
			}
		}
	}
	plan.Blockers = blockers
	return plan
}

// hostAccessSuffix ends the refusal for a setting that reaches the host. Only an administrator's token may deploy it.
const hostAccessSuffix = "; deploy it with an admin token"

// hostAccessBlockers lists what one service reaches on the host: privileged mode, host namespaces, added capabilities,
// host devices, relaxed security options, host bind mounts, env_file, and secrets or configs read from host files.
func hostAccessBlockers(name string, svc composeConfigService, cfg composeConfigFile) []string {
	var out []string
	if svc.Privileged {
		out = append(out, fmt.Sprintf("service %s runs privileged%s", name, hostAccessSuffix))
	}
	namespaces := []struct{ label, mode string }{
		{"network", svc.NetworkMode}, {"pid", svc.Pid}, {"ipc", svc.Ipc}, {"uts", svc.Uts}, {"user", svc.UsernsMode}, {"cgroup", svc.Cgroup},
	}
	for _, ns := range namespaces {
		if ns.mode == "host" {
			out = append(out, fmt.Sprintf("service %s uses the host %s namespace%s", name, ns.label, hostAccessSuffix))
		}
	}
	// A namespace or volume shared with a container outside the project shares that container's resources. Only a
	// service of the project is allowed: a bare name in volumes_from, or service: for a namespace.
	for _, mode := range []string{svc.NetworkMode, svc.Pid, svc.Ipc, svc.UsernsMode} {
		if other, ok := sharedContainer(mode); ok {
			out = append(out, sharesContainerBlocker(name, other))
		}
	}
	for _, ref := range svc.VolumesFrom {
		if other, ok := sharedContainer(ref); ok {
			out = append(out, sharesContainerBlocker(name, other))
		}
	}
	if len(svc.CapAdd) > 0 {
		out = append(out, fmt.Sprintf("service %s adds capabilities%s", name, hostAccessSuffix))
	}
	if nonEmptyJSON(svc.Devices) || len(svc.DeviceCgroupRules) > 0 {
		out = append(out, fmt.Sprintf("service %s maps host devices%s", name, hostAccessSuffix))
	}
	for _, opt := range svc.SecurityOpt {
		if relaxesSecurity(opt) {
			out = append(out, fmt.Sprintf("service %s relaxes security options%s", name, hostAccessSuffix))
			break
		}
	}
	for _, m := range svc.Volumes {
		if m.Type == "bind" {
			out = append(out, fmt.Sprintf("service %s mounts host path %s%s", name, safePath(m.Source), hostAccessSuffix))
		}
	}
	if nonEmptyJSON(svc.EnvFile) {
		out = append(out, fmt.Sprintf("service %s reads an env_file from the host%s", name, hostAccessSuffix))
	}
	for _, ref := range svc.Secrets {
		if cfg.Secrets[ref.Source].File != "" {
			out = append(out, fmt.Sprintf("service %s reads a host file through secret %s%s", name, ref.Source, hostAccessSuffix))
		}
	}
	for _, ref := range svc.Configs {
		if cfg.Configs[ref.Source].File != "" {
			out = append(out, fmt.Sprintf("service %s reads a host file through config %s%s", name, ref.Source, hostAccessSuffix))
		}
	}
	return out
}

// relaxesSecurity reports whether a security_opt entry switches a protection off.
func relaxesSecurity(opt string) bool {
	return strings.Contains(opt, "unconfined") || strings.Contains(opt, "label:disable") || strings.Contains(opt, "label=disable")
}

// safePath returns a host path that a refusal may show: short printable ASCII. Any other path is not shown.
func safePath(p string) string {
	if p == "" || len(p) > 200 {
		return "(path not shown)"
	}
	for i := 0; i < len(p); i++ {
		if p[i] < 0x20 || p[i] > 0x7e {
			return "(path not shown)"
		}
	}
	return p
}

// sharedContainer returns the container a reference names. A reference is "container:NAME", optionally followed by a
// ":mode" suffix as volumes_from allows. A service of the project is not a container, so it reports false.
func sharedContainer(ref string) (string, bool) {
	name, ok := strings.CutPrefix(ref, "container:")
	if !ok {
		return "", false
	}
	name, _, _ = strings.Cut(name, ":")
	return name, true
}

// sharesContainerBlocker refuses a service that shares a container outside the project. The name is shown only when
// it is safe to show.
func sharesContainerBlocker(service, container string) string {
	return fmt.Sprintf("service %s shares container %s%s", service, safePath(container), hostAccessSuffix)
}

// nonEmptyJSON reports whether a list or object in a Compose config holds anything.
func nonEmptyJSON(raw json.RawMessage) bool {
	t := strings.TrimSpace(string(raw))
	return t != "" && t != "null" && t != "[]" && t != "{}"
}

// firstDifference names the first service or volume an adoption would change, or returns "" when it would change none.
func firstDifference(p ComposePlan) string {
	for _, s := range p.Services {
		if s.Action != "keep" {
			return "service " + s.Name
		}
	}
	for _, v := range p.Volumes {
		if v.Action != "keep" {
			return "volume " + v.Name
		}
	}
	return ""
}

// composePlanIn plans req inside dir, which holds the staged files. It makes no changes.
func (c *Controller) composePlanIn(ctx context.Context, dir string, actor Actor, req Request, adopt bool) (ComposePlan, error) {
	project := req.Manifest.ComposeProject(req.App)
	app := req.Manifest.Apps[req.App]
	// --no-env-resolution keeps env_file in the output; config --hash still resolves env files, so a missing one fails here.
	out, _, err := c.Compose.Run(ctx, engine.ComposeRun{Project: project, Dir: dir, Args: []string{"config", "--format", "json", "--no-env-resolution"}})
	if err != nil {
		return ComposePlan{}, errComposeConfig
	}
	var cfg composeConfigFile
	if json.Unmarshal(out, &cfg) != nil || len(cfg.Services) == 0 {
		return ComposePlan{}, errComposeConfig
	}
	hashOut, _, err := c.Compose.Run(ctx, engine.ComposeRun{Project: project, Dir: dir, Args: []string{"config", "--hash", "*"}})
	if err != nil {
		return ComposePlan{}, errComposeConfig
	}
	hashes, err := composeHashes(hashOut)
	if err != nil {
		return ComposePlan{}, err
	}
	for name := range cfg.Services {
		if hashes[name] == "" {
			return ComposePlan{}, errComposeConfig
		}
	}
	ce, ok := c.Engine.(composeEngine)
	if !ok {
		return ComposePlan{}, errComposeUnavailable
	}
	containers, err := ce.ComposeContainers(ctx, project)
	if err != nil {
		return ComposePlan{}, errComposePlan
	}
	exists := map[string]bool{}
	for _, key := range sortedKeys(cfg.Volumes) {
		v := cfg.Volumes[key]
		if v.external() {
			continue
		}
		if v.Name == "" {
			return ComposePlan{}, errComposeConfig
		}
		found, err := ce.VolumeExists(ctx, v.Name)
		if err != nil {
			return ComposePlan{}, errComposePlan
		}
		exists[v.Name] = found
	}
	return buildComposePlan(composeInputs{
		Project:       project,
		App:           req.App,
		Root:          c.composeRoot(),
		Stateful:      app.Stateful,
		AllowRecreate: req.AllowRecreate && actor.Admin,
		Admin:         actor.Admin,
		Adopt:         adopt,
		Config:        cfg,
		Hashes:        hashes,
		Containers:    containers,
		VolumeExists:  exists,
	}), nil
}

// planCompose plans a Compose deploy or adoption without changing anything. Blockers are part of the plan.
func (c *Controller) planCompose(ctx context.Context, actor Actor, req Request) (Plan, error) {
	if err := req.Manifest.Validate(); err != nil {
		return Plan{}, err
	}
	if err := checkComposeRequest(req); err != nil {
		return Plan{}, err
	}
	if err := c.composeReady(); err != nil {
		return Plan{}, err
	}
	dir, err := c.newComposeDir(".plan-")
	if err != nil {
		return Plan{}, err
	}
	defer os.RemoveAll(dir)
	auth, err := c.stageCompose(ctx, dir, req)
	if err != nil {
		return Plan{}, err
	}
	if auth != "" {
		defer os.RemoveAll(auth)
	}
	plan, err := c.composePlanIn(ctx, dir, actor, req, req.Adopt)
	if err != nil {
		return Plan{}, err
	}
	return Plan{App: req.App, Roles: []string{}, Compose: &plan}, nil
}

// planComposeOperation plans a restart or scale of a Compose app. Only restart is supported. An exec is planned by execTarget.
func (c *Controller) planComposeOperation(ctx context.Context, req Request, m manifest.Manifest) (Plan, error) {
	if req.Kind == "scale" {
		return Plan{}, errScaleCompose
	}
	if req.Role != "" {
		return Plan{}, errComposeRole
	}
	if err := c.composeReady(); err != nil {
		return Plan{}, err
	}
	if _, err := os.Stat(c.composeDir(m.ComposeProject(req.App))); err != nil {
		return Plan{}, errComposeNoFiles
	}
	return Plan{App: req.App, Roles: []string{}}, nil
}

// runComposeRestart restarts the containers of a deployed Compose app. It recreates nothing.
func (c *Controller) runComposeRestart(ctx context.Context, m manifest.Manifest, req Request) state.Outcome {
	if _, err := c.planComposeOperation(ctx, req, m); err != nil {
		return failedWith(err)
	}
	project := m.ComposeProject(req.App)
	rctx, cancel := context.WithTimeout(ctx, composeRestartLimit)
	defer cancel()
	if _, _, err := c.Compose.Run(rctx, engine.ComposeRun{Project: project, Dir: c.composeDir(project), Args: []string{"restart"}}); err != nil {
		return failedWith(errComposeRestart)
	}
	return state.Outcome{State: "succeeded"}
}

// failedWith records err as the failure text. Refusals keep their text; anything else gets a generic message.
func failedWith(err error) state.Outcome {
	var r Refusal
	if errors.As(err, &r) {
		return state.Outcome{State: "failed", Err: string(r)}
	}
	return state.Outcome{State: "failed", Err: string(errComposeFailed)}
}

// reasonEvent is the public event data for one blocker.
func reasonEvent(text string) json.RawMessage {
	b, _ := json.Marshal(map[string]string{"reason": text})
	return b
}

// composeDigest is the release digest of a Compose deploy: the SHA-256 of the file it ran.
func composeDigest(file string) string {
	sum := sha256.Sum256([]byte(file))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// runCompose deploys a Compose app, or with adoption records it when nothing would change. Nothing changes when the
// plan has a blocker.
func (c *Controller) runCompose(ctx context.Context, op state.Operation, actor Actor, req Request) state.Outcome {
	if err := req.Manifest.Validate(); err != nil {
		return failedWith(errComposeConfig)
	}
	if err := checkComposeRequest(req); err != nil {
		return failedWith(err)
	}
	if err := c.composeReady(); err != nil {
		return failedWith(err)
	}
	app := req.Manifest.Apps[req.App]
	project := req.Manifest.ComposeProject(req.App)
	budget := composeDefaultTimeout
	if app.Deploy.DeployTimeout != "" {
		d, err := time.ParseDuration(app.Deploy.DeployTimeout)
		if err != nil || d < time.Second {
			return failedWith(errComposeConfig)
		}
		budget = d
	}
	stage, err := c.newComposeDir(".plan-")
	if err != nil {
		return failedWith(err)
	}
	defer os.RemoveAll(stage)
	auth, err := c.stageCompose(ctx, stage, req)
	if err != nil {
		return failedWith(err)
	}
	if auth != "" {
		defer os.RemoveAll(auth)
	}
	plan, err := c.composePlanIn(ctx, stage, actor, req, req.Adopt)
	if err != nil {
		return failedWith(err)
	}
	for _, b := range plan.Blockers {
		_ = c.Store.AppendEvent(ctx, op.ID, state.Event{Kind: "blocked", Data: reasonEvent(b)})
	}
	if len(plan.Blockers) > 0 {
		return failedWith(Refusal(plan.Blockers[0]))
	}
	if req.Adopt {
		if first := firstDifference(plan); first != "" {
			return failedWith(Refusal("adoption would change " + first + "; align the manifest first"))
		}
	}
	source, err := json.Marshal(struct {
		ComposeFile string `json:"compose_file"`
		Project     string `json:"project"`
	}{req.ComposeFile, project})
	if err != nil {
		return failedWith(errComposeJournal)
	}
	// The source is journaled before the project directory changes, so a rollback can redeploy this file.
	if err = c.Store.RecordIntent(ctx, op.ID, state.Intent{Ordinal: composeSourceOrdinal, Kind: "compose_source", Name: project, Desired: source}); err != nil {
		return failedWith(errComposeJournal)
	}
	dir, err := c.replaceProjectDir(stage, project)
	if err != nil {
		return failedWith(err)
	}
	manifestJSON, err := json.Marshal(req.Manifest)
	if err != nil {
		return failedWith(errComposeJournal)
	}
	release := &state.Release{App: req.App, Digest: composeDigest(req.ComposeFile), Commit: req.Commit, Modified: req.Modified, Manifest: manifestJSON}
	if req.Adopt {
		return state.Outcome{State: "succeeded", Protected: req.Manifest.Protected, Release: release}
	}
	// The run is journaled before up starts, so recovery knows a run may have changed the project.
	if err = c.Store.RecordIntent(ctx, op.ID, state.Intent{Ordinal: composeRunOrdinal, Kind: "compose", Name: project, Desired: source}); err != nil {
		return failedWith(errComposeJournal)
	}
	if err = c.Store.MarkStep(ctx, op.ID, composeRunOrdinal, "issued", json.RawMessage(`{}`)); err != nil {
		return failedWith(errComposeJournal)
	}
	upCtx, cancel := context.WithTimeout(ctx, budget+composeUpWindow)
	defer cancel()
	_, _, err = c.Compose.Run(upCtx, engine.ComposeRun{
		Project:      project,
		Dir:          dir,
		Args:         []string{"up", "-d", "--remove-orphans", "--wait", "--wait-timeout", strconv.FormatInt(int64(budget/time.Second), 10)},
		DockerConfig: auth,
	})
	if err != nil {
		_ = c.Store.MarkStep(ctx, op.ID, composeRunOrdinal, "finished", json.RawMessage(`{"ok":false}`))
		return failedWith(errComposeUp)
	}
	if err = c.Store.MarkStep(ctx, op.ID, composeRunOrdinal, "finished", json.RawMessage(`{"ok":true}`)); err != nil {
		return failedWith(errComposeJournal)
	}
	return state.Outcome{State: "succeeded", Protected: req.Manifest.Protected, Release: release}
}

// replaceProjectDir moves the staged files into the project's directory. The new contents are written beside it first.
func (c *Controller) replaceProjectDir(stage, project string) (string, error) {
	next := filepath.Join(c.composeRoot(), project+".next")
	dir := c.composeDir(project)
	if err := os.RemoveAll(next); err != nil {
		return "", errComposeFiles
	}
	if err := os.Rename(stage, next); err != nil {
		return "", errComposeFiles
	}
	if err := os.RemoveAll(dir); err != nil {
		return "", errComposeFiles
	}
	if err := os.Rename(next, dir); err != nil {
		return "", errComposeFiles
	}
	return dir, nil
}

// composeSource returns the Compose file a deploy journaled, or refuses when the operation kept none.
func (c *Controller) composeSource(ctx context.Context, opID string) (string, error) {
	if opID == "" {
		return "", errComposeSourceMissing
	}
	intents, err := c.Store.Intents(ctx, opID)
	if err != nil {
		return "", errComposeSourceMissing
	}
	for _, i := range intents {
		if i.Kind != "compose_source" {
			continue
		}
		var source struct {
			ComposeFile string `json:"compose_file"`
		}
		if json.Unmarshal(i.Desired, &source) != nil || source.ComposeFile == "" {
			return "", errComposeSourceMissing
		}
		return source.ComposeFile, nil
	}
	return "", errComposeSourceMissing
}

// appliedCompose returns the applied manifest of app and whether app is a Compose app. An app with no applied release is
// not one.
func (c *Controller) appliedCompose(ctx context.Context, app string) (manifest.Manifest, bool, error) {
	if c.Store == nil || app == "" {
		return manifest.Manifest{}, false, nil
	}
	r, err := c.Store.Applied(ctx, app)
	if errors.Is(err, sql.ErrNoRows) {
		return manifest.Manifest{}, false, nil
	}
	if err != nil {
		return manifest.Manifest{}, false, errors.New("applied release unavailable")
	}
	var m manifest.Manifest
	if json.Unmarshal(r.Manifest, &m) != nil {
		return manifest.Manifest{}, false, errors.New("applied manifest unavailable")
	}
	a, ok := m.Apps[app]
	return m, ok && a.Kind == "compose", nil
}
