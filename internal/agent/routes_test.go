package agent

import (
	"reflect"
	"strings"
	"testing"
)

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
