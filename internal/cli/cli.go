package cli

import (
	"berthy/internal/agent"
	"berthy/internal/agentclient"
	"berthy/internal/api"
	"berthy/internal/engine"
	"berthy/internal/logs"
	"berthy/internal/manifest"
	"berthy/internal/mcp"
	"berthy/internal/metrics"
	"berthy/internal/monitor"
	"berthy/internal/probehelper"
	"berthy/internal/scope"
	"berthy/internal/tui"
	"berthy/internal/version"
	"berthy/internal/volarchive"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"
)

func Run(args []string, out, errout io.Writer) int { return runIn(args, out, errout, ".", nil) }

// runIn is Run for a command line that may have come from an alias. Expanded commands inherit the alias's manifest directory.
// intent is non-nil only for an expanded alias: the dispatcher checks the real parsers against it before resolving a target.
func runIn(args []string, out, errout io.Writer, dir string, intent *aliasIntent) int {
	if len(args) == 0 {
		fmt.Fprintln(errout, "usage: berthy <setup|context|agent|version|docs|ps|describe|logs|stats|df|doctor|checks|jobs|job|agent-info|mcp|ui>")
		return 2
	}
	cmd := args[0]
	args = args[1:]
	if !slices.Contains(builtinCommands, cmd) && !strings.HasPrefix(cmd, "-") {
		// An alias is expanded before any flag is checked. expandAlias returns no error only for an alias, so the expanded
		// line starts with a built-in command and is not expanded again.
		expanded, aliasDir, aliasSel, isAlias, err := expandAlias(cmd, args)
		if err != nil {
			fmt.Fprintln(errout, err)
			if isAlias {
				return 1
			}
			return 2
		}
		return runIn(expanded, out, errout, aliasDir, aliasSel)
	}
	if cmd == "version" {
		// Local: it prints this build's version and needs no agent or config.
		return runVersion(args, out, errout)
	}
	if cmd == "docs" {
		// Local: it prints the manifest reference built into this binary and needs no agent, manifest or config.
		return runDocs(args, out, errout)
	}
	if cmd == "config" && len(args) > 0 && args[0] == "schema" {
		// Local too, and the same for every environment, so it takes no -e.
		return runConfigSchema(args[1:], out, errout)
	}
	if cmd == "probe-helper" {
		// Not in the usage: only the overlay helper container runs it.
		return runProbeHelper(errout)
	}
	switch cmd {
	case "volume-archive":
		// Not in the usage either: only the backup container runs it, with the volume mounted at /source.
		return runVolumeArchive(volarchive.SourceDir, out, errout)
	case "volume-extract":
		// Only the restore container runs it, with the new volume at /target and the archive on stdin.
		return runVolumeExtract(os.Stdin, volarchive.TargetDir, errout)
	}
	if cmd == "mcp" {
		// It serves MCP on stdin and stdout, so nothing else may be printed to out.
		return mcp.Run(args, os.Stdin, out, errout)
	}
	if cmd == "ui" {
		// It takes over the terminal: it needs the real standard input and output.
		return tui.Run(args, os.Stdin, out, errout)
	}
	if cmd == "setup" {
		// Local to the operator: it reaches a server over ssh, or runs on it, and needs no agent or config.
		return runSetup(args, out, errout)
	}
	if cmd == "context" {
		// Local: it edits the CLI configuration on this machine and contacts no agent.
		return runContext(args, out, errout)
	}
	if cmd == "state-restore" {
		// Local: it reads a snapshot from object storage and needs no agent, so it runs when the server is gone.
		return runStateRestore(args, out, errout)
	}
	if intent == nil && cmd != "agent" {
		dir = manifest.DefaultDir()
	}
	if isDeploymentCommand(cmd) {
		return runDeployment(cmd, args, out, errout, dir, intent)
	}
	if cmd == "agent" && len(args) > 0 && args[0] == "key-wrap" {
		return runKeyWrap(args[1:], os.Stdin, out, errout)
	}
	if cmd == "agent" && len(args) > 0 && args[0] == "token" {
		return runAgentToken(args[1:], out, errout)
	}
	var config, env, project string
	var jsonOutput, watch, projectGiven, configGiven, envGiven bool
	var since, tail, grep, role, replica string
	var follow bool
	var positional []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--config", "--project", "-e":
			if i+1 == len(args) {
				fmt.Fprintln(errout, "missing flag value")
				return 2
			}
			i++
			if args[i-1] == "--config" {
				config = args[i]
				configGiven = true
			} else if args[i-1] == "--project" {
				project = args[i]
				projectGiven = true
			} else {
				env = args[i]
				envGiven = true
			}
		case "--follow", "-f":
			follow = true
		case "--tail", "--grep", "--role", "--replica":
			if i+1 == len(args) {
				fmt.Fprintln(errout, "missing flag value")
				return 2
			}
			flag := args[i]
			i++
			switch flag {
			case "--tail":
				tail = args[i]
			case "--grep":
				grep = args[i]
			case "--role":
				role = args[i]
			case "--replica":
				replica = args[i]
			}
		case "--watch":
			watch = true
		case "--since":
			if i+1 == len(args) {
				fmt.Fprintln(errout, "missing since duration")
				return 2
			}
			i++
			since = args[i]
		case "--json":
			jsonOutput = true
		default:
			if strings.HasPrefix(args[i], "-") {
				fmt.Fprintf(errout, "unknown flag %s\n", args[i])
				return 2
			}
			positional = append(positional, args[i])
		}
	}
	if cmd == "agent" {
		if runtime.GOOS != "linux" {
			fmt.Fprintln(errout, "agent requires Linux")
			return 1
		}
		if config == "" {
			fmt.Fprintln(errout, "agent requires --config")
			return 2
		}
		f, err := agent.LoadFile(config)
		if err != nil {
			fmt.Fprintln(errout, err)
			return 1
		}
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		eng := engine.New(f.DockerSocket)
		// The one reader of Docker's event feed. It wakes the monitor and the rollouts early; their own reads still decide.
		events := engine.NewEventHub(eng)
		eventsDone := make(chan struct{})
		go func() {
			defer close(eventsDone)
			events.Run(ctx)
		}()
		defer func() { cancel(); <-eventsDone }()
		collector := metrics.New(metrics.NewHostReader(), eng)
		// Every environment's state is open and claimed before any of them starts, and the server listens only after all started.
		host, err := agent.Open(agent.Shared{Engine: eng, Collector: collector, Log: func(msg string) { fmt.Fprintln(errout, msg) }}, f)
		if err != nil {
			fmt.Fprintln(errout, err)
			return 1
		}
		defer func() { cancel(); host.Close() }()
		go collector.Run(ctx)
		if err = host.Start(ctx); err != nil {
			fmt.Fprintln(errout, err)
			return 1
		}
		server := http.Server{Addr: f.Listen, Handler: host, ReadHeaderTimeout: 5 * time.Second}
		shutdown := make(chan struct{})
		go func() {
			defer close(shutdown)
			<-ctx.Done()
			shutdownCtx, done := context.WithTimeout(context.Background(), 5*time.Second)
			defer done()
			// http.Server.Shutdown does not track a connection a handler took over, so the open sessions are closed here: the containers
			// of run sessions are stopped and removed before the agent exits.
			host.CloseSessions(shutdownCtx)
			_ = server.Shutdown(shutdownCtx)
		}()
		fmt.Fprintf(errout, "berthy agent %s listening on %s\n", version.String(), f.Listen)
		if err = server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintln(errout, err)
			return 1
		}
		<-shutdown
		return 0
	}
	env, envFromVariable := agentclient.EnvironmentName(env, envGiven)
	if env == "" {
		fmt.Fprintln(errout, "remote commands require -e")
		return 2
	}
	if !scope.ValidEnvironment(env) {
		if envFromVariable {
			fmt.Fprintln(errout, "BERTHY_ENV is not a valid environment")
			return 2
		}
		fmt.Fprintln(errout, "valid -e environment required")
		return 2
	}
	if projectGiven && !scope.ValidProject(project) {
		fmt.Fprintln(errout, "valid --project required")
		return 2
	}
	if code := checkAliasIntent(errout, intent, env, project, config, dir, projectGiven, configGiven, false); code != 0 {
		return code
	}
	path, err := agentclient.ConfigPath(config)
	if err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	target, err := agentclient.Resolve(path, agentclient.Selector{Project: project, Environment: env, ManifestDir: dir})
	if err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	u, token := *target.Base, target.Token
	var endpoint string
	switch cmd {
	case "ps":
		endpoint = "apps"
	case "describe":
		if len(positional) != 1 {
			fmt.Fprintln(errout, "describe requires an app")
			return 2
		}
		endpoint = "apps/" + url.PathEscape(positional[0])
	case "stats":
		endpoint = "stats"
	case "logs":
		if len(positional) != 1 {
			fmt.Fprintln(errout, "logs requires an app")
			return 2
		}
		endpoint = "apps/" + url.PathEscape(positional[0]) + "/logs"
	case "df", "doctor":
		endpoint = cmd
	case "checks", "jobs":
		endpoint = cmd
	case "agent-info":
		endpoint = "agent"
	default:
		fmt.Fprintln(errout, "unknown command")
		return 2
	}
	u.Path = target.Prefix + endpoint
	if cmd == "stats" {
		q := u.Query()
		if watch {
			q.Set("watch", "1")
		}
		if since != "" {
			q.Set("since", since)
		}
		u.RawQuery = q.Encode()
	}
	if cmd == "logs" {
		q := u.Query()
		if follow {
			q.Set("follow", "1")
		}
		if tail != "" {
			q.Set("tail", tail)
		}
		if since != "" {
			q.Set("since", since)
		}
		if grep != "" {
			q.Set("grep", grep)
		}
		if role != "" {
			q.Set("role", role)
		}
		if replica != "" {
			q.Set("replica", replica)
		}
		u.RawQuery = q.Encode()
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	client := http.Client{Transport: &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 5 * time.Second}).DialContext, ResponseHeaderTimeout: 20 * time.Second}, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	if watch || follow {
		client.Timeout = 0
	}
	return runReadRequest(ctx, &client, &u, token, cmd, watch || follow, jsonOutput, out, errout)
}

