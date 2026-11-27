package cli

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"berthy/internal/agentclient"
	"berthy/internal/deploy"
	"berthy/internal/gitcheck"
	"berthy/internal/manifest"
	"berthy/internal/scope"
	"berthy/internal/session"

	"github.com/distribution/reference"
)

func problemText(p manifest.Problem, s cliStyle) string {
	location := p.File
	if location != "" && p.Line > 0 {
		location += fmt.Sprintf(":%d", p.Line)
		if p.Column > 0 {
			location += fmt.Sprintf(":%d", p.Column)
		}
	}
	p.File = ""
	if location != "" {
		return s.paint(toneAccent, terminalSafe(location)) + ": " + terminalSafe(p.String())
	}
	return terminalSafe(p.String())
}

func printProblems(w io.Writer, problems manifest.Problems, human bool) {
	s := styleFor(w, human)
	for _, p := range problems {
		fmt.Fprintln(w, problemText(p, s))
	}
	count := fmt.Sprintf("%d problems", len(problems))
	if len(problems) == 1 {
		count = "1 problem"
	}
	fmt.Fprintln(w, s.paint(toneError, count))
}

func printWarnings(w io.Writer, warnings manifest.Problems) {
	s := styleFor(w, true)
	for _, p := range warnings {
		fmt.Fprintln(w, s.paint(toneWarning, "warning:")+" "+problemText(p, s))
	}
}

// originColumn is the widest the path and value are padded to. A longer one, such as a list of hooks, is printed in full, and
// its file and line follow it.
const originColumn = 60

// printOrigins prints one line per value: its path and value, padded to the longest of them up to originColumn, then the file
// and line that write it, or default.
func printOrigins(w io.Writer, origins []manifest.Origin) {
	left := make([]string, len(origins))
	width := 0
	for i, o := range origins {
		left[i] = terminalSafe(o.Path) + ": " + originValue(o.Value)
		width = max(width, utf8.RuneCountInString(left[i]))
	}
	width = min(width, originColumn)
	for i, o := range origins {
		where := "default"
		if o.File != "" {
			where = fmt.Sprintf("%s:%d", terminalSafe(o.File), o.Line)
		}
		fmt.Fprintf(w, "%-*s  %s\n", width, left[i], where)
	}
}

