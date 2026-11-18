package deploy

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"berthy/internal/cloudflare"
	"berthy/internal/manifest"
	"berthy/internal/state"
)

// Digests of the api image. A deploy in a test uses a digest that the service has not run, so the service changes.
var (
	apiFirst  = "sha256:" + strings.Repeat("d", 64)
	apiSecond = "sha256:" + strings.Repeat("e", 64)
)

// webRole is the JSON of a web role on port 8080, with a health check and the given hostnames, if any.
func webRole(hostnames ...string) string {
	hosts := ""
	if len(hostnames) > 0 {
		hosts = `,"hostnames":["` + strings.Join(hostnames, `","`) + `"]`
	}
	return `{"port":8080,"health":{"test":["CMD","true"]}` + hosts + `}`
}

// apiApps is the apps block with one image app, api, whose web role is role. extra is more JSON for the app, such as
// hooks.
func apiApps(role, extra string) string {
	return `"apps":{"api":{"kind":"image","image":"ghcr.io/acme/api"` + extra + `,"roles":{"web":` + role + `}}}`
}

// withIngress is a manifest with managed ingress, followed by the given members, such as an apps block.
func withIngress(t *testing.T, members string) manifest.Manifest {
	t.Helper()
	input := `{"project":"demo","network":"overlay","ingress":{"cloudflare":{"account_id":"acct","api_token_secret":"CF_TOKEN","image":"cloudflare/cloudflared@sha256:` + strings.Repeat("c", 64) + `"}},` + members + `}`
	m, err := manifest.DecodeJSON([]byte(input))
	if err != nil {
		t.Fatal(err)
	}
	return *m
}

// deploy admits a deploy of app at version and waits for it to finish.
func (f *ingressFixture) deploy(requestID, app, version string, m manifest.Manifest) state.Operation {
	f.t.Helper()
	return f.run(Request{RequestID: requestID, App: app, Version: version, Manifest: m})
}

// applyIngress runs an ingress apply and fails the test unless it succeeds.
func (f *ingressFixture) applyIngress(requestID string, m manifest.Manifest) {
	f.t.Helper()
	if op := f.run(Request{RequestID: requestID, Kind: "ingress", Manifest: m}); op.State != "succeeded" {
		f.t.Fatalf("apply %s: %s", op.State, op.Err)
	}
}

