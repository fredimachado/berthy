package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

func TestRestoreExecSendsTheCommandAndShowsWhereItWouldRun(t *testing.T) {
	var path, body string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		path, body = r.URL.Path, string(b)
		fmt.Fprint(w, `{"app":"","image":"","roles":[],"restore":{"key":"db/command/main-db/k.gz","backup":"main-db","app":"db","target":"db service postgres","command":["pg_restore","--clean","-d","scratch"],"size":1536,"sha256":"`+backupSum+`","finished":"2026-11-16T10:00:03Z"}}`)
	}))
	defer server.Close()
	cfg := ingressCLIConfig(t, server.URL)
	var out, errs bytes.Buffer
	args := []string{"restore", "db/command/main-db/k.gz", "-e", "dev", "--config", cfg, "--exec", "db", "--service", "postgres", "--timeout", "2h", "--dry-run", "--", "pg_restore", "--clean", "-d", "scratch"}
	if code := Run(args, &out, &errs); code != 0 {
		t.Fatalf("exit %d: %s", code, errs.String())
	}
	var req map[string]any
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	command, _ := req["command"].([]any)
	if path != "/v1/projects/demo/environments/dev/restore" || req["key"] != "db/command/main-db/k.gz" || req["app"] != "db" || req["service"] != "postgres" || req["timeout"] != "2h" ||
		req["dry_run"] != true || len(command) != 4 || command[0] != "pg_restore" || command[3] != "scratch" || req["into"] != nil {
		t.Fatalf("%s %s", path, body)
	}
	for _, want := range []string{"restore db/command/main-db/k.gz", "into the input of a command on db service postgres", "command: pg_restore --clean -d scratch", "backup: main-db (app db, command)", "1.5 KiB", backupSum} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("plan output lacks %q:\n%s", want, out.String())
		}
	}
}

func TestRestoreExecNeedsACommandAndExcludesInto(t *testing.T) {
	cfg := ingressCLIConfig(t, "http://127.0.0.1:1")
	for name, args := range map[string][]string{
		"no command":        {"restore", "k", "--exec", "db", "-e", "dev"},
		"into and exec":     {"restore", "k", "--exec", "db", "--into", "v", "-e", "dev", "--config", "CFG", "--", "true"},
		"neither":           {"restore", "k", "-e", "dev"},
		"command with into": {"restore", "k", "--into", "v", "-e", "dev", "--config", "CFG", "--", "true"},
		"role with into":    {"restore", "k", "--into", "v", "--role", "web", "-e", "dev"},
		"role and service":  {"restore", "k", "--exec", "db", "--role", "web", "--service", "pg", "-e", "dev", "--config", "CFG", "--", "true"},
		"exec on a deploy":  {"deploy", "api", "--exec", "db", "-e", "dev"},
	} {
		var out, errout bytes.Buffer
		if !slices.Contains(args, "CFG") {
			args = append(args, "--config", "CFG")
		}
		for i := range args {
			if args[i] == "CFG" {
				args[i] = cfg
			}
		}
		if code := Run(args, &out, &errout); code != 2 {
			t.Errorf("%s: exit %d: %q", name, code, errout.String())
		}
	}
}

func TestRestoreExecShowsTheStepsAndTheReasonOfAFailedCommand(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/projects/demo/environments/dev/restore":
			w.WriteHeader(202)
			fmt.Fprint(w, `{"operation_id":"op1","events_url":"/v1/projects/demo/environments/dev/operations/op1/events"}`)
		case "/v1/projects/demo/environments/dev/operations/op1/events":
			fmt.Fprint(w, "id: 1\nevent: step\ndata: {\"step\":\"restoring k into the command on db\"}\n\nid: 2\nevent: step\ndata: {\"step\":\"pg_restore: error: could not connect\"}\n\n")
		case "/v1/projects/demo/environments/dev/operations/op1":
			fmt.Fprint(w, `{"state":"failed","kind":"restore","error":"restore command failed","reason":"command exited with code 1: pg_restore: error: could not connect"}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()
	cfg := ingressCLIConfig(t, server.URL)
	var out, errs bytes.Buffer
	if code := Run([]string{"restore", "k", "--exec", "db", "-e", "dev", "--config", cfg, "--", "pg_restore"}, &out, &errs); code != 1 {
		t.Fatalf("exit %d: %s", code, errs.String())
	}
	for _, want := range []string{"restoring k into the command on db", "pg_restore: error: could not connect", "restore command failed"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}

func TestBackupListShowsACommandBackupWithoutAVolume(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `[{"key":"db/command/main-db/k.gz","format":"berthy-command-gzip-1","backup":"main-db","app":"db","size":1536,"sha256":"`+backupSum+`","finished":"2026-11-16T10:00:03Z"}]`)
	}))
	defer server.Close()
	cfg := ingressCLIConfig(t, server.URL)
	var out, errs bytes.Buffer
	if code := Run([]string{"backup", "list", "-e", "dev", "--config", cfg}, &out, &errs); code != 0 {
		t.Fatalf("exit %d: %s", code, errs.String())
	}
	if !strings.Contains(out.String(), "main-db") || !strings.Contains(out.String(), "command") || !strings.Contains(out.String(), "db/command/main-db/k.gz") {
		t.Fatalf("output:\n%s", out.String())
	}
}
