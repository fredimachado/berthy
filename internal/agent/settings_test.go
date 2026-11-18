package agent

import (
	"context"
	"fmt"
	"testing"

	"berthy/internal/state"
)

// twoAppManifest is a valid manifest of two apps. tick belongs to api and sweep to worker, and chat is the alert chat.
func twoAppManifest(tick, sweep, chat string) string {
	return fmt.Sprintf(`{"project":"demo","network":"demo",
	"apps":{"api":{"kind":"image","image":"ghcr.io/acme/api","roles":{"web":{}}},"worker":{"kind":"image","image":"ghcr.io/acme/worker","roles":{"main":{}}}},
	"jobs":{"tick":{"app":"api","command":["true"],"schedule":%q},"sweep":{"app":"worker","command":["true"],"schedule":%q}},
	"alerts":{"telegram":{"bot_token_secret":"TELEGRAM_BOT_TOKEN","chat_id":%q}}}`, tick, sweep, chat)
}

// recordRollback records the release a rollback of app makes: an older manifest becomes the newest release, and the
// environment's settings are not touched.
func recordRollback(t *testing.T, s *state.Store, id, app, manifestJSON string) {
	t.Helper()
	ctx := context.Background()
	op, _, err := s.Admit(ctx, state.Admission{RequestID: id, ActorID: "ci", Kind: "rollback", App: app, PublicRequest: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Finish(ctx, op.ID, state.Outcome{State: "succeeded", Release: &state.Release{App: app, Manifest: []byte(manifestJSON)}}); err != nil {
		t.Fatal(err)
	}
}

func TestRollbackKeepsOtherAppsAndEnvSettings(t *testing.T) {
	s := openIngressStore(t)
	ctx := context.Background()
	recordRelease(t, s, "first", twoAppManifest("* * * * *", "0 * * * *", "1"))
	// worker is deployed again with a new schedule for its job and a new alert chat. It also changes the job of api, which api
	// applies with its own next deploy.
	recordAppRelease(t, s, "second", "worker", twoAppManifest("7 * * * *", "5 * * * *", "2"))
	recordRollback(t, s, "back", "api", twoAppManifest("* * * * *", "0 * * * *", "1"))

	m, err := runningManifest(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	if got := m.Jobs["sweep"].Schedule; got != "5 * * * *" {
		t.Errorf("worker's job after api was rolled back: %q", got)
	}
	if got := m.Jobs["tick"].Schedule; got != "* * * * *" {
		t.Errorf("api's job after its rollback: %q", got)
	}
	if got := m.Alerts.Telegram.ChatID; got != "2" {
		t.Errorf("alert chat after api was rolled back: %q", got)
	}
	if len(m.Apps) != 2 {
		t.Errorf("apps: %v", m.Apps)
	}
}

func TestRunningManifestHasNoAppThatIsNotDeployed(t *testing.T) {
	s := openIngressStore(t)
	ctx := context.Background()
	if _, err := runningManifest(ctx, s); err == nil {
		t.Fatal("a manifest before anything was applied")
	}
	recordRelease(t, s, "both", twoAppManifest("* * * * *", "0 * * * *", "1"))
	forgetApp(t, s, "worker")
	m, err := runningManifest(ctx, s)
	if err != nil || len(m.Apps) != 1 || len(m.Jobs) != 1 || m.Jobs["tick"].App != "api" {
		t.Fatalf("running manifest after worker was removed: %+v %v", m, err)
	}
	recorded, err := recordedManifest(ctx, s)
	if err != nil || len(recorded.Apps) != 2 || len(recorded.Jobs) != 2 {
		t.Fatalf("recorded manifest keeps the removed app's part: %+v %v", recorded, err)
	}
}