// conflictReasons returns the reasons recorded as conflict events of an operation.
func conflictReasons(t *testing.T, s *state.Store, opID string) []string {
	t.Helper()
	events, err := s.EventsAfter(context.Background(), opID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, ev := range events {
		if ev.Kind != "conflict" {
			continue
		}
		var data struct {
			Reason string `json:"reason"`
		}
		if err = json.Unmarshal(ev.Data, &data); err != nil {
			t.Fatal(err)
		}
		out = append(out, data.Reason)
	}
	return out
}

// releaseWithDigest returns the ID of the api release whose digest ends with digest.
func releaseWithDigest(t *testing.T, s *state.Store, digest string) int64 {
	t.Helper()
	history, err := s.History(context.Background(), "api")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range history {
		if strings.HasSuffix(r.Digest, digest) {
			return r.ID
		}
	}
	t.Fatalf("no api release of %s", digest)
	return 0
}

func TestDeployPublishesRouteRecordAndStateAfterTheServiceStarts(t *testing.T) {
	f := newIngressFixture(t)
	ctx := context.Background()
	m := ingressManifest(t, "")
	f.applyIngress("apply", m)
	tunnelID := f.tunnel().ID
	f.calls.reset()
	if op := f.deploy("deploy", "api", apiFirst, m); op.State != "succeeded" {
		t.Fatalf("deploy %s: %s", op.State, op.Err)
	}
	if got := describeRules(f.tunnelConfig(tunnelID)); !reflect.DeepEqual(got, []string{"api.example.com||" + apiTarget, "||http_status:404"}) {
		t.Fatalf("tunnel rules %v", got)
	}
	records := f.cf.Records(f.zone)
	if len(records) != 1 || records[0].Type != "CNAME" || records[0].Name != "api.example.com" || records[0].Content != cloudflare.TunnelTarget(tunnelID) || !records[0].Proxied || records[0].Comment != cloudflare.RecordComment(ingressEnv) {
		t.Fatalf("records %+v", records)
	}
	routes, err := f.store.IngressRoutes(ctx, "api")
	if err != nil || len(routes) != 1 || routes[0].Service != apiTarget || routes[0].ZoneID != f.zone || routes[0].DNSRecordID != records[0].ID {
		t.Fatalf("routes %+v %v", routes, err)
	}
	service, publish := f.calls.index("service create demo-api-web"), f.calls.index("cloudflare PUT /accounts/")
	if service < 0 || publish < 0 || service > publish {
		t.Fatalf("calls %v: the service must start before its routes are published", f.calls.all())
	}
}

func TestDeployConflictStopsBeforeAnyChange(t *testing.T) {
	f := newIngressFixture(t)
	ctx := context.Background()
	f.applyIngress("apply", ingressManifest(t, ""))
	foreign := f.cf.AddRecord(f.zone, cloudflare.DNSRecord{Type: "CNAME", Name: "api.example.com", Content: "elsewhere.example.net", Proxied: true})
	m := withIngress(t, apiApps(webRole("api.example.com"), `,"hooks":{"pre_deploy":[{"run":["migrate"]}]}`))
	f.calls.reset()
	puts, writes := f.cf.ConfigPuts, f.cf.RecordWrites
	op := f.deploy("conflict", "api", apiFirst, m)
	if op.State != "failed" || op.Err != "ingress hostname conflict" {
		t.Fatalf("state %s: %s", op.State, op.Err)
	}
	if calls := f.calls.all(); len(calls) != 0 {
		t.Fatalf("changes after a conflict: %v", calls)
	}
	if f.cf.ConfigPuts != puts || f.cf.RecordWrites != writes {
		t.Fatalf("cloudflare writes: configurations %d->%d, records %d->%d", puts, f.cf.ConfigPuts, writes, f.cf.RecordWrites)
	}
	if _, err := f.store.Applied(ctx, "api"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("release recorded: %v", err)
	}
	if records := f.cf.Records(f.zone); len(records) != 1 || records[0].ID != foreign {
		t.Fatalf("records %+v", records)
	}
	if reasons := conflictReasons(t, f.store, op.ID); !reflect.DeepEqual(reasons, []string{"DNS name api.example.com is used by another record"}) {
		t.Fatalf("conflicts %v", reasons)
	}
}

func TestDeployWithoutAppliedIngressFailsBeforeAnyChange(t *testing.T) {
	f := newIngressFixture(t)
	ctx := context.Background()
	op := f.deploy("deploy", "api", apiFirst, ingressManifest(t, ""))
	if op.State != "failed" || op.Err != "managed ingress is not applied; run ingress apply" {
		t.Fatalf("state %s: %s", op.State, op.Err)
	}
	if calls := f.calls.all(); len(calls) != 0 {
		t.Fatalf("changes: %v", calls)
	}
	if n := len(f.bound.createdSpecs()); n != 0 {
		t.Fatalf("%d services created", n)
	}
	if f.tunnel() != nil {
		t.Fatal("a tunnel was created")
	}
	if _, err := f.store.Applied(ctx, "api"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("release recorded: %v", err)
	}
	if _, err := f.store.Lock(ctx); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("environment still locked: %v", err)
	}
}

func TestPublishFailureKeepsTheNewRelease(t *testing.T) {
	f := newIngressFixture(t)
	ctx := context.Background()
	m := ingressManifest(t, "")
	f.applyIngress("apply", m)
	f.cf.FailRequests("PUT", "/accounts/", http.StatusInternalServerError)
	op := f.deploy("deploy", "api", apiFirst, m)
	if op.State != "failed" || op.Err != "ingress publish failed" {
		t.Fatalf("state %s: %s", op.State, op.Err)
	}
	applied, err := f.store.Applied(ctx, "api")
	if err != nil || applied.OperationID != op.ID || !strings.HasSuffix(applied.Digest, apiFirst) {
		t.Fatalf("applied %+v: %v", applied, err)
	}
	svc, err := f.docker.InspectService(ctx, "demo-api-web")
	if err != nil || !strings.Contains(string(svc.Spec), apiFirst) {
		t.Fatalf("service does not run the new image: %v", err)
	}
	if routes, err := f.store.IngressRoutes(ctx, "api"); err != nil || len(routes) != 0 {
		t.Fatalf("routes %+v %v", routes, err)
	}
}

