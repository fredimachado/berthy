package agent

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"berthy/internal/deploy"
	"berthy/internal/manifest"
	"berthy/internal/state"
)

const fieldPreviewPath = "/deploys?diff=fields"

type fieldPreviewAnswer struct {
	deploy.Plan
	ReleaseDiff struct {
		Base   *int64             `json:"base"`
		Fields []deploy.DiffField `json:"fields"`
	} `json:"release_diff"`
}

func recordPreviewRelease(t *testing.T, s *state.Store, id string, raw json.RawMessage) int64 {
	t.Helper()
	ctx := t.Context()
	op, _, err := s.Admit(ctx, state.Admission{RequestID: id, ActorID: "ci", Kind: "deploy", App: "api", PublicRequest: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	rel := &state.Release{App: "api", Manifest: raw, Digest: "ghcr.io/acme/api@" + apDigest}
	if err := s.Finish(ctx, op.ID, state.Outcome{State: "succeeded", Release: rel}); err != nil {
		t.Fatal(err)
	}
	applied, err := s.Applied(ctx, rel.App)
	if err != nil {
		t.Fatal(err)
	}
	return applied.ID
}

func previewRequest(t *testing.T, edit func(*manifest.App)) []byte {
	t.Helper()
	m, err := manifest.DecodeJSON([]byte(apManifestSource))
	if err != nil {
		t.Fatal(err)
	}
	if edit != nil {
		app := m.Apps["api"]
		edit(&app)
		m.Apps["api"] = app
	}
	if err := m.Validate(); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(deploy.Request{App: "api", Version: apDigest, Manifest: *m, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func previewManifest(t *testing.T, body []byte) json.RawMessage {
	t.Helper()
	var req struct{ Manifest json.RawMessage }
	if err := json.Unmarshal(body, &req); err != nil || req.Manifest == nil {
		t.Fatalf("request manifest missing: %v", err)
	}
	return req.Manifest
}

func readFieldPreview(t *testing.T, raw []byte) fieldPreviewAnswer {
	t.Helper()
	var p fieldPreviewAnswer
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("decode preview: %v", err)
	}
	if p.ReleaseDiff.Base == nil || p.ReleaseDiff.Fields == nil {
		t.Fatal("missing release_diff base or fields array")
	}
	return p
}

func TestDeployFieldPreviewUsesComparedReleaseID(t *testing.T) {
	f := newApprovalFixture(t, false, nil)
	body := deployBodyWith(t, true)
	first := f.call("POST", fieldPreviewPath, apDeployToken, body)
	if first.Code != http.StatusOK {
		t.Fatalf("first preview: %d %s", first.Code, first.Body.String())
	}
	base := recordPreviewRelease(t, f.s, "initial", []byte(apManifestSource))
	a := f.call("POST", fieldPreviewPath, apDeployToken, body)
	b := f.call("POST", fieldPreviewPath, apDeployToken, body)
	if a.Code != http.StatusOK || b.Code != http.StatusOK || !bytes.Equal(a.Body.Bytes(), b.Body.Bytes()) {
		t.Fatalf("previews: %d %s\n%d %s", a.Code, a.Body.String(), b.Code, b.Body.String())
	}
	p := readFieldPreview(t, a.Body.Bytes())
	if *p.ReleaseDiff.Base != base || p.Image != "ghcr.io/acme/api@"+apDigest {
		t.Fatalf("base/image: %d %s, want base %d", *p.ReleaseDiff.Base, p.Image, base)
	}
	fields, err := json.Marshal(p.ReleaseDiff.Fields)
	if err != nil || string(fields) != `[{"path":["env","FEATURE_X"],"after":"1"}]` {
		t.Fatalf("fields: %s (%v)", fields, err)
	}
	p = readFieldPreview(t, first.Body.Bytes())
	if *p.ReleaseDiff.Base != 0 || len(p.ReleaseDiff.Fields) != 0 || p.Diff == nil || !p.Diff.New {
		t.Fatalf("first preview: %s", first.Body.String())
	}
	f.noOperation()
}

func TestDeployFieldPreviewIsOptIn(t *testing.T) {
	// The pre-extension agent response, including every key and the final newline.
	ordinary := `{"app":"api","image":"ghcr.io/acme/api@` + apDigest + `","roles":["web"],"diff":{"same":true}}` + "\n"
	request := func(options map[string]string) []byte {
		return previewRequest(t, func(app *manifest.App) {
			r := app.Roles["web"]
			r.Logging = &manifest.Logging{Driver: "json-file", Options: options}
			app.Roles["web"] = r
		})
	}
	absent, empty := request(nil), request(map[string]string{"option.name": ""})
	for _, tc := range []struct {
		name, side   string
		before, next []byte
	}{
		{"add", "after", absent, empty},
		{"remove", "before", empty, absent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newApprovalFixture(t, false, nil)
			recordPreviewRelease(t, f.s, "initial", previewManifest(t, tc.before))
			control := f.call("POST", "/deploys", apDeployToken, string(tc.next))
			if control.Code != http.StatusOK || control.Body.String() != ordinary {
				t.Fatalf("ordinary preview: %d %s", control.Code, control.Body.String())
			}
			rich := f.call("POST", fieldPreviewPath, apDeployToken, string(tc.next))
			if rich.Code != http.StatusOK {
				t.Fatalf("field preview: %d %s", rich.Code, rich.Body.String())
			}
			p := readFieldPreview(t, rich.Body.Bytes())
			fields, err := json.Marshal(p.ReleaseDiff.Fields)
			if p.Diff == nil || !p.Diff.Same || err != nil || string(fields) !=
				`[{"path":["roles","web","logging","options","option.name"],"`+tc.side+`":""}]` {
				t.Fatalf("field preview: %s (%v)", rich.Body.String(), err)
			}
		})
	}
	t.Run("unreadable applied manifest", func(t *testing.T) {
		f := newApprovalFixture(t, false, nil)
		recordPreviewRelease(t, f.s, "invalid", []byte(`{"apps":[]}`))
		body := deployBodyWith(t, true)
		ordinary := f.call("POST", "/deploys", apDeployToken, body)
		if ordinary.Code != http.StatusOK || strings.Contains(ordinary.Body.String(), `"diff"`) {
			t.Fatalf("ordinary preview: %d %s", ordinary.Code, ordinary.Body.String())
		}
		rich := f.call("POST", fieldPreviewPath, apDeployToken, body)
		if rich.Code != http.StatusBadRequest || !strings.Contains(rich.Body.String(), "deploy field comparison unavailable") {
			t.Fatalf("unreadable comparison: %d %s", rich.Code, rich.Body.String())
		}
	})
	t.Run("held request", func(t *testing.T) {
		f := newApprovalFixture(t, true, nil)
		h := f.hold("POST", fieldPreviewPath, apAgentDeploy, deployBodyWith(t, false))
		if !bytes.Contains(h.Plan, []byte(`"diff":{"new":true}`)) || bytes.Contains(h.Plan, []byte(`"release_diff"`)) {
			t.Fatalf("held plan: %s", h.Plan)
		}
		f.noOperation()
	})
}
