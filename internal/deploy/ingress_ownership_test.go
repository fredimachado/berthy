package deploy

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"berthy/internal/cloudflare"
	"berthy/internal/scope"
	"berthy/internal/state"
)

const teamComment = "the team's own record"

// adoptedFixture is an environment whose tunnel already routes api.example.com to the api service and whose zone already
// holds a proxied CNAME to the tunnel, both made by someone else, with api deployed so Berthy adopts them.
func adoptedFixture(t *testing.T) (f *ingressFixture, tunnelID, recordID string) {
	t.Helper()
	f = newIngressFixture(t)
	m := ingressManifest(t, "")
	f.applyIngress("apply", m)
	tunnelID = f.tunnel().ID
	cfg, err := json.Marshal(rulesOf(rule("api.example.com", apiTarget), rule("", "http_status:404")))
	if err != nil {
		t.Fatal(err)
	}
	f.cf.SetConfig(tunnelID, cfg)
	recordID = f.cf.AddRecord(f.zone, cloudflare.DNSRecord{Type: "CNAME", Name: "api.example.com", Content: cloudflare.TunnelTarget(tunnelID), Proxied: true, Comment: teamComment})
	if op := f.deploy("deploy", "api", apiFirst, m); op.State != "succeeded" {
		t.Fatalf("deploy %s: %s", op.State, op.Err)
	}
	return f, tunnelID, recordID
}

func TestIngressApplyRefusesARecordManagedByAnotherProject(t *testing.T) {
	f := newIngressFixture(t)
	m := ingressManifest(t, "")
	f.applyIngress("apply", m)
	tunnelID := f.tunnel().ID
	other := scope.ID{Project: "other", Environment: "prod"}
	foreign := f.cf.AddRecord(f.zone, cloudflare.DNSRecord{
		Type: "CNAME", Name: "api.example.com", Content: cloudflare.TunnelTarget(tunnelID), Proxied: true,
		Comment: cloudflare.RecordComment(other),
	})
	writes := f.cf.RecordWrites
	op := f.deploy("deploy", "api", apiFirst, m)
	if op.State != "failed" || op.Err != "ingress hostname conflict" {
		t.Fatalf("state %s: %s", op.State, op.Err)
	}
	if reasons := conflictReasons(t, f.store, op.ID); !reflect.DeepEqual(reasons, []string{"DNS name api.example.com is managed by another hosted environment"}) {
		t.Fatalf("conflicts %v", reasons)
	}
	records := f.cf.Records(f.zone)
	if len(records) != 1 || records[0].ID != foreign || records[0].Comment != cloudflare.RecordComment(other) || f.cf.RecordWrites != writes {
		t.Fatalf("foreign record changed: %+v writes %d->%d", records, writes, f.cf.RecordWrites)
	}
}

func TestIngressReplaceRefusesARecordManagedByAnotherProject(t *testing.T) {
	f := newIngressFixture(t)
	m := ingressManifest(t, "")
	f.applyIngress("apply", m)
	other := scope.ID{Project: "other", Environment: "prod"}
	foreign := f.cf.AddRecord(f.zone, cloudflare.DNSRecord{
		Type: "CNAME", Name: "api.example.com", Content: "elsewhere.example.net", Proxied: true,
		Comment: cloudflare.RecordComment(other),
	})
	writes := f.cf.RecordWrites
	op := f.run(Request{RequestID: "takeover", App: "api", Version: apiFirst, Manifest: m, ReplaceExisting: true})
	if op.State != "failed" || op.Err != "ingress hostname conflict" {
		t.Fatalf("state %s: %s", op.State, op.Err)
	}
	if reasons := conflictReasons(t, f.store, op.ID); !reflect.DeepEqual(reasons, []string{"DNS name api.example.com is managed by another hosted environment"}) {
		t.Fatalf("conflicts %v", reasons)
	}
	records := f.cf.Records(f.zone)
	if len(records) != 1 || records[0].ID != foreign || records[0].Comment != cloudflare.RecordComment(other) || records[0].Content != "elsewhere.example.net" || f.cf.RecordWrites != writes {
		t.Fatalf("foreign record changed: %+v writes %d->%d", records, writes, f.cf.RecordWrites)
	}
}

func TestDeployRecordsWhatBerthyCreatedAsCreated(t *testing.T) {
	f := newIngressFixture(t)
	m := ingressManifest(t, "")
	f.applyIngress("apply", m)
	if op := f.deploy("deploy", "api", apiFirst, m); op.State != "succeeded" {
		t.Fatalf("deploy %s: %s", op.State, op.Err)
	}
	routes, err := f.store.IngressRoutes(context.Background(), "api")
	if err != nil || len(routes) != 1 {
		t.Fatalf("routes %+v %v", routes, err)
	}
	if !routes[0].RouteCreated || !routes[0].DNSCreated || routes[0].DNSPriorComment != "" {
		t.Fatalf("a rule and a record Berthy added are Berthy's to remove: %+v", routes[0])
	}
}