func TestDeployWithoutTheHostnameRemovesItsRouteAndRecord(t *testing.T) {
	f := newIngressFixture(t)
	ctx := context.Background()
	f.applyIngress("apply", ingressManifest(t, ""))
	if op := f.deploy("publish", "api", apiFirst, ingressManifest(t, "")); op.State != "succeeded" {
		t.Fatalf("publish %s: %s", op.State, op.Err)
	}
	tunnelID := f.tunnel().ID
	foreign := f.cf.AddRecord(f.zone, cloudflare.DNSRecord{Type: "A", Name: "other.example.com", Content: "203.0.113.9"})
	if op := f.deploy("drop", "api", apiSecond, withIngress(t, apiApps(webRole(), ""))); op.State != "succeeded" {
		t.Fatalf("drop %s: %s", op.State, op.Err)
	}
	if got := describeRules(f.tunnelConfig(tunnelID)); !reflect.DeepEqual(got, []string{"||http_status:404"}) {
		t.Fatalf("tunnel rules after the hostname is dropped %v", got)
	}
	if records := f.cf.Records(f.zone); len(records) != 1 || records[0].ID != foreign || records[0].Type != "A" {
		t.Fatalf("records %+v", records)
	}
	if routes, err := f.store.IngressRoutes(ctx, "api"); err != nil || len(routes) != 0 {
		t.Fatalf("routes %+v %v", routes, err)
	}
}

func TestRollbackFollowsTheRolledBackHostnames(t *testing.T) {
	f := newIngressFixture(t)
	ctx := context.Background()
	f.applyIngress("apply", ingressManifest(t, ""))
	if op := f.deploy("bare", "api", apiFirst, withIngress(t, apiApps(webRole(), ""))); op.State != "succeeded" {
		t.Fatalf("bare deploy %s: %s", op.State, op.Err)
	}
	if op := f.deploy("host", "api", apiSecond, ingressManifest(t, "")); op.State != "succeeded" {
		t.Fatalf("host deploy %s: %s", op.State, op.Err)
	}
	tunnelID := f.tunnel().ID
	bare := releaseWithDigest(t, f.store, apiFirst)
	host := releaseWithDigest(t, f.store, apiSecond)

	if op := f.run(Request{RequestID: "back-to-bare", App: "api", Kind: "rollback", TargetRelease: bare}); op.State != "succeeded" {
		t.Fatalf("rollback to the release without the hostname %s: %s", op.State, op.Err)
	}
	if got := describeRules(f.tunnelConfig(tunnelID)); !reflect.DeepEqual(got, []string{"||http_status:404"}) {
		t.Fatalf("rules after the rollback %v", got)
	}
	if routes, err := f.store.IngressRoutes(ctx, "api"); err != nil || len(routes) != 0 {
		t.Fatalf("routes after the rollback %+v %v", routes, err)
	}
	if records := f.cf.Records(f.zone); len(records) != 0 {
		t.Fatalf("records after the rollback %+v", records)
	}

	if op := f.run(Request{RequestID: "back-to-host", App: "api", Kind: "rollback", TargetRelease: host}); op.State != "succeeded" {
		t.Fatalf("rollback to the release with the hostname %s: %s", op.State, op.Err)
	}
	if got := describeRules(f.tunnelConfig(tunnelID)); !reflect.DeepEqual(got, []string{"api.example.com||" + apiTarget, "||http_status:404"}) {
		t.Fatalf("rules after the second rollback %v", got)
	}
	if routes, err := f.store.IngressRoutes(ctx, "api"); err != nil || len(routes) != 1 || routes[0].Service != apiTarget {
		t.Fatalf("routes after the second rollback %+v %v", routes, err)
	}
	if records := f.cf.Records(f.zone); len(records) != 1 {
		t.Fatalf("records after the second rollback %+v", records)
	}
}

