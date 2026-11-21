package agent

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// seen is a handler that records the path it was given.
func seen(path *string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*path = r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	})
}

const labPrefix = "/v1/projects/demo/environments/lab/"

func TestMountStripsThePrefix(t *testing.T) {
	for _, tc := range []struct{ request, want string }{
		{"/v1/projects/demo/environments/lab/apps", "apps"},
		{"/v1/projects/demo/environments/lab/operations/op1/events", "operations/op1/events"},
		{"/v1/projects/demo/environments/lab/", ""},
	} {
		var got string
		w := httptest.NewRecorder()
		mount(labPrefix, seen(&got)).ServeHTTP(w, httptest.NewRequest("GET", tc.request, nil))
		if w.Code != http.StatusNoContent || got != tc.want {
			t.Errorf("%s: handler saw %q with status %d, want %q", tc.request, got, w.Code, tc.want)
		}
	}
}

func TestARequestForAnotherEnvironmentIs401(t *testing.T) {
	for _, path := range []string{
		"/v1/projects/demo/environments/other/apps",
		"/v1/projects/demo/environments/labs/apps",
		"/v1/projects/other/environments/lab/apps",
		"/v1/projects/demo/environments/other/anything/at/all",
	} {
		var got string
		w := httptest.NewRecorder()
		mount(labPrefix, seen(&got)).ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != http.StatusUnauthorized || got != "" || w.Body.String() != "{\"code\":\"unauthorized\",\"message\":\"unauthorized\"}\n" {
			t.Errorf("%s: %d %q, handler saw %q", path, w.Code, w.Body.String(), got)
		}
	}
}

func TestAPathThatIsNotAnAPIPathIs404(t *testing.T) {
	for _, path := range []string{"/", "/v1", "/v1/projects", "/v1/projects/demo", "/v1/projects/demo/environments", "/v1/projects/demo/environments/other", "/v1/environments", "/v2/projects/demo/environments/lab/apps", "/apps"} {
		var got string
		w := httptest.NewRecorder()
		mount(labPrefix, seen(&got)).ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != http.StatusNotFound || got != "" || !strings.Contains(w.Body.String(), `"message":"not found"`) {
			t.Errorf("%s: %d %s, handler saw %q", path, w.Code, w.Body.String(), got)
		}
	}
}

func TestAPrefixIsTheEnvironmentsAPIPath(t *testing.T) {
	if got := apiPrefix(Config{Project: "demo", Environment: "lab"}); got != labPrefix {
		t.Fatalf("prefix %q", got)
	}
}
