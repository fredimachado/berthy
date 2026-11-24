package cli

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"berthy/internal/agentclient"
	"berthy/internal/manifest"
)

func TestPromoteUsesAppliedSnapshotAndSeparateTokens(t *testing.T) {
	a, b := strings.Repeat("a", 64), strings.Repeat("b", 64)
	sourceBody := fmt.Sprintf(`{"id":12,"digest":"ghcr.io/acme/api:old@sha256:%s","tag":"stable"}`, a)
	destBody := fmt.Sprintf(`{"id":7,"digest":"ghcr.io/acme/api@sha256:%s","tag":"older"}`, b)
	sourceStatus, destStatus := 200, 200
	requests := 0
	server := func(env, token string, body *string, status *int) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests++
			if r.Method != "GET" || r.URL.Path != "/v1/projects/demo/environments/"+env+"/applied/api" || r.Header.Get("Authorization") != "Bearer "+token {
				t.Errorf("unexpected request to %s: %s %s %q", env, r.Method, r.URL.Path, r.Header.Get("Authorization"))
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
	if snap.sourceID != 12 || snap.expectedID != 7 || snap.image != "ghcr.io/acme/api@sha256:"+a || snap.tag != "stable" || requests != 2 {
		t.Fatalf("snapshot: %+v, requests %d", snap, requests)
	}
	sourceBody = fmt.Sprintf(`{"id":99,"digest":"ghcr.io/acme/api@sha256:%s"}`, b)
	destBody = `{"code":"not_deployed","message":"app is not deployed"}`
	destStatus = 404
	first, err := selectSnapshot()
	if err != nil || first.expectedID != 0 {
		t.Fatalf("initial destination: %+v %v", first, err)
	}
	if snap.sourceID != 12 || snap.expectedID != 7 || snap.image != "ghcr.io/acme/api@sha256:"+a {
		t.Fatalf("snapshot changed: %+v", snap)
	}
	for _, tc := range []struct {
		name, src, dst string
		status         int
	}{
		{"source absent", `{"code":"not_deployed"}`, destBody, 404},
		{"wrong shape", `[]`, destBody, 200},
		{"duplicate id", `{"id":1,"ID":2,"digest":"x"}`, destBody, 200},
		{"bare digest", fmt.Sprintf(`{"id":1,"digest":"sha256:%s"}`, a), destBody, 200},
		{"repository mismatch", fmt.Sprintf(`{"id":1,"digest":"ghcr.io/other/api@sha256:%s"}`, a), destBody, 200},
		{"bad tag", fmt.Sprintf(`{"id":1,"digest":"ghcr.io/acme/api@sha256:%s","tag":"bad tag"}`, a), destBody, 200},
		{"equal", fmt.Sprintf(`{"id":1,"digest":"ghcr.io/acme/api@sha256:%s"}`, a), fmt.Sprintf(`{"id":2,"digest":"ghcr.io/acme/api@sha256:%s"}`, a), 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sourceBody, sourceStatus, destBody, destStatus = tc.src, tc.status, tc.dst, 200
			if tc.dst == `{"code":"not_deployed","message":"app is not deployed"}` {
				destStatus = 404
			}
			if _, err := selectSnapshot(); err == nil {
				t.Fatal("expected refusal")
			}
		})
	}
}

func TestReadAppliedStrictResponses(t *testing.T) {
	body, status := "", http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status); fmt.Fprint(w, body) }))
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
			body, status = tc.body, tc.status
			_, found, err := readApplied(context.Background(), http.DefaultClient, target, "api")
			if (err == nil) != tc.valid || found != tc.found || err != nil && strings.Contains(err.Error(), "secret-token") {
				t.Fatalf("found=%v err=%v", found, err)
			}
		})
	}
}
