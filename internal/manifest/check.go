package manifest

import (
	"fmt"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	"berthy/internal/schedule"
	"berthy/internal/scope"
	"berthy/internal/sshhost"

	"github.com/distribution/reference"
)

// Validate returns nil, or the Problems of Check as an error. Its message is the first problem's.
func (m *Manifest) Validate() error {
	if p := m.Check(); len(p) > 0 {
		return p
	}
	return nil
}

// Check returns every problem of the manifest, in a stable order: the order the sections are checked in, and within a section
// by name. A check that needs a value that is itself wrong is not made, so one mistake is reported once.
func (m *Manifest) Check() Problems {
	var p Problems
	m.checkIngress(&p)
	if !scope.ValidProject(m.Project) {
		p.add("project", "invalid project or network")
	}
	if m.Network != "" && !identifier.MatchString(m.Network) {
		p.add("network", "invalid project or network")
	}
	for _, name := range sortedKeys(m.Servers) {
		path := join("servers", name)
		if !identifier.MatchString(name) {
			p.add(path, "invalid server name %q", name)
		}
		srv := m.Servers[name]
		if !sshhost.Valid(srv.SSH) {
			p.add(path+".ssh", "%s.ssh: use USER@SERVER or SERVER", path)
		}
		if srv.Address != "" {
			addr, err := netip.ParseAddr(srv.Address)
			if err != nil || addr.Zone() != "" || addr.Unmap().IsUnspecified() {
				p.add(path+".address", "%s.address: use one IP address of the server, such as 10.0.0.5", path)
			}
		}
	}
	if len(m.Apps) == 0 {
		p.add("apps", "at least one app is required")
	}
	m.checkRegistries(&p)
	m.checkSecrets(&p)
	hostnames := map[string]bool{}
	dependsOK := true
	for _, name := range sortedKeys(m.Apps) {
		dependsOK = m.checkApp(&p, name, m.Apps[name], hostnames) && dependsOK
	}
	if dependsOK {
		m.checkDependsCycle(&p)
	}
	m.checkJobs(&p)
	m.checkDestinations(&p)
	m.checkBackups(&p)
	if sb := m.StateBackup; sb != nil {
		m.checkStateBackup(&p, *sb)
	}
	m.checkTelemetry(&p)
	if m.MinVersion != "" && !minVersionPattern.MatchString(m.MinVersion) {
		p.add("min_version", "invalid min_version %q: use a version such as 0.9.0", m.MinVersion)
	}
	m.checkTimezone(&p)
	checkAliases(&p, m.Aliases)
	m.checkMonitoring(&p)
	return p
}

func (m *Manifest) checkIngress(p *Problems) {
	if m.Ingress == nil {
		return
	}
	c := m.Ingress.Cloudflare
	if c == nil {
		p.add("ingress", "invalid Cloudflare ingress")
		return
	}
	const path = "ingress.cloudflare"
	before := len(*p)
	for _, f := range []struct {
		bad bool
		key string
	}{
		{c.AccountID == "", "account_id"},
		{!envName.MatchString(c.APITokenSecret), "api_token_secret"},
		{c.Image == "", "image"},
		{c.Replicas != nil && *c.Replicas < 1, "replicas"},
		{c.Tunnel != "" && !identifier.MatchString(c.Tunnel), "tunnel"},
	} {
		if f.bad {
			p.add(join(path, f.key), "invalid Cloudflare ingress")
		}
	}
	if len(*p) > before {
		return
	}
	if c.APITokenSecret == TunnelTokenSecret {
		p.add(path+".api_token_secret", "%s is reserved for managed ingress", TunnelTokenSecret)
	}
	ref, err := reference.ParseNormalizedNamed(c.Image)
	if err != nil {
		p.add(path+".image", "invalid Cloudflare image")
		return
	}
	if reference.IsNameOnly(ref) {
		p.add(path+".image", "invalid Cloudflare image: a tag or digest is required")
	}
}

