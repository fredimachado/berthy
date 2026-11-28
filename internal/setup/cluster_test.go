package setup

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
)

type clusterSetupServer struct {
	*agentFake
	stateDir string
	probe    string
}

func (r clusterSetupServer) Run(ctx context.Context, script string, stdin io.Reader) (Result, error) {
	res, err := r.agentFake.Run(ctx, script, stdin)
	if strings.Contains(script, "# hosted path probe") && strings.Contains(script, shellQuote(r.stateDir)) {
		return Result{Stdout: []byte(r.probe)}, nil
	}
	return res, err
}

func TestSetupKeepsOneIndependentClusterKey(t *testing.T) {
	const clusterDir, clusterKey = "/var/lib/berthy/cluster/state", "/var/lib/berthy/cluster/key"
	exe := elfFile(t, emX86)
	server := clusterSetupServer{agentFake: freshAgentServer(t), stateDir: clusterDir}
	local := keyBytes()
	s := agentSession(t, server, exe, false, func(o *Options) {
		o.KeyFile = writeKey(t, local)
	})
	got, ok := runSteps(s)
	if !ok {
		t.Fatalf("fresh setup = %s", statuses(got))
	}
	gen, found := server.find("mv -f \"$tmp\" " + shellQuote(clusterKey))
	if !found || len(gen.stdin) != 0 || !strings.Contains(gen.script, "head -c 32 /dev/urandom") || !strings.Contains(gen.script, "chmod 0600") {
		t.Error("setup did not independently generate a private server key")
	}
	if upload, found := server.find("cat > \"$tmp\""); !found || !bytes.Equal(upload.stdin, local) || !strings.Contains(upload.script, shellQuote(keyPath)) {
		t.Error("--key-file did not upload the environment key on stdin")
	}
	if s.KeyFile() != keyPath {
		t.Fatal("key handover did not select the environment key")
	}
	cfg, found := server.find("cat > " + configPath + ".new")
	var file struct {
		Cluster struct {
			State string `json:"state_dir"`
			Key   string `json:"key_file"`
		} `json:"cluster"`
	}
	if !found || json.Unmarshal(cfg.stdin, &file) != nil || file.Cluster.State != clusterDir || file.Cluster.Key != clusterKey {
		t.Fatalf("configured cluster paths = %+v", file.Cluster)
	}

	installed := setUpAgent(t, exe)
	installed.config = strings.ReplaceAll(string(cfg.stdin), "/var/lib/berthy/cluster/", "/var/lib/berthy/server-alerts/")
	server = clusterSetupServer{agentFake: installed, stateDir: "/var/lib/berthy/server-alerts/state",
		probe: "state_dir_stat=" + wantPrivateDir + "\nstate_db=yes\nkey_stat=32 " + wantPrivate + "\nparent_0=" + wantPrivateDir + "\n"}
	s = agentSession(t, server, exe, false, nil)
	got, ok = runSteps(s)
	if !ok || stepOf(t, got, "key").Status != StatusOK || stepOf(t, got, "config").Status != StatusOK ||
		!server.ran("s="+shellQuote(server.stateDir)) || server.ran("head -c 32 /dev/urandom") {
		t.Fatalf("rerun did not preserve custom cluster paths and key: %s", statuses(got))
	}
}

func TestSetupDoesNotCreateAKeyOverExistingClusterState(t *testing.T) {
	exe := elfFile(t, emX86)
	server := clusterSetupServer{agentFake: setUpAgent(t, exe), stateDir: "/var/lib/berthy/cluster/state", probe: "state_db=yes\n"}
	s := agentSession(t, server, exe, false, nil)
	got, ok := runSteps(s)
	key := stepOf(t, got, "key")
	if ok || key.Status != StatusFailed || !strings.Contains(key.Detail, "restore the matching cluster key") {
		t.Fatalf("key step = %+v, want recovery error", key)
	}
	if server.ran("head -c 32 /dev/urandom") || server.ran("cat > "+configPath+".new") {
		t.Fatal("setup prepared new state configuration despite the missing cluster key")
	}
}
