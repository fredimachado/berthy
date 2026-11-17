package state

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestChecksRoundTripDeleteAndValidation(t *testing.T) {
	s, _, _ := testStore(t)
	ctx := context.Background()
	if checks, err := s.Checks(ctx); err != nil || len(checks) != 0 {
		t.Fatalf("empty store: %v %v", checks, err)
	}
	since := time.UnixMilli(1_700_000_000_123).UTC()
	notified := since.Add(time.Minute)
	if err := s.PutCheck(ctx, CheckState{Name: "probe:site", State: "critical", Detail: "HTTP 503", Since: since, NotifiedState: "critical", Notified: notified}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutCheck(ctx, CheckState{Name: "disk:/", State: "ok", Detail: "42.0% used", Since: since}); err != nil {
		t.Fatal(err)
	}
	checks, err := s.Checks(ctx)
	if err != nil || len(checks) != 2 || checks[0].Name != "disk:/" || checks[1].Name != "probe:site" {
		t.Fatalf("checks not sorted by name: %+v %v", checks, err)
	}
	site := checks[1]
	if site.State != "critical" || site.Detail != "HTTP 503" || !site.Since.Equal(since) || site.NotifiedState != "critical" || !site.Notified.Equal(notified) {
		t.Fatalf("stored state changed: %+v", site)
	}
	if disk := checks[0]; disk.NotifiedState != "" || !disk.Notified.IsZero() {
		t.Fatalf("unset alert fields not zero: %+v", disk)
	}
	if err = s.PutCheck(ctx, CheckState{Name: "probe:site", State: "ok", Detail: "HTTP 200", Since: notified}); err != nil {
		t.Fatal(err)
	}
	checks, err = s.Checks(ctx)
	if err != nil || len(checks) != 2 || checks[1].State != "ok" || checks[1].NotifiedState != "" || !checks[1].Notified.IsZero() {
		t.Fatalf("upsert kept old values: %+v %v", checks, err)
	}
	if err = s.DeleteCheck(ctx, "disk:/"); err != nil {
		t.Fatal(err)
	}
	if err = s.DeleteCheck(ctx, "disk:/"); err != nil {
		t.Fatalf("deleting a missing check failed: %v", err)
	}
	if checks, _ = s.Checks(ctx); len(checks) != 1 || checks[0].Name != "probe:site" {
		t.Fatalf("delete did not remove the check: %+v", checks)
	}
	for name, bad := range map[string]CheckState{
		"empty name":      {Name: "", State: "ok"},
		"unknown state":   {Name: "x", State: "fine"},
		"unknown notify":  {Name: "x", State: "ok", NotifiedState: "bogus"},
		"oversized value": {Name: "x", State: "ok", Detail: strings.Repeat("d", maxCheckDetail+1)},
	} {
		if err = s.PutCheck(ctx, bad); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestChecksSurviveReopen(t *testing.T) {
	s, dir, key := testStore(t)
	ctx := context.Background()
	if err := s.PutCheck(ctx, CheckState{Name: "memory", State: "warning", Detail: "8.0% available", Since: time.UnixMilli(5000).UTC(), NotifiedState: "warning", Notified: time.UnixMilli(6000).UTC()}); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err := Open(dir, key, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	checks, err := s.Checks(ctx)
	if err != nil || len(checks) != 1 || checks[0].State != "warning" || checks[0].NotifiedState != "warning" || checks[0].Notified.UnixMilli() != 6000 {
		t.Fatalf("state lost on reopen: %+v %v", checks, err)
	}
}

func TestUpgradeVersionFourAddsChecksTable(t *testing.T) {
	s, dir, key := testStore(t)
	if _, err := s.db.Exec(`DROP TABLE checks; PRAGMA user_version=4`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	upgraded, err := Open(dir, key, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()
	var version int
	if err = upgraded.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 13 {
		t.Fatalf("migration: %d %v", version, err)
	}
	if err = upgraded.PutCheck(context.Background(), CheckState{Name: "memory", State: "ok"}); err != nil {
		t.Fatalf("checks table unusable after upgrade: %v", err)
	}
}

func TestLatestManifestPicksNewestNonConnectorRelease(t *testing.T) {
	s, _, _ := testStore(t)
	ctx := context.Background()
	if _, _, err := s.LatestManifest(ctx); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("manifest before any deploy: %v", err)
	}
	release := func(id, app, manifest string) {
		t.Helper()
		op, _, err := s.Admit(ctx, Admission{RequestID: id, ActorID: "a", Kind: "deploy", App: app, PublicRequest: []byte(`{}`)})
		if err != nil {
			t.Fatal(err)
		}
		if err = s.Finish(ctx, op.ID, Outcome{State: "succeeded", Release: &Release{App: app, Manifest: []byte(manifest)}}); err != nil {
			t.Fatal(err)
		}
	}
	first := `{"project":"demo","network":"demo","apps":{"api":{"kind":"image","image":"ghcr.io/acme/api","roles":{"web":{}}}},"probes":{"site":{"http":"http://127.0.0.1:8080/"}}}`
	connector := `{"project":"demo","network":"demo","apps":{"ingress":{"kind":"image","image":"cloudflare/cloudflared","roles":{"connector":{}}}}}`
	release("first", "api", first)
	release("connector", "ingress", connector)
	m, applied, err := s.LatestManifest(ctx)
	if err != nil || m.Probes["site"].HTTP != "http://127.0.0.1:8080/" || applied.IsZero() {
		t.Fatalf("connector release used as the source: %+v %v %v", m, applied, err)
	}
	second := `{"project":"demo","network":"demo","apps":{"api":{"kind":"image","image":"ghcr.io/acme/api","roles":{"web":{}}}},"probes":{"other":{"tcp":"10.0.0.5:5432"}}}`
	release("second", "api", second)
	m, _, err = s.LatestManifest(ctx)
	if err != nil || len(m.Probes) != 1 || m.Probes["other"].TCP != "10.0.0.5:5432" {
		t.Fatalf("newest release not chosen: %+v %v", m.Probes, err)
	}
	release("broken", "api", `{"project":"demo"}`)
	if _, _, err = s.LatestManifest(ctx); err == nil {
		t.Fatal("unreadable stored manifest accepted")
	}
}

func TestRecordAuditKeepsActionAndOutcome(t *testing.T) {
	s, _, _ := testStore(t)
	ctx := context.Background()
	if err := s.RecordAudit(ctx, Actor{}, "alerts_test", "succeeded"); err == nil {
		t.Fatal("audit without an actor accepted")
	}
	if err := s.RecordAudit(ctx, Actor{ID: "fingerprint", Name: "token:abc123", Remote: "10.0.0.2:5555"}, "alerts_test", "failed"); err != nil {
		t.Fatal(err)
	}
	rows, err := s.AuditLog(ctx, 10)
	if err != nil || len(rows) != 1 || rows[0].Action != "alerts_test" || rows[0].Outcome != "failed" || rows[0].ActorName != "token:abc123" || rows[0].OperationID != "" {
		t.Fatalf("audit rows: %+v %v", rows, err)
	}
}
