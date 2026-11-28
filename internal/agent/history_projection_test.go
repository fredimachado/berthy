package agent

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"berthy/internal/api"
	"berthy/internal/engine"
	"berthy/internal/logs"
	"berthy/internal/state"
)

func seedArchivedReader(t *testing.T, f *approvalFixture) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(f.dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS release_facts (environment TEXT, app TEXT, kind TEXT, value TEXT, PRIMARY KEY(environment,app,kind,value));
		INSERT INTO release_facts VALUES ('lab','zeta','secret','PASSWORD'), ('lab','api','secret','PASSWORD'), ('lab','api','variable','OLD_PASSWORD')`); err != nil {
		t.Fatal(err)
	}
}

func TestManagedLogsRedactAnEvictedVariable(t *testing.T) {
	f := newApprovalFixture(t, false, nil)
	recordBoundRelease(t, f.s, "kept", monitoredManifest(""), []state.Binding{{App: "api", Role: "web", Name: "demo-api-web", ServiceID: "service1"}})
	seedArchivedReader(t, f)
	putPlainSecret(t, f.s, "PASSWORD", "new-value")
	app := api.App{Ref: api.AppRef{Kind: "image", Name: "api"}}
	source := fakeManagedLogs{service: engine.ServiceSnapshot{ID: "service1"}, env: []string{"OLD_PASSWORD=old-value"}}
	values, err := managedLogSecrets(t.Context(), f.ctrl, source, app, []logs.Identity{{Role: "web", ContainerID: "stopped"}})
	if err != nil {
		t.Fatal(err)
	}
	rows := redactRecords([]logs.Record{{Message: "old-value new-value"}}, values, nil)
	if rows[0].Message != "[REDACTED] [REDACTED]" {
		t.Fatal("managed logs exposed an archived variable or its current stored value")
	}
}

func TestGenerateRemembersAnEvictedSecretReader(t *testing.T) {
	f := newApprovalFixture(t, false, nil)
	recordRelease(t, f.s, "kept", monitoredManifest(""))
	seedArchivedReader(t, f)
	for _, removed := range []bool{false, true} {
		if removed {
			forgetApp(t, f.s, "api")
		}
		w := f.call("POST", "/secrets/generate", apAdminToken, `{"name":"PASSWORD"}`)
		if w.Code != 409 || !strings.Contains(w.Body.String(), "app api was deployed here and may hold the value of PASSWORD") {
			t.Fatalf("removed=%t: generation returned %d, want historical-reader conflict", removed, w.Code)
		}
		if _, ok := storedValue(t, f, "PASSWORD"); ok {
			t.Fatal("refused generation stored a secret")
		}
	}
	if w := f.call("POST", "/secrets/generate", apAdminToken, `{"name":"UNRELATED"}`); w.Code != 200 {
		t.Fatalf("unrelated generation returned %d", w.Code)
	}
}