func TestDryRunShowsThePublishWithoutWriting(t *testing.T) {
	f := newIngressFixture(t)
	ctx := context.Background()
	f.applyIngress("apply", ingressManifest(t, ""))
	f.calls.reset()
	puts, writes := f.cf.ConfigPuts, f.cf.RecordWrites
	p, err := f.ctrl.Plan(ctx, Actor{ID: "admin", Admin: true}, Request{App: "api", Version: apiFirst, DryRun: true, Manifest: ingressManifest(t, "")})
	if err != nil {
		t.Fatal(err)
	}
	if p.Ingress == nil || len(p.Blockers) != 0 || len(p.Ingress.Changes) != 1 {
		t.Fatalf("plan %+v", p)
	}
	change := p.Ingress.Changes[0]
	if change.Hostname != "api.example.com" || change.Route != "add" || change.DNS != "add" || change.Target != apiTarget {
		t.Fatalf("change %+v", change)
	}
	if calls := f.calls.all(); len(calls) != 0 || f.cf.ConfigPuts != puts || f.cf.RecordWrites != writes {
		t.Fatalf("dry run wrote: %v", calls)
	}
	if _, err = f.store.Applied(ctx, "api"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("dry run recorded a release: %v", err)
	}
}

func TestDryRunReportsWhatWouldStopThePublish(t *testing.T) {
	admin := Actor{ID: "admin", Admin: true}
	dry := Request{App: "api", Version: apiFirst, DryRun: true, Manifest: ingressManifest(t, "")}

	t.Run("not applied", func(t *testing.T) {
		f := newIngressFixture(t)
		p, err := f.ctrl.Plan(context.Background(), admin, dry)
		if err != nil {
			t.Fatalf("dry run failed: %v", err)
		}
		if p.Ingress != nil || !reflect.DeepEqual(p.Blockers, []string{"managed ingress is not applied; run ingress apply"}) {
			t.Fatalf("plan %+v", p)
		}
	})

	t.Run("conflict", func(t *testing.T) {
		f := newIngressFixture(t)
		f.applyIngress("apply", ingressManifest(t, ""))
		f.cf.AddRecord(f.zone, cloudflare.DNSRecord{Type: "CNAME", Name: "api.example.com", Content: "elsewhere.example.net", Proxied: true})
		p, err := f.ctrl.Plan(context.Background(), admin, dry)
		if err != nil {
			t.Fatalf("dry run failed: %v", err)
		}
		want := []string{"DNS name api.example.com is used by another record"}
		if p.Ingress == nil || !reflect.DeepEqual(p.Ingress.Conflicts, want) || !reflect.DeepEqual(p.Blockers, want) {
			t.Fatalf("plan %+v", p)
		}
	})

	t.Run("cloudflare unavailable", func(t *testing.T) {
		f := newIngressFixture(t)
		f.applyIngress("apply", ingressManifest(t, ""))
		f.cf.FailRequests("GET", "/accounts/", http.StatusInternalServerError)
		p, err := f.ctrl.Plan(context.Background(), admin, dry)
		if err != nil {
			t.Fatalf("dry run failed: %v", err)
		}
		if !reflect.DeepEqual(p.Blockers, []string{"Cloudflare unavailable"}) {
			t.Fatalf("blockers %v", p.Blockers)
		}
	})

	t.Run("connector not applied", func(t *testing.T) {
		f := newIngressFixture(t)
		failedConnectorApply(t, f)
		p, err := f.ctrl.Plan(context.Background(), admin, dry)
		if err != nil {
			t.Fatalf("dry run failed: %v", err)
		}
		if !reflect.DeepEqual(p.Blockers, []string{"managed ingress is not applied; run ingress apply"}) {
			t.Fatalf("plan %+v", p)
		}
	})
}

