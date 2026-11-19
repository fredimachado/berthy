package tui

import (
	"strings"
	"testing"
)

// followingARollback opens a roll back, and follows its operation by hand, so the events do not end it.
func followingARollback(t *testing.T) (*Model, *fakeAgent, *actionOverlay) {
	t.Helper()
	return following(t, actRollback)
}

// following opens an action and follows its operation by hand, as if the agent had accepted the change.
func following(t *testing.T, kind actionKind) (*Model, *fakeAgent, *actionOverlay) {
	t.Helper()
	m, agent := actionModel(t, 120, 40)
	o, _ := newActionOverlay(m, kind, fixtureApps()[0], "api", approvalRow{})
	m.overlay = o
	o.stage, o.opID, o.form, o.loading, o.release = stageFollow, "op1", nil, false, 2
	return m, agent, o
}

func cancelRequests(a *fakeAgent) []request {
	var out []request
	for _, r := range a.requests() {
		if r.path == "operations/op1/cancel" {
			out = append(out, r)
		}
	}
	return out
}

func TestTheRunningOperationOffersToCancelAndEscOnlyClosesTheView(t *testing.T) {
	m, agent, _ := followingARollback(t)
	out := plain(m)
	if !strings.Contains(out, "c cancel the operation") || !strings.Contains(out, "esc close, the operation goes on") {
		t.Fatalf("the keys are not offered:\n%s", out)
	}
	_, cmd := m.Update(keypress("esc"))
	settle(t, m, func() bool { return m.overlay == nil }, cmd)
	if len(cancelRequests(agent)) != 0 {
		t.Fatal("esc cancelled the operation")
	}
}

func TestCancelAsksForConfirmationAndSendsNothingUntilItIsGiven(t *testing.T) {
	m, agent, o := followingARollback(t)
	step(t, m, "c")
	out := plain(m)
	if o.form == nil || !strings.Contains(out, "Cancel operation op1?") || !strings.Contains(out, "Cancel it") || !strings.Contains(out, "Keep going") {
		t.Fatalf("no confirmation:\n%s", out)
	}
	if len(cancelRequests(agent)) != 0 {
		t.Fatal("a request was sent before the person confirmed")
	}
	// Saying no, or esc, goes back to following the operation and sends nothing.
	step(t, m, "esc")
	if o.form != nil || o.stage != stageFollow || len(cancelRequests(agent)) != 0 {
		t.Fatalf("esc did not go back: form %v stage %v", o.form, o.stage)
	}
	step(t, m, "c")
	press(t, m, func() bool { return o.form == nil }, "n")
	if len(cancelRequests(agent)) != 0 || o.stage != stageFollow {
		t.Fatal("a cancel was sent")
	}
}

func TestConfirmedCancelIsSentAndTheOperationEndsAsCancelled(t *testing.T) {
	m, agent, o := followingARollback(t)
	step(t, m, "c")
	press(t, m, func() bool { return o.cancelSent }, "y")
	reqs := cancelRequests(agent)
	if len(reqs) != 1 || reqs[0].method != "POST" || reqs[0].body["dry_run"] != false {
		t.Fatalf("requests %+v", reqs)
	}
	out := plain(m)
	if !strings.Contains(out, "Cancel accepted") || strings.Contains(out, "c cancel the operation") {
		t.Fatalf("after the cancel:\n%s", out)
	}
	// The operation then ends as the agent says.
	_, cmd := m.Update(opDoneMsg{id: o.id, state: "rolled_back", reason: "cancelled by tui"})
	settle(t, m, stageIs(m, stageDone), cmd)
	out = plain(m)
	for _, want := range []string{"Roll back of api was cancelled", "rolled back", "cancelled by tui"} {
		if !strings.Contains(out, want) {
			t.Errorf("result lacks %q:\n%s", want, out)
		}
	}
	if o.result.kind != outFailed {
		t.Errorf("result %+v", o.result)
	}
}

func TestRefusedCancelSaysWhyAndTheOperationGoesOn(t *testing.T) {
	m, agent, o := followingARollback(t)
	agent.cancelRefuse = "the deploy already switched traffic"
	step(t, m, "c")
	press(t, m, func() bool { return o.cancelWarn }, "y")
	out := plain(m)
	if !strings.Contains(out, "the deploy already switched traffic") || o.stage != stageFollow {
		t.Fatalf("refusal not shown:\n%s", out)
	}
	// The key stays: the operation goes on, and the person is not locked out.
	if !strings.Contains(out, "c cancel the operation") {
		t.Fatalf("the key is gone after a refusal:\n%s", out)
	}
}

func TestCancelOfAnAgentTokenOnAProtectedEnvironmentIsHeld(t *testing.T) {
	m, agent, o := followingARollback(t)
	agent.hold = true
	step(t, m, "c")
	press(t, m, func() bool { return o.cancelSent }, "y")
	out := plain(m)
	if !strings.Contains(out, "Held for approval held0002") || o.stage != stageFollow {
		t.Fatalf("held cancel not shown:\n%s", out)
	}
}

func TestOnlyTheOperationsThatCanBeCancelledOfferIt(t *testing.T) {
	m, _, o := following(t, actRestart)
	out := plain(m)
	if strings.Contains(out, "cancel the operation") {
		t.Fatalf("a restart offers a cancel:\n%s", out)
	}
	step(t, m, "c")
	if o.form != nil {
		t.Fatal("a restart opened the confirmation")
	}
}
