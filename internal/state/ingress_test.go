package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
)

func openIngressOp(t *testing.T, s *Store, requestID string) Operation {
	t.Helper()
	op, _, err := s.Admit(context.Background(), Admission{RequestID: requestID, ActorID: "admin", ActorName: "ops", Remote: "192.0.2.7", Kind: "ingress", PublicRequest: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	return op
}

func TestIngressStateAndRoutesNeedTheLock(t *testing.T) {
	s, _, _ := testStore(t)
	ctx := context.Background()
	st := IngressState{AccountID: "acct", APITokenSecret: "CLOUDFLARE_API_TOKEN", TunnelID: "tunnel-1", TunnelName: "demo-prod", DNS: true}
	route := IngressRoute{Hostname: "api.example.com", App: "api", Role: "web", Service: "http://demo-prod-api-web:8080", ZoneID: "zone-1", DNSRecordID: "record-1"}
	if _, err := s.Ingress(ctx); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("ingress before any apply: %v", err)
	}
	if err := s.SetIngress(ctx, "missing", st); !errors.Is(err, ErrConflict) {
		t.Fatalf("unlocked ingress write: %v", err)
	}
	if err := s.SetIngressRoute(ctx, "missing", route); !errors.Is(err, ErrConflict) {
		t.Fatalf("unlocked route write: %v", err)
	}
	if err := s.DeleteIngressRoute(ctx, "missing", route.Hostname); !errors.Is(err, ErrConflict) {
		t.Fatalf("unlocked route delete: %v", err)
	}
	if err := s.ClearIngress(ctx, "missing"); !errors.Is(err, ErrConflict) {
		t.Fatalf("unlocked clear: %v", err)
	}

	op := openIngressOp(t, s, "apply")
	if err := s.SetIngress(ctx, op.ID, st); err != nil {
		t.Fatal(err)
	}
	got, err := s.Ingress(ctx)
	if err != nil || got.AccountID != "acct" || got.APITokenSecret != "CLOUDFLARE_API_TOKEN" || got.TunnelID != "tunnel-1" || got.TunnelName != "demo-prod" || !got.DNS || got.Updated.IsZero() {
		t.Fatalf("ingress %+v %v", got, err)
	}
	st.DNS = false
	if err = s.SetIngress(ctx, op.ID, st); err != nil {
		t.Fatal(err)
	}
	if got, err = s.Ingress(ctx); err != nil || got.DNS {
		t.Fatalf("upsert did not change dns: %+v %v", got, err)
	}
	if err = s.SetIngress(ctx, op.ID, IngressState{}); err == nil {
		t.Fatal("empty ingress state accepted")
	}

	if err = s.SetIngressRoute(ctx, op.ID, route); err != nil {
		t.Fatal(err)
	}
	unmanaged := IngressRoute{Hostname: "admin.example.com", App: "admin", Role: "web", Service: "http://demo-prod-admin-web:9000", ZoneID: "zone-1"}
	if err = s.SetIngressRoute(ctx, op.ID, unmanaged); err != nil {
		t.Fatal(err)
	}
	routes, err := s.IngressRoutes(ctx, "")
	if err != nil || len(routes) != 2 || routes[0] != unmanaged || routes[1] != route {
		t.Fatalf("routes ordered by hostname: %+v %v", routes, err)
	}
	apiRoutes, err := s.IngressRoutes(ctx, "api")
	if err != nil || len(apiRoutes) != 1 || apiRoutes[0] != route {
		t.Fatalf("routes for api: %+v %v", apiRoutes, err)
	}
	route.Service = "http://demo-prod-api-web:8081"
	if err = s.SetIngressRoute(ctx, op.ID, route); err != nil {
		t.Fatal(err)
	}
	if routes, err = s.IngressRoutes(ctx, ""); err != nil || len(routes) != 2 || routes[1] != route {
		t.Fatalf("route upsert: %+v %v", routes, err)
	}
	if err = s.SetIngressRoute(ctx, op.ID, IngressRoute{Hostname: "x.example.com", App: "api", Role: "web"}); err == nil {
		t.Fatal("route without a service accepted")
	}
	if err = s.DeleteIngressRoute(ctx, op.ID, unmanaged.Hostname); err != nil {
		t.Fatal(err)
	}
	if err = s.DeleteIngressRoute(ctx, op.ID, unmanaged.Hostname); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("second delete: %v", err)
	}

	if err = s.Finish(ctx, op.ID, Outcome{State: "succeeded"}); err != nil {
		t.Fatal(err)
	}
	if err = s.SetIngressRoute(ctx, op.ID, route); !errors.Is(err, ErrConflict) {
		t.Fatalf("write after the operation finished: %v", err)
	}
	if routes, err = s.IngressRoutes(ctx, ""); err != nil || len(routes) != 1 {
		t.Fatalf("reads after finish: %+v %v", routes, err)
	}
}

