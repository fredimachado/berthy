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

func TestDeployProgressResumesAtCompleteEvent(t *testing.T) {
	for _, state := range []string{"succeeded", "failed", "interrupted", "recovery_required"} {
		for _, asJSON := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/json=%v", state, asJSON), func(t *testing.T) {
				streams := 0
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch r.URL.Path {
					case "/v1/operations/op/events":
						streams++
						if streams == 1 {
							fmt.Fprint(w, "id: 1\nevent: accepted\ndata: {}\n\nid: 2\nevent: verify\ndata: {\"step\":\"cut")
						} else {
							if r.Header.Get("Last-Event-ID") != "1" {
								t.Errorf("cursor %q", r.Header.Get("Last-Event-ID"))
							}
							fmt.Fprintf(w, "id: 1\nevent: accepted\ndata: {}\n\nid: 2\nevent: verify\ndata: {\"status\":\"warn\",\"step\":\"warning\"}\n\nid: 3\nevent: %s\ndata: {}\n\nevent: terminal\ndata: {}\n\n", state)
						}
					case "/v1/operations/op":
						fmt.Fprintf(w, `{"state":%q}`, state)
					default:
						t.Errorf("unexpected request %s", r.URL)
					}
				}))
				defer server.Close()
				base, _ := url.Parse(server.URL)
				var out, errs bytes.Buffer
				got := watchOperationWithProgress(context.Background(), server.Client(), base, "token", "/v1/", "op", "/v1/operations/op/events", &out, &errs, asJSON, "deploy", "dev", "demo")
				want := 1
				if state == "succeeded" {
					want = 0
				}
				if got != want || streams != 2 || strings.Contains(out.String(), "cut") || strings.Count(out.String(), "warning") != 1 {
					t.Fatalf("exit=%d streams=%d out=%q err=%q", got, streams, out.String(), errs.String())
				}
				if asJSON {
					var doc struct {
						State  string            `json:"state"`
						Checks []json.RawMessage `json:"checks"`
					}
					if json.Unmarshal(out.Bytes(), &doc) != nil || doc.State != state || len(doc.Checks) != 1 {
						t.Fatalf("output %q", out.String())
					}
				} else if strings.Count(out.String(), "accepted\n") != 1 || strings.Count(out.String(), "terminal\n") != 1 {
					t.Fatalf("transcript %q", out.String())
				}
			})
		}
	}
}

func TestDeployProgressReplayFailureKeepsStatus(t *testing.T) {
	for _, state := range []string{"succeeded", "failed"} {
		t.Run(state, func(t *testing.T) {
			streams := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/events") {
					streams++
					if streams == 1 {
						fmt.Fprint(w, "id: 1\nevent: accepted\ndata: {}\n\nid: 2\nevent: verify\ndata: {")
					} else {
						http.Error(w, "unavailable", 503)
					}
					return
				}
				fmt.Fprintf(w, `{"state":%q}`, state)
			}))
			defer server.Close()
			base, _ := url.Parse(server.URL)
			var out, errs bytes.Buffer
			got := watchOperationWithProgress(context.Background(), server.Client(), base, "token", "/v1/", "op", "/v1/operations/op/events", &out, &errs, true, "deploy", "dev", "demo")
			want := 1
			if state == "succeeded" {
				want = 0
			}
			if got != want || streams != 2 || !json.Valid(out.Bytes()) || !strings.Contains(errs.String(), "op") || !strings.Contains(errs.String(), state) || !strings.Contains(errs.String(), "incomplete") {
				t.Fatalf("exit=%d streams=%d out=%q err=%q", got, streams, out.String(), errs.String())
			}
		})
	}
}

