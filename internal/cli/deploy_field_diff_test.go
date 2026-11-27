package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"berthy/internal/api"
	"berthy/internal/deploy"
)

var fieldDiffImage = "docker.io/library/alpine@sha256:" + strings.Repeat("a", 64)

func fieldImagePlan(tag, compact, extension string) string {
	plan := fmt.Sprintf(`{"app":"api","image":%q,"tag":%q,"roles":["web"],"diff":%s`, fieldDiffImage, tag, compact)
	if extension != "" {
		plan += `,"release_diff":` + extension
	}
	return plan + "}"
}

const sameFieldDiff = `{"same":true}`
const changedFieldDiff = `{"changes":[{"area":"web","change":"replicas 2 -> 3"}]}`
const replicaFieldDiff = `{"base":17,"fields":[{"path":["roles","web","replicas"],"before":"2","after":"3"}]}`
const fieldHeading = "Deploy demo/dev app api\n"

type fieldDeployCall struct {
	query string
	req   deploy.Request
}

func TestDeployDiffFlagArity(t *testing.T) {
	var callsMu sync.Mutex
	var calls []fieldDeployCall
	var history []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if answerCapablePolicy(w, r) {
			return
		}
		callsMu.Lock()
		defer callsMu.Unlock()
		switch {
		case strings.HasSuffix(r.URL.Path, "/deploys"):
			var req deploy.Request
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Error(err)
				return
			}
			calls = append(calls, fieldDeployCall{r.URL.RawQuery, req})
			fmt.Fprint(w, fieldImagePlan("v1", changedFieldDiff, replicaFieldDiff))
		case strings.HasSuffix(r.URL.Path, "/history/api/diff"):
			history = append(history, r.URL.RawQuery)
			fmt.Fprint(w, sameFieldDiff)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	cfg := ingressCLIConfig(t, server.URL)
	dir := writeAliasDir(t, "aliases:\n  preview: deploy --diff api\n  compared: history api --diff 2 1\n"+versionsManifest)
	common := []string{"-e", "dev", "--config", cfg, "--manifest-dir", dir}
	for _, tc := range []struct{ args, tail []string }{
		{[]string{"deploy", "--diff", "api", "--version", "v1"}, []string{"--dry-run"}},
		{[]string{"deploy", "--version", "api=v1"}, []string{"--dry-run", "--diff"}},
		{[]string{"preview", "--version", "v1"}, []string{"--dry-run"}},
		{[]string{"history", "api", "--diff", "2"}, nil},
		{[]string{"compared"}, nil},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			var out, errs bytes.Buffer
			args := slices.Concat(tc.args, common, tc.tail)
			if tc.args[0] == "history" {
				args = slices.Concat(tc.args, common[:4])
			}
			if code := Run(args, &out, &errs); code != 0 {
				t.Fatalf("exit %d: %s", code, errs.String())
			}
		})
	}
	callsMu.Lock()
	defer callsMu.Unlock()
	if len(calls) != 3 {
		t.Errorf("deploy requests: %d, want 3", len(calls))
	}
	for _, call := range calls {
		if call.query != "diff=fields" || call.req.App != "api" || !call.req.DryRun {
			t.Errorf("unexpected deploy: %q app %q dry-run %v", call.query, call.req.App, call.req.DryRun)
		}
	}
	if !slices.Equal(history, []string{"release=2", "base=1&release=2"}) {
		t.Errorf("history queries: %v", history)
	}
	if err := checkAliases(map[string]string{"bad": "deploy --diff --config fake api"}); err == nil || !strings.Contains(err.Error(), "--config is not allowed") {
		t.Errorf("forbidden alias config: %v", err)
	}
}

