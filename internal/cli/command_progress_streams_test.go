package cli

import (
	"berthy/internal/setup"
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

func TestCommandProgressStreams(t *testing.T) {
	t.Setenv("TERM", "xterm")
	t.Setenv("NO_COLOR", "1")
	oldTerminal, oldWidth, oldVT := stderrTerminal, progressWidth, progressVT
	t.Cleanup(func() { stderrTerminal, progressWidth, progressVT = oldTerminal, oldWidth, oldVT })
	for _, command := range []string{"deploy", "approve"} {
		for _, mode := range []string{"stderr terminal", "stdout terminal", "json"} {
			t.Run(command+"/"+mode, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					var out, errs progressTestBuffer
					stderrTerminal = func(w io.Writer) bool {
						return (mode == "stdout terminal" && w == &out) || (mode != "stdout terminal" && w == &errs)
					}
					progressWidth = func(io.Writer) (int, error) { return 100, nil }
					progressVT = func(io.Writer, string) bool { return true }
					release := make(chan struct{})
					done := make(chan int, 1)
					flags := commandFlags{json: mode == "json", pos: []string{"ap1"}}
					base, _ := url.Parse("http://agent.invalid")
					go func() {
						if command == "deploy" {
							client := &http.Client{Transport: progressRoundTrip(func(r *http.Request) (*http.Response, error) {
								<-release
								return &http.Response{StatusCode: 403, Body: io.NopCloser(strings.NewReader(`{"message":"refused"}`)), Header: make(http.Header), Request: r}, nil
							})}
							done <- sendCommand(context.Background(), client, base, "/v1/", "token", "POST", "deploys", nil, flags, "deploy", &out, &errs)
						} else {
							call := func(method, endpoint, query string, body []byte) ([]byte, int, error) {
								if method == "GET" {
									return []byte(`[]`), 200, nil
								}
								<-release
								return []byte(`{"message":"refused"}`), 403, nil
							}
							done <- runApprove(context.Background(), call, &http.Client{}, flags, base, "/v1/", "token", &out, &errs)
						}
					}()
					defer func() { close(release); <-done }()
					synctest.Wait()
					time.Sleep(250 * time.Millisecond)
					synctest.Wait()
					wantErr := ""
					if mode == "stderr terminal" {
						label := "deploying"
						if command == "approve" {
							label = "approving"
						}
						wantErr = "\r\x1b[2K| " + label + " (0.2s)"
					}
					if got := out.String(); got != "" {
						t.Errorf("pending stdout = %q", got)
					}
					if got := errs.String(); got != wantErr {
						t.Errorf("pending stderr = %q, want %q", got, wantErr)
					}
					close(release)
					release = make(chan struct{})
					synctest.Wait()
					if wantErr != "" {
						wantErr += "\r\x1b[2K"
					}
					wantErr += "agent returned HTTP 403: refused\n"
					if got := out.String(); got != "" {
						t.Errorf("final stdout = %q", got)
					}
					if got := errs.String(); got != wantErr {
						t.Errorf("final stderr = %q, want %q", got, wantErr)
					}
				})
			})
		}
	}
}

type streamSetupRunner struct{ release <-chan struct{} }

func (r streamSetupRunner) Target() string { return "ops@s1" }
func (r streamSetupRunner) Run(context.Context, string, io.Reader) (setup.Result, error) {
	<-r.release
	return setup.Result{}, errors.New("fixture failure")
}

