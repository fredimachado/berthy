package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"berthy/internal/agent"
	"berthy/internal/api"
	"berthy/internal/version"
)

// doctorVersionServer answers the doctor with the given agent version as the agent check's detail, or without an agent
// check when agentDetail is empty. When failing is set, the answer also holds a critical check that is not OK.
func doctorVersionServer(t *testing.T, agentDetail string, failing bool) *httptest.Server {
	t.Helper()
	checks := []string{}
	if agentDetail != "" {
		checks = append(checks, fmt.Sprintf(`{"name":"agent","ok":true,"critical":true,"detail":%q}`, agentDetail))
	}
	checks = append(checks, `{"name":"host","ok":true,"critical":true,"detail":"lab"}`)
	if failing {
		checks = append(checks, `{"name":"network","ok":false,"critical":true,"detail":"lab-net: missing"}`)
	}
	body := `{"checks":[` + strings.Join(checks, ",") + `]}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, body)
	}))
	t.Cleanup(server.Close)
	return server
}

// stampCLI sets the CLI's version for the test and returns the function that restores it.
func stampCLI(t *testing.T, v string) {
	t.Helper()
	saved := version.Version
	version.Version = v
	t.Cleanup(func() { version.Version = saved })
}

func TestDoctorPrintsTheCLIAndTheAgentVersion(t *testing.T) {
	stampCLI(t, "v1.0.0")
	server := doctorVersionServer(t, "v1.0.0", false)
	var out, errout bytes.Buffer
	if code := Run([]string{"doctor", "-e", "dev", "--config", ingressCLIConfig(t, server.URL)}, &out, &errout); code != 0 {
		t.Fatalf("exit %d: %s", code, errout.String())
	}
	if !strings.HasPrefix(out.String(), "cli: v1.0.0\nagent: v1.0.0\n") || strings.Contains(out.String(), "note:") {
		t.Fatalf("output:\n%s", out.String())
	}
}

func TestDoctorNotesADifferentReleaseOfTheAgent(t *testing.T) {
	stampCLI(t, "v1.0.0")
	server := doctorVersionServer(t, "v0.9.0", false)
	var out, errout bytes.Buffer
	if code := Run([]string{"doctor", "-e", "dev", "--config", ingressCLIConfig(t, server.URL)}, &out, &errout); code != 0 {
		t.Fatalf("exit %d: %s", code, errout.String())
	}
	want := "note: the agent runs v0.9.0, this CLI is v1.0.0; berthy setup upgrades the agent"
	if !strings.Contains(out.String(), "agent: v0.9.0\n") || !strings.Contains(out.String(), want+"\n") {
		t.Fatalf("output:\n%s", out.String())
	}
	if strings.Contains(errout.String(), "note:") {
		t.Fatalf("the note went to stderr in text mode: %q", errout.String())
	}
}

func TestDoctorNotesWhenTheAgentIsNewer(t *testing.T) {
	stampCLI(t, "v1.0.0")
	server := doctorVersionServer(t, "v1.1.0", false)
	var out, errout bytes.Buffer
	if code := Run([]string{"doctor", "-e", "dev", "--config", ingressCLIConfig(t, server.URL)}, &out, &errout); code != 0 {
		t.Fatalf("exit %d: %s", code, errout.String())
	}
	want := "note: the agent runs v1.1.0, this CLI is v1.0.0; upgrade this CLI"
	if !strings.Contains(out.String(), "agent: v1.1.0\n") || !strings.Contains(out.String(), want+"\n") {
		t.Fatalf("output:\n%s", out.String())
	}
	if strings.Contains(out.String(), "berthy setup upgrades the agent") {
		t.Fatalf("recommended a setup upgrade for a newer agent:\n%s", out.String())
	}
}

func TestDoctorNotesWhenTheAgentHasBuildMetadata(t *testing.T) {
	stampCLI(t, "v1.0.0")
	server := doctorVersionServer(t, "v1.1.0+build.1", false)
	var out, errout bytes.Buffer
	if code := Run([]string{"doctor", "-e", "dev", "--config", ingressCLIConfig(t, server.URL)}, &out, &errout); code != 0 {
		t.Fatalf("exit %d: %s", code, errout.String())
	}
	want := "note: the agent runs v1.1.0+build.1, this CLI is v1.0.0; upgrade this CLI"
	if !strings.Contains(out.String(), "agent: v1.1.0+build.1\n") || !strings.Contains(out.String(), want+"\n") {
		t.Fatalf("output:\n%s", out.String())
	}
	if strings.Contains(out.String(), "berthy setup upgrades the agent") {
		t.Fatalf("recommended a setup upgrade for a newer agent:\n%s", out.String())
	}
}

func TestDoctorSkipsNoteWhenOnlyBuildMetadataDiffers(t *testing.T) {
	stampCLI(t, "v1.3.0+build.1")
	server := doctorVersionServer(t, "v1.3.0+build.2", false)
	var out, errout bytes.Buffer
	if code := Run([]string{"doctor", "-e", "dev", "--config", ingressCLIConfig(t, server.URL)}, &out, &errout); code != 0 {
		t.Fatalf("exit %d: %s", code, errout.String())
	}
	if !strings.Contains(out.String(), "agent: v1.3.0+build.2\n") {
		t.Fatalf("output:\n%s", out.String())
	}
	if strings.Contains(out.String(), "note:") || strings.Contains(errout.String(), "note:") {
		t.Fatalf("noted equal-precedence builds:\n%s", out.String())
	}
}

func TestDoctorNeverWarnsAboutADevBuild(t *testing.T) {
	for _, tc := range []struct{ cli, agent string }{
		{"dev", "v0.9.0"},
		{"v1.0.0", "dev (8649d6e)"},
		{"v1.0.0", "dev"},
		{"dev", "dev (8649d6e)"},
		{"v1.0.0", "v1.0.0"},
	} {
		stampCLI(t, tc.cli)
		server := doctorVersionServer(t, tc.agent, false)
		var out, errout bytes.Buffer
		if code := Run([]string{"doctor", "-e", "dev", "--config", ingressCLIConfig(t, server.URL)}, &out, &errout); code != 0 {
			t.Fatalf("cli %s agent %s: exit %d: %s", tc.cli, tc.agent, code, errout.String())
		}
		if strings.Contains(out.String(), "note:") || strings.Contains(errout.String(), "note:") {
			t.Errorf("cli %s agent %s: warned: %q", tc.cli, tc.agent, out.String())
		}
	}
}

func TestDoctorSaysWhenAnOldAgentHasNoVersion(t *testing.T) {
	stampCLI(t, "v1.0.0")
	server := doctorVersionServer(t, "", false)
	var out, errout bytes.Buffer
	if code := Run([]string{"doctor", "-e", "dev", "--config", ingressCLIConfig(t, server.URL)}, &out, &errout); code != 0 {
		t.Fatalf("exit %d: %s", code, errout.String())
	}
	if !strings.Contains(out.String(), "agent: unknown\n") || strings.Contains(out.String(), "note:") {
		t.Fatalf("output:\n%s", out.String())
	}
}

func TestDoctorNoteKeepsTheExitCode(t *testing.T) {
	stampCLI(t, "v1.0.0")
	server := doctorVersionServer(t, "v0.9.0", true)
	var out, errout bytes.Buffer
	if code := Run([]string{"doctor", "-e", "dev", "--config", ingressCLIConfig(t, server.URL)}, &out, &errout); code != 1 {
		t.Fatalf("critical failure: exit %d, want 1: %s", code, out.String())
	}
	if !strings.Contains(out.String(), "note: the agent runs v0.9.0") {
		t.Fatalf("the note is missing with a failing check:\n%s", out.String())
	}
}

func TestDoctorJSONKeepsItsShapeAndSendsTheNoteToStderr(t *testing.T) {
	stampCLI(t, "v1.0.0")
	server := doctorVersionServer(t, "v0.9.0", false)
	var out, errout bytes.Buffer
	if code := Run([]string{"doctor", "-e", "dev", "--config", ingressCLIConfig(t, server.URL), "--json"}, &out, &errout); code != 0 {
		t.Fatalf("exit %d: %s", code, errout.String())
	}
	var v struct {
		Checks []struct {
			Name string `json:"name"`
		} `json:"checks"`
	}
	if err := json.Unmarshal(out.Bytes(), &v); err != nil || len(v.Checks) != 2 || v.Checks[0].Name != "agent" {
		t.Fatalf("stdout is not the agent's JSON: %v %q", err, out.String())
	}
	if strings.Contains(out.String(), "note:") || errout.String() != "note: the agent runs v0.9.0, this CLI is v1.0.0; berthy setup upgrades the agent\n" {
		t.Fatalf("stdout %q stderr %q", out.String(), errout.String())
	}
}

func TestReadClientTimeouts(t *testing.T) {
	if agent.DoctorRequestTimeout <= 0 {
		t.Fatalf("DoctorRequestTimeout = %s", agent.DoctorRequestTimeout)
	}
	for _, tc := range []struct {
		name, cmd                  string
		stream                     bool
		total, responseHeader      time.Duration
	}{
		{"doctor", "doctor", false, agent.DoctorRequestTimeout, agent.DoctorRequestTimeout},
		{"ps", "ps", false, 10 * time.Second, 20 * time.Second},
		{"logs --follow", "logs", true, 0, 20 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := newReadClient(tc.cmd, tc.stream)
			if client.Timeout != tc.total {
				t.Errorf("Timeout = %s, want %s", client.Timeout, tc.total)
			}
			got := client.Transport.(*http.Transport).ResponseHeaderTimeout
			if got != tc.responseHeader {
				t.Errorf("ResponseHeaderTimeout = %s, want %s", got, tc.responseHeader)
			}
		})
	}
}

// TestDoctorReceivesSlowTunnelWarning checks that a doctor report that takes longer than the old 10-second read limit
// still reaches the CLI: 9 seconds for a degraded tunnel, 4 for Compose and 8 for later checks before headers.
func TestDoctorReceivesSlowTunnelWarning(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		clientConn, peerConn := net.Pipe()
		defer clientConn.Close()
		defer peerConn.Close()

		peerErr := make(chan error, 1)
		go func() {
			var err error
			defer func() { peerErr <- err }()
			req, readErr := http.ReadRequest(bufio.NewReader(peerConn))
			if readErr != nil {
				err = readErr
				return
			}
			io.Copy(io.Discard, req.Body)
			req.Body.Close()
			time.Sleep(9 * time.Second)
			time.Sleep(4 * time.Second)
			time.Sleep(8 * time.Second)
			body := []byte(`{"checks":[{"name":"ingress_tunnel","ok":false,"critical":false,"detail":"unavailable"},{"name":"compose","ok":true,"critical":false,"detail":"v2.29.0"}]}`)
			resp := fmt.Sprintf("HTTP/1.1 200 OK\r\nContent-Length: %d\r\nContent-Type: application/json\r\n\r\n%s", len(body), body)
			_, err = io.WriteString(peerConn, resp)
		}()

		client := newReadClient("doctor", false)
		client.Transport.(*http.Transport).DialContext = func(context.Context, string, string) (net.Conn, error) {
			return clientConn, nil
		}
		u, err := url.Parse("http://127.0.0.1:9/doctor")
		if err != nil {
			t.Fatal(err)
		}
		var out, errout bytes.Buffer
		code := runReadRequest(context.Background(), client, u, "token", "doctor", false, true, &out, &errout)
		clientConn.Close()
		peerConn.Close()
		<-peerErr

		if code != 0 {
			t.Fatalf("exit %d, stderr %q, stdout %q", code, errout.String(), out.String())
		}
		var report struct {
			Checks []api.Check `json:"checks"`
		}
		if err := json.Unmarshal(out.Bytes(), &report); err != nil {
			t.Fatalf("decode stdout: %v (%q)", err, out.String())
		}
		if len(report.Checks) < 2 {
			t.Fatalf("checks: %+v", report.Checks)
		}
		tunnel, compose := report.Checks[0], report.Checks[1]
		if tunnel.Name != "ingress_tunnel" || tunnel.OK || tunnel.Critical || tunnel.Detail != "unavailable" {
			t.Fatalf("tunnel check: %+v", tunnel)
		}
		if compose.Name != "compose" || !compose.OK || compose.Detail != "v2.29.0" {
			t.Fatalf("compose check: %+v", compose)
		}
	})
}