func TestDeployDiffRendersSafeOrderedPairs(t *testing.T) {
	for _, tc := range []struct {
		name, compact, extension, block string
		color, noColor                  bool
	}{
		{"redirected", changedFieldDiff, `{"base":17,"fields":[
		  {"path":["roles","web","replicas"],"before":"2","after":"3"},
		  {"path":["roles","web","health","start_period"],"before":"30s","after":"60s"},
		  {"path":["env","test-token\u001b"],"after":"test-token old -> new\r\n\u009b"},
		  {"path":["env","REMOVED"],"before":"gone"},
		  {"path":["compose","database"],"note":"recreate database"}]}`,
			"  roles:\n    web:\n      - replicas: 2\n      + replicas: 3\n      health:\n        - start_period: 30s\n        + start_period: 60s\n  env:\n    + [redacted]\\x1b: [redacted] old -> new  \\x9b\n    - REMOVED: gone\n  compose:\n    recreate database\n", false, false},
		{"colored", changedFieldDiff, replicaFieldDiff, "  roles:\n    web:\n      \x1b[31m- replicas: 2\x1b[0m\n      \x1b[32m+ replicas: 3\x1b[0m\n", true, false},
		{"NO_COLOR", changedFieldDiff, replicaFieldDiff, "  roles:\n    web:\n      - replicas: 2\n      + replicas: 3\n", true, true},
		{"empty addition", sameFieldDiff, `{"base":17,"fields":[{"path":["roles","web","logging","options","option.name"],"after":""}]}`,
			"  roles:\n    web:\n      logging:\n        options:\n          + option.name: \"\"\n", false, false},
		{"empty removal", sameFieldDiff, `{"base":17,"fields":[{"path":["roles","web","logging","options","option.name"],"before":""}]}`,
			"  roles:\n    web:\n      logging:\n        options:\n          - option.name: \"\"\n", false, false},
		{"new", `{"new":true}`, `{"base":0,"fields":[]}`, "  new app\n", false, false},
		{"unchanged", sameFieldDiff, `{"base":17,"fields":[]}`, "  no changes: the same release would be recorded\n", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, _ := planDiffAgent(t, fieldImagePlan("v1", tc.compact, tc.extension))
			var out, errs bytes.Buffer
			if tc.color {
				slice5Destination(t, &out)
			}
			if tc.noColor {
				t.Setenv("NO_COLOR", "1")
			}
			code := Run([]string{"deploy", "api", "--version", "v1", "--dry-run", "--diff", "-e", "dev", "--config", cfg, "--manifest-dir", apiManifestDir(t)}, &out, &errs)
			got := fieldOutputBlock(out.String())
			if code != 0 || got != fieldHeading+tc.block {
				t.Fatalf("exit %d stderr %q\nfield block %q\nwant %q", code, errs.String(), got, fieldHeading+tc.block)
			}
		})
	}
}

func fieldOutputBlock(output string) string {
	_, lines, ok := strings.Cut(output, fieldHeading)
	if !ok {
		return ""
	}
	block := fieldHeading
	for _, line := range strings.Split(lines, "\n") {
		if !strings.HasPrefix(line, "  ") {
			break
		}
		block += line + "\n"
	}
	return block
}

func TestDeployDiffShowsStatefulComposeBlockers(t *testing.T) {
	const reason = "stateful app db would recreate database; deploy with --allow-recreate and an admin token"
	for _, tc := range []struct {
		name, action string
		dry          bool
	}{
		{"preview", "recreate", true},
		{"execution", "recreate", false},
		{"invalid action", "restart", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan := fmt.Sprintf(`{"app":"db","image":"","roles":[],"blockers":[%q],"compose":{"project":"demo-dev-db","services":[{"name":"database","action":%q}],"volumes":[],"blockers":[%q]},"diff":{"changes":[{"area":"compose","change":"recreate database"}]},"release_diff":{"base":2,"fields":[{"path":["compose","database"],"note":"recreate database"}]}}`, reason, tc.action, reason)
			cfg, paths := planDiffAgent(t, plan)
			dir := writeComposeProject(t, composeManifest, "services:\n  database:\n    image: postgres:16\n")
			args := []string{"deploy", "db", "--diff", "-e", "dev", "--config", cfg, "--manifest-dir", dir}
			if tc.dry {
				args = append(args, "--dry-run")
			}
			var out, errs bytes.Buffer
			code := Run(args, &out, &errs)
			if !slices.Equal(paths(), []string{"POST /v1/projects/demo/environments/dev/deploys?diff=fields"}) {
				t.Errorf("unexpected requests: %v", paths())
			}
			if tc.action == "recreate" {
				wantCode := 1
				if tc.dry {
					wantCode = 0
				}
				if code != wantCode || strings.Count(out.String(), reason) != 1 || !strings.Contains(out.String(), "\n    recreate database\n") || strings.Contains(out.String(), "Proceed with") {
					t.Errorf("exit %d stdout %q stderr %q", code, out.String(), errs.String())
				}
			} else if code != 1 || out.Len() != 0 || !strings.Contains(errs.String(), "invalid agent deploy plan") {
				t.Errorf("malformed blocked plan: exit %d stdout %q stderr %q", code, out.String(), errs.String())
			}
		})
	}
}