func TestDeployProgressMalformedStreamAndRedaction(t *testing.T) {
	for _, asJSON := range []bool{false, true} {
		t.Run(fmt.Sprint(asJSON), func(t *testing.T) {
			streams := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/events") {
					streams++
					if streams == 1 {
						fmt.Fprint(w, "id: 1\nevent: accepted\ndata: {}\n\nid: 2\nevent: verify\ndata: {\n\n")
						return
					}
					if r.Header.Get("Last-Event-ID") != "1" {
						t.Errorf("cursor %q", r.Header.Get("Last-Event-ID"))
					}
					fmt.Fprint(w, "id: 2\nevent: verify\ndata: {\"step\":\"a secret\\u001b[31m\"}\n\nid: 3\nevent: succeeded\ndata: {}\n\n")
					return
				}
				fmt.Fprint(w, `{"state":"succeeded","reason":"secret\u001b[31m"}`)
			}))
			defer server.Close()
			base, _ := url.Parse(server.URL)
			var out, errs bytes.Buffer
			got := watchOperationWithProgress(context.Background(), server.Client(), base, "secret", "/v1/", "op", "/v1/operations/op/events", &out, &errs, asJSON, "deploy", "dev", "demo")
			if got != 0 || streams != 2 || strings.Contains(out.String()+errs.String(), "secret") || strings.Contains(out.String()+errs.String(), "\x1b") || !strings.Contains(out.String(), "[redacted]") {
				t.Fatalf("exit=%d streams=%d out=%q err=%q", got, streams, out.String(), errs.String())
			}
		})
	}
}

func TestDeployProgressCancellationNamesOperation(t *testing.T) {
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
	}))
	defer server.Close()
	base, _ := url.Parse(server.URL)
	var out, errs bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() {
		done <- watchOperationWithProgress(ctx, server.Client(), base, "token", "/v1/", "op", "/v1/operations/op/events", &out, &errs, false, "deploy", "dev", "demo")
	}()
	<-started
	cancel()
	got := <-done
	if got != 1 || !strings.Contains(errs.String(), "op") || !strings.Contains(errs.String(), "berthy diagnostics") || !strings.Contains(errs.String(), "berthy cancel") || !strings.Contains(errs.String(), "may continue") {
		t.Fatalf("exit=%d out=%q err=%q", got, out.String(), errs.String())
	}
}

func TestDeployProgressTerminalMessages(t *testing.T) {
	forceStderrTerminal(t, true)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/events") {
			if r.URL.RawQuery != "progress=1" {
				t.Errorf("query %q", r.URL.RawQuery)
			}
			fmt.Fprint(w, "id: 1\nevent: rollout\ndata: {\"role\":\"web\",\"status\":\"started\"}\n\nid: 2\nevent: rollout\ndata: {\"role\":\"web\",\"status\":\"ready\"}\n\nid: 3\nevent: step\ndata: {\"step\":\"retry hook\"}\n\nid: 4\nevent: verify\ndata: {\"step\":\"warning\"}\n\nid: 5\nevent: succeeded\ndata: {}\n\nevent: terminal\ndata: {}\n\n")
			return
		}
		fmt.Fprint(w, `{"state":"succeeded"}`)
	}))
	defer server.Close()
	base, _ := url.Parse(server.URL)
	var out, errs bytes.Buffer
	got := watchOperationWithProgress(context.Background(), server.Client(), base, "token", "/v1/", "op", "/v1/operations/op/events", &out, &errs, false, "deploy", "dev", "demo")
	if got != 0 || strings.Count(out.String(), "retry hook") != 1 || strings.Count(out.String(), "warning") != 1 || strings.Contains(out.String(), "rollout\n") || strings.Contains(out.String(), "terminal\n") {
		t.Fatalf("exit=%d out=%q err=%q", got, out.String(), errs.String())
	}
}

