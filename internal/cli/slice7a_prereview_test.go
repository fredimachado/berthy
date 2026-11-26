package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestRegistryDiagnosticSpansPreserveProtocol(t *testing.T) {
	for _, tc := range []struct{ name, input, want string }{
		{"repeated", `{"z":123456789012345678901234567890,"services_error":"long\u002dtoken token","services_error":"safe","Registry":"token","SERVICES":[{"Error":"token","name":"long-token"}],"x":true}`, `{"z":123456789012345678901234567890,"services_error":"[redacted] [redacted]","services_error":"safe","Registry":"[redacted]","SERVICES":[{"Error":"[redacted]","name":"[redacted]"}],"x":true}`},
		{"safe first", `{"services_error":"safe","services_error":"token"}`, `{"services_error":"safe","services_error":"[redacted]"}`},
		{"trailing", `{"services_error":"token"} garbage`, `{"services_error":"[redacted]"} garbage`},
		{"safe bytes", `{"z":1e+100,"services_error":"fine","z":2}`, `{"z":1e+100,"services_error":"fine","z":2}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := string(redactRegistryJSON([]byte(tc.input), "token", "long-token")); got != tc.want {
				t.Error("registry diagnostic rewrite changed protocol bytes or exposed a credential")
			}
		})
	}
}

func TestDrySecretRegistryDiagnostic(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"action":"secret set","exists":false,"services":["test-token-web"],"services_error":"test-token"}`)
	}))
	defer server.Close()
	base, _ := url.Parse(server.URL)
	for _, asJSON := range []bool{false, true} {
		var out, errs bytes.Buffer
		code := sendCommand(context.Background(), server.Client(), base, "/", "test-token", "POST", "secrets", nil, commandFlags{pos: []string{"set", "PASS"}, json: asJSON, dry: true}, "secret", &out, &errs, "entered-value")
		if code != 0 || strings.Contains(out.String()+errs.String(), "test-token") || !strings.Contains(out.String(), "[redacted]-web") || !strings.Contains(out.String(), "secret set") || !strings.Contains(out.String(), "false") {
			t.Error("dry secret diagnostics changed plan or exposed token")
		}
	}
}

func TestHeldDiagnosticSpansPreserveProtocol(t *testing.T) {
	input := `{"approval_id":"ap1","expires":"2026-11-18T12:00:00Z","plan":{"z":123456789012345678901234567890,"action":"secret set","detail":"action entered-value","detail":"safe"},"extra":true}`
	want := `{"approval_id":"ap1","expires":"2026-11-18T12:00:00Z","plan":{"z":123456789012345678901234567890,"action":"secret set","detail":"[redacted] [redacted]","detail":"safe"},"extra":true}`
	var out, errs bytes.Buffer
	code, held := reportHeldMarkers([]byte(input), &out, &errs, true, nil, "action", "entered-value")
	var pretty bytes.Buffer
	if json.Indent(&pretty, []byte(want), "", "  ") != nil {
		t.Fatal("bad fixture")
	}
	if !held || code != exitHeld || out.String() != pretty.String()+"\n" || errs.String() != "held for approval: ap1 (expires 2026-11-18T12:00:00Z)\n" {
		t.Error("held response changed fields, ordering or exit")
	}
	if got := redactHeldJSON([]byte(input), "absent"); string(got) != input {
		t.Error("safe held bytes changed")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusAccepted); fmt.Fprint(w, input) }))
	defer server.Close()
	base, _ := url.Parse(server.URL)
	out.Reset()
	errs.Reset()
	code = sendCommand(context.Background(), server.Client(), base, "/", "action", "POST", "secrets", nil, commandFlags{pos: []string{"set", "PASS"}, json: true}, "secret", &out, &errs, "entered-value")
	if code != exitHeld || out.String() != pretty.String()+"\n" {
		t.Error("named held response changed ordering or leaked a credential")
	}
}

func TestSecretRegistryFallbackRetainsTrailingDataBehavior(t *testing.T) {
	for _, asJSON := range []bool{false, true} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{"registry":"registry.example","services_error":"test-token","services":false} garbage`)
		}))
		base, _ := url.Parse(server.URL)
		var out, errs bytes.Buffer
		code := sendCommand(context.Background(), server.Client(), base, "/", "test-token", "POST", "secrets", nil, commandFlags{pos: []string{"set", "PASS"}, json: asJSON}, "secret", &out, &errs, "entered-value")
		server.Close()
		if code != 0 || strings.Contains(out.String()+errs.String(), "test-token") || !strings.Contains(out.String(), "[redacted]") {
			t.Error("fallback changed exit or exposed token")
		}
	}
}
