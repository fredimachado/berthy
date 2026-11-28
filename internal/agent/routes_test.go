package agent

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"

	"berthy/internal/version"
)

func TestReadRouteUsesItsDeclaredScope(t *testing.T) {
	c := Config{Project: "demo", Environment: "lab", Tokens: []Token{
		{Name: "reader", Digest: Digest(apReadToken), Scope: "read"},
		{Name: "deployer", Digest: Digest(apDeployToken), Scope: "deploy"},
	}}
	rt := &routeRuntime{tokens: newTokenSource(c, nil)}
	row := &envRoute{pattern: "apps/{app}/logs", method: "GET", scope: "deploy", lane: legacyReadRoute,
		handle: func(_ *routeRuntime, call routeCall) { call.w.WriteHeader(http.StatusNoContent) },
	}
	for _, tc := range []struct {
		name, token string
		status      int
		body        string
	}{
		{"read scope", apReadToken, http.StatusForbidden, "{\"code\":\"forbidden\",\"message\":\"forbidden\"}\n"},
		{"deploy scope", apDeployToken, http.StatusNoContent, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			r := httptest.NewRequest("GET", "/apps/web/logs", nil)
			r.Header.Set("Authorization", "Bearer "+tc.token)
			rt.serveRead(routeCall{w: w, r: r, row: row})
			if w.Code != tc.status || w.Body.String() != tc.body {
				t.Errorf("got %d %q, want %d %q for %s", w.Code, w.Body.String(), tc.status, tc.body, row.pattern)
			}
		})
	}
}

func TestRouteRefusalOrder(t *testing.T) {
	c := Config{Project: "demo", Environment: "lab", Tokens: []Token{
		{Name: "reader", Digest: Digest(apReadToken), Scope: "read"},
		{Name: "deployer", Digest: Digest(apDeployToken), Scope: "deploy"},
		{Name: "admin", Digest: Digest(apAdminToken), Scope: "admin"},
		{Name: "agent", Digest: Digest(apAgentAdmin), Scope: "admin", Agent: true},
	}}
	host, _ := os.Hostname()
	info, err := json.Marshal(map[string]any{
		"project": c.Project, "environment": c.Environment, "hostname": host, "version": version.String(),
		"protected": false, "effective_protected": nil,
	})
	if err != nil {
		t.Fatal(err)
	}
	const unauthorized = `{"code":"unauthorized","message":"unauthorized"}`
	const notFound = `{"code":"not_found","message":"not found"}`
	const forbidden = `{"code":"forbidden","message":"forbidden"}`
	const wrongMethod = `{"code":"method_not_allowed","message":"method not allowed"}`
	const upgrade = `{"code":"not_found","message":"this agent serves /v1/projects/\u003cproject\u003e/environments/\u003cenvironment\u003e/: upgrade the CLI"}`
	for _, entry := range []struct {
		name       string
		handler    http.Handler
		deployment bool
	}{
		{"read", Handler(c, nil), false},
		{"deployment", NewDeploymentHandler(c, nil, nil, nil), true},
	} {
		t.Run(entry.name, func(t *testing.T) {
			for _, tc := range []struct {
				name, method, path, token string
				status                    int
				body                      string
				mutation                  bool
			}{
				{"unknown without a token", "GET", labPrefix + "unknown", "", 401, unauthorized, false},
				{"unknown with a token", "GET", labPrefix + "unknown", apReadToken, 404, notFound, false},
				{"old URL", "GET", "/v1/environments/lab/agent", apReadToken, 404, upgrade, false},
				{"trailing slash", "GET", labPrefix + "agent/", apReadToken, 200, string(info), false},
				{"HEAD with no token", "HEAD", labPrefix + "agent", "", 401, unauthorized, false},
				{"HEAD with a token", "HEAD", labPrefix + "agent", apReadToken, 405, wrongMethod, false},
				{"wrong verb without a token", "PUT", labPrefix + "deploys", "", 401, unauthorized, true},
				{"wrong verb with insufficient scope", "PUT", labPrefix + "deploys", apReadToken, 403, forbidden, true},
				{"wrong verb with sufficient scope", "PUT", labPrefix + "deploys", apDeployToken, 405, wrongMethod, true},
				{"agent token management", "PUT", labPrefix + "tokens", apAgentAdmin, 403, `{"code":"forbidden","message":"an agent token cannot manage tokens"}`, true},
				{"agent image lookup", "POST", labPrefix + "images/resolve", apAgentAdmin, 403, forbidden, true},
				{"human image lookup without state", "POST", labPrefix + "images/resolve", apAdminToken, 503, `{"code":"state_unavailable","message":"state unavailable"}`, true},
			} {
				t.Run(tc.name, func(t *testing.T) {
					status, body := tc.status, tc.body
					if tc.mutation && !entry.deployment && tc.token != "" {
						status, body = 404, notFound
					}
					w := serve(entry.handler, tc.method, tc.path, tc.token, "")
					if w.Code != status || w.Body.String() != body+"\n" || w.Header().Get("Content-Type") != "application/json" || w.Header().Get("Location") != "" {
						t.Errorf("got %d %q, headers %v, want %d %q with JSON and no Location", w.Code, w.Body.String(), w.Header(), status, body+"\n")
					}
				})
			}
		})
	}
}

