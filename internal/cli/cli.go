package cli

import (
	"berthy/internal/agent"
	"berthy/internal/api"
	"berthy/internal/deploy"
	"berthy/internal/engine"
	"berthy/internal/logs"
	"berthy/internal/metrics"
	"berthy/internal/monitor"
	"berthy/internal/probehelper"
	"berthy/internal/state"
	"berthy/internal/volarchive"
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"
	"unicode"
	"unicode/utf8"
)

type Config struct {
	Environments map[string]struct {
		URL      string `json:"url"`
		TokenEnv string `json:"token_env"`
	} `json:"environments"`
}

var envPattern = regexp.MustCompile(`^[a-z0-9-]+$`)

func Run(args []string, out, errout io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(errout, "usage: berthy <agent|ps|describe|logs|stats|df|doctor|checks|jobs|job|agent-info>")
		return 2
	}
	cmd := args[0]
	args = args[1:]
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
	if isDeploymentCommand(cmd) {
		return runDeployment(cmd, args, out, errout)
	}
	if cmd == "agent" && len(args) == 3 && args[0] == "token" && args[1] == "-e" {
		if !envPattern.MatchString(args[2]) {
			fmt.Fprintln(errout, "invalid environment")
			return 2
		}
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			fmt.Fprintln(errout, err)
			return 1
		}
		token := "bty." + args[2] + "." + hex.EncodeToString(b)
		fmt.Fprintf(out, "token: %s\nsha256: %s\n", token, agent.Digest(token))
		return 0
	}
	var config, env string
	var jsonOutput, watch bool
	var since, tail, grep, role, replica string
	var follow bool
	var positional []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--config", "-e":
			if i+1 == len(args) {
				fmt.Fprintln(errout, "missing flag value")
				return 2
			}
			i++
			if args[i-1] == "--config" {
				config = args[i]
			} else {
				env = args[i]
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
		c, err := agent.Load(config)
		if err != nil {
			fmt.Fprintln(errout, err)
			return 1
		}
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		eng := engine.New(c.DockerSocket)
		deployment, err := agent.OpenDeployment(c, eng)
		if err != nil {
			fmt.Fprintln(errout, err)
			return 1
		}
		if deployment != nil {
			defer deployment.Store.Close()
			recoveryCtx, done := context.WithTimeout(ctx, 30*time.Second)
			err = deployment.Recover(recoveryCtx)
			done()
			if err != nil && !errors.Is(err, deploy.ErrRecoveryPending) {
				fmt.Fprintln(errout, "deployment recovery failed")
				return 1
			}
			// Recover reports the first move into recovery_required as success, so the lock is checked as well.
			if op, activeErr := deployment.Store.Active(ctx); activeErr == nil && op.State == "recovery_required" {
				fmt.Fprintln(errout, recoveryNotice(op.ID))
			} else if errors.Is(err, deploy.ErrRecoveryPending) {
				fmt.Fprintln(errout, recoveryNotice(""))
			}
			stopped := make(chan struct{})
			go func() {
				defer close(stopped)
				retryRecovery(ctx, 10*time.Second, deployment.Store.Active, deployment.Recover)
			}()
			defer func() { cancel(); <-stopped }()
		}
		collector := metrics.New(metrics.NewHostReader(), eng)
		go collector.Run(ctx)
		monitoring := agent.NewMonitoring(c, eng, collector, deployment, func(msg string) { fmt.Fprintln(errout, msg) })
		if monitoring != nil {
			monitorDone := make(chan struct{})
			go func() {
				defer close(monitorDone)
				monitoring.Run(ctx)
			}()
			// Registered after the store's Close, so it runs first: the monitor stops before the store closes.
			defer func() { cancel(); <-monitorDone }()
		}
		scheduler := agent.NewJobs(c, eng, deployment, func(msg string) { fmt.Fprintln(errout, msg) })
		if scheduler != nil {
			jobsDone := make(chan struct{})
			go func() {
				defer close(jobsDone)
				scheduler.Run(ctx)
			}()
			// Registered after the store's Close, so it runs first: the runs end and are recorded before the store closes.
			defer func() { cancel(); <-jobsDone }()
		}
		backups := agent.NewBackups(c, eng, deployment, scheduler, monitoring.HelperImage, func(msg string) { fmt.Fprintln(errout, msg) })
		if backups != nil {
			backups.Start(ctx)
			// Registered after the store's Close, so it runs first: the runs end and are recorded before the store closes.
			defer func() { cancel(); backups.Wait() }()
		}
		exporter := agent.NewTelemetry(c, collector, deployment, func(msg string) { fmt.Fprintln(errout, msg) })
		if exporter != nil {
			exportDone := make(chan struct{})
			go func() {
				defer close(exportDone)
				exporter.Run(ctx)
			}()
			// Registered after the monitor's stop, so the last export is flushed before the store closes.
			defer func() { cancel(); <-exportDone }()
		}
		server := http.Server{Addr: c.Listen, Handler: agent.NewDeploymentHandlerWithBackups(c, eng, collector, deployment, exporter, scheduler, backups), ReadHeaderTimeout: 5 * time.Second}
		go func() {
			<-ctx.Done()
			shutdownCtx, done := context.WithTimeout(context.Background(), 5*time.Second)
			defer done()
			_ = server.Shutdown(shutdownCtx)
		}()
		if err = server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintln(errout, err)
			return 1
		}
		return 0
	}
	if env == "" || config == "" {
		fmt.Fprintln(errout, "remote commands require -e and --config")
		return 2
	}
	b, err := os.ReadFile(config)
	if err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	var c Config
	decConfig := json.NewDecoder(bytes.NewReader(b))
	decConfig.DisallowUnknownFields()
	if err = decConfig.Decode(&c); err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	if err = decConfig.Decode(&struct{}{}); err != io.EOF {
		fmt.Fprintln(errout, "invalid trailing JSON")
		return 1
	}
	target, ok := c.Environments[env]
	if !ok || target.TokenEnv == "" {
		fmt.Fprintln(errout, "unknown environment or missing token_env")
		return 1
	}
	token := os.Getenv(target.TokenEnv)
	if token == "" {
		fmt.Fprintln(errout, "token environment variable is empty")
		return 1
	}
	u, err := url.Parse(target.URL)
	if err != nil || u.Scheme != "http" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		fmt.Fprintln(errout, "invalid agent URL")
		return 1
	}
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
	u.Path = strings.TrimRight(u.Path, "/") + "/v1/environments/" + url.PathEscape(env) + "/" + endpoint
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
	req, err := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
	if err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	req.Header.Set("Authorization", "Bearer "+token)
	client := http.Client{Transport: &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 5 * time.Second}).DialContext, ResponseHeaderTimeout: 20 * time.Second}, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	if watch || follow {
		client.Timeout = 0
	}
	resp, err := client.Do(req)
	if err != nil {
		fmt.Fprintln(errout, "agent connection failed")
		return 1
	}
	defer resp.Body.Close()
	if (watch || follow) && resp.StatusCode == 200 {
		return readStream(ctx, resp.Body, out, errout, jsonOutput)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		fmt.Fprintln(errout, "agent response could not be read")
		return 1
	}
	if resp.StatusCode != 200 {
		fmt.Fprintln(errout, agentError(resp.StatusCode, body))
		return 1
	}
	if cmd == "checks" {
		return printChecks(out, errout, body, jsonOutput)
	}
	if cmd == "jobs" {
		return printJobs(out, errout, body, jsonOutput)
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
		printText(out, v, "")
	}

	if cmd == "doctor" {
		var d struct {
			Checks []api.Check `json:"checks"`
		}
		if json.Unmarshal(body, &d) != nil {
			return 1
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

func retryRecovery(ctx context.Context, interval time.Duration, active func(context.Context) (state.Operation, error), recover func(context.Context) error) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			attempt, done := context.WithTimeout(ctx, 30*time.Second)
			op, err := active(attempt)
			if err == nil && op.State == "recovery_required" {
				_ = recover(attempt)
			}
			done()
		}
	}
}

