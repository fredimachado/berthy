package cli

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"berthy/internal/agentclient"
	"berthy/internal/deploy"
	"berthy/internal/gitcheck"
	"berthy/internal/manifest"
)

func TestLegacyApprovalAndBackupRefusals(t *testing.T) {
	for _, cmd := range [][]string{{"approvals"}, {"backup", "list"}} {
		for _, confirmed := range []bool{false, true} {
			for _, mode := range []string{"terminal", "pipe", "json", "no_color", "overlap"} {
				t.Run(fmt.Sprintf("%v/confirmed=%t/%s", cmd, confirmed, mode), func(t *testing.T) {
					calls := 0
					srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						calls++
						if r.URL.Path == "/v1/environments/dev/agent" && confirmed {
							fmt.Fprint(w, `{"version":"v0.8.0"}`)
							return
						}
						w.WriteHeader(http.StatusNotFound)
						fmt.Fprintf(w, `{"message":%q}`, agentclient.OlderAgentMessage)
					}))
					defer srv.Close()
					cfg := ingressCLIConfig(t, srv.URL)
					var out, errs bytes.Buffer
					if mode != "pipe" {
						slice5Destination(t, &errs)
					}
					if mode == "no_color" {
						t.Setenv("NO_COLOR", "1")
					}
					if mode == "overlap" {
						t.Setenv("BERTHY_TEST_TOKEN", "release")
					}
					args := append(append([]string{}, cmd...), "-e", "dev", "--config", cfg)
					if mode == "json" {
						args = append(args, "--json")
					}
					code := Run(args, &out, &errs)
					lead := "agent returned HTTP 404: "
					hint := agentclient.OlderAgentMessage
					if mode == "overlap" {
						hint = "the agent runs an older [redacted] than this CLI; upgrade it with berthy setup"
					}
					want := lead + hint + "\n"
					if mode == "terminal" {
						if confirmed {
							want = "\x1b[31m" + lead + "\x1b[0m\x1b[36m" + hint + "\x1b[0m\n"
						} else {
							want = "\x1b[31m" + lead + hint + "\x1b[0m\n"
						}
					}
					if mode == "overlap" {
						want = "\x1b[31m" + lead + hint + "\x1b[0m\n"
					}
					if code != 1 || out.Len() != 0 || errs.String() != want || calls != 2 {
						t.Fatalf("exit %d stdout %q stderr %q want %q calls %d", code, out.String(), errs.String(), want, calls)
					}
				})
			}
		}
	}
}

func TestLegacyRefusalSiblings(t *testing.T) {
	for _, tc := range []struct {
		name         string
		args         []string
		path, method string
		requests     int
	}{
		{"reject", []string{"reject", "ap1"}, "/approvals/ap1/reject", "POST", 2},
		{"approve", []string{"approve", "ap1"}, "/approvals/ap1/approve", "POST", 4},
		{"backup run", []string{"backup", "run", "daily"}, "/backups/daily/run", "POST", 2},
		{"job run", []string{"job", "run", "daily"}, "/jobs/daily/run", "POST", 2},
		{"job cancel", []string{"job", "cancel", "daily"}, "/jobs/daily/cancel", "POST", 2},
		{"state backup", []string{"state-backup"}, "/state-backup", "POST", 2},
	} {
		for _, confirmed := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/confirmed=%t", tc.name, confirmed), func(t *testing.T) {
				var calls []string
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls = append(calls, r.Method+" "+r.URL.Path)
					if r.URL.Path == "/v1/environments/dev/agent" && confirmed {
						fmt.Fprint(w, `{"version":"v0.8.0"}`)
						return
					}
					w.WriteHeader(404)
					fmt.Fprintf(w, `{"message":%q}`, agentclient.OlderAgentMessage)
				}))
				defer srv.Close()
				cfg := ingressCLIConfig(t, srv.URL)
				var out, errs bytes.Buffer
				slice5Destination(t, &errs)
				args := append(append([]string{}, tc.args...), "-e", "dev", "--config", cfg)
				code := Run(args, &out, &errs)
				lead := "agent returned HTTP 404: "
				want := "\x1b[31m" + lead + agentclient.OlderAgentMessage + "\x1b[0m\n"
				if confirmed {
					want = "\x1b[31m" + lead + "\x1b[0m\x1b[36m" + agentclient.OlderAgentMessage + "\x1b[0m\n"
				}
				if code != 1 || out.Len() != 0 || errs.String() != want || len(calls) != tc.requests || !strings.HasSuffix(calls[len(calls)-2], tc.method+" /v1/projects/demo/environments/dev"+tc.path) {
					t.Fatalf("exit %d out %q err %q want %q calls %v", code, out.String(), errs.String(), want, calls)
				}
			})
		}
	}
}

