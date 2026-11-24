package registrycheck_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"berthy/internal/registrycheck"
)

const testDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func TestResolveTagContract(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != "HEAD" || r.URL.Path != "/v2/acme/api/manifests/v1" {
			t.Error("incorrect request")
		}
		for _, media := range []string{"application/vnd.oci.image.manifest.v1+json", "application/vnd.oci.image.index.v1+json", "application/vnd.docker.distribution.manifest.v2+json", "application/vnd.docker.distribution.manifest.list.v2+json"} {
			if !strings.Contains(r.Header.Get("Accept"), media) {
				t.Error("missing accepted media type")
			}
		}
		w.Header().Set("Docker-Content-Digest", testDigest)
	}))
	defer srv.Close()
	digest, err := probe(srv).ResolveTag(context.Background(), "acme/api", "v1")
	if err != nil || digest != testDigest || calls != 1 {
		t.Fatalf("unexpected result: %q %v calls=%d", digest, err, calls)
	}
}

func TestResolveTagAuthentication(t *testing.T) {
	for _, mode := range []string{"basic", "bearer", "hub"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			p := registrycheck.Probe{Host: "ghcr.io", Username: user, Password: secret}
			if mode == "hub" {
				p.Host = "docker.io"
			}
			p.HTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				if calls == 1 {
					if r.Method != "HEAD" || r.Header.Get("Authorization") != "" {
						t.Error("wrong initial request")
					}
					challenge := `Basic realm="fixture"`
					if mode == "bearer" {
						challenge = `Bearer realm="https://ghcr.io/token"`
					}
					if mode == "hub" {
						challenge = `Bearer realm="https://auth.docker.io/token",service="registry.docker.io"`
					}
					return response(r, 401, "", http.Header{"Www-Authenticate": []string{challenge}}), nil
				}
				if r.URL.Path == "/token" {
					if r.Method != "GET" || r.Header.Get("Authorization") != basicHeader || r.URL.Query().Get("scope") != "repository:acme/api:pull" {
						t.Error("wrong token exchange")
					}
					return response(r, 200, `{"token":"`+issued+`"}`, nil), nil
				}
				want := basicHeader
				if mode != "basic" {
					want = "Bearer " + issued
				}
				if r.Method != "HEAD" || r.Header.Get("Authorization") != want {
					t.Error("wrong retry")
				}
				return response(r, 200, "", http.Header{"Docker-Content-Digest": []string{testDigest}}), nil
			})}
			got, err := p.ResolveTag(context.Background(), "acme/api", "v1")
			expectedCalls := 3
			if mode == "basic" {
				expectedCalls = 2
			}
			if err != nil || got != testDigest || calls != expectedCalls {
				t.Fatalf("resolution failed: %v calls=%d", err, calls)
			}
		})
	}
}

func TestResolveTagRejectsInvalidResponses(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		values []string
	}{
		{"missing", 200, nil}, {"duplicate", 200, []string{testDigest, testDigest}}, {"comma", 200, []string{testDigest + "," + testDigest}}, {"wrong algorithm", 200, []string{"sha512:" + strings.Repeat("a", 128)}}, {"uppercase", 200, []string{"sha256:" + strings.Repeat("A", 64)}}, {"short", 200, []string{"sha256:abc"}}, {"denied", 403, []string{testDigest}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := registrycheck.Probe{Host: "ghcr.io", HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				return response(r, tc.status, "", http.Header{"Docker-Content-Digest": tc.values}), nil
			})}}
			got, err := p.ResolveTag(context.Background(), "acme/api", "v1")
			if err == nil || got != "" {
				t.Fatalf("expected failure and empty digest")
			}
		})
	}
}

func TestResolveTagRejectsUntrustedDestinations(t *testing.T) {
	for _, challenge := range []string{`Bearer realm="https://elsewhere.example/token"`, `Bearer realm="https://ghcr.io:444/token"`, `Bearer realm="https://auth.docker.io/token?extra=x",service="registry.docker.io"`} {
		t.Run(challenge, func(t *testing.T) {
			calls := 0
			p := registrycheck.Probe{Host: "ghcr.io", Username: user, Password: secret, HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				if calls > 1 {
					t.Error("unexpected outbound request")
				}
				return response(r, 401, "", http.Header{"Www-Authenticate": []string{challenge}}), nil
			})}}
			got, err := p.ResolveTag(context.Background(), "acme/api", "v1")
			if err == nil || got != "" || calls != 1 {
				t.Fatal("untrusted issuer accepted")
			}
			requireClean(t, err)
		})
	}
	targetCalls := 0
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { targetCalls++ }))
	defer target.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 302) }))
	defer srv.Close()
	got, err := probe(srv).ResolveTag(context.Background(), "acme/api", "v1")
	if err == nil || got != "" || targetCalls != 0 {
		t.Fatal("redirect accepted")
	}
}

func TestResolveTagRejectsInvalidInputAndCancellation(t *testing.T) {
	calls := 0
	p := registrycheck.Probe{Host: "ghcr.io", HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) { calls++; return nil, errors.New("unexpected") })}}
	for _, pair := range [][2]string{{"../repo", "v1"}, {"acme/api", "a/b"}, {"acme/api", ""}} {
		got, err := p.ResolveTag(context.Background(), pair[0], pair[1])
		if err == nil || got != "" {
			t.Fatal("invalid input accepted")
		}
	}
	if calls != 0 {
		t.Fatal("invalid input contacted registry")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got, err := p.ResolveTag(ctx, "acme/api", "v1")
	if err == nil || got != "" {
		t.Fatal("cancellation accepted")
	}
	_ = fmt.Sprint(err)
}

func TestResolveTagRedirectsAndTokenLimits(t *testing.T) {
	for _, stage := range []string{"initial", "token", "retry", "oversized token"} {
		t.Run(stage, func(t *testing.T) {
			targetCalls := 0
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { targetCalls++ }))
			defer target.Close()
			var srv *httptest.Server
			srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if stage == "initial" || (stage == "token" && r.URL.Path == "/token") || (stage == "retry" && r.Header.Get("Authorization") != "") {
					http.Redirect(w, r, target.URL, 302)
					return
				}
				if r.URL.Path == "/token" {
					fmt.Fprint(w, strings.Repeat("x", (1<<20)+1))
					return
				}
				if stage == "retry" {
					w.Header().Set("WWW-Authenticate", `Basic realm="x"`)
				} else {
					w.Header().Set("WWW-Authenticate", `Bearer realm="`+srv.URL+`/token"`)
				}
				w.WriteHeader(401)
			}))
			defer srv.Close()
			got, err := probe(srv).ResolveTag(context.Background(), "acme/api", "v1")
			if err == nil || got != "" || targetCalls != 0 {
				t.Fatal("unexpected response or redirect")
			}
			requireClean(t, err)
		})
	}
}