func TestAdoptionPublishesAfterBinding(t *testing.T) {
	f := newIngressFixture(t)
	ctx := context.Background()
	f.applyIngress("apply", ingressManifest(t, ""))
	one := 1
	role := manifest.Role{Service: "existing-api", Port: 8080, Replicas: &one, Health: &manifest.Health{Test: []string{"CMD", "true"}}}
	spec, err := serviceSpec(ingressEnv, "demo", "api", "web", "prior", "net", "ghcr.io/acme/api@sha256:"+strings.Repeat("a", 64), nil, role, "start-first", "1s", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err = json.Unmarshal(spec, &body); err != nil {
		t.Fatal(err)
	}
	body["Name"] = "existing-api"
	if spec, err = json.Marshal(body); err != nil {
		t.Fatal(err)
	}
	f.bound.add("existing-api-id", spec)
	m := withIngress(t, apiApps(`{"service":"existing-api","port":8080,"health":{"test":["CMD","true"]},"hostnames":["api.example.com"]}`, ""))
	f.calls.reset()
	op := f.run(Request{RequestID: "adopt", Adopt: true, App: "api", Manifest: m})
	if op.State != "succeeded" {
		t.Fatalf("adoption %s: %s", op.State, op.Err)
	}
	bindings, err := f.store.Bindings(ctx, "api")
	if err != nil || len(bindings) != 1 || bindings[0] != (state.Binding{App: "api", Role: "web", Name: "existing-api", ServiceID: "existing-api-id"}) {
		t.Fatalf("bindings %+v: %v", bindings, err)
	}
	if got := describeRules(f.tunnelConfig(f.tunnel().ID)); !reflect.DeepEqual(got, []string{"api.example.com||http://existing-api:8080", "||http_status:404"}) {
		t.Fatalf("tunnel rules %v", got)
	}
	if records := f.cf.Records(f.zone); len(records) != 1 {
		t.Fatalf("records %+v", records)
	}
	if calls := f.calls.all(); f.calls.count("service") != 0 || len(calls) != 2 {
		t.Fatalf("adoption changed a service, or published more than its routes and record: %v", calls)
	}
}

func TestConnectorApplyDoesNotPublishAsIngress(t *testing.T) {
	f := newIngressFixture(t)
	ctx := context.Background()
	seedDeployed(t, f.store, "api", apiBinding())
	op := f.run(Request{RequestID: "apply", Kind: "ingress", Manifest: ingressManifest(t, "")})
	if op.State != "succeeded" {
		t.Fatalf("apply %s: %s", op.State, op.Err)
	}
	if f.cf.ConfigPuts != 1 || f.cf.RecordWrites != 1 {
		t.Fatalf("apply wrote %d configurations and %d records, want one of each for the api route", f.cf.ConfigPuts, f.cf.RecordWrites)
	}
	if reasons := conflictReasons(t, f.store, op.ID); len(reasons) != 0 {
		t.Fatalf("conflicts %v", reasons)
	}
	if routes, err := f.store.IngressRoutes(ctx, "ingress"); err != nil || len(routes) != 0 {
		t.Fatalf("connector routes %+v %v", routes, err)
	}
}

func TestIngressRemovalDeletesTheTunnelToken(t *testing.T) {
	f := newIngressFixture(t)
	ctx := context.Background()
	seedDeployed(t, f.store, "api", apiBinding())
	f.applyIngress("apply", ingressManifest(t, ""))
	if _, _, err := f.store.Secret(ctx, manifest.TunnelTokenSecret); err != nil {
		t.Fatalf("no tunnel token after apply: %v", err)
	}
	op := f.run(Request{RequestID: "remove", Kind: "ingress", Manifest: plainManifest(t)})
	if op.State != "succeeded" {
		t.Fatalf("removal %s: %s", op.State, op.Err)
	}
	if _, _, err := f.store.Secret(ctx, manifest.TunnelTokenSecret); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("tunnel token kept after removal: %v", err)
	}
	audit, err := f.store.AuditLog(ctx, 50)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range audit {
		if a.Action == "secret_delete:"+manifest.TunnelTokenSecret && a.OperationID == op.ID && a.Outcome == "succeeded" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no audit row for the token removal: %+v", audit)
	}
}

