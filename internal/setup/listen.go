package setup

import (
	"context"
	"fmt"
	"net"
	"strconv"
)

// ParseListen reads --listen: ADDRESS:PORT with an IPv4 address, or [ADDRESS]:PORT with an IPv6 one. The address must be a
// specific one of the server, so 0.0.0.0 and :: are refused.
func ParseListen(s string) (net.IP, int, error) {
	host, portText, err := net.SplitHostPort(s)
	if err != nil {
		return nil, 0, fmt.Errorf("invalid --listen %q: use ADDRESS:PORT", s)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return nil, 0, fmt.Errorf("invalid --listen %q: %q is not an IP address", s, host)
	}
	if ip.IsUnspecified() {
		return nil, 0, fmt.Errorf("invalid --listen %q: use a specific address of the server, not %s", s, host)
	}
	port, err := strconv.Atoi(portText)
	if err != nil || portText[0] < '0' || portText[0] > '9' || port < 1 || port > 65535 {
		return nil, 0, fmt.Errorf("invalid --listen %q: the port must be 1 to 65535", s)
	}
	return ip, port, nil
}

// checkListenPort reads the server's TCP tables before setup changes the server.
func (s *Session) checkListenPort(ctx context.Context, f *Facts) error {
	if s.existing != nil && f.ServiceActive {
		return nil
	}
	endpoint := s.Opts.listen()
	addr4, addr6 := procListenAddresses(endpoint)
	wild4 := ""
	if addr4 != "" {
		wild4 = fmt.Sprintf("00000000:%04X", s.Opts.ListenPort)
	}
	wild6 := fmt.Sprintf("00000000000000000000000000000000:%04X", s.Opts.ListenPort)
	script := "# setup listen port\n" +
		"set -- /proc/net/tcp\n" +
		"if [ -e /proc/net/tcp6 ]; then set -- \"$@\" /proc/net/tcp6; fi\n" +
		"LC_ALL=C awk -v addr4=" + shellQuote(addr4) + " -v addr6=" + shellQuote(addr6) +
		" -v wild4=" + shellQuote(wild4) + " -v wild6=" + shellQuote(wild6) + ` '
    $4 == "0A" && ($2 == addr4 || $2 == addr6 || $2 == wild4 || $2 == wild6) { busy=1 }
    END { if (busy) print "busy"; else print "free" }
  ' "$@"` + "\n"
	if s.DryRun {
		s.Log.Would(script)
		return nil
	}
	res, err := s.R.Run(ctx, script, nil)
	if err != nil {
		return fmt.Errorf("could not check listen address %s: %w", endpoint, err)
	}
	if res.Code != 0 {
		return fmt.Errorf("could not check listen address %s", endpoint)
	}
	switch string(res.Stdout) {
	case "free\n":
		return nil
	case "busy\n":
		return fmt.Errorf("listen address %s is already in use", endpoint)
	default:
		return fmt.Errorf("could not check listen address %s", endpoint)
	}
}
