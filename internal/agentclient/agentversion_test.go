package agentclient

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"

	"berthy/internal/manifest"
	"berthy/internal/version"
)

// stampCLIVersion sets the CLI's version for the test and restores it afterwards.
func stampCLIVersion(t *testing.T, v string) {
	t.Helper()
	saved := version.Version
	version.Version = v
	t.Cleanup(func() { version.Version = saved })
}

// versionAgent answers the agent endpoint of the prod environment with agentVersion, and counts the requests it gets. An empty
// agentVersion answers without a version; a failing agent answers the endpoint with HTTP 500.
func versionAgent(t *testing.T, agentVersion string, failing bool) (*Target, *atomic.Int32) {
	t.Helper()
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if failing {
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, `{"code":"internal","message":"internal error"}`)
			return
		}
		fmt.Fprintf(w, `{"environment":"prod","hostname":"host","protected":false,"version":%q}`, agentVersion)
	}))
	t.Cleanup(server.Close)
	u, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	return &Target{Env: "prod", Base: u, Prefix: "/v1/environments/prod/", Token: "test-token"}, &requests
}

func manifestNeeding(minVersion string) manifest.Manifest {
	return manifest.Manifest{MinVersion: minVersion}
}

func TestAgentVersionReadsTheVersionTheAgentReports(t *testing.T) {
	target, _ := versionAgent(t, "v0.8.2", false)
	got, err := AgentVersion(context.Background(), NewHTTPClient(), target)
	if err != nil || got != "v0.8.2" {
		t.Fatalf("AgentVersion = %q, %v", got, err)
	}
}

func TestCheckAgentVersionRefusesAnOlderAgent(t *testing.T) {
	stampCLIVersion(t, "v0.9.0")
	target, _ := versionAgent(t, "v0.8.2", false)
	err := CheckAgentVersion(context.Background(), NewHTTPClient(), target, manifestNeeding("0.9.0"))
	want := "this manifest needs Berthy 0.9.0 or newer; the agent of prod runs v0.8.2: upgrade it with berthy setup"
	if err == nil || err.Error() != want {
		t.Fatalf("error %v, want %q", err, want)
	}
}

func TestCheckAgentVersionPassesWhatTheAgentMeets(t *testing.T) {
	stampCLIVersion(t, "v0.9.0")
	for _, agentVersion := range []string{"v0.9.0", "v1.0.0", "dev (abc1234)", "dev", ""} {
		target, _ := versionAgent(t, agentVersion, false)
		if err := CheckAgentVersion(context.Background(), NewHTTPClient(), target, manifestNeeding("0.9.0")); err != nil {
			t.Errorf("agent %q: %v", agentVersion, err)
		}
	}
}

func TestCheckAgentVersionAsksNothingWithoutAMinVersion(t *testing.T) {
	stampCLIVersion(t, "v0.9.0")
	target, requests := versionAgent(t, "v0.8.2", false)
	if err := CheckAgentVersion(context.Background(), NewHTTPClient(), target, manifestNeeding("")); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 0 {
		t.Fatalf("the agent got %d requests for a manifest without min_version", requests.Load())
	}
}

func TestCheckAgentVersionSkipsADevelopmentCLI(t *testing.T) {
	stampCLIVersion(t, "dev")
	target, requests := versionAgent(t, "v0.8.2", false)
	if err := CheckAgentVersion(context.Background(), NewHTTPClient(), target, manifestNeeding("99.0.0")); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 0 {
		t.Fatalf("a development CLI asked the agent %d times", requests.Load())
	}
}

func TestCheckAgentVersionPassesWhenTheLookupFails(t *testing.T) {
	stampCLIVersion(t, "v0.9.0")
	target, _ := versionAgent(t, "", true)
	if err := CheckAgentVersion(context.Background(), NewHTTPClient(), target, manifestNeeding("0.9.0")); err != nil {
		t.Fatalf("a failed lookup refused the manifest: %v", err)
	}
}

func TestOlderAgentHintNamesAnOlderReleaseOnly(t *testing.T) {
	const hint = "the agent runs an older release than this CLI; upgrade it with berthy setup"
	stampCLIVersion(t, "v0.9.0")
	for _, tc := range []struct{ agentVersion, want string }{
		{"v0.8.0", hint},
		{"v0.9.0", ""},
		{"v1.0.0", ""},
		{"dev (abc1234)", ""},
		{"", ""},
	} {
		target, _ := versionAgent(t, tc.agentVersion, false)
		if got := OlderAgentHint(context.Background(), NewHTTPClient(), target); got != tc.want {
			t.Errorf("agent %q: hint %q, want %q", tc.agentVersion, got, tc.want)
		}
	}
}

func TestOlderAgentHintIsEmptyForADevelopmentCLI(t *testing.T) {
	stampCLIVersion(t, "dev")
	target, requests := versionAgent(t, "v0.8.0", false)
	if got := OlderAgentHint(context.Background(), NewHTTPClient(), target); got != "" || requests.Load() != 0 {
		t.Fatalf("hint %q after %d requests", got, requests.Load())
	}
}

func TestInvalidRequestIsTheAgentsDecodeRefusal(t *testing.T) {
	if !InvalidRequest(http.StatusBadRequest, []byte(`{"code":"bad_request","message":"invalid request"}`)) {
		t.Error("the decode refusal was not recognized")
	}
	for _, tc := range []struct {
		status int
		body   string
	}{
		{http.StatusOK, `{"code":"bad_request","message":"invalid request"}`},
		{http.StatusBadRequest, `{"code":"operation_failed","message":"invalid request"}`},
		{http.StatusBadRequest, `{"code":"bad_request","message":"this manifest needs Berthy 1.0.0"}`},
		{http.StatusBadRequest, `not json`},
	} {
		if InvalidRequest(tc.status, []byte(tc.body)) {
			t.Errorf("status %d body %s recognized as a decode refusal", tc.status, tc.body)
		}
	}
}