func TestDeployRecordsAdoptedRuleAndRecordAsNotCreated(t *testing.T) {
	f, _, recordID := adoptedFixture(t)
	routes, err := f.store.IngressRoutes(context.Background(), "api")
	if err != nil || len(routes) != 1 {
		t.Fatalf("routes %+v %v", routes, err)
	}
	r := routes[0]
	if r.RouteCreated || r.DNSCreated || r.DNSPriorComment != teamComment || r.DNSRecordID != recordID {
		t.Fatalf("an adopted rule and record must be marked as not created, with the old comment kept: %+v", r)
	}
	// Berthy marks the adopted record, so it can tell it from a record it never touched.
	records := f.cf.Records(f.zone)
	if len(records) != 1 || records[0].Comment != cloudflare.RecordComment(ingressID) {
		t.Fatalf("records %+v", records)
	}
}

func TestRepeatDeployKeepsTheAdoptedMarks(t *testing.T) {
	f, _, _ := adoptedFixture(t)
	if op := f.deploy("again", "api", apiSecond, ingressManifest(t, "")); op.State != "succeeded" {
		t.Fatalf("deploy %s: %s", op.State, op.Err)
	}
	routes, err := f.store.IngressRoutes(context.Background(), "api")
	if err != nil || len(routes) != 1 {
		t.Fatalf("routes %+v %v", routes, err)
	}
	if r := routes[0]; r.RouteCreated || r.DNSCreated || r.DNSPriorComment != teamComment {
		t.Fatalf("a later deploy must not turn an adopted rule or record into one Berthy created: %+v", r)
	}
}

func TestTakeOverCountsAsCreated(t *testing.T) {
	f := newIngressFixture(t)
	m := ingressManifest(t, "")
	f.applyIngress("apply", m)
	tunnelID := f.tunnel().ID
	cfg, err := json.Marshal(rulesOf(rule("api.example.com", "http://elsewhere:80"), rule("", "http_status:404")))
	if err != nil {
		t.Fatal(err)
	}
	f.cf.SetConfig(tunnelID, cfg)
	f.cf.AddRecord(f.zone, cloudflare.DNSRecord{Type: "CNAME", Name: "api.example.com", Content: "elsewhere.example.net", Proxied: true, Comment: teamComment})
	if op := f.run(Request{RequestID: "takeover", App: "api", Version: apiFirst, Manifest: m, ReplaceExisting: true}); op.State != "succeeded" {
		t.Fatalf("deploy %s: %s", op.State, op.Err)
	}
	routes, err := f.store.IngressRoutes(context.Background(), "api")
	if err != nil || len(routes) != 1 {
		t.Fatalf("routes %+v %v", routes, err)
	}
	if r := routes[0]; !r.RouteCreated || !r.DNSCreated || r.DNSPriorComment != "" {
		t.Fatalf("a rule and a record that replace foreign ones are Berthy's own: %+v", r)
	}
}

