package tui

import (
	"strings"
	"testing"
)

// diffPlan is the plan of a deploy that an agent asked for: it changes the image, a replica count and a variable, and one of
// the values holds a terminal escape.
const diffPlan = `{"action":"deploy","app":"api","image":"127.0.0.1:15001/app@sha256:abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789","roles":["web"],"diff":{"changes":[` +
	`{"area":"image","change":"sha256:41ab1c2d3e4f… -> sha256:9f2c0d1e2f3a…   (v1.4.2 -> v1.4.3)"},` +
	`{"area":"web","change":"replicas 2 -> 3"},` +
	`{"area":"env","change":"+ FEATURE_X (plain)"},` +
	`{"area":"env","change":"- LEGACY_MODE (plain)"},` +
	`{"area":"env","change":"~ NOTE (plain) \"a\" -> \"\u001b[31mred\""}]}}`

func TestSplitDiffTakesTheDiffOutOfThePlan(t *testing.T) {
	lines, rest := splitDiff([]byte(diffPlan))
	if len(lines) != 5 || lines[1] != "web      replicas 2 -> 3" {
		t.Fatalf("lines %q", lines)
	}
	for _, l := range lines {
		if strings.ContainsRune(l, 0x1b) {
			t.Fatalf("a control character reached the screen: %q", l)
		}
	}
	if strings.Contains(string(rest), `"diff"`) || !strings.Contains(string(rest), `"app":"api"`) {
		t.Fatalf("rest %s", rest)
	}
	// A plan without a diff is returned as it is.
	plan := []byte(`{"app":"api"}`)
	if lines, rest := splitDiff(plan); lines != nil || string(rest) != string(plan) {
		t.Fatalf("lines %q rest %s", lines, rest)
	}
	if lines, _ := splitDiff([]byte(`{"diff":{"new":true}}`)); len(lines) != 1 || lines[0] != "new app" {
		t.Fatalf("lines %q", lines)
	}
	if lines, rest := splitDiff([]byte(`not json`)); lines != nil || string(rest) != "not json" {
		t.Fatalf("lines %q rest %s", lines, rest)
	}
}

func TestApprovalDetailShowsWhatChangesNotRawJSON(t *testing.T) {
	for _, s := range sizes {
		m := loadedModel(t, s.w, s.h)
		keys(m, "4")
		rows := fixtureApprovals()
		rows[0].Plan = []byte(diffPlan)
		send(m, approvalsMsg{rows: rows})
		keys(m, "enter")
		if m.approvals.detail == nil {
			t.Fatal("detail not opened")
		}
		screen := plain(m)
		for _, want := range []string{"What changes", "replicas 2 -> 3", "+ FEATURE_X (plain)", "- LEGACY_MODE (plain)", "(v1.4.2 -> v1.4.3)"} {
			if !strings.Contains(screen, want) {
				t.Errorf("%s: the detail lacks %q:\n%s", s.name, want, screen)
			}
		}
		for _, bad := range []string{`"area"`, `"change"`, `"diff"`, "\x1b"} {
			if strings.Contains(screen, bad) {
				t.Errorf("%s: the detail shows %q:\n%s", s.name, bad, screen)
			}
		}
		golden(t, "approval-diff-"+s.name, screen)
	}
}

func TestRollbackConfirmationShowsWhatChanges(t *testing.T) {
	m, agent := actionModel(t, 120, 40)
	agent.plan = diffPlan
	step(t, m, "enter")
	press(t, m, pickReady(m), "b")
	press(t, m, stageIs(m, stageConfirm), "enter")
	screen := plain(m)
	for _, want := range []string{"What changes", "replicas 2 -> 3", "+ FEATURE_X (plain)"} {
		if !strings.Contains(screen, want) {
			t.Errorf("the confirmation lacks %q:\n%s", want, screen)
		}
	}
	if strings.Contains(screen, `"area"`) || strings.Contains(screen, `"diff"`) {
		t.Errorf("the confirmation shows the diff as JSON:\n%s", screen)
	}
	golden(t, "action-rollback-diff-120x40", screen)
}
