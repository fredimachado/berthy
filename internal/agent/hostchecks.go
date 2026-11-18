package agent

import (
	"bufio"
	"cmp"
	"context"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"net"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"berthy/internal/api"
	"berthy/internal/engine"
	"berthy/internal/monitor"
)

// hostFiles is the host's file system, which the host checks read: the socket tables of /proc, and the files that the update
// notifier writes when a reboot is pending. Tests point it at fixtures.
var hostFiles fs.FS = os.DirFS("/")

const (
	// hostCheckInterval is how often the monitor runs the host checks. hostDoctorTimeout bounds the doctor's answer about them.
	hostCheckInterval = 10 * time.Minute
	hostDoctorTimeout = 5 * time.Second
	// updateNotifierDir is the package directory that writes the reboot-required file. A host without it does not provide the
	// mechanism, so the reboot check is left out there.
	updateNotifierDir  = "usr/share/update-notifier"
	rebootFlagFile     = "var/run/reboot-required"
	rebootPackagesFile = "var/run/reboot-required.pkgs"
)

// portNotes names the ports that the public ports check shows with a note. Any other port is shown by its number.
var portNotes = map[uint16]string{
	22:    "SSH",
	80:    "HTTP",
	443:   "HTTPS",
	2375:  "Docker API without TLS",
	2376:  "Docker API",
	2377:  "Swarm management",
	7946:  "Swarm gossip",
	5432:  "PostgreSQL",
	3306:  "MySQL",
	6379:  "Redis",
	27017: "MongoDB",
}

// quietPorts are listed on a public address without making the check warn: the web and SSH ports a server is expected to show.
var quietPorts = []uint16{22, 80, 443}

// listener is a TCP socket in the LISTEN state.
type listener struct {
	IP   net.IP
	Port uint16
}

// parseListeners reads a socket table, /proc/net/tcp or /proc/net/tcp6, and returns its LISTEN sockets in table order. The
// header and the sockets in other states are skipped.
func parseListeners(r io.Reader) ([]listener, error) {
	var out []listener
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		// Field 3 is the socket state, and 0A is LISTEN.
		if len(f) < 4 || f[3] != "0A" {
			continue
		}
		addr, port, ok := strings.Cut(f[1], ":")
		if !ok {
			return nil, errors.New("invalid local address")
		}
		ip, err := parseKernelAddress(addr)
		if err != nil {
			return nil, err
		}
		p, err := strconv.ParseUint(port, 16, 16)
		if err != nil {
			return nil, err
		}
		out = append(out, listener{IP: ip, Port: uint16(p)})
	}
	return out, sc.Err()
}

// parseKernelAddress decodes an address of a socket table: 8 hex digits for IPv4, 32 for IPv6. The kernel writes each 32-bit
// word in the host's byte order, so on the little-endian hosts Berthy runs on the bytes of each word come out reversed. An
// IPv4-mapped IPv6 address is returned as IPv4.
func parseKernelAddress(s string) (net.IP, error) {
	b, err := hex.DecodeString(s)
	if err != nil || (len(b) != net.IPv4len && len(b) != net.IPv6len) {
		return nil, errors.New("invalid address")
	}
	for i := 0; i < len(b); i += 4 {
		slices.Reverse(b[i : i+4])
	}
	ip := net.IP(b)
	if v4 := ip.To4(); v4 != nil {
		return v4, nil
	}
	return ip, nil
}

// readListeners returns the LISTEN sockets of the IPv4 and the IPv6 tables. A kernel without IPv6 has no tcp6 table, which is
// not an error.
func readListeners(fsys fs.FS) ([]listener, error) {
	var out []listener
	for _, name := range []string{"proc/net/tcp", "proc/net/tcp6"} {
		f, err := fsys.Open(name)
		if errors.Is(err, fs.ErrNotExist) && name == "proc/net/tcp6" {
			continue
		}
		if err != nil {
			return nil, err
		}
		ls, err := parseListeners(f)
		f.Close()
		if err != nil {
			return nil, err
		}
		out = append(out, ls...)
	}
	return out, nil
}