// removeAppRoutes plans and applies the removal of every route of app, as a removal does.
func removeAppRoutes(t *testing.T, f *ingressFixture, app string) IngressPlan {
	t.Helper()
	ctx := context.Background()
	op, _, err := f.store.Admit(ctx, state.Admission{RequestID: "remove-routes-" + t.Name(), ActorID: "admin", Kind: "remove", App: app, PublicRequest: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.store.Finish(ctx, op.ID, state.Outcome{State: "succeeded"}) }()
	sy, err := f.ctrl.publishSync(ctx, ingressManifest(t, ""), op.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	plan, cfg, steps, err := sy.plan(ctx, nil, ownedBy(app), false)
	if err != nil {
		t.Fatal(err)
	}
	if err = sy.apply(ctx, plan, cfg, steps); err != nil {
		t.Fatal(err)
	}
	return plan
}

func TestRemovalDeletesWhatBerthyCreated(t *testing.T) {
	f := newIngressFixture(t)
	m := ingressManifest(t, "")
	f.applyIngress("apply", m)
	tunnelID := f.tunnel().ID
	if op := f.deploy("deploy", "api", apiFirst, m); op.State != "succeeded" {
		t.Fatalf("deploy %s: %s", op.State, op.Err)
	}
	plan := removeAppRoutes(t, f, "api")
	if len(plan.Changes) != 1 || plan.Changes[0].Route != "remove" || plan.Changes[0].DNS != "remove" {
		t.Fatalf("plan %+v", plan.Changes)
	}
	if got := describeRules(f.tunnelConfig(tunnelID)); !reflect.DeepEqual(got, []string{"||http_status:404"}) {
		t.Fatalf("rules %v", got)
	}
	if records := f.cf.Records(f.zone); len(records) != 0 {
		t.Fatalf("records %+v", records)
	}
	if routes, err := f.store.IngressRoutes(context.Background(), "api"); err != nil || len(routes) != 0 {
		t.Fatalf("routes %+v %v", routes, err)
	}
}

func TestRemovalReleasesAdoptedRecordAndKeepsAdoptedRule(t *testing.T) {
	f, tunnelID, recordID := adoptedFixture(t)
	plan := removeAppRoutes(t, f, "api")
	if len(plan.Changes) != 1 || plan.Changes[0].Route != "release" || plan.Changes[0].DNS != "release" {
		t.Fatalf("an adopted rule and record are released, not removed: %+v", plan.Changes)
	}
	if got := describeRules(f.tunnelConfig(tunnelID)); !reflect.DeepEqual(got, []string{"api.example.com||" + apiTarget, "||http_status:404"}) {
		t.Fatalf("the adopted rule must stay in the tunnel: %v", got)
	}
	records := f.cf.Records(f.zone)
	if len(records) != 1 || records[0].ID != recordID || records[0].Comment != teamComment || records[0].Content != cloudflare.TunnelTarget(tunnelID) || !records[0].Proxied {
		t.Fatalf("the adopted record must stay with its old comment: %+v", records)
	}
	if routes, err := f.store.IngressRoutes(context.Background(), "api"); err != nil || len(routes) != 0 {
		t.Fatalf("Berthy's rows must go: %+v %v", routes, err)
	}
}

func TestReleasingARecordWithoutAnOldCommentClearsBerthysMark(t *testing.T) {
	f := newIngressFixture(t)
	m := ingressManifest(t, "")
	f.applyIngress("apply", m)
	tunnelID := f.tunnel().ID
	recordID := f.cf.AddRecord(f.zone, cloudflare.DNSRecord{Type: "CNAME", Name: "api.example.com", Content: cloudflare.TunnelTarget(tunnelID), Proxied: true})
	if op := f.deploy("deploy", "api", apiFirst, m); op.State != "succeeded" {
		t.Fatalf("deploy %s: %s", op.State, op.Err)
	}
	removeAppRoutes(t, f, "api")
	records := f.cf.Records(f.zone)
	if len(records) != 1 || records[0].ID != recordID || records[0].Comment != "" {
		t.Fatalf("the record had no comment and must have none again: %+v", records)
	}
}

func TestRemovalLeavesARecordThatLostBerthysMark(t *testing.T) {
	f, _, recordID := adoptedFixture(t)
	// Someone edited the record since: it is no longer marked, so Berthy must not touch it.
	zone := f.zone
	rec := f.cf.Records(zone)[0]
	rec.Comment = "edited by hand"
	if _, err := f.api.UpdateDNSRecord(context.Background(), zone, recordID, rec); err != nil {
		t.Fatal(err)
	}
	removeAppRoutes(t, f, "api")
	records := f.cf.Records(zone)
	if len(records) != 1 || records[0].Comment != "edited by hand" {
		t.Fatalf("records %+v", records)
	}
}

func TestMergeRoutesReleasesAnAdoptedRuleInsteadOfRemovingIt(t *testing.T) {
	adopted := map[string]state.IngressRoute{"api.example.com": {Hostname: "api.example.com", App: "api", Role: "web", Service: apiTarget, RouteCreated: false}}
	created := map[string]state.IngressRoute{"api.example.com": {Hostname: "api.example.com", App: "api", Role: "web", Service: apiTarget, RouteCreated: true}}
	cfg := rulesOf(rule("api.example.com", apiTarget), rule("other.example.com", "http://other:80"), rule("", "http_status:404"))
	removable := map[string]bool{"api.example.com": true}

	out, changes, conflicts := mergeRoutes(cfg, nil, adopted, removable, false)
	if len(conflicts) != 0 || len(changes) != 1 || changes[0].Route != "release" {
		t.Fatalf("adopted: %+v %v", changes, conflicts)
	}
	if got := describeRules(out); !reflect.DeepEqual(got, []string{"api.example.com||" + apiTarget, "other.example.com||http://other:80", "||http_status:404"}) {
		t.Fatalf("an adopted rule stays where it is: %v", got)
	}

	out, changes, conflicts = mergeRoutes(cfg, nil, created, removable, false)
	if len(conflicts) != 0 || len(changes) != 1 || changes[0].Route != "remove" {
		t.Fatalf("created: %+v %v", changes, conflicts)
	}
	if got := describeRules(out); !reflect.DeepEqual(got, []string{"other.example.com||http://other:80", "||http_status:404"}) {
		t.Fatalf("a created rule goes: %v", got)
	}
}

func TestIngressApplyKeepsRoutesOfADeployedAppMissingFromTheManifest(t *testing.T) {
	f := newIngressFixture(t)
	m := ingressManifest(t, "")
	f.applyIngress("apply", m)
	tunnelID := f.tunnel().ID
	if op := f.deploy("deploy", "api", apiFirst, m); op.State != "succeeded" {
		t.Fatalf("deploy %s: %s", op.State, op.Err)
	}
	// The manifest drops api while its services keep running.
	dropped := withIngress(t, `"apps":{"web":{"kind":"image","image":"ghcr.io/acme/web","roles":{"web":`+webRole()+`}}}`)
	plan, err := f.ctrl.Plan(context.Background(), Actor{ID: "admin", Admin: true}, Request{Kind: "ingress", Manifest: dropped, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	want := "app api is deployed but not in the manifest; run berthy remove api"
	if plan.Ingress == nil || !reflect.DeepEqual(plan.Ingress.Notes, []string{want}) {
		t.Fatalf("the dry run must say why the routes stay: %+v", plan.Ingress)
	}
	for _, ch := range plan.Ingress.Changes {
		if ch.Route == "remove" || ch.DNS == "remove" {
			t.Fatalf("the dry run would remove a deployed app's route: %+v", ch)
		}
	}
	if len(plan.Blockers) != 0 {
		t.Fatalf("the note is not a failure: %v", plan.Blockers)
	}
	if op := f.run(Request{RequestID: "apply-dropped", Kind: "ingress", Manifest: dropped}); op.State != "succeeded" {
		t.Fatalf("apply %s: %s", op.State, op.Err)
	}
	if got := describeRules(f.tunnelConfig(tunnelID)); !reflect.DeepEqual(got, []string{"api.example.com||" + apiTarget, "||http_status:404"}) {
		t.Fatalf("the route of a deployed app must stay: %v", got)
	}
	if records := f.cf.Records(f.zone); len(records) != 1 {
		t.Fatalf("the record must stay: %+v", records)
	}
	if routes, err := f.store.IngressRoutes(context.Background(), "api"); err != nil || len(routes) != 1 {
		t.Fatalf("routes %+v %v", routes, err)
	}
	events, err := f.store.EventsAfter(context.Background(), lastOperationID(t, f), 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, ev := range events {
		found = found || strings.Contains(string(ev.Data), want)
	}
	if !found {
		t.Fatalf("the real run must record the note too: %+v", events)
	}
}

func TestIngressApplyStillRemovesRoutesOfAnAppWithNoRelease(t *testing.T) {
	f := newIngressFixture(t)
	m := ingressManifest(t, "")
	f.applyIngress("apply", m)
	tunnelID := f.tunnel().ID
	if op := f.deploy("deploy", "api", apiFirst, m); op.State != "succeeded" {
		t.Fatalf("deploy %s: %s", op.State, op.Err)
	}
	// api has no applied release any more, as after berthy remove, though a row is left over.
	ctx := context.Background()
	op, _, err := f.store.Admit(ctx, state.Admission{RequestID: "forget", ActorID: "admin", Kind: "remove", App: "api", PublicRequest: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err = f.store.ForgetApp(ctx, op.ID, "api"); err != nil {
		t.Fatal(err)
	}
	if err = f.store.Finish(ctx, op.ID, state.Outcome{State: "succeeded"}); err != nil {
		t.Fatal(err)
	}
	dropped := withIngress(t, `"apps":{"web":{"kind":"image","image":"ghcr.io/acme/web","roles":{"web":`+webRole()+`}}}`)
	if applied := f.run(Request{RequestID: "apply-dropped", Kind: "ingress", Manifest: dropped}); applied.State != "succeeded" {
		t.Fatalf("apply %s: %s", applied.State, applied.Err)
	}
	if got := describeRules(f.tunnelConfig(tunnelID)); !reflect.DeepEqual(got, []string{"||http_status:404"}) {
		t.Fatalf("rules %v", got)
	}
}

// lastOperationID is the newest operation of the environment, found through the audit log.
func lastOperationID(t *testing.T, f *ingressFixture) string {
	t.Helper()
	audit, err := f.store.AuditLog(context.Background(), 1)
	if err != nil || len(audit) != 1 {
		t.Fatalf("audit %v %v", audit, err)
	}
	return audit[0].OperationID
}
