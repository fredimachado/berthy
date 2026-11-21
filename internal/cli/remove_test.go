package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRemoveSendsTheAppAndFlags(t *testing.T) {
	var path, body string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if r.Method != "POST" || r.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("bad request %s", r.Method)
		}
		path, body = r.URL.Path, string(b)
		fmt.Fprint(w, `{"app":"api","roles":[],"remove":{"app":"api","kind":"image","services":["demo-api-web"],"kept_volumes":["data"],"ingress":[{"hostname":"api.example.com","route":"remove","dns":"remove"},{"hostname":"b.example.com","route":"release","dns":"release"}]}}`)
	}))
	defer server.Close()
	cfg := ingressCLIConfig(t, server.URL)
	var out, errs bytes.Buffer
	if code := Run([]string{"remove", "api", "-e", "dev", "--config", cfg, "--volumes", "--allow-recreate", "--dry-run"}, &out, &errs); code != 0 {
		t.Fatalf("exit %d: %s", code, errs.String())
	}
	if path != "/v1/projects/demo/environments/dev/remove" {
		t.Fatalf("path %s", path)
	}
	var req map[string]any
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	if req["app"] != "api" || req["volumes"] != true || req["allow_recreate"] != true || req["dry_run"] != true || req["request_id"] == "" {
		t.Fatalf("body %s", body)
	}
	if m, _ := req["manifest"].(map[string]any); m["project"] != "" || m["apps"] != nil {
		t.Fatalf("remove must not send a manifest: %s", body)
	}
	for _, want := range []string{"remove api (image)", "demo-api-web", "api.example.com", "route remove, DNS remove", "b.example.com", "route release, DNS release", "volumes kept", "data"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("dry run output lacks %q:\n%s", want, out.String())
		}
	}
	out.Reset()
	if code := Run([]string{"remove", "api", "-e", "dev", "--config", cfg, "--dry-run", "--json"}, &out, &errs); code != 0 || !json.Valid(out.Bytes()) {
		t.Fatalf("json: exit %d: %s", code, out.String())
	}
	if strings.Contains(body, `"volumes"`) || strings.Contains(body, `"allow_recreate"`) {
		t.Fatalf("flags sent without being asked: %s", body)
	}
}

func TestRemoveNeedsOneAppAndNoManifest(t *testing.T) {
	cfg := ingressCLIConfig(t, "http://127.0.0.1:9")
	for _, args := range [][]string{{"remove", "-e", "dev"}, {"remove", "a", "b", "-e", "dev"}} {
		var out, errs bytes.Buffer
		if code := Run(append(args, "--config", cfg), &out, &errs); code != 2 {
			t.Fatalf("%v: exit %d", args, code)
		}
	}
	var out, errs bytes.Buffer
	if code := Run([]string{"deploy", "api", "-e", "dev", "--config", cfg, "--volumes", "--version", "v1"}, &out, &errs); code != 2 || !strings.Contains(errs.String(), "--volumes applies to remove and destroy only") {
		t.Fatalf("exit %d: %s", code, errs.String())
	}
}

