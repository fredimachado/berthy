package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"berthy/internal/agentclient"
)

func watchOperation(ctx context.Context, c *http.Client, base *url.URL, token, prefix, id, events string, out, errout io.Writer, asJSON bool, cmd, env, project string, extraTokens ...string) int {
	redact := append([]string{token}, extraTokens...)
	interactive := !asJSON && (cmd == "deploy" || cmd == "rollback") && stderrTerminal(out)
	progress := newProgressLine(out, interactive)
	defer progress.Close()
	if interactive {
		progress.Start("deploying")
	}
	printResult := func(data []byte) {
		if !asJSON {
			printResponse(out, errout, data, false)
			return
		}
		var pretty bytes.Buffer
		if json.Indent(&pretty, data, "", "  ") != nil {
			fmt.Fprintln(errout, "invalid agent response")
			return
		}
		fmt.Fprintln(out, escapeJSONC1(pretty.String()))
	}
	var cursor uint64
	var checks []json.RawMessage
	finals := make(map[string]bool)
	started, ready := make(map[string]bool), make(map[string]bool)
	failures := 0
	terminalShown := false

	accept := func(ev operationEvent) {
		var payload any
		dec := json.NewDecoder(bytes.NewReader(ev.Data))
		dec.UseNumber()
		if dec.Decode(&payload) != nil {
			return
		}
		payload = redactOperationJSON(payload, redact...)
		safeData, err := json.Marshal(payload)
		if err != nil {
			return
		}
		var detail struct{ Step, Role, Status string }
		_ = json.Unmarshal(ev.Data, &detail)
		if ev.ID != 0 {
			switch ev.Kind {
			case "succeeded", "failed", "rolled_back", "interrupted", "recovery_required":
				finals[ev.Kind] = true
			}
		}
		if ev.Kind == "terminal" {
			if terminalShown {
				return
			}
			terminalShown = true
		}
		if ev.Kind == "rollout" {
			if interactive && detail.Role != "" {
				switch detail.Status {
				case "started":
					if started[detail.Role] {
						return
					}
					started[detail.Role] = true
				case "ready":
					if ready[detail.Role] {
						return
					}
					started[detail.Role], ready[detail.Role] = true, true
				default:
					return
				}
				progress.Start(fmt.Sprintf("deploying %d ready of %d started", len(ready), len(started)))
			}
			return
		}
		if showsText(cmd, ev.Kind) && detail.Step != "" {
			if ev.Kind == "verify" {
				checks = append(checks, safeData)
			}
			if !asJSON {
				if interactive {
					progress.Message("  " + safeSecretText(detail.Step, redact...))
				} else {
					fmt.Fprintln(out, "  "+safeSecretText(detail.Step, redact...))
				}
			}
			return
		}
		if interactive {
			if ev.Kind == "step" && detail.Step != "" {
				progress.Message("  " + safeSecretText(detail.Step, redact...))
			}
		} else if !asJSON && !showsText(cmd, ev.Kind) {
			fmt.Fprintln(out, safeSecretText(ev.Kind, redact...))
		}
	}
	var notFound string
	stream := func() error {
		notFound = ""
		streamURL := *base
		streamURL.Path, streamURL.RawPath, streamURL.RawQuery = events, "", ""
		if interactive {
			streamURL.RawQuery = "progress=1"
		}
		streamCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(streamCtx, http.MethodGet, streamURL.String(), nil)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		if cursor != 0 {
			req.Header.Set("Last-Event-ID", strconv.FormatUint(cursor, 10))
		}
		resp, err := agentclient.SendRequest(streamCtx, c, req, token)
		if err != nil {
			if resp != nil {
				resp.Body.Close()
			}
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusNotFound {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			notFound = safeSecretText(agentclient.Message(resp.StatusCode, body), redact...)
			return fmt.Errorf("operation stream unavailable")
		}
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("operation stream unavailable")
		}
		cursor, err = readOperationEvents(resp.Body, cursor, accept)
		return err
	}
	interrupted := func() int {
		progress.Close()
		fmt.Fprintf(errout, "watch interrupted; operation %s may continue; inspect with berthy diagnostics %s -e %s --project %s or cancel with berthy cancel %s -e %s --project %s\n", safeSecretText(id, redact...), safeSecretText(id, redact...), safeSecretText(env, redact...), safeSecretText(project, redact...), safeSecretText(id, redact...), safeSecretText(env, redact...), safeSecretText(project, redact...))
		return 1
	}
	for {
		streamErr := stream()
		if notFound != "" {
			progress.Close()
			fmt.Fprintln(errout, notFound)
			return 1
		}
		if ctx.Err() != nil {
			return interrupted()
		}
		statusURL := *base
		statusURL.Path, statusURL.RawPath, statusURL.RawQuery = prefix+"operations/"+url.PathEscape(id), "", ""
		status, statusErr := agentclient.Send(ctx, c, http.MethodGet, statusURL.String(), token, nil)
		var data []byte
		var state string
		if statusErr == nil {
			var e error
			data, e = io.ReadAll(io.LimitReader(status.Body, (1<<20)+1))
			status.Body.Close()
			var v struct {
				State string `json:"state"`
			}
			if e == nil && len(data) <= 1<<20 && status.StatusCode == http.StatusOK && json.Unmarshal(data, &v) == nil {
				switch v.State {
				case "running", "succeeded", "failed", "rolled_back", "interrupted", "recovery_required":
					state = v.State
				}
			}
		} else if status != nil {
			status.Body.Close()
		}
		if ctx.Err() != nil {
			return interrupted()
		}
		if state != "" && state != "running" {
			if streamErr != nil || !finals[state] {
				replayErr := stream()
				if replayErr != nil || !finals[state] {
					progress.Close()
					fmt.Fprintf(errout, "incomplete event delivery for operation %s (state %s)\n", safeSecretText(id, redact...), state)
				}
			}
			progress.Close()
			var doc any
			dec := json.NewDecoder(bytes.NewReader(data))
			dec.UseNumber()
			_ = dec.Decode(&doc)
			if asJSON {
				doc = redactOperationJSON(doc, redact...)
			} else {
				doc = safeJSONValue(doc, redact...)
			}
			data, _ = json.Marshal(doc)
			exitCode := 0
			if cmd == "exec" || cmd == "run" {
				outputURL := *base
				outputURL.Path, outputURL.RawPath, outputURL.RawQuery = prefix+"operations/"+url.PathEscape(id)+"/output", "", ""
				output, e := agentclient.Send(ctx, c, http.MethodGet, outputURL.String(), token, nil)
				if e != nil {
					fmt.Fprintln(errout, cmd+" output unavailable")
					return 1
				}
				b, e := io.ReadAll(io.LimitReader(output.Body, (1<<20)+1))
				output.Body.Close()
				if output.StatusCode == http.StatusNotFound && state == "failed" {
					printResult(data)
					return 1
				}
				var result struct {
					Output   string `json:"output"`
					ExitCode int    `json:"exit_code"`
				}
				if e != nil || len(b) > 1<<20 || output.StatusCode != http.StatusOK || json.Unmarshal(b, &result) != nil {
					fmt.Fprintln(errout, cmd+" output unavailable")
					return 1
				}
				exitCode = result.ExitCode
				if asJSON {
					var object map[string]any
					dec := json.NewDecoder(bytes.NewReader(data))
					dec.UseNumber()
					if dec.Decode(&object) != nil {
						return 1
					}
					object["output"], object["exit_code"] = result.Output, result.ExitCode
					data, _ = json.Marshal(object)
				} else {
					fmt.Fprint(out, result.Output)
				}
			}
			if asJSON {
				data = withChecks(data, checks)
			}
			printResult(data)
			if state == "succeeded" && exitCode == 0 {
				return 0
			}
			if (state == "failed" || state == "rolled_back" || state == "recovery_required") && (cmd == "deploy" || cmd == "rollback" || cmd == "ingress") {
				hint := out
				if asJSON {
					hint = errout
				}
				fmt.Fprintln(hint, safeSecretText(failureHint(env, project, id), redact...))
			}
			return 1
		}
		if state == "running" {
			failures = 0
		} else {
			failures++
			if failures >= 3 {
				progress.Close()
				fmt.Fprintln(errout, "operation status unavailable")
				return 1
			}
		}
		select {
		case <-time.After(time.Second):
		case <-ctx.Done():
			return interrupted()
		}
	}
}

func redactOperationJSON(value any, tokens ...string) any {
	switch v := value.(type) {
	case string:
		return redactSecretText(v, tokens...)
	case []any:
		for i := range v {
			v[i] = redactOperationJSON(v[i], tokens...)
		}
		return v
	case map[string]any:
		clean := make(map[string]any, len(v))
		for key, item := range v {
			clean[redactSecretText(key, tokens...)] = redactOperationJSON(item, tokens...)
		}
		return clean
	default:
		return value
	}
}