func TestDeployDiffShowsIngressConflictsOnce(t *testing.T) {
	const reason = "route api.example.com belongs to app other"
	for _, tc := range []struct {
		name, blockers, conflicts, changes string
		dry, valid                         bool
	}{
		{"preview", `["` + reason + `"]`, `["` + reason + `"]`, `[]`, true, true},
		{"execution", `["` + reason + `","` + reason + `"]`, `["` + reason + `"]`, `[]`, false, true},
		{"unlisted conflict", `["another blocker"]`, `["` + reason + `"]`, `[]`, true, false},
		{"null conflict entry", `["` + reason + `"]`, `[null]`, `[]`, true, false},
		{"invalid later decision", `["` + reason + `"]`, `["` + reason + `"]`, `[{"hostname":"api.example.com","route":"unknown"}]`, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan := strings.TrimSuffix(fieldImagePlan("v1", changedFieldDiff, replicaFieldDiff), "}") +
				fmt.Sprintf(`,"blockers":%s,"ingress":{"tunnel":"demo","dns":false,"conflicts":%s,"changes":%s}}`, tc.blockers, tc.conflicts, tc.changes)
			cfg, paths := planDiffAgent(t, plan)
			args := []string{"deploy", "api", "--diff", "--version", "v1", "-e", "dev", "--config", cfg, "--manifest-dir", apiManifestDir(t)}
			if tc.dry {
				args = append(args, "--dry-run")
			}
			var out, errs bytes.Buffer
			code := Run(args, &out, &errs)
			if !slices.Equal(paths(), []string{"POST /v1/projects/demo/environments/dev/deploys?diff=fields"}) {
				t.Errorf("expected only the preview request, got %v", paths())
			}
			if tc.valid {
				want := 1
				if tc.dry {
					want = 0
				}
				if code != want || strings.Count(out.String()+errs.String(), reason) != 1 || strings.Contains(errs.String(), "invalid agent deploy plan") {
					t.Errorf("exit %d, stdout %q, stderr %q", code, &out, &errs)
				}
			} else if code != 1 || out.Len() != 0 || !strings.Contains(errs.String(), "invalid agent deploy plan") {
				t.Errorf("malformed conflict: exit %d, stdout %q, stderr %q", code, &out, &errs)
			}
		})
	}
}

func TestDeployDiffRejectsOverdeepPathBeforeAdmission(t *testing.T) {
	const extension = `{"base":17,"fields":[{"path":["roles","web","logging","options","key","extra"],"after":"value"}]}`
	cfg, paths := planDiffAgent(t, fieldImagePlan("v1", sameFieldDiff, extension))
	var out, errs bytes.Buffer
	code := Run([]string{"deploy", "api", "--diff", "--version", "v1", "-e", "dev", "--config", cfg, "--manifest-dir", apiManifestDir(t)}, &out, &errs)
	if code != 1 || out.Len() != 0 || !strings.Contains(errs.String(), "invalid agent deploy plan") {
		t.Errorf("overdeep path: exit %d, stdout %q, stderr %q", code, &out, &errs)
	}
	if !slices.Equal(paths(), []string{"POST /v1/projects/demo/environments/dev/deploys?diff=fields"}) {
		t.Errorf("expected only the preview request, got %v", paths())
	}
}

type fieldOutputObserver struct {
	bytes.Buffer
	printed atomic.Bool
	short   bool
}

func (w *fieldOutputObserver) Write(p []byte) (int, error) {
	if w.short {
		return len(p) - 1, nil
	}
	if bytes.Contains(p, []byte(fieldHeading)) {
		w.printed.Store(true)
	}
	return w.Buffer.Write(p)
}

