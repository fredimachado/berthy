package cli

import (
	"bufio"
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
	"os/signal"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"berthy/internal/agentclient"
	"berthy/internal/deploy"
	"berthy/internal/manifest"
)

func isDeploymentCommand(s string) bool {
	switch s {
	case "config", "deploy", "rollback", "restart", "scale", "exec", "prune", "remove", "destroy", "wait", "history", "diagnostics", "audit", "lock", "recovery", "secret", "ingress", "alerts", "job", "backup", "restore", "state-backup", "approvals", "approve", "reject":
		return true
	}
	return false
}

// withRemoteFlags is the flags of a command that calls the agent, which all read -e, --config and --json, with extra.
func withRemoteFlags(extra ...string) []string {
	return append([]string{"-e", "--config", "--json"}, extra...)
}

// changeFlags is the flags each change command reads, keyed by command, or by command and subcommand where the subcommands
// differ. A flag that is not in its command's list is refused before anything is sent: a command given a flag it does not
// read would otherwise run as if the flag were not there, so --dry-run on approve would approve for real. The lists were
// taken from the code: the flags the handler reads, and for the deploy requests the fields the agent acts on for that kind.
// scale accepts --manifest-dir and does not read it: a scale needs no checkout, and TestScaleDoesNotLoadCheckoutManifest relies on that.
var changeFlags = map[string][]string{
	"deploy":           withRemoteFlags("--dry-run", "--request-id", "--manifest-dir", "--version", "--commit", "--adopt", "--replace-existing", "--allow-recreate"),
	"rollback":         withRemoteFlags("--dry-run", "--request-id", "--to", "--revert-schema", "--adopt", "--replace-existing", "--allow-recreate"),
	"restart":          withRemoteFlags("--dry-run", "--request-id", "--role"),
	"scale":            withRemoteFlags("--dry-run", "--request-id", "--role", "--replicas", "--manifest-dir"),
	"exec":             withRemoteFlags("--dry-run", "--request-id", "--role"),
	"prune":            withRemoteFlags("--dry-run", "--request-id"),
	"remove":           withRemoteFlags("--dry-run", "--request-id", "--volumes", "--allow-recreate"),
	"destroy":          withRemoteFlags("--dry-run", "--request-id", "--volumes", "--allow-recreate", "--confirm", "--tunnel"),
	"restore":          withRemoteFlags("--dry-run", "--request-id", "--into"),
	"wait":             withRemoteFlags("--role", "--timeout", "--healthy"),
	"history":          withRemoteFlags(),
	"diagnostics":      withRemoteFlags(),
	"audit":            withRemoteFlags(),
	"lock status":      withRemoteFlags(),
	"lock acquire":     withRemoteFlags("--dry-run", "--message"),
	"lock release":     withRemoteFlags("--dry-run"),
	"recovery resolve": withRemoteFlags("--dry-run", "--message"),
	"ingress apply":    withRemoteFlags("--dry-run", "--manifest-dir", "--request-id", "--replace-existing", "--commit"),
	"ingress status":   withRemoteFlags(),
	"alerts test":      withRemoteFlags(),
	"secret list":      withRemoteFlags(),
	"secret set":       withRemoteFlags("--dry-run", "--registry", "--manifest-dir"),
	"secret delete":    withRemoteFlags("--dry-run"),
	"secret rm":        withRemoteFlags("--dry-run"),
	"job run":          withRemoteFlags(),
	"backup list":      withRemoteFlags("--backup"),
	"backup run":       withRemoteFlags(),
	"state-backup":     withRemoteFlags(),
	"approvals":        withRemoteFlags("--all"),
	"approve":          withRemoteFlags(),
	"reject":           withRemoteFlags("--message"),
	"config show":      {"-e", "--manifest-dir", "--json"},
	"config validate":  {"-e", "--manifest-dir"},
}

