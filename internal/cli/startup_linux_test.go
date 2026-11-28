package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"berthy/internal/agent"
	"berthy/internal/scope"
	"berthy/internal/state"
)

func TestAgentPublishesEveryStartupResultBeforeServing(t *testing.T) {
	if os.Getenv("BERTHY_TEST_STARTUP_CHILD") == "1" {
		os.Exit(Run(os.Args[len(os.Args)-3:], io.Discard, os.Stderr))
	}
	if os.Geteuid() != 0 {
		t.Skip("the startup file fixture needs root ownership")
	}
	root := t.TempDir()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	f := agent.File{Listen: listener.Addr().String(), DockerSocket: filepath.Join(root, "no-docker"),
		Cluster: agent.ClusterConfig{StateDir: filepath.Join(root, "cluster-state"), KeyFile: filepath.Join(root, "cluster-key")}}
	if err := os.WriteFile(f.Cluster.KeyFile, bytes.Repeat([]byte{2}, 32), 0600); err != nil {
		t.Fatal(err)
	}
	ids := []scope.ID{{Project: "shop", Environment: "dev"}, {Project: "demo", Environment: "dev"}, {Project: "tools", Environment: "dev"}}
	var failedToken string
	for i, id := range ids {
		token, digest, err := agent.NewToken(id)
		if err != nil {
			t.Fatal(err)
		}
		entry := agent.Hosted{Project: id.Project, Environment: id.Environment,
			StateDir: filepath.Join(root, id.Project), KeyFile: filepath.Join(root, id.Project+".key"),
			Tokens: []agent.Token{{Name: "reader", Scope: "read", Digest: digest}}}
		if err := os.WriteFile(entry.KeyFile, make([]byte, 32), 0600); err != nil {
			t.Fatal(err)
		}
		if i == 1 {
			failedToken = token
			store, err := state.Open(entry.StateDir, entry.KeyFile, entry.Environment)
			if err != nil {
				t.Fatal(err)
			}
			store.Close()
			if err := os.WriteFile(entry.KeyFile, bytes.Repeat([]byte{1}, 32), 0600); err != nil {
				t.Fatal(err)
			}
		}
		f.Environments = append(f.Environments, entry)
	}
	raw, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(root, "agent.json")
	if err = os.WriteFile(config, raw, 0600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "startup")
	if err = os.Mkdir(dir, 0750); err != nil {
		t.Fatal(err)
	}
	result := filepath.Join(dir, "result")
	if err = os.WriteFile(result, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(result, 0620); err != nil {
		t.Fatal(err)
	}
	const invocation = "0123456789abcdef0123456789abcdef"
	cmd := exec.Command(os.Args[0], "-test.run=^TestAgentPublishesEveryStartupResultBeforeServing$", "--", "agent", "--config", config)
	cmd.Env = append(os.Environ(), "BERTHY_TEST_STARTUP_CHILD=1", "BERTHY_STARTUP_RESULT="+result, "INVOCATION_ID="+invocation)
	var logs bytes.Buffer
	cmd.Stderr = &logs
	listener.Close()
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	var runErr error
	go func() { runErr = cmd.Wait(); close(done) }()
	t.Cleanup(func() { _ = cmd.Process.Kill(); <-done })
	url := "http://" + f.Listen
	for {
		select {
		case <-done:
			t.Fatalf("agent exited before serving: %v\n%s", runErr, &logs)
		default:
		}
		resp, err := http.Get(url + "/healthz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("health status: %d", resp.StatusCode)
			}
			break
		}
		runtime.Gosched()
	}
	got, err := os.ReadFile(result)
	if err != nil {
		t.Fatal(err)
	}
	reason := state.ErrWrongKey.Error()
	want := "berthy-startup-v1\t" + invocation + "\t3\nshop/dev\tstarted\t\ndemo/dev\tfailed\t" + reason + "\ntools/dev\tstarted\t\ncomplete\n"
	if string(got) != want {
		t.Fatalf("startup result at first health request: got %q, want %q", got, want)
	}
	req, err := http.NewRequest(http.MethodGet, url+ids[1].APIPath()+"agent", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+failedToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var answer struct{ Code, Message string }
	if err := json.NewDecoder(resp.Body).Decode(&answer); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 503 || answer.Code != "environment_failed" || !strings.HasSuffix(answer.Message, ": "+reason) {
		t.Errorf("failed environment: %d %+v", resp.StatusCode, answer)
	}
	for _, name := range []string{"symlink", "wrong-mode", "nonempty"} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(dir, name)
			if name == "symlink" {
				if err := os.Symlink(result, path); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.WriteFile(path, []byte("keep"), 0600); err != nil {
					t.Fatal(err)
				}
				if name == "nonempty" {
					if err := os.Chmod(path, 0620); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := publishStartupResult(nil, path, invocation); err == nil {
				t.Fatalf("accepted %s as a prepared result", name)
			}
			contents, err := os.ReadFile(path)
			want := "keep"
			if name == "symlink" {
				want = string(got)
			}
			if err != nil || string(contents) != want {
				t.Fatalf("refused result changed: %q, %v", contents, err)
			}
		})
	}
}