func TestPostDeployFailureWinsOverPublishFailure(t *testing.T) {
	f := newIngressFixture(t)
	ctx := context.Background()
	f.applyIngress("apply", ingressManifest(t, ""))
	f.docker.exit = 1
	f.cf.FailRequests("PUT", "/accounts/", http.StatusInternalServerError)
	m := withIngress(t, apiApps(webRole("api.example.com"), `,"hooks":{"post_deploy":[{"run":["notify"]}]}`))
	op := f.deploy("deploy", "api", apiFirst, m)
	if op.State != "failed" || op.Err != "post-deploy command failed" {
		t.Fatalf("state %s: %s", op.State, op.Err)
	}
	if n := f.calls.count("one-off berthy-"); n != 1 {
		t.Fatalf("post-deploy command ran %d times, want once: %v", n, f.calls.all())
	}
	if routes, err := f.store.IngressRoutes(ctx, "api"); err != nil || len(routes) != 0 {
		t.Fatalf("the publish was meant to fail, but routes were recorded: %+v %v", routes, err)
	}
	if _, err := f.store.Applied(ctx, "api"); err != nil {
		t.Fatalf("release not recorded: %v", err)
	}
}

func TestDeployCannotTakeAnotherAppsRoute(t *testing.T) {
	f := newIngressFixture(t)
	ctx := context.Background()
	f.applyIngress("apply", ingressManifest(t, ""))
	if op := f.deploy("api", "api", apiFirst, ingressManifest(t, "")); op.State != "succeeded" {
		t.Fatalf("api deploy %s: %s", op.State, op.Err)
	}
	adminDigest := "sha256:" + strings.Repeat("a", 64)
	both := withIngress(t, `"apps":{"api":{"kind":"image","image":"ghcr.io/acme/api","roles":{"web":`+webRole()+`}},"admin":{"kind":"image","image":"ghcr.io/acme/admin","roles":{"web":{"port":9000,"health":{"test":["CMD","true"]},"hostnames":["api.example.com"]}}}}`)

	op := f.deploy("admin", "admin", adminDigest, both)
	if op.State != "failed" || op.Err != "ingress hostname conflict" {
		t.Fatalf("state %s: %s", op.State, op.Err)
	}
	if reasons := conflictReasons(t, f.store, op.ID); !reflect.DeepEqual(reasons, []string{"route api.example.com belongs to app api"}) {
		t.Fatalf("conflicts %v", reasons)
	}
	if n := f.calls.count("service create demo-admin-web"); n != 0 {
		t.Fatalf("admin service created despite the conflict: %v", f.calls.all())
	}

	op = f.run(Request{RequestID: "admin-replace", App: "admin", Version: adminDigest, Manifest: both, ReplaceExisting: true})
	if op.State != "succeeded" {
		t.Fatalf("replace %s: %s", op.State, op.Err)
	}
	routes, err := f.store.IngressRoutes(ctx, "admin")
	if err != nil || len(routes) != 1 || routes[0].Hostname != "api.example.com" || routes[0].Service != "http://demo-admin-web:9000" {
		t.Fatalf("admin routes %+v %v", routes, err)
	}
}

// failedConnectorApply runs an ingress apply while the connector cannot start. The tunnel, its stored token and the
// ingress state remain, but the connector has no applied release. It returns the switch that lets later runs start it.
func failedConnectorApply(t *testing.T, f *ingressFixture) *atomic.Bool {
	t.Helper()
	down := connectorOutage(f)
	down.Store(true)
	op := f.run(Request{RequestID: "apply-broken", Kind: "ingress", Manifest: ingressManifest(t, "")})
	if op.State != "failed" || op.Err != "rollout failed and was cleaned up" {
		t.Fatalf("apply %s: %s", op.State, op.Err)
	}
	return down
}