func TestClearIngressRemovesStateAndRoutes(t *testing.T) {
	s, _, _ := testStore(t)
	ctx := context.Background()
	op := openIngressOp(t, s, "apply")
	if err := s.SetIngress(ctx, op.ID, IngressState{AccountID: "acct", APITokenSecret: "CLOUDFLARE_API_TOKEN", TunnelID: "t", TunnelName: "demo-prod"}); err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{"a.example.com", "b.example.com"} {
		if err := s.SetIngressRoute(ctx, op.ID, IngressRoute{Hostname: host, App: "api", Role: "web", Service: "http://svc:8080", ZoneID: "z"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.ClearIngress(ctx, op.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Ingress(ctx); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("ingress after clear: %v", err)
	}
	if routes, err := s.IngressRoutes(ctx, ""); err != nil || len(routes) != 0 {
		t.Fatalf("routes after clear: %+v %v", routes, err)
	}
}

func TestPutOperationSecretNeedsTheLockAndAudits(t *testing.T) {
	s, _, _ := testStore(t)
	ctx := context.Background()
	if err := s.PutOperationSecret(ctx, "missing", "TUNNEL_TOKEN", []byte("token-value")); !errors.Is(err, ErrConflict) {
		t.Fatalf("unlocked secret write: %v", err)
	}
	op := openIngressOp(t, s, "apply")
	if err := s.PutSecret(ctx, Actor{ID: "admin"}, SecretInfo{Name: "OTHER"}, []byte("value")); !errors.Is(err, ErrConflict) {
		t.Fatalf("PutSecret while locked: %v", err)
	}
	if err := s.PutOperationSecret(ctx, op.ID, "TUNNEL_TOKEN", []byte("token-value")); err != nil {
		t.Fatal(err)
	}
	info, value, err := s.Secret(ctx, "TUNNEL_TOKEN")
	if err != nil || string(value) != "token-value" || info.Registry != "" {
		t.Fatalf("secret round trip %+v %v", info, err)
	}
	if err = s.PutOperationSecret(ctx, op.ID, "TUNNEL_TOKEN", []byte("rotated")); err != nil {
		t.Fatal(err)
	}
	if _, value, err = s.Secret(ctx, "TUNNEL_TOKEN"); err != nil || string(value) != "rotated" {
		t.Fatalf("rotated secret %q %v", value, err)
	}
	audit, err := s.AuditLog(ctx, 20)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range audit {
		if a.Action == "secret_set:TUNNEL_TOKEN" {
			found = a.OperationID == op.ID && a.ActorID == "admin" && a.ActorName == "ops" && a.Outcome == "succeeded"
			break
		}
	}
	if !found {
		t.Fatalf("no audit row for the secret write: %+v", audit)
	}
	if err = s.Finish(ctx, op.ID, Outcome{State: "succeeded"}); err != nil {
		t.Fatal(err)
	}
	if err = s.PutOperationSecret(ctx, op.ID, "TUNNEL_TOKEN", []byte("late")); !errors.Is(err, ErrConflict) {
		t.Fatalf("secret write after the operation finished: %v", err)
	}
}

func TestDeleteOperationSecretNeedsTheLockAndAudits(t *testing.T) {
	s, _, _ := testStore(t)
	ctx := context.Background()
	if err := s.DeleteOperationSecret(ctx, "missing", "TUNNEL_TOKEN"); !errors.Is(err, ErrConflict) {
		t.Fatalf("unlocked secret delete: %v", err)
	}
	op := openIngressOp(t, s, "remove")
	if err := s.PutOperationSecret(ctx, op.ID, "TUNNEL_TOKEN", []byte("token-value")); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteOperationSecret(ctx, op.ID, "TUNNEL_TOKEN"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Secret(ctx, "TUNNEL_TOKEN"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("secret after delete: %v", err)
	}
	if err := s.DeleteOperationSecret(ctx, op.ID, "TUNNEL_TOKEN"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("second delete: %v", err)
	}
	audit, err := s.AuditLog(ctx, 20)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range audit {
		if a.Action == "secret_delete:TUNNEL_TOKEN" {
			found = a.OperationID == op.ID && a.ActorID == "admin" && a.ActorName == "ops" && a.Outcome == "succeeded"
			break
		}
	}
	if !found {
		t.Fatalf("no audit row for the secret delete: %+v", audit)
	}
	if err = s.Finish(ctx, op.ID, Outcome{State: "succeeded"}); err != nil {
		t.Fatal(err)
	}
	if err = s.DeleteOperationSecret(ctx, op.ID, "TUNNEL_TOKEN"); !errors.Is(err, ErrConflict) {
		t.Fatalf("delete after the operation finished: %v", err)
	}
}

func TestForgetAppNeedsTheLockAndKeepsHistory(t *testing.T) {
	s, _, _ := testStore(t)
	ctx := context.Background()
	deployed, _, err := s.Admit(ctx, Admission{RequestID: "deployed", ActorID: "a", Kind: "deploy", App: "ingress", PublicRequest: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Finish(ctx, deployed.ID, Outcome{State: "succeeded", Release: &Release{App: "ingress", Digest: "digest-1", Manifest: []byte(`{}`), Bindings: []Binding{{App: "ingress", Role: "connector", Name: "demo-ingress-connector", ServiceID: "svc-1"}}}}); err != nil {
		t.Fatal(err)
	}
	if err = s.ForgetApp(ctx, "missing", "ingress"); !errors.Is(err, ErrConflict) {
		t.Fatalf("unlocked forget: %v", err)
	}
	op := openIngressOp(t, s, "remove")
	if err = s.ForgetApp(ctx, op.ID, "ingress"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Applied(ctx, "ingress"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("applied after forget: %v", err)
	}
	if bindings, err := s.Bindings(ctx, "ingress"); err != nil || len(bindings) != 0 {
		t.Fatalf("bindings after forget: %+v %v", bindings, err)
	}
	if history, err := s.History(ctx, "ingress"); err != nil || len(history) != 1 {
		t.Fatalf("history after forget: %d %v", len(history), err)
	}
	if err = s.Finish(ctx, op.ID, Outcome{State: "succeeded"}); err != nil {
		t.Fatal(err)
	}
}

func TestReplaceExistingIsAPublicBoolean(t *testing.T) {
	s, _, _ := testStore(t)
	ctx := context.Background()
	if _, _, err := s.Admit(ctx, Admission{RequestID: "replace", ActorID: "a", Kind: "deploy", App: "api", PublicRequest: []byte(`{"app":"api","replace_existing":true}`)}); err != nil {
		t.Fatalf("boolean refused: %v", err)
	}
	if _, _, err := s.Admit(ctx, Admission{RequestID: "replace-text", ActorID: "a", Kind: "deploy", App: "api", PublicRequest: []byte(`{"replace_existing":"yes"}`)}); err == nil {
		t.Fatal("string accepted for a boolean")
	}
}

func TestUpgradeVersionEightKeepsRoutesCreatedByBerthy(t *testing.T) {
	s, dir, key := testStore(t)
	ctx := context.Background()
	op := openIngressOp(t, s, "apply")
	if err := s.SetIngressRoute(ctx, op.ID, IngressRoute{Hostname: "api.example.com", App: "api", Role: "web", Service: "http://svc:8080", ZoneID: "z", DNSRecordID: "r", RouteCreated: true, DNSCreated: true}); err != nil {
		t.Fatal(err)
	}
	// A schema 8 database has none of the ownership columns.
	if _, err := s.db.Exec(`ALTER TABLE ingress_routes DROP COLUMN route_created; ALTER TABLE ingress_routes DROP COLUMN dns_created; ALTER TABLE ingress_routes DROP COLUMN dns_prior_comment; PRAGMA user_version=8`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	upgraded, err := Open(dir, key, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()
	var version int
	if err = upgraded.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 10 {
		t.Fatalf("migration: %d %v", version, err)
	}
	routes, err := upgraded.IngressRoutes(ctx, "")
	if err != nil || len(routes) != 1 || !routes[0].RouteCreated || !routes[0].DNSCreated || routes[0].DNSPriorComment != "" {
		t.Fatalf("an existing route must count as created by Berthy: %+v %v", routes, err)
	}
}

func TestIngressRouteOwnershipRoundTrips(t *testing.T) {
	s, _, _ := testStore(t)
	ctx := context.Background()
	op := openIngressOp(t, s, "apply")
	adopted := IngressRoute{Hostname: "a.example.com", App: "api", Role: "web", Service: "http://svc:8080", ZoneID: "z", DNSRecordID: "r1", RouteCreated: false, DNSCreated: false, DNSPriorComment: "the team's record"}
	created := IngressRoute{Hostname: "b.example.com", App: "api", Role: "web", Service: "http://svc:8080", ZoneID: "z", DNSRecordID: "r2", RouteCreated: true, DNSCreated: true}
	for _, r := range []IngressRoute{adopted, created} {
		if err := s.SetIngressRoute(ctx, op.ID, r); err != nil {
			t.Fatal(err)
		}
	}
	routes, err := s.IngressRoutes(ctx, "api")
	if err != nil || len(routes) != 2 || routes[0] != adopted || routes[1] != created {
		t.Fatalf("ownership flags lost: %+v %v", routes, err)
	}
	// An upsert replaces the flags with the new row's.
	adopted.RouteCreated, adopted.DNSCreated, adopted.DNSPriorComment = true, true, ""
	if err = s.SetIngressRoute(ctx, op.ID, adopted); err != nil {
		t.Fatal(err)
	}
	if routes, err = s.IngressRoutes(ctx, "api"); err != nil || routes[0] != adopted {
		t.Fatalf("upsert kept stale flags: %+v %v", routes, err)
	}
}

func TestForgetIngressRoutesRemovesOneAppsRowsUnderTheLock(t *testing.T) {
	s, _, _ := testStore(t)
	ctx := context.Background()
	if err := s.ForgetIngressRoutes(ctx, "missing", "api"); !errors.Is(err, ErrConflict) {
		t.Fatalf("unlocked forget: %v", err)
	}
	op := openIngressOp(t, s, "apply")
	for _, r := range []IngressRoute{
		{Hostname: "a.example.com", App: "api", Role: "web", Service: "http://svc:8080"},
		{Hostname: "b.example.com", App: "api", Role: "web", Service: "http://svc:8080"},
		{Hostname: "c.example.com", App: "admin", Role: "web", Service: "http://svc2:8080"},
	} {
		if err := s.SetIngressRoute(ctx, op.ID, r); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.ForgetIngressRoutes(ctx, op.ID, "api"); err != nil {
		t.Fatal(err)
	}
	routes, err := s.IngressRoutes(ctx, "")
	if err != nil || len(routes) != 1 || routes[0].App != "admin" {
		t.Fatalf("only the other app's route may stay: %+v %v", routes, err)
	}
	if err = s.ForgetIngressRoutes(ctx, op.ID, "api"); err != nil {
		t.Fatalf("forgetting an app with no routes is not an error: %v", err)
	}
	if err = s.ForgetIngressRoutes(ctx, op.ID, ""); err == nil {
		t.Fatal("an empty app name was accepted")
	}
}

func TestAppliedAppsListsAppsWithAnAppliedRelease(t *testing.T) {
	s, _, _ := testStore(t)
	ctx := context.Background()
	if apps, err := s.AppliedApps(ctx); err != nil || len(apps) != 0 {
		t.Fatalf("empty state: %v %v", apps, err)
	}
	for _, app := range []string{"web", "api"} {
		op, _, err := s.Admit(ctx, Admission{RequestID: "d-" + app, ActorID: "a", Kind: "deploy", App: app, PublicRequest: json.RawMessage(`{}`)})
		if err != nil {
			t.Fatal(err)
		}
		if err = s.Finish(ctx, op.ID, Outcome{State: "succeeded", Release: &Release{App: app, Digest: "d", Manifest: json.RawMessage(`{}`)}}); err != nil {
			t.Fatal(err)
		}
	}
	apps, err := s.AppliedApps(ctx)
	if err != nil || len(apps) != 2 || apps[0] != "api" || apps[1] != "web" {
		t.Fatalf("applied apps: %v %v", apps, err)
	}
	op, _, err := s.Admit(ctx, Admission{RequestID: "forget", ActorID: "a", Kind: "remove", App: "api", PublicRequest: json.RawMessage(`{"app":"api","volumes":true}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ForgetApp(ctx, op.ID, "api"); err != nil {
		t.Fatal(err)
	}
	if apps, err = s.AppliedApps(ctx); err != nil || len(apps) != 1 || apps[0] != "web" {
		t.Fatalf("a forgotten app is no longer applied: %v %v", apps, err)
	}
}

func TestKeptTunnelIsRememberedUnderTheLockAndForgottenWhenIngressIsApplied(t *testing.T) {
	s, _, _ := testStore(t)
	ctx := context.Background()
	st := IngressState{AccountID: "acct", APITokenSecret: "CF_TOKEN", TunnelID: "tunnel-1", TunnelName: "demo-prod", DNS: true}
	if _, err := s.KeptTunnel(ctx); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("before any: %v", err)
	}
	if err := s.SetKeptTunnel(ctx, "missing", st); !errors.Is(err, ErrConflict) {
		t.Fatalf("unlocked set: %v", err)
	}
	if err := s.ClearKeptTunnel(ctx, "missing"); !errors.Is(err, ErrConflict) {
		t.Fatalf("unlocked clear: %v", err)
	}
	op := openIngressOp(t, s, "destroy")
	if err := s.SetKeptTunnel(ctx, op.ID, st); err != nil {
		t.Fatal(err)
	}
	got, err := s.KeptTunnel(ctx)
	if err != nil || got.AccountID != "acct" || got.APITokenSecret != "CF_TOKEN" || got.TunnelID != "tunnel-1" || got.TunnelName != "demo-prod" {
		t.Fatalf("kept tunnel %+v %v", got, err)
	}
	if err = s.ClearKeptTunnel(ctx, op.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.ClearKeptTunnel(ctx, op.ID); err != nil {
		t.Fatalf("clearing nothing is not an error: %v", err)
	}
	if _, err = s.KeptTunnel(ctx); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("after clear: %v", err)
	}
	if err = s.SetKeptTunnel(ctx, op.ID, st); err != nil {
		t.Fatal(err)
	}
	// Applying managed ingress again puts a tunnel in use, so the pointer to a left-over one goes.
	if err = s.SetIngress(ctx, op.ID, st); err != nil {
		t.Fatal(err)
	}
	if _, err = s.KeptTunnel(ctx); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("after SetIngress: %v", err)
	}
}
