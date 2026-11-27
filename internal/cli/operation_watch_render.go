package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"runtime"
	"strings"
)

type watchRender struct {
	out, errout                        io.Writer
	progress                           *progressLine
	cmd, env, project, config, id      string
	redact                             []string
	asJSON, interactive, terminalShown bool
	finals, started, ready             map[string]bool
	checks                             []json.RawMessage
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

func (w *watchRender) accept(ev operationEvent) {
	var payload any
	dec := json.NewDecoder(bytes.NewReader(ev.Data))
	dec.UseNumber()
	if dec.Decode(&payload) != nil {
		return
	}
	payload = redactOperationJSON(payload, w.redact...)
	safeData, err := json.Marshal(payload)
	if err != nil {
		return
	}
	var detail struct{ Step, Role, Status string }
	_ = json.Unmarshal(ev.Data, &detail)
	if ev.ID != 0 {
		switch ev.Kind {
		case "succeeded", "failed", "rolled_back", "interrupted", "recovery_required":
			w.finals[ev.Kind] = true
		}
	}
	if ev.Kind == "terminal" {
		if w.terminalShown {
			return
		}
		w.terminalShown = true
	}
	if ev.Kind == "rollout" {
		if w.progress.enabled && (w.cmd == "deploy" || w.cmd == "rollback") && detail.Role != "" {
			switch detail.Status {
			case "started":
				if w.started[detail.Role] {
					return
				}
				w.started[detail.Role] = true
			case "ready":
				if w.ready[detail.Role] {
					return
				}
				w.started[detail.Role], w.ready[detail.Role] = true, true
			default:
				return
			}
			verb := "deploying"
			if w.cmd == "rollback" {
				verb = "rolling back"
			}
			w.progress.Start(fmt.Sprintf("%s %d ready of %d started", verb, len(w.ready), len(w.started)))
		}
		return
	}
	if ev.Kind == "verify" && detail.Step != "" {
		w.checks = append(w.checks, safeData)
	}
	if detail.Step != "" && ((w.interactive && (ev.Kind == "step" || ev.Kind == "verify")) || (!w.interactive && showsText(w.cmd, ev.Kind))) {
		if !w.asJSON {
			line := safeSecretText(detail.Step, w.redact...)
			if w.interactive && ev.Kind == "verify" {
				if marker := verifyMarker(detail.Status); marker != "" {
					line = styleFor(w.out, true).paint(statusTone(detail.Status), marker) + " " + line
				}
			}
			w.progress.Message(func() { fmt.Fprintln(w.out, "  "+line) })
		}
	} else if !w.interactive && !w.asJSON && !showsText(w.cmd, ev.Kind) {
		w.progress.Message(func() { fmt.Fprintln(w.out, safeSecretText(ev.Kind, w.redact...)) })
	}
}

func (w *watchRender) interrupted() int {
	w.progress.Close()
	suffix := fmt.Sprintf(" -e %s --project %s", safeSecretText(w.env, w.redact...), safeSecretText(w.project, w.redact...))
	if w.config != "" {
		suffix += " --config " + operationHintArg(safeSecretText(w.config, w.redact...), true)
	}
	shownID := safeSecretText(w.id, w.redact...)
	argID := operationHintArg(shownID, false)
	fmt.Fprintf(w.errout, "watch interrupted; operation %s may continue\ninspect: berthy diagnostics %s%s\ncancel: berthy cancel %s%s\n", shownID, argID, suffix, argID, suffix)
	return 1
}

func (w *watchRender) printResult(data []byte) {
	if !w.asJSON {
		printResponseShape(w.out, w.errout, data, false, shapeOperation)
		return
	}
	var pretty bytes.Buffer
	if json.Indent(&pretty, data, "", "  ") != nil {
		fmt.Fprintln(w.errout, "invalid agent response")
		return
	}
	fmt.Fprintln(w.out, escapeJSONC1(pretty.String()))
}

func (w *watchRender) finish(state string, data []byte, output *watchOutput) int {
	var doc any
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	_ = dec.Decode(&doc)
	if w.asJSON {
		doc = redactOperationJSON(doc, w.redact...)
	} else {
		doc = safeJSONValue(doc, w.redact...)
	}
	data, _ = json.Marshal(doc)
	if output == nil && watchHasOutput(w.cmd) {
		w.printResult(data)
		return 1
	}
	exitCode := 0
	if output != nil {
		exitCode = output.ExitCode
		if w.asJSON {
			var object map[string]any
			dec := json.NewDecoder(bytes.NewReader(data))
			dec.UseNumber()
			if dec.Decode(&object) != nil {
				return 1
			}
			object["output"], object["exit_code"] = output.Output, output.ExitCode
			data, _ = json.Marshal(object)
		} else {
			fmt.Fprint(w.out, output.Output)
		}
	}
	if w.asJSON {
		data = withChecks(data, w.checks)
	}
	w.printResult(data)
	if state == "succeeded" && exitCode == 0 {
		return 0
	}
	if (state == "failed" || state == "rolled_back" || state == "recovery_required") && (w.cmd == "deploy" || w.cmd == "rollback" || w.cmd == "ingress") {
		hint := w.out
		if w.asJSON {
			hint = w.errout
		}
		fmt.Fprintln(hint, safeSecretText(failureHint(w.env, w.project, w.id), w.redact...))
	}
	return 1
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
