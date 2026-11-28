package agent

import (
	"context"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"berthy/internal/monitor"
)

// procNetTCP is /proc/net/tcp as the kernel writes it. Its LISTEN lines cover loopback, private, VPN, link-local and public
// addresses, and its other lines are sockets in other states, which the check ignores.
const procNetTCP = `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 0100007F:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 101 1 0000000000000000 100 0 0 10 0
   1: 00000000:0016 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 102 1 0000000000000000 100 0 0 10 0
   2: 0500000A:1538 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 103 1 0000000000000000 100 0 0 10 0
   3: 077100CB:0BB8 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 104 1 0000000000000000 100 0 0 10 0
   4: 09004064:0050 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 105 1 0000000000000000 100 0 0 10 0
   5: 010AFEA9:01BB 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 106 1 0000000000000000 100 0 0 10 0
   6: 0A01A8C0:0947 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 107 1 0000000000000000 100 0 0 10 0
   7: 077100CB:0947 0100007F:D2F0 01 00000000:00000000 00:00000000 00000000     0        0 108 1 0000000000000000 100 0 0 10 0
   8: 00000000:1F0A 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 109 1 0000000000000000 100 0 0 10 0
   9: 00000000:270F 00000000:0000 06 00000000:00000000 00:00000000 00000000     0        0 110 1 0000000000000000 100 0 0 10 0
`

// procNetTCP6 is /proc/net/tcp6 as the kernel writes it: IPv6 listeners, and an IPv4-mapped listener on each side of the
// private boundary.
const procNetTCP6 = `  sl  local_address                         remote_address                        st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000000000000000000000000000:1F90 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 201 1 0000000000000000 100 0 0 10 0
   1: 00000000000000000000000001000000:0BB8 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 202 1 0000000000000000 100 0 0 10 0
   2: 0000000000000000FFFF00000500000A:0050 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 203 1 0000000000000000 100 0 0 10 0
   3: 0000000000000000FFFF0000077100CB:01BB 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 204 1 0000000000000000 100 0 0 10 0
   4: B80D0120000000000000000001000000:01BB 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 205 1 0000000000000000 100 0 0 10 0
   5: 000000FD000000000000000001000000:1538 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 206 1 0000000000000000 100 0 0 10 0
`

// TestMain gives the whole package an empty host, so no test depends on the sockets of the machine that runs it: a test that
// counts alerts would otherwise see the public listeners of a developer's workstation. Tests that need host files set them
// with setHostFiles.
func TestMain(m *testing.M) {
	hostFiles = fstest.MapFS{}
	os.Exit(m.Run())
}

// setHostFiles makes the host's files these for the rest of the test. The names are those the host checks read, relative to
// the host's root, such as proc/net/tcp.
func setHostFiles(t *testing.T, files map[string]string) {
	t.Helper()
	m := fstest.MapFS{}
	for name, body := range files {
		m[name] = &fstest.MapFile{Data: []byte(body)}
	}
	saved := hostFiles
	hostFiles = m
	t.Cleanup(func() { hostFiles = saved })
}

// updateNotifierFiles are the host files of a host that provides the reboot mechanism, with the TCP tables given.
func updateNotifierFiles(tcp string) map[string]string {
	return map[string]string{
		"proc/net/tcp": tcp,
		"usr/share/update-notifier/notify-reboot-required": "",
	}
}

// listenerStrings renders the listeners as host:port, in order.
func listenerStrings(ls []listener) []string {
	out := make([]string, 0, len(ls))
	for _, l := range ls {
		out = append(out, net.JoinHostPort(l.IP.String(), strconv.Itoa(int(l.Port))))
	}
	return out
}