func (m *Manifest) checkRegistries(p *Problems) {
	for _, name := range sortedKeys(m.Registries) {
		r := m.Registries[name]
		path := join("registries", name)
		if r.PasswordSecret == TunnelTokenSecret {
			p.add(path+".password_secret", "registry %s: %s is reserved for managed ingress", name, TunnelTokenSecret)
		}
		switch {
		case name == "" || strings.ContainsAny(name, "/ @"):
			p.add(path, "invalid registry %q", name)
		case r.Username == "":
			p.add(path+".username", "invalid registry %q", name)
		case !envName.MatchString(r.PasswordSecret):
			p.add(path+".password_secret", "invalid registry %q", name)
		}
	}
}

// checkApp checks one app and its roles, hooks and migrations. It returns false when the app's depends_on is wrong, since
// a cycle is only looked for among dependencies that are all apps of the manifest.
func (m *Manifest) checkApp(p *Problems, name string, a App, hostnames map[string]bool) bool {
	path := join("apps", name)
	if !identifier.MatchString(name) {
		p.add(path, "invalid app %q", name)
		return true
	}
	if m.Ingress != nil && name == ConnectorAppName {
		p.add(path, "app name %s is reserved for the managed ingress connector", ConnectorAppName)
	}
	if a.ProjectName != "" && !identifier.MatchString(a.ProjectName) {
		p.add(path+".project_name", "app %s: invalid project name", name)
	}
	known := a.Kind == "image" || a.Kind == "compose"
	if !known {
		p.add(path+".kind", "app %s: invalid kind", name)
	}
	if a.Kind == "image" {
		ref, e := reference.ParseNormalizedNamed(a.Image)
		if e != nil || !reference.IsNameOnly(ref) {
			p.add(path+".image", "app %s: image must be a repository and roles required", name)
		} else if len(a.Roles) == 0 {
			p.add(path+".roles", "app %s: image must be a repository and roles required", name)
		}
	} else if a.Kind == "compose" && !composeFileOK(a.File) {
		p.add(path+".file", "app %s: invalid compose file", name)
	}
	dependsOK := m.checkDependsOn(p, name, a)
	m.checkEnv(p, name, a)
	if !duration(a.Deploy.DeployTimeout, false) {
		p.add(path+".deploy.deploy_timeout", "app %s: invalid deploy settings", name)
	}
	if !duration(a.Deploy.DrainTimeout, false) {
		p.add(path+".deploy.drain_timeout", "app %s: invalid deploy settings", name)
	}
	if a.Deploy.Retain != nil && *a.Deploy.Retain < 1 {
		p.add(path+".deploy.retain", "app %s: invalid deploy settings", name)
	}
	if n := a.Deploy.StartAttempts; n != nil && (*n < 1 || *n > maxStartAttempts) {
		p.add(path+".deploy.start_attempts", "app %s: invalid start_attempts", name)
	}
	if o := a.Deploy.Order; o != "" && o != StartFirst && o != StopFirst {
		p.add(path+".deploy.order", "app %s: invalid deploy order", name)
	}
	for _, rn := range sortedKeys(a.Roles) {
		m.checkRole(p, name, a, rn, a.Roles[rn], known, hostnames)
	}
	m.checkHooks(p, name, a)
	if x := a.Migrations; x != nil {
		checkMigrations(p, name, *x)
	}
	return dependsOK
}

