package registrycheck_test

import (
	"berthy/internal/registrycheck"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTagsReturnsCompleteBoundedList(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/acme/api/tags/list" || r.URL.RawQuery != "" {
			t.Errorf("unexpected listing request")
		}
		fmt.Fprint(w, `{"name":"acme/api","tags":["z","a","z"]}`)
	}))
	defer srv.Close()
	tags, err := probe(srv).Tags(context.Background(), "acme/api")
	if err != nil || fmt.Sprint(tags) != "[a z]" {
		t.Fatalf("tags=%v err=%v", tags, err)
	}
}

func TestTagsTrustsOnlyRegistryOrHubIssuer(t *testing.T) {
	for _, tc := range []struct {
		name, host, realm, service string
		allowed                    bool
	}{
		{"ghcr", "ghcr.io", "https://ghcr.io/token", "", true},
		{"hub", "docker.io", "https://auth.docker.io/token", "registry.docker.io", true},
		{"hub port", "docker.io:444", "https://auth.docker.io/token", "registry.docker.io", false},
		{"other issuer", "ghcr.io", "https://other.example/token", "", false},
		{"loopback port", "localhost:1234", "http://localhost:1235/token", "", false},
		{"userinfo", "ghcr.io", "https://name@ghcr.io/token", "", false},
		{"fragment", "ghcr.io", "https://ghcr.io/token#x", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			p := registrycheck.Probe{Host: tc.host, Username: user, Password: secret, HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				if calls == 1 {
					if r.Header.Get("Authorization") != "" {
						t.Error("initial request had authorization")
					}
					return response(r, 401, "", http.Header{"Www-Authenticate": []string{`Bearer realm="` + tc.realm + `",service="` + tc.service + `"`}}), nil
				}
				if !tc.allowed {
					t.Error("rejected issuer received a request")
				}
				if calls == 2 {
					if r.Header.Get("Authorization") != basicHeader || r.URL.Query().Get("scope") != "repository:acme/api:pull" {
						t.Error("wrong token request")
					}
					return response(r, 200, `{"token":"`+issued+`"}`, nil), nil
				}
				if r.Header.Get("Authorization") != "Bearer "+issued {
					t.Error("wrong listing authorization")
				}
				return response(r, 200, `{"name":"acme/api","tags":[]}`, nil), nil
			})}}
			tags, err := p.Tags(context.Background(), "acme/api")
			requireClean(t, err)
			if tc.allowed && (err != nil || calls != 3 || tags == nil) {
				t.Fatalf("allowed: calls=%d err=%v", calls, err)
			}
			if !tc.allowed && (err == nil || calls != 1 || tags != nil) {
				t.Fatalf("rejected: calls=%d err=%v", calls, err)
			}
		})
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func response(r *http.Request, status int, body string, h http.Header) *http.Response {
	return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(strings.NewReader(body)), Request: r}
}

func TestTagsPaginationAndFailures(t *testing.T) {
	tests := []struct {
		name          string
		first, second string
		link          string
		fail          bool
	}{
		{"short pages", `{"name":"acme/api","tags":["b"]}`, `{"name":"acme/api","tags":["a"]}`, `<?last=b>; rel="next"`, false},
		{"null", `{"name":"acme/api","tags":null}`, "", "", false},
		{"empty", `{"name":"acme/api","tags":[]}`, "", "", false},
		{"missing", `{"name":"acme/api"}`, "", "", true},
		{"wrong name", `{"name":"other","tags":[]}`, "", "", true},
		{"wrong type", `{"name":"acme/api","tags":{}}`, "", "", true},
		{"invalid tag", `{"name":"acme/api","tags":["bad tag"]}`, "", "", true},
		{"trailing", `{"name":"acme/api","tags":[]}x`, "", "", true},
		{"truncated", `{"name":"acme/api","tags":[`, "", "", true},
		{"failed final", `{"name":"acme/api","tags":["b"]}`, `bad`, `<?last=b>; rel="next"`, true},
		{"wrong path", `{"name":"acme/api","tags":["b"]}`, "", `</v2/other/tags/list>; rel="next"`, true},
		{"loop", `{"name":"acme/api","tags":["b"]}`, "", `</v2/acme/api/tags/list>; rel="next"`, true},
		{"repeated rel", `{"name":"acme/api","tags":["b"]}`, `{"name":"acme/api","tags":["a"]}`, `<?a>; rel="next"; rel="next"`, true},
		{"ambiguous", `{"name":"acme/api","tags":["b"]}`, "", `<?a>; rel="next", <?b>; rel="next"`, true},
		{"oversize", `{"name":"acme/api","tags":[]}` + strings.Repeat(" ", 1<<20), "", "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.URL.Path != "/v2/acme/api/tags/list" {
					t.Error("unexpected endpoint")
				}
				if calls == 1 {
					if tc.link != "" {
						w.Header().Set("Link", tc.link)
					}
					fmt.Fprint(w, tc.first)
				} else {
					fmt.Fprint(w, tc.second)
				}
			}))
			defer srv.Close()
			tags, err := probe(srv).Tags(context.Background(), "acme/api")
			if tc.fail && (err == nil || tags != nil) {
				t.Fatalf("expected failure with no partial tags, got %v %v", tags, err)
			}
			if !tc.fail && (err != nil || tags == nil) {
				t.Fatalf("expected complete result, got %v %v", tags, err)
			}
			if tc.name == "short pages" && fmt.Sprint(tags) != "[a b]" {
				t.Fatalf("tags %v", tags)
			}
		})
	}
}

