package cli

import (
	"berthy/internal/agent"
	"berthy/internal/api"
	"berthy/internal/deploy"
	"berthy/internal/engine"
	"berthy/internal/logs"
	"berthy/internal/metrics"
	"berthy/internal/state"
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
	"strings"
	"syscall"
	"time"
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
		fmt.Fprintln(errout, "usage: berthy <agent|ps|describe|logs|stats|df|doctor|agent-info>")
		return 2
	}
	cmd := args[0]
	args = args[1:]
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
			stopped := make(chan struct{})
			go func() {
				defer close(stopped)
				retryRecovery(ctx, 10*time.Second, deployment.Store.Active, deployment.Recover)
			}()
			defer func() { cancel(); <-stopped }()
		}
		collector := metrics.New(metrics.NewHostReader(), eng)
		go collector.Run(ctx)
		server := http.Server{Addr: c.Listen, Handler: agent.NewDeploymentHandler(c, eng, collector, deployment), ReadHeaderTimeout: 5 * time.Second}
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
		fmt.Fprintf(errout, "agent returned HTTP %d\n", resp.StatusCode)
		return 1
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
