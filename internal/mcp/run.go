package mcp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"berthy/internal/agentclient"
	"berthy/internal/scope"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Version is what the server reports to its client.
const Version = "dev"

// nopCloser lets the transport finish without closing the standard output of the process.
type nopCloser struct{ io.Writer }

func (nopCloser) Close() error { return nil }

// Run is "berthy mcp -e ENV [--config CONFIG] [--manifest-dir DIR]": it serves MCP over stdin and stdout until stdin ends or the
// process is stopped. Nothing but protocol messages goes to stdout; every other line goes to stderr, and none holds the token.
func Run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	var env, config string
	dir := "."
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-e", "--config", "--manifest-dir":
			if i+1 == len(args) {
				fmt.Fprintf(stderr, "missing value for %s\n", args[i])
				return 2
			}
			i++
			switch args[i-1] {
			case "-e":
				env = args[i]
			case "--config":
				config = args[i]
			default:
				dir = args[i]
			}
		default:
			fmt.Fprintf(stderr, "unknown argument %s\nusage: mcp -e ENV [--config CONFIG] [--manifest-dir DIR]\n", args[i])
			return 2
		}
	}
	if !scope.ValidEnvironment(env) {
		fmt.Fprintln(stderr, "valid -e environment required")
		return 2
	}
	path, err := agentclient.ConfigPath(config)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	target, err := agentclient.Resolve(path, agentclient.Selector{Environment: env, ManifestDir: dir})
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	server := NewServer(Config{Target: target, ManifestDir: dir, Version: Version})
	err = server.Run(ctx, &sdk.IOTransport{Reader: io.NopCloser(stdin), Writer: nopCloser{stdout}})
	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, io.EOF) {
		fmt.Fprintln(stderr, "mcp server stopped:", target.Redact(err.Error()))
		return 1
	}
	return 0
}
