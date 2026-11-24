package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"berthy/internal/agentclient"
	"berthy/internal/deploy"
	"berthy/internal/manifest"
)

func TestPromoteUsesAppliedSnapshotAndSeparateTokens(t *testing.T) {
	a, b := strings.Repeat("a", 64), strings.Repeat("b", 64)
	sourceBody := fmt.Sprintf(`{"id":12,"digest":"ghcr.io/acme/api:old@sha256:%s","tag":"stable"}`, a)
	destBody := fmt.Sprintf(`{"id":7,"digest":"ghcr.io/acme/api@sha256:%s","tag":"older"}`, b)
	sourceStatus, destStatus := 200, 200
	requests := 0
	var mu sync.Mutex
	server := func(env, token string, body *string, status *int) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			defer mu.Unlock()
			requests++
			if r.Method != "GET" || r.URL.Path != "/v1/projects/demo/environments/"+env+"/applied/api" || r.Header.Get("Authorization") != "Bearer "+token {
				t.Errorf("unexpected method, path or credential for %s", env)
			}
			w.WriteHeader(*status)
			fmt.Fprint(w, *body)
		}))
	}
	src := server("staging", "source-secret", &sourceBody, &sourceStatus)
	defer src.Close()
	dst := server("prod", "dest-secret", &destBody, &destStatus)
	defer dst.Close()
	t.Setenv("PROMOTE_SRC_TOKEN", "source-secret")
	t.Setenv("PROMOTE_DST_TOKEN", "dest-secret")
	path := filepath.Join(t.TempDir(), "config.json")
	cfg := fmt.Sprintf(`{"projects":{"demo":{"environments":{"staging":{"url":%q,"token_env":"PROMOTE_SRC_TOKEN"},"prod":{"url":%q,"token_env":"PROMOTE_DST_TOKEN"}}}}}`, src.URL, dst.URL)
	if err := os.WriteFile(path, []byte(cfg), 0600); err != nil {
		t.Fatal(err)
	}
	target, err := agentclient.Resolve(path, agentclient.Selector{Project: "demo", Environment: "prod"})
	if err != nil {
		t.Fatal(err)
	}
	m := &manifest.Manifest{Apps: map[string]manifest.App{"api": {Kind: "image", Image: "ghcr.io/acme/api"}}}
	selectSnapshot := func() (promotionSnapshot, error) {
		return selectPromotion(context.Background(), http.DefaultClient, target, path, "staging", m, "api")
	}
	snap, err := selectSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	gotRequests := requests
	mu.Unlock()
	if snap.sourceID != 12 || snap.expectedID != 7 || snap.image != "ghcr.io/acme/api@sha256:"+a || snap.tag != "stable" || gotRequests != 2 {
		t.Fatalf("snapshot fields or request count were wrong: %d requests", gotRequests)
	}
	mu.Lock()
	sourceBody = fmt.Sprintf(`{"id":99,"digest":"ghcr.io/acme/api@sha256:%s"}`, b)
	destBody = `{"code":"not_deployed","message":"app is not deployed"}`
	destStatus = 404
	mu.Unlock()
	first, err := selectSnapshot()
	if err != nil || first.expectedID != 0 {
		t.Fatalf("initial destination expected ID %d, error %v", first.expectedID, err)
	}
	if snap.sourceID != 12 || snap.expectedID != 7 || snap.image != "ghcr.io/acme/api@sha256:"+a {
		t.Fatal("snapshot changed")
	}
	for _, tc := range []struct {
		name, src, dst string
		status         int
	}{
		{"source absent", `{"code":"not_deployed","message":"not deployed"}`, destBody, 404},
		{"wrong shape", `[]`, destBody, 200},
		{"duplicate id", `{"id":1,"ID":2,"digest":"x"}`, destBody, 200},
		{"bare digest", fmt.Sprintf(`{"id":1,"digest":"sha256:%s"}`, a), destBody, 200},
		{"repository mismatch", fmt.Sprintf(`{"id":1,"digest":"ghcr.io/other/api@sha256:%s"}`, a), destBody, 200},
		{"bad tag", fmt.Sprintf(`{"id":1,"digest":"ghcr.io/acme/api@sha256:%s","tag":"bad tag"}`, a), destBody, 200},
		{"equal", fmt.Sprintf(`{"id":1,"digest":"ghcr.io/acme/api@sha256:%s"}`, a), fmt.Sprintf(`{"id":2,"digest":"ghcr.io/acme/api@sha256:%s"}`, a), 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mu.Lock()
			sourceBody, sourceStatus, destBody, destStatus = tc.src, tc.status, tc.dst, 200
			if tc.dst == `{"code":"not_deployed","message":"app is not deployed"}` {
				destStatus = 404
			}
			mu.Unlock()
			if _, err := selectSnapshot(); err == nil {
				t.Fatal("expected refusal")
			}
		})
	}
}