func runReadRequest(ctx context.Context, client *http.Client, u *url.URL, token, cmd string, stream, jsonOutput bool, out, errout io.Writer) int {
	progress := startWait(errout, !jsonOutput && !stream, "reading "+cmd)
	resp, err := agentclient.Send(ctx, client, "GET", u.String(), token, nil)
	if err != nil {
		progress.Close()
		fmt.Fprintln(errout, "agent connection failed")
		return 1
	}
	defer resp.Body.Close()
	if stream && resp.StatusCode == 200 {
		progress.Close()
		return readStream(ctx, resp.Body, out, errout, jsonOutput)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	progress.Close()
	if err != nil {
		fmt.Fprintln(errout, "agent response could not be read")
		return 1
	}
	if resp.StatusCode != 200 {
		fmt.Fprintln(errout, agentclient.Message(resp.StatusCode, body))
		return 1
	}
	if cmd == "checks" {
		return printChecks(out, errout, body, jsonOutput)
	}
	if cmd == "jobs" {
		return printJobs(out, errout, body, jsonOutput)
	}
	if cmd == "logs" && !jsonOutput {
		return printLogLines(out, errout, body)
	}
	if jsonOutput {
		var pretty bytes.Buffer
		if json.Indent(&pretty, body, "", "  ") != nil {
			fmt.Fprintln(errout, "invalid agent response")
			return 1
		}
		fmt.Fprintln(out, pretty.String())
	} else {
		var v any
		dec := json.NewDecoder(bytes.NewReader(body))
		dec.UseNumber()
		if dec.Decode(&v) != nil {
			fmt.Fprintln(errout, "invalid agent response")
			return 1
		}
		if cmd == "doctor" {
			agentVersion, known := doctorAgentVersion(body)
			if !known {
				agentVersion = "unknown"
			}
			fmt.Fprintf(out, "cli: %s\nagent: %s\n", version.String(), agentVersion)
		}
		shape := shapeGeneric
		switch cmd {
		case "doctor":
			shape = shapeDoctor
		case "ps":
			shape = shapeApps
		case "describe":
			shape = shapeApp
		case "df":
			shape = shapeDF
		case "stats":
			shape = shapeStats
		}
		printStyledText(out, v, "", styleFor(out, true), shape)
	}

	if cmd == "doctor" {
		var d struct {
			Checks []api.Check `json:"checks"`
		}
		if json.Unmarshal(body, &d) != nil {
			return 1
		}
		agentVersion, _ := doctorAgentVersion(body)
		if note := versionNote(agentVersion); note != "" {
			// stdout stays one JSON document under --json, so the note goes to stderr then.
			noteOut := out
			if jsonOutput {
				noteOut = errout
			}
			fmt.Fprintln(noteOut, note)
		}
		for _, ch := range d.Checks {
			if ch.Critical && !ch.OK {
				return 1
			}
		}
	}
	return 0
}

// runProbeHelper serves the overlay helper's probes until it is stopped. Its token comes from the environment, and the
// process never prints it.
func runProbeHelper(errout io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := probehelper.Serve(ctx, net.JoinHostPort("0.0.0.0", strconv.Itoa(monitor.HelperPort)), os.Getenv(monitor.HelperTokenEnv)); err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	return 0
}

type textShape uint8

const (
	shapeGeneric textShape = iota
	shapeOperation
	shapeDoctor
	shapeChecks
	shapeCheck
	shapeApps
	shapeApp
	shapeRoles
	shapeRole
	shapeReplicas
	shapeReplica
	shapeDF
	shapeWait
	shapeAudit
	shapeIngress
	shapeTunnel
	shapeStats
	shapeContainers
	shapeContainer
)

func printText(w io.Writer, v any, indent string) {
	printStyledText(w, v, indent, styleFor(w, true), shapeGeneric)
}

func printStyledText(w io.Writer, v any, indent string, s cliStyle, shape textShape) {
	switch x := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			v := x[k]
			fmt.Fprintf(w, "%s%s: ", indent, s.paint(toneMuted, terminalSafe(k)))
			child := shapeGeneric
			switch {
			case shape == shapeIngress && k == "tunnel":
				child = shapeTunnel
			case shape == shapeStats && k == "containers":
				child = shapeContainers
			case shape == shapeDoctor && k == "checks":
				if _, ok := v.([]any); ok {
					child = shapeChecks
				}
			case shape == shapeApp && k == "roles":
				child = shapeRoles
			case shape == shapeRole && k == "replicas":
				child = shapeReplicas
			}
			switch v.(type) {
			case map[string]any, []any:
				fmt.Fprintln(w)
				printStyledText(w, v, indent+"  ", s, child)
			default:
				value := terminalSafe(fmt.Sprint(v))
				tone := tonePlain
				switch {
				case shape == shapeOperation && k == "state", shape == shapeWait && k == "status", shape == shapeReplica && (k == "state" || k == "health"), shape == shapeDF && (k == "host_status" || k == "docker_status"), shape == shapeAudit && k == "outcome", shape == shapeTunnel && k == "status":
					tone = statusTone(fmt.Sprint(v))
				case (shape == shapeStats && k == "docker_error" || shape == shapeContainer && k == "error") && value != "":
					tone = toneError
				case shape == shapeOperation && (k == "error" || k == "reason") && value != "":
					tone = toneError
				case shape == shapeCheck && (k == "name" || k == "ok"):
					ok, valid := x["ok"].(bool)
					critical, validCritical := x["critical"].(bool)
					if valid && validCritical {
						if ok {
							tone = toneOK
						} else if critical {
							tone = toneError
						} else {
							tone = toneWarning
						}
					}
				}
				fmt.Fprintln(w, s.paint(tone, value))
			}
		}
	case []any:
		child := shape
		switch shape {
		case shapeChecks:
			child = shapeCheck
		case shapeApps:
			child = shapeApp
		case shapeRoles:
			child = shapeRole
		case shapeReplicas:
			child = shapeReplica
		case shapeContainers:
			child = shapeContainer
		}
		for _, v := range x {
			switch v.(type) {
			case map[string]any:
				printStyledText(w, v, indent, s, child)
				fmt.Fprintln(w)
			case []any:
				printStyledText(w, v, indent, s, shapeGeneric)
				fmt.Fprintln(w)
			default:
				fmt.Fprintf(w, "%s- %s\n", indent, terminalSafe(fmt.Sprint(v)))
			}
		}
	default:
		fmt.Fprintln(w, terminalSafe(fmt.Sprint(x)))
	}
}

