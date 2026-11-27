package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"berthy/internal/cloudflare/cftest"
	"berthy/internal/engine"
	"berthy/internal/manifest"
	"berthy/internal/state"
)

type gateEngine struct{ service bool }

var errGateEngine = errors.New("unexpected engine call")

func (e gateEngine) InspectService(_ context.Context, name string) (engine.ServiceSnapshot, error) {
	if !e.service || name != "demo-api-web" {
		return engine.ServiceSnapshot{}, &engine.StatusError{Code: 404}
	}
	return engine.ServiceSnapshot{ID: "service1", Version: 1, Spec: json.RawMessage(`{"Name":"demo-api-web","TaskTemplate":{"ContainerSpec":{"Image":"ghcr.io/acme/api:v1","Labels":{"berthy.operation":"g1"}}},"Mode":{"Replicated":{"Replicas":1}}}`)}, nil
}
func (gateEngine) CreateService(context.Context, json.RawMessage, *engine.RegistryAuth) (string, error) {
	return "", errGateEngine
}
func (gateEngine) UpdateService(context.Context, string, uint64, json.RawMessage, *engine.RegistryAuth) error {
	return errGateEngine
}
func (gateEngine) DeleteService(context.Context, string) error { return errGateEngine }
func (gateEngine) InspectNetwork(_ context.Context, name string) (engine.Network, error) {
	return engine.Network{ID: "net", Name: name, Driver: "overlay", Scope: "swarm", Attachable: true}, nil
}
func (gateEngine) ResolveImage(context.Context, string, *engine.RegistryAuth) (string, error) {
	return "", errGateEngine
}
func (gateEngine) PullImage(context.Context, string, *engine.RegistryAuth) error {
	return errGateEngine
}
func (gateEngine) InspectImage(context.Context, string) (engine.Image, error) {
	return engine.Image{}, errGateEngine
}
func (gateEngine) ServiceTasks(context.Context, string) ([]engine.ServiceTask, error) {
	return []engine.ServiceTask{{ServiceID: "service1", State: "running", DesiredState: "running", Slot: 1, ContainerID: "c1", Labels: map[string]string{"berthy.operation": "g1"}}}, nil
}
func (gateEngine) InspectContainerHealth(context.Context, string) (string, error) {
	return "healthy", nil
}
func (gateEngine) InspectContainer(context.Context, string) (engine.ContainerSnapshot, error) {
	return engine.ContainerSnapshot{ID: "c1", Running: true, Labels: map[string]string{"berthy.operation": "g1"}}, nil
}
func (gateEngine) Exec(context.Context, string, []string) (engine.ExecResult, error) {
	return engine.ExecResult{}, errGateEngine
}
func (gateEngine) InventoryImages(context.Context) ([]engine.InventoryImage, error) { return nil, nil }
func (gateEngine) InventoryContainers(context.Context) ([]engine.InventoryContainer, error) {
	return nil, nil
}
func (gateEngine) InventoryServices(context.Context) ([]engine.InventoryService, error) {
	return nil, nil
}
func (gateEngine) RemoveImageRef(context.Context, string) error  { return errGateEngine }
func (gateEngine) RemoveContainer(context.Context, string) error { return errGateEngine }

