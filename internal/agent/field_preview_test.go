package agent

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"berthy/internal/agentclient"
	"berthy/internal/api"
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
		body := deployBodyWith(t, true)
		id := recordPreviewRelease(t, f.s, "initial", previewManifest(t, []byte(body)))
		db, err := sql.Open("sqlite", filepath.Join(f.dir, "state.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		if _, err := db.Exec(`UPDATE releases SET manifest=? WHERE id=?`, []byte(`{"apps":[]}`), id); err != nil {
			t.Fatal(err)
		}
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

func TestDeployFieldPreviewReadsBothLargeReleases(t *testing.T) {
	f := newApprovalFixture(t, false, nil)
	values := func(part string) (body []byte, value string) {
		value = strings.Repeat(part, 150<<10)
		body = previewRequest(t, func(app *manifest.App) {
			app.Env.Plain = map[string]string{"ALPHA": value, "BETA": value, "GAMMA": value, "OMEGA": value}
		})
		if len(body) >= api.MaxRequestBody {
			t.Fatalf("request is %d bytes", len(body))
		}
		return body, value
	}
	before, oldValue := values("a")
	next, newValue := values("b")
	baseID := recordPreviewRelease(t, f.s, "initial", previewManifest(t, before))
	server := httptest.NewServer(f.h)
	t.Cleanup(server.Close)
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	base, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	call := func(query string) (agentclient.Result, error) {
		return agentclient.DoResult(ctx, server.Client(), base, apiPrefix(f.cfg), apDeployToken, "POST", "deploys", query, next)
	}
	compact, err := call("")
	if err != nil || compact.Status != http.StatusOK || len(compact.Data) >= 1<<20 {
		t.Fatalf("compact preview: status %d, %d bytes, %v", compact.Status, len(compact.Data), err)
	}
	result, err := call("diff=fields")
	if err != nil || result.Status != http.StatusOK || len(result.Data) <= 1<<20 {
		t.Fatalf("field preview: status %d, %d bytes, %v", result.Status, len(result.Data), err)
	}
	preview := readFieldPreview(t, result.Data)
	if *preview.ReleaseDiff.Base != baseID || len(preview.ReleaseDiff.Fields) != 4 {
		t.Fatalf("field preview base %d, rows %d", *preview.ReleaseDiff.Base, len(preview.ReleaseDiff.Fields))
	}
	for i, name := range []string{"ALPHA", "BETA", "GAMMA", "OMEGA"} {
		row := preview.ReleaseDiff.Fields[i]
		if !slices.Equal(row.Path, []string{"env", name}) || row.Before == nil || *row.Before != oldValue || row.After == nil || *row.After != newValue {
			t.Fatalf("incomplete row for %s: path %q", name, row.Path)
		}
	}
	f.noOperation()
}

type fieldPreviewTransport func(*http.Request) (*http.Response, error)

func (f fieldPreviewTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type previewWhitespace struct{}

func (previewWhitespace) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = ' '
	}
	return len(p), nil
}

func TestDeployFieldPreviewResponseLimit(t *testing.T) {
	f := newApprovalFixture(t, false, nil)
	const role = "application-with-a-long-service-role-name"
	const count = 2048
	request := func(value string) []byte {
		return previewRequest(t, func(app *manifest.App) {
			options := make(map[string]string, count)
			for i := range count {
				options[fmt.Sprintf("option.%04d", i)] = value
			}
			app.Roles = map[string]manifest.Role{role: {Logging: &manifest.Logging{Driver: "json-file", Options: options}}}
		})
	}
	before, after := request("old"), request("new")
	recordPreviewRelease(t, f.s, "initial", previewManifest(t, before))
	w := f.call("POST", fieldPreviewPath, apDeployToken, string(after))
	if w.Code != http.StatusOK {
		t.Fatalf("field preview: %d %s", w.Code, w.Body.String())
	}
	raw := w.Body.Bytes()
	preview := readFieldPreview(t, raw)
	pathPrefix := []byte(`"path":["roles","` + role + `","logging","options","option.`)
	sides := []byte(`"before":"old","after":"new"`)
	if len(preview.ReleaseDiff.Fields) != count || bytes.Count(raw, pathPrefix) != count || bytes.Count(raw, sides) != count {
		t.Fatalf("expected %d encoded paths and both sides in %d bytes", count, len(raw))
	}
	if len(raw) <= len(before)+len(after) {
		t.Fatalf("repeated paths should make preview (%d bytes) larger than the requests (%d bytes)", len(raw), len(before)+len(after))
	}

	for _, tc := range []struct {
		name, query   string
		status, limit int
		document      []byte
		wantInvalid   bool
	}{
		{"at limit", "diff=fields", http.StatusOK, api.MaxDeployDiffResponse, raw, false},
		{"one byte over", "diff=fields", http.StatusOK, api.MaxDeployDiffResponse + 1, raw, true},
		{"ordinary", "", http.StatusOK, api.MaxDeployDiffResponse, raw, true},
		{"error response", "diff=fields", http.StatusBadRequest, api.MaxDeployDiffResponse, raw, true},
		{"admission", "diff=fields", http.StatusAccepted, api.MaxDeployDiffResponse, raw, true},
		{"truncated", "diff=fields", http.StatusOK, 7, []byte(`{"app":`), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &http.Client{Transport: fieldPreviewTransport(func(*http.Request) (*http.Response, error) {
				body := io.MultiReader(bytes.NewReader(tc.document), io.LimitReader(previewWhitespace{}, int64(tc.limit-len(tc.document))))
				return &http.Response{StatusCode: tc.status, Body: io.NopCloser(body)}, nil
			})}
			base := &url.URL{Scheme: "http", Host: "agent"}
			result, err := agentclient.DoResult(t.Context(), client, base, apiPrefix(f.cfg), apDeployToken, "POST", "deploys", tc.query, after)
			if tc.wantInvalid {
				if !errors.Is(err, agentclient.ErrInvalidResponse) {
					t.Fatalf("read %d bytes, error = %v, want ErrInvalidResponse", len(result.Data), err)
				}
			} else if err != nil || len(result.Data) != tc.limit || !bytes.HasPrefix(result.Data, raw) {
				t.Fatalf("read %d bytes at rich limit, error = %v", len(result.Data), err)
			}
		})
	}
}
