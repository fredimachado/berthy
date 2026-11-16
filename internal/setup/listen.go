package setup

import (
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
