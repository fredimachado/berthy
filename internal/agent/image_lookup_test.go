package agent

import (
	"berthy/internal/deploy"
	"berthy/internal/engine"
	"berthy/internal/state"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestImageLookupRequiresHumanAdminBeforeOutbound(t *testing.T) {
	for _, protected := range []bool{false, true} {
		f := newApprovalFixture(t, protected, nil)
		for _, path := range []string{"/images/resolve", "/images/tags"} {
			for _, token := range []string{apReadToken, apDeployToken, apAgentRead, apAgentDeploy, apAgentAdmin} {
				w := f.call(http.MethodPost, path, token, `{}`)
				if w.Code != http.StatusForbidden {
					t.Errorf("protected=%v path=%s: got %d, want 403", protected, path, w.Code)
				}
			}
		}
		f.noOperation()
	}
}

func TestAgentInfoDistinguishesUnavailableEffectivePolicy(t *testing.T) {
	f := newApprovalFixture(t, true, nil)
	w := f.call(http.MethodGet, "/agent", apReadToken, "")
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["protected"] != true || body["effective_protected"] != true {
		t.Fatalf("agent info: %v", body)
	}
	f2 := newApprovalFixture(t, false, nil)
	w = f2.call(http.MethodGet, "/agent", apReadToken, "")
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body["protected"] != false || body["effective_protected"] != false {
		t.Fatalf("unprotected agent info: %v, %v", body, err)
	}
	store := f2.ctrl.Store
	f2.ctrl.Store = nil
	w = f2.call(http.MethodGet, "/agent", apReadToken, "")
	f2.ctrl.Store = store
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body["effective_protected"] != nil {
		t.Fatalf("missing state agent info: %v, %v", body, err)
	}
	if err := f2.s.Close(); err != nil {
		t.Fatal(err)
	}
	w = f2.call(http.MethodGet, "/agent", apReadToken, "")
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if value, ok := body["effective_protected"]; !ok || value != nil {
		t.Fatalf("unavailable agent info: %v", body)
	}
}

func TestFirstProtectedManifestHoldsPinnedDigest(t *testing.T) {
	f := newApprovalFixture(t, false, nil)
	body := strings.Replace(deployBody(t, false), `"protected":false`, `"protected":true`, 1)
	f.hold(http.MethodPost, "/deploys", apAgentDeploy, body)
	f.noOperation()
}

func TestDeployRejectsAmbiguousFields(t *testing.T) {
	for _, protected := range []bool{false, true} {
		f := newApprovalFixture(t, protected, nil)
		body := strings.Replace(deployBody(t, false), `"dry_run":false`, `"dry_run":false,"DRY_RUN":true`, 1)
		// dry_run is omitted when false; inject both variants at the root.
		body = strings.Replace(body, `{`, `{"dry_run":false,"DRY_RUN":true,`, 1)
		w := f.call(http.MethodPost, "/deploys", apAgentDeploy, body)
		if w.Code != http.StatusBadRequest {
			t.Errorf("protected=%v: got %d", protected, w.Code)
		}
		f.noOperation()
	}
}

type lookupEngine struct {
	deploy.Engine
	calls int
}

func (e *lookupEngine) ResolveImage(_ context.Context, _ string, _ *engine.RegistryAuth) (string, error) {
	e.calls++
	return apDigest, nil
}

func TestLookupHumanAndBoundCredential(t *testing.T) {
	f := newApprovalFixture(t, false, nil)
	e := &lookupEngine{}
	f.ctrl.Engine = e
	request := `{"manifest":` + apManifestSource + `,"app":"api","tag":"v1"}`
	w := f.call(http.MethodPost, "/images/resolve", apAdminToken, request)
	var result struct{ Image, Tag string }
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &result) != nil || result.Image != "ghcr.io/acme/api@"+apDigest || result.Tag != "v1" || e.calls != 1 {
		t.Fatalf("lookup: status=%d result=%+v calls=%d", w.Code, result, e.calls)
	}
	privateManifest := strings.Replace(apManifestSource, `"network":"overlay"`, `"network":"overlay","registries":{"ghcr.io":{"username":"user","password_secret":"REGISTRY_PASSWORD"}}`, 1)
	request = `{"manifest":` + privateManifest + `,"app":"api","tag":"v1"}`
	w = f.call(http.MethodPost, "/images/resolve", apAdminToken, request)
	if w.Code == http.StatusOK || e.calls != 1 {
		t.Fatalf("unbound secret: status=%d calls=%d", w.Code, e.calls)
	}
	if err := f.s.PutSecret(context.Background(), state.Actor{ID: "test", Name: "test"}, state.SecretInfo{Name: "REGISTRY_PASSWORD", Registry: "other.example"}, []byte("test-value")); err != nil {
		t.Fatal(err)
	}
	w = f.call(http.MethodPost, "/images/resolve", apAdminToken, request)
	if w.Code == http.StatusOK || e.calls != 1 {
		t.Fatalf("wrong host: status=%d calls=%d", w.Code, e.calls)
	}
	if err := f.s.PutSecret(context.Background(), state.Actor{ID: "test", Name: "test"}, state.SecretInfo{Name: "REGISTRY_PASSWORD", Registry: "ghcr.io"}, []byte("test-value")); err != nil {
		t.Fatal(err)
	}
	w = f.call(http.MethodPost, "/images/resolve", apAdminToken, request)
	if w.Code != http.StatusOK || e.calls != 2 {
		t.Fatalf("bound secret: status=%d calls=%d body=%s", w.Code, e.calls, w.Body.String())
	}
	f.noOperation()
}

func TestHeldPinReplansWithoutResolving(t *testing.T) {
	f := newApprovalFixture(t, true, nil)
	e := &lookupEngine{Engine: planEngine{}}
	f.ctrl.Engine = e
	body := strings.Replace(deployBody(t, false), `"request_id":`, `"version_tag":"v1","request_id":`, 1)
	h := f.hold(http.MethodPost, "/deploys", apAgentDeploy, body)
	if e.calls != 0 || !strings.Contains(string(h.Plan), apDigest) || !strings.Contains(string(h.Plan), `"tag":"v1"`) {
		t.Fatalf("plan=%s calls=%d", h.Plan, e.calls)
	}
	w := f.call(http.MethodGet, "/approvals", apAdminToken, "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), apDigest) {
		t.Fatalf("approval list status=%d", w.Code)
	}
	f.noOperation()
}

func TestDeployRejectsAmbiguousManifestFields(t *testing.T) {
	f := newApprovalFixture(t, true, nil)
	body := strings.Replace(deployBody(t, false), `"protected":false`, `"protected":false,"PROTECTED":true`, 1)
	w := f.call(http.MethodPost, "/deploys", apAgentDeploy, body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("got %d", w.Code)
	}
	f.noOperation()
}