// recoveryNotice is the line printed at startup while an operation waits for recovery. id is empty when it is unknown.
func recoveryNotice(id string) string {
	if id == "" {
		id = "OPERATION_ID"
	}
	return fmt.Sprintf(`deployment recovery required; check "berthy lock status" and resolve with "berthy recovery resolve %s -m REASON"`, id)
}
func printText(w io.Writer, v any, indent string) {
	switch x := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			v := x[k]
			fmt.Fprintf(w, "%s%s: ", indent, k)
			switch v.(type) {
			case map[string]any, []any:
				fmt.Fprintln(w)
				printText(w, v, indent+"  ")
			default:
				fmt.Fprintln(w, v)
			}
		}
	case []any:
		for _, v := range x {
			printText(w, v, indent)
			fmt.Fprintln(w)
		}
	default:
		fmt.Fprintln(w, x)
	}
}

func readStream(ctx context.Context, r io.Reader, out, errout io.Writer, jsonOutput bool) int {
	scan := bufio.NewScanner(r)
	scan.Buffer(make([]byte, 4096), 16<<20)
	event := ""
	var data []string
	size := 0
	for scan.Scan() {
		line := scan.Text()
		if line == "" {
			if len(data) == 0 {
				event = ""
				continue
			}
			payload := strings.Join(data, "\n")
			data = nil
			size = 0
			if !json.Valid([]byte(payload)) || len(payload) == 0 || payload[0] != '{' {
				fmt.Fprintln(errout, "invalid stream event")
				return 1
			}
			switch event {
			case "done":
				var v struct {
					Complete bool `json:"complete"`
				}
				if json.Unmarshal([]byte(payload), &v) != nil || !v.Complete {
					fmt.Fprintln(errout, "invalid stream event")
					return 1
				}
				return 0
			case "error":
				fmt.Fprintln(errout, "agent stream failed")
				return 1
			case "log":
				var v logs.Record
				if json.Unmarshal([]byte(payload), &v) != nil {
					fmt.Fprintln(errout, "invalid stream event")
					return 1
				}
			case "stats":
				var v metrics.Sample
				if json.Unmarshal([]byte(payload), &v) != nil {
					fmt.Fprintln(errout, "invalid stream event")
					return 1
				}
			default:
				fmt.Fprintln(errout, "invalid stream event")
				return 1
			}
			event = ""
			var v any
			dec := json.NewDecoder(strings.NewReader(payload))
			dec.UseNumber()
			if dec.Decode(&v) != nil {
				fmt.Fprintln(errout, "invalid stream event")
				return 1
			}
			if jsonOutput {
				var compact bytes.Buffer
				if json.Compact(&compact, []byte(payload)) != nil {
					fmt.Fprintln(errout, "invalid stream event")
					return 1
				}
				fmt.Fprintln(out, compact.String())
			} else {
				printText(out, v, "")
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, ok := strings.Cut(line, ":")
		if !ok {
			value = ""
		}
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "event":
			event = value
		case "data":
			size += len(value) + 1
			if size > 16<<20 {
				fmt.Fprintln(errout, "invalid stream event")
				return 1
			}
			data = append(data, value)
		}
	}
	if ctx.Err() != nil {
		// The user interrupted (Ctrl-C) and the request context was canceled: a normal exit.
		return 0
	}
	if scan.Err() != nil {
		fmt.Fprintln(errout, "agent stream could not be read")
	} else {
		fmt.Fprintln(errout, "agent stream ended unexpectedly")
	}
	return 1
}

// agentError describes a refused agent request. The agent message is shown only when it is safe to print.
func agentError(status int, body []byte) string {
	var reply struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &reply) == nil && printableMessage(reply.Message) {
		return fmt.Sprintf("agent returned HTTP %d: %s", status, reply.Message)
	}
	return fmt.Sprintf("agent returned HTTP %d", status)
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
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", r.Name, r.State, r.Since.UTC().Format(time.RFC3339), r.Detail)
	}
	if err := tw.Flush(); err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	return 0
}

// printableMessage reports whether s is at most 300 bytes of valid UTF-8 without control characters.
func printableMessage(s string) bool {
	if s == "" || len(s) > 300 || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}
