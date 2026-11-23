package state

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"berthy/internal/manifest"
)

func testSettings(t *testing.T, project, chat string) *Settings {
	t.Helper()
	m := manifest.Manifest{Project: project, Network: "net", Apps: map[string]manifest.App{"web": {}, "api": {}}, Alerts: &manifest.Alerts{Telegram: &manifest.Telegram{BotTokenSecret: "TG", ChatID: chat}}}
	st, err := NewSettings(m, "abc123", false)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// deployWith records a successful deploy of app that also applies settings, as a deploy of the agent does.
func deployWith(t *testing.T, s *Store, id, app string, st *Settings) {
	t.Helper()
	ctx := context.Background()
	op, _, err := s.Admit(ctx, Admission{RequestID: id, ActorID: "a", Kind: "deploy", App: app, PublicRequest: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Finish(ctx, op.ID, Outcome{State: "succeeded", Release: &Release{App: app, Manifest: []byte(`{"project":"x"}`)}, Settings: st}); err != nil {
		t.Fatal(err)
	}
}

func TestSettingsAreEmptyUntilSomethingWritesThem(t *testing.T) {
	s, _, _ := testStore(t)
	if _, ok, err := s.Settings(context.Background()); ok || err != nil {
		t.Fatalf("settings of a fresh store: %v %v", ok, err)
	}
}

func TestAReleaseWritesTheSettingsInTheSameTransaction(t *testing.T) {
	s, _, _ := testStore(t)
	ctx := context.Background()
	deployWith(t, s, "one", "api", testSettings(t, "demo", "1"))
	got, ok, err := s.Settings(ctx)
	if err != nil || !ok || got.Project != "demo" || got.Commit != "abc123" || strings.Join(got.Apps, ",") != "api,web" || got.OperationID == "" || got.Updated.IsZero() {
		t.Fatalf("settings after a deploy: %+v %v %v", got, ok, err)
	}
	m, err := manifest.DecodeSettings(got.Manifest)
	if err != nil || m.Alerts.Telegram.ChatID != "1" {
		t.Fatalf("stored manifest: %+v %v", m, err)
	}
	// A release without settings, as a rollback records, leaves them as they are.
	deployWith(t, s, "two", "api", nil)
	if again, _, _ := s.Settings(ctx); again.OperationID != got.OperationID {
		t.Fatalf("a release without settings changed them: %+v", again)
	}
	// A later deploy replaces them.
	deployWith(t, s, "three", "web", testSettings(t, "demo", "2"))
	again, _, _ := s.Settings(ctx)
	m, _ = manifest.DecodeSettings(again.Manifest)
	if m.Alerts.Telegram.ChatID != "2" || again.OperationID == got.OperationID {
		t.Fatalf("settings not replaced: %+v", again)
	}
}

func TestSettingsWithoutAReleaseAreRefused(t *testing.T) {
	s, _, _ := testStore(t)
	ctx := context.Background()
	op, _, err := s.Admit(ctx, Admission{RequestID: "x", ActorID: "a", Kind: "deploy", App: "api", PublicRequest: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Finish(ctx, op.ID, Outcome{State: "failed", Settings: testSettings(t, "demo", "1")}); err == nil {
		t.Fatal("settings recorded for an operation with no release")
	}
}

func TestApplySettingsWritesAuditsAndNeedsNoLock(t *testing.T) {
	s, _, _ := testStore(t)
	ctx := context.Background()
	actor := Actor{ID: "ci", Name: "ci"}
	if err := s.ApplySettings(ctx, actor, *testSettings(t, "demo", "1"), false); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.Settings(ctx)
	if err != nil || !ok || got.Project != "demo" || got.OperationID != "" {
		t.Fatalf("settings after apply: %+v %v %v", got, ok, err)
	}
	log, err := s.AuditLog(ctx, 10)
	if err != nil || len(log) != 1 || log[0].Action != "apply" || log[0].Outcome != "succeeded" || log[0].ActorID != "ci" {
		t.Fatalf("audit: %+v %v", log, err)
	}
	if err = s.ApplySettings(ctx, Actor{}, *testSettings(t, "demo", "1"), false); err == nil {
		t.Fatal("apply without an actor")
	}
}

func TestApplySettingsIsRefusedWhileTheLockIsHeld(t *testing.T) {
	s, _, _ := testStore(t)
	ctx := context.Background()
	actor := Actor{ID: "ci", Name: "ci"}
	if err := s.AcquireManual(ctx, actor, "ci", "maintenance"); err != nil {
		t.Fatal(err)
	}
	if err := s.ApplySettings(ctx, actor, *testSettings(t, "demo", "1"), false); !errors.Is(err, ErrConflict) {
		t.Fatalf("apply under a manual lock: %v", err)
	}
	if _, ok, _ := s.Settings(ctx); ok {
		t.Fatal("a refused apply wrote settings")
	}
	if log, _ := s.AuditLog(ctx, 10); len(log) != 1 {
		t.Fatalf("a refused apply was audited: %+v", log)
	}
}

func TestApplySettingsOfProtectedMakesProtectionSticky(t *testing.T) {
	s, _, _ := testStore(t)
	ctx := context.Background()
	if err := s.ApplySettings(ctx, Actor{ID: "ci"}, *testSettings(t, "demo", "1"), true); err != nil {
		t.Fatal(err)
	}
	if on, err := s.Protected(ctx); err != nil || !on {
		t.Fatalf("protected: %v %v", on, err)
	}
}

func TestLatestReleasesHoldsTheNewestReleaseOfEveryAppEverDeployed(t *testing.T) {
	s, _, _ := testStore(t)
	ctx := context.Background()
	if got, err := s.LatestReleases(ctx); err != nil || len(got) != 0 {
		t.Fatalf("fresh store: %v %v", got, err)
	}
	release := func(id, app, tag string) {
		t.Helper()
		op, _, err := s.Admit(ctx, Admission{RequestID: id, ActorID: "a", Kind: "deploy", App: app, PublicRequest: []byte(`{}`)})
		if err != nil {
			t.Fatal(err)
		}
		if err = s.Finish(ctx, op.ID, Outcome{State: "succeeded", Release: &Release{App: app, Tag: tag, Manifest: []byte(`{}`)}}); err != nil {
			t.Fatal(err)
		}
	}
	release("a1", "api", "v1")
	release("w1", "web", "v1")
	release("a2", "api", "v2")
	op, _, err := s.Admit(ctx, Admission{RequestID: "rm", ActorID: "a", Kind: "remove", PublicRequest: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ForgetApp(ctx, op.ID, "web"); err != nil {
		t.Fatal(err)
	}
	if err = s.Finish(ctx, op.ID, Outcome{State: "succeeded"}); err != nil {
		t.Fatal(err)
	}
	got, err := s.LatestReleases(ctx)
	if err != nil || len(got) != 2 || got["api"].Tag != "v2" || got["web"].Tag != "v1" || string(got["api"].Manifest) != `{}` {
		t.Fatalf("latest releases: %+v %v", got, err)
	}
	if _, err = s.Applied(ctx, "web"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("web still applied: %v", err)
	}
}

func TestSettingsMustBeJSON(t *testing.T) {
	s, _, _ := testStore(t)
	st := testSettings(t, "demo", "1")
	st.Manifest = []byte("not json")
	if err := s.ApplySettings(context.Background(), Actor{ID: "ci"}, *st, false); err == nil || !strings.Contains(err.Error(), "settings") {
		t.Fatalf("invalid settings accepted: %v", err)
	}
}

func TestReleasesReturnsEveryRelease(t *testing.T) {
	s, _, _ := testStore(t)
	ctx := context.Background()
	for _, tag := range []string{"v1", "v2"} {
		op, _, err := s.Admit(ctx, Admission{RequestID: tag, ActorID: "a", Kind: "deploy", App: "api", PublicRequest: []byte(`{}`)})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Finish(ctx, op.ID, Outcome{State: "succeeded", Release: &Release{App: "api", Tag: tag, Manifest: []byte(`{}`)}}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.Releases(ctx)
	if err != nil || len(got) != 2 || got[0].Tag != "v1" || got[1].Tag != "v2" {
		t.Fatalf("releases: %+v %v", got, err)
	}
}

func TestDeployAttempted(t *testing.T) {
	s, _, _ := testStore(t)
	ctx := context.Background()
	for _, tc := range []struct{ app, status string }{{"ok", "succeeded"}, {"failed", "failed"}} {
		op, _, err := s.Admit(ctx, Admission{RequestID: tc.app, ActorID: "a", Kind: "deploy", App: tc.app, PublicRequest: []byte(`{}`)})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Finish(ctx, op.ID, Outcome{State: tc.status}); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		app  string
		want bool
	}{{"ok", true}, {"failed", true}, {"other", false}} {
		got, err := s.DeployAttempted(ctx, tc.app)
		if err != nil || got != tc.want {
			t.Fatalf("%s: %v %v", tc.app, got, err)
		}
	}
}
