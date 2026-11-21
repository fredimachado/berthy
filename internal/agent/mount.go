package agent

import (
	"net/http"
	"net/url"
	"strings"

	"berthy/internal/scope"
)

// apiPrefix is the path prefix of the environment's API. It is the one place that builds it.
func apiPrefix(c Config) string { return c.ID().APIPath() }

// mount serves h under prefix. h sees the path after the prefix ("apps", "operations/ID/events"). A request outside the
// prefix is refused by refuseOutside.
func mount(prefix string, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rest, ok := strings.CutPrefix(r.URL.Path, prefix)
		if !ok {
			refuseOutside(w, r.URL.Path)
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

// environmentPath splits the path of a hosted environment's API, /v1/projects/<project>/environments/<environment>/<rest>.
// ok is false for any other path. rest is empty for the environment's own path.
func environmentPath(path string) (id scope.ID, rest []string, ok bool) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) < 5 || parts[0] != "v1" || parts[1] != "projects" || parts[3] != "environments" {
		return scope.ID{}, nil, false
	}
	return scope.ID{Project: parts[2], Environment: parts[4]}, parts[5:], true
}

// refuseOutside answers a request for a path that no hosted environment of the handler serves. It is the one place that
// decides what such a request gets. The path of an environment's API is answered as a bad token is, so the answer does not
// say which environments this agent serves. The URL of an earlier release, which had no project, is told to upgrade the CLI.
// Anything else is not found.
func refuseOutside(w http.ResponseWriter, path string) {
	w.Header().Set("Content-Type", "application/json")
	if _, rest, ok := environmentPath(path); ok && len(rest) > 0 {
		refuseToken(w, http.StatusUnauthorized)
		return
	}
	if strings.HasPrefix(path, "/v1/environments/") {
		writeError(w, 404, "not_found", "this agent serves /v1/projects/<project>/environments/<environment>/: upgrade the CLI")
		return
	}
	writeError(w, 404, "not_found", "not found")
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