func TestTagsRejectsRedirects(t *testing.T) {
	for _, stage := range []string{"initial", "token", "retry", "page"} {
		t.Run(stage, func(t *testing.T) {
			targetCalls := 0
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { targetCalls++ }))
			defer target.Close()
			var srv *httptest.Server
			srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if stage == "initial" || (stage == "token" && r.URL.Path == "/token") || (stage == "retry" && r.Header.Get("Authorization") != "") || (stage == "page" && r.URL.RawQuery != "") {
					http.Redirect(w, r, target.URL, http.StatusFound)
					return
				}
				if stage == "token" && r.URL.Path != "/token" {
					w.Header().Set("WWW-Authenticate", `Bearer realm="`+srv.URL+`/token"`)
					w.WriteHeader(401)
					return
				}
				if stage == "retry" {
					w.Header().Set("WWW-Authenticate", `Basic realm="x"`)
					w.WriteHeader(401)
					return
				}
				w.Header().Set("Link", `<?page=2>; rel="next"`)
				fmt.Fprint(w, `{"name":"acme/api","tags":["a"]}`)
			}))
			defer srv.Close()
			tags, err := probe(srv).Tags(context.Background(), "acme/api")
			if err == nil || tags != nil || targetCalls != 0 {
				t.Fatalf("redirect followed or succeeded: calls=%d err=%v", targetCalls, err)
			}
			requireClean(t, err)
		})
	}
}

func TestTagsUnpagedAndLimits(t *testing.T) {
	for _, tc := range []struct {
		name         string
		count, pages int
		wantErr      bool
	}{
		{"100 unpaged", 100, 1, false}, {"101 unpaged", 101, 1, false},
		{"tag limit", 5001, 1, true}, {"page limit", 1, 101, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if calls < tc.pages {
					w.Header().Set("Link", fmt.Sprintf(`<?page=%d>; rel="next"`, calls+1))
				}
				values := make([]string, tc.count)
				for i := range values {
					values[i] = fmt.Sprintf("tag-%04d-%04d", calls, i)
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"name": "acme/api", "tags": values})
			}))
			defer srv.Close()
			tags, err := probe(srv).Tags(context.Background(), "acme/api")
			if tc.wantErr {
				if err == nil || tags != nil {
					t.Fatalf("expected failure, got %d tags and %v", len(tags), err)
				}
				return
			}
			if err != nil || len(tags) != tc.count || calls != 1 {
				t.Fatalf("got %d tags, %d requests, %v", len(tags), calls, err)
			}
		})
	}
}

func TestTagsBasicAndAnonymousBearer(t *testing.T) {
	for _, scheme := range []string{"basic", "bearer"} {
		t.Run(scheme, func(t *testing.T) {
			calls := 0
			var srv *httptest.Server
			srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if calls == 1 {
					if r.Header.Get("Authorization") != "" {
						t.Error("initial request carried credentials")
					}
					if scheme == "basic" {
						w.Header().Set("WWW-Authenticate", `Basic realm="x"`)
					} else {
						w.Header().Set("WWW-Authenticate", `Bearer realm="`+srv.URL+`/token"`)
					}
					w.WriteHeader(401)
					return
				}
				if r.URL.Path == "/token" {
					if r.Header.Get("Authorization") != "" {
						t.Error("anonymous exchange carried credentials")
					}
					fmt.Fprint(w, `{"token":"`+issued+`"}`)
					return
				}
				want := basicHeader
				if scheme == "bearer" {
					want = "Bearer " + issued
				}
				if r.Header.Get("Authorization") != want {
					t.Error("incorrect listing authorization")
				}
				fmt.Fprint(w, `{"name":"acme/api","tags":[]}`)
			}))
			defer srv.Close()
			p := probe(srv)
			if scheme == "bearer" {
				p.Username = ""
				p.Password = ""
			}
			tags, err := p.Tags(context.Background(), "acme/api")
			if err != nil || tags == nil {
				t.Fatalf("listing failed: %v", err)
			}
		})
	}
}
