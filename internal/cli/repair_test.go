package cli

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRemoteErrorDoesNotEchoToken(t *testing.T) {
	token := "secret-echo"
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		fmt.Fprint(w, r.Header.Get("Authorization"))
	}))
	defer s.Close()
	t.Setenv("BERTHY_TEST_TOKEN", token)
	p := filepath.Join(t.TempDir(), "client.json")
	os.WriteFile(p, []byte(fmt.Sprintf(`{"environments":{"lab":{"url":%q,"token_env":"BERTHY_TEST_TOKEN"}}}`, s.URL)), 0600)
	var out, errs bytes.Buffer
	if Run([]string{"ps", "-e", "lab", "--config", p}, &out, &errs) == 0 || strings.Contains(errs.String(), token) {
		t.Fatalf("stderr %s", errs.String())
	}
}