// shortIDLength is how many characters of a container id a log line shows: the ones docker ps shows.
const shortIDLength = 12

// logLine is one log record as a line: time, source and message. The source is the role and the container's short id
// (the 12 characters docker ps and the container's host name show), so a line can be matched to the replica that answered.
// A stderr line is marked. Control characters in the message are made safe as everywhere else in the CLI (terminalSafe).
func logLine(r logs.Record) string {
	line := r.Timestamp.UTC().Format(time.RFC3339) + " " + logSource(r) + " "
	if r.Stream == "stderr" {
		line += "[stderr] "
	}
	return line + terminalSafe(r.Message)
}

// logSource is the role and the short container id of a record, or of its task id when no container id is known. A record
// with no role is from the app itself.
func logSource(r logs.Record) string {
	role := r.Role
	if role == "" {
		role = "app"
	}
	id := r.ContainerID
	if id == "" {
		id = r.Replica
	}
	if len(id) > shortIDLength {
		id = id[:shortIDLength]
	}
	if id == "" {
		return terminalSafe(role)
	}
	return terminalSafe(role + "." + id)
}

// printLogLines prints the records of a logs response, one line each.
func printLogLines(out, errout io.Writer, body []byte) int {
	var rows []logs.Record
	if json.Unmarshal(body, &rows) != nil {
		fmt.Fprintln(errout, "invalid agent response")
		return 1
	}
	for _, r := range rows {
		fmt.Fprintln(out, logLine(r))
	}
	return 0
}

