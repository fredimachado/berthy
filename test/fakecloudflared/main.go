// Command cloudflared stands in for the Cloudflare connector in the end-to-end verification of managed ingress. It is a
// test tool. The agent does not import it, and it is built as a static binary and packaged as the connector image.
//
// It accepts the two invocations Berthy makes, and refuses every other one with exit code 2:
//
//	cloudflared tunnel --no-autoupdate --metrics 0.0.0.0:20241 run   the connector; needs TUNNEL_TOKEN
//	cloudflared tunnel --metrics 127.0.0.1:20241 ready               the readiness check; exits 0 on HTTP 200
//
// The connector serves GET /ready on its metrics address. It answers 503 for the first few seconds and 200 afterwards.
// It connects to nothing, and it never prints the tunnel token.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

const (
	defaultMetrics = "127.0.0.1:20241"
	readyAfter     = 3 * time.Second
	logEvery       = 30 * time.Second
	readyTimeout   = 2 * time.Second
)

// usage is the message for an invocation the stand-in does not accept.
var usage = errors.New("usage: cloudflared tunnel [--no-autoupdate] [--metrics ADDR] run|ready")

// invocation is one accepted command line.
type invocation struct {
	verb    string // run or ready
	metrics string // the metrics address, defaulted when the flag is absent
}

func main() {
	os.Exit(run(os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}

// parse reads a command line the way cloudflared does: flags may appear before the subcommand as well as after it, and
// the two words that remain must be "tunnel" and then "run" or "ready".
func parse(args []string) (invocation, error) {
	metrics := ""
	var words []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--no-autoupdate":
			// Accepted for compatibility with Berthy's invocation; it changes nothing here.
		case a == "--metrics":
			if i+1 >= len(args) || args[i+1] == "" || strings.HasPrefix(args[i+1], "-") {
				return invocation{}, usage
			}
			metrics = args[i+1]
			i++
		case strings.HasPrefix(a, "--metrics="):
			metrics = strings.TrimPrefix(a, "--metrics=")
			if metrics == "" {
				return invocation{}, usage
			}
		case strings.HasPrefix(a, "-"):
			return invocation{}, usage
		default:
			words = append(words, a)
		}
	}
	if len(words) != 2 || words[0] != "tunnel" || (words[1] != "run" && words[1] != "ready") {
		return invocation{}, usage
	}
	if metrics == "" {
		metrics = defaultMetrics
	}
	return invocation{verb: words[1], metrics: metrics}, nil
}

// run executes one invocation and returns the process exit code: 0 on success, 1 when the connector or the readiness
// check fails, and 2 for an invocation the stand-in does not accept.
func run(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	inv, err := parse(args)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if inv.verb == "ready" {
		return checkReady(inv.metrics, stderr)
	}
	// The message names the variable only. The token is never printed.
	if getenv("TUNNEL_TOKEN") == "" {
		fmt.Fprintln(stderr, "TUNNEL_TOKEN is not set; the connector needs a tunnel token")
		return 1
	}
	ln, err := net.Listen("tcp", inv.metrics)
	if err != nil {
		fmt.Fprintln(stderr, "metrics listen failed:", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	conn := connector{started: time.Now(), readyAfter: readyAfter, now: time.Now}
	if err := serveConnector(ctx, ln, conn, logEvery, stdout); err != nil {
		fmt.Fprintln(stderr, "connector failed:", err)
		return 1
	}
	return 0
}

// connector decides readiness from its start time and a clock. The clock is a parameter so tests can run it without
// waiting.
type connector struct {
	started    time.Time
	readyAfter time.Duration
	now        func() time.Time
}

// ready reports whether the connector has been up for readyAfter.
func (c connector) ready() bool {
	return c.now().Sub(c.started) >= c.readyAfter
}

// serveConnector serves GET /ready on ln and logs "connector running" every logEvery until ctx is done. It stops the
// server and returns nil when ctx is done.
func serveConnector(ctx context.Context, ln net.Listener, conn connector, logEvery time.Duration, logw io.Writer) error {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ready", func(w http.ResponseWriter, _ *http.Request) {
		if !conn.ready() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()
	fmt.Fprintf(logw, "connector starting, metrics on %s\n", ln.Addr())
	ticker := time.NewTicker(logEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = srv.Shutdown(shutdown)
			fmt.Fprintln(logw, "connector stopped")
			return nil
		case err := <-served:
			return err
		case <-ticker.C:
			fmt.Fprintln(logw, "connector running")
		}
	}
}

// checkReady queries the connector's readiness on addr. It exits 0 only on HTTP 200.
func checkReady(addr string, stderr io.Writer) int {
	client := &http.Client{Timeout: readyTimeout, Transport: &http.Transport{Proxy: nil}}
	resp, err := client.Get("http://" + addr + "/ready")
	if err != nil {
		fmt.Fprintln(stderr, "not ready: connector unreachable")
		return 1
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(stderr, "not ready: HTTP %d\n", resp.StatusCode)
		return 1
	}
	return 0
}