func TestDeployProgressReplaysCleanStreamWithoutFinal(t *testing.T) {
	streams := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/events") {
			streams++
			if r.URL.RawQuery != "" {
				t.Errorf("unexpected query %q", r.URL.RawQuery)
			}
			if streams == 1 {
				fmt.Fprint(w, "id: 1\nevent: accepted\ndata: {}\n\nevent: terminal\ndata: {}\n\n")
				return
			}
			if r.Header.Get("Last-Event-ID") != "1" {
				t.Errorf("cursor %q", r.Header.Get("Last-Event-ID"))
			}
			fmt.Fprint(w, "id: 2\nevent: succeeded\ndata: {}\n\nevent: terminal\ndata: {}\n\n")
			return
		}
		fmt.Fprint(w, `{"state":"succeeded"}`)
	}))
	defer server.Close()
	base, _ := url.Parse(server.URL)
	var out, errs bytes.Buffer
	got := watchOperationWithProgress(context.Background(), server.Client(), base, "token", "/v1/", "op", "/v1/operations/op/events", &out, &errs, false, "deploy", "dev", "demo")
	if got != 0 || streams != 2 || strings.Count(out.String(), "terminal\n") != 1 || errs.Len() != 0 {
		t.Fatalf("exit=%d streams=%d out=%q err=%q", got, streams, out.String(), errs.String())
	}
}

func TestDeployProgressDoesNotTrustStreamOutcome(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/events") {
			fmt.Fprint(w, "id: 1\nevent: succeeded\ndata: {}\n\nevent: terminal\ndata: {\"status\":\"succeeded\"}\n\n")
			return
		}
		cancel()
	}))
	defer server.Close()
	base, _ := url.Parse(server.URL)
	var out, errs bytes.Buffer
	got := watchOperationWithProgress(ctx, server.Client(), base, "token", "/v1/", "op", "/v1/operations/op/events", &out, &errs, true, "deploy", "dev", "demo")
	if got != 1 || out.Len() != 0 || !strings.Contains(errs.String(), "may continue") {
		t.Fatalf("exit=%d out=%q err=%q", got, out.String(), errs.String())
	}
}

func TestDeployProgressPreservesRunOutput(t *testing.T) {
	for _, asJSON := range []bool{false, true} {
		t.Run(fmt.Sprint(asJSON), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v1/operations/op/events":
					fmt.Fprint(w, "id: 1\nevent: succeeded\ndata: {}\n\n")
				case "/v1/operations/op":
					fmt.Fprint(w, `{"state":"succeeded"}`)
				case "/v1/operations/op/output":
					fmt.Fprint(w, `{"output":"run output\n","exit_code":3}`)
				default:
					t.Errorf("unexpected request %s", r.URL)
				}
			}))
			defer server.Close()
			base, _ := url.Parse(server.URL)
			var out, errs bytes.Buffer
			got := watchOperationWithProgress(context.Background(), server.Client(), base, "token", "/v1/", "op", "/v1/operations/op/events", &out, &errs, asJSON, "run", "dev", "demo")
			if got != 1 || !strings.Contains(out.String(), "run output") || errs.Len() != 0 || (asJSON && !json.Valid(out.Bytes())) {
				t.Fatalf("exit=%d out=%q err=%q", got, out.String(), errs.String())
			}
		})
	}
}

func TestDeployProgressCancelledReplayKeepsKnownStatus(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	streams := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/events") {
			streams++
			if streams == 1 {
				fmt.Fprint(w, "id: 1\nevent: accepted\ndata: {}\n\nid: 2\nevent: verify\ndata: {")
				return
			}
			cancel()
			return
		}
		fmt.Fprint(w, `{"state":"succeeded"}`)
	}))
	defer server.Close()
	base, _ := url.Parse(server.URL)
	var out, errs bytes.Buffer
	got := watchOperationWithProgress(ctx, server.Client(), base, "token", "/v1/", "op", "/v1/operations/op/events", &out, &errs, true, "deploy", "dev", "demo")
	if got != 0 || streams != 2 || !json.Valid(out.Bytes()) || !strings.Contains(errs.String(), "incomplete") || !strings.Contains(errs.String(), "succeeded") {
		t.Fatalf("exit=%d streams=%d out=%q err=%q", got, streams, out.String(), errs.String())
	}
}