func TestLegacyOperationStreamRefusal(t *testing.T) {
	for _, confirmed := range []bool{false, true} {
		t.Run(fmt.Sprint(confirmed), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/environments/dev/agent" && confirmed {
					fmt.Fprint(w, `{}`)
					return
				}
				w.WriteHeader(404)
				fmt.Fprintf(w, `{"message":%q}`, agentclient.OlderAgentMessage)
			}))
			defer srv.Close()
			base, _ := url.Parse(srv.URL)
			var out, errs bytes.Buffer
			slice5Destination(t, &errs)
			code := watchOperation(context.Background(), srv.Client(), base, "fixture-token", "/v1/projects/demo/environments/dev/", "op1", "/v1/projects/demo/environments/dev/operations/op1/events", &out, &errs, false, "restart", "dev", "demo")
			lead := "agent returned HTTP 404: "
			want := "\x1b[31m" + lead + agentclient.OlderAgentMessage + "\x1b[0m\n"
			if confirmed {
				want = "\x1b[31m" + lead + "\x1b[0m\x1b[36m" + agentclient.OlderAgentMessage + "\x1b[0m\n"
			}
			if code != 1 || out.Len() != 0 || errs.String() != want {
				t.Fatalf("exit %d out %q err %q want %q", code, out.String(), errs.String(), want)
			}
		})
	}
}

func TestLegacyProtectionRefusalProvenance(t *testing.T) {
	for _, confirmed := range []bool{false, true} {
		t.Run(fmt.Sprint(confirmed), func(t *testing.T) {
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.URL.Path == "/v1/environments/dev/agent" && confirmed {
					fmt.Fprint(w, `{}`)
					return
				}
				w.WriteHeader(404)
				fmt.Fprintf(w, `{"message":%q}`, agentclient.OlderAgentMessage)
			}))
			defer srv.Close()
			base, _ := url.Parse(srv.URL)
			target := &agentclient.Target{Base: base, Prefix: "/v1/projects/demo/environments/dev/", Token: "fixture-token"}
			_, err := agentclient.AgentProtection(context.Background(), srv.Client(), target)
			if err == nil {
				t.Fatal("missing refusal")
			}
			var out bytes.Buffer
			slice5Destination(t, &out)
			printAgentRefusal(&out, terminalSafe(err.Error()), true, agentclient.IsOlderAgentError(err))
			lead := "agent returned HTTP 404: "
			want := "\x1b[31m" + lead + agentclient.OlderAgentMessage + "\x1b[0m\n"
			if confirmed {
				want = "\x1b[31m" + lead + "\x1b[0m\x1b[36m" + agentclient.OlderAgentMessage + "\x1b[0m\n"
			}
			if calls != 2 || out.String() != want {
				t.Fatalf("calls %d out %q want %q", calls, out.String(), want)
			}
		})
	}
}

func TestLegacyBatchTagLookupRefusal(t *testing.T) {
	for _, confirmed := range []bool{false, true} {
		t.Run(fmt.Sprint(confirmed), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/projects/demo/environments/dev/agent" {
					fmt.Fprint(w, `{"project":"demo","environment":"dev","protected":true,"effective_protected":true}`)
					return
				}
				if r.URL.Path == "/v1/environments/dev/agent" && confirmed {
					fmt.Fprint(w, `{}`)
					return
				}
				w.WriteHeader(404)
				fmt.Fprintf(w, `{"message":%q}`, agentclient.OlderAgentMessage)
			}))
			defer srv.Close()
			base, _ := url.Parse(srv.URL)
			target := &agentclient.Target{Base: base, Prefix: "/v1/projects/demo/environments/dev/", Project: "demo", Env: "dev", Token: "fixture-token"}
			m := &manifest.Manifest{Apps: map[string]manifest.App{"api": {Kind: "image", Image: "example.com/api"}}}
			var out, errs bytes.Buffer
			slice5Destination(t, &errs)
			handled, code := prepareBatchTags(context.Background(), srv.Client(), target, commandFlags{dry: true}, m, gitcheck.Checkout{}, []deployStep{{app: "api", req: deploy.Request{App: "api"}}}, map[string]string{"api": "stable"}, &out, &errs)
			lead := "agent returned HTTP 404: "
			want := "\x1b[31m" + lead + agentclient.OlderAgentMessage + "\x1b[0m\n"
			if confirmed {
				want = "\x1b[31m" + lead + "\x1b[0m\x1b[36m" + agentclient.OlderAgentMessage + "\x1b[0m\n"
			}
			if !handled || code != 1 || errs.String() != want {
				t.Fatalf("handled %t exit %d out %q err %q want %q", handled, code, out.String(), errs.String(), want)
			}
		})
	}
}