// composeFileOK reports whether file, the file of a Compose app, is a relative path inside the manifest directory. The
// answer is the same on every OS, because the CLI that reads a manifest on Windows and the agent that reads it on Linux must
// agree: a backslash is a separator, and a leading separator (which covers the UNC and \\?\ forms too), a drive letter and
// any .. segment are refused.
func composeFileOK(file string) bool {
	f := strings.ReplaceAll(file, `\`, "/")
	if f == "" || strings.HasPrefix(f, "/") || len(f) >= 2 && f[1] == ':' && (f[0]|0x20 >= 'a' && f[0]|0x20 <= 'z') {
		return false
	}
	for _, segment := range strings.Split(f, "/") {
		if segment == ".." {
			return false
		}
	}
	return true
}

func (m *Manifest) checkEnv(p *Problems, name string, a App) {
	path := join("apps", name) + ".env"
	for _, k := range sortedKeys(a.Env.Plain) {
		kp := path + ".plain." + k
		switch {
		case k == TunnelTokenSecret:
			p.add(kp, "app %s: %s is reserved for managed ingress", name, TunnelTokenSecret)
		case strings.HasPrefix(k, berthyEnvPrefix):
			p.add(kp, "app %s: names starting with BERTHY_ are reserved for Berthy", name)
		case !envName.MatchString(k) || strings.ContainsRune(a.Env.Plain[k], 0):
			p.add(kp, "app %s: invalid environment name or value", name)
		}
	}
	seen := map[string]bool{}
	for i, s := range a.Env.Secret {
		sp := path + ".secret." + strconv.Itoa(i)
		switch {
		case (s.Var == TunnelTokenSecret || s.Secret == TunnelTokenSecret) && !(m.Connector && name == ConnectorAppName):
			p.add(sp, "app %s: %s is reserved for managed ingress", name, TunnelTokenSecret)
		case strings.HasPrefix(s.Var, berthyEnvPrefix):
			p.add(sp, "app %s: names starting with BERTHY_ are reserved for Berthy", name)
		case !envName.MatchString(s.Var) || !envName.MatchString(s.Secret):
			p.add(sp, "app %s: invalid secret", name)
		case seen[s.Var]:
			p.add(sp, "app %s: variable %s is listed twice", name, s.Var)
		default:
			if _, ok := a.Env.Plain[s.Var]; ok {
				p.add(sp, "app %s: duplicate environment name", name)
			}
			seen[s.Var] = true
		}
	}
	for _, k := range sortedKeys(a.Env.Template) {
		kp := path + ".template." + k
		t := a.Env.Template[k]
		refs, err := TemplateRefs(t)
		_, inPlain := a.Env.Plain[k]
		switch {
		case k == TunnelTokenSecret:
			p.add(kp, "app %s: %s is reserved for managed ingress", name, TunnelTokenSecret)
		case strings.HasPrefix(k, berthyEnvPrefix):
			p.add(kp, "app %s: names starting with BERTHY_ are reserved for Berthy", name)
		case !envName.MatchString(k) || strings.ContainsRune(t, 0):
			p.add(kp, "app %s: invalid environment name or value", name)
		case seen[k] || inPlain:
			p.add(kp, "app %s: duplicate environment name", name)
		case err != nil:
			p.add(kp, "app %s: %s", name, err)
		case slices.Contains(refs, TunnelTokenSecret):
			p.add(kp, "app %s: %s is reserved for managed ingress", name, TunnelTokenSecret)
		}
	}
}

// checkRole checks one role of an app. known is whether the app's kind is one of the two, which the hostname and logging rules
// need.
func (m *Manifest) checkRole(p *Problems, name string, a App, rn string, r Role, known bool, hostnames map[string]bool) {
	path := join("apps", name) + ".roles." + rn
	where := fmt.Sprintf("app %s role %s", name, rn)
	if known {
		for i, host := range r.Hostnames {
			if a.Kind != "image" || m.Ingress == nil || r.Port == 0 || len(host) > 253 || !hostnamePattern.MatchString(host) || hostnames[host] {
				p.add(path+".hostnames."+strconv.Itoa(i), "%s: invalid or duplicate hostname", where)
				continue
			}
			hostnames[host] = true
		}
	}
	for i, v := range r.Volumes {
		mount, err := ParseMount(v)
		if err != nil {
			p.add(path+".volumes."+strconv.Itoa(i), "%s: invalid volume", where)
		} else if r.Health != nil && r.Health.Uptime != nil &&
			(mount.Target == uptimeProbeDirectory || strings.HasPrefix(mount.Target, uptimeProbeDirectory+"/")) {
			p.add(path+".volumes."+strconv.Itoa(i), "%s: %s is reserved for uptime health checks", where, uptimeProbeDirectory)
		}
	}
	if r.Resources != nil {
		if r.Resources.CPU != "" {
			if _, err := ParseCPU(r.Resources.CPU); err != nil {
				p.add(path+".resources.cpu", "%s: invalid CPU", where)
			}
		}
		if r.Resources.Memory != "" {
			if _, err := ParseMemory(r.Resources.Memory); err != nil {
				p.add(path+".resources.memory", "%s: invalid memory", where)
			}
		}
	}
	if r.Logging != nil && known {
		switch {
		case a.Kind != "image":
			p.add(path+".logging", "%s: logging is for image apps", where)
		case !logDriverName.MatchString(r.Logging.Driver):
			p.add(path+".logging.driver", "%s: invalid logging", where)
		case !validLogOptions(r.Logging.Options):
			p.add(path+".logging.options", "%s: invalid logging", where)
		}
	}
	for _, f := range []struct {
		bad  bool
		path string
	}{
		{!identifier.MatchString(rn), path},
		{r.Service != "" && !identifier.MatchString(r.Service), path + ".service"},
		{r.Replicas != nil && *r.Replicas < 1, path + ".replicas"},
		{r.Port < 0 || r.Port > 65535, path + ".port"},
		{len(r.Command) > 0 && !argv(r.Command), path + ".command"},
		{len(r.Entrypoint) > 0 && !argv(r.Entrypoint), path + ".entrypoint"},
		{r.User != "" && !userName.MatchString(r.User), path + ".user"},
	} {
		if f.bad {
			p.add(f.path, "%s: invalid settings", where)
		}
	}
	if h := r.Health; h != nil {
		if h.Uptime != nil {
			h.checkUptime(p, path+".health", where, a)
			return
		}
		for _, f := range []struct {
			bad bool
			key string
		}{
			{!argv(h.Test) || (h.Test[0] != "CMD" && h.Test[0] != "CMD-SHELL"), "test"},
			{!duration(h.Interval, false), "interval"},
			{!duration(h.Timeout, false), "timeout"},
			{!duration(h.StartPeriod, true), "start_period"},
			{!duration(h.StartInterval, false), "start_interval"},
			{h.Retries != nil && *h.Retries < 1, "retries"},
		} {
			if f.bad {
				p.add(path+".health."+f.key, "%s: invalid health", where)
			}
		}
	}
}

func (m *Manifest) checkHooks(p *Problems, name string, a App) {
	for _, lh := range []struct {
		list  string
		hooks []Hook
	}{{"pre_deploy", a.Hooks.PreDeploy}, {"post_deploy", a.Hooks.PostDeploy}} {
		for n, h := range lh.hooks {
			hp := join("apps", name) + ".hooks." + lh.list + "." + strconv.Itoa(n)
			before := len(*p)
			switch {
			case (h.Backup == "") == (len(h.Run) == 0):
				p.add(hp, "app %s: invalid hook", name)
			default:
				if h.Backup != "" && !identifier.MatchString(h.Backup) {
					p.add(hp+".backup", "app %s: invalid hook", name)
				}
				if len(h.Run) > 0 && !argv(h.Run) {
					p.add(hp+".run", "app %s: invalid hook", name)
				}
			}
			if len(*p) > before {
				continue
			}
			switch {
			case h.Backup != "" && (h.Timeout != "" || h.Retries != nil):
				key := ".retries"
				if h.Timeout != "" {
					key = ".timeout"
				}
				p.add(hp+key, "app %s: %s hook %d: timeout and retries apply to run hooks only", name, lh.list, n+1)
			case !validCommandTimeout(h.Timeout):
				p.add(hp+".timeout", "app %s: %s hook %d: invalid timeout", name, lh.list, n+1)
			case !validCommandRetries(h.Retries):
				p.add(hp+".retries", "app %s: %s hook %d: invalid retries", name, lh.list, n+1)
			}
			if _, ok := m.Backups[h.Backup]; h.Backup != "" && !ok {
				p.add(hp+".backup", "app %s: %s hook %d: unknown backup", name, lh.list, n+1)
			}
		}
	}
}

func checkMigrations(p *Problems, name string, x Migrations) {
	path := join("apps", name) + ".migrations"
	if !argv(x.Version) {
		p.add(path+".version", "app %s: invalid migrations", name)
	}
	if !argv(x.Apply) {
		p.add(path+".apply", "app %s: invalid migrations", name)
	}
	if len(x.Revert) > 0 && !argv(x.Revert) {
		p.add(path+".revert", "app %s: invalid migrations", name)
	}
	if !validCommandTimeout(x.Timeout) {
		p.add(path+".timeout", "app %s: invalid migrations timeout", name)
	}
	if !validCommandRetries(x.Retries) {
		p.add(path+".retries", "app %s: invalid migrations retries", name)
	}
	n := 0
	for _, s := range x.Revert {
		if strings.Contains(s, "{target}") && s != "{target}" {
			p.add(path+".revert", "app %s: target must be a standalone argument", name)
			return
		}
		n += strings.Count(s, "{target}")
	}
	if len(x.Revert) > 0 && n != 1 {
		p.add(path+".revert", "app %s: revert requires one {target}", name)
	}
}

func (m *Manifest) checkJobs(p *Problems) {
	for _, name := range sortedKeys(m.Jobs) {
		j := m.Jobs[name]
		path := join("jobs", name)
		before := len(*p)
		for _, f := range []struct {
			bad  bool
			path string
		}{
			{!identifier.MatchString(name), path},
			{!identifier.MatchString(j.App), path + ".app"},
			{!argv(j.Command), path + ".command"},
			{j.Role != "" && !identifier.MatchString(j.Role), path + ".role"},
			{j.Service != "" && !identifier.MatchString(j.Service), path + ".service"},
			{j.Role != "" && j.Service != "", path + ".service"},
		} {
			if f.bad {
				p.add(f.path, "invalid job %q", name)
			}
		}
		if len(*p) == before {
			m.checkJob(p, name, j)
		}
	}
}

// checkJob checks a job's schedule, timeout and target. An image app's job names a role, which may be left out when
// the app has one role. A Compose job names a service, which the manifest cannot check, so only its shape is checked.
func (m *Manifest) checkJob(p *Problems, name string, j Job) {
	path := join("jobs", name)
	if m.Ingress != nil && j.App == ConnectorAppName {
		p.add(path+".app", "job %s: app %s is reserved for the managed ingress connector", name, ConnectorAppName)
		return
	}
	app, ok := m.Apps[j.App]
	if !ok {
		p.add(path+".app", "job %s: unknown app", name)
		return
	}
	if j.Schedule != "" {
		if _, err := schedule.Parse(j.Schedule); err != nil {
			p.add(path+".schedule", "job %s: invalid schedule: %v", name, err)
		}
	}
	if !validJobTimeout(j.Timeout) {
		p.add(path+".timeout", "job %s: invalid timeout", name)
	}
	checkCommandTarget(p, path, "job "+name, app, j.Role, j.Service)
}

// checkCommandTarget checks where a job or a command backup runs its command. An image app's command names a role, which may
// be left out when the app has one role. A Compose command names a service, which the manifest cannot check.
func checkCommandTarget(p *Problems, path, what string, app App, role, service string) {
	if app.Kind != "image" {
		if role != "" {
			p.add(path+".role", "%s: use service for a Compose app", what)
		}
		return
	}
	switch {
	case service != "":
		p.add(path+".service", "%s: use role for an image app", what)
	case role != "":
		if _, known := app.Roles[role]; !known {
			p.add(path+".role", "%s: role is not a role of the app", what)
		}
	case len(app.Roles) != 1:
		p.add(path, "%s: role required for an app with several roles", what)
	}
}

// CheckCommandTarget reports why the app of the manifest cannot run a command in the role or service given, as a job's
// target is checked, or "" when it can.
func (m *Manifest) CheckCommandTarget(app, role, service string) string {
	a, ok := m.Apps[app]
	if !ok {
		return "app is not in the manifest"
	}
	if role != "" && service != "" {
		return "name a role or a service, not both"
	}
	var p Problems
	checkCommandTarget(&p, "", "command", a, role, service)
	if len(p) == 0 {
		return ""
	}
	return strings.TrimPrefix(p[0].Message, "command: ")
}

func (m *Manifest) checkDestinations(p *Problems) {
	for _, name := range sortedKeys(m.Destinations) {
		d := m.Destinations[name]
		path := join("destinations", name)
		if d.AccessKeySecret == TunnelTokenSecret {
			p.add(path+".access_key_secret", "destination %s: %s is reserved for managed ingress", name, TunnelTokenSecret)
		}
		if d.SecretKeySecret == TunnelTokenSecret {
			p.add(path+".secret_key_secret", "destination %s: %s is reserved for managed ingress", name, TunnelTokenSecret)
		}
		for _, f := range []struct {
			bad  bool
			path string
		}{
			{!identifier.MatchString(name), path},
			{d.Type != "s3", path + ".type"},
			{!envName.MatchString(d.AccessKeySecret), path + ".access_key_secret"},
			{!envName.MatchString(d.SecretKeySecret), path + ".secret_key_secret"},
		} {
			if f.bad {
				p.add(f.path, "invalid destination %q", name)
			}
		}
		if !bucketName.MatchString(d.Bucket) {
			p.add(path+".bucket", "destination %s: invalid bucket name", name)
		}
		if err := ValidEndpoint(d.Endpoint); err != nil {
			p.add(path+".endpoint", "destination %s: invalid endpoint: %v", name, err)
		}
		if d.Region != "" && !regionName.MatchString(d.Region) {
			p.add(path+".region", "destination %s: invalid region", name)
		}
	}
}

func (m *Manifest) checkBackups(p *Problems) {
	for _, name := range sortedKeys(m.Backups) {
		b := m.Backups[name]
		path := join("backups", name)
		before := len(*p)
		for _, f := range []struct {
			bad  bool
			path string
		}{
			{!identifier.MatchString(name), path},
			{!identifier.MatchString(b.App), path + ".app"},
			{!identifier.MatchString(b.Destination), path + ".destination"},
			{b.Before != "" && !identifier.MatchString(b.Before), path + ".before"},
			{len(b.Command) > 0 && !argv(b.Command), path + ".command"},
			{b.Role != "" && !identifier.MatchString(b.Role), path + ".role"},
			{b.Service != "" && !identifier.MatchString(b.Service), path + ".service"},
			{b.Role != "" && b.Service != "", path + ".service"},
		} {
			if f.bad {
				p.add(f.path, "invalid backup %q", name)
			}
		}
		if len(*p) > before {
			continue
		}
		app, known := m.Apps[b.App]
		if !known {
			p.add(path+".app", "backup %s: unknown app", name)
		}
		m.checkBackupSource(p, path, name, b, app, known)
		if _, ok := m.Destinations[b.Destination]; !ok {
			p.add(path+".destination", "backup %s: unknown destination", name)
		}
		if b.Schedule != "" {
			if _, err := schedule.Parse(b.Schedule); err != nil {
				p.add(path+".schedule", "backup %s: invalid schedule: %v", name, err)
			}
		}
		if b.Before != "" && b.Volume != "" {
			if _, ok := m.Jobs[b.Before]; !ok {
				p.add(path+".before", "backup %s: unknown job", name)
			}
		}
		if b.Retention != nil && b.Retention.Keep != nil && !validRetentionKeep(b.Retention.Keep) {
			p.add(path+".retention.keep", "backup %s: invalid retention", name)
		}
	}
}

// checkBackupSource checks that a backup has exactly one source, a volume or a command, and the settings that belong to it. app
// is the backup's app, and known says whether the manifest has it.
func (m *Manifest) checkBackupSource(p *Problems, path, name string, b Backup, app App, known bool) {
	switch {
	case b.Volume != "" && len(b.Command) > 0:
		p.add(path+".command", "backup %s: name exactly one of volume and command", name)
	case b.Volume == "" && len(b.Command) == 0:
		p.add(path+".volume", "backup %s: name exactly one of volume and command", name)
	case b.Volume != "":
		if b.Timeout != "" {
			p.add(path+".timeout", "backup %s: timeout applies to a command backup", name)
		}
		if b.Role != "" {
			p.add(path+".role", "backup %s: role and service apply to a command backup", name)
		} else if b.Service != "" {
			p.add(path+".service", "backup %s: role and service apply to a command backup", name)
		}
	default:
		if b.Before != "" {
			p.add(path+".before", "backup %s: before applies to a volume backup", name)
		}
		if !validJobTimeout(b.Timeout) {
			p.add(path+".timeout", "backup %s: invalid timeout", name)
		}
		if known {
			checkCommandTarget(p, path, "backup "+name, app, b.Role, b.Service)
		}
	}
}

// validRetentionKeep accepts "all", or a whole number from 1.
func validRetentionKeep(keep any) bool {
	switch v := keep.(type) {
	case string:
		return v == "all"
	case float64:
		return v >= 1 && v == float64(int64(v))
	}
	return false
}

// checkStateBackup checks the environment's state backup: its destination, schedule, prefix and retention.
func (m *Manifest) checkStateBackup(p *Problems, sb StateBackup) {
	const path = "state_backup"
	if _, ok := m.Destinations[sb.Destination]; !ok {
		p.add(path+".destination", "state_backup: unknown destination")
	}
	if sb.Schedule != "" {
		if _, err := schedule.Parse(sb.Schedule); err != nil {
			p.add(path+".schedule", "state_backup: invalid schedule: %v", err)
		}
	}
	if sb.Keep != nil && (*sb.Keep < 1 || *sb.Keep > 1000) {
		p.add(path+".keep", "state_backup: keep must be from 1 to 1000")
	}
	if prefix := strings.Trim(sb.Prefix, "/"); prefix != "" {
		for _, seg := range strings.Split(prefix, "/") {
			if seg == "." || seg == ".." || !stateBackupPrefixSegment.MatchString(seg) {
				p.add(path+".prefix", "state_backup: invalid prefix")
				break
			}
		}
	}
}

func (m *Manifest) checkTelemetry(p *Problems) {
	t := m.Telemetry
	if t == nil {
		return
	}
	if t.HeadersSecret == TunnelTokenSecret {
		p.add("telemetry.headers_secret", "telemetry: %s is reserved for managed ingress", TunnelTokenSecret)
	}
	if t.OTLPEndpoint == "" {
		p.add("telemetry.otlp_endpoint", "invalid telemetry")
	}
	if t.HeadersSecret != "" && !envName.MatchString(t.HeadersSecret) {
		p.add("telemetry.headers_secret", "invalid telemetry")
	}
}

// maxAliases is the most aliases a manifest may hold.
const maxAliases = 64

// checkAliases checks the shape of the aliases. Each name is an identifier, and each value has at least one word. Words
// are split on spaces and never quoted, so a value with a quote, an apostrophe or a NUL byte is refused rather than misread.
// What an alias means is checked by the CLI, which owns the command list.
func checkAliases(p *Problems, aliases map[string]string) {
	if len(aliases) > maxAliases {
		p.add("aliases", "at most %d aliases", maxAliases)
	}
	for _, name := range sortedKeys(aliases) {
		value := aliases[name]
		path := join("aliases", name)
		switch {
		case !identifier.MatchString(name):
			p.add(path, "alias %s: invalid name", name)
		case len(strings.Fields(value)) == 0:
			p.add(path, "alias %s: a command is required", name)
		case strings.ContainsAny(value, "'\""):
			p.add(path, "alias %s: quotes are not interpreted; separate the words with spaces", name)
		case strings.ContainsRune(value, 0):
			p.add(path, "alias %s: invalid value", name)
		}
	}
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