// originValue is a value as config show --origin prints it: a string as it is, and anything else as compact JSON.
func originValue(v any) string {
	if s, ok := v.(string); ok {
		return terminalSafe(s)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}

func isDeploymentCommand(s string) bool {
	return slices.Contains(deploymentCommands, s)
}

// withRemoteFlags is the flags of a command that calls the agent, which all read -e, --config and --json, with extra.
func withRemoteFlags(extra ...string) []string {
	return append([]string{"-e", "--project", "--config", "--json"}, extra...)
}

// changeFlags is the flags each change command reads, keyed by command, or by command and subcommand where the subcommands
// differ. A flag that is not in its command's list is refused before anything is sent: a command given a flag it does not
// read would otherwise run as if the flag were not there, so --dry-run on approve would approve for real. The lists were
// taken from the code: the flags the handler reads, and for the deploy requests the fields the agent acts on for that kind.
// scale accepts --manifest-dir and does not read it: a scale needs no checkout, and TestScaleDoesNotLoadCheckoutManifest relies on that.
var changeFlags = map[string][]string{
	"versions":                 withRemoteFlags("--manifest-dir"),
	"promote":                  withRemoteFlags("--manifest-dir", "--from", "--dry-run", "--request-id", "--commit"),
	"deploy":                   withRemoteFlags("--dry-run", "--request-id", "--manifest-dir", "--version", "--commit", "--adopt", "--replace-existing", "--allow-recreate"),
	"apply":                    withRemoteFlags("--dry-run", "--manifest-dir", "--commit"),
	"rollback":                 withRemoteFlags("--dry-run", "--request-id", "--to", "--revert-schema", "--adopt", "--replace-existing", "--allow-recreate"),
	"restart":                  withRemoteFlags("--dry-run", "--request-id", "--role"),
	"scale":                    withRemoteFlags("--dry-run", "--request-id", "--role", "--replicas", "--manifest-dir"),
	"exec":                     withRemoteFlags("--dry-run", "--request-id", "--role", "--service", "--interactive"),
	"run":                      withRemoteFlags("--dry-run", "--request-id", "--role", "--timeout", "--interactive"),
	"prune":                    withRemoteFlags("--dry-run", "--request-id"),
	"remove":                   withRemoteFlags("--dry-run", "--request-id", "--volumes", "--allow-recreate"),
	"destroy":                  withRemoteFlags("--dry-run", "--request-id", "--volumes", "--allow-recreate", "--confirm", "--tunnel"),
	"restore":                  withRemoteFlags("--dry-run", "--request-id", "--into", "--exec", "--role", "--service", "--timeout"),
	"wait":                     withRemoteFlags("--role", "--timeout", "--healthy"),
	"history":                  withRemoteFlags("--diff"),
	"diagnostics":              withRemoteFlags(),
	"audit":                    withRemoteFlags(),
	"lock status":              withRemoteFlags(),
	"lock acquire":             withRemoteFlags("--dry-run", "--message"),
	"lock release":             withRemoteFlags("--dry-run"),
	"recovery resolve":         withRemoteFlags("--dry-run", "--message"),
	"cancel":                   withRemoteFlags("--dry-run"),
	"job cancel":               withRemoteFlags(),
	"ingress apply":            withRemoteFlags("--dry-run", "--manifest-dir", "--request-id", "--replace-existing", "--commit"),
	"ingress status":           withRemoteFlags(),
	"alerts test":              withRemoteFlags(),
	"secret list":              withRemoteFlags("--manifest-dir"),
	"secret import":            withRemoteFlags("--dry-run"),
	"secret set missing":       {"-e", "--project", "--config", "--manifest-dir"},
	"secret set":               withRemoteFlags("--dry-run", "--registry", "--manifest-dir"),
	"secret generate":          withRemoteFlags("--dry-run", "--replace", "--bytes", "--format", "--manifest-dir"),
	"secret generate declared": withRemoteFlags("--dry-run", "--manifest-dir"),
	"secret delete":            withRemoteFlags("--dry-run"),
	"secret rm":                withRemoteFlags("--dry-run"),
	"job run":                  withRemoteFlags(),
	"job logs":                 withRemoteFlags("--run"),
	"backup list":              withRemoteFlags("--backup"),
	"backup run":               withRemoteFlags(),
	"state-backup":             withRemoteFlags(),
	"key rotate":               withRemoteFlags("--dry-run"),
	"approvals":                withRemoteFlags("--all"),
	"approve":                  withRemoteFlags(),
	"reject":                   withRemoteFlags("--message"),
	"token create":             withRemoteFlags("--dry-run", "--scope", "--agent", "--expires"),
	"token list":               withRemoteFlags(),
	"token revoke":             withRemoteFlags("--dry-run"),
	"config show":              {"-e", "--manifest-dir", "--json", "--origin"},
	"config validate":          {"-e", "--manifest-dir", "--json"},
}

type commandFlags struct {
	env, project, config, dir, commit, id, role, service, timeout, registry, message, to, replicas, confirm, backup, into, exec, scope, expires, run string
	bytes, format, diff, from, redactToken                                                                                                           string
	json, dry, adopt, healthy, revert, replace, allowRecreate, volumes, tunnel, all, interactive, agent, origin                                      bool
	// replaceGenerated is --replace of secret generate: it replaces a stored secret. It is not replace, which is --replace-existing.
	replaceGenerated bool
	envFromVariable  bool
	// dirSet is true only when --manifest-dir's value was assigned. An incomplete --manifest-dir leaves it false even if the
	// flag name was seen, including when parsing stops with a later missing-value error.
	dirSet                        bool
	pos, command, given, versions []string
}

func parseDeploymentArgs(args []string) (commandFlags, error) {
	f := commandFlags{dir: "."}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			f.command = append(f.command, args[i+1:]...)
			break
		}
		if strings.HasPrefix(a, "-") {
			f.given = append(f.given, a)
		}
		switch a {
		case "--json":
			f.json = true
		case "--dry-run":
			f.dry = true
		case "-i", "--interactive":
			f.interactive = true
		case "--adopt":
			f.adopt = true
		case "--healthy":
			f.healthy = true
		case "--replace-existing":
			f.replace = true
		case "--replace":
			f.replaceGenerated = true
		case "--allow-recreate":
			f.allowRecreate = true
		case "--volumes":
			f.volumes = true
		case "--tunnel":
			f.tunnel = true
		case "--all":
			f.all = true
		case "--agent":
			f.agent = true
		case "--origin":
			f.origin = true
		case "--revert-schema":
			f.revert = true
		default:
			if !deploymentFlagTakesValue(a) {
				if strings.HasPrefix(a, "-") {
					return f, fmt.Errorf("unknown flag %s", a)
				}
				f.pos = append(f.pos, a)
				continue
			}
			if i+1 >= len(args) {
				return f, fmt.Errorf("missing value for %s", a)
			}
			i++
			v := args[i]
			switch a {
			case "--from":
				f.from = v
			case "--bytes":
				f.bytes = v
			case "--format":
				f.format = v
			case "-e":
				f.env = v
			case "--project":
				f.project = v
			case "--config":
				f.config = v
			case "--confirm":
				f.confirm = v
			case "--backup":
				f.backup = v
			case "--into":
				f.into = v
			case "--exec":
				f.exec = v
			case "--manifest-dir":
				f.dir = v
				f.dirSet = true
			case "--version":
				f.versions = append(f.versions, v)
			case "--commit":
				f.commit = v
			case "--request-id":
				f.id = v
			case "--role":
				f.role = v
			case "--service":
				f.service = v
			case "--timeout":
				f.timeout = v
			case "--registry":
				f.registry = v
			case "-m", "--message":
				f.message = v
			case "--to":
				f.to = v
			case "--replicas":
				f.replicas = v
			case "--scope":
				f.scope = v
			case "--expires":
				f.expires = v
			case "--diff":
				f.diff = v
			case "--run":
				f.run = v
			}
		}
	}
	return f, nil
}

// deploymentFlagTakesValue is shared by parsing and alias validation.
func deploymentFlagTakesValue(flag string) bool {
	switch flag {
	case "--from", "-e", "--project", "--config", "--confirm", "--backup", "--into", "--exec", "--manifest-dir", "--version", "--commit", "--request-id", "--role", "--service", "--timeout", "--registry", "-m", "--message", "--to", "--replicas", "--bytes", "--format", "--scope", "--expires", "--diff", "--run":
		return true
	}
	return false
}

func countFlag(flags []string, wanted string) int {
	n := 0
	for _, flag := range flags {
		if flag == wanted {
			n++
		}
	}
	return n
}

// canonicalFlag is the name changeFlags lists a flag under: -m is --message, and -i is --interactive.
func canonicalFlag(flag string) string {
	if flag == "-m" {
		return "--message"
	}
	if flag == "-i" {
		return "--interactive"
	}
	return flag
}

