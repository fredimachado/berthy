package probehelper

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

const testToken = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// serve sends one request to the helper's handler for token and returns the status and body.
func serve(t *testing.T, token, method, path, auth, body string) (int, string) {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if auth != "" {
		r.Header.Set("Authorization", auth)
	}
	w := httptest.NewRecorder()
	Handler(token).ServeHTTP(w, r)
	return w.Code, w.Body.String()
}

// probeBody is a valid HTTP probe body of the target.
func probeBody(target string) string {
	return `{"kind":"http","target":"` + target + `","path":"/health","timeout_ms":5000,"expect":[200]}`
}

// hostPort returns the host:port of a test server.
func hostPort(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return u.Host
}

func TestHandlerNeedsTheBearerToken(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer ok.Close()
	body := probeBody(hostPort(t, ok))
	for name, auth := range map[string]string{
		"no header":    "",
		"other scheme": "Basic " + testToken,
		"wrong token":  "Bearer " + strings.Repeat("0", 64),
		"bare token":   testToken,
		"empty bearer": "Bearer ",
	} {
		if code, _ := serve(t, testToken, http.MethodPost, "/probe", auth, body); code != http.StatusUnauthorized {
			t.Errorf("%s: %d, want 401", name, code)
		}
	}
	if code, out := serve(t, testToken, http.MethodPost, "/probe", "Bearer "+testToken, body); code != http.StatusOK || !strings.Contains(out, `"ok":true`) {
		t.Fatalf("right token: %d %s", code, out)
	}
	// Without a token configured nothing is accepted, not even an empty bearer.
	if code, _ := serve(t, "", http.MethodPost, "/probe", "Bearer ", body); code != http.StatusUnauthorized {
		t.Fatalf("empty configured token: %d, want 401", code)
	}
	// Unauthenticated requests get 401 whatever the route, so the helper says nothing without the token.
	if code, _ := serve(t, testToken, http.MethodGet, "/elsewhere", "", ""); code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated GET elsewhere: %d, want 401", code)
	}
}

func TestHandlerRoutesOnlyPostProbe(t *testing.T) {
	auth := "Bearer " + testToken
	if code, _ := serve(t, testToken, http.MethodGet, "/probe", auth, ""); code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /probe: %d", code)
	}
	if code, _ := serve(t, testToken, http.MethodPost, "/other", auth, probeBody("web:80")); code != http.StatusNotFound {
		t.Fatalf("POST /other: %d", code)
	}
}

func TestHandlerRefusesMalformedRequests(t *testing.T) {
	auth := "Bearer " + testToken
	for name, body := range map[string]string{
		"not JSON":         `{`,
		"unknown field":    `{"kind":"tcp","target":"web:80","timeout_ms":5000,"extra":1}`,
		"trailing data":    `{"kind":"tcp","target":"web:80","timeout_ms":5000} x`,
		"second object":    `{"kind":"tcp","target":"web:80","timeout_ms":5000}{}`,
		"wrong type":       `{"kind":"tcp","target":"web:80","timeout_ms":"5000"}`,
		"empty body":       ``,
		"array not object": `[]`,
	} {
		if code, out := serve(t, testToken, http.MethodPost, "/probe", auth, body); code != http.StatusBadRequest {
			t.Errorf("%s: %d %s, want 400", name, code, out)
		}
	}
	big := `{"kind":"tcp","target":"web:80","timeout_ms":5000,"path":"` + strings.Repeat("a", 5000) + `"}`
	if code, _ := serve(t, testToken, http.MethodPost, "/probe", auth, big); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("body over 4 KiB: %d, want 413", code)
	}
}

