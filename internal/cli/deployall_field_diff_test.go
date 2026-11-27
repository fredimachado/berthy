package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const fieldProjectManifest = `project: demo
network: demo
apps:
  zstack:
    kind: compose
    file: compose.yml
    depends_on: [redis]
  redis:
    kind: image
    image: redis
    depends_on: [api]
    roles:
      cache: {}
  api:
    kind: image
    image: ghcr.io/acme/api
    roles:
      web: {}
`

const fieldProjectCompose = "services:\n  database:\n    image: alpine\n"

func fieldProjectFixture(t *testing.T, protected bool) (*fakeDeployAgent, []string) {
	t.Helper()
	digest := strings.Repeat("a", 64)
	compact := map[string]string{
		"api":    fmt.Sprintf(`{"app":"api","image":"ghcr.io/acme/api@sha256:%s","tag":"v1","roles":["web"],"diff":%s}`, digest, changedFieldDiff),
		"redis":  fmt.Sprintf(`{"app":"redis","image":"docker.io/library/redis@sha256:%s","tag":"v1.0.2","roles":["cache"],"diff":{"changes":[{"area":"cache","change":"replicas 1 -> 4"}]}}`, digest),
		"zstack": `{"app":"zstack","image":"","roles":[],"compose":{"project":"demo-dev-zstack","services":[{"name":"database","action":"recreate"}],"volumes":[]},"diff":{"changes":[{"area":"compose","change":"recreate database"}]}}`,
	}
	extensions := map[string]string{
		"api":    replicaFieldDiff,
		"redis":  `{"base":4,"fields":[{"path":["roles","cache","replicas"],"before":"1","after":"4"}]}`,
		"zstack": `{"base":9,"fields":[{"path":["compose","database"],"note":"recreate database"}]}`,
	}
	agent := &fakeDeployAgent{protected: protected, mixedPlans: true, plans: compact, fieldPlans: map[string]string{}, guardIDs: map[string]int64{}}
	for app, plan := range compact {
		agent.fieldPlans[app] = strings.TrimSuffix(plan, "}") + `,"release_diff":` + extensions[app] + "}"
	}
	dir, cfg := deployAllFixture(t, fieldProjectManifest, agent)
	if err := os.WriteFile(filepath.Join(dir, "compose.yml"), []byte(fieldProjectCompose), 0600); err != nil {
		t.Fatal(err)
	}
	return agent, []string{"deploy", "-e", "dev", "--config", cfg, "--manifest-dir", dir, "--version", "api=v1"}
}

func assertFieldProjectCalls(t *testing.T, agent *fakeDeployAgent, want ...string) {
	t.Helper()
	var calls []string
	bases := map[string]float64{"api": 17, "redis": 4, "zstack": 9}
	for i, req := range agent.deploys {
		app := req["app"].(string)
		if req["dry_run"] == true {
			calls = append(calls, "preview "+app+"?"+agent.deployQueries[i])
			continue
		}
		calls = append(calls, "admit "+app)
		if req["expected_applied"] != bases[app] || agent.deployQueries[i] != "" {
			t.Errorf("admission base/query for %s: %v / %q", app, req["expected_applied"], agent.deployQueries[i])
		}
		switch app {
		case "api":
			if req["version"] != "ghcr.io/acme/api@sha256:"+strings.Repeat("a", 64) || req["version_tag"] != "v1" {
				t.Errorf("api selection: %v / %v", req["version"], req["version_tag"])
			}
		case "redis":
			if !agent.protected && (req["keep_version"] != true || req["keep_digest"] != "docker.io/library/redis@sha256:"+strings.Repeat("a", 64)) {
				t.Errorf("redis kept selection: %v / %v", req["keep_version"], req["keep_digest"])
			}
		case "zstack":
			if req["compose_file"] != fieldProjectCompose {
				t.Errorf("Compose bytes: %q", req["compose_file"])
			}
		}
	}
	if !slices.Equal(calls, want) {
		t.Errorf("deploy calls %v, want %v", calls, want)
	}
}