// changeKey is the key of changeFlags for a command and its positional arguments. ok is false when the arguments name no
// known subcommand: the handler then refuses them with its usage message.
func changeKey(cmd string, pos []string) (key string, ok bool) {
	sub := ""
	if len(pos) > 0 {
		sub = pos[0]
	}
	switch cmd {
	case "config":
		if len(pos) == 1 && (sub == "show" || sub == "validate") {
			return cmd + " " + sub, true
		}
	case "lock":
		if len(pos) == 0 || (len(pos) == 1 && sub == "status") {
			return "lock status", true
		}
		if len(pos) == 1 && (sub == "acquire" || sub == "release") {
			return "lock " + sub, true
		}
	case "recovery":
		if len(pos) == 2 && sub == "resolve" {
			return "recovery resolve", true
		}
	case "ingress":
		if len(pos) == 1 && (sub == "apply" || sub == "status") {
			return "ingress " + sub, true
		}
	case "alerts":
		if len(pos) == 1 && sub == "test" {
			return "alerts test", true
		}
	case "key":
		if len(pos) == 1 && sub == "rotate" {
			return "key rotate", true
		}
	case "job":
		if len(pos) == 2 && (sub == "run" || sub == "logs" || sub == "cancel") {
			return "job " + sub, true
		}
	case "backup":
		if len(pos) == 1 && sub == "list" {
			return "backup list", true
		}
		if len(pos) == 2 && sub == "run" {
			return "backup run", true
		}
	case "token":
		if len(pos) == 2 && (sub == "create" || sub == "revoke") || len(pos) == 1 && sub == "list" {
			return "token " + sub, true
		}
	case "secret":
		if len(pos) == 1 && sub == "list" {
			return "secret list", true
		}
		if len(pos) == 1 && sub == "import" {
			return "secret import", true
		}
		if len(pos) == 1 && sub == "set" {
			return "secret set missing", true
		}
		if len(pos) == 2 && sub == "set" {
			return "secret set", true
		}
		if len(pos) == 1 && sub == "generate" {
			return "secret generate declared", true
		}
		if len(pos) == 2 && sub == "generate" {
			return "secret generate", true
		}
		if len(pos) == 2 && (sub == "delete" || sub == "rm") {
			return "secret " + sub, true
		}
	default:
		if _, known := changeFlags[cmd]; known {
			return cmd, true
		}
	}
	return "", false
}

// refusedFlag returns the first flag given to cmd that its subcommand does not read, as the person typed it, and the name of
// the command it was given to. flag is empty when every flag is read, or when the subcommand is not known.
func refusedFlag(cmd string, f commandFlags) (flag, key string) {
	key, ok := changeKey(cmd, f.pos)
	if !ok {
		return "", ""
	}
	for _, typed := range f.given {
		if !slices.Contains(changeFlags[key], canonicalFlag(typed)) {
			return typed, key
		}
	}
	return "", key
}

// captureDeployCheckout is the first checkout read for a single mutable-tag deploy.
var captureDeployCheckout = gitcheck.Capture

