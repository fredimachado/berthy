package manifest

import (
	"slices"
	"strings"
)

// Key documents one manifest key. Keys is the only place a key's meaning is written down: the user guide's reference,
// `berthy docs` and the JSON Schema are generated from it, and a test keeps it in step with the Manifest type and with
// Validate. To add a manifest key, add its field to the type and its Key here, then run: go generate ./internal/manifest
type Key struct {
	Path        string   // dotted, with a placeholder for each map key: "apps.<name>.roles.<role>.logging.driver"; "[]" marks a list of objects
	Section     string   // the reference section it is listed under: the Name of one of Sections
	Type        string   // as a person reads it: "name", "string", "boolean", "integer", "duration", "list", "map", ...
	Default     string   // as shown in the reference; "" when there is none. A literal (false, 120s) is also the schema's default
	Required    bool     // the manifest is refused without it; for a key with Applies, without it in an app of that kind
	Applies     string   // for an app's keys: "" for both kinds of app, "image" or "compose"
	Enum        []string // the allowed values, when the key has a fixed set; the reference says them, so Description does not
	NamePattern string   // for a map: the regular expression each of its names must match
	Description string   // Markdown: code in backticks, links as [text](#anchor). Without a pipe, so it fits a table cell
}

// Section is one table of the reference.
type Section struct {
	Name   string // what `berthy docs <name>` takes
	Title  string
	Prefix string // the part of a key's path that the table leaves out, so each key is shown relative to it
	Code   bool   // Title is a manifest path, which Markdown shows as code
	Note   string // a line under the title
}

// Sections are the tables of the reference, in order.
var Sections = []Section{
	{Name: "top", Title: "Top level"},
	{Name: "registries", Title: "registries.<host>", Prefix: "registries.<host>.", Code: true},
	{Name: "secrets", Title: "secrets.<SECRET>", Prefix: "secrets.<SECRET>.", Code: true},
	{Name: "apps", Title: "apps.<name>", Prefix: "apps.<name>.", Code: true},
	{Name: "roles", Title: "apps.<name>.roles.<role>", Prefix: "apps.<name>.roles.<role>.", Code: true, Note: "Roles belong to image apps."},
	{Name: "hooks", Title: "Hooks and migrations", Prefix: "apps.<name>.", Note: "Hooks and migrations belong to image apps. A step is either `backup` or `run`, never both."},
	{Name: "jobs", Title: "jobs.<name>", Prefix: "jobs.<name>.", Code: true},
	{Name: "backups", Title: "backups.<name>", Prefix: "backups.<name>.", Code: true},
	{Name: "destinations", Title: "destinations.<name>", Prefix: "destinations.<name>.", Code: true},
	{Name: "state_backup", Title: "state_backup", Prefix: "state_backup.", Code: true},
	{Name: "probes", Title: "probes.<name>", Prefix: "probes.<name>.", Code: true, Note: "A probe has exactly one of `http`, `tcp` and `service`."},
	{Name: "peers", Title: "peers", Prefix: "peers.", Code: true},
	{Name: "alerts", Title: "alerts", Prefix: "alerts.", Code: true},
	{Name: "telemetry", Title: "telemetry", Prefix: "telemetry.", Code: true},
	{Name: "ingress", Title: "ingress", Prefix: "ingress.", Code: true},
}

// hookKeys are the keys of a list of hook steps, which pre_deploy and post_deploy share.
func hookKeys(list string) []Key {
	p := "apps.<name>.hooks." + list + "[]."
	return []Key{
		{Path: p + "backup", Section: "hooks", Type: "name", Description: "The step runs the backup of this name and waits for it. The name must be a key of `backups`. A `backup` step takes no other setting."},
		{Path: p + "run", Section: "hooks", Type: "list", Description: "The step runs this command, as a list, in a container of the new image with the app's environment."},
		{Path: p + "timeout", Section: "hooks", Type: "duration", Default: defaultCommandTimeout, Description: "A `run` step: the time limit of each attempt, from `1s` to `24h`. A command that runs past it is never retried, because it may still be running."},
		{Path: p + "retries", Section: "hooks", Type: "integer", Default: "0", Description: "A `run` step: how many more times the command runs after it exits non-zero, from 0 to 5. Set it only for a command that is safe to run again."},
	}
}