func TestReadAppliedStrictResponses(t *testing.T) {
	body, status := "", http.StatusOK
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}))
	defer srv.Close()
	base, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	target := &agentclient.Target{Base: base, Prefix: "/", Token: "secret-token"}
	for _, tc := range []struct {
		name, body   string
		status       int
		found, valid bool
	}{
		{"valid", `{"id":1,"digest":"x","tag":""}`, 200, true, true},
		{"absent", `{"code":"not_deployed","message":"none"}`, 404, false, true},
		{"missing message", `{"code":"not_deployed"}`, 404, false, false},
		{"null message", `{"code":"not_deployed","message":null}`, 404, false, false},
		{"empty message", `{"code":"not_deployed","message":""}`, 404, false, false},
		{"wrong message", `{"code":"not_deployed","message":3}`, 404, false, false},
		{"empty digest", `{"id":1,"digest":""}`, 200, false, false},
		{"non-json", `oops`, 200, false, false},
		{"null", `null`, 200, false, false},
		{"trailing", `{"id":1,"digest":"x"}{}`, 200, false, false},
		{"fraction", `{"id":1.5,"digest":"x"}`, 200, false, false},
		{"zero", `{"id":0,"digest":"x"}`, 200, false, false},
		{"null digest", `{"id":1,"digest":null}`, 200, false, false},
		{"null tag", `{"id":1,"digest":"x","tag":null}`, 200, false, false},
		{"ambiguous absence", `{"code":"not_deployed","CODE":"not_deployed"}`, 404, false, false},
		{"other absence", `{"code":"missing"}`, 404, false, false},
		{"error token", `{"message":"secret-token"}`, 403, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mu.Lock()
			body, status = tc.body, tc.status
			mu.Unlock()
			_, found, err := readApplied(context.Background(), http.DefaultClient, target, "api")
			if (err == nil) != tc.valid || found != tc.found || err != nil && strings.Contains(err.Error(), "secret-token") {
				t.Fatalf("found=%v err=%v", found, err)
			}
		})
	}
}