// runDeployment runs a command of deploymentCommands. dir is where the project is looked for when the line has no --manifest-dir.
// intent is non-nil for an expanded alias: after ordinary validation, the parsed selectors must match it.
func runDeployment(cmd string, args []string, out, errout io.Writer, dir string, intent *aliasIntent) int {
	if (cmd == "versions" || cmd == "promote") && slices.Contains(args, "--") {
		fmt.Fprintf(errout, "%s accepts no arguments after --\n", cmd)
		return 2
	}
	f, err := parseDeploymentArgs(args)
	if err != nil {
		fmt.Fprintln(errout, err)
		return 2
	}
	f.env, f.envFromVariable = agentclient.EnvironmentName(f.env, slices.Contains(f.given, "-e"))
	if !scope.ValidEnvironment(f.env) {
		if f.envFromVariable {
			printRefusal(errout, "BERTHY_ENV is not a valid environment", !f.json)
			return 2
		}
		printRefusal(errout, "valid -e environment required", !f.json)
		return 2
	}
	projectGiven := slices.Contains(f.given, "--project")
	if projectGiven && !scope.ValidProject(f.project) {
		printRefusal(errout, "valid --project required", !f.json)
		return 2
	}
	if f.volumes && cmd != "remove" && cmd != "destroy" {
		printRefusal(errout, "--volumes applies to remove and destroy only", !f.json)
		return 2
	}
	if f.all && cmd != "approvals" {
		printRefusal(errout, "--all applies to approvals only", !f.json)
		return 2
	}
	if (f.tunnel || f.confirm != "") && cmd != "destroy" {
		printRefusal(errout, "--tunnel and --confirm apply to destroy only", !f.json)
		return 2
	}
	if cmd == "destroy" && len(f.pos) != 0 {
		printRefusal(errout, "destroy takes no app, and --confirm must name the project and the environment, as project/environment", !f.json)
		return 2
	}
	if cmd == "key" && (len(f.pos) != 1 || f.pos[0] != "rotate") {
		printRefusal(errout, "usage: key rotate -e ENV [--dry-run] [--json]", !f.json)
		return 2
	}
	if (f.backup != "" && cmd != "backup") || ((f.into != "" || f.exec != "") && cmd != "restore") {
		printRefusal(errout, "--backup applies to backup list, and --into and --exec to restore", !f.json)
		return 2
	}
	if flag, key := refusedFlag(cmd, f); flag != "" {
		printRefusal(errout, fmt.Sprintf("%s is not a flag of %s", flag, key), !f.json)
		return 2
	}
	if cmd == "secret" && len(f.pos) == 1 && (f.pos[0] == "generate" || f.pos[0] == "set") && len(f.command) > 0 {
		printRefusal(errout, "this command accepts no arguments after --", !f.json)
		return 2
	}
	if cmd == "promote" {
		if len(f.pos) != 1 || !scope.ValidEnvironment(f.from) || f.from == f.env || countFlag(f.given, "--from") != 1 {
			printRefusal(errout, "promote requires one app and one different valid --from environment", !f.json)
			return 2
		}
	}
	if cmd == "versions" && len(f.pos) != 1 {
		printRefusal(errout, "versions requires one app", !f.json)
		return 2
	}
	configGiven := slices.Contains(f.given, "--config")
	if code := checkAliasIntent(errout, intent, f.env, f.project, f.config, f.dir, projectGiven, configGiven, f.dirSet); code != 0 {
		return code
	}
	if !f.dirSet {
		f.dir = dir
	}
	if cmd == "config" {
		if len(f.pos) != 1 || (f.pos[0] != "show" && f.pos[0] != "validate") {
			printRefusal(errout, "config requires show or validate", !f.json)
			return 2
		}
		loaded, err := manifest.LoadDetailed(f.dir, f.env)
		if err != nil {
			var problems manifest.Problems
			if f.pos[0] == "validate" && errors.As(err, &problems) {
				printProblems(errout, problems, !f.json)
				return 1
			}
			fmt.Fprintln(errout, err)
			return 1
		}
		if f.pos[0] == "validate" {
			if err := checkAliases(loaded.Manifest.Aliases); err != nil {
				fmt.Fprintln(errout, err)
				return 1
			}
			warnings := loaded.Warnings
			if f.json {
				if warnings == nil {
					warnings = manifest.Problems{}
				}
				b, _ := json.Marshal(struct {
					Warnings manifest.Problems `json:"warnings"`
				}{warnings})
				return printResponse(out, errout, b, true)
			}
			printWarnings(errout, warnings)
			fmt.Fprintln(out, styleFor(out, true).paint(toneOK, "manifest valid"))
			return 0
		}
		if f.origin {
			origins, err := loaded.Origins()
			if err != nil {
				fmt.Fprintln(errout, err)
				return 1
			}
			if f.json {
				b, _ := json.Marshal(origins)
				return printResponse(out, errout, b, true)
			}
			printOrigins(out, origins)
			return 0
		}
		b, _ := json.Marshal(loaded.Manifest)
		return printResponse(out, errout, b, f.json)
	}
	ctx, stop := commandContext()
	defer stop()
	var checkoutBefore gitcheck.Identity
	// A mutable tag needs the identity from before target resolution or manifest loading reads the checkout.
	if cmd == "deploy" && len(f.pos) == 1 && len(f.versions) == 1 && !strings.Contains(f.versions[0], "@") && !bareSHA256(f.versions[0]) {
		checkoutBefore, err = captureDeployCheckout(ctx, f.dir)
		if err != nil {
			fmt.Fprintln(errout, err)
			return 1
		}
	}
	cfgPath, err := agentclient.ConfigPath(f.config)
	if err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	target, err := agentclient.ResolveContext(ctx, cfgPath, agentclient.Selector{Project: f.project, Environment: f.env, ManifestDir: f.dir})
	if err != nil {
		if f.project == "" && (cmd == "deploy" || cmd == "apply" || (cmd == "ingress" && len(f.pos) > 0 && f.pos[0] == "apply")) {
			if _, _, projectErr := manifest.ProjectName(f.dir); projectErr != nil {
				if _, loadErr := manifest.Load(f.dir, f.env); loadErr != nil {
					fmt.Fprintln(errout, loadErr)
					return 1
				}
			}
		}
		fmt.Fprintln(errout, err)
		return 1
	}
	f.project = target.Project
	u, token, prefix := target.Base, target.Token, target.Prefix
	if key, ok := changeKey(cmd, f.pos); ok && !slices.Contains(readOnlyCommands, key) {
		printBanner(errout, scope.ID{Project: f.project, Environment: f.env}, "at "+u.String(), f.envFromVariable, !f.json)
	}
	if cmd == "destroy" && f.confirm != (scope.ID{Project: f.project, Environment: f.env}).String() {
		printRefusal(errout, "destroy takes no app, and --confirm must name the project and the environment, as project/environment", !f.json)
		return 2
	}
	if cmd == "approvals" || cmd == "approve" || cmd == "reject" {
		return runApprovalCommand(ctx, cmd, f, u, prefix, token, out, errout)
	}
	if cmd == "token" {
		return runTokenCommand(ctx, f, u, prefix, token, out, errout)
	}
	if cmd == "job" {
		return runJobCommand(ctx, f, u, prefix, token, out, errout)
	}
	if cmd == "backup" {
		return runBackupCommand(ctx, f, u, prefix, token, out, errout)
	}
	if cmd == "state-backup" {
		return runStateBackupCommand(ctx, f, u, prefix, token, out, errout)
	}
	if cmd == "secret" && len(f.pos) == 1 && f.pos[0] == "list" && slices.Contains(f.given, "--manifest-dir") {
		return runSecretList(ctx, f, u, prefix, token, out, errout)
	}
	if cmd == "secret" && len(f.pos) == 1 && f.pos[0] == "set" {
		return runSecretSetMissing(ctx, f, target, f.dir, out, errout)
	}
	if cmd == "secret" && len(f.pos) == 1 && f.pos[0] == "generate" {
		return runSecretGenerateDeclared(ctx, f, target, f.dir, out, errout)
	}
	client := agentclient.NewHTTPClient()
	if cmd == "promote" {
		return runPromote(ctx, client, target, cfgPath, f.from, f, out, errout)
	}
	if cmd == "versions" {
		return runVersions(ctx, client, target, f, out, errout)
	}
	method := "GET"
	endpoint := ""
	var body any
	var enteredValues []string
	switch cmd {
	case "deploy", "rollback", "restart", "scale", "exec", "run", "prune", "remove", "destroy":
		if cmd == "deploy" && len(f.pos) == 0 {
			return runDeployAll(ctx, f, target, out, errout)
		}
		if cmd != "prune" && cmd != "destroy" && len(f.pos) != 1 || (cmd == "prune" || cmd == "destroy") && len(f.pos) != 0 {
			printRefusal(errout, "command requires an app", !f.json)
			return 2
		}
		if cmd == "deploy" && len(f.versions) > 1 {
			printRefusal(errout, "--version given more than once", !f.json)
			return 2
		}
		if cmd == "deploy" && len(f.versions) == 1 && strings.Contains(f.versions[0], "=") {
			printRefusal(errout, "--version APP=REF is for deploy without an app", !f.json)
			return 2
		}
		var req deploy.Request
		if cmd != "prune" && cmd != "destroy" {
			req.App = f.pos[0]
		}
		req.Confirm = f.confirm
		req.Tunnel = f.tunnel
		if len(f.versions) > 0 {
			req.Version = f.versions[0]
		}
		req.Commit = f.commit
		req.RequestID = f.id
		req.DryRun = f.dry
		req.Adopt = f.adopt
		req.Role = f.role
		req.Service = f.service
		req.RevertSchema = f.revert
		req.ReplaceExisting = f.replace
		req.AllowRecreate = f.allowRecreate
		req.Volumes = f.volumes
		req.Command = f.command
		req.Timeout = f.timeout
		if f.id == "" {
			if req.RequestID, err = newRequestID(); err != nil {
				return 1
			}
		}
		if f.to != "" {
			req.TargetRelease, err = strconv.ParseInt(f.to, 10, 64)
			if err != nil || req.TargetRelease < 1 {
				printRefusal(errout, "invalid --to", !f.json)
				return 2
			}
		}
		if f.replicas != "" {
			v, e := strconv.Atoi(f.replicas)
			if e != nil {
				printRefusal(errout, "invalid --replicas", !f.json)
				return 2
			}
			if v < 0 {
				printRefusal(errout, "invalid --replicas: must be 0 or more", !f.json)
				return 2
			}
			req.Replicas = &v
		}
		if cmd == "scale" && req.Replicas == nil {
			printRefusal(errout, "scale requires --replicas", !f.json)
			return 2
		}
		if cmd == "exec" && f.role != "" && f.service != "" {
			printRefusal(errout, "--role and --service cannot be combined", !f.json)
			return 2
		}
		if cmd == "exec" && len(req.Command) == 0 {
			printRefusal(errout, "exec requires a command after --", !f.json)
			return 2
		}
		if cmd == "run" && len(req.Command) == 0 {
			printRefusal(errout, "run requires a command after --", !f.json)
			return 2
		}
		// A dry run shows what would run, so -i changes nothing about it.
		if f.interactive && !f.dry {
			return runInteractive(ctx, target, session.Request{Kind: cmd, App: req.App, Role: req.Role, Service: req.Service, Command: req.Command, Timeout: req.Timeout}, os.Stdin, out, errout)
		}
		if cmd == "deploy" {
			loaded, e := loadDeployManifest(f.dir, f.env)
			if e != nil {
				fmt.Fprintln(errout, e)
				return 1
			}
			m := loaded.Manifest
			if !manifestMatchesTarget(errout, f.dir, m.Project, target.Project) {
				return 2
			}
			if req.Version != "" && m.Apps[req.App].Kind == "image" {
				selected, selectErr := m.ImageReference(req.App, req.Version)
				if selectErr != nil {
					fmt.Fprintln(errout, selectErr)
					return 1
				}
				if _, canonical := selected.(reference.Canonical); !canonical {
					return runSingleTagDeploy(ctx, client, target, f, loaded, req, selected, checkoutBefore, out, errout)
				}
			}
			co, e := agentclient.PrepareDeploy(ctx, f.dir, f.env, &req)
			if e != nil {
				switch {
				case errors.Is(e, agentclient.ErrVersionRequired):
					printRefusal(errout, "deploy requires --version", !f.json)
					return 2
				case errors.Is(e, agentclient.ErrVersionNotAllowed):
					printRefusal(errout, "--version does not apply to a compose app", !f.json)
					return 2
				}
				fmt.Fprintln(errout, e)
				return 1
			}
			if !manifestMatchesTarget(errout, f.dir, req.Manifest.Project, target.Project) {
				return 2
			}
			warnCheckout(errout, f.commit, co)
			if e := refuseLocalChanges(ctx, client, target, req.Manifest.Protected, co); e != nil {
				fmt.Fprintln(errout, e)
				return 1
			}
			if e := agentclient.CheckAgentVersion(ctx, client, target, req.Manifest); e != nil {
				printRefusal(errout, terminalSafe(e.Error()), !f.json)
				return 1
			}
		}
		method = "POST"
		endpoint = cmd
		if cmd == "deploy" {
			endpoint = "deploys"
		}
		body = req
	case "apply":
		if len(f.pos) != 0 {
			printRefusal(errout, "usage: apply -e ENV [--manifest-dir DIR] [--dry-run] [--json]", !f.json)
			return 2
		}
		m, e := manifest.Load(f.dir, f.env)
		if e != nil {
			fmt.Fprintln(errout, e)
			return 1
		}
		if !manifestMatchesTarget(errout, f.dir, m.Project, target.Project) {
			return 2
		}
		if e := agentclient.CheckAgentVersion(ctx, client, target, *m); e != nil {
			printRefusal(errout, terminalSafe(e.Error()), !f.json)
			return 1
		}
		// The request carries the checkout's commit and local-changes flag as a deploy does.
		stamp := deploy.Request{Commit: f.commit}
		co, e := agentclient.StampCheckout(ctx, f.dir, f.env, &stamp)
		if e != nil {
			fmt.Fprintln(errout, e)
			return 1
		}
		warnCheckout(errout, f.commit, co)
		if e := refuseLocalChanges(ctx, client, target, m.Protected, co); e != nil {
			fmt.Fprintln(errout, e)
			return 1
		}
		method = "POST"
		endpoint = "apply"
		body = deploy.ApplyRequest{Manifest: *m, Commit: stamp.Commit, Modified: stamp.Modified, DryRun: f.dry}
	case "restore":
		// A restore fills a new volume, or feeds a command in a running container: exactly one of the two.
		if len(f.pos) != 1 || f.pos[0] == "" || (f.into == "") == (f.exec == "") || f.exec != "" && len(f.command) == 0 {
			printRefusal(errout, "usage: restore KEY --into VOLUME [--dry-run] | restore KEY --exec APP [--service NAME | --role NAME] [--timeout 1h] [--dry-run] -- COMMAND...", !f.json)
			return 2
		}
		if f.into != "" && (f.role != "" || f.service != "" || f.timeout != "" || len(f.command) > 0) {
			printRefusal(errout, "--role, --service, --timeout and a command belong to restore --exec", !f.json)
			return 2
		}
		if f.role != "" && f.service != "" {
			printRefusal(errout, "--role and --service cannot be combined", !f.json)
			return 2
		}
		id := f.id
		if id == "" {
			if id, err = newRequestID(); err != nil {
				return 1
			}
		}
		method = "POST"
		endpoint = "restore"
		if f.exec != "" {
			body = map[string]any{"key": f.pos[0], "app": f.exec, "service": f.service, "role": f.role, "command": f.command, "timeout": f.timeout, "dry_run": f.dry, "request_id": id}
		} else {
			body = map[string]any{"key": f.pos[0], "into": f.into, "dry_run": f.dry, "request_id": id}
		}
	case "history", "wait":
		if cmd == "history" && f.diff != "" {
			if len(f.pos) < 1 || len(f.pos) > 2 || !releaseNumber(f.diff) || len(f.pos) == 2 && !releaseNumber(f.pos[1]) {
				printRefusal(errout, "usage: history APP --diff RELEASE [BASE]", !f.json)
				return 2
			}
			endpoint = "history/" + url.PathEscape(f.pos[0]) + "/diff"
			break
		}
		if len(f.pos) != 1 {
			printRefusal(errout, "command requires an app", !f.json)
			return 2
		}
		endpoint = cmd + "/" + url.PathEscape(f.pos[0])
	case "diagnostics":
		if len(f.pos) != 1 || f.pos[0] == "" {
			printRefusal(errout, "usage: diagnostics OPERATION_ID -e ENV [--json]", !f.json)
			return 2
		}
		endpoint = "operations/" + url.PathEscape(f.pos[0]) + "/diagnostics"
	case "audit":
		if len(f.pos) != 0 {
			return 2
		}
		endpoint = "audit"
	case "lock":
		if len(f.pos) == 0 || len(f.pos) == 1 && f.pos[0] == "status" {
			endpoint = "lock"
		} else if len(f.pos) == 1 && (f.pos[0] == "acquire" || f.pos[0] == "release") {
			if f.pos[0] == "acquire" && f.message == "" {
				printRefusal(errout, "lock acquire requires -m", !f.json)
				return 2
			}
			endpoint = "lock/" + f.pos[0]
			method = "POST"
			body = map[string]any{"message": f.message, "dry_run": f.dry}
		} else {
			printRefusal(errout, "invalid lock command", !f.json)
			return 2
		}
	case "cancel":
		if len(f.pos) != 1 || f.pos[0] == "" {
			printRefusal(errout, "usage: cancel OPERATION_ID -e ENV [--dry-run] [--json]", !f.json)
			return 2
		}
		endpoint = "operations/" + url.PathEscape(f.pos[0]) + "/cancel"
		method = "POST"
		body = map[string]any{"dry_run": f.dry}
	case "recovery":
		if len(f.pos) != 2 || f.pos[0] != "resolve" || f.pos[1] == "" {
			printRefusal(errout, "usage: recovery resolve OPERATION_ID -m REASON", !f.json)
			return 2
		}
		if f.message == "" {
			printRefusal(errout, "recovery resolve requires -m", !f.json)
			return 2
		}
		endpoint = "operations/" + url.PathEscape(f.pos[1]) + "/resolve"
		method = "POST"
		body = map[string]any{"reason": f.message, "dry_run": f.dry}
	case "ingress":
		if len(f.pos) != 1 || (f.pos[0] != "apply" && f.pos[0] != "status") {
			printRefusal(errout, "usage: ingress apply|status", !f.json)
			return 2
		}
		if f.pos[0] == "status" {
			endpoint = "ingress/status"
		} else {
			m, e := manifest.Load(f.dir, f.env)
			if e != nil {
				fmt.Fprintln(errout, e)
				return 1
			}
			if !manifestMatchesTarget(errout, f.dir, m.Project, target.Project) {
				return 2
			}
			if e := agentclient.CheckAgentVersion(ctx, client, target, *m); e != nil {
				printRefusal(errout, terminalSafe(e.Error()), !f.json)
				return 1
			}
			req := deploy.Request{Manifest: *m, RequestID: f.id, DryRun: f.dry, ReplaceExisting: f.replace, Commit: f.commit}
			co, e := agentclient.StampCheckout(ctx, f.dir, f.env, &req)
			if e != nil {
				fmt.Fprintln(errout, e)
				return 1
			}
			warnCheckout(errout, f.commit, co)
			if e := refuseLocalChanges(ctx, client, target, m.Protected, co); e != nil {
				fmt.Fprintln(errout, e)
				return 1
			}
			if req.RequestID == "" {
				if req.RequestID, err = newRequestID(); err != nil {
					return 1
				}
			}
			method = "POST"
			endpoint = "ingress/apply"
			body = req
		}
	case "alerts":
		if len(f.pos) != 1 || f.pos[0] != "test" {
			printRefusal(errout, "usage: alerts test", !f.json)
			return 2
		}
		endpoint = "alerts/test"
		method = "POST"
	case "key":
		endpoint = "key/rotate"
		method = "POST"
		body = map[string]any{"dry_run": f.dry}
	case "secret":
		if len(f.pos) == 1 && f.pos[0] == "list" {
			endpoint = "secrets"
		} else if len(f.pos) == 1 && f.pos[0] == "import" {
			if stdinTerminal() {
				printRefusal(errout, "pipe NAME=value lines into secret import: standard input is a terminal", !f.json)
				return 2
			}
			imp, err := readSecretImport(ctx, f.dry)
			if err != nil {
				fmt.Fprintln(errout, err)
				return 1
			}
			endpoint = "secrets/import"
			method = "POST"
			body = imp
		} else if len(f.pos) == 2 && (f.pos[0] == "set" || f.pos[0] == "delete" || f.pos[0] == "rm") {
			if f.pos[0] == "set" {
				stored := func() ([]storedSecret, bool) {
					return storedSecrets(ctx, client, agentURL(u, prefix, "secrets"), token, errout, !f.json)
				}
				value, registry, code := secretValue(ctx, f, errout, stored, token)
				if code != 0 {
					return code
				}
				endpoint = "secrets"
				method = "POST"
				body = map[string]any{"name": f.pos[1], "value": value, "registry": registry, "dry_run": f.dry}
				enteredValues = append(enteredValues, value)
			} else {
				endpoint = "secrets/" + url.PathEscape(f.pos[1])
				method = "DELETE"
				body = map[string]any{"dry_run": f.dry}
			}
		} else if len(f.pos) == 2 && f.pos[0] == "generate" {
			req, code := secretGenerateRequest(f, errout)
			if code != 0 {
				return code
			}
			if !req.Replace {
				m, loadErr := manifest.Load(f.dir, f.env)
				if loadErr != nil {
					if _, statErr := os.Stat(filepath.Join(f.dir, "berthy.yml")); statErr == nil {
						fmt.Fprintf(errout, "manifest not loaded: %v\n", loadErr)
					}
				} else if m.Project == target.Project && len(m.SecretReaders(req.Name)) > 0 {
					req.Manifest = m
					for _, app := range m.SecretReaders(req.Name) {
						if m.Apps[app].Kind != "compose" {
							continue
						}
						file, err := agentclient.ReadComposeFile(f.dir, m.Apps[app].File)
						if err != nil {
							continue
						}
						if req.ComposeFiles == nil {
							req.ComposeFiles = map[string]string{}
						}
						req.ComposeFiles[app] = file
					}
				}
			}
			endpoint = "secrets/generate"
			method = "POST"
			body = req
		} else {
			printRefusal(errout, "invalid secret command", !f.json)
			return 2
		}
	}
	if cmd == "wait" {
		client = agentclient.WithTimeout(client, 11*time.Minute)
	}
	return sendCommand(ctx, client, u, prefix, token, method, endpoint, body, f, cmd, out, errout, enteredValues...)
}

