package registrycheck_test

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"berthy/internal/registrycheck"
)

// Test values, not credentials.
const (
	user   = "berthy-check"
	secret = "not-a-real-password"
	issued = "issued-token-not-real"
)

var basicHeader = "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+secret))

// seen is one request a test server received.
type seen struct {
	Path, Query, Auth string
}

// recorder keeps every request a test server received.
type recorder struct {
	mu   sync.Mutex
	reqs []seen
}

func (r *recorder) add(req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reqs = append(r.reqs, seen{req.URL.Path, req.URL.RawQuery, req.Header.Get("Authorization")})
}

func (r *recorder) all() []seen {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]seen(nil), r.reqs...)
}

func probe(srv *httptest.Server) registrycheck.Probe {
	return registrycheck.Probe{Host: strings.TrimPrefix(srv.URL, "http://"), Username: user, Password: secret}
}

// basicRegistry answers the tag list with a Basic challenge and accepts only the right credential.
func basicRegistry(t *testing.T, rec *recorder) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.add(r)
		if r.Header.Get("Authorization") != basicHeader {
			w.Header().Set("WWW-Authenticate", `Basic realm="berthy-test"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// bearerRegistry challenges with a Bearer realm on the same server. tokenStatus is the token endpoint's status (200 issues
// tokenBody), repoStatus the status of the tag list when the issued token is presented.
func bearerRegistry(t *testing.T, rec *recorder, tokenStatus int, tokenBody string, repoStatus int) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.add(r)
		switch {
		case r.URL.Path == "/token":
			if tokenStatus != http.StatusOK {
				w.WriteHeader(tokenStatus)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(tokenBody))
		case r.Header.Get("Authorization") == "Bearer "+issued:
			w.WriteHeader(repoStatus)
		default:
			w.Header().Set("WWW-Authenticate", `Bearer realm="`+srv.URL+`/token",service="registry.test",scope="repository:acme/api:pull,push"`)
			w.WriteHeader(http.StatusUnauthorized)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func requireNotDenied(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("want an error, got nil")
	}
	if errors.Is(err, registrycheck.ErrDenied) {
		t.Fatalf("want a non-denial error, got %v", err)
	}
}

// requireClean checks that the error text holds none of the secrets.
func requireClean(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		return
	}
	for _, s := range []string{secret, issued, base64.StdEncoding.EncodeToString([]byte(user + ":" + secret))} {
		if strings.Contains(err.Error(), s) {
			t.Fatalf("error text contains a secret: %q", err.Error())
		}
	}
}

func TestBasicRegistryAcceptsRightCredential(t *testing.T) {
	rec := &recorder{}
	srv := basicRegistry(t, rec)
	if err := probe(srv).CanPull(context.Background(), "acme/api"); err != nil {
		t.Fatal(err)
	}
	reqs := rec.all()
	if len(reqs) != 2 {
		t.Fatalf("want 2 requests, got %+v", reqs)
	}
	if reqs[0].Auth != "" {
		t.Fatalf("challenge carried credentials: %+v", reqs[0])
	}
	last := reqs[1]
	if last.Path != "/v2/acme/api/tags/list" || last.Query != "n=1" || last.Auth != basicHeader {
		t.Fatalf("repository request %+v", last)
	}
}

func TestBasicRegistryDeniesWrongPassword(t *testing.T) {
	srv := basicRegistry(t, &recorder{})
	p := probe(srv)
	p.Password = "wrong-" + secret
	err := p.CanPull(context.Background(), "acme/api")
	if !errors.Is(err, registrycheck.ErrDenied) {
		t.Fatalf("want ErrDenied, got %v", err)
	}
	if strings.Contains(err.Error(), p.Password) {
		t.Fatalf("error text contains the password: %q", err)
	}
}

func TestBearerRegistryAcceptsRightCredential(t *testing.T) {
	rec := &recorder{}
	srv := bearerRegistry(t, rec, 200, `{"token":"`+issued+`"}`, 200)
	if err := probe(srv).CanPull(context.Background(), "acme/api"); err != nil {
		t.Fatal(err)
	}
	reqs := rec.all()
	if len(reqs) != 3 {
		t.Fatalf("want 3 requests, got %+v", reqs)
	}
	if reqs[0].Auth != "" {
		t.Fatalf("challenge carried credentials: %+v", reqs[0])
	}
	tok := reqs[1]
	if tok.Path != "/token" || tok.Auth != basicHeader {
		t.Fatalf("token request %+v", tok)
	}
	q := "&" + tok.Query + "&"
	if !strings.Contains(q, "&service=registry.test&") || !strings.Contains(q, "&scope=repository%3Aacme%2Fapi%3Apull&") {
		t.Fatalf("token query %q lacks service or the pull scope", tok.Query)
	}
	last := reqs[2]
	if last.Path != "/v2/acme/api/tags/list" || last.Query != "n=1" || last.Auth != "Bearer "+issued {
		t.Fatalf("repository request %+v", last)
	}
}

func TestBearerTokenEndpointDenies(t *testing.T) {
	for _, status := range []int{401, 403} {
		srv := bearerRegistry(t, &recorder{}, status, "", 200)
		err := probe(srv).CanPull(context.Background(), "acme/api")
		if !errors.Is(err, registrycheck.ErrDenied) {
			t.Fatalf("token status %d: want ErrDenied, got %v", status, err)
		}
		requireClean(t, err)
	}
}

func TestBearerRepositoryDenies(t *testing.T) {
	for _, status := range []int{401, 403, 404} {
		srv := bearerRegistry(t, &recorder{}, 200, `{"token":"`+issued+`"}`, status)
		err := probe(srv).CanPull(context.Background(), "acme/api")
		if !errors.Is(err, registrycheck.ErrDenied) {
			t.Fatalf("repository status %d: want ErrDenied, got %v", status, err)
		}
		requireClean(t, err)
	}
}

func TestBearerAcceptsAccessTokenField(t *testing.T) {
	srv := bearerRegistry(t, &recorder{}, 200, `{"access_token":"`+issued+`"}`, 200)
	if err := probe(srv).CanPull(context.Background(), "acme/api"); err != nil {
		t.Fatal(err)
	}
}

func TestBearerTokenResponseProblemsAreNotDenials(t *testing.T) {
	cases := map[string]struct {
		status int
		body   string
	}{
		"no token field": {200, `{"expires_in":300}`},
		"not JSON":       {200, `<html>`},
		"server error":   {500, ""},
	}
	for name, c := range cases {
		srv := bearerRegistry(t, &recorder{}, c.status, c.body, 200)
		err := probe(srv).CanPull(context.Background(), "acme/api")
		if err == nil || errors.Is(err, registrycheck.ErrDenied) {
			t.Fatalf("%s: want a non-denial error, got %v", name, err)
		}
		requireClean(t, err)
	}
}

func TestRegistryWithoutChallengeNeedsNoCredential(t *testing.T) {
	rec := &recorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.add(r)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	if err := probe(srv).CanPull(context.Background(), "acme/api"); err != nil {
		t.Fatal(err)
	}
	reqs := rec.all()
	if len(reqs) != 1 || reqs[0].Auth != "" {
		t.Fatalf("want one unauthenticated request, got %+v", reqs)
	}
}

func TestUnusableAnswersAreNotDenials(t *testing.T) {
	cases := map[string]func(w http.ResponseWriter){
		"server error":          func(w http.ResponseWriter) { w.WriteHeader(500) },
		"401 without challenge": func(w http.ResponseWriter) { w.WriteHeader(401) },
		"unknown scheme": func(w http.ResponseWriter) {
			w.Header().Set("WWW-Authenticate", `Negotiate abc`)
			w.WriteHeader(401)
		},
		"bearer without realm": func(w http.ResponseWriter) {
			w.Header().Set("WWW-Authenticate", `Bearer service="s"`)
			w.WriteHeader(401)
		},
		"unexpected status": func(w http.ResponseWriter) { w.WriteHeader(418) },
	}
	for name, answer := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { answer(w) }))
		err := probe(srv).CanPull(context.Background(), "acme/api")
		srv.Close()
		if err == nil || errors.Is(err, registrycheck.ErrDenied) {
			t.Fatalf("%s: want a non-denial error, got %v", name, err)
		}
		requireClean(t, err)
	}
}

func TestBearerChallengeSchemeIsCaseInsensitive(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/token":
			_, _ = w.Write([]byte(`{"token":"` + issued + `"}`))
		case r.Header.Get("Authorization") == "Bearer "+issued:
		default:
			w.Header().Set("WWW-Authenticate", `bEaReR realm="`+srv.URL+`/token"`)
			w.WriteHeader(401)
		}
	}))
	t.Cleanup(srv.Close)
	if err := probe(srv).CanPull(context.Background(), "acme/api"); err != nil {
		t.Fatal(err)
	}
}

func TestPlainHTTPRealmIsRefusedWithoutSendingTheCredential(t *testing.T) {
	rec := &recorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.add(r)
		w.Header().Set("WWW-Authenticate", `Bearer realm="http://example.com/token",service="s"`)
		w.WriteHeader(401)
	}))
	t.Cleanup(srv.Close)
	err := probe(srv).CanPull(context.Background(), "acme/api")
	requireNotDenied(t, err)
	requireClean(t, err)
	reqs := rec.all()
	if len(reqs) != 1 || reqs[0].Auth != "" {
		t.Fatalf("want only the unauthenticated challenge, got %+v", reqs)
	}
}

func TestRealmThatIsNotAnAbsoluteURLIsRefused(t *testing.T) {
	for _, realm := range []string{"/token", "token", "ftp://localhost/token", "https:///token"} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="`+realm+`"`)
			w.WriteHeader(401)
		}))
		err := probe(srv).CanPull(context.Background(), "acme/api")
		srv.Close()
		if err == nil || errors.Is(err, registrycheck.ErrDenied) {
			t.Fatalf("realm %q: want a non-denial error, got %v", realm, err)
		}
	}
}