func readStream(ctx context.Context, r io.Reader, out, errout io.Writer, jsonOutput bool) int {
	failed := false
	err := agentclient.ReadEvents(ctx, r, func(event, payload string) error {
		switch event {
		case "done":
			var v struct {
				Complete bool `json:"complete"`
			}
			if json.Unmarshal([]byte(payload), &v) != nil || !v.Complete {
				return agentclient.ErrInvalidEvent
			}
			return agentclient.ErrStopEvents
		case "error":
			failed = true
			return errors.New("agent stream failed")
		case "log":
			var v logs.Record
			if json.Unmarshal([]byte(payload), &v) != nil {
				return agentclient.ErrInvalidEvent
			}
			if !jsonOutput {
				fmt.Fprintln(out, logLine(v))
				return nil
			}
		case "stats":
			var v metrics.Sample
			if json.Unmarshal([]byte(payload), &v) != nil {
				return agentclient.ErrInvalidEvent
			}
		default:
			return agentclient.ErrInvalidEvent
		}
		var v any
		dec := json.NewDecoder(strings.NewReader(payload))
		dec.UseNumber()
		if dec.Decode(&v) != nil {
			return agentclient.ErrInvalidEvent
		}
		if jsonOutput {
			var compact bytes.Buffer
			if json.Compact(&compact, []byte(payload)) != nil {
				return agentclient.ErrInvalidEvent
			}
			fmt.Fprintln(out, compact.String())
		} else {
			printStyledText(out, v, "", styleFor(out, true), shapeStats)
		}
		return nil
	})
	switch {
	case err == nil:
		return 0
	case ctx.Err() != nil:
		// The user interrupted (Ctrl-C) and the request context was canceled: a normal exit.
		return 0
	case failed:
		fmt.Fprintln(errout, "agent stream failed")
	case errors.Is(err, agentclient.ErrInvalidEvent):
		fmt.Fprintln(errout, "invalid stream event")
	case errors.Is(err, agentclient.ErrStreamRead):
		fmt.Fprintln(errout, "agent stream could not be read")
	default:
		fmt.Fprintln(errout, "agent stream ended unexpectedly")
	}
	return 1
}

// printChecks prints the stored check states as a table, or as indented JSON when asJSON is set.
func printChecks(out, errout io.Writer, body []byte, asJSON bool) int {
	if asJSON {
		var pretty bytes.Buffer
		if json.Indent(&pretty, body, "", "  ") != nil {
			fmt.Fprintln(errout, "invalid agent response")
			return 1
		}
		fmt.Fprintln(out, pretty.String())
		return 0
	}
	var rows []struct {
		Name   string    `json:"name"`
		State  string    `json:"state"`
		Detail string    `json:"detail"`
		Since  time.Time `json:"since"`
	}
	if json.Unmarshal(body, &rows) != nil {
		fmt.Fprintln(errout, "invalid agent response")
		return 1
	}
	if len(rows) == 0 {
		fmt.Fprintln(out, "no checks yet")
		return 0
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tSTATE\tSINCE\tDETAIL")
	for _, r := range rows {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", terminalSafe(r.Name), terminalSafe(r.State), r.Since.UTC().Format(time.RFC3339), terminalSafe(r.Detail))
	}
	if err := tw.Flush(); err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	return 0
}
