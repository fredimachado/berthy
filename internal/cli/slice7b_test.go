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

	"berthy/internal/manifest"
)

func TestOriginPathsAreTerminalSafe(t *testing.T) {
	var out bytes.Buffer
	printOrigins(&out, []manifest.Origin{{Path: "a\x1bb", Value: "x", File: "f\nname", Line: 3}, {Path: "z", Value: "y"}})
	if got, want := out.String(), "a\\x1bb: x  f name:3\nz: y       default\n"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestUsageStyleAfterModeResolution(t *testing.T) {
	var out, errs bytes.Buffer
	slice5Destination(t, &errs)
	t.Setenv("BERTHY_ENV", "")
	for _, args := range [][]string{{"ps", "--bad", "--json"}, {"ps", "--json", "--bad"}, {"deploy", "--bad", "--json"}, {"deploy", "--json", "--bad"}} {
		errs.Reset()
		if code := Run(args, &out, &errs); code != 2 || errs.String() != "unknown flag --bad\n" {
			t.Fatalf("%v: %d %q", args, code, errs.String())
		}
	}
	for _, args := range [][]string{{"setup", "--bad", "--json"}, {"setup", "--json", "--bad"}} {
		errs.Reset()
		code := Run(args, &out, &errs)
		want := "flag provided but not defined: -bad\n" +
			"usage: berthy setup -e ENV [--host USER@SERVER | --server NAME | --local] [--project NAME] [--listen ADDRESS:PORT] [--network NAME] [--protected]\n" +
			"                    [--manifest-dir DIR] [--binary PATH] [--key-file PATH] [--new-token] [--show-token] [--config PATH]\n" +
			"                    [--no-install-docker] [--cluster-alerts FILE|none] [--dry-run] [--json] [--verbose]\n" +
			"       berthy setup --cluster-secret NAME -e ENV [--host USER@SERVER | --server NAME | --local]\n" +
			"                    [--project NAME] [--manifest-dir DIR] [--dry-run] [--json] [--verbose]\n" +
			"       berthy setup --remove -e ENV [--host USER@SERVER | --server NAME | --local] [--project NAME]\n" +
			"                    [--manifest-dir DIR] [--config PATH] [--purge] [--dry-run] [--json] [--verbose]\n"
		if code != 2 || errs.String() != want {
			t.Fatalf("%v: %d %q", args, code, errs.String())
		}
	}
	f, err := parseDeploymentArgs("exec", []string{"web", "-e", "dev", "--", "--json"})
	if err != nil || f.json || len(f.command) != 1 || f.command[0] != "--json" {
		t.Fatalf("child args: %+v %v", f, err)
	}
	for _, tc := range []struct {
		args   []string
		styled bool
	}{
		{[]string{"deploy"}, true}, {[]string{"deploy", "--json"}, false},
		{[]string{"setup"}, true}, {[]string{"setup", "--json"}, false},
	} {
		errs.Reset()
		code := Run(tc.args, &out, &errs)
		if code != 2 || strings.Contains(errs.String(), "\x1b[31m") != tc.styled {
			t.Fatalf("%v: %d %q", tc.args, code, errs.String())
		}
	}
	errs.Reset()
	if code := Run([]string{"ps"}, &out, &errs); code != 2 || !strings.Contains(errs.String(), "\x1b[31mremote commands require -e\x1b[0m") {
		t.Fatalf("human %d %q", code, errs.String())
	}
	errs.Reset()
	if code := Run([]string{"ps", "--json"}, &out, &errs); code != 2 || errs.String() != "remote commands require -e\n" {
		t.Fatalf("json %d %q", code, errs.String())
	}
}

func TestHeldHeadingStyle(t *testing.T) {
	var out, errs bytes.Buffer
	slice5Destination(t, &out)
	code, held := reportHeldMarkers([]byte(`{"approval_id":"ap1","expires":"2030-01-01T00:00:00Z","plan":{}}`), &out, &errs, false, nil)
	if !held || code != exitHeld || !strings.Contains(out.String(), "\x1b[33mheld for approval\x1b[0m:") {
		t.Fatalf("%d %q", code, out.String())
	}
}

func TestSetupAndDestructivePlanSpans(t *testing.T) {
	var out, errs bytes.Buffer
	slice5Destination(t, &out)
	if code := printRemovePlan(&out, &errs, []byte(`{"remove":{"app":"a","kind":"image","volumes":["v"],"kept_volumes":["k"],"blockers":["b"]}}`)); code != 0 {
		t.Fatal(errs.String())
	}
	for _, want := range []string{"\x1b[31mvolumes to delete:\x1b[0m", "\x1b[31mblockers:\x1b[0m", "volumes kept"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q in %q", want, out.String())
		}
	}
	out.Reset()
	if code := printDestroyPlan(&out, &errs, []byte(`{"destroy":{"apps":[{"app":"a","kind":"image","volumes":["v"],"kept_volumes":["k"]}],"blockers":["b"]}}`), "dev"); code != 0 || !strings.Contains(out.String(), "\x1b[31mvolume to delete:\x1b[0m") || !strings.Contains(out.String(), "volume kept: k") || !strings.Contains(out.String(), "\x1b[31mblockers:\x1b[0m") {
		t.Fatalf("destroy %d %q %q", code, out.String(), errs.String())
	}
	out.Reset()
	if got := setupSummary(setupReport{Project: "p", Environment: "e", Target: "h"}, styleFor(&out, true)); !strings.Contains(got, "\x1b[31mfailed at\x1b[0m connect") {
		t.Fatal(got)
	}
	if got := setupSummary(setupReport{Project: "p", Environment: "e", Target: "host", OK: true}, styleFor(&out, true)); !strings.Contains(got, "\x1b[32mdone\x1b[0m") {
		t.Errorf("summary %q", got)
	}
}