func TestValidateTargets(t *testing.T) {
	valid := []string{"web:8080", "api-1.internal:1", "a_b:65535", "127.0.0.1:80", "x:0080", "a:443"}
	invalid := []string{
		"web",           // no port
		"web:0",         // port zero
		"web:65536",     // port too large
		"web:http",      // port not a number
		"web:80/x",      // path smuggled into the target
		"http://web:80", // scheme
		"user@web:80",   // user information
		"web:80:81",     // two ports
		"-web:80",       // name starts with a dash
		"we b:80",       // space
		"web:",          // empty port
		":80",           // empty name
		strings.Repeat("a", 129) + ":80",
	}
	for _, target := range valid {
		if !validTarget(target) {
			t.Errorf("valid target %q refused", target)
		}
	}
	for _, target := range invalid {
		if validTarget(target) {
			t.Errorf("invalid target %q accepted", target)
		}
	}
}

func TestValidateRequests(t *testing.T) {
	// Valid requests really run, so their target is a local server rather than a name that needs DNS.
	local := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer local.Close()
	good := func(mut func(*requestFields)) string {
		f := requestFields{Kind: "http", Target: hostPort(t, local), TimeoutMS: 5000}
		mut(&f)
		b, err := json.Marshal(f)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	cases := map[string]struct {
		body  string
		valid bool
	}{
		"http minimal":          {good(func(f *requestFields) {}), true},
		"http path and expect":  {good(func(f *requestFields) { f.Path = "/a?b=c&d=%20"; f.Expect = []int{200, 503} }), true},
		"http path without /":   {good(func(f *requestFields) { f.Path = "health" }), false},
		"http path with space":  {good(func(f *requestFields) { f.Path = "/a b" }), false},
		"http path fragment":    {good(func(f *requestFields) { f.Path = "/a#frag" }), false},
		"http path control":     {good(func(f *requestFields) { f.Path = "/a\x01" }), false},
		"http expect too low":   {good(func(f *requestFields) { f.Expect = []int{99} }), false},
		"http expect too high":  {good(func(f *requestFields) { f.Expect = []int{600} }), false},
		"http expect too many":  {good(func(f *requestFields) { f.Expect = make([]int, maxExpect+1) }), false},
		"tcp minimal":           {good(func(f *requestFields) { f.Kind = "tcp" }), true},
		"tcp with path":         {good(func(f *requestFields) { f.Kind = "tcp"; f.Path = "/" }), false},
		"tcp with expect":       {good(func(f *requestFields) { f.Kind = "tcp"; f.Expect = []int{200} }), false},
		"unknown kind":          {good(func(f *requestFields) { f.Kind = "udp" }), false},
		"timeout below 1s":      {good(func(f *requestFields) { f.TimeoutMS = 999 }), false},
		"timeout above 30s":     {good(func(f *requestFields) { f.TimeoutMS = 30001 }), false},
		"timeout missing":       {good(func(f *requestFields) { f.TimeoutMS = 0 }), false},
		"timeout at the bounds": {good(func(f *requestFields) { f.TimeoutMS = 30000 }), true},
		"bad target":            {good(func(f *requestFields) { f.Target = "http://web:80" }), false},
	}
	for name, tc := range cases {
		code, out := serve(t, testToken, http.MethodPost, "/probe", "Bearer "+testToken, tc.body)
		accepted := code == http.StatusOK
		if accepted != tc.valid {
			t.Errorf("%s: status %d %s, want valid=%v", name, code, out, tc.valid)
		}
	}
}

// requestFields is a probe request as the tests build it. The helper decodes the same JSON shape.
type requestFields struct {
	Kind      string `json:"kind"`
	Target    string `json:"target"`
	Path      string `json:"path,omitempty"`
	TimeoutMS int    `json:"timeout_ms"`
	Expect    []int  `json:"expect,omitempty"`
}

func TestHTTPProbeAnswersWhatTheServiceSays(t *testing.T) {
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/redirect":
			http.Redirect(w, r, "/followed", http.StatusFound)
		case "/followed":
			w.WriteHeader(http.StatusInternalServerError)
		default:
			w.WriteHeader(status)
		}
	}))
	defer srv.Close()
	target := hostPort(t, srv)
	// run sends one probe body and returns the helper's verdict and detail.
	run := func(body string) (bool, string) {
		t.Helper()
		code, out := serve(t, testToken, http.MethodPost, "/probe", "Bearer "+testToken, body)
		if code != http.StatusOK {
			t.Fatalf("probe refused: %d %s", code, out)
		}
		var got struct {
			OK     bool   `json:"ok"`
			Detail string `json:"detail"`
		}
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			t.Fatal(err)
		}
		return got.OK, got.Detail
	}
	if ok, detail := run(probeBody(target)); !ok || detail != "HTTP 200" {
		t.Fatalf("200: %v %q", ok, detail)
	}
	status = http.StatusServiceUnavailable
	if ok, detail := run(probeBody(target)); ok || detail != "HTTP 503" {
		t.Fatalf("503: %v %q", ok, detail)
	}
	if ok, detail := run(`{"kind":"http","target":"` + target + `","path":"/health","timeout_ms":5000,"expect":[503]}`); !ok || detail != "HTTP 503" {
		t.Fatalf("expected 503: %v %q", ok, detail)
	}
	// A redirect is the status the probe sees: the helper does not follow it to the server's 500.
	if ok, detail := run(`{"kind":"http","target":"` + target + `","path":"/redirect","timeout_ms":5000}`); !ok || detail != "HTTP 302" {
		t.Fatalf("redirect: %v %q", ok, detail)
	}
	// Without a path the probe asks for "/", which answers 503 here.
	if ok, detail := run(`{"kind":"http","target":"` + target + `","timeout_ms":5000}`); ok || detail != "HTTP 503" {
		t.Fatalf("root path: %v %q", ok, detail)
	}
}

