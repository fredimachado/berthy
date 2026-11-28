package cli

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"berthy/internal/agentclient"
	"berthy/internal/manifest"
	"berthy/internal/scope"
)

// deploymentCommands are the commands runDeployment serves: they call the agent and read their own flags.
var deploymentCommands = []string{"config", "versions", "promote", "deploy", "apply", "rollback", "restart", "scale", "exec", "run", "prune", "remove", "destroy", "wait", "history", "diagnostics", "audit", "lock", "recovery", "cancel", "secret", "ingress", "alerts", "job", "backup", "restore", "state-backup", "key", "approvals", "approve", "reject", "token"}

// readCommandNames are the commands Run serves with a GET.
var readCommandNames = []string{"ps", "describe", "stats", "logs", "df", "doctor", "checks", "jobs", "agent-info"}

// localCommands are the public built-ins that never call the agent through a manifest alias: they serve, set up or print locally.
var localCommands = []string{"version", "docs", "mcp", "ui", "setup", "context", "state-restore", "agent"}

// helperCommands are internal entry points. They stay in builtinCommands so aliases cannot take their names, but bare usage omits them.
var helperCommands = []string{"probe-helper", "volume-archive", "volume-extract"}

// agentCommands are the commands that call the agent, and so may be the first word of an alias.
var agentCommands = slices.Concat(deploymentCommands, readCommandNames)

// publicCommands is every first word the bare usage lists.
var publicCommands = slices.Concat(localCommands, agentCommands)

// builtinCommands is every first word Run dispatches. An alias may not take one of these names. TestBuiltinCommandsAllDispatch
// keeps it in step with Run.
var builtinCommands = slices.Concat(publicCommands, helperCommands)

// checkAliases reports the first alias, in name order, that the CLI cannot run. An alias may not take the name of a built-in
// command. Its first word must be a command that talks to an agent, not another alias. It may not set -e, --config or
// --environment, because the environment comes from the command line or BERTHY_ENV. Each error names the alias.
func checkAliases(aliases map[string]string) error {
	for _, name := range slices.Sorted(maps.Keys(aliases)) {
		words := strings.Fields(aliases[name])
		if slices.Contains(builtinCommands, name) {
			return fmt.Errorf("alias %s shadows the built-in command %s", name, name)
		}
		if len(words) == 0 {
			return fmt.Errorf("alias %s: a command is required", name)
		}
		if _, another := aliases[words[0]]; another {
			return fmt.Errorf("alias %s: its first word is another alias", name)
		}
		if !slices.Contains(agentCommands, words[0]) {
			return fmt.Errorf("alias %s: its first word must be a command that talks to an agent", name)
		}
		for i := 1; i < len(words); i++ {
			w := words[i]
			if w == "--" {
				break
			}
			if w == "-e" || w == "--project" || w == "--config" || w == "--environment" {
				return fmt.Errorf("alias %s: %s is not allowed; the environment is given on the command line or by BERTHY_ENV", name, w)
			}
			if aliasDefFlagTakesValue(words[0], w) {
				i++
			}
		}
	}
	return nil
}

// aliasDefFlagTakesValue is for validating an alias definition: which flags in the alias text take a following word.
func aliasDefFlagTakesValue(cmd, flag string) bool {
	return deploymentFlagTakesValue(cmd, flag) || slices.Contains([]string{"--since", "--tail", "--grep", "--replica"}, flag)
}

// aliasIntent is the environment, project, config and manifest directory the operator chose when invoking an alias, before
// expansion. The real command parsers must resolve the same selectors; ordinary builtin calls carry a nil intent.
type aliasIntent struct {
	name                                string
	env                                 string
	project, config, manifestDir        string
	projectGiven, configGiven, dirGiven bool
}

