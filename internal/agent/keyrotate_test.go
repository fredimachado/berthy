package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"berthy/internal/api"
	"berthy/internal/deploy"
	"berthy/internal/state"
)

func (f *approvalFixture) keyBytes() []byte {
	f.t.Helper()
	b, err := os.ReadFile(filepath.Join(f.dir, "key"))
	if err != nil {
		f.t.Fatal(err)
	}
	return b
}

func TestOnlyAnAdminPersonRotatesTheStateKey(t *testing.T) {
	for _, protected := range []bool{false, true} {
		f := newApprovalFixture(t, protected, nil)
		before, id := f.keyBytes(), f.s.KeyID()
		for token, want := range map[string]int{apReadToken: 403, apDeployToken: 403, apAgentRead: 403, apAgentDeploy: 403, apAgentAdmin: 403} {
			w := f.call("POST", "/key/rotate", token, `{}`)
			if w.Code != want {
				t.Errorf("protected=%v token %s: %d %s", protected, token[:12], w.Code, w.Body.String())
			}
			if token == apAgentAdmin && !strings.Contains(w.Body.String(), "an agent token cannot rotate the state key") {
				t.Errorf("protected=%v: an agent token was refused for another reason: %s", protected, w.Body.String())
			}
		}
		if got, _ := f.s.Approvals(context.Background(), "", f.now); len(got) != 0 {
			t.Fatalf("protected=%v: a refused rotation was held: %+v", protected, got)
		}
		if f.s.KeyID() != id || !bytes.Equal(f.keyBytes(), before) {
			t.Fatalf("protected=%v: a refused rotation changed the key", protected)
		}
		if w := f.call("GET", "/key/rotate", apAdminToken, ""); w.Code != http.StatusMethodNotAllowed {
			t.Errorf("GET: %d", w.Code)
		}

		w := f.call("POST", "/key/rotate", apAdminToken, `{}`)
		var rep api.KeyRotation
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &rep) != nil {
			t.Fatalf("protected=%v: admin: %d %s", protected, w.Code, w.Body.String())
		}
		if rep.OldKeyID != id || rep.NewKeyID != f.s.KeyID() || rep.NewKeyID == id || rep.KeyFile != filepath.Join(f.dir, "key") || rep.DryRun {
			t.Fatalf("report %+v (old key %s)", rep, id)
		}
		if bytes.Equal(f.keyBytes(), before) {
			t.Fatal("the key file is the old key")
		}
		if strings.Contains(w.Body.String(), string(f.keyBytes())) {
			t.Fatal("the answer holds the key")
		}
		var audited bool
		for _, line := range f.audit() {
			audited = audited || line == "alice key_rotate succeeded"
		}
		if !audited {
			t.Fatalf("audit: %v", f.audit())
		}
	}
}

func TestRotateKeyDryRunChangesNothing(t *testing.T) {
	f := newApprovalFixture(t, false, nil)
	if w := f.call("POST", "/secrets", apAdminToken, `{"name":"A","value":"v"}`); w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}
	before, id := f.keyBytes(), f.s.KeyID()
	w := f.call("POST", "/key/rotate", apAdminToken, `{"dry_run":true}`)
	var rep api.KeyRotation
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &rep) != nil {
		t.Fatalf("dry run: %d %s", w.Code, w.Body.String())
	}
	if !rep.DryRun || rep.OldKeyID != id || rep.NewKeyID != "" || rep.Values != 2 {
		t.Fatalf("dry run report %+v", rep)
	}
	if f.s.KeyID() != id || !bytes.Equal(f.keyBytes(), before) {
		t.Fatal("a dry run changed the key")
	}
	if _, err := os.Stat(filepath.Join(f.dir, "key.prev")); !os.IsNotExist(err) {
		t.Fatalf("a dry run kept a previous key: %v", err)
	}
}

func TestRotateKeyIsRefusedWhileTheEnvironmentIsLocked(t *testing.T) {
	f := newApprovalFixture(t, false, nil)
	if w := f.call("POST", "/lock/acquire", apAdminToken, `{"message":"freeze"}`); w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}
	id := f.s.KeyID()
	w := f.call("POST", "/key/rotate", apAdminToken, `{}`)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "the state key cannot be rotated while an operation runs or the deploy lock is held") {
		t.Fatalf("rotate while locked: %d %s", w.Code, w.Body.String())
	}
	if f.s.KeyID() != id {
		t.Fatal("a refused rotation changed the key")
	}
}

func TestDoctorSaysWhenAPreviousStateKeyIsKept(t *testing.T) {
	e, _ := doctorDocker(t, map[string]string{"demo": attachableOverlay})
	s := openIngressStore(t)
	recordRelease(t, s, "release-key", monitoredManifest(""))
	h := NewDeploymentHandler(monitoringConfig(""), e, nil, &deploy.Controller{Store: s, Environment: "lab"})
	if c, ok := findCheck(doctorChecks(t, h), "state_key"); ok {
		t.Fatalf("a state that was never rotated reports its key: %+v", c)
	}
	if _, err := s.RotateKey(context.Background(), state.Actor{ID: "admin", Name: "admin"}, false); err != nil {
		t.Fatal(err)
	}
	c, ok := findCheck(doctorChecks(t, h), "state_key")
	if !ok || !c.OK || c.Critical || c.Detail != "previous key kept until the next state snapshot succeeds" {
		t.Fatalf("after a rotation: %+v found %v", c, ok)
	}
}