func TestTCPProbeAnswersWhetherThePortAccepts(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	closed := l.Addr().String()
	l.Close()
	open := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer open.Close()
	for target, want := range map[string]string{hostPort(t, open): "connected", closed: "connection refused"} {
		code, out := serve(t, testToken, http.MethodPost, "/probe", "Bearer "+testToken, `{"kind":"tcp","target":"`+target+`","timeout_ms":2000}`)
		if code != http.StatusOK || !strings.Contains(out, `"detail":"`+want+`"`) {
			t.Fatalf("tcp %s: %d %s, want %q", target, code, out, want)
		}
	}
}

func TestProbeEndsAtItsTimeout(t *testing.T) {
	release := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer slow.Close()
	defer close(release)
	start := time.Now()
	code, out := serve(t, testToken, http.MethodPost, "/probe", "Bearer "+testToken, `{"kind":"http","target":"`+hostPort(t, slow)+`","timeout_ms":1000}`)
	if code != http.StatusOK || !strings.Contains(out, `"ok":false`) || !strings.Contains(out, `"detail":"timeout"`) {
		t.Fatalf("slow probe: %d %s", code, out)
	}
	if elapsed := time.Since(start); elapsed > 4*time.Second {
		t.Fatalf("probe ran %s past a 1s timeout", elapsed)
	}
}

func TestServeRefusesAnInvalidToken(t *testing.T) {
	for _, token := range []string{"", "short", strings.Repeat("G", 64)} {
		err := Serve(context.Background(), "127.0.0.1:0", token)
		if err == nil || (token != "" && strings.Contains(err.Error(), token)) {
			t.Fatalf("token %q: %v", token, err)
		}
	}
}

func TestServeAnswersUntilItsContextEnds(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, addr, testToken) }()
	var resp *http.Response
	deadline := time.Now().Add(5 * time.Second)
	for {
		req, _ := http.NewRequest(http.MethodPost, "http://"+addr+"/probe", strings.NewReader(`{"kind":"tcp","target":"127.0.0.1:1","timeout_ms":1000}`))
		req.Header.Set("Authorization", "Bearer "+testToken)
		resp, err = http.DefaultClient.Do(req)
		if err == nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("helper did not answer: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve after shutdown: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not return after its context ended")
	}
}
