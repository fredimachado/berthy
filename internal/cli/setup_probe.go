package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"berthy/internal/agentclient"
	"berthy/internal/monitor"
	"berthy/internal/scope"
	"berthy/internal/setup"
)

func runSetupProbe(args []string, stdin io.Reader, out, errout io.Writer) int {
	if len(args) == 1 && args[0] == "version" {
		if _, err := fmt.Fprintln(out, "berthy-setup-probe/1"); err != nil {
			fmt.Fprintln(errout, "setup-probe: cannot write version")
			return 1
		}
		return 0
	}
	if len(args) == 0 || (args[0] != "health" && args[0] != "doctor") {
		fmt.Fprintln(errout, "setup-probe: invalid command")
		return 2
	}

	flags := flag.NewFlagSet("setup-probe", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	listen := flags.String("listen", "", "")
	var timeout time.Duration
	var scopeName string
	if args[0] == "health" {
		flags.DurationVar(&timeout, "timeout", 0, "")
	} else {
		flags.StringVar(&scopeName, "scope", "", "")
	}
	if err := flags.Parse(args[1:]); err != nil || flags.NArg() != 0 {
		fmt.Fprintln(errout, "setup-probe: invalid arguments")
		return 2
	}
	ip, port, err := setup.ParseListen(*listen)
	if err != nil {
		fmt.Fprintln(errout, "setup-probe: invalid listen address")
		return 2
	}
	base := url.URL{Scheme: "http", Host: net.JoinHostPort(ip.String(), strconv.Itoa(port))}
	if args[0] == "doctor" {
		return setupProbeDoctor(&base, scopeName, stdin, out, errout)
	}
	if timeout <= 0 || timeout > 3*time.Second {
		fmt.Fprintln(errout, "setup-probe: timeout must be positive and at most 3s")
		return 2
	}
	if monitor.ProbePeer(context.Background(), base.String(), timeout, nil).State != monitor.OK {
		return 1
	}
	return 0
}

func setupProbeDoctor(base *url.URL, scopeName string, stdin io.Reader, out, errout io.Writer) int {
	id, err := scope.Parse(scopeName)
	if err != nil {
		fmt.Fprintln(errout, "setup-probe: invalid scope")
		return 2
	}
	const maxTokenInput = 1024
	input, err := io.ReadAll(io.LimitReader(stdin, maxTokenInput+1))
	if err != nil || len(input) > maxTokenInput {
		fmt.Fprintln(errout, "setup-probe: invalid token input")
		return 2
	}
	token, err := parseToken(input)
	if err != nil {
		fmt.Fprintln(errout, "setup-probe:", err)
		return 2
	}
	client := agentclient.NewHTTPClient()
	defer client.CloseIdleConnections()
	body, status, err := agentclient.Do(context.Background(), client, base, id.APIPath(), token, http.MethodGet, "doctor", "", nil)
	if err != nil {
		fmt.Fprintln(errout, "setup-probe: doctor request failed")
		return 1
	}
	if status != http.StatusOK {
		fmt.Fprintf(errout, "setup-probe: doctor returned HTTP %d\n", status)
		return 1
	}
	if _, err := out.Write(body); err != nil {
		fmt.Fprintln(errout, "setup-probe: cannot write doctor response")
		return 1
	}
	return 0
}
