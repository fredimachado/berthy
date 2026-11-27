package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"berthy/internal/monitor"
)

func TestAgentSetupProbeHealth(t *testing.T) {
	t.Run("version", func(t *testing.T) {
		withStdin(t, "")
		var out, errout bytes.Buffer
		if code := Run([]string{"agent", "setup-probe", "version"}, &out, &errout); code != 0 ||
			out.String() != "berthy-setup-probe/1\n" || errout.Len() != 0 {
			t.Fatalf("version: exit %d, stdout %q, stderr %q", code, &out, &errout)
		}
	})

	for _, tt := range []struct {
		name   string
		status int
		code   int
	}{
		{"ready", http.StatusOK, 0},
		{"redirect", http.StatusFound, 1},
		{"unavailable", http.StatusServiceUnavailable, 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != http.MethodGet || r.URL.RequestURI() != "/healthz" || r.Header.Get("Authorization") != "" {
					t.Error("expected an unauthenticated GET /healthz")
				}
				w.Header().Set("Location", "/would-be-ready")
				if r.URL.Path == "/would-be-ready" {
					w.WriteHeader(http.StatusOK)
					return
				}
				w.WriteHeader(tt.status)
			}))
			defer srv.Close()
			var out, errout bytes.Buffer
			args := []string{"agent", "setup-probe", "health", "--listen", srv.Listener.Addr().String(), "--timeout", "3s"}
			if code := Run(args, &out, &errout); code != tt.code || out.Len() != 0 || errout.Len() != 0 || calls.Load() != 1 {
				t.Fatalf("health: exit %d, want %d, stdout %q, stderr %q, requests %d", code, tt.code, &out, &errout, calls.Load())
			}
		})
	}

	for _, tt := range []struct{ name, listen, timeout string }{
		{"host name", "localhost:7710", "1s"},
		{"wildcard", "0.0.0.0:7710", "1s"},
		{"unbracketed IPv6", "::1:7710", "1s"},
		{"missing port", "127.0.0.1", "1s"},
		{"invalid duration", "127.0.0.1:7710", "3"},
		{"zero duration", "127.0.0.1:7710", "0s"},
		{"negative duration", "127.0.0.1:7710", "-1s"},
		{"long duration", "127.0.0.1:7710", "3001ms"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var out, errout bytes.Buffer
			args := []string{"agent", "setup-probe", "health", "--listen", tt.listen, "--timeout", tt.timeout}
			if code := Run(args, &out, &errout); code != 2 || out.Len() != 0 {
				t.Fatalf("invalid health arguments: exit %d, stdout %q, stderr %q", code, &out, &errout)
			}
		})
	}

	t.Run("request deadline", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			client, peer := net.Pipe()
			defer client.Close()
			defer peer.Close()
			dialed := false
			dial := func(context.Context, string, string) (net.Conn, error) {
				dialed = true
				return client, nil
			}
			const budget = 750 * time.Millisecond
			start := time.Now()
			result := monitor.ProbePeer(t.Context(), "http://127.0.0.1:7710", budget, dial)
			synctest.Wait()
			if elapsed := time.Since(start); !dialed || elapsed != budget || result.State != monitor.Critical || result.Detail != "silent: timeout" {
				t.Fatalf("blocked health request: dialed %t, elapsed %s, result %+v", dialed, elapsed, result)
			}
		})
	})
}

func TestAgentSetupProbeDoctor(t *testing.T) {
	const token = "setup-test-token"
	const reply = `{"checks":[{"name":"docker","state":"ok"}]}`
	for _, tt := range []struct {
		name, input, scope string
		status, code       int
		wantOut            string
	}{
		{"LF", token + "\n", "shop/staging", http.StatusOK, 0, reply},
		{"CRLF", token + "\r\n", "shop/staging", http.StatusOK, 0, reply},
		{"empty token", "\n", "shop/staging", 0, 2, ""},
		{"multiline token", token + "\nextra\n", "shop/staging", 0, 2, ""},
		{"oversized token", strings.Repeat("x", 8192), "shop/staging", 0, 2, ""},
		{"invalid scope", token + "\n", "staging", 0, 2, ""},
		{"unauthorized", token + "\n", "shop/staging", http.StatusUnauthorized, 1, ""},
		{"agent error", token + "\n", "shop/staging", http.StatusInternalServerError, 1, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			withStdinFile(t, tt.input)
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != http.MethodGet || r.URL.RequestURI() != "/v1/projects/shop/environments/staging/doctor" ||
					r.Header.Get("Authorization") != "Bearer "+token {
					t.Error("expected a scoped doctor GET using the stdin token")
				}
				status := tt.status
				if status == 0 {
					status = http.StatusBadRequest
				}
				w.WriteHeader(status)
				if status == http.StatusOK {
					io.WriteString(w, reply)
				} else {
					io.WriteString(w, `{"message":"rejected `+token+`"}`)
				}
			}))
			defer srv.Close()

			var out, errout bytes.Buffer
			args := []string{"agent", "setup-probe", "doctor", "--listen", srv.Listener.Addr().String(), "--scope", tt.scope}
			code := Run(args, &out, &errout)
			if strings.Contains(out.String(), token) || strings.Contains(errout.String(), token) {
				t.Fatal("doctor displayed the stdin token")
			}
			var wantCalls int32
			if tt.status != 0 {
				wantCalls = 1
			}
			if code != tt.code || out.String() != tt.wantOut || calls.Load() != wantCalls {
				t.Fatalf("doctor: exit %d, want %d, stdout %q, stderr %q, requests %d, want %d", code, tt.code, &out, &errout, calls.Load(), wantCalls)
			}
			if tt.code == 0 && errout.Len() != 0 {
				t.Errorf("successful doctor wrote stderr: %q", &errout)
			}
			if tt.code == 1 && !strings.Contains(errout.String(), fmt.Sprintf("HTTP %d", tt.status)) {
				t.Errorf("doctor did not report HTTP %d: %q", tt.status, &errout)
			}
		})
	}
}
