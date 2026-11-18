package mcp

import (
	"strings"
	"testing"
)

const diffPlanJSON = `{"app":"api","diff":{"changes":[{"area":"image","change":"sha256:aaaaaaaaaaaa… -> sha256:bbbbbbbbbbbb…"},{"area":"web","change":"replicas 2 -> 3"},{"area":"env","change":"+ FEATURE_X (plain)"}]}}`

const diffLines = "changes of api:\n  image    sha256:aaaaaaaaaaaa… -> sha256:bbbbbbbbbbbb…\n  web      replicas 2 -> 3\n  env      + FEATURE_X (plain)"

func TestDryRunTextAndStructuredResultCarryTheDiff(t *testing.T) {
	agent, u := newFakeAgent(t)
	agent.on("POST", "deploys", 200, diffPlanJSON)
	cs := connect(t, testConfig(u, manifestDir(t)))
	res := okResult(t, call(t, cs, "execute_resource_command", map[string]any{"resource_name": "api", "command": "deploy", "version": "1.2.3", "dry_run": true}))
	plan, _ := structured(t, res)["plan"].(map[string]any)
	if _, ok := plan["diff"].(map[string]any); !ok {
		t.Fatalf("the structured plan lacks the diff: %v", plan)
	}
	if !strings.Contains(text(t, res), diffLines) {
		t.Fatalf("the text lacks the diff lines:\n%s", text(t, res))
	}
}

func TestHeldResultCarriesTheDiff(t *testing.T) {
	agent, u := newFakeAgent(t)
	agent.onFunc("POST", "deploys", func(request) reply {
		return reply{Status: 202, Header: map[string]string{"Berthy-Approval": "pending"},
			Body: `{"approval_id":"apr-123","expires":"2026-11-16T12:00:00Z","plan":` + diffPlanJSON + `}`}
	})
	cs := connect(t, testConfig(u, manifestDir(t)))
	res := okResult(t, call(t, cs, "execute_resource_command", map[string]any{"resource_name": "api", "command": "deploy", "version": "1.2.3"}))
	plan, _ := structured(t, res)["plan"].(map[string]any)
	if _, ok := plan["diff"].(map[string]any); !ok {
		t.Fatalf("the structured plan lacks the diff: %v", plan)
	}
	if !strings.Contains(text(t, res), diffLines) || !strings.Contains(text(t, res), "held for approval apr-123") {
		t.Fatalf("the text lacks the diff lines:\n%s", text(t, res))
	}
}

func TestADiffLineNeverCarriesTheAgentsTokenOrAControlCharacter(t *testing.T) {
	agent, u := newFakeAgent(t)
	agent.on("POST", "deploys", 200, `{"app":"api","diff":{"changes":[{"area":"env","change":"~ NOTE (plain) \"a\" -> \"`+testToken+`\u001b[31m\""}]}}`)
	cs := connect(t, testConfig(u, manifestDir(t)))
	res := okResult(t, call(t, cs, "execute_resource_command", map[string]any{"resource_name": "api", "command": "deploy", "version": "1.2.3", "dry_run": true}))
	if strings.Contains(text(t, res), testToken) || strings.ContainsRune(text(t, res), 0x1b) {
		t.Fatalf("text %q", text(t, res))
	}
}