func TestRealmKeepsItsOwnQuery(t *testing.T) {
	rec := &recorder{}
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.add(r)
		switch {
		case r.URL.Path == "/token":
			_, _ = w.Write([]byte(`{"token":"` + issued + `"}`))
		case r.Header.Get("Authorization") == "Bearer "+issued:
		default:
			w.Header().Set("WWW-Authenticate", `Bearer realm="`+srv.URL+`/token?account=`+user+`"`)
			w.WriteHeader(401)
		}
	}))
	t.Cleanup(srv.Close)
	if err := probe(srv).CanPull(context.Background(), "acme/api"); err != nil {
		t.Fatal(err)
	}
	q := rec.all()[1].Query
	if !strings.Contains(q, "account="+user) || !strings.Contains(q, "scope=repository%3Aacme%2Fapi%3Apull") || strings.Contains(q, "service=") {
		t.Fatalf("token query %q", q)
	}
}

// stubTransport records the request URL and answers 200.
type stubTransport struct{ url string }

func (s *stubTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	s.url = r.URL.String()
	return &http.Response{StatusCode: 200, Body: http.NoBody, Header: http.Header{}, Request: r}, nil
}

func TestBaseURLScheme(t *testing.T) {
	cases := []struct{ host, want string }{
		{"ghcr.io", "https://ghcr.io/v2/acme/api/tags/list?n=1"},
		{"ghcr.io:8443", "https://ghcr.io:8443/v2/acme/api/tags/list?n=1"},
		{"localhost:5000", "http://localhost:5000/v2/acme/api/tags/list?n=1"},
		{"localhost", "http://localhost/v2/acme/api/tags/list?n=1"},
		{"127.0.0.1:5000", "http://127.0.0.1:5000/v2/acme/api/tags/list?n=1"},
		{"[::1]:5000", "http://[::1]:5000/v2/acme/api/tags/list?n=1"},
		{"192.168.1.5:5000", "https://192.168.1.5:5000/v2/acme/api/tags/list?n=1"},
	}
	for _, c := range cases {
		st := &stubTransport{}
		p := registrycheck.Probe{Host: c.host, Username: user, Password: secret, HTTP: &http.Client{Transport: st}}
		if err := p.CanPull(context.Background(), "acme/api"); err != nil {
			t.Fatalf("%s: %v", c.host, err)
		}
		if st.url != c.want {
			t.Fatalf("%s: asked %q, want %q", c.host, st.url, c.want)
		}
	}
}

func TestCancelledContextIsNotADenial(t *testing.T) {
	srv := basicRegistry(t, &recorder{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := probe(srv).CanPull(ctx, "acme/api")
	requireNotDenied(t, err)
	requireClean(t, err)
}

func TestConnectionFailureIsNotADenial(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	p := probe(srv)
	srv.Close()
	err := p.CanPull(context.Background(), "acme/api")
	requireNotDenied(t, err)
	requireClean(t, err)
}