// expandAlias returns the command line the alias cmd of the manifest stands for. The person's -e, --config and --manifest-dir
// go right after the alias's first word, and every other argument the person gave goes at the end. ok reports whether cmd is
// an alias of the manifest. When it is not, err says why cmd is unknown. When it is, but the manifest's aliases cannot run,
// ok is true and err names the alias. dir is the manifest directory the alias was found in: the expanded line may not take
// --manifest-dir, and the command still has to find its project in that directory, not in the one it runs in. intent holds the
// selectors extracted from the invocation for the dispatcher to check after the real parsers run.
func expandAlias(cmd string, args []string) (expanded []string, dir string, intent *aliasIntent, ok bool, err error) {
	dir, env, project, config := manifest.DefaultDir(), "", "", ""
	var envGiven, projectGiven, configGiven bool
	var flags, manifestDir, rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			rest = append(rest, args[i:]...)
			break
		}
		switch a {
		case "-e", "--project", "--config", "--manifest-dir":
			if i+1 == len(args) {
				return nil, "", nil, false, fmt.Errorf("missing value for %s", a)
			}
			i++
			switch a {
			case "-e":
				env = args[i]
				envGiven = true
				flags = append(flags, a, args[i])
			case "--config":
				config = args[i]
				configGiven = true
				flags = append(flags, a, args[i])
			case "--project":
				project = args[i]
				projectGiven = true
				flags = append(flags, a, args[i])
			case "--manifest-dir":
				dir = args[i]
				manifestDir = []string{a, args[i]}
			}
		default:
			rest = append(rest, a)
		}
	}
	env, fromVariable := agentclient.EnvironmentName(env, envGiven)
	unknown := fmt.Errorf("unknown command %s", cmd)
	if env == "" {
		// Aliases live in the manifest, and an overlay is per environment, so -e is needed to read them. Without -e the CLI cannot
		// tell a typo from an alias, so the message says both.
		if _, statErr := os.Stat(filepath.Join(dir, "berthy.yml")); statErr == nil {
			return nil, "", nil, false, fmt.Errorf("unknown command %s (an alias of the manifest needs -e)", cmd)
		}
		return nil, "", nil, false, unknown
	}
	if fromVariable && !scope.ValidEnvironment(env) {
		return nil, "", nil, false, fmt.Errorf("BERTHY_ENV is not a valid environment")
	}
	m, err := manifest.Load(dir, env)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, "", nil, false, unknown
	}
	if err != nil {
		return nil, "", nil, false, fmt.Errorf("unknown command %s (the manifest did not load: %v)", cmd, err)
	}
	value, found := m.Aliases[cmd]
	if !found {
		return nil, "", nil, false, unknown
	}
	if err := checkAliases(m.Aliases); err != nil {
		return nil, "", nil, true, err
	}
	words := strings.Fields(value)
	prefix := func(pairs []string) []string {
		out := append([]string{words[0]}, pairs...)
		return append(out, words[1:]...)
	}
	line := func(pairs []string) []string {
		return append(prefix(pairs), rest...)
	}
	// --manifest-dir is read for the lookup above. It goes on to the expanded line only when that command takes it.
	trusted := prefix(flags)
	expanded = line(flags)
	if withDir := line(append(slices.Clone(flags), manifestDir...)); len(manifestDir) > 0 && acceptsFlag(withDir, "--manifest-dir") {
		trusted = prefix(append(slices.Clone(flags), manifestDir...))
		expanded = withDir
	}
	intent = &aliasIntent{
		name: cmd, env: env,
		project: project, config: config,
		projectGiven: projectGiven, configGiven: configGiven,
	}
	// A complete --manifest-dir in the operator flags or the alias definition is trusted, even when the trusted prefix ends
	// in another value flag whose argument the caller will supply. An incomplete --manifest-dir is not (dirSet stays false).
	if tf, _ := parseDeploymentArgs(trusted[0], trusted[1:]); tf.dirSet {
		intent.dirGiven = true
		intent.manifestDir = tf.dir
	}
	return expanded, dir, intent, true, nil
}

// checkAliasIntent refuses when the real parser resolved a different environment, project, config or manifest directory than
// the ones used to load the alias. Call it only after the ordinary environment and project validations.
func checkAliasIntent(errout io.Writer, intent *aliasIntent, env, project, config, manifestDir string, projectGiven, configGiven, dirGiven bool) int {
	if intent == nil {
		return 0
	}
	switch {
	case env != intent.env:
		fmt.Fprintf(errout, "alias %s targets environment %s but was loaded for environment %s\n", intent.name, env, intent.env)
	case projectGiven != intent.projectGiven || project != intent.project:
		fmt.Fprintf(errout, "alias %s targets a different project than the one used to load it\n", intent.name)
	case configGiven != intent.configGiven || config != intent.config:
		fmt.Fprintf(errout, "alias %s targets a different config than the one used to load it\n", intent.name)
	case dirGiven && (!intent.dirGiven || manifestDir != intent.manifestDir):
		// Refuse a directory the final parser saw that was not complete in the trusted prefix (operator flags + alias text).
		fmt.Fprintf(errout, "alias %s targets a different manifest directory than the one used to load it\n", intent.name)
	default:
		return 0
	}
	return 1
}

// acceptsFlag reports whether the command line takes flag. It asks the list that already refuses the flags a change command does
// not read: changeFlags, under the key of the line's command and subcommand. A command outside that list, such as a read
// command, takes none of these flags.
func acceptsFlag(line []string, flag string) bool {
	f, err := parseDeploymentArgs(line[0], line[1:])
	if err != nil {
		return false
	}
	key, known := changeKey(line[0], f.pos)
	return known && slices.Contains(changeFlags[key], flag)
}