// Keys is every key of the manifest, in reference order.
//
//go:generate go run ./gendocs
var Keys = slices.Concat(
	[]Key{
		{Path: "project", Section: "top", Type: "name", Required: true, Description: "The project's name, which prefixes service and Compose project names. Lowercase letters, digits, `_` and `-`, at most 63 characters, starting with a letter or digit."},
		{Path: "network", Section: "top", Type: "name", Default: "<project>-<environment>", Description: "Override the environment network to adopt an existing overlay network. `berthy setup` creates it when it does not exist."},
		{Path: "servers", Section: "top", Type: "map", NamePattern: identifier.String(), Description: "Servers by name. Define them in the environment overlay when environments use different servers."},
		{Path: "servers.<name>.ssh", Section: "top", Type: "string", Required: true, Description: "The SSH destination `berthy setup` connects to, such as `ops@10.0.0.5` or an alias from your SSH config. Setup uses it when the manifest names only this server, or with `--server <name>`."},
		{Path: "servers.<name>.address", Section: "top", Type: "string", Description: "The server's private or VPN IP address. When setup picks this server from the manifest (no `--host`, no `--local`), a first install listens on `<address>:7710` unless `--listen` says otherwise; `--host` and `--local` still need `--listen`. A later run refuses when the agent listens on another address."},
		{Path: "protected", Section: "top", Type: "boolean", Default: "false", Description: "Protect the environment: digest-only deploys, and changes that an AI agent's token asks for are held for approval."},
		{Path: "registries", Section: "top", Type: "map", Description: "Registry credentials, by registry host."},
		{Path: "secrets", Section: "top", Type: "map", NamePattern: envName.String(), Description: "Secrets Berthy may generate, by name. Each must be read by an app."},
		{Path: "apps", Section: "top", Type: "map", Required: true, NamePattern: identifier.String(), Description: "The apps, by name. At least one."},
		{Path: "jobs", Section: "top", Type: "map", NamePattern: identifier.String(), Description: "Scheduled or on-demand commands, by name."},
		{Path: "backups", Section: "top", Type: "map", NamePattern: identifier.String(), Description: "Volume backups, by name."},
		{Path: "destinations", Section: "top", Type: "map", NamePattern: identifier.String(), Description: "Object storage for backups and state backups, by name."},
		{Path: "state_backup", Section: "top", Type: "map", Description: "Snapshots of the agent's own state."},
		{Path: "telemetry", Section: "top", Type: "map", Description: "OpenTelemetry export of metrics and operation records."},
		{Path: "probes", Section: "top", Type: "map", NamePattern: identifier.String(), Description: "Scheduled checks, by name."},
		{Path: "peers", Section: "top", Type: "map", NamePattern: identifier.String(), Description: "Other agents to watch, by name."},
		{Path: "alerts", Section: "top", Type: "map", Description: "Where alerts go."},
		{Path: "ingress", Section: "top", Type: "map", Description: "Managed Cloudflare Tunnel."},
		{Path: "aliases", Section: "top", Type: "map", NamePattern: identifier.String(), Description: "Short names for command lines, for people at a terminal. At most 64. The MCP server does not offer them. See [Aliases](#aliases)."},
		{Path: "aliases.<alias>", Section: "top", Type: "string", Description: "The command line the alias stands for: a command that talks to an agent, then its arguments, separated by spaces. Quotes are not interpreted, and `-e` and `--config` are not allowed."},
		{Path: "min_version", Section: "top", Type: "version", Description: "The oldest Berthy release the manifest works with, such as `0.9.0`. The CLI and the agent refuse a manifest that needs a newer release. See [the top of `berthy.yml`](#the-top-of-berthyyml)."},

		{Path: "timezone", Section: "top", Type: "string", Default: "UTC", Description: "The time zone every `schedule` is read in, as an IANA name such as `Europe/Lisbon`. A time that does not exist on a day, because the clocks go forward, is skipped. A time that happens twice, because they go back, runs once, the first time. See [Scheduled commands](#step-9-scheduled-commands)."},

		{Path: "registries.<host>.username", Section: "registries", Type: "string", Required: true, Description: "The user name for the registry."},
		{Path: "registries.<host>.password_secret", Section: "registries", Type: "secret name", Required: true, Description: "The secret that holds the password. Store it with `berthy secret set NAME --registry <host>`."},

		{Path: "secrets.<SECRET>.generate", Section: "secrets", Type: "map", Required: true, Description: "Generated by `berthy setup` at a terminal and by `berthy secret generate` without a name, only while the secret is not stored and no app that reads it was ever deployed."},
		{Path: "secrets.<SECRET>.generate.format", Section: "secrets", Type: "string", Required: true, Enum: GenerateFormats, Description: "How the value is encoded: `hex`, URL-safe `base64` without padding, or `alnum`."},
		{Path: "secrets.<SECRET>.generate.bytes", Section: "secrets", Type: "integer", Default: "32", Description: "How many random bytes (for hex and base64) or characters (for alnum), from 16 to 256."},

		{Path: "apps.<name>.kind", Section: "apps", Type: "string", Required: true, Enum: []string{"image", "compose"}, Description: "The kind of app: an image app runs as Swarm services, a Compose app as a Docker Compose project."},
		{Path: "apps.<name>.image", Section: "apps", Type: "string", Required: true, Applies: "image", Description: "The image's repository, with no tag or digest."},
		{Path: "apps.<name>.roles", Section: "apps", Type: "map", Required: true, Applies: "image", NamePattern: identifier.String(), Description: "The app's roles, by name, at least one. A role is a set of containers that run the image with a command, replicas and health check of its own."},
		{Path: "apps.<name>.file", Section: "apps", Type: "string", Required: true, Applies: "compose", Description: "The Compose file's path, relative to the manifest directory. No `..`."},
		{Path: "apps.<name>.project_name", Section: "apps", Type: "name", Default: "<project>-<environment>-<app>", Applies: "compose", Description: "The Compose project name. Never change it once the app holds data."},
		{Path: "apps.<name>.stateful", Section: "apps", Type: "boolean", Default: "false", Applies: "compose", Description: "Recreating or removing the app needs `--allow-recreate` and an `admin` token."},
		{Path: "apps.<name>.depends_on", Section: "apps", Type: "list", Description: "The apps that `berthy deploy` without an app deploys before this one. Each must be another app of the manifest, listed once, with no cycle. It orders nothing else."},
		{Path: "apps.<name>.env", Section: "apps", Type: "map", Description: "The environment of the app's containers."},
		{Path: "apps.<name>.env.plain", Section: "apps", Type: "map", NamePattern: envName.String(), Description: "Variables with a value written in the manifest, which is reviewed like code. Names that start with `BERTHY_` are reserved."},
		{Path: "apps.<name>.env.plain.<VAR>", Section: "apps", Type: "string", Description: "The variable's value."},
		{Path: "apps.<name>.env.secret", Section: "apps", Type: "list", Description: "Secrets whose values the containers get, kept in the agent. An entry is a secret's name, which is also the variable's name, or a one-key map from a variable to a secret's name, such as `DATABASE_URL: API_DATABASE_URL`. No variable twice, and none that is also in `plain`."},
		{Path: "apps.<name>.env.template", Section: "apps", Type: "map", NamePattern: envName.String(), Description: "Variables whose value is built from secrets. The agent fills a template in when it deploys. The manifest and the release keep the template, and the filled value is a secret: it is redacted like one. A variable is in one of `plain`, `secret` and `template`. Names that start with `BERTHY_` are reserved."},
		{Path: "apps.<name>.env.template.<VAR>", Section: "apps", Type: "string", Description: "The variable's template: text with `${NAME}` where the value of the secret NAME goes, such as `postgres://app:${DB_PASSWORD}@db:5432/app`. `$$` writes one `$`, and any other `$` is an error. There are no defaults, no nesting and no references to variables. A reference cannot ask for URL encoding: the hex value `berthy secret generate` makes by default needs none, but a password with `@` or `/` does, so use one that has none."},
		{Path: "apps.<name>.deploy", Section: "apps", Type: "map", Applies: "image", Description: "How a rollout of a new version runs."},
		{Path: "apps.<name>.deploy.deploy_timeout", Section: "apps", Type: "duration", Default: "120s", Applies: "image", Description: "The longest a new version may take to become healthy."},
		{Path: "apps.<name>.deploy.drain_timeout", Section: "apps", Type: "duration", Default: "30s", Applies: "image", Description: "Time the old version gets to finish its requests."},
		{Path: "apps.<name>.deploy.start_attempts", Section: "apps", Type: "integer", Default: "3", Applies: "image", Description: "Failed task starts that a rollout tolerates before it gives up, from 1 to 10."},
		{Path: "apps.<name>.deploy.order", Section: "apps", Type: "string", Enum: []string{StartFirst, StopFirst}, Applies: "image", Description: "How an update replaces each role's old version. `start-first` starts the new one beside the old one. `stop-first` stops the old one before the new one starts, so two versions never write a volume at once. Empty: `stop-first` for a role that mounts a read-write volume, and `start-first` for any other role. `config validate` warns about `start-first` on a role with a read-write volume, and about more than one replica of one."},
		{Path: "apps.<name>.deploy.verify", Section: "apps", Type: "boolean", Default: "true", Applies: "image", Description: "Whether a deploy checks, once the new version serves, that each role with a `port` answers on the shared network and that each of its `hostnames` answers from outside. A check that fails only warns: the deploy still succeeds. `false` turns both off."},
		{Path: "apps.<name>.deploy.retain", Section: "apps", Type: "integer", Default: "5", Applies: "image", Description: "The number of deploys whose images are kept, at least 1."},
		{Path: "apps.<name>.hooks", Section: "apps", Type: "map", Applies: "image", Description: "Steps that run around a deploy. An overlay's list replaces the base manifest's."},
		{Path: "apps.<name>.hooks.pre_deploy", Section: "apps", Type: "list", Applies: "image", Description: "Steps that run in order before the new version starts. A failed step stops the deploy."},
		{Path: "apps.<name>.hooks.post_deploy", Section: "apps", Type: "list", Applies: "image", Description: "Steps that run in order after the new version is serving."},
		{Path: "apps.<name>.migrations", Section: "apps", Type: "map", Applies: "image", Description: "Database migrations, which run as a step of the deploy after the pre-deploy hooks."},

		{Path: "apps.<name>.roles.<role>.service", Section: "roles", Type: "name", Default: "<project>-<environment>-<app>-<role>", Description: "The name of the role's Swarm service. Set it to adopt an existing service."},
		{Path: "apps.<name>.roles.<role>.command", Section: "roles", Type: "list", Default: "the image's", Description: "The container's command, as a list."},
		{Path: "apps.<name>.roles.<role>.entrypoint", Section: "roles", Type: "list", Default: "the image's", Description: "The container's entrypoint, as a list. `command` is passed to it as arguments."},
		{Path: "apps.<name>.roles.<role>.user", Section: "roles", Type: "string", Default: "the image's", Description: "The user the container runs as: a name or a number, with a group after a colon, such as `1000:1000`."},
		{Path: "apps.<name>.roles.<role>.replicas", Section: "roles", Type: "integer", Default: "1", Description: "The number of replicas, at least 1."},
		{Path: "apps.<name>.roles.<role>.port", Section: "roles", Type: "integer", Description: "The port the role listens on, from 0 to 65535. Needed for `hostnames`."},
		{Path: "apps.<name>.roles.<role>.health", Section: "roles", Type: "map", Description: "The container health check, with the fields Docker uses. A new version must pass it before it takes traffic."},
		{Path: "apps.<name>.roles.<role>.health.test", Section: "roles", Type: "list", Required: true, Description: "The command, as a list that starts with `CMD` or `CMD-SHELL`. Exit code 0 means healthy. The image must contain the command."},
		{Path: "apps.<name>.roles.<role>.health.interval", Section: "roles", Type: "duration", Default: "30s", Description: "Time between checks."},
		{Path: "apps.<name>.roles.<role>.health.timeout", Section: "roles", Type: "duration", Default: "30s", Description: "The longest one check may take."},
		{Path: "apps.<name>.roles.<role>.health.start_period", Section: "roles", Type: "duration", Default: "0s", Description: "Grace time after the container starts. Failures in it do not count."},
		{Path: "apps.<name>.roles.<role>.health.start_interval", Section: "roles", Type: "duration", Default: "Docker's", Description: "Time between checks during the start period. Check that your Docker version supports it for Swarm services."},
		{Path: "apps.<name>.roles.<role>.health.retries", Section: "roles", Type: "integer", Default: "3", Description: "Failed checks in a row before the container is unhealthy, at least 1."},
		{Path: "apps.<name>.roles.<role>.volumes", Section: "roles", Type: "list", Description: "Named volumes to mount, each `name:/absolute/path`, with `:ro` or `:rw` at the end. A named volume, never a host path."},
		{Path: "apps.<name>.roles.<role>.resources", Section: "roles", Type: "map", Description: "Resource limits of each container."},
		{Path: "apps.<name>.roles.<role>.resources.cpu", Section: "roles", Type: "string", Default: "no limit", Description: "Cores, as a string such as `\"0.5\"`."},
		{Path: "apps.<name>.roles.<role>.resources.memory", Section: "roles", Type: "string", Default: "no limit", Description: "Memory, such as `512M`, `1G` or `256MiB`."},
		{Path: "apps.<name>.roles.<role>.hostnames", Section: "roles", Type: "list", Description: "Public hostnames of the role. They need `ingress` and `port`. A hostname belongs to one role."},
		{Path: "apps.<name>.roles.<role>.logging", Section: "roles", Type: "map", Description: "The log driver and options of the role's containers. They replace Berthy's default log rotation, which is `max-size: 10m` and `max-file: 3` on `json-file` or `local`; any other driver gets nothing from Berthy unless the role sets `logging`."},
		{Path: "apps.<name>.roles.<role>.logging.driver", Section: "roles", Type: "string", Required: true, Description: "The Docker log driver, such as `json-file`, `local` or `journald`, or a plugin image."},
		{Path: "apps.<name>.roles.<role>.logging.options", Section: "roles", Type: "map", NamePattern: logOptionKey.String(), Description: "Driver options, as strings, such as `max-size: 50m` and `max-file: \"5\"`. The role gets exactly these options. Quote a number."},
		{Path: "apps.<name>.roles.<role>.logging.options.<option>", Section: "roles", Type: "string", Description: "The option's value, at most 1024 bytes."},
	},
	hookKeys("pre_deploy"),
	hookKeys("post_deploy"),
	[]Key{
		{Path: "apps.<name>.migrations.version", Section: "hooks", Type: "list", Required: true, Description: "The command that prints one line naming the current schema state. Berthy stores the line and never interprets it."},
		{Path: "apps.<name>.migrations.apply", Section: "hooks", Type: "list", Required: true, Description: "The command that brings the schema to the latest state the image knows."},
		{Path: "apps.<name>.migrations.revert", Section: "hooks", Type: "list", Description: "The command that brings the schema back to a given state, which `berthy rollback --revert-schema` uses. Exactly one argument must be the standalone word `{target}`."},
		{Path: "apps.<name>.migrations.timeout", Section: "hooks", Type: "duration", Default: defaultCommandTimeout, Description: "The time limit of each migration command, from `1s` to `24h`."},
		{Path: "apps.<name>.migrations.retries", Section: "hooks", Type: "integer", Default: "0", Description: "How many times a failed `apply` runs again, from 0 to 5. `version` and `revert` are never retried."},

		{Path: "jobs.<name>.app", Section: "jobs", Type: "name", Required: true, Description: "The app the job runs in."},
		{Path: "jobs.<name>.role", Section: "jobs", Type: "name", Description: "For an image app: the role whose environment, limits and named volumes the job container takes. Needed when the app has several roles. Not with `service`."},
		{Path: "jobs.<name>.service", Section: "jobs", Type: "name", Description: "For a Compose app: the service that runs the command. Not with `role`."},
		{Path: "jobs.<name>.schedule", Section: "jobs", Type: "string", Default: "on demand", Description: "A five-field cron expression, read in the manifest's `timezone`, UTC by default. Without it the job runs only on demand."},
		{Path: "jobs.<name>.command", Section: "jobs", Type: "list", Required: true, Description: "The command to run, as a list. An image app runs it in a new container of its applied release, and a Compose app in its service's container."},
		{Path: "jobs.<name>.timeout", Section: "jobs", Type: "duration", Default: defaultJobTimeout, Description: "The time limit of a run, from `1s` to `24h`. A run past it is stopped; for an image app that stops its container."},

		{Path: "backups.<name>.app", Section: "backups", Type: "name", Required: true, Description: "The app that mounts the volume, or that the command runs in."},
		{Path: "backups.<name>.volume", Section: "backups", Type: "string", Description: "What to back up: a volume, archived whole. For a Compose app, the key in the Compose file's top-level `volumes`; Berthy works out the real Docker name. For an image app, the name of a volume one of its roles mounts. A backup has exactly one of `volume` and `command`."},
		{Path: "backups.<name>.command", Section: "backups", Type: "list", Description: "What to back up instead of a volume: a command whose standard output is the backup, such as a database dump. The output is compressed and sent to the destination as it is produced, with nothing written to the server's disk. A command that exits non-zero, or writes nothing, fails the backup and nothing is kept. Restore it with `berthy restore KEY --exec`."},
		{Path: "backups.<name>.service", Section: "backups", Type: "name", Description: "For a command backup of a Compose app: the service whose running container runs the command. Not with `role`."},
		{Path: "backups.<name>.role", Section: "backups", Type: "name", Description: "For a command backup of an image app: the role whose environment, limits and named volumes the command's new container takes. Needed when the app has several roles. Not with `service`."},
		{Path: "backups.<name>.timeout", Section: "backups", Type: "duration", Default: defaultJobTimeout, Description: "For a command backup: the time limit of the command, from `1s` to `24h`. A command past it is stopped and the backup fails."},
		{Path: "backups.<name>.schedule", Section: "backups", Type: "string", Default: "on demand", Description: "A five-field cron expression, read in the manifest's `timezone`, UTC by default. Without it the backup runs only on demand or from a deploy hook."},
		{Path: "backups.<name>.destination", Section: "backups", Type: "name", Required: true, Description: "A key of `destinations`."},
		{Path: "backups.<name>.prefix", Section: "backups", Type: "string", Default: "<app>/<volume>", Description: "Where the backup's objects sit in the bucket, under `<project>/<environment>/`. A command backup has `<app>/command` in place of `<app>/<volume>`."},
		{Path: "backups.<name>.before", Section: "backups", Type: "name", Description: "For a volume backup: a job to run first, such as one that dumps a database. If it fails, the backup fails and nothing is uploaded. A command backup needs no such job."},
		{Path: "backups.<name>.retention", Section: "backups", Type: "map", Description: "How many backups to keep."},
		{Path: "backups.<name>.retention.keep", Section: "backups", Type: "integer or all", Default: "all", Description: "`all`, or a whole number of at least 1: keep that many of the newest successful backups."},

		{Path: "destinations.<name>.type", Section: "destinations", Type: "string", Required: true, Enum: []string{"s3"}, Description: "The kind of storage: any S3-compatible object storage."},
		{Path: "destinations.<name>.endpoint", Section: "destinations", Type: "string", Default: "AWS", Description: "`https://host[:port]`, with no path. `http://` is accepted only for localhost or a loopback address. Leave it out for AWS."},
		{Path: "destinations.<name>.region", Section: "destinations", Type: "string", Description: "The bucket's region, when the storage needs one."},
		{Path: "destinations.<name>.bucket", Section: "destinations", Type: "string", Required: true, Description: "The bucket's name: 3 to 63 characters of lowercase letters, digits, `.` and `-`, starting and ending with a letter or digit."},
		{Path: "destinations.<name>.access_key_secret", Section: "destinations", Type: "secret name", Required: true, Description: "The secret that holds the access key."},
		{Path: "destinations.<name>.secret_key_secret", Section: "destinations", Type: "secret name", Required: true, Description: "The secret that holds the secret key."},

		{Path: "state_backup.destination", Section: "state_backup", Type: "name", Required: true, Description: "A key of `destinations`."},
		{Path: "state_backup.prefix", Section: "state_backup", Type: "string", Default: "berthy-state", Description: "The object prefix of the snapshots, under `<project>/<environment>/`."},
		{Path: "state_backup.schedule", Section: "state_backup", Type: "string", Default: DefaultStateBackupSchedule, Description: "A five-field cron expression, read in the manifest's `timezone`, UTC by default. The default is daily at 03:17."},
		{Path: "state_backup.keep", Section: "state_backup", Type: "integer", Default: "14", Description: "The number of snapshots kept, from 1 to 1000."},

		{Path: "probes.<name>.http", Section: "probes", Type: "string", Description: "A URL, `http` or `https`, with a host."},
		{Path: "probes.<name>.tcp", Section: "probes", Type: "string", Description: "`host:port`, with a port from 1 to 65535."},
		{Path: "probes.<name>.service", Section: "probes", Type: "map", Description: "A role or Compose service on the shared network, reached by the overlay helper. A Compose service must join the shared network."},
		{Path: "probes.<name>.service.app", Section: "probes", Type: "name", Required: true, Description: "The app, which must be in the manifest."},
		{Path: "probes.<name>.service.role", Section: "probes", Type: "name", Description: "The role, or for a Compose app the service. May be left out when the app has one role."},
		{Path: "probes.<name>.service.port", Section: "probes", Type: "integer", Required: true, Description: "The port, from 1 to 65535."},
		{Path: "probes.<name>.service.path", Section: "probes", Type: "string", Description: "The path to request. It starts with `/`."},
		{Path: "probes.<name>.expect_status", Section: "probes", Type: "list", Default: "any 200 to 399", Description: "The status codes that count as healthy, each from 100 to 599."},
		{Path: "probes.<name>.interval", Section: "probes", Type: "duration", Default: "30s", Description: "Time between runs. At least `5s`."},
		{Path: "probes.<name>.timeout", Section: "probes", Type: "duration", Default: "5s", Description: "The longest one run may take. Shorter than `interval`. For a `service` probe, from `1s` to `25s`."},
		{Path: "probes.<name>.failures", Section: "probes", Type: "integer", Default: "3", Description: "Failed runs in a row before the state changes, at least 1."},

		{Path: "peers.<name>", Section: "peers", Type: "string", Description: "The base URL of another agent to watch: `http` or `https`, a host and a port, and no path, query or user."},

		{Path: "alerts.reminder", Section: "alerts", Type: "duration", Default: "4h", Description: "How often to repeat an alert while its check keeps failing, on every channel. `0s` turns reminders off."},
		{Path: "alerts.deploys", Section: "alerts", Type: "string", Default: deploysFailed, Enum: []string{deploysFailed, deploysAll, deploysNone}, Description: "Which deploys and rollbacks send a message when they end, on every channel."},
		{Path: "alerts.telegram", Section: "alerts", Type: "map", Description: "Alerts to one Telegram chat."},
		{Path: "alerts.telegram.bot_token_secret", Section: "alerts", Type: "secret name", Required: true, Description: "The secret that holds the bot token."},
		{Path: "alerts.telegram.chat_id", Section: "alerts", Type: "string", Required: true, Description: "A numeric chat ID, or `@channel_name`. Quote it."},
		{Path: "alerts.webhook", Section: "alerts", Type: "map", Description: "Alerts as an HTTP POST of a small JSON document: `environment`, `kind` (`check` or `deploy`), `name`, `state`, `detail`, `since` and `text`, the line Telegram would get. It holds no application output and no secret value."},
		{Path: "alerts.webhook.url_secret", Section: "alerts", Type: "secret name", Required: true, Description: "The secret that holds the URL, an `http` or `https` URL with a host. The URL is often the credential, so it is never in the manifest."},
		{Path: "alerts.heartbeat", Section: "alerts", Type: "map", Description: "Dead man's switch: the agent sends one `GET` to a URL every `interval` while it runs and Docker answers, with a 5-second timeout and no retries. A service that watches for the pings raises the alarm when they stop. `berthy checks` and `doctor` show a `heartbeat` entry with the time of the last accepted ping. It is `unknown` when the URL cannot be reached, and it never alerts."},
		{Path: "alerts.heartbeat.url_secret", Section: "alerts", Type: "secret name", Required: true, Description: "The secret that holds the URL, an `http` or `https` URL with a host. A 2xx answer counts as accepted."},
		{Path: "alerts.heartbeat.interval", Section: "alerts", Type: "duration", Default: "1m", Description: "Time between pings. At least `10s`. A change applies within a minute."},
		{Path: "alerts.webhook.headers_secret", Section: "alerts", Type: "secret name", Description: "The secret that holds request headers, `name=value,name=value`, for example an `Authorization` header."},

		{Path: "telemetry.otlp_endpoint", Section: "telemetry", Type: "string", Required: true, Description: "The collector's OTLP endpoint. The agent runs on the host, so it must be an address the host can reach; a Docker service name does not resolve there."},
		{Path: "telemetry.headers_secret", Section: "telemetry", Type: "secret name", Description: "The secret that holds request headers in the `OTEL_EXPORTER_OTLP_HEADERS` form, `name=value,name=value`, for backends that need authorization."},

		{Path: "ingress.cloudflare", Section: "ingress", Type: "map", Required: true, Description: "Managed ingress: Berthy owns the tunnel, the connector that runs on the server, the hostname routes and the DNS records."},
		{Path: "ingress.cloudflare.account_id", Section: "ingress", Type: "string", Required: true, Description: "The Cloudflare account that owns the tunnel."},
		{Path: "ingress.cloudflare.api_token_secret", Section: "ingress", Type: "secret name", Required: true, Description: "The secret that holds the Cloudflare API token."},
		{Path: "ingress.cloudflare.image", Section: "ingress", Type: "string", Required: true, Description: "The connector's image, with a tag or a digest."},
		{Path: "ingress.cloudflare.replicas", Section: "ingress", Type: "integer", Default: "1", Description: "The number of connectors, at least 1."},
		{Path: "ingress.cloudflare.tunnel", Section: "ingress", Type: "name", Default: "<project>-<environment>", Description: "The tunnel's name in Cloudflare."},
		{Path: "ingress.cloudflare.dns", Section: "ingress", Type: "boolean", Default: "true", Description: "Whether Berthy creates the DNS records."},
	},
)

// keyAt returns the key at path.
func keyAt(path string) (Key, bool) {
	for _, k := range Keys {
		if k.Path == path {
			return k, true
		}
	}
	return Key{}, false
}

// placeholder returns the placeholder, such as "<name>", that the keys under the map at path use for the map's own keys.
func placeholder(path string) (string, bool) {
	prefix := path + ".<"
	for _, k := range Keys {
		if rest, ok := strings.CutPrefix(k.Path, prefix); ok {
			if end := strings.Index(rest, ">"); end >= 0 {
				return "<" + rest[:end+1], true
			}
		}
	}
	return "", false
}
