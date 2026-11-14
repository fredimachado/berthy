//go:build linux

package agent

import (
	"berthy/internal/engine"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestDoctorReportsIncompatibleReachableDocker(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "docker.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	server := http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/version" {
			w.Write([]byte(`{"ApiVersion":"1.50","MinAPIVersion":"1.45"}`))
			return
		}
		http.Error(w, "unsupported", 400)
	})}
	go server.Serve(l)
	defer server.Close()
	token := "bty.lab." + strings.Repeat("a", 64)
	h := Handler(Config{Environment: "lab", Tokens: []Token{{Digest: Digest(token), Scope: "read"}}}, engine.New(sock))
	r := httptest.NewRequest("GET", "/v1/environments/lab/doctor", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if !strings.Contains(w.Body.String(), `"docker_api"`) || !strings.Contains(w.Body.String(), "1.45") || !strings.Contains(w.Body.String(), `"docker","ok":true`) {
		t.Fatal(w.Body.String())
	}
}