func TestRemoveReportsWhatItDid(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/projects/demo/environments/dev/remove":
			w.WriteHeader(202)
			fmt.Fprint(w, `{"operation_id":"op1","events_url":"/v1/projects/demo/environments/dev/operations/op1/events"}`)
		case "/v1/projects/demo/environments/dev/operations/op1/events":
			fmt.Fprint(w, "id: 1\nevent: accepted\ndata: {}\n\nid: 2\nevent: step\ndata: {\"step\":\"deleted service demo-api-web\"}\n\nid: 3\nevent: step\ndata: {\"step\":\"kept volume data\"}\n\nid: 4\nevent: succeeded\ndata: {}\n\n")
		case "/v1/projects/demo/environments/dev/operations/op1":
			fmt.Fprint(w, `{"state":"succeeded"}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()
	cfg := ingressCLIConfig(t, server.URL)
	var out, errs bytes.Buffer
	if code := Run([]string{"remove", "api", "-e", "dev", "--config", cfg}, &out, &errs); code != 0 {
		t.Fatalf("exit %d: %s", code, errs.String())
	}
	for _, want := range []string{"deleted service demo-api-web", "kept volume data"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output lacks %q:\n%s", want, out.String())
		}
	}
	out.Reset()
	if code := Run([]string{"remove", "api", "-e", "dev", "--config", cfg, "--json"}, &out, &errs); code != 0 || !json.Valid(out.Bytes()) {
		t.Fatalf("json output is not one document: exit %d %q", code, out.String())
	}
}

func TestDestroyNeedsTheEnvironmentNameBeforeAnyRequest(t *testing.T) {
	asked := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { asked = true }))
	defer server.Close()
	cfg := ingressCLIConfig(t, server.URL)
	for _, args := range [][]string{
		{"destroy", "-e", "dev", "--config", cfg},
		{"destroy", "-e", "dev", "--config", cfg, "--confirm", "prod"},
		{"destroy", "-e", "dev", "--config", cfg, "--confirm", "Dev"},
		{"destroy", "-e", "dev", "--config", cfg, "--confirm", "dev", "extra"},
		{"destroy", "-e", "dev", "--config", cfg, "--confirm", "dev", "--dry-run", "--confirm", "other"},
	} {
		var out, errs bytes.Buffer
		if code := Run(args, &out, &errs); code != 2 {
			t.Fatalf("%v: exit %d %s", args, code, errs.String())
		}
	}
	if asked {
		t.Fatal("a refused destroy reached the agent")
	}
}

func TestDestroySendsTheFlagsAndShowsThePlan(t *testing.T) {
	var path, body string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		path, body = r.URL.Path, string(b)
		fmt.Fprint(w, `{"roles":[],"destroy":{"apps":[{"app":"api","kind":"image","services":["demo-api-web"],"kept_volumes":["data"]},{"app":"db","kind":"compose","project":"shop-db","services":["database"],"volumes":["shop-db_data"]}],"remove_connector":true,"helpers":["berthy-helper-dev"],"one_offs":["hook-1"],"images":["ghcr.io/acme/api@sha256:1"],"tunnel":{"name":"demo-dev","id":"t1","delete":false,"reasons":["a rule for foreign.example.com remains"]},"blockers":["removing stateful app db needs --allow-recreate"]}}`)
	}))
	defer server.Close()
	cfg := ingressCLIConfig(t, server.URL)
	var out, errs bytes.Buffer
	if code := Run([]string{"destroy", "-e", "dev", "--config", cfg, "--confirm", "dev", "--volumes", "--allow-recreate", "--tunnel", "--dry-run"}, &out, &errs); code != 0 {
		t.Fatalf("exit %d: %s", code, errs.String())
	}
	var req map[string]any
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	if path != "/v1/projects/demo/environments/dev/destroy" || req["confirm"] != "dev" || req["volumes"] != true || req["allow_recreate"] != true || req["tunnel"] != true || req["dry_run"] != true || req["request_id"] == "" {
		t.Fatalf("%s %s", path, body)
	}
	for _, want := range []string{"destroy dev", "api (image)", "demo-api-web", "db (compose)", "shop-db_data", "managed ingress", "connector", "berthy-helper-dev", "hook-1", "ghcr.io/acme/api@sha256:1", "tunnel demo-dev: keep", "a rule for foreign.example.com remains", "blockers", "removing stateful app db needs --allow-recreate"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("plan output lacks %q:\n%s", want, out.String())
		}
	}
}

func TestDestroyReportsWhatItDid(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/projects/demo/environments/dev/destroy":
			w.WriteHeader(202)
			fmt.Fprint(w, `{"operation_id":"op1","events_url":"/v1/projects/demo/environments/dev/operations/op1/events"}`)
		case "/v1/projects/demo/environments/dev/operations/op1/events":
			fmt.Fprint(w, "id: 1\nevent: accepted\ndata: {}\n\nid: 2\nevent: step\ndata: {\"step\":\"api: deleted service demo-api-web\"}\n\nid: 3\nevent: step\ndata: {\"step\":\"kept tunnel demo-dev: a rule for foreign.example.com remains\"}\n\nid: 4\nevent: succeeded\ndata: {}\n\n")
		case "/v1/projects/demo/environments/dev/operations/op1":
			fmt.Fprint(w, `{"state":"succeeded"}`)
		}
	}))
	defer server.Close()
	cfg := ingressCLIConfig(t, server.URL)
	var out, errs bytes.Buffer
	if code := Run([]string{"destroy", "-e", "dev", "--config", cfg, "--confirm", "dev"}, &out, &errs); code != 0 {
		t.Fatalf("exit %d: %s", code, errs.String())
	}
	for _, want := range []string{"api: deleted service demo-api-web", "kept tunnel demo-dev"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output lacks %q:\n%s", want, out.String())
		}
	}
}
