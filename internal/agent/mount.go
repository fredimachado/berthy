package agent

import (
	"net/http"
	"net/url"
	"strings"
)

// apiPrefix is the path prefix of the environment's API. It is the one place that builds it.
func apiPrefix(c Config) string { return "/v1/environments/" + c.Environment + "/" }

// mount serves h under prefix. h sees the path after the prefix ("apps", "operations/ID/events"). A request outside the
// prefix is answered 401 when it has the shape of an environment's API, and 404 otherwise.
func mount(prefix string, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rest, ok := strings.CutPrefix(r.URL.Path, prefix)
		if !ok {
			w.Header().Set("Content-Type", "application/json")
			if isEnvironmentPath(r.URL.Path) {
				// The same answer as a bad token, so it does not say which environments this agent serves.
				refuseToken(w, http.StatusUnauthorized)
			} else {
				writeError(w, 404, "not_found", "not found")
			}
			return
		}
		r2 := new(http.Request)
		*r2 = *r
		r2.URL = new(url.URL)
		*r2.URL = *r.URL
		r2.URL.Path, r2.URL.RawPath = rest, ""
		h.ServeHTTP(w, r2)
	})
}

// isEnvironmentPath reports whether path has the shape of an environment's API: /v1/environments/<name>/<something>.
func isEnvironmentPath(path string) bool {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	return len(parts) >= 4 && parts[0] == "v1" && parts[1] == "environments"
}

// serveAPI is the handler of an environment's agent: /healthz, which names no environment, and h under the environment's
// API prefix.
func serveAPI(c Config, h http.Handler) http.Handler {
	api := mount(apiPrefix(c), h)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if serveHealth(w, r) {
			return
		}
		api.ServeHTTP(w, r)
	})
}
