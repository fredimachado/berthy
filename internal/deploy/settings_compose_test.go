package deploy

import (
	"context"
	"testing"

	"berthy/internal/manifest"
)

func TestAComposeDeployWritesTheSettingsAndItsRollbackDoesNot(t *testing.T) {
	runner := &fakeCompose{config: configOne, hashes: hashOne}
	c, s := composeController(t, runner, newComposeDocker())
	ctx := context.Background()
	admin := Actor{ID: "admin", Admin: true}
	for _, tc := range []struct{ id, file, chat string }{{"first", composeFileOne, "1"}, {"second", composeFileTwo, "2"}} {
		m := composeApp(false)
		m.Alerts = &manifest.Alerts{Telegram: &manifest.Telegram{BotTokenSecret: "TG", ChatID: tc.chat}}
		op, _, err := admit(t, c, ctx, admin, composeReq(tc.id, tc.file, m))
		if err != nil {
			t.Fatal(err)
		}
		if got := waitComposeOperation(t, s, op.ID); got.State != "succeeded" {
			t.Fatalf("%s: %s %s", tc.id, got.State, got.Err)
		}
		if got := settingsChat(t, s); got != tc.chat {
			t.Fatalf("chat after %s: %s", tc.id, got)
		}
	}
	op, _, err := admit(t, c, ctx, admin, Request{RequestID: "back", App: "db", Kind: "rollback", Manifest: composeApp(false)})
	if err != nil {
		t.Fatal(err)
	}
	if got := waitComposeOperation(t, s, op.ID); got.State != "succeeded" {
		t.Fatalf("rollback %s: %s", got.State, got.Err)
	}
	if got := settingsChat(t, s); got != "2" {
		t.Fatalf("a compose rollback changed the alert chat to %s", got)
	}
}
