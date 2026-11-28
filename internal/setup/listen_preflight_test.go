package setup

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type listenReply struct {
	*agentFake
	result Result
	err    error
}

func (r *listenReply) Run(ctx context.Context, script string, in io.Reader) (Result, error) {
	if strings.Contains(script, "# setup listen port") {
		r.calls = append(r.calls, call{script: script})
		return r.result, r.err
	}
	return r.agentFake.Run(ctx, script, in)
}
func TestSetupRejectsOccupiedListenAddressBeforeProvisioning(t *testing.T) {
	exe := elfFile(t, emX86)
	for _, tc := range []struct {
		name   string
		result Result
		err    error
		detail string
	}{
		{"busy", Result{Stdout: []byte("busy\n")}, nil, "listen address 172.31.250.10:7710 is already in use"},
		{"empty", Result{}, nil, "could not check"},
		{"unknown", Result{Stdout: []byte("unknown\n")}, nil, "could not check"},
		{"exit", Result{Code: 1}, nil, "could not check"},
		{"runner error", Result{}, errors.New("connection lost"), "could not check"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := freshAgentServer(t)
			r := &listenReply{agentFake: f, result: tc.result, err: tc.err}
			s := agentSession(t, r, exe, false, nil)
			got, ok := runSteps(s)
			if ok || statuses(got) != "connect=ok check=failed" || !strings.Contains(lastResult(got).Detail, tc.detail) {
				t.Errorf("results: %+v", got)
			}
			if len(f.calls) == 0 || !strings.Contains(f.calls[len(f.calls)-1].script, "# setup listen port") {
				t.Errorf("last call is not the listen probe")
			}
			for _, c := range f.calls {
				if agentChange(c.script) {
					t.Errorf("provisioned: %s", c.script)
				}
			}
		})
	}
}
func TestSetupListenPortMatchesTheResolvedEndpoint(t *testing.T) {
	exe := elfFile(t, emX86)
	t.Run("configured active and stopped", func(t *testing.T) {
		for _, active := range []bool{true, false} {
			t.Run(map[bool]string{true: "active", false: "stopped"}[active], func(t *testing.T) {
				f := setUpAgent(t, exe)
				if !active {
					f.probe = without(f.probe, "service_active=")
				}
				r := &listenReply{agentFake: f, result: Result{Stdout: []byte("busy\n")}}
				s := agentSession(t, r, exe, false, nil)
				got, ok := runSteps(s)
				if active {
					if !ok || f.ran("# setup listen port") {
						t.Fatalf("active run: %+v", got)
					}
				} else if ok || lastResult(got).Step != "check" || !strings.Contains(lastResult(got).Detail, "already in use") {
					t.Fatalf("stopped run: %+v", got)
				}
			})
		}
	})
	t.Run("tcp tables", func(t *testing.T) {
		dir, _ := shellFixture(t)
		f := freshAgentServer(t)
		r := &listenReply{agentFake: f, result: Result{Stdout: []byte("free\n")}}
		s := agentSession(t, r, exe, false, nil)
		runSteps(s)
		c, found := f.find("# setup listen port")
		if !found {
			t.Fatal("listen probe was not sent")
		}
		tcp := filepath.Join(dir, "tcp")
		tcp6 := filepath.Join(dir, "tcp6")
		script := strings.NewReplacer("/proc/net/tcp6", tcp6, "/proc/net/tcp", tcp).Replace(c.script)
		for _, tc := range []struct{ name, row4, row6, want string }{
			{"exact", "0AFA1FAC:1E1E", "", "busy\n"}, {"wildcard", "00000000:1E1E", "", "busy\n"},
			{"other address", "0BFA1FAC:1E1E", "", "free\n"}, {"other port", "0AFA1FAC:1E1F", "", "free\n"},
			{"ipv6 wildcard", "", "00000000000000000000000000000000:1E1E", "busy\n"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				header := "  sl local_address rem_address st tx_queue rx_queue tr tm->when retrnsmt uid timeout inode\n"
				row := func(addr string) string {
					if addr == "" {
						return ""
					}
					return "0: " + addr + " 00000000:0000 0A 00000000:00000000 00:00000000 00000000 0 0 1\n"
				}
				if err := os.WriteFile(tcp, []byte(header+row(tc.row4)), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(tcp6, []byte(header+row(tc.row6)), 0600); err != nil {
					t.Fatal(err)
				}
				out, err := exec.Command("sh", "-c", script).CombinedOutput()
				if err != nil || string(out) != tc.want {
					t.Fatalf("out=%q err=%v want=%q", out, err, tc.want)
				}
			})
		}
	})
}