func TestRunPromotePreview(t *testing.T) {
	image := "ghcr.io/acme/api@sha256:" + strings.Repeat("a", 64)
	for _, tc := range []struct {
		name, reply string
		status      int
		dry, asJSON bool
		want        int
	}{
		{"json snapshot", fmt.Sprintf(`{"app":"api","image":%q,"tag":"stable","roles":["web"]}`, image), 200, true, true, 0},
		{"text snapshot", fmt.Sprintf(`{"app":"api","image":%q,"tag":"stable","roles":["web"]}`, image), 200, true, false, 0},
		{"noninteractive", fmt.Sprintf(`{"app":"api","image":%q,"tag":"stable","roles":["web"]}`, image), 200, false, false, 1},
		{"legacy", `{"code":"invalid_request","message":"invalid request"}`, 400, true, false, 1},
		{"wrong app", fmt.Sprintf(`{"app":"other","image":%q,"tag":"stable","roles":["web"]}`, image), 200, true, false, 1},
		{"duplicate app", fmt.Sprintf(`{"app":"api","app":"other","image":%q,"tag":"stable","roles":["web"]}`, image), 200, true, false, 1},
		{"case shifted app", fmt.Sprintf(`{"App":"api","image":%q,"tag":"stable","roles":["web"]}`, image), 200, true, false, 1},
		{"unrelated plan", fmt.Sprintf(`{"app":"api","image":%q,"tag":"stable","roles":["web"],"remove":{}}`, image), 200, true, false, 1},
		{"blocked", fmt.Sprintf(`{"app":"api","image":%q,"tag":"stable","roles":["web"],"blockers":["no"]}`, image), 200, true, false, 1},
		{"compose", fmt.Sprintf(`{"app":"api","image":%q,"tag":"stable","roles":["web"],"compose":{}}`, image), 200, true, false, 1},
		{"wrong tag", fmt.Sprintf(`{"app":"api","image":%q,"tag":"other","roles":["web"]}`, image), 200, true, false, 1},
		{"wrong image", fmt.Sprintf(`{"app":"api","image":%q,"tag":"stable","roles":["web"]}`, "ghcr.io/acme/api@sha256:"+strings.Repeat("b", 64)), 200, true, false, 1},
		{"short write", fmt.Sprintf(`{"app":"api","image":%q,"tag":"stable","roles":["web"]}`, image), 200, true, false, 1},
		{"cancelled output", fmt.Sprintf(`{"app":"api","image":%q,"tag":"stable","roles":["web"]}`, image), 200, true, false, 1},
		{"redaction", `{"message":"source-secret dest-secret"}`, 403, true, false, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var sent []deploy.Request
			src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer source-secret" {
					t.Error("wrong source credential")
				}
				fmt.Fprintf(w, `{"id":12,"digest":%q,"tag":"stable"}`, image)
			}))
			defer src.Close()
			dst := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer dest-secret" {
					t.Error("wrong destination credential")
				}
				switch {
				case strings.HasSuffix(r.URL.Path, "/applied/api"):
					w.WriteHeader(404)
					fmt.Fprint(w, `{"code":"not_deployed","message":"not deployed"}`)
				case strings.HasSuffix(r.URL.Path, "/deploys"):
					var req deploy.Request
					if json.NewDecoder(r.Body).Decode(&req) != nil {
						t.Error("invalid request")
					}
					mu.Lock()
					sent = append(sent, req)
					mu.Unlock()
					w.WriteHeader(tc.status)
					fmt.Fprint(w, tc.reply)
				default:
					t.Errorf("unexpected path %s", r.URL.Path)
					w.WriteHeader(500)
				}
			}))
			defer dst.Close()
			t.Setenv("PROMOTE_SRC_TOKEN", "source-secret")
			t.Setenv("PROMOTE_DST_TOKEN", "dest-secret")
			dir := t.TempDir()
			config := filepath.Join(dir, "config.json")
			cfg := fmt.Sprintf(`{"projects":{"demo":{"environments":{"staging":{"url":%q,"token_env":"PROMOTE_SRC_TOKEN"},"prod":{"url":%q,"token_env":"PROMOTE_DST_TOKEN"}}}}}`, src.URL, dst.URL)
			if err := os.WriteFile(config, []byte(cfg), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "berthy.yml"), []byte("project: demo\nnetwork: demo\napps:\n  api:\n    kind: image\n    image: ghcr.io/acme/api\n    roles:\n      web: {}\n"), 0600); err != nil {
				t.Fatal(err)
			}
			target, err := agentclient.Resolve(config, agentclient.Selector{Project: "demo", Environment: "prod"})
			if err != nil {
				t.Fatal(err)
			}
			var out, errout bytes.Buffer
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var output io.Writer = &out
			if tc.name == "short write" {
				output = promotionTestWriter{write: func(p []byte) (int, error) { return len(p) / 2, nil }}
			}
			if tc.name == "cancelled output" {
				output = promotionTestWriter{write: func(p []byte) (int, error) { cancel(); return len(p), nil }}
			}
			code := runPromote(ctx, http.DefaultClient, target, config, "staging", commandFlags{dir: dir, env: "prod", pos: []string{"api"}, id: "snapshot-id", dry: tc.dry, json: tc.asJSON}, output, &errout)
			if code != tc.want {
				t.Fatalf("exit %d, stderr %s", code, errout.String())
			}
			mu.Lock()
			requests := append([]deploy.Request(nil), sent...)
			mu.Unlock()
			if tc.name == "noninteractive" {
				if len(requests) != 0 {
					t.Fatal("noninteractive promotion reached agent")
				}
				return
			}
			if len(requests) != 1 || !requests[0].DryRun || requests[0].ExpectedApplied == nil || *requests[0].ExpectedApplied != 0 || requests[0].Version != image || requests[0].VersionTag != "stable" || requests[0].RequestID != "snapshot-id" || requests[0].Manifest.Project != "demo" {
				t.Fatalf("wrong frozen preview: %+v", requests)
			}
			if tc.want == 0 && (!strings.Contains(out.String(), image) || tc.asJSON && !json.Valid(out.Bytes()) || !tc.asJSON && !strings.Contains(out.String(), "staging")) {
				t.Fatalf("output %q", out.String())
			}
			if tc.name == "redaction" && (strings.Contains(errout.String(), "source-secret") || strings.Contains(errout.String(), "dest-secret")) {
				t.Fatal("credentials appeared in diagnostic")
			}
			if tc.name == "legacy" && !strings.Contains(errout.String(), "upgrade") {
				t.Fatalf("missing upgrade hint: %s", errout.String())
			}
		})
	}
}