type commandFlags struct {
	env, config, dir, commit, id, role, timeout, registry, message, to, replicas, confirm, backup, into string
	json, dry, adopt, healthy, revert, replace, allowRecreate, volumes, tunnel, all                     bool
	pos, command, given, versions                                                                       []string
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
		case "--adopt":
			f.adopt = true
		case "--healthy":
			f.healthy = true
		case "--replace-existing":
			f.replace = true
		case "--allow-recreate":
			f.allowRecreate = true
		case "--volumes":
			f.volumes = true
		case "--tunnel":
			f.tunnel = true
		case "--all":
			f.all = true
		case "--revert-schema":
			f.revert = true
		case "-e", "--config", "--confirm", "--backup", "--into", "--manifest-dir", "--version", "--commit", "--request-id", "--role", "--timeout", "--registry", "-m", "--message", "--to", "--replicas":
			if i+1 >= len(args) {
				return f, fmt.Errorf("missing value for %s", a)
			}
			i++
			v := args[i]
			switch a {
			case "-e":
				f.env = v
			case "--config":
				f.config = v
			case "--confirm":
				f.confirm = v
			case "--backup":
				f.backup = v
			case "--into":
				f.into = v
			case "--manifest-dir":
				f.dir = v
			case "--version":
				f.versions = append(f.versions, v)
			case "--commit":
				f.commit = v
			case "--request-id":
				f.id = v
			case "--role":
				f.role = v
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
			}
		default:
			if strings.HasPrefix(a, "-") {
				return f, fmt.Errorf("unknown flag %s", a)
			}
			f.pos = append(f.pos, a)
		}
	}
	return f, nil
}

