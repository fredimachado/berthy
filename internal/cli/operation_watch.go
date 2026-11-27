package cli

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"time"
)

type operationWatchOptions struct {
	config      string
	extraTokens []string
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
	render := watchRender{
		out: out, errout: errout, progress: progress,
		cmd: cmd, env: env, project: project, config: config, id: id,
		redact: redact, asJSON: asJSON, interactive: interactive,
		finals: make(map[string]bool), started: make(map[string]bool), ready: make(map[string]bool),
	}
	interrupted := func() int {
		code := render.interrupted()
		if s, ok := context.Cause(ctx).(commandSignal); ok && s.Signal != os.Interrupt {
			return 128 + int(s.Signal)
		}
		return code
	}
	var cursor uint64
	failures := 0
	reader := watchReader{client: c, base: base, token: token, prefix: prefix, id: id, events: events}
	requestProgress := progress.enabled && (cmd == "deploy" || cmd == "rollback")
	for {
		last, refusal, older, streamErr := reader.stream(ctx, cursor, requestProgress, render.accept)
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
			if streamErr != nil || !render.finals[state] {
				last, _, _, replayErr := reader.stream(ctx, cursor, requestProgress, render.accept)
				cursor = last
				if replayErr != nil || !render.finals[state] {
					progress.Close()
					fmt.Fprintf(errout, "incomplete event delivery for operation %s (state %s)\n", safeSecretText(id, redact...), state)
				}
			}
			progress.Close()
			var output *watchOutput
			if cmd == "exec" || cmd == "run" {
				var err error
				output, err = reader.output(ctx, state)
				if err != nil {
					fmt.Fprintln(errout, cmd+" output unavailable")
					return 1
				}
			}
			return render.finish(state, data, output)
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
