package setup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func fakeSSH(t *testing.T, body string) *SSH {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture requires Unix")
	}
	path := filepath.Join(t.TempDir(), "ssh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	return &SSH{Dest: "ops@s1", Program: path}
}

func TestSSHTransportExit(t *testing.T) {
	for _, login := range []bool{false, true} {
		for _, code := range []string{"255", "1"} {
			t.Run(code+map[bool]string{true: "_login", false: "_run"}[login], func(t *testing.T) {
				r := fakeSSH(t, "echo private-out; echo private-err >&2; exit "+code)
				r.SetSudo(true)
				var res Result
				var err error
				if login {
					res, err = r.runSSHLogin(context.Background(), "id")
				} else {
					res, err = r.Run(context.Background(), "id", nil)
				}
				if code == "255" {
					var transport *SSHTransportError
					if !errors.As(err, &transport) || transport.Target != "ops@s1" || !strings.Contains(err.Error(), "ops@s1") || strings.Contains(err.Error(), "private") || len(res.Stdout) > 0 || len(res.Stderr) > 0 || res.Code != 0 {
						t.Fatalf("result=%+v err=%v", res, err)
					}
				} else if err != nil || res.Code != 1 || !strings.Contains(string(res.Stdout), "private-out") || !strings.Contains(string(res.Stderr), "private-err") {
					t.Fatalf("result=%+v err=%v", res, err)
				}
			})
		}
	}
	if runtime.GOOS == "linux" {
		res, err := (&Local{}).Run(context.Background(), "exit 255", nil)
		if err != nil || res.Code != 255 {
			t.Fatalf("local=%+v err=%v", res, err)
		}
	}
}

func TestSSHHostKeyAndRefusal(t *testing.T) {
	for _, tc := range []struct {
		diagnostic, want string
		hint             bool
	}{
		{"Host key verification failed.", "Host key verification failed", true},
		{"Connection refused", "Connection refused", false},
	} {
		t.Run(tc.diagnostic, func(t *testing.T) {
			r := fakeSSH(t, "echo '"+tc.diagnostic+"' >&2; exit 255")
			_, err := Connect(context.Background(), r)
			var transport *SSHTransportError
			if !errors.As(err, &transport) || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), "connect once with ssh ops@s1") != tc.hint {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestSSHKeepaliveTimeoutUsesSafeDiagnostic(t *testing.T) {
	r := fakeSSH(t, `echo 'Timeout, server private-server not responding. private-secret' >&2; echo private-stdout; exit 255`)
	res, err := r.Run(context.Background(), "id", nil)
	var transport *SSHTransportError
	if !errors.As(err, &transport) || err.Error() != "SSH connection to ops@s1 failed: Connection timed out" || len(res.Stdout) != 0 || len(res.Stderr) != 0 || res.Code != 0 {
		t.Fatalf("result=%+v err=%v", res, err)
	}
}

func TestSudoProbeConnectionFailure(t *testing.T) {
	r := fakeSSH(t, `case "$*" in *"sudo -n true"*) exit 255;; *) printf '1000\nops\n';; esac`)
	_, err := Connect(context.Background(), r)
	if err == nil || !strings.Contains(err.Error(), "SSH connection to ops@s1 failed") {
		t.Fatalf("err=%v", err)
	}
}

func TestRootLoginNeverExecutesSudo(t *testing.T) {
	dir, tools := shellFixture(t)
	shellTool(t, tools, "id", `case "$1" in -u) echo 0;; -un) echo root;; *) exit 1;; esac`)
	shellTool(t, tools, "sudo", `echo called > "$SUDO_MARKER"; exit 1`)
	t.Setenv("PATH", tools+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("SUDO_MARKER", filepath.Join(dir, "sudo-called"))
	r := fakeSSH(t, `for arg do last=$arg; done; exec /bin/sh -c "$last"`)
	login, err := Connect(context.Background(), r)
	if err != nil || login.User != "root" || login.Sudo || r.sudo {
		t.Fatalf("login=%+v err=%v", login, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "sudo-called")); !os.IsNotExist(err) {
		t.Fatalf("sudo executed or stat failed: %v", err)
	}
}

func TestRestartConnectionFailureLeavesRecoveryToServer(t *testing.T) {
	for _, staged := range []Staged{{Config: true}, {}} {
		t.Run(map[bool]string{true: "armed", false: "unarmed"}[staged.Config], func(t *testing.T) {
			calls := filepath.Join(t.TempDir(), "calls")
			r := fakeSSH(t, `case "$*" in *"systemctl restart berthy"*) echo restart >> '`+calls+`'; exit 255;; *"mv -f"*) echo swap >> '`+calls+`'; echo prev_cfg=yes;; *) echo later >> '`+calls+`';; esac`)
			err := Activate(context.Background(), r, "127.0.0.1:7710", staged, time.Second)
			b, e := os.ReadFile(calls)
			if e != nil {
				t.Fatal(e)
			}
			want := "restart\n"
			if staged.Config {
				want = "swap\nrestart\n"
			}
			var transport *SSHTransportError
			var activation *ActivateError
			if string(b) != want || !errors.As(err, &transport) || !errors.As(err, &activation) || (staged.Config && activation.ConfigOutcome != ConfigUnconfirmed) || !strings.Contains(err.Error(), "SSH connection to ops@s1 failed") {
				t.Fatalf("calls=%q err=%v", b, err)
			}
		})
	}
}
