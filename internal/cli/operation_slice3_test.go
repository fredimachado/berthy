package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

func TestOperationTerminalStepMatrix(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	oldTerm, oldWidth, oldVT := stderrTerminal, progressWidth, progressVT
	t.Cleanup(func() { stderrTerminal, progressWidth, progressVT = oldTerm, oldWidth, oldVT })
	progressWidth = func(io.Writer) (int, error) { return 80, nil }
	progressVT = func(io.Writer, string) bool { return true }
	events := "id: 1\nevent: accepted\ndata: {}\n\nid: 2\nevent: step\ndata: {\"step\":\"hello token\\u001b\"}\n\nid: 3\nevent: verify\ndata: {\"step\":\"good\",\"status\":\"ok\"}\n\nid: 4\nevent: verify\ndata: {\"step\":\"caution\",\"status\":\"warn\"}\n\nid: 5\nevent: verify\ndata: {\"step\":\"unsure\",\"status\":\"unknown\"}\n\nid: 6\nevent: verify\ndata: {\"step\":\"plain\"}\n\nid: 7\nevent: succeeded\ndata: {}\n\nevent: terminal\ndata: {}\n\n"
	for _, cmd := range []string{"deploy", "rollback", "restart", "scale", "prune", "exec", "run", "remove", "destroy", "restore", "ingress", "unrecognized"} {
		for _, mode := range []string{"terminal", "pipe", "json"} {
			t.Run(cmd+"/"+mode, func(t *testing.T) {
				var out, errs bytes.Buffer
				stderrTerminal = func(w io.Writer) bool { return mode == "terminal" && w == &out }
				client := &http.Client{Transport: progressRoundTrip(func(r *http.Request) (*http.Response, error) {
					body := `{"state":"succeeded"}`
					if strings.HasSuffix(r.URL.Path, "/events") {
						body = events
						if r.URL.RawQuery != "" {
							t.Errorf("unexpected query %q", r.URL.RawQuery)
						}
					}
					if strings.HasSuffix(r.URL.Path, "/output") {
						body = `{"output":"raw\u001b[31m","exit_code":0}`
					}
					return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: r}, nil
				})}
				base, _ := url.Parse("http://agent.invalid")
				code := watchOperation(context.Background(), client, base, "token", "/v1/", "op", "/v1/operations/op/events", &out, &errs, mode == "json", cmd, "dev", "demo")
				if code != 0 {
					t.Fatalf("exit %d: %s", code, errs.String())
				}
				got := out.String()
				if mode == "terminal" {
					want := "  hello [redacted]\\x1b\n  ✓ good\n  ! caution\n  ? unsure\n  plain\n"
					if !strings.HasPrefix(got, want) {
						t.Errorf("human steps = %q, want prefix %q", got, want)
					}
				} else if mode == "pipe" {
					first := "accepted\nstep\n"
					if reportsSteps(cmd) {
						first = "accepted\n  hello [redacted]\\x1b\n"
					}
					if !strings.HasPrefix(got, first+"  good\n  caution\n  unsure\n  plain\nsucceeded\nterminal\n") {
						t.Errorf("legacy pipe = %q", got)
					}
				} else if !strings.Contains(got, `"checks":`) || !strings.Contains(got, `"status": "ok"`) {
					t.Errorf("JSON checks = %q", got)
				}
				if mode != "json" && (cmd == "exec" || cmd == "run") && !strings.Contains(got, "raw\x1b[31m") {
					t.Errorf("raw output lost: %q", got)
				}
			})
		}
	}
}

func TestRollbackProgressLabel(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	forceStderrTerminal(t, true)
	forceProgressVT(t)
	old := progressWidth
	progressWidth = func(io.Writer) (int, error) { return 100, nil }
	t.Cleanup(func() { progressWidth = old })
	synctest.Test(t, func(t *testing.T) {
		reader, writer := io.Pipe()
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan int, 1)
		defer func() { cancel(); writer.Close(); <-done }()
		client := &http.Client{Transport: progressRoundTrip(func(r *http.Request) (*http.Response, error) {
			if r.URL.RawQuery != "progress=1" {
				t.Errorf("query %q", r.URL.RawQuery)
			}
			return &http.Response{StatusCode: 200, Body: reader, Header: make(http.Header), Request: r}, nil
		})}
		base, _ := url.Parse("http://agent.invalid")
		var out, errs progressTestBuffer
		go func() {
			done <- watchOperation(ctx, client, base, "token", "/v1/", "op", "/v1/operations/op/events", &out, &errs, false, "rollback", "dev", "demo")
		}()
		synctest.Wait()
		time.Sleep(250 * time.Millisecond)
		synctest.Wait()
		if !strings.Contains(errs.String(), "following rollback") {
			t.Errorf("initial frame: %q", errs.String())
		}
		fmt.Fprint(writer, "id: 1\nevent: rollout\ndata: {\"role\":\"web\",\"status\":\"ready\"}\n\n")
		synctest.Wait()
		time.Sleep(250 * time.Millisecond)
		synctest.Wait()
		if !strings.Contains(errs.String(), "rolling back 1 ready of 1 started") {
			t.Errorf("rollout frame: %q", errs.String())
		}
	})
}