func TestProcNetReadsTheListeningSockets(t *testing.T) {
	got, err := parseListeners(strings.NewReader(procNetTCP))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"127.0.0.1:8080", "0.0.0.0:22", "10.0.0.5:5432", "203.0.113.7:3000", "100.64.0.9:80",
		"169.254.10.1:443", "192.168.1.10:2375", "0.0.0.0:7946",
	}
	if strings.Join(listenerStrings(got), " ") != strings.Join(want, " ") {
		t.Fatalf("tcp listeners:\n got %v\nwant %v", listenerStrings(got), want)
	}

	got, err = parseListeners(strings.NewReader(procNetTCP6))
	if err != nil {
		t.Fatal(err)
	}
	want = []string{"[::]:8080", "[::1]:3000", "10.0.0.5:80", "203.0.113.7:443", "[2001:db8::1]:443", "[fd00::1]:5432"}
	if strings.Join(listenerStrings(got), " ") != strings.Join(want, " ") {
		t.Fatalf("tcp6 listeners:\n got %v\nwant %v", listenerStrings(got), want)
	}
}

func TestProcNetRejectsAMalformedAddress(t *testing.T) {
	if _, err := parseListeners(strings.NewReader("   0: 0100ZZZZ:1F90 00000000:0000 0A\n")); err == nil {
		t.Fatal("a malformed address was accepted")
	}
}

func TestPublicAddressFollowsTheHostRules(t *testing.T) {
	for addr, public := range map[string]bool{
		"127.0.0.1":     false,
		"::1":           false,
		"10.1.2.3":      false,
		"172.16.0.1":    false,
		"192.168.1.1":   false,
		"fd00::1":       false,
		"169.254.10.1":  false,
		"fe80::1":       false,
		"100.64.0.9":    false,
		"100.127.255.1": false,
		"0.0.0.0":       true,
		"::":            true,
		"203.0.113.7":   true,
		"100.128.0.1":   true,
		"2001:db8::1":   true,
	} {
		if got := publicAddress(net.ParseIP(addr)); got != public {
			t.Errorf("%s: public %v, want %v", addr, got, public)
		}
	}
}

// TestListenRuleIsPinned pins which addresses the agent accepts to listen on. The rule is shared with the public ports check,
// so a change to it shows here first.
func TestListenRuleIsPinned(t *testing.T) {
	base := Config{Project: "demo", Environment: "lab", Tokens: []Token{{Digest: Digest(monitoringToken("a")), Scope: "read"}}}
	for addr, ok := range map[string]bool{
		"127.0.0.1:9999":   true,
		"[::1]:9999":       true,
		"10.1.2.3:9999":    true,
		"172.16.0.1:9999":  true,
		"192.168.1.1:9999": true,
		"[fd00::1]:9999":   true,
		"100.64.0.1:9999":  true,
		"169.254.1.1:9999": false,
		"0.0.0.0:9999":     false,
		"203.0.113.1:9999": false,
		"224.0.0.1:9999":   false,
	} {
		c := base
		c.Listen = addr
		if err := c.Validate(); (err == nil) != ok {
			t.Errorf("%s: valid %v, error %v", addr, err == nil, err)
		}
	}
}

func TestPublicPortsWarnsAboutAPublishedDatabase(t *testing.T) {
	// docker-proxy listens on 0.0.0.0:5432 for the published port, so the same port shows as a listener and as a container's.
	setHostFiles(t, updateNotifierFiles(procNetTCP+"  10: 00000000:1538 00000000:0000 0A\n"))
	e, _ := doctorDockerContainers(t, map[string]string{}, `[{"Ports":[
		{"IP":"0.0.0.0","PrivatePort":5432,"PublicPort":5432,"Type":"tcp"},
		{"IP":"0.0.0.0","PrivatePort":53,"PublicPort":5353,"Type":"udp"},
		{"PrivatePort":6379,"Type":"tcp"}]}]`)
	res := publicPorts(context.Background(), e, "10.0.0.5:9999")
	if res.State != monitor.Warning {
		t.Fatalf("state %s, want warning: %+v", res.State, res)
	}
	if !strings.Contains(res.Detail, "5432 PostgreSQL on 0.0.0.0 (published by a container)") {
		t.Fatalf("detail does not name the database: %q", res.Detail)
	}
	if strings.Count(res.Detail, "5432 PostgreSQL") != 1 {
		t.Errorf("the port is listed more than once: %q", res.Detail)
	}
	if strings.Contains(res.Detail, "5353") {
		t.Errorf("a UDP port is listed: %q", res.Detail)
	}
	if !strings.Contains(res.Detail, "not proof that it is reachable") {
		t.Errorf("detail does not say a listening port may not be reachable: %q", res.Detail)
	}
}