func requestWaitLabel(cmd string, f commandFlags, method string) string {
	key, ok := changeKey(cmd, f.pos)
	if !ok {
		return ""
	}
	switch key {
	case "wait":
		return "waiting for app"
	case "ingress status":
		return "reading ingress status"
	case "alerts test":
		return "testing alerts"
	case "key rotate":
		if f.dry {
			return "checking key rotation"
		}
		return "rotating key"
	case "secret set":
		if f.dry {
			return "checking secret"
		}
		return "storing secret"
	case "deploy":
		if method == http.MethodPost {
			if f.dry {
				return "checking deploy"
			}
			return "deploying"
		}
	}
	return ""
}

// sendCommand sends one change or read request to the agent and follows what the agent answers: a held approval, an
// operation to watch to its end, or a result to print. It is what runDeployment does once it has built the request. base is
// the agent's URL, which it does not change.
func sendCommand(ctx context.Context, client *http.Client, base *url.URL, prefix, token, method, endpoint string, body any, f commandFlags, cmd string, out, errout io.Writer, enteredValues ...string) int {
	for _, value := range append([]string(nil), enteredValues...) {
		enteredValues = append(enteredValues, wireSecret(value))
	}
	u := agentURL(base, prefix, endpoint)
	if cmd == "wait" {
		q := u.Query()
		if f.role != "" {
			q.Set("role", f.role)
		}
		if f.timeout != "" {
			q.Set("timeout", f.timeout)
		}
		if f.healthy {
			q.Set("healthy", "true")
		}
		u.RawQuery = q.Encode()
	}
	if cmd == "history" && f.diff != "" {
		q := u.Query()
		q.Set("release", f.diff)
		if len(f.pos) == 2 {
			q.Set("base", f.pos[1])
		}
		u.RawQuery = q.Encode()
	}
	var payload []byte
	if body != nil {
		var err error
		payload, err = json.Marshal(body)
		if err != nil {
			return 1
		}
	}
	progress := startWait(errout, !f.json, requestWaitLabel(cmd, f, method))
	var requestErr bytes.Buffer
	status, data, older, ok := askAgent(ctx, client, u, token, method, payload, &requestErr)
	progress.Close()
	if !ok {
		_, _ = io.Copy(errout, &requestErr)
		return 1
	}
	var promotionMarkers []cliTone
	if f.redactToken != "" && (status == http.StatusOK || status == http.StatusAccepted) {
		safe, err := safePromotionResponse(data, token, f.redactToken)
		if err != nil {
			fmt.Fprintln(errout, err)
			return 1
		}
		if status == http.StatusAccepted {
			promotionMarkers = diffMarkers(data, true, token, f.redactToken)
		}
		data = safe
	}
	if status == http.StatusAccepted {
		heldValues := append([]string{token, f.redactToken}, enteredValues...)
		if f.redactToken != "" {
			heldValues = nil
		}
		if code, held := reportHeldMarkers(data, out, errout, f.json, promotionMarkers, heldValues...); held {
			return code
		}
		var accepted struct {
			ID     string `json:"operation_id"`
			Events string `json:"events_url"`
		}
		if json.Unmarshal(data, &accepted) != nil || accepted.ID == "" {
			fmt.Fprintln(errout, "invalid operation response")
			return 1
		}
		expected := prefix + "operations/" + url.PathEscape(accepted.ID) + "/events"
		if accepted.Events != expected || strings.HasPrefix(accepted.Events, "//") || path.Clean(accepted.Events) != accepted.Events {
			fmt.Fprintln(errout, "invalid operation events URL")
			return 1
		}
		return watchOperation(ctx, client, u, token, prefix, accepted.ID, accepted.Events, out, errout, f.json, cmd, f.env, f.project, operationWatchOptions{config: f.config, extraTokens: append([]string{f.redactToken}, enteredValues...)})
	}
	if status != http.StatusOK {
		values := append([]string{token, f.redactToken}, enteredValues...)
		printAgentRefusal(errout, safeSecretText(agentclient.Message(status, data), values...), !f.json, older)
		if payload != nil && agentclient.InvalidRequest(status, data) {
			if hint := agentclient.OlderAgentHint(ctx, client, &agentclient.Target{Env: f.env, Base: base, Prefix: prefix, Token: token}); hint != "" {
				fmt.Fprintln(errout, styleFor(errout, !f.json).paint(toneInfo, terminalSafe(hint)))
			}
		}
		return 1
	}
	if cmd == "cancel" && !f.json {
		return reportCancel(ctx, client, base, prefix, token, data, f, out, errout)
	}
	if cmd == "diagnostics" && !f.json {
		return printDiagnostics(out, errout, data)
	}
	if cmd == "remove" && f.dry && !f.json {
		return printRemovePlan(out, errout, data)
	}
	if cmd == "destroy" && f.dry && !f.json {
		return printDestroyPlan(out, errout, data, f.env)
	}
	if cmd == "restore" && f.dry && !f.json {
		return printRestorePlan(out, errout, data)
	}
	if cmd == "history" && f.diff != "" && !f.json {
		return printDiff(out, errout, data)
	}
	if (cmd == "deploy" || cmd == "rollback") && f.dry && !f.json {
		return printDryRun(out, errout, data)
	}
	if cmd == "history" && !f.json {
		return printHistory(out, errout, data)
	}
	if cmd == "alerts" {
		return printAlertsTest(out, errout, data, f.json)
	}
	if cmd == "key" && !f.json {
		return printKeyRotation(out, errout, data, scope.ID{Project: f.project, Environment: f.env})
	}
	if cmd == "apply" && !f.json {
		return printApply(out, errout, data, f.dry)
	}
	if cmd == "secret" && f.pos[0] == "set" && !f.dry && !f.json {
		if printSecretSet(out, data, append([]string{token}, enteredValues...)...) {
			return 0
		}
	}
	if cmd == "secret" && len(f.pos) > 0 && f.pos[0] == "set" {
		data = redactRegistryJSON(data, append([]string{token}, enteredValues...)...)
	}
	if cmd == "secret" && f.pos[0] == "import" && !f.json {
		return printSecretImport(out, errout, data, f.dry)
	}
	if cmd == "secret" && f.pos[0] == "generate" && !f.json {
		return printSecretGenerate(out, errout, data, f.dry, f.replaceGenerated)
	}
	shape := shapeGeneric
	switch cmd {
	case "wait":
		shape = shapeWait
	case "audit":
		shape = shapeAudit
	case "lock":
		if len(f.pos) == 0 || len(f.pos) == 1 && f.pos[0] == "status" {
			shape = shapeLock
		}
	case "secret":
		if len(f.pos) == 1 && f.pos[0] == "list" {
			shape = shapeSecrets
		}
	case "ingress":
		if len(f.pos) > 0 && f.pos[0] == "status" {
			shape = shapeIngress
		}
	}
	return printResponseShape(out, errout, data, f.json, shape)
}

