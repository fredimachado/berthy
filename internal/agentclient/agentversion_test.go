package agentclient

import (
	"berthy/internal/dialtest"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
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
	return &Target{Env: "prod", Base: u, Prefix: "/v1/projects/demo/environments/prod/", Token: "test-token"}, &requests
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

func TestAgentProtectionRequiresUnambiguousScopedCapability(t *testing.T) {
	const prefix = `/v1/projects/demo/environments/prod/`
	const token = `fixture-token`
	cases := []struct {
		name, body string
		status     int
		want       Protection
		fail       bool
	}{
		{"legacy unprotected", `{"project":"demo","environment":"prod","protected":false,"extra":{"x":1}}`, 200, ProtectionLegacyUnprotected, false},
		{"legacy protected", `{"project":"demo","environment":"prod","protected":true}`, 200, ProtectionLegacyProtected, false},
		{"effective unprotected", `{"project":"demo","environment":"prod","protected":false,"effective_protected":false}`, 200, ProtectionUnprotected, false},
		{"effective protected", `{"project":"demo","environment":"prod","protected":true,"effective_protected":true}`, 200, ProtectionProtected, false},
		{"sticky protected", `{"project":"demo","environment":"prod","protected":false,"effective_protected":true}`, 200, ProtectionProtected, false},
		{"contradiction", `{"project":"demo","environment":"prod","protected":true,"effective_protected":false}`, 200, ProtectionUnknown, true},
		{"null effective", `{"project":"demo","environment":"prod","protected":false,"effective_protected":null}`, 200, ProtectionUnknown, true},
		{"invalid effective", `{"project":"demo","environment":"prod","protected":false,"effective_protected":"false"}`, 200, ProtectionUnknown, true},
		{"missing protected", `{"project":"demo","environment":"prod"}`, 200, ProtectionUnknown, true},
		{"null protected", `{"project":"demo","environment":"prod","protected":null}`, 200, ProtectionUnknown, true},
		{"invalid protected", `{"project":"demo","environment":"prod","protected":0}`, 200, ProtectionUnknown, true},
		{"wrong project", `{"project":"other","environment":"prod","protected":false}`, 200, ProtectionUnknown, true},
		{"wrong environment", `{"project":"demo","environment":"other","protected":false}`, 200, ProtectionUnknown, true},
		{"missing identity", `{"environment":"prod","protected":false}`, 200, ProtectionUnknown, true},
		{"null identity", `{"project":null,"environment":"prod","protected":false}`, 200, ProtectionUnknown, true},
		{"duplicate project", `{"project":"demo","project":"demo","environment":"prod","protected":false}`, 200, ProtectionUnknown, true},
		{"variant environment", `{"project":"demo","Environment":"prod","protected":false}`, 200, ProtectionUnknown, true},
		{"duplicate protected", `{"project":"demo","environment":"prod","protected":false,"protected":true}`, 200, ProtectionUnknown, true},
		{"variant effective", `{"project":"demo","environment":"prod","protected":false,"Effective_Protected":false}`, 200, ProtectionUnknown, true},
		{"nonobject", `[]`, 200, ProtectionUnknown, true},
		{"truncated", `{"project":"demo"`, 200, ProtectionUnknown, true},
		{"trailing json", `{"project":"demo","environment":"prod","protected":false} {}`, 200, ProtectionUnknown, true},
		{"redirect", `{}`, 300, ProtectionUnknown, true},
		{"unauthorized", `{}`, 401, ProtectionUnknown, true},
		{"forbidden", `{}`, 403, ProtectionUnknown, true},
		{"not found", `{}`, 404, ProtectionUnknown, true},
		{"server error", `{}`, 500, ProtectionUnknown, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" || (r.URL.Path != prefix+"agent" && !(tc.status == 404 && r.URL.Path == "/v1/environments/prod/agent")) || r.URL.RawQuery != "" || r.Header.Get("Authorization") != "Bearer "+token {
					t.Errorf("request binding incorrect")
				}
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			defer server.Close()
			u, _ := url.Parse(server.URL)
			got, err := AgentProtection(context.Background(), NewHTTPClient(), &Target{Project: "demo", Env: "prod", Base: u, Prefix: prefix, Token: token})
			if got != tc.want || (err != nil) != tc.fail {
				t.Errorf("got %v, error %v; want %v, error %v", got, err, tc.want, tc.fail)
			}
		})
	}
	t.Run("transport", func(t *testing.T) {
		u, _ := url.Parse("http://" + dialtest.Refused)
		got, err := AgentProtection(context.Background(), dialtest.Client(), &Target{Project: "demo", Env: "prod", Base: u, Prefix: prefix, Token: token})
		if got != ProtectionUnknown || err == nil {
			t.Fatalf("got %v, %v", got, err)
		}
	})
}

