package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"berthy/internal/api"
	"berthy/internal/keycopy"
	"berthy/internal/scope"
)

func TestDoctorWarnsUntilTheCurrentKeyCopyExists(t *testing.T) {
	home := t.TempDir()
	oldHome := KeyHome
	KeyHome = func() (string, error) { return home, nil }
	t.Cleanup(func() { KeyHome = oldHome })
	id := scope.ID{Project: "demo", Environment: "dev"}
	oldID, currentID := "0123456789abcdef", "fedcba9876543210"
	keyID := oldID
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"checks":[],"key_id":%q}`, keyID)
	}))
	defer server.Close()
	cfg := ingressCLIConfig(t, server.URL)
	dir := filepath.Join(home, ".berthy", "keys")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, keycopy.FileName(id, oldID)), []byte("marker"), 0600); err != nil {
		t.Fatal(err)
	}
	want := "warning: this machine has no saved copy of the current state key " + currentID + ". Run remote berthy setup for demo/dev from this machine without --key-file to save a copy.\n"
	run := func(warning string) {
		t.Helper()
		var out, errs bytes.Buffer
		if code := Run([]string{"doctor", "-e", "dev", "--config", cfg}, &out, &errs); code != 0 || strings.Count(out.String(), "warning:") != strings.Count(warning, "warning:") || (warning != "" && !strings.Contains(out.String(), warning)) || errs.Len() != 0 {
			t.Fatalf("code %d stdout %q stderr %q; want warning %q", code, out.String(), errs.String(), warning)
		}
	}
	run("")
	keyID = currentID
	run(want)
	if err := os.WriteFile(filepath.Join(dir, keycopy.FileName(id, currentID)), []byte("marker"), 0600); err != nil {
		t.Fatal(err)
	}
	run("")
}

func TestDoctorKeyCopyWarningKeepsJSONAndExitCode(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".berthy", "keys")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, keycopy.FileName(scope.ID{Project: "demo", Environment: "dev"}, "0123456789abcdef")), []byte("marker"), 0600); err != nil {
		t.Fatal(err)
	}
	oldHome := KeyHome
	KeyHome = func() (string, error) { return home, nil }
	t.Cleanup(func() { KeyHome = oldHome })
	for _, failing := range []bool{false, true} {
		t.Run(fmt.Sprintf("critical_failure_%v", failing), func(t *testing.T) {
			checks := []api.Check{{Name: "host", OK: !failing, Critical: true, Detail: "host"}}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				json.NewEncoder(w).Encode(api.DoctorReport{Checks: checks, KeyID: "fedcba9876543210"})
			}))
			cfg := ingressCLIConfig(t, server.URL)
			var out, errs bytes.Buffer
			code := Run([]string{"doctor", "-e", "dev", "--config", cfg, "--json"}, &out, &errs)
			server.Close()
			wantCode := 0
			if failing {
				wantCode = 1
			}
			var got api.DoctorReport
			if err := json.Unmarshal(out.Bytes(), &got); err != nil || got.KeyID != "fedcba9876543210" || len(got.Checks) != 1 || got.Checks[0] != checks[0] || code != wantCode {
				t.Fatalf("code %d report %q error %v", code, out.String(), err)
			}
			want := "warning: this machine has no saved copy of the current state key fedcba9876543210. Run remote berthy setup for demo/dev from this machine without --key-file to save a copy.\n"
			if errs.String() != want {
				t.Fatalf("stderr %q, want %q", errs.String(), want)
			}
		})
	}
}

func TestDoctorRejectsMalformedKeyID(t *testing.T) {
	oldHome := KeyHome
	KeyHome = func() (string, error) { t.Fatal("looked up home for invalid ID"); return "", nil }
	t.Cleanup(func() { KeyHome = oldHome })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"checks":[],"key_id":"../bad"}`) }))
	defer server.Close()
	var out, errs bytes.Buffer
	code := Run([]string{"doctor", "-e", "dev", "--config", ingressCLIConfig(t, server.URL)}, &out, &errs)
	if code != 1 || !strings.Contains(errs.String(), "invalid agent response") || strings.Contains(out.String(), "../bad") || strings.Contains(errs.String(), "../bad") {
		t.Fatalf("code %d stdout %q stderr %q", code, out.String(), errs.String())
	}
}