func TestOperationReplayVisibleStepOnce(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	forceStderrTerminal(t, true)
	streams := 0
	client := &http.Client{Transport: progressRoundTrip(func(r *http.Request) (*http.Response, error) {
		body := `{"state":"succeeded"}`
		if strings.HasSuffix(r.URL.Path, "/events") {
			streams++
			if streams == 1 {
				body = "id: 1\nevent: step\ndata: {\"step\":\"once\"}\n\n"
			} else {
				if r.Header.Get("Last-Event-ID") != "1" {
					t.Errorf("cursor = %q", r.Header.Get("Last-Event-ID"))
				}
				body = "id: 1\nevent: step\ndata: {\"step\":\"once\"}\n\nid: 2\nevent: succeeded\ndata: {}\n\n"
			}
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: r}, nil
	})}
	base, _ := url.Parse("http://agent.invalid")
	var out, errs bytes.Buffer
	code := watchOperation(context.Background(), client, base, "token", "/v1/", "op", "/v1/operations/op/events", &out, &errs, false, "ingress", "dev", "demo")
	if code != 0 || streams != 2 || strings.Count(out.String(), "once") != 1 {
		t.Fatalf("exit=%d streams=%d out=%q err=%q", code, streams, out.String(), errs.String())
	}
}

func TestOperationVerifyMarkerColor(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	forceStderrTerminal(t, true)
	forceProgressVT(t)
	old := progressWidth
	progressWidth = func(io.Writer) (int, error) { return 80, nil }
	t.Cleanup(func() { progressWidth = old })
	client := &http.Client{Transport: progressRoundTrip(func(r *http.Request) (*http.Response, error) {
		body := `{"state":"succeeded"}`
		if strings.HasSuffix(r.URL.Path, "/events") {
			body = "id: 1\nevent: verify\ndata: {\"status\":\"ok\",\"step\":\"good\"}\n\nid: 2\nevent: verify\ndata: {\"status\":\"warn\",\"step\":\"careful\"}\n\nid: 3\nevent: verify\ndata: {\"status\":\"unknown\",\"step\":\"unsure\"}\n\nid: 4\nevent: succeeded\ndata: {}\n\n"
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: r}, nil
	})}
	base, _ := url.Parse("http://agent.invalid")
	var out, errs bytes.Buffer
	code := watchOperation(context.Background(), client, base, "token", "/v1/", "op", "/v1/operations/op/events", &out, &errs, false, "restart", "dev", "demo")
	want := "  \x1b[32m✓\x1b[0m good\n  \x1b[33m!\x1b[0m careful\n  \x1b[2m?\x1b[0m unsure\n"
	if code != 0 || !strings.HasPrefix(out.String(), want) {
		t.Fatalf("exit=%d out=%q err=%q", code, out.String(), errs.String())
	}
}

func TestApprovedOperationWatcherIdentity(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	forceStderrTerminal(t, true)
	forceProgressVT(t)
	old := progressWidth
	progressWidth = func(io.Writer) (int, error) { return 100, nil }
	t.Cleanup(func() { progressWidth = old })
	for _, tc := range []struct {
		kind, label string
		output      bool
	}{{"ingress apply", "following ingress apply", false}, {"exec web", "following exec", true}, {"mystery operation", "following operation", false}, {"", "following operation", false}} {
		t.Run(tc.kind, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				reader, writer := io.Pipe()
				ctx, cancel := context.WithCancel(context.Background())
				done := make(chan int, 1)
				defer func() { cancel(); writer.Close(); <-done }()
				client := &http.Client{Transport: progressRoundTrip(func(r *http.Request) (*http.Response, error) {
					body := io.ReadCloser(io.NopCloser(strings.NewReader(`{"state":"succeeded"}`)))
					if strings.HasSuffix(r.URL.Path, "/events") {
						if r.URL.RawQuery != "" {
							t.Errorf("query %q", r.URL.RawQuery)
						}
						body = reader
					}
					if strings.HasSuffix(r.URL.Path, "/output") {
						body = io.NopCloser(strings.NewReader(`{"output":"child\u001b[0m","exit_code":0}`))
					}
					return &http.Response{StatusCode: 200, Body: body, Header: make(http.Header), Request: r}, nil
				})}
				call := func(method, endpoint, query string, body []byte) ([]byte, int, error) {
					if method == "GET" {
						return []byte(fmt.Sprintf(`[{"id":"ap1","kind":%q}]`, tc.kind)), 200, nil
					}
					return []byte(`{"operation_id":"op","events_url":"/v1/operations/op/events"}`), 202, nil
				}
				base, _ := url.Parse("http://agent.invalid")
				var out, errs progressTestBuffer
				go func() {
					done <- runApprove(ctx, call, client, commandFlags{pos: []string{"ap1"}}, base, "/v1/", "token", &out, &errs)
				}()
				synctest.Wait()
				time.Sleep(250 * time.Millisecond)
				synctest.Wait()
				if !strings.Contains(errs.String(), tc.label) {
					t.Errorf("frame = %q, want %q", errs.String(), tc.label)
				}
				fmt.Fprint(writer, "id: 1\nevent: step\ndata: {\"step\":\"visible\"}\n\nid: 2\nevent: succeeded\ndata: {}\n\n")
				writer.Close()
				synctest.Wait()
				if !strings.Contains(out.String(), "  visible\n") {
					t.Errorf("step = %q", out.String())
				}
				if tc.output && !strings.Contains(out.String(), "child\x1b[0m") {
					t.Errorf("child = %q", out.String())
				}
			})
		})
	}
}
