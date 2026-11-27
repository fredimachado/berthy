package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"berthy/internal/state"
)

// gateRow is one kind of change the gate plans.
type gateRow struct {
	method, path string
	sample       string
	fixture      func(t *testing.T) *approvalFixture
	setup        func(f *approvalFixture)
	watched      func(f *approvalFixture) string
}

func TestPlanningChangesNothingWhateverTheBody(t *testing.T) {
	fixture := func(t *testing.T) *approvalFixture { return newApprovalFixture(t, true, nil) }
	nothing := func(*approvalFixture) {}
	lock := func(f *approvalFixture) string {
		v, err := f.s.Lock(context.Background())
		if err != nil {
			return "unlocked"
		}
		return fmt.Sprintf("%+v", v)
	}
	secrets := func(f *approvalFixture) string {
		v, err := f.s.Secrets(context.Background())
		if err != nil {
			f.t.Fatal(err)
		}
		return fmt.Sprint(v)
	}
	settings := func(f *approvalFixture) string {
		v, ok, err := f.s.Settings(context.Background())
		if err != nil {
			f.t.Fatal(err)
		}
		return fmt.Sprintf("%t %s", ok, v.Manifest)
	}
	cancelState := func(f *approvalFixture) string {
		op, err := f.s.Active(context.Background())
		if err != nil {
			return fmt.Sprint("no active operation: ", err, f.audit())
		}
		var cancels []string
		for _, a := range f.audit() {
			if strings.Contains(a, " cancel:"+op.ID+" ") {
				cancels = append(cancels, a)
			}
		}
		return fmt.Sprint(op.State, cancels)
	}
	rows := map[string]gateRow{
		"lock acquire": {"POST", "/lock/acquire", `{"message":"m"}`, fixture, nothing, lock},
		"lock release": {"POST", "/lock/release", `{}`, fixture, func(f *approvalFixture) {
			if err := f.s.AcquireManual(context.Background(), state.Actor{ID: "human", Name: "human"}, "human", "held"); err != nil {
				f.t.Fatal(err)
			}
		}, lock},
		"secret set":               {"POST", "/secrets", `{"name":"NEW","value":"v"}`, fixture, nothing, secrets},
		"secret import":            {"POST", "/secrets/import", `{"secrets":[{"name":"IMP","value":"v"}]}`, fixture, nothing, secrets},
		"secret generate":          {"POST", "/secrets/generate", `{"name":"GEN"}`, fixture, nothing, secrets},
		"secret generate declared": {"POST", "/secrets/generate-declared", `{"manifest":{"project":"demo","network":"overlay","apps":{"api":{"kind":"image","image":"ghcr.io/acme/api","roles":{"web":{}},"env":{"secret":["DB_PASS"]}}},"secrets":{"DB_PASS":{"generate":{"format":"hex"}}}}}`, fixture, func(f *approvalFixture) { f.ctrl.Engine = generateEngine{} }, secrets},
		"apply":                    {"POST", "/apply", applyBody(t, `{"project":"demo","network":"overlay","apps":{"api":{"kind":"image","image":"ghcr.io/acme/api","roles":{"web":{}}}},"probes":{"site":{"http":"http://127.0.0.1:8080/"}}}`, false), fixture, nothing, settings},
		"secret delete": {"DELETE", "/secrets/KEEP", `{}`, fixture, func(f *approvalFixture) {
			if err := f.s.PutSecret(context.Background(), state.Actor{ID: "human", Name: "human"}, state.SecretInfo{Name: "KEEP"}, []byte("v")); err != nil {
				f.t.Fatal(err)
			}
		}, secrets},
		"cancel": {"POST", "/operations/{id}/cancel", `{}`, fixture, func(f *approvalFixture) { runningRun(f.t, f) }, cancelState},
		"recovery resolve": {"POST", "/operations/{id}/resolve", `{"reason":"gone"}`, fixture, func(f *approvalFixture) {
			op, _, err := f.s.Admit(context.Background(), state.Admission{RequestID: "stuck", ActorID: "a", Kind: "deploy", App: "api", PublicRequest: []byte(`{}`)})
			if err != nil {
				f.t.Fatal(err)
			}
			if err := f.s.Finish(context.Background(), op.ID, state.Outcome{State: "recovery_required", Err: "service recovery required"}); err != nil {
				f.t.Fatal(err)
			}
		}, func(f *approvalFixture) string {
			op, err := f.s.Active(context.Background())
			if err != nil {
				return "resolved"
			}
			return op.State
		}},
	}
	for _, kind := range dryRunKinds {
		row, ok := rows[kind]
		if !ok {
			t.Errorf("no row for %s", kind)
			continue
		}
		t.Run(kind, func(t *testing.T) {
			f := row.fixture(t)
			row.setup(f)
			path := row.path
			if strings.Contains(path, "{id}") {
				op, err := f.s.Active(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				path = strings.ReplaceAll(path, "{id}", op.ID)
			}
			before, audit0 := row.watched(f), f.audit()
			check := func(body string) {
				t.Helper()
				if got := row.watched(f); got != before {
					t.Fatalf("body %q changed state: before %q after %q", body, before, got)
				}
				audit := f.audit()
				if len(audit) < len(audit0) {
					t.Fatalf("audit lost rows: %v", audit)
				}
				for _, line := range audit[len(audit0):] {
					if !strings.Contains(line, " approval_request:") {
						t.Fatalf("body %q changed audit: %s", body, line)
					}
				}
			}
			for _, body := range []string{"", "null", "{}"} {
				f.call(row.method, path, apAgentAdmin, body)
				check(body)
			}
			w := f.call(row.method, path, apAgentAdmin, row.sample)
			var h held
			if w.Code != http.StatusAccepted || w.Header().Get("Berthy-Approval") != "pending" || json.Unmarshal(w.Body.Bytes(), &h) != nil || h.ID == "" || !json.Valid(h.Plan) || sameJSON(t, h.Plan, mustDescribe(t, path, row.method)) {
				t.Fatalf("sample plan: %d %s", w.Code, w.Body.String())
			}
			check(row.sample)
			w = f.call("POST", "/approvals/"+h.ID+"/approve", apAdminToken, "")
			if w.Code < 200 || w.Code >= 300 {
				t.Fatalf("approve: %d %s", w.Code, w.Body.String())
			}
			if kind == "cancel" {
				f.waitTerminal(strings.Split(path, "/")[2])
			}
			if after := row.watched(f); after == before {
				t.Fatalf("approval did not change state: %q", after)
			}
		})
	}
}

func mustDescribe(t *testing.T, path, method string) []byte {
	t.Helper()
	b, err := json.Marshal(describe(restOf(strings.TrimPrefix(path, "/")), method))
	if err != nil {
		t.Fatal(err)
	}
	return b
}