type promotionTestWriter struct{ write func([]byte) (int, error) }

func (w promotionTestWriter) Write(p []byte) (int, error) { return w.write(p) }

func TestPromoteDispatchRejectsInvalidBeforeRemote(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(500) }))
	defer server.Close()
	t.Setenv("PROMOTE_DST_TOKEN", "destination-token")
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	cfg := fmt.Sprintf(`{"projects":{"demo":{"environments":{"prod":{"url":%q,"token_env":"PROMOTE_DST_TOKEN"}}}}}`, server.URL)
	if err := os.WriteFile(path, []byte(cfg), 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"promote", "api"}, {"promote", "api", "--from"}, {"promote", "api", "--from", "prod"},
		{"promote", "api", "--from", "BAD!"}, {"promote", "api", "--from", "stage", "--from", "stage"},
		{"promote", "api", "extra", "--from", "stage"}, {"promote", "api", "--from", "stage", "--"},
		{"promote", "api", "--from", "stage", "--version", "v1"},
		{"promote", "api", "--from", "stage", "--json"}, {"promote", "api", "--from", "stage"},
	} {
		var out, errs bytes.Buffer
		argv := append(append([]string{}, args...), "-e", "prod", "--config", path)
		if code := Run(argv, &out, &errs); code == 0 || calls.Load() != 0 {
			t.Fatalf("invalid request reached agent or succeeded: %v, exit %d", args, code)
		}
	}
}

func TestPromotionPreviewRedactsDecodedTokens(t *testing.T) {
	image := "ghcr.io/acme/api@sha256:" + strings.Repeat("a", 64)
	plan := fmt.Sprintf(`{"app":"api","image":%q,"tag":"stable","roles":[],"notes":["source\u002dsecret", "dest\u002dsecret"]}`, image)
	var out bytes.Buffer
	err := showImagePlan(&out, []byte(plan), "dest-secret", true, "", "source-secret")
	if err != nil || strings.Contains(out.String(), "source-secret") || strings.Contains(out.String(), "dest-secret") || !strings.Contains(out.String(), "[redacted]") {
		t.Fatalf("unsafe preview or error: %v", err)
	}
}

func TestPromotionSubmissionRedactsDecodedResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		fmt.Fprint(w, `{"approval_id":"ap1","expires":"2030-01-01T00:00:00Z","plan":{"app":"api","image":"image","notes":["source\u002dsecret","dest\u002dsecret"]}}`)
	}))
	defer server.Close()
	u, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	var out, errs bytes.Buffer
	code := sendCommand(context.Background(), http.DefaultClient, u, "/", "dest-secret", "POST", "deploys", struct{}{}, commandFlags{redactToken: "source-secret"}, "deploy", &out, &errs)
	if code != 3 || strings.Contains(out.String(), "source-secret") || strings.Contains(out.String(), "dest-secret") || !strings.Contains(out.String(), "[redacted]") {
		t.Fatalf("unsafe hold output, exit %d", code)
	}
}