func TestManifestProblemLocationStyle(t *testing.T) {
	var out bytes.Buffer
	slice5Destination(t, &out)
	printProblems(&out, manifest.Problems{{File: "x:y", Line: 3, Column: 4, Path: "app.port", Message: "app: invalid"}, {Message: "bad"}}, true)
	if want := "\x1b[35mx:y:3:4\x1b[0m: app: invalid\nbad\n\x1b[31m2 problems\x1b[0m\n"; out.String() != want {
		t.Fatalf("got %q want %q", out.String(), want)
	}
}

func TestChildJSONArgumentAndBanner(t *testing.T) {
	var out, errs bytes.Buffer
	slice5Destination(t, &errs)
	old := stderrTerminal
	stderrTerminal = func(w io.Writer) bool { return w == &errs }
	t.Cleanup(func() { stderrTerminal = old })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Command []string `json:"command"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if len(body.Command) != 1 || body.Command[0] != "--json" {
			t.Errorf("command %v", body.Command)
		}
		w.WriteHeader(http.StatusAccepted)
		fmt.Fprint(w, `{"approval_id":"ap1","expires":"2030-01-01T00:00:00Z","plan":{}}`)
	}))
	defer server.Close()
	cfg := ingressCLIConfig(t, server.URL)
	code := Run([]string{"exec", "web", "-e", "dev", "--config", cfg, "--", "--json"}, &out, &errs)
	if code != exitHeld || !strings.Contains(out.String(), "held for approval") {
		t.Fatalf("exit %d: %q %q", code, out.String(), errs.String())
	}
	if !strings.Contains(errs.String(), "\x1b[35mberthy:\x1b[0m \x1b[1mdemo/dev\x1b[0m") {
		t.Fatalf("banner %q", errs.String())
	}
}

func TestManifestWarningPartialLocation(t *testing.T) {
	var out bytes.Buffer
	slice5Destination(t, &out)
	printWarnings(&out, manifest.Problems{{File: "f:name", Path: "a.b", Message: "a: risky"}, {File: "f", Line: 2, Message: "bad"}})
	want := "\x1b[33mwarning:\x1b[0m \x1b[35mf:name\x1b[0m: a: risky\n\x1b[33mwarning:\x1b[0m \x1b[35mf:2\x1b[0m: bad\n"
	if out.String() != want {
		t.Fatalf("got %q want %q", out.String(), want)
	}
}