func seedGateRelease(t *testing.T, s *state.Store, id, digest string) {
	t.Helper()
	ctx := context.Background()
	op, _, err := s.Admit(ctx, state.Admission{RequestID: id, ActorID: "seed", Kind: "deploy", App: "api", PublicRequest: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	rel := &state.Release{App: "api", Digest: digest, Tag: id, Manifest: json.RawMessage(apManifestSource), Bindings: []state.Binding{{App: "api", Role: "web", Name: "demo-api-web", ServiceID: "service1"}}}
	if err := s.Finish(ctx, op.ID, state.Outcome{State: "succeeded", Release: rel}); err != nil {
		t.Fatal(err)
	}
}

// gateRow is one kind of change the gate plans.
type gateRow struct {
	method, path string
	sample       string
	fixture      func(t *testing.T) *approvalFixture
	setup        func(f *approvalFixture)
	watched      func(f *approvalFixture) string
}

func gateRows(t *testing.T, protected bool) map[string]gateRow {
	fixture := func(t *testing.T) *approvalFixture {
		f := newApprovalFixture(t, protected, nil)
		ctrl := f.ctrl
		t.Cleanup(func() { _ = ctrl.Drain(context.Background()) })
		return f
	}
	nothing := func(*approvalFixture) {}
	lock := func(f *approvalFixture) string {
		v, err := f.s.Lock(context.Background())
		if errors.Is(err, sql.ErrNoRows) {
			return "unlocked"
		}
		if err != nil {
			f.t.Fatal(err)
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
	operation := func(id string) func(*approvalFixture) string {
		return func(f *approvalFixture) string {
			_, err := f.s.AdmissionForRequest(context.Background(), id, "", [32]byte{})
			if errors.Is(err, sql.ErrNoRows) {
				return "absent"
			}
			if errors.Is(err, state.ErrConflict) || err == nil {
				return "admitted"
			}
			f.t.Fatal(err)
			return ""
		}
	}
	seed := func(f *approvalFixture) {
		seedGateRelease(f.t, f.s, "seed1", apRunDigest)
		f.ctrl.Engine = gateEngine{service: true}
	}
	cancelState := func(f *approvalFixture) string {
		op, err := f.s.Active(context.Background())
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Sprint("no active operation: ", err, f.audit())
		}
		if err != nil {
			f.t.Fatal(err)
		}
		var cancels []string
		for _, a := range f.audit() {
			if strings.Contains(a, " cancel:"+op.ID+" ") {
				cancels = append(cancels, a)
			}
		}
		return fmt.Sprint(op.State, cancels)
	}
	return map[string]gateRow{
		"deploy": {"POST", "/deploys", deployBody(t, false), fixture, func(f *approvalFixture) { f.ctrl.Engine = gateEngine{} }, operation("req-1")},
		"rollback": {"POST", "/rollback", `{"app":"api","request_id":"r"}`, fixture, func(f *approvalFixture) {
			seed(f)
			seedGateRelease(f.t, f.s, "seed2", "ghcr.io/acme/api@sha256:"+strings.Repeat("b", 64))
		}, operation("r")},
		"restart": {"POST", "/restart", `{"app":"api","role":"web","request_id":"r"}`, fixture, seed, operation("r")},
		"scale":   {"POST", "/scale", `{"app":"api","role":"web","replicas":2,"request_id":"r"}`, fixture, seed, operation("r")},
		"exec":    {"POST", "/exec", `{"app":"api","role":"web","command":["true"],"request_id":"r"}`, fixture, seed, operation("r")},
		"run": {"POST", "/run", `{"app":"api","command":["true"],"request_id":"r"}`, fixture, func(f *approvalFixture) {
			seedRunApp(f.t, f.s)
			e := &runEngine{started: make(chan struct{}), release: make(chan struct{})}
			f.ctrl.Engine = e
			f.t.Cleanup(func() { e.once.Do(func() { close(e.release) }) })
		}, operation("r")},
		"prune":   {"POST", "/prune", `{"request_id":"r"}`, fixture, func(f *approvalFixture) { f.ctrl.Engine = gateEngine{} }, operation("r")},
		"remove":  {"POST", "/remove", `{"app":"api","request_id":"r"}`, fixture, seed, operation("r")},
		"destroy": {"POST", "/destroy", `{"request_id":"r","confirm":"demo/lab"}`, fixture, func(f *approvalFixture) { f.ctrl.Engine = gateEngine{} }, operation("r")},
		"restore": {"POST", "/restore", `{"key":"demo/lab/api/uploads/uploads/20261116T120000Z-1.tar.gz","into":"restored-1","request_id":"r"}`, func(t *testing.T) *approvalFixture {
			f := fixture(t)
			g := newBackupRig(t)
			g.backups.Runner.Now = func() time.Time { return apStartTime }
			m, err := manifest.DecodeJSON([]byte(backupManifest(g.url)))
			if err != nil {
				t.Fatal(err)
			}
			result, err := g.backups.Runner.Run(context.Background(), *m, "uploads", "manual")
			if err != nil || result.Run.Status != "succeeded" || result.Key != "demo/lab/api/uploads/uploads/20261116T120000Z-1.tar.gz" {
				t.Fatalf("seed backup: %+v %v", result, err)
			}
			f.s, f.ctrl = g.store, g.ctrl
			f.ctrl.Protected = protected
			f.h = NewDeploymentHandlerWithBackups(f.cfg, nil, nil, f.ctrl, nil, nil, g.backups)
			t.Cleanup(func() { _ = g.ctrl.Drain(context.Background()) })
			return f
		}, nothing, operation("r")},
		"ingress apply": {"POST", "/ingress/apply", strings.Replace(string(ingressTestRequest(t, false)), `"dry_run":true`, `"dry_run":false`, 1), fixture, func(f *approvalFixture) {
			if err := f.s.PutSecret(context.Background(), state.Actor{ID: "human", Name: "human"}, state.SecretInfo{Name: "CF_TOKEN"}, []byte(ingressTestCFToken)); err != nil {
				f.t.Fatal(err)
			}
			fake := cftest.New(ingressTestCFToken, "acct")
			fake.AddZone("example.com")
			srv := httptest.NewServer(fake.Handler())
			f.t.Cleanup(srv.Close)
			f.ctrl.CloudflareURL = srv.URL + "/client/v4"
			f.ctrl.Engine = gateEngine{}
			f.t.Cleanup(func() { _ = f.ctrl.Drain(context.Background()) })
		}, operation("ingress-test")},
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
			if errors.Is(err, sql.ErrNoRows) {
				return "resolved"
			}
			if err != nil {
				f.t.Fatal(err)
			}
			return op.State
		}},
	}

}

func TestPlanningChangesNothingWhateverTheBody(t *testing.T) {
	rows := gateRows(t, true)
	for _, kind := range dryRunKinds {
		row, ok := rows[kind]
		if !ok {
			t.Fatalf("no row for %s", kind)
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
				if stream := f.waitTerminal(strings.Split(path, "/")[2]); !strings.Contains(stream, "event: terminal") {
					t.Fatalf("missing terminal: %s", stream)
				}
			} else if strings.HasPrefix(row.watched(f), "admitted") {
				var answer struct {
					ID string `json:"operation_id"`
				}
				if json.Unmarshal(w.Body.Bytes(), &answer) != nil || answer.ID == "" {
					t.Fatalf("approval has no operation: %s", w.Body.String())
				}
				if e, ok := f.ctrl.Engine.(*runEngine); ok {
					e.once.Do(func() { close(e.release) })
				}
				if stream := f.waitTerminal(answer.ID); !strings.Contains(stream, "event: terminal") {
					t.Fatalf("missing terminal: %s", stream)
				}
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