func TestPublicPortsListsWebAndSSHWithoutWarning(t *testing.T) {
	setHostFiles(t, map[string]string{"proc/net/tcp": `  sl  local_address rem_address   st
   0: 00000000:0016 00000000:0000 0A
   1: 00000000:0050 00000000:0000 0A
   2: 00000000:01BB 00000000:0000 0A
   3: 0500000A:1538 00000000:0000 0A
   4: 0100007F:1F90 00000000:0000 0A
`})
	e, _ := doctorDocker(t, map[string]string{})
	res := publicPorts(context.Background(), e, "10.0.0.5:9999")
	if res.State != monitor.OK {
		t.Fatalf("state %s, want ok: %+v", res.State, res)
	}
	for _, want := range []string{"22 SSH on 0.0.0.0", "80 HTTP on 0.0.0.0", "443 HTTPS on 0.0.0.0", "not proof that it is reachable"} {
		if !strings.Contains(res.Detail, want) {
			t.Errorf("detail lacks %q: %q", want, res.Detail)
		}
	}
	if strings.Contains(res.Detail, "5432") {
		t.Errorf("a private port is listed: %q", res.Detail)
	}
}

func TestPublicPortsIsOKWhenNothingIsPublic(t *testing.T) {
	setHostFiles(t, map[string]string{"proc/net/tcp": `  sl  local_address rem_address   st
   0: 0100007F:1F90 00000000:0000 0A
   1: 0500000A:1538 00000000:0000 0A
`})
	e, _ := doctorDocker(t, map[string]string{})
	res := publicPorts(context.Background(), e, "10.0.0.5:9999")
	if res.State != monitor.OK || res.Detail != "no port listens on a public address" {
		t.Fatalf("nothing public: %+v", res)
	}
}

func TestPublicPortsNamesTheAgentsOwnPort(t *testing.T) {
	setHostFiles(t, map[string]string{"proc/net/tcp": `  sl  local_address rem_address   st
   0: 00000000:270F 00000000:0000 0A
`})
	e, _ := doctorDocker(t, map[string]string{})
	res := publicPorts(context.Background(), e, "10.0.0.5:9999")
	if res.State != monitor.Warning || !strings.Contains(res.Detail, "9999 berthy agent on 0.0.0.0") {
		t.Fatalf("agent port: %+v", res)
	}
}

func TestPublicPortsIsUnknownWhenASourceIsMissing(t *testing.T) {
	setHostFiles(t, map[string]string{"proc/net/tcp": `  sl  local_address rem_address   st
   0: 00000000:0016 00000000:0000 0A
`})
	res := publicPorts(context.Background(), nil, "10.0.0.5:9999")
	if res.State != monitor.Unknown || !strings.Contains(res.Detail, "Not checked: container ports") {
		t.Fatalf("without Docker: %+v", res)
	}

	setHostFiles(t, map[string]string{})
	e, _ := doctorDocker(t, map[string]string{})
	res = publicPorts(context.Background(), e, "10.0.0.5:9999")
	if res.State != monitor.Unknown || !strings.Contains(res.Detail, "Not checked: host sockets") {
		t.Fatalf("without /proc: %+v", res)
	}
}

