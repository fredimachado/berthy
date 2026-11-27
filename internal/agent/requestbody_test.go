package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"berthy/internal/api"
	"berthy/internal/deploy"
	"berthy/internal/manifest"
	"berthy/internal/session"
	"berthy/internal/state"
)

type commandPlanRestorer struct{ deploy.VolumeRestorer }

func (commandPlanRestorer) CheckRestoreCommand(_ context.Context, key string) (deploy.RestorePlan, error) {
	return deploy.RestorePlan{Key: key, Backup: "command", App: "api"}, nil
}

func TestEveryBodyAClientSendsIsAccepted(t *testing.T) {
	rows := gateRows(t, false)
	fields := map[string]string{"deploy": "image", "rollback": "image", "restart": "roles", "scale": "roles", "exec": "exec", "run": "command", "prune": "roles", "remove": "remove", "destroy": "destroy", "restore": "restore", "apply": "changes", "ingress apply": "ingress", "secret set": "action", "secret delete": "action", "secret import": "action", "secret generate": "action", "secret generate declared": "action", "lock acquire": "action", "lock release": "action", "recovery resolve": "resolved", "cancel": "accepted"}
	for kind, row := range rows {
		t.Run(kind, func(t *testing.T) {
			f := row.fixture(t)
			row.setup(f)
			path := row.path
			if strings.Contains(path, "{id}") {
				op, err := f.s.Active(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				path = strings.ReplaceAll(path, "{id}", op.ID)
			}
			var body map[string]any
			if err := json.Unmarshal([]byte(row.sample), &body); err != nil {
				t.Fatal(err)
			}
			body["dry_run"] = true
			var payload any = body
			switch kind {
			case "deploy", "rollback", "restart", "scale", "exec", "run", "prune", "remove", "destroy", "ingress apply":
				payload = &deploy.Request{}
			case "apply":
				payload = &deploy.ApplyRequest{}
			case "secret import":
				payload = &api.SecretImport{}
			case "secret generate":
				payload = &api.SecretGenerate{}
			case "secret generate declared":
				payload = &api.SecretsGenerateDeclared{}
			}
			if payload != nil && reflect.ValueOf(payload).Kind() == reflect.Pointer {
				if err := json.Unmarshal([]byte(row.sample), payload); err != nil || !setDryRun(payload) {
					t.Fatalf("typed request: %v", err)
				}
			}
			encoded, err := json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
			w := f.call(row.method, path, apAdminToken, string(encoded))
			var got map[string]json.RawMessage
			if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &got) != nil || len(got[fields[kind]]) == 0 {
				t.Fatalf("%s: %d %s (want field %q)", kind, w.Code, w.Body.String(), fields[kind])
			}
		})
	}
	t.Run("image lookup", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Header().Set("Docker-Content-Digest", apDigest) }))
		t.Cleanup(srv.Close)
		host := strings.TrimPrefix(srv.URL, "http://")
		m, err := manifest.DecodeJSON([]byte(strings.Replace(apManifestSource, "ghcr.io/acme/api", host+"/acme/api", 1)))
		if err != nil {
			t.Fatal(err)
		}
		b, err := json.Marshal(api.ImageLookup{Manifest: *m, App: "api", Tag: "v1"})
		if err != nil {
			t.Fatal(err)
		}
		f := newApprovalFixture(t, false, nil)
		w := f.call("POST", "/images/resolve", apAdminToken, string(b))
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"image":`) {
			t.Fatalf("lookup: %d %s", w.Code, w.Body.String())
		}
	})
	t.Run("marshaled manifest shapes", func(t *testing.T) {
		input := `{"project":"demo","network":"overlay","apps":{"api":{"kind":"image","image":"ghcr.io/acme/api","roles":{"web":{"volumes":["uploads:/data"]}},"env":{"plain":{"ONE":"1"},"secret":["PASSWORD",{"TOKEN":"API_TOKEN"}],"template":{"URL":"${PASSWORD}"}}}},"destinations":{"offsite":{"type":"s3","endpoint":"http://127.0.0.1:9000","bucket":"bucket","access_key_secret":"ACCESS","secret_key_secret":"SECRET"}},"backups":{"uploads":{"app":"api","volume":"uploads","destination":"offsite","retention":{"keep":2}}}}`
		m, err := manifest.DecodeJSON([]byte(input))
		if err != nil {
			t.Fatal(err)
		}
		f := newApprovalFixture(t, false, nil)
		f.ctrl.Engine = gateEngine{}
		for _, name := range []string{"PASSWORD", "API_TOKEN", "ACCESS", "SECRET"} {
			if err := f.s.PutSecret(context.Background(), state.Actor{ID: "human"}, state.SecretInfo{Name: name}, []byte("value")); err != nil {
				t.Fatal(err)
			}
		}
		for _, tc := range []struct {
			path  string
			body  any
			field string
		}{
			{"/deploys", deploy.Request{App: "api", Version: apDigest, RequestID: "r", Manifest: *m, DryRun: true}, `"image":`},
			{"/apply", deploy.ApplyRequest{Manifest: *m, Commit: "c", DryRun: true}, `"changes":`},
		} {
			b, err := json.Marshal(tc.body)
			if err != nil {
				t.Fatal(err)
			}
			w := f.call("POST", tc.path, apAdminToken, string(b))
			if w.Code != 200 || !strings.Contains(w.Body.String(), tc.field) {
				t.Errorf("%s: %d %s", tc.path, w.Code, w.Body.String())
			}
		}
	})
	t.Run("generate with manifest and compose files", func(t *testing.T) {
		m, err := manifest.DecodeJSON([]byte(`{"project":"demo","network":"overlay","apps":{"db":{"kind":"compose","file":"compose.yml","env":{"secret":["DB_PASS"]}}},"secrets":{"DB_PASS":{"generate":{"format":"hex"}}}}`))
		if err != nil {
			t.Fatal(err)
		}
		b, err := json.Marshal(api.SecretGenerate{Name: "DB_PASS", Manifest: m, ComposeFiles: map[string]string{"db": "volumes: {data: {external: true, name: shared}}"}, DryRun: true})
		if err != nil {
			t.Fatal(err)
		}
		f := newApprovalFixture(t, false, nil)
		f.ctrl.Engine = &generateComposeEngine{volumes: map[string]bool{}}
		w := f.call("POST", "/secrets/generate", apAdminToken, string(b))
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"action":"secret generate"`) {
			t.Fatalf("generate: %d %s", w.Code, w.Body.String())
		}
	})
	t.Run("session size", func(t *testing.T) {
		f := newSessionFixture(t, false)
		b, err := json.Marshal(session.Request{Kind: "exec", App: "web", Command: []string{"sh"}, TTY: true, Size: &session.Size{Rows: 24, Cols: 80}})
		if err != nil {
			t.Fatal(err)
		}
		conn, _, resp := f.open(apAdminToken, nil, string(b))
		if resp.StatusCode != 101 {
			t.Fatalf("session: %d", resp.StatusCode)
		}
		conn.Close()
	})
	for _, body := range []string{"", `{"reason":"reason"}`} {
		t.Run("reject/"+body, func(t *testing.T) {
			f := newApprovalFixture(t, true, nil)
			h := f.hold("POST", "/lock/acquire", apAgentDeploy, `{"message":"x"}`)
			w := f.call("POST", "/approvals/"+h.ID+"/reject", apAdminToken, body)
			if w.Code != 200 || !strings.Contains(w.Body.String(), `"status":"rejected"`) {
				t.Fatalf("reject: %d %s", w.Code, w.Body.String())
			}
		})
	}
	t.Run("token and key requests", func(t *testing.T) {
		f := newApprovalFixture(t, false, nil)
		f.makeToken(`{"name":"victim","scope":"read"}`)
		for _, tc := range []struct{ method, path, body, field string }{
			{"POST", "/tokens", `{"name":"other","scope":"read","agent":false,"dry_run":true}`, `"action":"token create"`},
			{"DELETE", "/tokens/victim", `{"dry_run":true}`, `"action":"token revoke"`},
			{"POST", "/key/rotate", `{"dry_run":true}`, `"dry_run":true`},
			{"POST", "/secrets", `{"name":"NEW","value":"v","registry":"","dry_run":false}`, `"ok":true`},
			{"POST", "/secrets", `{"name":"OTHER","value":"v","registry":""}`, `"ok":true`},
		} {
			w := f.call(tc.method, tc.path, apAdminToken, tc.body)
			if w.Code != 200 || !strings.Contains(w.Body.String(), tc.field) {
				t.Errorf("%s %s: %d %s", tc.method, tc.path, w.Code, w.Body.String())
			}
		}
	})
	t.Run("lock release map", func(t *testing.T) {
		row := rows["lock release"]
		f := row.fixture(t)
		row.setup(f)
		b, err := json.Marshal(map[string]any{"message": "", "dry_run": true})
		if err != nil {
			t.Fatal(err)
		}
		w := f.call("POST", row.path, apAdminToken, string(b))
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"action":"lock release"`) {
			t.Fatalf("lock: %d %s", w.Code, w.Body.String())
		}
	})
	t.Run("restore command map", func(t *testing.T) {
		row := rows["restore"]
		f := row.fixture(t)
		row.setup(f)
		f.ctrl.Restorer = commandPlanRestorer{f.ctrl.Restorer}
		body := map[string]any{"key": "backup.gz", "app": "api", "service": "", "role": "web", "command": []string{"cat"}, "timeout": "", "dry_run": true, "request_id": "r"}
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		w := f.call("POST", "/restore", apAdminToken, string(b))
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"restore":`) {
			t.Fatalf("restore command: %d %s", w.Code, w.Body.String())
		}
	})
	for _, kind := range []string{"restart", "rollback"} {
		t.Run("tui execute/"+kind, func(t *testing.T) {
			row := rows[kind]
			f := row.fixture(t)
			row.setup(f)
			body := map[string]any{"app": "api", "request_id": "r", "dry_run": false}
			if kind == "restart" {
				body["role"] = "web"
			} else {
				body["target_release"] = 1
			}
			b, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			w := f.call("POST", row.path, apAdminToken, string(b))
			var result struct {
				ID string `json:"operation_id"`
			}
			if w.Code != 202 || json.Unmarshal(w.Body.Bytes(), &result) != nil || result.ID == "" {
				t.Fatalf("execute: %d %s", w.Code, w.Body.String())
			}
			if stream := f.waitTerminal(result.ID); !strings.Contains(stream, "event: terminal") {
				t.Fatalf("terminal: %s", stream)
			}
		})
	}
	t.Run("cancel map", func(t *testing.T) {
		f := newApprovalFixture(t, false, nil)
		id, _ := runningRun(t, f)
		body, err := json.Marshal(map[string]any{"dry_run": false})
		if err != nil {
			t.Fatal(err)
		}
		w := f.call("POST", "/operations/"+id+"/cancel", apAdminToken, string(body))
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"accepted":true`) {
			t.Fatalf("cancel: %d %s", w.Code, w.Body.String())
		}
		f.waitTerminal(id)
	})
}

func TestABodyThatIsNotAnObjectIsRefused(t *testing.T) {
	rows := gateRows(t, false)
	base := func(t *testing.T) *approvalFixture { return newApprovalFixture(t, false, nil) }
	rows["key rotate"] = gateRow{method: "POST", path: "/key/rotate", fixture: base, setup: func(*approvalFixture) {}, watched: func(f *approvalFixture) string { return string(f.keyBytes()) }}
	rows["token create"] = gateRow{method: "POST", path: "/tokens", fixture: base, setup: func(*approvalFixture) {}, watched: func(f *approvalFixture) string {
		v, err := f.s.Tokens(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		return fmt.Sprint(v)
	}}
	rows["token revoke"] = gateRow{method: "DELETE", path: "/tokens/victim", fixture: base, setup: func(f *approvalFixture) { f.makeToken(`{"name":"victim","scope":"read"}`) }, watched: func(f *approvalFixture) string {
		v, err := f.s.Tokens(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		return fmt.Sprint(v)
	}}
	for kind, row := range rows {
		for _, body := range []string{"null", "[]", `"x"`, "1"} {
			t.Run(kind+"/"+body, func(t *testing.T) {
				f := row.fixture(t)
				row.setup(f)
				path := row.path
				if strings.Contains(path, "{id}") {
					op, err := f.s.Active(t.Context())
					if err != nil {
						t.Fatal(err)
					}
					path = strings.ReplaceAll(path, "{id}", op.ID)
				}
				before := row.watched(f)
				w := f.call(row.method, path, apAdminToken, body)
				if w.Code != 400 || strings.TrimSpace(w.Body.String()) != `{"code":"bad_request","message":"invalid request"}` {
					t.Errorf("%s: %d %s", body, w.Code, w.Body.String())
				}
				if after := row.watched(f); after != before {
					t.Errorf("%s changed watched state: %q -> %q", body, before, after)
				}
			})
		}
	}
}

func TestSetDryRunFindsTheTagNameBeforeTheComma(t *testing.T) {
	a, b := &deploy.Request{}, &deploy.ApplyRequest{}
	c, d, e := &api.SecretImport{}, &api.SecretGenerate{}, &api.SecretsGenerateDeclared{}
	f := &struct {
		DryRun bool `json:"dry_run"`
	}{}
	for _, v := range []any{a, b, c, d, e, f} {
		if !setDryRun(v) {
			t.Errorf("not found in %T", v)
		}
	}
	if !a.DryRun || !b.DryRun || !c.DryRun || !d.DryRun || !e.DryRun || !f.DryRun {
		t.Fatal("field not set")
	}
	for _, v := range []any{&struct {
		DryRun bool `json:"dry_runs"`
	}{}, &struct {
		DryRun bool `json:"x,dry_run"`
	}{}, reflect.New(reflect.StructOf([]reflect.StructField{{Name: "dryRun", PkgPath: "agent", Type: reflect.TypeOf(false), Tag: `json:"dry_run"`}})).Interface()} {
		if setDryRun(v) {
			t.Errorf("matched %T", v)
		}
	}
}

func TestPlanningFailsClosedWithoutDryRun(t *testing.T) {
	r := asPlan(httptest.NewRequest("POST", "/", strings.NewReader(`{}`)))
	if err := decodeBody(httptest.NewRecorder(), r, &struct{}{}); !errors.Is(err, errNoDryRun) {
		t.Fatalf("planning without dry_run: %v", err)
	}
}