func TestPromoteRefusesCheckoutSwitchDuringCapture(t *testing.T) {
	manifestText := "project: demo\napps:\n  api:\n    kind: image\n    image: ghcr.io/acme/api\n    roles:\n      web: {}\n"
	dir, first := checkoutRepo(t, map[string]string{"berthy.yml": manifestText})
	if err := os.WriteFile(filepath.Join(dir, "berthy.yml"), []byte(manifestText+"# another revision\n"), 0600); err != nil {
		t.Fatal(err)
	}
	gitRunIn(t, dir, "add", "berthy.yml")
	gitRunIn(t, dir, "commit", "-q", "-m", "second")
	second := gitRunIn(t, dir, "rev-parse", "HEAD")
	gitRunIn(t, dir, "checkout", "-q", "--detach", first)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(500) }))
	defer server.Close()
	t.Setenv("PROMOTE_SWITCH_TOKEN", "test-token")
	path := filepath.Join(t.TempDir(), "config.json")
	cfg := fmt.Sprintf(`{"projects":{"demo":{"environments":{"stage":{"url":%q,"token_env":"PROMOTE_SWITCH_TOKEN"},"prod":{"url":%q,"token_env":"PROMOTE_SWITCH_TOKEN"}}}}}`, server.URL, server.URL)
	if err := os.WriteFile(path, []byte(cfg), 0600); err != nil {
		t.Fatal(err)
	}
	target, err := agentclient.Resolve(path, agentclient.Selector{Project: "demo", Environment: "prod"})
	if err != nil {
		t.Fatal(err)
	}
	old := loadDeployManifest
	loadDeployManifest = func(dir, env string) (*manifest.Loaded, error) {
		m, err := old(dir, env)
		if err == nil {
			gitRunIn(t, dir, "checkout", "-q", "--detach", second)
		}
		return m, err
	}
	defer func() { loadDeployManifest = old }()
	var out, errs bytes.Buffer
	code := runPromote(context.Background(), http.DefaultClient, target, path, "stage", commandFlags{dir: dir, env: "prod", pos: []string{"api"}, dry: true}, &out, &errs)
	if code == 0 || calls.Load() != 0 || !strings.Contains(errs.String(), "checkout changed") {
		t.Fatalf("checkout switch: exit %d calls %d stderr %q", code, calls.Load(), errs.String())
	}
}

func TestPromotionHeldPlanEscapesControls(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		fmt.Fprint(w, `{"approval_id":"ap1","expires":"2030-01-01T00:00:00Z","plan":{"app":"api","image":"image","notes":["hello\u001b[31m source\u002dsecret", "world\rdest\u002dsecret"],"key\u001b[0m":"value\u0007","diff":{"changes":[{"area":"env","change":"changed"}]}}}`)
	}))
	defer server.Close()
	u, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	var out, errs bytes.Buffer
	code := sendCommand(context.Background(), http.DefaultClient, u, "/", "dest-secret", "POST", "deploys", struct{}{}, commandFlags{redactToken: "source-secret"}, "deploy", &out, &errs)
	text := out.String() + errs.String()
	if code != 3 || strings.ContainsAny(text, "\x1b\r\x07") || strings.Contains(text, "source-secret") || strings.Contains(text, "dest-secret") || !strings.Contains(text, "ap1") || !strings.Contains(text, "env") || !strings.Contains(text, "changed") {
		t.Fatalf("unsafe or incomplete held display, exit %d, output %q", code, text)
	}
}

func TestPromotionResponseRejectsAmbiguousEnvelope(t *testing.T) {
	for _, body := range []string{
		`{"approval_id":"ap1","expires":"2030-01-01T00:00:00Z","Plan":{"notes":["\u001b[31m"]}}`,
		`{"approval_id":"ap1","expires":"2030-01-01T00:00:00Z","plan":{},"plan":{"notes":["\r"]}}`,
		`{"Approval_id":"ap1","expires":"2030-01-01T00:00:00Z","plan":{}}`,
		`{"approval_id":"ap1","Expires":"2030-01-01T00:00:00Z","plan":{}}`,
		`{"operation_id":"op1","Operation_ID":"op2","events_url":"/events"}`,
		`{"operation_id":"op1","Events_Url":"/events"}`,
	} {
		if _, err := safePromotionResponse([]byte(body), "source-secret", "dest-secret"); err == nil {
			t.Errorf("ambiguous envelope accepted: %s", body)
		}
	}
	safe, err := safePromotionResponse([]byte(`{"operation_id":"op1","events_url":"/events","metadata":"future"}`), "source-secret", "dest-secret")
	if err != nil || !bytes.Contains(safe, []byte(`"operation_id":"op1"`)) || !bytes.Contains(safe, []byte(`"metadata":"future"`)) {
		t.Fatalf("valid accepted response changed: %s %v", safe, err)
	}
}
