package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strings"
	"time"
)

type operationWatchOptions struct {
	config      string
	extraTokens []string
}

var powerShellSingleQuotes = strings.NewReplacer(
	"'", "''",
	"\u2018", "\u2018\u2018",
	"\u2019", "\u2019\u2019",
	"\u201a", "\u201a\u201a",
	"\u201b", "\u201b\u201b",
)

func operationHintArg(word string, config bool) string {
	if runtime.GOOS == "windows" {
		if !config && word != "" && strings.Trim(word, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_.-") == "" {
			return word
		}
		return "'" + powerShellSingleQuotes.Replace(word) + "'"
	}
	if !config && strings.Contains(word, "~") {
		return "'" + strings.ReplaceAll(word, "'", `'\''`) + "'"
	}
	return shellWord(word)
}

func watchLabel(cmd string) string {
	switch cmd {
	case "deploy", "rollback", "restart", "scale", "prune", "exec", "run", "remove", "destroy", "restore":
		return "following " + cmd
	case "ingress":
		return "following ingress apply"
	default:
		return "following operation"
	}
}

func verifyMarker(status string) string {
	switch status {
	case "ok":
		return "✓"
	case "warn":
		return "!"
	case "unknown":
		return "?"
	default:
		return ""
	}
}

func watchOperation(ctx context.Context, c *http.Client, base *url.URL, token, prefix, id, events string, out, errout io.Writer, asJSON bool, cmd, env, project string, options ...operationWatchOptions) int {
	var config string
	redact := []string{token}
	for _, opt := range options {
		config = opt.config
		redact = append(redact, opt.extraTokens...)
	}
	interactive := !asJSON && stderrTerminal(out)
	progress := newProgressLine(errout, !asJSON && stderrTerminal(errout))
	defer progress.Close()
	if !asJSON {
		progress.Start(watchLabel(cmd))
	}
	printResult := func(data []byte) {
		if !asJSON {
			printResponseShape(out, errout, data, false, shapeOperation)
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
			if progress.enabled && (cmd == "deploy" || cmd == "rollback") && detail.Role != "" {
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
				verb := "deploying"
				if cmd == "rollback" {
					verb = "rolling back"
				}
				progress.Start(fmt.Sprintf("%s %d ready of %d started", verb, len(ready), len(started)))
			}
			return
		}
		if ev.Kind == "verify" && detail.Step != "" {
			checks = append(checks, safeData)
		}
		if detail.Step != "" && ((interactive && (ev.Kind == "step" || ev.Kind == "verify")) || (!interactive && showsText(cmd, ev.Kind))) {
			if !asJSON {
				line := safeSecretText(detail.Step, redact...)
				if interactive && ev.Kind == "verify" {
					if marker := verifyMarker(detail.Status); marker != "" {
						line = styleFor(out, true).paint(statusTone(detail.Status), marker) + " " + line
					}
				}
				progress.Message(func() { fmt.Fprintln(out, "  "+line) })
			}
		} else if !interactive && !asJSON && !showsText(cmd, ev.Kind) {
			progress.Message(func() { fmt.Fprintln(out, safeSecretText(ev.Kind, redact...)) })
		}
	}
	interrupted := func() int {
		progress.Close()
		suffix := fmt.Sprintf(" -e %s --project %s", safeSecretText(env, redact...), safeSecretText(project, redact...))
		if config != "" {
			suffix += " --config " + operationHintArg(safeSecretText(config, redact...), true)
		}
		shownID := safeSecretText(id, redact...)
		argID := operationHintArg(shownID, false)
		fmt.Fprintf(errout, "watch interrupted; operation %s may continue\ninspect: berthy diagnostics %s%s\ncancel: berthy cancel %s%s\n", shownID, argID, suffix, argID, suffix)
		if s, ok := context.Cause(ctx).(commandSignal); ok && s.Signal != os.Interrupt {
			return 128 + int(s.Signal)
		}
		return 1
	}
	reader := watchReader{client: c, base: base, token: token, prefix: prefix, id: id, events: events}
	requestProgress := progress.enabled && (cmd == "deploy" || cmd == "rollback")
	for {
		last, refusal, older, streamErr := reader.stream(ctx, cursor, requestProgress, accept)
		cursor = last
		if refusal != "" {
			progress.Close()
			printAgentRefusal(errout, safeSecretText(refusal, redact...), !asJSON, older)
			return 1
		}
		if ctx.Err() != nil {
			return interrupted()
		}
		state, data := reader.status(ctx)
		if state != "" && state != "running" {
			if streamErr != nil || !finals[state] {
				last, _, _, replayErr := reader.stream(ctx, cursor, requestProgress, accept)
				cursor = last
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
				result, err := reader.output(ctx, state)
				if err != nil {
					fmt.Fprintln(errout, cmd+" output unavailable")
					return 1
				}
				if result == nil {
					printResult(data)
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
		if ctx.Err() != nil {
			return interrupted()
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
