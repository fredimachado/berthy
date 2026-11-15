package state

import (
	"context"
	"database/sql"
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
