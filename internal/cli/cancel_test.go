package cli

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// cancelServer answers the cancel of operation op1 with the plan, and the operation itself as running for the first poll
// and then with its end.
type cancelServer struct {
	*httptest.Server
	post   atomic.Value // the last body of a cancel
	polls  atomic.Int32
	status int
	answer string
	end    string
}

func newCancelServer(t *testing.T, status int, answer, end string) *cancelServer {
	poll := cancelPoll
	cancelPoll = 5 * time.Millisecond
	t.Cleanup(func() { cancelPoll = poll })
	s := &cancelServer{status: status, answer: answer, end: end}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST" && r.URL.Path == "/v1/projects/demo/environments/dev/operations/op1/cancel":
			b, _ := io.ReadAll(r.Body)
			s.post.Store(string(b))
			w.WriteHeader(s.status)
			fmt.Fprint(w, s.answer)
		case r.Method == "GET" && r.URL.Path == "/v1/projects/demo/environments/dev/operations/op1":
			if s.polls.Add(1) < 2 {
				fmt.Fprint(w, `{"id":"op1","state":"running","kind":"deploy","app":"web"}`)
				return
			}
			fmt.Fprint(w, s.end)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(s.Close)
	return s
}

func TestCancelAsksTheAgentAndWaitsForTheOperationToEnd(t *testing.T) {
	s := newCancelServer(t, 200, `{"operation_id":"op1","kind":"deploy","app":"web","accepted":true}`,
		`{"id":"op1","state":"rolled_back","kind":"deploy","app":"web","reason":"cancelled by tok","error":"operation cancelled"}`)
	var out, errs bytes.Buffer
	if code := Run([]string{"cancel", "op1", "-e", "dev", "--config", ingressCLIConfig(t, s.URL)}, &out, &errs); code != 0 {
		t.Fatalf("exit %d: %s %s", code, out.String(), errs.String())
	}
	if got := s.post.Load(); got != `{"dry_run":false}` {
		t.Errorf("body %v", got)
	}
	for _, want := range []string{"cancel accepted for deploy web", "operation op1 ended: rolled_back", "cancelled by tok"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}

func TestCancelDryRunSaysWhetherItWouldBeAccepted(t *testing.T) {
	s := newCancelServer(t, 200, `{"operation_id":"op1","kind":"deploy","app":"web","accepted":true}`, "")
	var out, errs bytes.Buffer
	if code := Run([]string{"cancel", "op1", "-e", "dev", "--config", ingressCLIConfig(t, s.URL), "--dry-run"}, &out, &errs); code != 0 {
		t.Fatalf("exit %d: %s %s", code, out.String(), errs.String())
	}
	if got := s.post.Load(); got != `{"dry_run":true}` || !strings.Contains(out.String(), "would be accepted") || s.polls.Load() != 0 {
		t.Errorf("body %v output %q polls %d", got, out.String(), s.polls.Load())
	}
	refused := newCancelServer(t, 200, `{"operation_id":"op1","kind":"deploy","app":"web","accepted":false,"reason":"the deploy already switched traffic"}`, "")
	out.Reset()
	errs.Reset()
	if code := Run([]string{"cancel", "op1", "-e", "dev", "--config", ingressCLIConfig(t, refused.URL), "--dry-run"}, &out, &errs); code != 1 || !strings.Contains(out.String(), "would be refused: the deploy already switched traffic") {
		t.Fatalf("exit %d: %s %s", code, out.String(), errs.String())
	}
}

func TestCancelRefusedPrintsTheReasonAndFails(t *testing.T) {
	s := newCancelServer(t, 409, `{"code":"cancel_refused","message":"a migration is running"}`, "")
	var out, errs bytes.Buffer
	if code := Run([]string{"cancel", "op1", "-e", "dev", "--config", ingressCLIConfig(t, s.URL)}, &out, &errs); code != 1 || !strings.Contains(errs.String(), "a migration is running") {
		t.Fatalf("exit %d: %s %s", code, out.String(), errs.String())
	}
	if s.polls.Load() != 0 {
		t.Error("the CLI waited for an operation that was not cancelled")
	}
}

func TestCancelNeedsOneOperation(t *testing.T) {
	var out, errs bytes.Buffer
	if code := Run([]string{"cancel", "-e", "dev", "--config", ingressCLIConfig(t, "http://127.0.0.1:1")}, &out, &errs); code != 2 || !strings.Contains(errs.String(), "usage: cancel OPERATION_ID") {
		t.Fatalf("exit %d: %s", code, errs.String())
	}
}

func TestCancelHeldForApprovalSaysSo(t *testing.T) {
	s := newCancelServer(t, 202, `{"approval_id":"ap1","expires":"2026-11-19T10:00:00Z","plan":{"operation_id":"op1","accepted":true}}`, "")
	var out, errs bytes.Buffer
	if code := Run([]string{"cancel", "op1", "-e", "dev", "--config", ingressCLIConfig(t, s.URL)}, &out, &errs); code != exitHeld || !strings.Contains(out.String(), "held for approval: ap1") {
		t.Fatalf("exit %d: %s %s", code, out.String(), errs.String())
	}
}

func TestJobCancelStopsTheRunOfTheJob(t *testing.T) {
	var path atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path.Store(r.Method + " " + r.URL.Path)
		fmt.Fprint(w, `{"job":"dump","accepted":true}`)
	}))
	defer server.Close()
	var out, errs bytes.Buffer
	if code := Run([]string{"job", "cancel", "dump", "-e", "dev", "--config", ingressCLIConfig(t, server.URL)}, &out, &errs); code != 0 {
		t.Fatalf("exit %d: %s %s", code, out.String(), errs.String())
	}
	if path.Load() != "POST /v1/projects/demo/environments/dev/jobs/dump/cancel" || !strings.Contains(out.String(), "cancelled the run of job dump") {
		t.Errorf("request %v output %q", path.Load(), out.String())
	}
	refusing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(409)
		fmt.Fprint(w, `{"code":"cancel_refused","message":"job is not running"}`)
	}))
	defer refusing.Close()
	out.Reset()
	errs.Reset()
	if code := Run([]string{"job", "cancel", "dump", "-e", "dev", "--config", ingressCLIConfig(t, refusing.URL)}, &out, &errs); code != 1 || !strings.Contains(errs.String(), "job is not running") {
		t.Fatalf("exit %d: %s %s", code, out.String(), errs.String())
	}
}