// agentURL returns base with the path of endpoint under prefix. base is not changed.
func agentURL(base *url.URL, prefix, endpoint string) *url.URL {
	copied := *base
	copied.Path = prefix + endpoint
	return &copied
}

// askAgent sends one request to the agent at u with the token, with payload as its body when it is not nil, and reads the
// answer. When the agent cannot be reached or its answer cannot be read, it prints why and returns ok false.
func askAgent(ctx context.Context, client *http.Client, u *url.URL, token, method string, payload []byte, errout io.Writer) (status int, data []byte, older, ok bool) {
	resp, err := agentclient.Send(ctx, client, method, u.String(), token, payload)
	if err != nil {
		fmt.Fprintln(errout, "agent connection failed")
		return 0, nil, false, false
	}
	defer resp.Body.Close()
	data, err = io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err != nil || len(data) > 1<<20 {
		fmt.Fprintln(errout, "invalid agent response")
		return 0, nil, false, false
	}
	return resp.StatusCode, data, agentclient.IsOlderAgent(resp), true
}

// newRequestID is a fresh random request ID, which the agent uses to recognize a retry of the same request.
func newRequestID() (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(random[:]), nil
}

func printResponse(out, errout io.Writer, b []byte, asJSON bool) int {
	return printResponseShape(out, errout, b, asJSON, shapeGeneric)
}

