package agent

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestListenValidation(t *testing.T) {
	c := Config{Project: "demo", Environment: "lab", Tokens: []Token{{Digest: Digest("x"), Scope: "read"}}}
	for _, x := range []struct {
		addr string
		ok   bool
	}{{"127.0.0.1:1234", true}, {"100.64.1.1:1234", true}, {"[fd00::1]:1234", true}, {"0.0.0.0:1234", false}, {"8.8.8.8:1234", false}, {"localhost:1234", false}, {"127.0.0.1:0", false}} {
		c.Listen = x.addr
		if (c.Validate() == nil) != x.ok {
			t.Errorf("%s", x.addr)
		}
	}
}
func TestOutageKeepsDoctorAndDiskAvailable(t *testing.T) {
	token := "bty.demo.lab." + strings.Repeat("a", 64)
	h := Handler(Config{Project: "demo", Environment: "lab", Tokens: []Token{{Digest: Digest(token), Scope: "read"}}}, nil)
	for _, x := range []struct {
		path string
		code int
	}{{"agent", 200}, {"doctor", 200}, {"df", 200}, {"apps", 503}} {
		r := httptest.NewRequest(http.MethodGet, "/v1/projects/demo/environments/lab/"+x.path, nil)
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != x.code {
			t.Errorf("%s: %d", x.path, w.Code)
		}
		var v any
		if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
			t.Errorf("%s: %v", x.path, err)
		}
	}
}