func TestRouteLookupCompatibility(t *testing.T) {
	for _, tc := range []struct {
		path, method      string
		scope, wantMethod string
		kind, summary     string
		dryRun            bool
		target            string
	}{
		{path: "tokens", method: "GET", scope: "admin", wantMethod: "GET"},
		{path: "tokens", method: "POST", scope: "admin", wantMethod: "POST"},
		{path: "tokens", method: "PUT", scope: "admin", wantMethod: "POST"},
		{path: "secrets", method: "GET", scope: "admin", wantMethod: "GET", kind: "secret set", summary: "secret set NAME", dryRun: true},
		{path: "secrets", method: "POST", scope: "admin", wantMethod: "POST", kind: "secret set", summary: "secret set NAME", dryRun: true},
		{path: "secrets", method: "PUT", scope: "admin", wantMethod: "POST", kind: "secret set", summary: "secret set NAME", dryRun: true},
		{path: "secrets/import", method: "POST", scope: "admin", wantMethod: "POST", kind: "secret import", summary: "secret import 1 secret", dryRun: true},
		{path: "secrets/import", method: "DELETE", scope: "admin", wantMethod: "DELETE", kind: "secret delete", summary: "secret delete import", dryRun: true},
		{path: "secrets/import", method: "GET", scope: "admin", wantMethod: "DELETE", kind: "secret delete", summary: "secret delete import", dryRun: true},
		{path: "secrets/generate", method: "POST", scope: "admin", wantMethod: "POST", kind: "secret generate", summary: "secret generate NAME", dryRun: true},
		{path: "secrets/generate", method: "DELETE", scope: "admin", wantMethod: "DELETE", kind: "secret delete", summary: "secret delete generate", dryRun: true},
		{path: "secrets/generate", method: "GET", scope: "admin", wantMethod: "DELETE", kind: "secret delete", summary: "secret delete generate", dryRun: true},
		{path: "secrets/generate-declared", method: "POST", scope: "admin", wantMethod: "POST", kind: "secret generate declared", summary: "generate the declared secrets", dryRun: true},
		{path: "secrets/generate-declared", method: "DELETE", scope: "admin", wantMethod: "DELETE", kind: "secret delete", summary: "secret delete generate-declared", dryRun: true},
		{path: "secrets/generate-declared", method: "GET", scope: "admin", wantMethod: "DELETE", kind: "secret delete", summary: "secret delete generate-declared", dryRun: true},
		{path: "secrets/OLD", method: "POST", scope: "admin", wantMethod: "DELETE", kind: "secret delete", summary: "secret delete OLD", dryRun: true},
		{path: "operations//cancel", method: "POST", scope: "deploy", wantMethod: "POST", kind: "cancel", summary: "cancel ", dryRun: true},
		{path: "jobs/line\nbreak/run", method: "POST", scope: "deploy", wantMethod: "POST", kind: "job run", summary: "job run line?break", target: "line\nbreak"},
		{path: "operations/id/unknown", method: "GET"},
		{path: "secrets/generate/unknown", method: "POST"},
		{path: "apps/example/logs", method: "GET"},
	} {
		t.Run(tc.method+"/"+tc.path, func(t *testing.T) {
			rest := strings.Split(tc.path, "/")
			if scope, method, ok := routeScope(rest, tc.method); scope != tc.scope || method != tc.wantMethod || ok != (tc.scope != "") {
				t.Errorf("routeScope = (%q, %q, %t), want (%q, %q, %t)", scope, method, ok, tc.scope, tc.wantMethod, tc.scope != "")
			}
			if kind := routeKind(rest, tc.method); kind != tc.kind {
				t.Errorf("routeKind = %q, want %q", kind, tc.kind)
			}
			if dryRun := supportsDryRun(rest, tc.method); dryRun != tc.dryRun {
				t.Errorf("supportsDryRun = %t, want %t", dryRun, tc.dryRun)
			}
			wantDescription := map[string]string{"action": tc.kind}
			if tc.target != "" {
				wantDescription["target"] = tc.target
			}
			if description := describe(rest, tc.method); !reflect.DeepEqual(description, wantDescription) {
				t.Errorf("describe = %v, want %v", description, wantDescription)
			}
			wantSummary := tc.summary
			if wantSummary == "" {
				wantSummary = "change"
			}
			if summary := summarize(rest, tc.method, []byte(`{"name":"NAME","secrets":[{}]}`)); summary != wantSummary {
				t.Errorf("summarize = %q, want %q", summary, wantSummary)
			}
		})
	}
}