func TestDeployDiffUsesPreviewForAdmission(t *testing.T) {
	for _, tc := range []struct {
		name, state, compact, extension string
		want                            int
		dry, short                      bool
	}{
		{"success", "succeeded", changedFieldDiff, replicaFieldDiff, 0, false, false},
		{"rollback", "rolled_back", changedFieldDiff, replicaFieldDiff, 1, false, false},
		{"hold", "held", changedFieldDiff, replicaFieldDiff, 3, false, false},
		{"different held decision", "held", changedFieldDiff, replicaFieldDiff, 3, false, false},
		{"new", "succeeded", `{"new":true}`, `{"base":0,"fields":[]}`, 0, false, false},
		{"unchanged", "succeeded", sameFieldDiff, `{"base":17,"fields":[]}`, 0, false, false},
		{"legacy preview", "", changedFieldDiff, "", 0, true, false},
		{"legacy admission", "", changedFieldDiff, "", 1, false, false},
		{"short write", "", changedFieldDiff, replicaFieldDiff, 1, false, true},
		{"truncated", "", changedFieldDiff, "{", 1, false, false},
		{"oversize", "", changedFieldDiff, replicaFieldDiff, 1, false, false},
		{"large preview", "", changedFieldDiff, "large", 0, true, false},
		{"large admission", "succeeded", changedFieldDiff, "large", 0, false, false},
		{"null side", "", sameFieldDiff, `{"base":17,"fields":[{"path":["env","A"],"before":null,"after":"x"}]}`, 1, false, false},
		{"duplicate member", "", sameFieldDiff, `{"base":17,"fields":[{"path":["env","A"],"after":"x","after":"y"}]}`, 1, false, false},
		{"first with rows", "", `{"new":true}`, `{"base":0,"fields":[{"path":["env","A"],"after":"x"}]}`, 1, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := &fieldOutputObserver{short: tc.short}
			var errs bytes.Buffer
			const prefix = "/v1/projects/demo/environments/dev/"
			planTag := "v1"
			rich := tc.extension
			if rich == "large" {
				value := strings.Repeat("a", 600<<10)
				rich = fmt.Sprintf(`{"base":17,"fields":[{"path":["env","LARGE"],"before":%q,"after":%q}]}`, value, strings.ReplaceAll(value, "a", "b"))
			}
			var previews, admissions atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if answerCapablePolicy(w, r) {
					return
				}
				switch r.URL.Path {
				case prefix + "deploys":
					var req deploy.Request
					if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
						t.Error(err)
						return
					}
					if req.DryRun {
						previews.Add(1)
						if r.URL.RawQuery != "diff=fields" || req.ExpectedApplied != nil || out.printed.Load() {
							t.Error("preview did not precede the field block")
						}
						fmt.Fprint(w, fieldImagePlan(planTag, tc.compact, rich))
						if tc.name == "oversize" {
							chunk := strings.Repeat(" ", 1<<20)
							for range (api.MaxDeployDiffResponse >> 20) + 1 {
								if _, err := io.WriteString(w, chunk); err != nil {
									break
								}
							}
						}
						return
					}
					admissions.Add(1)
					base := int64(17)
					if tc.name == "new" {
						base = 0
					}
					if !out.printed.Load() || r.URL.RawQuery != "" || req.ExpectedApplied == nil || *req.ExpectedApplied != base || req.Version != fieldDiffImage || req.VersionTag != planTag || req.RequestID != "field-test" {
						t.Errorf("admitted before printing or lost selection: %+v", req)
					}
					w.WriteHeader(http.StatusAccepted)
					if tc.state == "held" {
						compact := tc.compact
						if tc.name == "different held decision" {
							compact = `{"changes":[{"area":"web","change":"replicas 4 -> 3"}]}`
						}
						fmt.Fprintf(w, `{"approval_id":"ap1","expires":"2030-01-01T00:00:00Z","plan":%s}`, fieldImagePlan(planTag, compact, ""))
						return
					}
					fmt.Fprintf(w, `{"operation_id":"op","events_url":%q}`, prefix+"operations/op/events")
				case prefix + "operations/op/events":
					fmt.Fprintf(w, "id: 1\nevent: %s\ndata: {}\n\nevent: terminal\ndata: {}\n\n", tc.state)
				case prefix + "operations/op":
					fmt.Fprintf(w, `{"id":"op","state":%q,"app":"api"}`, tc.state)
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			cfg := ingressCLIConfig(t, server.URL)
			args := []string{"deploy", "api", "--diff", "--version", "v1", "--request-id", "field-test", "-e", "dev", "--config", cfg, "--manifest-dir", apiManifestDir(t)}
			if tc.dry {
				args = append(args, "--dry-run")
			}
			code := Run(args, out, &errs)
			wantAdmission := int32(0)
			if tc.state != "" {
				wantAdmission = 1
			}
			if code != tc.want || previews.Load() != 1 || admissions.Load() != wantAdmission {
				t.Fatalf("exit %d, previews %d, admissions %d, stderr %q", code, previews.Load(), admissions.Load(), errs.String())
			}
			text := out.String()
			if tc.state == "held" {
				if !strings.Contains(text, "held for approval: ap1 (expires 2030-01-01T00:00:00Z)") ||
					strings.Contains(text, "replicas 2 -> 3") ||
					(tc.name == "different held decision" && (!strings.Contains(text, "held plan:") || !strings.Contains(text, "replicas 4 -> 3"))) {
					t.Errorf("held output: %q", text)
				}
			} else if tc.state != "" && !strings.Contains(text, tc.state) {
				t.Errorf("missing outcome %q", tc.state)
			}
			switch tc.name {
			case "legacy preview":
				if !strings.Contains(text, "replicas 2 -> 3") || !strings.Contains(text+errs.String(), "field detail is unavailable") {
					t.Errorf("legacy preview: %q / %q", text, errs.String())
				}
			case "legacy admission":
				if !strings.Contains(errs.String(), "upgrade the agent or run without --diff") {
					t.Errorf("legacy execution: %q", errs.String())
				}
			case "large preview", "large admission":
				if !strings.Contains(text, "- LARGE: "+strings.Repeat("a", 600<<10)) || !strings.Contains(text, "+ LARGE: "+strings.Repeat("b", 600<<10)) {
					t.Error("large field was lost or shortened")
				}
			}
		})
	}
}