// A kernel without IPv6 has no tcp6 file, which is not a missing source: the check still answers.
func TestPublicPortsAcceptsAKernelWithoutIPv6(t *testing.T) {
	setHostFiles(t, map[string]string{"proc/net/tcp": `  sl  local_address rem_address   st
   0: 00000000:0016 00000000:0000 0A
`})
	e, _ := doctorDocker(t, map[string]string{})
	res := publicPorts(context.Background(), e, "10.0.0.5:9999")
	if res.State != monitor.OK {
		t.Fatalf("without tcp6: %+v", res)
	}
}

func TestDoctorShowsTheHostChecksAsWarnings(t *testing.T) {
	files := updateNotifierFiles(procNetTCP)
	files["var/run/reboot-required"] = ""
	files["var/run/reboot-required.pkgs"] = "libc6\n"
	setHostFiles(t, files)
	e, _ := doctorDockerContainers(t, map[string]string{}, `[{"Ports":[{"IP":"0.0.0.0","PrivatePort":5432,"PublicPort":5432,"Type":"tcp"}]}]`)
	checks := doctorChecks(t, NewDeploymentHandler(monitoringConfig(""), e, nil, nil))
	ports, ok := findCheck(checks, "public_ports")
	if !ok || ports.OK || ports.Critical || !strings.Contains(ports.Detail, "5432 PostgreSQL on 0.0.0.0 (published by a container)") {
		t.Fatalf("public_ports: %+v found %v", ports, ok)
	}
	reboot, ok := findCheck(checks, "reboot_required")
	if !ok || reboot.OK || reboot.Critical || reboot.Detail != "reboot required for libc6" {
		t.Fatalf("reboot_required: %+v found %v", reboot, ok)
	}
}

func TestRebootRequiredWarnsAndListsThePackages(t *testing.T) {
	files := updateNotifierFiles("")
	files["var/run/reboot-required"] = "*** System restart required ***\n"
	files["var/run/reboot-required.pkgs"] = "linux-image-6.8.0-1\nlibc6\nlibc6\n"
	setHostFiles(t, files)
	res := rebootResult(hostFiles)
	if res.State != monitor.Warning || res.Detail != "reboot required for libc6, linux-image-6.8.0-1" {
		t.Fatalf("pending reboot: %+v", res)
	}
}

func TestRebootRequiredIsOKWithoutTheFile(t *testing.T) {
	setHostFiles(t, updateNotifierFiles(""))
	res := rebootResult(hostFiles)
	if res.State != monitor.OK || res.Detail != "no reboot pending" {
		t.Fatalf("no reboot file: %+v", res)
	}
}

func TestRebootRequiredWithoutPackageListSaysOnlyThatARebootIsPending(t *testing.T) {
	files := updateNotifierFiles("")
	files["var/run/reboot-required"] = "*** System restart required ***\n"
	setHostFiles(t, files)
	res := rebootResult(hostFiles)
	if res.State != monitor.Warning || res.Detail != "reboot required" {
		t.Fatalf("reboot without packages: %+v", res)
	}
}

func TestRebootCheckIsSkippedWhereTheHostHasNoMechanism(t *testing.T) {
	setHostFiles(t, map[string]string{"proc/net/tcp": ""})
	if rebootMechanism(hostFiles) {
		t.Fatal("a host without the update notifier has the mechanism")
	}
	for _, c := range hostChecks(nil, "10.0.0.5:9999") {
		if c.Name() == "reboot_required" {
			t.Fatal("reboot check on a host without the mechanism")
		}
	}
}

func TestMonitorChecksTheHostOnTenMinutes(t *testing.T) {
	setHostFiles(t, updateNotifierFiles(""))
	c := &Cluster{}
	found := map[string]bool{}
	for _, c := range c.checks(context.Background()) {
		if c.Name() != "public_ports" && c.Name() != "reboot_required" {
			continue
		}
		found[c.Name()] = true
		if c.Interval() != 10*time.Minute || c.Failures() != 1 {
			t.Errorf("%s settings: %v %d", c.Name(), c.Interval(), c.Failures())
		}
	}
	if !found["public_ports"] || !found["reboot_required"] {
		t.Fatalf("host checks missing from the monitor: %v", found)
	}
}
