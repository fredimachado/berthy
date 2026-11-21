package agent

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// seen is a handler that records the path it was given.
func seen(path *string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*path = r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	})
}

func TestMountStripsThePrefix(t *testing.T) {
	for _, tc := range []struct{ request, want string }{
		{"/v1/environments/lab/apps", "apps"},
		{"/v1/environments/lab/operations/op1/events", "operations/op1/events"},
		{"/v1/environments/lab/", ""},
	} {
		var got string
		w := httptest.NewRecorder()
		mount("/v1/environments/lab/", seen(&got)).ServeHTTP(w, httptest.NewRequest("GET", tc.request, nil))
		if w.Code != http.StatusNoContent || got != tc.want {
			t.Errorf("%s: handler saw %q with status %d, want %q", tc.request, got, w.Code, tc.want)
		}
	}
}

func TestARequestForAnotherEnvironmentIs401(t *testing.T) {
	for _, path := range []string{"/v1/environments/other/apps", "/v1/environments/labs/apps", "/v1/environments/other/anything/at/all"} {
		var got string
		w := httptest.NewRecorder()
		mount("/v1/environments/lab/", seen(&got)).ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != http.StatusUnauthorized || got != "" || w.Body.String() != "{\"code\":\"unauthorized\",\"message\":\"unauthorized\"}\n" {
			t.Errorf("%s: %d %q, handler saw %q", path, w.Code, w.Body.String(), got)
		}
	}
}

func TestAPathThatIsNotAnAPIPathIs404(t *testing.T) {
	for _, path := range []string{"/", "/v1", "/v1/environments", "/v1/environments/lab", "/v1/environments/other", "/v2/environments/lab/apps", "/apps"} {
		var got string
		w := httptest.NewRecorder()
		mount("/v1/environments/lab/", seen(&got)).ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != http.StatusNotFound || got != "" {
			t.Errorf("%s: %d, handler saw %q", path, w.Code, got)
		}
	}
}

func TestAPrefixIsTheEnvironmentsAPIPath(t *testing.T) {
	if got := apiPrefix(Config{Environment: "lab"}); got != "/v1/environments/lab/" {
		t.Fatalf("prefix %q", got)
	}
}