// seedOwnedRoute records a route Berthy owns, as an earlier publish would have, under an operation that finishes at once,
// so the environment is free for the test's next run.
func seedOwnedRoute(t *testing.T, f *ingressFixture, route state.IngressRoute) {
	t.Helper()
	ctx := context.Background()
	op, _, err := f.store.Admit(ctx, state.Admission{RequestID: "seed-route-" + route.Hostname, ActorID: "admin", Kind: "ingress", App: "ingress", PublicRequest: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err = f.store.SetIngressRoute(ctx, op.ID, route); err != nil {
		t.Fatal(err)
	}
	if err = f.store.Finish(ctx, op.ID, state.Outcome{State: "succeeded"}); err != nil {
		t.Fatal(err)
	}
}

func TestFailedConnectorRolloutBlocksPublishingUntilItIsApplied(t *testing.T) {
	f := newIngressFixture(t)
	ctx := context.Background()
	down := failedConnectorApply(t, f)
	if _, err := f.store.Ingress(ctx); err != nil {
		t.Fatalf("ingress state lost: %v", err)
	}
	if _, _, err := f.store.Secret(ctx, manifest.TunnelTokenSecret); err != nil {
		t.Fatalf("tunnel token lost: %v", err)
	}

	f.calls.reset()
	puts, writes := f.cf.ConfigPuts, f.cf.RecordWrites
	op := f.deploy("publish-early", "api", apiFirst, ingressManifest(t, ""))
	if op.State != "failed" || op.Err != "managed ingress is not applied; run ingress apply" {
		t.Fatalf("state %s: %s", op.State, op.Err)
	}
	if calls := f.calls.all(); len(calls) != 0 || f.cf.ConfigPuts != puts || f.cf.RecordWrites != writes {
		t.Fatalf("changes before the refusal: %v", calls)
	}
	if _, err := f.store.Applied(ctx, "api"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("release recorded: %v", err)
	}
	if routes, err := f.store.IngressRoutes(ctx, ""); err != nil || len(routes) != 0 {
		t.Fatalf("routes %+v %v", routes, err)
	}

	// Once the connector runs, the same deploy publishes its route.
	down.Store(false)
	f.applyIngress("apply-2", ingressManifest(t, ""))
	if op := f.deploy("publish-later", "api", apiFirst, ingressManifest(t, "")); op.State != "succeeded" {
		t.Fatalf("deploy after the apply %s: %s", op.State, op.Err)
	}
	if got := describeRules(f.tunnelConfig(f.tunnel().ID)); !reflect.DeepEqual(got, []string{"api.example.com||" + apiTarget, "||http_status:404"}) {
		t.Fatalf("tunnel rules %v", got)
	}
}

// A route is seeded directly here: a normal flow records a route only with a connector release behind it. The test checks
// that removing the app's last hostname does not need that release.
func TestRemovalStillPublishesWithoutAConnector(t *testing.T) {
	f := newIngressFixture(t)
	ctx := context.Background()
	failedConnectorApply(t, f)
	tunnelID := f.tunnel().ID
	record := f.cf.AddRecord(f.zone, cloudflare.DNSRecord{Type: "CNAME", Name: "api.example.com", Content: cloudflare.TunnelTarget(tunnelID), Proxied: true, Comment: cloudflare.RecordComment(ingressEnv)})
	f.cf.SetConfig(tunnelID, json.RawMessage(`{"ingress":[{"hostname":"api.example.com","service":"`+apiTarget+`"},{"service":"http_status:404"}]}`))
	seedOwnedRoute(t, f, state.IngressRoute{Hostname: "api.example.com", App: "api", Role: "web", Service: apiTarget, ZoneID: f.zone, DNSRecordID: record, RouteCreated: true, DNSCreated: true})

	// The app's last hostname goes. Its route and record are its own, so the removal publishes with no connector.
	if op := f.deploy("drop", "api", apiFirst, withIngress(t, apiApps(webRole(), ""))); op.State != "succeeded" {
		t.Fatalf("removal deploy %s: %s", op.State, op.Err)
	}
	if got := describeRules(f.tunnelConfig(tunnelID)); !reflect.DeepEqual(got, []string{"||http_status:404"}) {
		t.Fatalf("tunnel rules after the removal %v", got)
	}
	if records := f.cf.Records(f.zone); len(records) != 0 {
		t.Fatalf("records after the removal %+v", records)
	}
	if routes, err := f.store.IngressRoutes(ctx, "api"); err != nil || len(routes) != 0 {
		t.Fatalf("routes after the removal %+v %v", routes, err)
	}
}