// publicAddress reports whether a socket on ip can be reached from beyond the private network. Loopback, private, VPN and
// link-local addresses cannot be; an unspecified address (0.0.0.0 or ::) listens on every address, so it can.
func publicAddress(ip net.IP) bool {
	return !privateAddress(ip) && !ip.IsLinkLocalUnicast()
}

// exposedPort is one TCP port open on a public address: a socket that listens there, or a port that a container publishes
// there. Address is the address as text. Published is true when a container publishes the port.
type exposedPort struct {
	Port      uint16
	Address   string
	Published bool
}

// exposures returns the public exposures: the public listeners and the TCP ports that containers publish on a public address.
// A listener and a published port on the same address and port are one exposure, which is marked published. The result is
// sorted by port, then address.
func exposures(listeners []listener, published []engine.PublishedPort) []exposedPort {
	type key struct {
		port uint16
		addr string
	}
	seen := map[key]bool{}
	for _, l := range listeners {
		if !publicAddress(l.IP) {
			continue
		}
		k := key{l.Port, l.IP.String()}
		if _, ok := seen[k]; !ok {
			seen[k] = false
		}
	}
	for _, p := range published {
		if p.Type != "tcp" {
			continue
		}
		addr := p.IP
		if ip := net.ParseIP(p.IP); ip != nil {
			if !publicAddress(ip) {
				continue
			}
			addr = ip.String()
		}
		seen[key{p.PublicPort, addr}] = true
	}
	out := make([]exposedPort, 0, len(seen))
	for k, pub := range seen {
		out = append(out, exposedPort{Port: k.port, Address: k.addr, Published: pub})
	}
	slices.SortFunc(out, func(a, b exposedPort) int {
		if c := cmp.Compare(a.Port, b.Port); c != 0 {
			return c
		}
		return strings.Compare(a.Address, b.Address)
	})
	return out
}

// portNote is the note of a port: its known name, or the agent's name for the agent's own port.
func portNote(port, agentPort uint16) string {
	if note, ok := portNotes[port]; ok {
		return note
	}
	if agentPort != 0 && port == agentPort {
		return "berthy agent"
	}
	return ""
}

// portLine is one exposure as the check's detail shows it: the port, its note when one is known, the address, and whether a
// container publishes it.
func portLine(p exposedPort, agentPort uint16) string {
	s := strconv.Itoa(int(p.Port))
	if note := portNote(p.Port, agentPort); note != "" {
		s += " " + note
	}
	s += " on " + p.Address
	if p.Published {
		s += " (published by a container)"
	}
	return s
}

// publicPortsResult is the public_ports check's result for the exposures ex, the sources that could not be read in missing,
// and the agent's own port. It warns about every exposure but the quiet ports, which are listed. It is unknown when nothing
// warns and a source is missing, so a source that cannot be read is never taken for no exposure.
func publicPortsResult(ex []exposedPort, missing []string, agentPort uint16) monitor.Result {
	warn := false
	lines := make([]string, 0, len(ex))
	for _, p := range ex {
		if !slices.Contains(quietPorts, p.Port) {
			warn = true
		}
		lines = append(lines, portLine(p, agentPort))
	}
	var parts []string
	if len(lines) > 0 {
		parts = append(parts, "listening on a public address: "+strings.Join(lines, ", ")+". A listening port is not proof that it is reachable, since a firewall may sit in front.")
	}
	if len(missing) > 0 {
		parts = append(parts, "Not checked: "+strings.Join(missing, ", ")+".")
	}
	if len(parts) == 0 {
		parts = append(parts, "no port listens on a public address")
	}
	state := monitor.OK
	switch {
	case warn:
		state = monitor.Warning
	case len(missing) > 0:
		state = monitor.Unknown
	}
	return monitor.Result{State: state, Detail: strings.Join(parts, " ")}
}

