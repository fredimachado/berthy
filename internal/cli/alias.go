package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"berthy/internal/manifest"
)

// deploymentCommands are the commands runDeployment serves: they call the agent and read their own flags.
var deploymentCommands = []string{"config", "deploy", "apply", "rollback", "restart", "scale", "exec", "run", "prune", "remove", "destroy", "wait", "history", "diagnostics", "audit", "lock", "recovery", "cancel", "secret", "ingress", "alerts", "job", "backup", "restore", "state-backup", "key", "approvals", "approve", "reject", "token"}

// readCommandNames are the commands Run serves with a GET.
var readCommandNames = []string{"ps", "describe", "stats", "logs", "df", "doctor", "checks", "jobs", "agent-info"}

// localCommands are the built-ins that never call the agent through a manifest alias: they serve, set up or print locally.
var localCommands = []string{"version", "docs", "probe-helper", "volume-archive", "volume-extract", "mcp", "ui", "setup", "context", "state-restore", "agent"}

// agentCommands are the commands that call the agent, and so may be the first word of an alias.
var agentCommands = slices.Concat(deploymentCommands, readCommandNames)

// builtinCommands is every first word Run dispatches. An alias may not take one of these names. TestBuiltinCommandsAllDispatch
// keeps it in step with Run.
var builtinCommands = slices.Concat(localCommands, agentCommands)

// checkAliases reports the first alias, in name order, that the CLI cannot run. An alias may not take the name of a built-in
// command. Its first word must be a command that talks to an agent, not another alias. It may not set -e, --config or
// --environment, because the environment is always given on the command line. Each error names the alias.
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
				return fmt.Errorf("alias %s: %s is not allowed; the environment is given on the command line", name, w)
			}
			if deploymentFlagTakesValue(w) || slices.Contains([]string{"--since", "--tail", "--grep", "--replica"}, w) {
				i++
			}
		}
	}
	return nil
}

// expandAlias returns the command line the alias cmd of the manifest stands for. The person's -e, --config and --manifest-dir
// go right after the alias's first word, and every other argument the person gave goes at the end. ok reports whether cmd is
// an alias of the manifest. When it is not, err says why cmd is unknown. When it is, but the manifest's aliases cannot run,
// ok is true and err names the alias. dir is the manifest directory the alias was found in: the expanded line may not take
// --manifest-dir, and the command still has to find its project in that directory, not in the one it runs in.
func expandAlias(cmd string, args []string) (expanded []string, dir string, ok bool, err error) {
	dir, env := ".", ""
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
				return nil, "", false, fmt.Errorf("missing value for %s", a)
			}
			i++
			switch a {
			case "-e":
				env = args[i]
				flags = append(flags, a, args[i])
			case "--config", "--project":
				flags = append(flags, a, args[i])
			case "--manifest-dir":
				dir = args[i]
				manifestDir = []string{a, args[i]}
			}
		default:
			rest = append(rest, a)
		}
	}
	unknown := fmt.Errorf("unknown command %s", cmd)
	if env == "" {
		// Aliases live in the manifest, and an overlay is per environment, so -e is needed to read them. Without -e the CLI cannot
		// tell a typo from an alias, so the message says both.
		if _, statErr := os.Stat(filepath.Join(dir, "berthy.yml")); statErr == nil {
			return nil, "", false, fmt.Errorf("unknown command %s (an alias of the manifest needs -e)", cmd)
		}
		return nil, "", false, unknown
	}
	m, err := manifest.Load(dir, env)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, "", false, unknown
	}
	if err != nil {
		return nil, "", false, fmt.Errorf("unknown command %s (the manifest did not load: %v)", cmd, err)
	}
	value, found := m.Aliases[cmd]
	if !found {
		return nil, "", false, unknown
	}
	if err := checkAliases(m.Aliases); err != nil {
		return nil, "", true, err
	}
	words := strings.Fields(value)
	line := func(pairs []string) []string {
		out := append([]string{words[0]}, pairs...)
		out = append(out, words[1:]...)
		return append(out, rest...)
	}
	// --manifest-dir is read for the lookup above. It goes on to the expanded line only when that command takes it.
	if withDir := line(append(slices.Clone(flags), manifestDir...)); len(manifestDir) > 0 && acceptsFlag(withDir, "--manifest-dir") {
		return withDir, dir, true, nil
	}
	return line(flags), dir, true, nil
}

// acceptsFlag reports whether the command line takes flag. It asks the list that already refuses the flags a change command does
// not read: changeFlags, under the key of the line's command and subcommand. A command outside that list, such as a read
// command, takes none of these flags.
func acceptsFlag(line []string, flag string) bool {
	f, err := parseDeploymentArgs(line[1:])
	if err != nil {
		return false
	}
	key, known := changeKey(line[0], f.pos)
	return known && slices.Contains(changeFlags[key], flag)
}
