package cli

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path"
	"strconv"
	"strings"
	"time"

	"berthy/internal/deploy"
	"berthy/internal/manifest"
)

func isDeploymentCommand(s string) bool {
	switch s {
	case "config", "deploy", "rollback", "restart", "scale", "exec", "prune", "wait", "history", "audit", "lock", "recovery", "secret", "ingress":
		return true
	}
	return false
}

type commandFlags struct {
	env, config, dir, version, commit, id, role, timeout, registry, message, to, replicas string
	json, dry, adopt, healthy, revert, replace                                            bool
	pos, command                                                                          []string
}

func parseDeploymentArgs(args []string) (commandFlags, error) {
	f := commandFlags{dir: "."}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			f.command = append(f.command, args[i+1:]...)
			break
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
		case "--revert-schema":
			f.revert = true
		case "-e", "--config", "--manifest-dir", "--version", "--commit", "--request-id", "--role", "--timeout", "--registry", "-m", "--message", "--to", "--replicas":
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
			case "--manifest-dir":
				f.dir = v
			case "--version":
				f.version = v
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
	b, err := os.ReadFile(f.config)
	if err != nil {
		fmt.Fprintln(errout, "cannot read config")
		return 1
	}
	var c Config
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if d.Decode(&c) != nil || d.Decode(&struct{}{}) != io.EOF {
		fmt.Fprintln(errout, "invalid config")
		return 1
	}
	target, ok := c.Environments[f.env]
	token := os.Getenv(target.TokenEnv)
	u, err := url.Parse(target.URL)
	if !ok || token == "" || err != nil || u.Scheme != "http" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		fmt.Fprintln(errout, "invalid agent configuration")
		return 1
	}
	prefix := strings.TrimRight(u.Path, "/") + "/v1/environments/" + url.PathEscape(f.env) + "/"
	method := "GET"
	endpoint := ""
	var body any
	switch cmd {
	case "deploy", "rollback", "restart", "scale", "exec", "prune":
		if cmd != "prune" && len(f.pos) != 1 || cmd == "prune" && len(f.pos) != 0 {
			fmt.Fprintln(errout, "command requires an app")
			return 2
		}
		var req deploy.Request
		if cmd != "prune" {
			req.App = f.pos[0]
		}
		req.Version = f.version
		req.Commit = f.commit
		req.RequestID = f.id
		req.DryRun = f.dry
		req.Adopt = f.adopt
		req.Role = f.role
		req.RevertSchema = f.revert
		req.ReplaceExisting = f.replace
		req.Command = f.command
		if cmd == "deploy" && f.version == "" {
			fmt.Fprintln(errout, "deploy requires --version")
			return 2
		}
		if f.id == "" {
			var random [16]byte
			if _, err = rand.Read(random[:]); err != nil {
				return 1
			}
			req.RequestID = hex.EncodeToString(random[:])
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
			if e != nil || v < 0 {
				fmt.Fprintln(errout, "invalid --replicas")
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
			m, e := manifest.Load(f.dir, f.env)
			if e != nil {
				fmt.Fprintln(errout, e)
				return 1
			}
			req.Manifest = *m
			if _, e = m.CheckImageDeploy(req.App, req.Version, false); e != nil {
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
	case "history", "wait":
		if len(f.pos) != 1 {
			fmt.Fprintln(errout, "command requires an app")
			return 2
		}
		endpoint = cmd + "/" + url.PathEscape(f.pos[0])
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
				var random [16]byte
				if _, err = rand.Read(random[:]); err != nil {
					return 1
				}
				req.RequestID = hex.EncodeToString(random[:])
			}
			method = "POST"
			endpoint = "ingress/apply"
			body = req
		}
	case "secret":
		if len(f.pos) == 1 && f.pos[0] == "list" {
			endpoint = "secrets"
		} else if len(f.pos) == 2 && (f.pos[0] == "set" || f.pos[0] == "delete" || f.pos[0] == "rm") {
			if f.pos[0] == "set" {
				data, e := io.ReadAll(io.LimitReader(os.Stdin, 65537))
				if e != nil || len(data) > 65536 {
					fmt.Fprintln(errout, "secret input unavailable or too large")
					return 1
				}
				endpoint = "secrets"
				method = "POST"
				body = map[string]any{"name": f.pos[1], "value": strings.TrimSuffix(string(data), "\n"), "registry": f.registry, "dry_run": f.dry}
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
	u.Path = prefix + endpoint
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
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	client := &http.Client{Transport: &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 5 * time.Second}).DialContext, ResponseHeaderTimeout: 20 * time.Second}, Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	if cmd == "wait" {
		client.Timeout = 11 * time.Minute
		client.Transport.(*http.Transport).ResponseHeaderTimeout = 11 * time.Minute
	}
	var payload []byte
	if body != nil {
		payload, err = json.Marshal(body)
		if err != nil {
			return 1
		}
	}
	resp, err := send(ctx, client, method, u.String(), token, payload)
	if err != nil {
		fmt.Fprintln(errout, "agent connection failed")
		return 1
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err != nil || len(data) > 1<<20 {
		fmt.Fprintln(errout, "invalid agent response")
		return 1
	}
	if resp.StatusCode == http.StatusAccepted {
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
		return watchOperation(ctx, client, u, token, prefix, accepted.ID, accepted.Events, out, errout, f.json, cmd == "exec")
	}
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(errout, agentError(resp.StatusCode, data))
		return 1
	}
	return printResponse(out, errout, data, f.json)
}
func send(ctx context.Context, c *http.Client, method, address, token string, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, address, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return c.Do(req)
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
func watchOperation(ctx context.Context, c *http.Client, base *url.URL, token, prefix, id, events string, out, errout io.Writer, asJSON, isExec bool) int {
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
					if !asJSON {
						fmt.Fprintln(out, kind)
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
		status, e := send(ctx, c, "GET", statusURL.String(), token, nil)
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
					if isExec {
						outputURL := *base
						outputURL.Path = prefix + "operations/" + url.PathEscape(id) + "/output"
						outputURL.RawQuery = ""
						output, e := send(ctx, c, "GET", outputURL.String(), token, nil)
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