// agentPortOf returns the port of the agent's listen address, or 0 when it has none.
func agentPortOf(listen string) uint16 {
	_, port, err := net.SplitHostPort(listen)
	if err != nil {
		return 0
	}
	n, err := strconv.ParseUint(port, 10, 16)
	if err != nil {
		return 0
	}
	return uint16(n)
}

// publicPorts is the public_ports check: the public listeners of the host and the ports that containers publish on a public
// address. e is nil when Docker cannot be asked, and listen is the agent's listen address.
func publicPorts(ctx context.Context, e *engine.Engine, listen string) monitor.Result {
	var missing []string
	listeners, err := readListeners(hostFiles)
	if err != nil {
		missing = append(missing, "host sockets")
	}
	var published []engine.PublishedPort
	if e == nil {
		missing = append(missing, "container ports")
	} else {
		published, err = e.PublishedPorts(ctx)
		if err != nil {
			missing = append(missing, "container ports")
		}
	}
	return publicPortsResult(exposures(listeners, published), missing, agentPortOf(listen))
}

// rebootMechanism reports whether the host provides the reboot-required file. The update notifier writes it, so a host without
// its package directory is not asked.
func rebootMechanism(fsys fs.FS) bool {
	_, err := fs.Stat(fsys, updateNotifierDir)
	return !errors.Is(err, fs.ErrNotExist)
}

// rebootResult reports whether a reboot is pending, which is when the reboot-required file exists. The packages that asked for
// it are listed from the .pkgs file when the host writes one.
func rebootResult(fsys fs.FS) monitor.Result {
	_, err := fs.Stat(fsys, rebootFlagFile)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return monitor.Result{State: monitor.OK, Detail: "no reboot pending"}
	case err != nil:
		return monitor.Result{State: monitor.Unknown, Detail: "unavailable"}
	}
	detail := "reboot required"
	if b, err := fs.ReadFile(fsys, rebootPackagesFile); err == nil {
		if pkgs := packageNames(b); len(pkgs) > 0 {
			detail += " for " + strings.Join(pkgs, ", ")
		}
	}
	return monitor.Result{State: monitor.Warning, Detail: detail}
}

// packageNames returns the distinct package names of a .pkgs file, which lists one name per line, in sorted order.
func packageNames(b []byte) []string {
	pkgs := strings.Fields(string(b))
	slices.Sort(pkgs)
	return slices.Compact(pkgs)
}

// hostChecks returns the monitor's host checks: the public ports, and the reboot check when the host provides the mechanism.
// Neither is critical; both warn and change nothing on the host.
func hostChecks(e *engine.Engine, listen string) []monitor.Check {
	out := []monitor.Check{monitor.NewFunc("public_ports", hostCheckInterval, 1, func(ctx context.Context) monitor.Result {
		return publicPorts(ctx, e, listen)
	})}
	if rebootMechanism(hostFiles) {
		out = append(out, monitor.NewFunc("reboot_required", hostCheckInterval, 1, func(context.Context) monitor.Result {
			return rebootResult(hostFiles)
		}))
	}
	return out
}

// hostDoctorChecks is the doctor's view of hostChecks: the same results, each as a check that is never critical, so the
// doctor still exits 0 with them.
func hostDoctorChecks(parent context.Context, e *engine.Engine, listen string) []api.Check {
	ctx, cancel := context.WithTimeout(parent, hostDoctorTimeout)
	defer cancel()
	ports := publicPorts(ctx, e, listen)
	out := []api.Check{{Name: "public_ports", OK: ports.State == monitor.OK, Detail: ports.Detail}}
	if rebootMechanism(hostFiles) {
		reboot := rebootResult(hostFiles)
		out = append(out, api.Check{Name: "reboot_required", OK: reboot.State == monitor.OK, Detail: reboot.Detail})
	}
	return out
}