func printResponseShape(out, errout io.Writer, b []byte, asJSON bool, shape textShape) int {
	var v any
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if dec.Decode(&v) != nil {
		fmt.Fprintln(errout, "invalid agent response")
		return 1
	}
	if asJSON {
		pretty, _ := json.MarshalIndent(v, "", "  ")
		fmt.Fprintln(out, string(pretty))
	} else {
		printStyledText(out, v, "", styleFor(out, true), shape)
	}
	return 0
}

// reportsSteps says whether the command's operation reports what it did as steps.
func reportsSteps(cmd string) bool { return cmd == "remove" || cmd == "destroy" || cmd == "restore" }

// showsText says whether an event of kind carries a line of text that the command prints instead of the event's name: the
// steps of a removal, a destroy or a restore, and the results of the checks at the end of any operation.
func showsText(cmd, kind string) bool {
	return kind == "verify" || reportsSteps(cmd) && kind == "step"
}

// withChecks adds the verify events of an operation to its JSON document as "checks". Without any it returns data as it is.
func withChecks(data []byte, checks []json.RawMessage) []byte {
	if len(checks) == 0 {
		return data
	}
	var doc map[string]json.RawMessage
	if json.Unmarshal(data, &doc) != nil {
		return data
	}
	list, err := json.Marshal(checks)
	if err != nil {
		return data
	}
	doc["checks"] = list
	out, err := json.Marshal(doc)
	if err != nil {
		return data
	}
	return out
}

func manifestMatchesTarget(errout io.Writer, dir, project, target string) bool {
	if err := agentclient.CheckManifestProject(dir, project, target); err != nil {
		fmt.Fprintln(errout, err)
		return false
	}
	return true
}