func TestRouteDefinitions(t *testing.T) {
	optional := map[string]bool{
		"POST operations/{id}/cancel": true,
		"DELETE tokens/{name}":        true,
		"POST approvals/{id}/reject":  true,
	}
	for i := range envRoutes {
		row := &envRoutes[i]
		key := row.method + " " + row.pattern
		t.Run(key, func(t *testing.T) {
			if row.handle == nil {
				t.Errorf("%s has no handler", key)
			}
			rest := strings.Split(row.pattern, "/")
			for i, segment := range rest {
				if strings.HasPrefix(segment, "{") && strings.HasSuffix(segment, "}") {
					rest[i] = "example"
				}
			}
			if got := findEnvRoute(rest, row.method, false); got != row {
				t.Errorf("%s resolves to %v, want its own row", key, got)
			}
			if row.gateDryRun && (row.body == nil || row.body.Kind() != reflect.Struct || !setDryRun(reflect.New(row.body).Interface())) {
				t.Errorf("%s has no usable dry_run field in %v", key, row.body)
			}
			if row.kind != "" && (row.summary == nil || row.summary(row.kind, rest, summaryBody{}) == "") {
				t.Errorf("%s has no summary for %q", key, row.kind)
			}
			if row.optionalBody != optional[key] {
				t.Errorf("%s optionalBody = %t, want %t", key, row.optionalBody, optional[key])
			}
			if row.optionalBody && row.body == nil {
				t.Errorf("%s has no registered optional body", key)
			}
		})
		delete(optional, key)
	}
	for key := range optional {
		t.Errorf("%s is missing from the route table", key)
	}
}

func dryRunKinds(yield func(int, string) bool) {
	for i, row := range envRoutes {
		if row.gateDryRun && !yield(i, row.kind) {
			return
		}
	}
}
