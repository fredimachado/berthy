package agent

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"berthy/internal/api"
	"berthy/internal/manifest"
	"berthy/internal/monitor"
	"berthy/internal/state"
)

// stateBackupManifest returns the backup rig's manifest with a state_backup block that keeps two snapshots under state/lab.
func stateBackupManifest(endpoint string) string {
	return `{"project":"demo","network":"demo",
	"apps":{"api":{"kind":"image","image":"ghcr.io/acme/api","roles":{"web":{}}}},
	"destinations":{"offsite":{"type":"s3","endpoint":"` + endpoint + `","bucket":"bkt","access_key_secret":"S3_ACCESS_KEY","secret_key_secret":"S3_SECRET_KEY"}},
	"state_backup":{"destination":"offsite","prefix":"state/lab","schedule":"0 0 1 1 *","keep":2}}`
}

func TestStateBackupCheckStatesFromItsHistory(t *testing.T) {
	sb := manifest.StateBackup{Destination: "offsite"} // the default schedule is 03:17 UTC daily
	day := func(d, h, m int) time.Time { return time.Date(2026, 11, d, h, m, 0, 0, time.UTC) }
	success := func(at time.Time) state.StateBackupHistory {
		run := &state.StateBackupRun{Status: "succeeded", Finished: at, Size: 2048}
		return state.StateBackupHistory{Last: run, Success: run}
	}
	cases := []struct {
		name      string
		h         state.StateBackupHistory
		firstSeen time.Time
		now       time.Time
		want      string
		detail    string
	}{
		{"never ran and recently seen", state.StateBackupHistory{}, day(16, 9, 0), day(16, 9, 30), monitor.Unknown, "not run yet"},
		{"recent success", success(day(16, 3, 17)), day(15, 10, 0), day(16, 5, 0), monitor.OK, "last snapshot 1h ago, 2.0 KiB"},
		{"success within the grace after the occurrence", success(day(15, 3, 17)), day(15, 10, 0), day(16, 4, 0), monitor.OK, "last snapshot 24h ago, 2.0 KiB"},
		{"overdue after the grace", success(day(15, 3, 17)), day(15, 10, 0), day(16, 6, 0), monitor.Critical, "overdue: last snapshot"},
		{"never succeeded and overdue", state.StateBackupHistory{}, day(15, 10, 0), day(16, 6, 0), monitor.Critical, "overdue: no snapshot yet"},
		{"newest run failed", state.StateBackupHistory{
			Last:    &state.StateBackupRun{Status: "failed", Detail: "upload failed: access denied"},
			Success: &state.StateBackupRun{Status: "succeeded", Finished: day(16, 3, 17), Size: 2048},
		}, day(15, 10, 0), day(16, 5, 0), monitor.Critical, "upload failed: access denied"},
		{"an interrupted run is not the last one", state.StateBackupHistory{Success: &state.StateBackupRun{Status: "succeeded", Finished: day(16, 3, 17), Size: 2048}},
			day(15, 10, 0), day(16, 5, 0), monitor.OK, "last snapshot 1h ago, 2.0 KiB"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := stateBackupResult(sb, tc.h, tc.firstSeen, tc.now)
			if res.State != tc.want || !strings.Contains(res.Detail, tc.detail) {
				t.Fatalf("got %v %q, want %v containing %q", res.State, res.Detail, tc.want, tc.detail)
			}
		})
	}
}

func TestStateBackupRunsOnDemandThroughTheAdminAPI(t *testing.T) {
	g := newBackupRig(t)
	if w := g.call("POST", "/v1/environments/lab/state-backup", adminToken, ""); w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "not configured") {
		t.Fatalf("without the block: %d %s", w.Code, w.Body.String())
	}
	recordAppRelease(t, g.store, "r-state", "api", stateBackupManifest(g.url))
	const base = "/v1/environments/lab/state-backup"
	for _, tc := range []struct {
		name, method, path, token string
		want                      int
	}{
		{"deploy cannot start it", "POST", base, deployToken, http.StatusForbidden},
		{"read cannot start it", "POST", base, readToken, http.StatusForbidden},
		{"start needs POST", "GET", base, adminToken, http.StatusMethodNotAllowed},
		{"read reads its runs", "GET", base + "/runs", readToken, http.StatusOK},
		{"runs limit is checked", "GET", base + "/runs?limit=0", readToken, http.StatusBadRequest},
	} {
		if w := g.call(tc.method, tc.path, tc.token, ""); w.Code != tc.want {
			t.Errorf("%s: %d %s", tc.name, w.Code, w.Body.String())
		}
	}
	w := g.call("POST", "/v1/environments/lab/state-backup", adminToken, "")
	var accepted struct {
		ID int64 `json:"state_backup_run_id"`
	}
	if w.Code != http.StatusAccepted || json.Unmarshal(w.Body.Bytes(), &accepted) != nil || accepted.ID == 0 {
		t.Fatalf("start: %d %s", w.Code, w.Body.String())
	}
	var run api.StateBackupRun
	deadline := time.Now().Add(10 * time.Second)
	for {
		runs := g.stateRuns()
		if len(runs) > 0 && runs[0].ID == accepted.ID && runs[0].Status != "running" {
			run = runs[0]
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("run %d did not finish: %+v", accepted.ID, runs)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if run.Status != "succeeded" || !strings.HasPrefix(run.Key, "state/lab/") || !strings.HasSuffix(run.Key, ".db") || run.Size == 0 || len(run.SHA256) != 64 || run.Trigger != "manual" {
		t.Fatalf("run: %+v", run)
	}
	// The outcome is audited just after the run record is finished, so the audit log is read until it has both steps.
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		audit, err := g.store.AuditLog(g.ctx, 20)
		if err != nil {
			t.Fatal(err)
		}
		outcomes := map[string]bool{}
		for _, a := range audit {
			if a.Action == "state_backup" {
				outcomes[a.Outcome] = true
			}
		}
		if outcomes["started"] && outcomes["succeeded"] {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("audit of the run: %+v", audit)
		}
	}
}

// stateRuns reads the state backup's runs through the API with the read token.
func (g *backupRig) stateRuns() []api.StateBackupRun {
	g.t.Helper()
	w := g.call("GET", "/v1/environments/lab/state-backup/runs?limit=5", readToken, "")
	var runs []api.StateBackupRun
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &runs) != nil {
		g.t.Fatalf("runs: %d %s", w.Code, w.Body.String())
	}
	return runs
}