func TestAgentProtectionRejectsRedirectWithoutChangingClient(t *testing.T) {
	for _, policy := range []string{"default", "permissive"} {
		t.Run(policy, func(t *testing.T) {
			var followed atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/elsewhere" {
					followed.Add(1)
					fmt.Fprint(w, `{"project":"demo","environment":"prod","protected":false,"version":"v0.5.0"}`)
					return
				}
				http.Redirect(w, r, "/elsewhere", http.StatusFound)
			}))
			defer server.Close()
			client := &http.Client{}
			if policy == "permissive" {
				client.CheckRedirect = func(*http.Request, []*http.Request) error { return nil }
			}
			u, _ := url.Parse(server.URL)
			target := &Target{Project: "demo", Env: "prod", Base: u, Prefix: "/v1/projects/demo/environments/prod/", Token: "fixture-token"}
			got, err := AgentProtection(context.Background(), client, target)
			if got != ProtectionUnknown || err == nil {
				t.Errorf("AgentProtection = %v, %v; want unknown and error", got, err)
			}
			if followed.Load() != 0 {
				t.Errorf("followed redirect %d times", followed.Load())
			}
			before := followed.Load()
			version, err := AgentVersion(context.Background(), client, target)
			if err != nil || version != "v0.5.0" || followed.Load() != before+1 {
				t.Errorf("caller redirect behavior changed: version %q, error %v, follows %d", version, err, followed.Load())
			}
		})
	}
}

func TestAgentProtectionRejectsInvalidUTF8(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "{\"project\":\"demo\",\"environment\":\"prod\",\"protected\":false,\"effective_protected\xff\":false}")
	}))
	defer server.Close()
	u, _ := url.Parse(server.URL)
	got, err := AgentProtection(context.Background(), NewHTTPClient(), &Target{Project: "demo", Env: "prod", Base: u, Prefix: "/v1/projects/demo/environments/prod/", Token: "fixture-token"})
	if got != ProtectionUnknown || err == nil {
		t.Fatalf("AgentProtection = %v, %v; want unknown and error", got, err)
	}
}

func TestCheckAgentVersionRedactsBearerToken(t *testing.T) {
	stampCLIVersion(t, "v0.9.0")
	target, _ := versionAgent(t, "v0.8.2", false)
	target.Token = "v0.8.2"
	err := CheckAgentVersion(context.Background(), NewHTTPClient(), target, manifestNeeding("0.9.0"))
	if err == nil || strings.Contains(err.Error(), target.Token) {
		t.Fatal("expected a redacted minimum-version diagnostic")
	}
}

func TestAgentProtectionPreservesSafeRefusal(t *testing.T) {
	const prefix = "/v1/projects/demo/environments/prod/"
	const token = "fixture-secret-token"
	cases := []struct {
		name      string
		status    int
		body      string
		oldStatus int
		want      string
	}{
		{"pre-project agent", 404, `{}`, 200, "agent returned HTTP 404: " + OlderAgentMessage},
		{"ordinary not found", 404, `{}`, 404, "agent returned HTTP 404"},
		{"redacted credential", 403, `{"message":"credential ` + token + ` refused"}`, 0, "agent returned HTTP 403: credential [redacted] refused"},
		{"control character", 500, `{"message":"unsafe\nmessage"}`, 0, "agent returned HTTP 500"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var scoped, fallback atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer "+token || r.URL.RawQuery != "" {
					t.Error("request binding incorrect")
				}
				switch r.URL.Path {
				case prefix + "agent":
					scoped.Add(1)
					w.WriteHeader(tc.status)
					fmt.Fprint(w, tc.body)
				case "/v1/environments/prod/agent":
					fallback.Add(1)
					w.WriteHeader(tc.oldStatus)
				default:
					t.Error("unexpected request path")
					w.WriteHeader(http.StatusInternalServerError)
				}
			}))
			defer server.Close()
			u, err := url.Parse(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			got, err := AgentProtection(context.Background(), NewHTTPClient(), &Target{Project: "demo", Env: "prod", Base: u, Prefix: prefix, Token: token})
			if got != ProtectionUnknown || err == nil || err.Error() != tc.want {
				t.Errorf("got %v, %v; want unknown, %q", got, err, tc.want)
			}
			wantFallback := int32(0)
			if tc.status == 404 {
				wantFallback = 1
			}
			if scoped.Load() != 1 || fallback.Load() != wantFallback {
				t.Errorf("requests: scoped %d, fallback %d; want 1, %d", scoped.Load(), fallback.Load(), wantFallback)
			}
		})
	}
}
