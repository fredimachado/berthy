package agentclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"berthy/internal/session"
)

func sessionTarget(t *testing.T, srv *httptest.Server) *Target {
	t.Helper()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return &Target{Env: "lab", Base: u, Prefix: "/v1/projects/demo/environments/lab/", Token: "tok"}
}

func TestOpenSessionSwitchesProtocolsAndKeepsWhatFollowsTheAnswer(t *testing.T) {
	var got session.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/v1/projects/demo/environments/lab/sessions" || r.Header.Get("Authorization") != "Bearer tok" ||
			r.Header.Get("Upgrade") != session.Upgrade || !strings.EqualFold(r.Header.Get("Connection"), "Upgrade") {
			http.Error(w, "bad request", 400)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			http.Error(w, "bad body", 400)
			return
		}
		conn, brw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		_ = brw
		// The first frame follows the answer in the same write, as a fast agent might send it.
		conn.Write([]byte("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: " + session.Upgrade + "\r\n\r\n"))
		session.WriteFrame(conn, session.Stdout, []byte("hello"))
		session.WriteFrame(conn, session.Exit, []byte(`{"code":3}`))
	}))
	defer srv.Close()
	req := session.Request{Kind: "exec", App: "api", Command: []string{"sh"}, TTY: true, Size: &session.Size{Rows: 24, Cols: 80}}
	conn, br, err := OpenSession(context.Background(), sessionTarget(t, srv), req)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if got.Kind != "exec" || got.App != "api" || !got.TTY || got.Size == nil || got.Size.Cols != 80 {
		t.Fatalf("request %+v", got)
	}
	if k, p, err := session.ReadFrame(br); err != nil || k != session.Stdout || string(p) != "hello" {
		t.Fatalf("frame %d %q %v", k, p, err)
	}
	if k, p, err := session.ReadFrame(br); err != nil || k != session.Exit || string(p) != `{"code":3}` {
		t.Fatalf("frame %d %q %v", k, p, err)
	}
}

func TestOpenSessionReturnsTheAgentsRefusal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		w.Write([]byte(`{"code":"forbidden","message":"an interactive session cannot be held for approval; use a person's token"}`))
	}))
	defer srv.Close()
	_, _, err := OpenSession(context.Background(), sessionTarget(t, srv), session.Request{Kind: "exec", App: "api", Command: []string{"sh"}})
	if err == nil || err.Error() != "agent returned HTTP 403: an interactive session cannot be held for approval; use a person's token" {
		t.Fatalf("error %v", err)
	}
}

func TestOpenSessionGivesUpWhenItsContextEnds(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-release }))
	defer srv.Close()
	defer close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, _, err := OpenSession(ctx, sessionTarget(t, srv), session.Request{Kind: "exec", App: "api", Command: []string{"sh"}}); err == nil {
		t.Fatal("no error")
	}
	if time.Since(start) > 3*time.Second {
		t.Error("it did not give up")
	}
}

func TestOpenSessionWithAnUnreachableAgent(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	target := sessionTarget(t, srv)
	srv.Close()
	if _, _, err := OpenSession(context.Background(), target, session.Request{Kind: "exec", App: "api", Command: []string{"sh"}}); err == nil {
		t.Fatal("no error")
	}
}

func TestAnInteractiveSessionAgainstAnAgentWithoutProjectsSaysToUpgrade(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/environments/lab/agent" {
			w.WriteHeader(http.StatusOK)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	_, _, err := OpenSession(context.Background(), sessionTarget(t, srv), session.Request{Kind: "exec", App: "api", Command: []string{"sh"}})
	if err == nil || !strings.Contains(err.Error(), OlderAgentMessage) {
		t.Fatalf("error %v", err)
	}
}
