package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"time"

	"berthy/internal/monitor"
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
	if len(args) == 0 || args[0] != "health" {
		fmt.Fprintln(errout, "setup-probe: invalid command")
		return 2
	}

	flags := flag.NewFlagSet("setup-probe", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	listen := flags.String("listen", "", "")
	timeout := flags.Duration("timeout", 0, "")
	if err := flags.Parse(args[1:]); err != nil || flags.NArg() != 0 {
		fmt.Fprintln(errout, "setup-probe: invalid arguments")
		return 2
	}
	ip, port, err := setup.ParseListen(*listen)
	if err != nil {
		fmt.Fprintln(errout, "setup-probe: invalid listen address")
		return 2
	}
	if *timeout <= 0 || *timeout > 3*time.Second {
		fmt.Fprintln(errout, "setup-probe: timeout must be positive and at most 3s")
		return 2
	}
	base := url.URL{Scheme: "http", Host: net.JoinHostPort(ip.String(), strconv.Itoa(port))}
	if monitor.ProbePeer(context.Background(), base.String(), *timeout, nil).State != monitor.OK {
		return 1
	}
	return 0
}