func TestProjectDeployDiffRunsInDependencyOrder(t *testing.T) {
	for _, mode := range []string{"preview", "preview refused", "success", "rollback", "hold", "kept base changed"} {
		t.Run(mode, func(t *testing.T) {
			agent, args := fieldProjectFixture(t, false)
			args = append(args, "--diff")
			wantExit := 0
			want := []string{"preview api?diff=fields", "preview redis?diff=fields", "preview zstack?diff=fields"}
			if mode == "preview" || mode == "preview refused" {
				args = append(args, "--dry-run")
			} else {
				want = []string{"preview api?diff=fields", "admit api", "preview redis?diff=fields", "admit redis", "preview zstack?diff=fields", "admit zstack"}
			}
			switch mode {
			case "preview refused":
				wantExit = 1
				agent.mixedPlans = false
				delete(agent.fieldPlans, "redis")
				delete(agent.plans, "redis")
				agent.failPlan = map[string]bool{"redis": true}
			case "rollback":
				wantExit, want, agent.rollBack = 1, want[:4], "redis"
			case "hold":
				wantExit, want, agent.held = 3, want[:4], "redis"
			case "kept base changed":
				wantExit, want = 1, want[:3]
				agent.fieldPlans["redis"] = strings.Replace(agent.fieldPlans["redis"], `"base":4`, `"base":5`, 1)
			}
			var output bytes.Buffer
			if exit := Run(args, &output, &output); exit != wantExit {
				t.Errorf("exit %d, want %d: %s", exit, wantExit, &output)
			}
			assertFieldProjectCalls(t, agent, want...)
			text := output.String()
			api, redis, compose := strings.Index(text, "Deploy demo/dev app api"), strings.Index(text, "Deploy demo/dev app redis"), strings.Index(text, "Deploy demo/dev app zstack")
			if (mode == "success" || mode == "preview") && !(api >= 0 && api < redis && redis < compose && strings.Contains(text, "    recreate database")) {
				t.Errorf("field blocks are missing or out of order: %q", text)
			}
			if mode == "preview refused" && (redis >= 0 || compose < 0 || !strings.Contains(text, "plan refused for redis")) {
				t.Errorf("preview did not continue after a refusal: %q", text)
			}
			if mode == "rollback" || mode == "hold" || mode == "kept base changed" {
				stop := strings.Index(text, "deploy stopped at redis; not run: zstack")
				needle := map[string]string{"rollback": "rolled_back", "hold": "held for approval", "kept base changed": "applied release changed"}[mode]
				outcome := strings.Index(text, needle)
				if api < 0 || (mode != "kept base changed" && redis < api) || (mode == "kept base changed" && redis >= 0) || outcome < 0 || stop < outcome || compose >= 0 {
					t.Errorf("batch stop: %q", text)
				}
			}
		})
	}
}

func TestProjectDeployDiffKeepsJSONContract(t *testing.T) {
	for _, mode := range []string{"dry run", "success", "stopped", "held"} {
		t.Run(mode, func(t *testing.T) {
			var control []byte
			for _, flag := range []bool{false, true} {
				agent, args := fieldProjectFixture(t, false)
				args = append(args, "--json")
				want := 0
				switch mode {
				case "dry run":
					args = append(args, "--dry-run")
				case "stopped":
					agent.rollBack, want = "redis", 1
				case "held":
					agent.held, want = "redis", 3
				}
				if flag {
					args = append(args, "--diff")
				}
				var out, errs bytes.Buffer
				if code := Run(args, &out, &errs); code != want {
					t.Fatalf("exit %d, want %d: %s", code, want, &errs)
				}
				raw, err := capturedDocument(out.Bytes())
				var doc struct{ Apps []deployAllEntry }
				if err != nil || json.Unmarshal(raw, &doc) != nil || len(doc.Apps) != 3 {
					t.Fatalf("expected one batch JSON document: %q, %v", &out, err)
				}
				for _, query := range agent.deployQueries {
					if query != "" {
						t.Errorf("JSON used a field preview: %q", query)
					}
				}
				for i, entry := range doc.Apps {
					if entry.App != []string{"api", "redis", "zstack"}[i] {
						t.Errorf("unexpected app order: %+v", entry)
					}
					switch {
					case want != 0 && i == 2:
						if entry.State != "not_run" || entry.Exit != nil || entry.Result != nil {
							t.Errorf("skipped entry: %+v", entry)
						}
					case want != 0 && i == 1:
						if entry.Exit == nil || *entry.Exit != want || len(entry.Result) == 0 {
							t.Errorf("stopped entry: %+v", entry)
						}
					default:
						if entry.Exit == nil || *entry.Exit != 0 || len(entry.Result) == 0 {
							t.Errorf("successful entry: %+v", entry)
						}
					}
				}
				if flag && !bytes.Equal(control, raw) {
					t.Errorf("--diff changed JSON: %s\ncontrol: %s", raw, control)
				}
				control = raw
			}
		})
	}
}
