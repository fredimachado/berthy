// Command mcpdriver drives an MCP server as a client would, for verification. It starts the server command, initializes, lists the
// tools, makes the calls of a script in order and writes what came back.
//
//	mcpdriver --command "berthy mcp -e ENV --config CFG" --script calls.json --out results.json
//
// calls.json is [{"tool":"list_resources","args":{}}, ...]. results.json holds the server's name and version, the tool names and
// one entry per call: its structured content, its text and whether the server marked it as an error. A tool error is an answer,
// not a failure; the exit code is non-zero only when the protocol failed (the server did not start or initialize, a request
// got no answer). The value of every environment variable whose name starts with BERTHY_ and contains TOKEN, and of those named
// by --redact-env, is replaced by [redacted] in the results and in the server's stderr.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type step struct {
	Tool string         `json:"tool"`
	Args map[string]any `json:"args"`
}

type callResult struct {
	Index      int            `json:"index"`
	Tool       string         `json:"tool"`
	Args       map[string]any `json:"args"`
	IsError    bool           `json:"is_error"`
	Text       string         `json:"text"`
	Structured any            `json:"structured,omitempty"`
}

type results struct {
	Server struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"server"`
	Tools []string     `json:"tools"`
	Calls []callResult `json:"calls"`
	// Error is the protocol failure that ended the run, when there was one.
	Error string `json:"error,omitempty"`
}

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

// splitCommand splits a command line into words. Single and double quotes group a word; there are no escapes.
func splitCommand(s string) ([]string, error) {
	var words []string
	var cur strings.Builder
	inWord := false
	var quote rune
	for _, r := range s {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '"' || r == '\'':
			quote, inWord = r, true
		case r == ' ' || r == '\t' || r == '\n':
			if inWord {
				words = append(words, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteRune(r)
			inWord = true
		}
	}
	if quote != 0 {
		return nil, errors.New("unterminated quote in --command")
	}
	if inWord {
		words = append(words, cur.String())
	}
	return words, nil
}

// secrets is the values that must not reach a file or a terminal.
func secrets(extra string) []string {
	names := map[string]bool{}
	for _, name := range strings.Split(extra, ",") {
		if name = strings.TrimSpace(name); name != "" {
			names[name] = true
		}
	}
	var values []string
	for _, kv := range os.Environ() {
		name, value, _ := strings.Cut(kv, "=")
		if value != "" && (names[name] || (strings.HasPrefix(name, "BERTHY_") && strings.Contains(name, "TOKEN"))) {
			values = append(values, value)
		}
	}
	return values
}

func scrub(b []byte, values []string) []byte {
	for _, v := range values {
		b = bytes.ReplaceAll(b, []byte(v), []byte("[redacted]"))
	}
	return b
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("mcpdriver", flag.ContinueOnError)
	fs.SetOutput(stderr)
	command := fs.String("command", "", "the server command line")
	script := fs.String("script", "", "the JSON file of calls")
	out := fs.String("out", "", "where to write the results")
	redact := fs.String("redact-env", "", "comma-separated names of more environment variables whose values are never written")
	timeout := fs.Duration("timeout", 15*time.Minute, "limit of the whole run")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *command == "" || *script == "" || *out == "" || fs.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: mcpdriver --command \"CMD ARGS\" --script calls.json --out results.json [--redact-env A,B] [--timeout 15m]")
		return 2
	}
	words, err := splitCommand(*command)
	if err != nil || len(words) == 0 {
		fmt.Fprintln(stderr, "invalid --command")
		return 2
	}
	raw, err := os.ReadFile(*script)
	if err != nil {
		fmt.Fprintln(stderr, "cannot read script:", err)
		return 2
	}
	var steps []step
	if err := json.Unmarshal(raw, &steps); err != nil {
		fmt.Fprintln(stderr, "invalid script:", err)
		return 2
	}
	for i, s := range steps {
		if s.Tool == "" {
			fmt.Fprintf(stderr, "invalid script: call %d has no tool\n", i)
			return 2
		}
	}
	hidden := secrets(*redact)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	var res results
	res.Tools, res.Calls = []string{}, []callResult{}
	var serverErr bytes.Buffer
	failure := drive(ctx, words, steps, &res, &serverErr)
	if failure != nil {
		res.Error = failure.Error()
	}
	b, err := json.MarshalIndent(&res, "", "  ")
	if err != nil {
		fmt.Fprintln(stderr, "cannot encode results:", err)
		return 1
	}
	if err := os.WriteFile(*out, append(scrub(b, hidden), '\n'), 0600); err != nil {
		fmt.Fprintln(stderr, "cannot write results:", err)
		return 1
	}
	if serverErr.Len() > 0 {
		stderr.Write(scrub(serverErr.Bytes(), hidden))
	}
	if failure != nil {
		fmt.Fprintln(stderr, "protocol error:", string(scrub([]byte(failure.Error()), hidden)))
		return 1
	}
	toolErrors := 0
	for _, c := range res.Calls {
		if c.IsError {
			toolErrors++
		}
	}
	fmt.Fprintf(stdout, "%d tools, %d calls, %d tool errors\n", len(res.Tools), len(res.Calls), toolErrors)
	return 0
}

// drive connects, lists the tools and makes the calls. It fills res as it goes, so a failure keeps what came before it.
func drive(ctx context.Context, words []string, steps []step, res *results, serverErr *bytes.Buffer) error {
	cmd := exec.Command(words[0], words[1:]...)
	cmd.Stderr = serverErr
	client := sdk.NewClient(&sdk.Implementation{Name: "mcpdriver", Version: "v1"}, nil)
	session, err := client.Connect(ctx, &sdk.CommandTransport{Command: cmd}, nil)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer session.Close()
	if init := session.InitializeResult(); init != nil && init.ServerInfo != nil {
		res.Server.Name, res.Server.Version = init.ServerInfo.Name, init.ServerInfo.Version
	}
	for tool, err := range session.Tools(ctx, nil) {
		if err != nil {
			return fmt.Errorf("tools/list: %w", err)
		}
		res.Tools = append(res.Tools, tool.Name)
	}
	for i, s := range steps {
		args := s.Args
		if args == nil {
			args = map[string]any{}
		}
		out, err := session.CallTool(ctx, &sdk.CallToolParams{Name: s.Tool, Arguments: args})
		if err != nil {
			return fmt.Errorf("call %d (%s): %w", i, s.Tool, err)
		}
		var text strings.Builder
		for _, c := range out.Content {
			if t, ok := c.(*sdk.TextContent); ok {
				text.WriteString(t.Text)
			}
		}
		res.Calls = append(res.Calls, callResult{Index: i, Tool: s.Tool, Args: args, IsError: out.IsError, Text: text.String(), Structured: out.StructuredContent})
	}
	return nil
}