// canonicalFlag is the name changeFlags lists a flag under: -m is --message.
func canonicalFlag(flag string) string {
	if flag == "-m" {
		return "--message"
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
	case "job":
		if len(pos) == 2 && sub == "run" {
			return "job run", true
		}
	case "backup":
		if len(pos) == 1 && sub == "list" {
			return "backup list", true
		}
		if len(pos) == 2 && sub == "run" {
			return "backup run", true
		}
	case "secret":
		if len(pos) == 1 && sub == "list" {
			return "secret list", true
		}
		if len(pos) == 2 && sub == "set" {
			return "secret set", true
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

func runDeployment(cmd string, args []string, out, errout io.Writer) int {
	f, err := parseDeploymentArgs(args)
	if err != nil {
		fmt.Fprintln(errout, err)
		return 2
	}
	if !envPattern.MatchString(f.env) {
		fmt.Fprintln(errout, "valid -e environment required")
		return 2
	}
	if f.volumes && cmd != "remove" && cmd != "destroy" {
		fmt.Fprintln(errout, "--volumes applies to remove and destroy only")
		return 2
	}
	if f.all && cmd != "approvals" {
		fmt.Fprintln(errout, "--all applies to approvals only")
		return 2
	}
	if (f.tunnel || f.confirm != "") && cmd != "destroy" {
		fmt.Fprintln(errout, "--tunnel and --confirm apply to destroy only")
		return 2
	}
	if cmd == "destroy" && (len(f.pos) != 0 || f.confirm != f.env) {
		fmt.Fprintln(errout, "destroy takes no app, and --confirm must repeat the environment name")
		return 2
	}
	if (f.backup != "" && cmd != "backup") || (f.into != "" && cmd != "restore") {
		fmt.Fprintln(errout, "--backup applies to backup list, and --into to restore")
		return 2
	}
	if flag, key := refusedFlag(cmd, f); flag != "" {
		fmt.Fprintf(errout, "%s is not a flag of %s\n", flag, key)
		return 2
	}
	if cmd == "config" {
		if len(f.pos) != 1 || (f.pos[0] != "show" && f.pos[0] != "validate") {
			fmt.Fprintln(errout, "config requires show or validate")
			return 2
		}
		m, err := manifest.Load(f.dir, f.env)
		if err != nil {
			fmt.Fprintln(errout, err)
			return 1
		}
		if f.pos[0] == "validate" {
			fmt.Fprintln(out, "manifest valid")
			return 0
		}
		b, _ := json.Marshal(m)
		return printResponse(out, errout, b, f.json)
	}
	if f.config == "" {
		fmt.Fprintln(errout, "remote commands require --config")
		return 2
	}
	target, err := agentclient.Load(f.config, f.env)
	if err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	u, token, prefix := target.Base, target.Token, target.Prefix
	if cmd == "approvals" || cmd == "approve" || cmd == "reject" {
		return runApprovalCommand(cmd, f, u, prefix, token, out, errout)
	}
	if cmd == "job" {
		return runJobCommand(f, u, prefix, token, out, errout)
	}
	if cmd == "backup" {
		return runBackupCommand(f, u, prefix, token, out, errout)
	}
	if cmd == "state-backup" {
		return runStateBackupCommand(f, u, prefix, token, out, errout)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	client := agentclient.NewHTTPClient()
	method := "GET"
	endpoint := ""
	var body any
	switch cmd {
	case "deploy", "rollback", "restart", "scale", "exec", "prune", "remove", "destroy":
		if cmd == "deploy" && len(f.pos) == 0 {
			return runDeployAll(f, u, prefix, token, out, errout)
		}
		if cmd != "prune" && cmd != "destroy" && len(f.pos) != 1 || (cmd == "prune" || cmd == "destroy") && len(f.pos) != 0 {
			fmt.Fprintln(errout, "command requires an app")
			return 2
		}
		if cmd == "deploy" && len(f.versions) > 1 {
			fmt.Fprintln(errout, "--version given more than once")
			return 2
		}
		if cmd == "deploy" && len(f.versions) == 1 && strings.Contains(f.versions[0], "=") {
			fmt.Fprintln(errout, "--version APP=REF is for deploy without an app")
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
		req.RevertSchema = f.revert
		req.ReplaceExisting = f.replace
		req.AllowRecreate = f.allowRecreate
		req.Volumes = f.volumes
		req.Command = f.command
		if f.id == "" {
			if req.RequestID, err = newRequestID(); err != nil {
				return 1
			}
		}
		if f.to != "" {
			req.TargetRelease, err = strconv.ParseInt(f.to, 10, 64)
			if err != nil || req.TargetRelease < 1 {
				fmt.Fprintln(errout, "invalid --to")
				return 2
			}
		}
		if f.replicas != "" {
			v, e := strconv.Atoi(f.replicas)
			if e != nil {
				fmt.Fprintln(errout, "invalid --replicas")
				return 2
			}
			if v < 1 {
				fmt.Fprintln(errout, "invalid --replicas: must be at least 1")
				return 2
			}
			req.Replicas = &v
		}
		if cmd == "scale" && req.Replicas == nil {
			fmt.Fprintln(errout, "scale requires --replicas")
			return 2
		}
		if cmd == "exec" && len(req.Command) == 0 {
			fmt.Fprintln(errout, "exec requires a command after --")
			return 2
		}
		if cmd == "deploy" {
			if e := agentclient.PrepareDeploy(f.dir, f.env, &req); e != nil {
				switch {
				case errors.Is(e, agentclient.ErrVersionRequired):
					fmt.Fprintln(errout, "deploy requires --version")
					return 2
				case errors.Is(e, agentclient.ErrVersionNotAllowed):
					fmt.Fprintln(errout, "--version does not apply to a compose app")
					return 2
				}
				fmt.Fprintln(errout, e)
				return 1
			}
		}
		method = "POST"
		endpoint = cmd
		if cmd == "deploy" {
			endpoint = "deploys"
		}
		body = req
	case "restore":
		if len(f.pos) != 1 || f.pos[0] == "" || f.into == "" {
			fmt.Fprintln(errout, "usage: restore KEY --into VOLUME [--dry-run]")
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
		body = map[string]any{"key": f.pos[0], "into": f.into, "dry_run": f.dry, "request_id": id}
	case "history", "wait":
		if len(f.pos) != 1 {
			fmt.Fprintln(errout, "command requires an app")
			return 2
		}
		endpoint = cmd + "/" + url.PathEscape(f.pos[0])
	case "diagnostics":
		if len(f.pos) != 1 || f.pos[0] == "" {
			fmt.Fprintln(errout, "usage: diagnostics OPERATION_ID -e ENV [--json]")
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
				fmt.Fprintln(errout, "lock acquire requires -m")
				return 2
			}
			endpoint = "lock/" + f.pos[0]
			method = "POST"
			body = map[string]any{"message": f.message, "dry_run": f.dry}
		} else {
			fmt.Fprintln(errout, "invalid lock command")
			return 2
		}
	case "recovery":
		if len(f.pos) != 2 || f.pos[0] != "resolve" || f.pos[1] == "" {
			fmt.Fprintln(errout, "usage: recovery resolve OPERATION_ID -m REASON")
			return 2
		}
		if f.message == "" {
			fmt.Fprintln(errout, "recovery resolve requires -m")
			return 2
		}
		endpoint = "operations/" + url.PathEscape(f.pos[1]) + "/resolve"
		method = "POST"
		body = map[string]any{"reason": f.message, "dry_run": f.dry}
	case "ingress":
		if len(f.pos) != 1 || (f.pos[0] != "apply" && f.pos[0] != "status") {
			fmt.Fprintln(errout, "usage: ingress apply|status")
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
			req := deploy.Request{Manifest: *m, RequestID: f.id, DryRun: f.dry, ReplaceExisting: f.replace, Commit: f.commit}
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
			fmt.Fprintln(errout, "usage: alerts test")
			return 2
		}
		endpoint = "alerts/test"
		method = "POST"
	case "secret":
		if len(f.pos) == 1 && f.pos[0] == "list" {
			endpoint = "secrets"
		} else if len(f.pos) == 2 && (f.pos[0] == "set" || f.pos[0] == "delete" || f.pos[0] == "rm") {
			if f.pos[0] == "set" {
				stored := func() ([]storedSecret, bool) {
					return storedSecrets(ctx, client, agentURL(u, prefix, "secrets"), token, errout)
				}
				value, registry, code := secretValue(f, errout, stored)
				if code != 0 {
					return code
				}
				endpoint = "secrets"
				method = "POST"
				body = map[string]any{"name": f.pos[1], "value": value, "registry": registry, "dry_run": f.dry}
			} else {
				endpoint = "secrets/" + url.PathEscape(f.pos[1])
				method = "DELETE"
				body = map[string]any{"dry_run": f.dry}
			}
		} else {
			fmt.Fprintln(errout, "invalid secret command")
			return 2
		}
	}
	if cmd == "wait" {
		client = agentclient.WithTimeout(client, 11*time.Minute)
	}
	return sendCommand(ctx, client, u, prefix, token, method, endpoint, body, f, cmd, out, errout)
}

// sendCommand sends one change or read request to the agent and follows what the agent answers: a held approval, an
// operation to watch to its end, or a result to print. It is what runDeployment does once it has built the request. base is
// the agent's URL, which it does not change.
func sendCommand(ctx context.Context, client *http.Client, base *url.URL, prefix, token, method, endpoint string, body any, f commandFlags, cmd string, out, errout io.Writer) int {
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
	var payload []byte
	if body != nil {
		var err error
		payload, err = json.Marshal(body)
		if err != nil {
			return 1
		}
	}
	status, data, ok := askAgent(ctx, client, u, token, method, payload, errout)
	if !ok {
		return 1
	}
	if status == http.StatusAccepted {
		if code, held := reportHeld(data, out, errout, f.json); held {
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
		return watchOperation(ctx, client, u, token, prefix, accepted.ID, accepted.Events, out, errout, f.json, cmd, f.env)
	}
	if status != http.StatusOK {
		fmt.Fprintln(errout, agentclient.Message(status, data))
		return 1
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
	if cmd == "secret" && f.pos[0] == "set" && !f.dry && !f.json {
		if printSecretSet(out, data) {
			return 0
		}
	}
	return printResponse(out, errout, data, f.json)
}

// agentURL returns base with the path of endpoint under prefix. base is not changed.
func agentURL(base *url.URL, prefix, endpoint string) *url.URL {
	copied := *base
	copied.Path = prefix + endpoint
	return &copied
}

// askAgent sends one request to the agent at u with the token, with payload as its body when it is not nil, and reads the
// answer. When the agent cannot be reached or its answer cannot be read, it prints why and returns ok false.
func askAgent(ctx context.Context, client *http.Client, u *url.URL, token, method string, payload []byte, errout io.Writer) (status int, data []byte, ok bool) {
	resp, err := agentclient.Send(ctx, client, method, u.String(), token, payload)
	if err != nil {
		fmt.Fprintln(errout, "agent connection failed")
		return 0, nil, false
	}
	defer resp.Body.Close()
	data, err = io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err != nil || len(data) > 1<<20 {
		fmt.Fprintln(errout, "invalid agent response")
		return 0, nil, false
	}
	return resp.StatusCode, data, true
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
		printText(out, v, "")
	}
	return 0
}

// reportsSteps says whether the command's operation reports what it did as steps.
func reportsSteps(cmd string) bool { return cmd == "remove" || cmd == "destroy" || cmd == "restore" }

func watchOperation(ctx context.Context, c *http.Client, base *url.URL, token, prefix, id, events string, out, errout io.Writer, asJSON bool, cmd, env string) int {
	cursor := ""
	failures := 0
	for {
		streamURL := *base
		streamURL.Path = events
		streamURL.RawQuery = ""
		streamCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		req, _ := http.NewRequestWithContext(streamCtx, "GET", streamURL.String(), nil)
		req.Header.Set("Authorization", "Bearer "+token)
		if cursor != "" {
			req.Header.Set("Last-Event-ID", cursor)
		}
		resp, err := c.Do(req)
		if err == nil && resp.StatusCode == 200 {
			scanner := bufio.NewScanner(resp.Body)
			scanner.Buffer(make([]byte, 4096), 1<<20)
			kind := ""
			for scanner.Scan() {
				line := scanner.Text()
				if strings.HasPrefix(line, "id:") {
					cursor = strings.TrimSpace(strings.TrimPrefix(line, "id:"))
				}
				if strings.HasPrefix(line, "event:") {
					kind = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
					// A removal or a restore reports what it did as steps, so their text is shown instead of their name.
					if !asJSON && !(reportsSteps(cmd) && kind == "step") {
						fmt.Fprintln(out, kind)
					}
				}
				if reportsSteps(cmd) && kind == "step" && !asJSON && strings.HasPrefix(line, "data:") {
					var step struct {
						Step string `json:"step"`
					}
					if json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &step) == nil && step.Step != "" {
						fmt.Fprintln(out, "  "+terminalSafe(step.Step))
					}
				}
			}
			resp.Body.Close()
		} else if resp != nil {
			resp.Body.Close()
		}
		cancel()
		statusURL := *base
		statusURL.Path = prefix + "operations/" + url.PathEscape(id)
		statusURL.RawQuery = ""
		status, e := agentclient.Send(ctx, c, "GET", statusURL.String(), token, nil)
		validStatus := false
		if e == nil {
			data, e := io.ReadAll(io.LimitReader(status.Body, 1<<20))
			status.Body.Close()
			if e == nil && status.StatusCode == 200 {
				validStatus = true
				var v struct {
					State string `json:"state"`
				}
				if json.Unmarshal(data, &v) == nil && v.State != "running" && v.State != "" {
					exitCode := 0
					if cmd == "exec" {
						outputURL := *base
						outputURL.Path = prefix + "operations/" + url.PathEscape(id) + "/output"
						outputURL.RawQuery = ""
						output, e := agentclient.Send(ctx, c, "GET", outputURL.String(), token, nil)
						if e != nil {
							fmt.Fprintln(errout, "exec output unavailable")
							return 1
						}
						b, e := io.ReadAll(io.LimitReader(output.Body, (1<<20)+1))
						output.Body.Close()
						var result struct {
							Output   string `json:"output"`
							ExitCode int    `json:"exit_code"`
						}
						if e != nil || len(b) > 1<<20 || output.StatusCode != 200 || json.Unmarshal(b, &result) != nil {
							fmt.Fprintln(errout, "exec output unavailable")
							return 1
						}
						exitCode = result.ExitCode
						if asJSON {
							var resultObject map[string]any
							if json.Unmarshal(data, &resultObject) != nil {
								return 1
							}
							resultObject["output"] = result.Output
							resultObject["exit_code"] = result.ExitCode
							data, _ = json.Marshal(resultObject)
						} else {
							fmt.Fprint(out, result.Output)
						}
					}
					printResponse(out, errout, data, asJSON)
					if v.State == "succeeded" && exitCode == 0 {
						return 0
					}
					if (v.State == "failed" || v.State == "rolled_back" || v.State == "recovery_required") && (cmd == "deploy" || cmd == "rollback" || cmd == "ingress") {
						// stdout stays one JSON document under --json, so the pointer goes to stderr then.
						hint := out
						if asJSON {
							hint = errout
						}
						fmt.Fprintln(hint, failureHint(env, id))
					}
					return 1
				}
			}
		}
		if validStatus {
			failures = 0
		} else {
			failures++
			if failures >= 3 {
				fmt.Fprintln(errout, "operation status unavailable")
				return 1
			}
		}
		if ctx.Err() != nil {
			fmt.Fprintln(errout, "operation status unavailable")
			return 1
		}
		select {
		case <-time.After(time.Second):
		case <-ctx.Done():
			return 1
		}
	}
}
