package agent

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestEventsURLIsUnderTheProjectPath(t *testing.T) {
	f := newApprovalFixture(t, false, func(c *Config) { c.Project = "demo" })
	// api was deployed and then removed, so removing it again is a plan that needs no Docker.
	recordAppRelease(t, f.s, "api", "api", apManifestSource)
	forgetApp(t, f.s, "api")
	w := f.call("POST", "/remove", apAdminToken, `{"app":"api","request_id":"rm-1"}`)
	var accepted struct {
		OperationID string `json:"operation_id"`
		EventsURL   string `json:"events_url"`
	}
	if w.Code != http.StatusAccepted || json.Unmarshal(w.Body.Bytes(), &accepted) != nil {
		t.Fatalf("remove: %d %s", w.Code, w.Body.String())
	}
	want := "/v1/projects/demo/environments/lab/operations/" + accepted.OperationID + "/events"
	if accepted.EventsURL != want || w.Header().Get("Location") != want {
		t.Fatalf("events_url %q, Location %q, want %q", accepted.EventsURL, w.Header().Get("Location"), want)
	}
	// The URL the agent hands out is one it serves.
	events := f.call("GET", "/operations/"+accepted.OperationID+"/events", apReadToken, "")
	if events.Code != http.StatusOK || !strings.Contains(events.Body.String(), "event: terminal") {
		t.Fatalf("events: %d %s", events.Code, events.Body.String())
	}
}

func TestAgentInfoNamesTheProject(t *testing.T) {
	f := newApprovalFixture(t, false, func(c *Config) { c.Project = "demo" })
	w := f.call("GET", "/agent", apReadToken, "")
	var info struct {
		Project     string `json:"project"`
		Environment string `json:"environment"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &info) != nil || info.Project != "demo" || info.Environment != "lab" {
		t.Fatalf("agent: %d %s", w.Code, w.Body.String())
	}
}