func TestSetupCommandProgressStreams(t *testing.T) {
	t.Setenv("TERM", "xterm")
	t.Setenv("NO_COLOR", "1")
	oldTerminal, oldWidth, oldVT, oldRunner := stderrTerminal, progressWidth, progressVT, newSetupRunner
	t.Cleanup(func() {
		stderrTerminal, progressWidth, progressVT, newSetupRunner = oldTerminal, oldWidth, oldVT, oldRunner
	})
	for _, mode := range []string{"stderr terminal", "stdout terminal", "json"} {
		t.Run(mode, func(t *testing.T) {
			args := setupArgs(t, "--config", filepath.Join(t.TempDir(), "config"), "--dry-run")
			if mode == "json" {
				args = append(args, "--json")
			}
			synctest.Test(t, func(t *testing.T) {
				var out, errs progressTestBuffer
				stderrTerminal = func(w io.Writer) bool {
					return (mode == "stdout terminal" && w == &out) || (mode != "stdout terminal" && w == &errs)
				}
				progressWidth = func(io.Writer) (int, error) { return 100, nil }
				progressVT = func(io.Writer, string) bool { return true }
				release := make(chan struct{})
				done := make(chan int, 1)
				newSetupRunner = func(string, bool) (setup.Runner, error) { return streamSetupRunner{release}, nil }
				go func() { done <- runSetupWithContext(context.Background(), args, &out, &errs) }()
				defer func() { close(release); <-done }()
				synctest.Wait()
				time.Sleep(250 * time.Millisecond)
				synctest.Wait()
				banner := ""
				if mode != "stdout terminal" {
					banner = "berthy: shop/p5 on ops@s1\n"
				}
				wantErr := banner
				if mode == "stderr terminal" {
					wantErr += "\r\x1b[2K| connect (0.2s)"
				}
				if got := out.String(); got != "" {
					t.Errorf("pending stdout = %q", got)
				}
				if got := errs.String(); got != wantErr {
					t.Errorf("pending stderr = %q, want %q", got, wantErr)
				}
				close(release)
				release = make(chan struct{})
				synctest.Wait()
				if mode == "stderr terminal" {
					wantErr += "\r\x1b[2K"
				}
				if got := errs.String(); got != wantErr {
					t.Errorf("final stderr = %q, want %q", got, wantErr)
				}
				wantOut := "✖ connect     could not run commands on ops@s1: fixture failure\nsetup of shop/p5 on ops@s1: failed at connect\n"
				if mode == "stdout terminal" {
					wantOut = "failed connect     could not run commands on ops@s1: fixture failure (0.2s)\nsetup of shop/p5 on ops@s1: failed at connect\n"
				}
				if mode == "json" {
					wantOut = "{\n  \"project\": \"shop\",\n  \"environment\": \"p5\",\n  \"target\": \"ops@s1\",\n  \"dry_run\": true,\n  \"steps\": [\n    {\n      \"step\": \"connect\",\n      \"status\": \"failed\",\n      \"detail\": \"could not run commands on ops@s1: fixture failure\"\n    }\n  ],\n  \"ok\": false\n}\n"
				}
				if got := out.String(); got != wantOut {
					t.Errorf("final stdout = %q, want %q", got, wantOut)
				}
			})
		})
	}
}

func TestWatchJSONWithoutProgress(t *testing.T) {
	t.Setenv("TERM", "xterm")
	t.Setenv("NO_COLOR", "1")
	oldTerminal, oldWidth, oldVT := stderrTerminal, progressWidth, progressVT
	t.Cleanup(func() { stderrTerminal, progressWidth, progressVT = oldTerminal, oldWidth, oldVT })
	synctest.Test(t, func(t *testing.T) {
		var out, errs progressTestBuffer
		stderrTerminal = func(w io.Writer) bool { return w == &errs }
		progressWidth = func(io.Writer) (int, error) { return 100, nil }
		progressVT = func(io.Writer, string) bool { return true }
		reader, writer := io.Pipe()
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan int, 1)
		client := &http.Client{Transport: progressRoundTrip(func(r *http.Request) (*http.Response, error) {
			if strings.HasSuffix(r.URL.Path, "/events") {
				return &http.Response{StatusCode: 200, Body: reader, Header: make(http.Header), Request: r}, nil
			}
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"state":"succeeded"}`)), Header: make(http.Header), Request: r}, nil
		})}
		base, _ := url.Parse("http://agent.invalid")
		go func() {
			done <- watchOperation(ctx, client, base, "token", "/v1/", "op", "/v1/operations/op/events", &out, &errs, true, "deploy", "dev", "demo")
		}()
		defer func() { cancel(); writer.Close(); <-done }()
		synctest.Wait()
		time.Sleep(250 * time.Millisecond)
		synctest.Wait()
		if out.String() != "" || errs.String() != "" {
			t.Errorf("pending stdout=%q stderr=%q", out.String(), errs.String())
		}
		io.WriteString(writer, "id: 1\nevent: succeeded\ndata: {}\n\nevent: terminal\ndata: {}\n\n")
		writer.Close()
		synctest.Wait()
		if out.String() != "{\n  \"state\": \"succeeded\"\n}\n" || errs.String() != "" {
			t.Errorf("final stdout=%q stderr=%q", out.String(), errs.String())
		}
	})
}
