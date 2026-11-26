package cli

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"berthy/internal/agentclient"
	"berthy/internal/api"
)

func TestPollProgressUntilMatchingRun(t *testing.T) {
	t.Setenv("TERM", "xterm")
	t.Setenv("NO_COLOR", "1")
	forceStderrTerminal(t, true)
	forceProgressVT(t)
	old := progressWidth
	progressWidth = func(io.Writer) (int, error) { return 80, nil }
	t.Cleanup(func() { progressWidth = old })
	for _, kind := range []string{"job", "backup", "state backup", "cancellation"} {
		t.Run(kind, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				release := make(chan struct{})
				releaseSecond := make(chan struct{})
				defer func() {
					select {
					case <-releaseSecond:
					default:
						close(releaseSecond)
					}
				}()
				polls := 0
				defer func() {
					select {
					case <-release:
					default:
						close(release)
					}
				}()
				entered := make(chan struct{}, 1)
				base, _ := url.Parse("http://agent.invalid")
				client := &http.Client{Transport: progressRoundTrip(func(r *http.Request) (*http.Response, error) {
					body, status := `[{"id":7,"status":"succeeded"}]`, 200
					if r.Method == "POST" {
						status = 202
						switch kind {
						case "job":
							body = `{"run_id":7}`
						case "backup":
							body = `{"backup_run_id":7}`
						case "state backup":
							body = `{"state_backup_run_id":7}`
						}
					}
					if kind == "job" && strings.HasSuffix(r.URL.Path, "/jobs") {
						body = `[]`
					}
					if kind == "cancellation" {
						body = `{"state":"failed"}`
					}
					var reader io.ReadCloser = io.NopCloser(strings.NewReader(body))
					if strings.Contains(r.URL.Path, "/runs") || kind == "cancellation" {
						polls++
						gate := release
						if polls == 1 {
							if kind == "cancellation" {
								body = `{"state":"running"}`
							} else {
								body = `[{"id":7,"status":"running"}]`
							}
						} else {
							gate = releaseSecond
						}
						reader = &waitBody{r.Context(), entered, gate, strings.NewReader(body)}
					}
					return &http.Response{StatusCode: status, Header: make(http.Header), Body: reader, Request: r}, nil
				})}
				var out, errout progressTestBuffer
				done := make(chan int, 1)
				go func() {
					switch kind {
					case "job":
						done <- runJobRun(ctx, client, base, "", "token", "task", false, &out, &errout)
					case "backup":
						done <- runBackupRun(ctx, agentclient.Caller(ctx, client, base, "", "token"), "task", false, &out, &errout)
					case "state backup":
						done <- runStateBackup(ctx, client, base, "", "token", false, &out, &errout)
					case "cancellation":
						done <- reportCancel(ctx, client, base, "", "token", []byte(`{"operation_id":"op7","kind":"deploy","accepted":true}`), commandFlags{}, &out, &errout)
					}
				}()
				synctest.Wait()
				select {
				case <-entered:
				default:
					cancel()
					close(release)
					<-done
					t.Fatal("status request was not reached")
				}
				time.Sleep(250 * time.Millisecond)
				synctest.Wait()
				want := "\r\x1b[2K| waiting for " + kind + " (0.2s)"
				if got := errout.String(); got != want {
					t.Errorf("frame: got %q want %q", got, want)
				}
				close(release)
				time.Sleep(2 * time.Second)
				synctest.Wait()
				select {
				case <-entered:
				default:
					cancel()
					close(releaseSecond)
					<-done
					t.Fatal("second status request was not reached")
				}
				if got := errout.String(); strings.HasSuffix(got, "\r\x1b[2K") || strings.Contains(got, "\r\x1b[2K\r\x1b[2K") || !strings.Contains(got, "waiting for "+kind) {
					t.Errorf("progress did not span polls: %q", got)
				}
				close(releaseSecond)
				synctest.Wait()
				if code := <-done; code != 0 {
					t.Errorf("exit: %d; output %q; stderr %q", code, out.String(), errout.String())
				}
				if got := errout.String(); !strings.HasSuffix(got, "\r\x1b[2K") {
					t.Errorf("clear: %q", got)
				}
				if kind == "cancellation" {
					if !strings.Contains(out.String(), "operation op7 ended: failed\n") {
						t.Errorf("result: %q", out.String())
					}
				} else if !strings.Contains(out.String(), "run: 7\nstatus: succeeded\n") {
					t.Errorf("result: %q", out.String())
				}
				before := errout.String()
				time.Sleep(time.Second)
				synctest.Wait()
				if errout.String() != before {
					t.Error("late write")
				}
			})
		})
	}
}

func TestPollResultStatusStyle(t *testing.T) {
	t.Setenv("TERM", "xterm")
	t.Setenv("NO_COLOR", "")
	forceProgressVT(t)
	old := progressWidth
	progressWidth = func(io.Writer) (int, error) { return 80, nil }
	t.Cleanup(func() { progressWidth = old })
	for _, print := range []func(io.Writer, io.Writer){
		func(o, e io.Writer) { printRun(o, e, api.JobRun{ID: 7, Status: "succeeded"}, false) },
		func(o, e io.Writer) { printBackupRun(o, e, api.BackupRun{ID: 7, Status: "succeeded"}, false) },
		func(o, e io.Writer) { printStateBackupRun(o, e, api.StateBackupRun{ID: 7, Status: "succeeded"}, false) },
	} {
		var out, errout progressTestBuffer
		print(&out, &errout)
		if !strings.Contains(out.String(), "status: \x1b[32msucceeded\x1b[0m\n") {
			t.Errorf("status style: %q", out.String())
		}
	}
}

func TestPollCancellationClearsProgress(t *testing.T) {
	t.Setenv("TERM", "xterm")
	t.Setenv("NO_COLOR", "1")
	forceStderrTerminal(t, true)
	forceProgressVT(t)
	old := progressWidth
	progressWidth = func(io.Writer) (int, error) { return 80, nil }
	t.Cleanup(func() { progressWidth = old })
	for _, kind := range []string{"job", "backup", "state backup", "cancellation"} {
		t.Run(kind, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				gate := make(chan struct{})
				defer close(gate)
				entered := make(chan struct{}, 1)
				base, _ := url.Parse("http://agent.invalid")
				client := &http.Client{Transport: progressRoundTrip(func(r *http.Request) (*http.Response, error) {
					body, status := `[]`, 200
					if r.Method == "POST" {
						status = 202
						switch kind {
						case "job":
							body = `{"run_id":7}`
						case "backup":
							body = `{"backup_run_id":7}`
						case "state backup":
							body = `{"state_backup_run_id":7}`
						}
					}
					var rd io.ReadCloser = io.NopCloser(strings.NewReader(body))
					if strings.Contains(r.URL.Path, "/runs") || kind == "cancellation" {
						rd = &waitBody{r.Context(), entered, gate, strings.NewReader(body)}
					}
					return &http.Response{StatusCode: status, Header: make(http.Header), Body: rd, Request: r}, nil
				})}
				var out, errout progressTestBuffer
				done := make(chan int, 1)
				go func() {
					switch kind {
					case "job":
						done <- runJobRun(ctx, client, base, "", "token", "task", false, &out, &errout)
					case "backup":
						done <- runBackupRun(ctx, agentclient.Caller(ctx, client, base, "", "token"), "task", false, &out, &errout)
					case "state backup":
						done <- runStateBackup(ctx, client, base, "", "token", false, &out, &errout)
					case "cancellation":
						done <- reportCancel(ctx, client, base, "", "token", []byte(`{"operation_id":"op7","kind":"deploy","accepted":true}`), commandFlags{}, &out, &errout)
					}
				}()
				synctest.Wait()
				select {
				case <-entered:
				default:
					cancel()
					<-done
					t.Fatal("status request was not reached")
				}
				time.Sleep(250 * time.Millisecond)
				synctest.Wait()
				cancel()
				synctest.Wait()
				if code := <-done; code != 1 {
					t.Errorf("exit: %d", code)
				}
				diagnostic := "stopped waiting; run 7 continues on the agent\n"
				if kind == "cancellation" {
					diagnostic = "stopped waiting; operation op7 goes on ending on the agent\n"
				}
				want := "\r\x1b[2K| waiting for " + kind + " (0.2s)\r\x1b[2K" + diagnostic
				if got := errout.String(); got != want {
					t.Errorf("stderr: got %q want %q", got, want)
				}
				before := errout.String()
				time.Sleep(time.Second)
				synctest.Wait()
				if errout.String() != before {
					t.Error("late write")
				}
			})
		})
	}
}

func TestPollSelectsAdmittedRun(t *testing.T) {
	for _, kind := range []string{"job", "backup", "state backup"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			base, _ := url.Parse("http://agent.invalid")
			var queries []string
			client := &http.Client{Transport: progressRoundTrip(func(r *http.Request) (*http.Response, error) {
				body, status := `[]`, 200
				if r.Method == "POST" {
					status = 202
					switch kind {
					case "job":
						body = `{"run_id":7}`
					case "backup":
						body = `{"backup_run_id":7}`
					case "state backup":
						body = `{"state_backup_run_id":7}`
					}
				} else if strings.Contains(r.URL.Path, "/runs") {
					queries = append(queries, r.URL.RawQuery)
					body = `[{"id":9,"status":"failed"}]`
					if r.URL.RawQuery == "limit=100" {
						body = `[{"id":9,"status":"failed"},{"id":7,"status":"succeeded"}]`
					}
				}
				return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
			})}
			var out, errout progressTestBuffer
			var code int
			switch kind {
			case "job":
				code = runJobRun(ctx, client, base, "", "token", "task", false, &out, &errout)
			case "backup":
				code = runBackupRun(ctx, agentclient.Caller(ctx, client, base, "", "token"), "task", false, &out, &errout)
			case "state backup":
				code = runStateBackup(ctx, client, base, "", "token", false, &out, &errout)
			}
			if code != 0 || !strings.Contains(out.String(), "run: 7\nstatus: succeeded\n") {
				t.Fatalf("exit %d out %q err %q", code, out.String(), errout.String())
			}
			want := "limit=1,limit=100"
			if kind == "state backup" {
				want = "limit=100"
			}
			if strings.Join(queries, ",") != want {
				t.Errorf("queries: %v want %s", queries, want)
			}
		})
	}
}

func TestPollOutcomeProgress(t *testing.T) {
	t.Setenv("TERM", "xterm")
	t.Setenv("NO_COLOR", "1")
	forceStderrTerminal(t, true)
	forceProgressVT(t)
	oldWidth := progressWidth
	progressWidth = func(io.Writer) (int, error) { return 80, nil }
	t.Cleanup(func() { progressWidth = oldWidth })
	oldBackup, oldGrace, oldCancel := backupWaitLimit, jobWaitGrace, cancelWait
	backupWaitLimit, jobWaitGrace, cancelWait = time.Second, 0, time.Second
	t.Cleanup(func() { backupWaitLimit, jobWaitGrace, cancelWait = oldBackup, oldGrace, oldCancel })
	for _, kind := range []string{"job", "backup", "state backup", "cancellation"} {
		for _, scenario := range []string{"failure", "timeout", "retry", "json", "held"} {
			if kind == "cancellation" && scenario == "held" {
				continue
			}
			t.Run(kind+"/"+scenario, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					gate := make(chan struct{})
					defer func() {
						select {
						case <-gate:
						default:
							close(gate)
						}
					}()
					entered := make(chan struct{}, 1)
					base, _ := url.Parse("http://agent.invalid")
					polls := 0
					client := &http.Client{Transport: progressRoundTrip(func(r *http.Request) (*http.Response, error) {
						status, body := 200, `[]`
						if r.Method == "POST" {
							status = 202
							switch kind {
							case "job":
								body = `{"run_id":7}`
							case "backup":
								body = `{"backup_run_id":7}`
							case "state backup":
								body = `{"state_backup_run_id":7}`
							}
							if scenario == "held" {
								body = `{"approval_id":"a7","expires":"2030-01-01T00:00:00Z"}`
							}
						}
						if kind == "job" && strings.HasSuffix(r.URL.Path, "/jobs") {
							body = `[{"name":"task","timeout":"1s"}]`
						}
						gated := strings.Contains(r.URL.Path, "/runs") || kind == "cancellation"
						if gated {
							polls++
							state := "succeeded"
							if scenario == "failure" {
								state = "failed"
							}
							if scenario == "timeout" {
								state = "running"
							}
							if kind == "cancellation" {
								if state == "succeeded" || state == "failed" {
									state = "failed"
								}
								body = `{"state":"` + state + `"}`
							} else {
								body = `[{"id":7,"status":"` + state + `"}]`
							}
							if scenario == "retry" && polls == 1 {
								status = 503
								body = `{}`
							}
						}
						var rd io.ReadCloser = io.NopCloser(strings.NewReader(body))
						if gated && polls == 1 {
							rd = &waitBody{r.Context(), entered, gate, strings.NewReader(body)}
						}
						return &http.Response{StatusCode: status, Header: make(http.Header), Body: rd, Request: r}, nil
					})}
					var combined progressTestBuffer
					done := make(chan int, 1)
					asJSON := scenario == "json"
					go func() {
						switch kind {
						case "job":
							done <- runJobRun(ctx, client, base, "", "token", "task", asJSON, &combined, &combined)
						case "backup":
							done <- runBackupRun(ctx, agentclient.Caller(ctx, client, base, "", "token"), "task", asJSON, &combined, &combined)
						case "state backup":
							done <- runStateBackup(ctx, client, base, "", "token", asJSON, &combined, &combined)
						case "cancellation":
							done <- reportCancel(ctx, client, base, "", "token", []byte(`{"operation_id":"op7","kind":"deploy","accepted":true}`), commandFlags{json: asJSON}, &combined, &combined)
						}
					}()
					synctest.Wait()
					if scenario == "held" {
						var code int
						select {
						case code = <-done:
						default:
							cancel()
							close(gate)
							<-done
							t.Fatal("held admission continued polling")
						}
						if code != exitHeld || !strings.Contains(combined.String(), "held for approval: a7") || strings.Contains(combined.String(), "\x1b") || polls != 0 {
							t.Fatalf("held: exit %d polls %d output %q", code, polls, combined.String())
						}
						return
					}
					select {
					case <-entered:
					default:
						cancel()
						<-done
						t.Fatal("status body not entered")
					}
					time.Sleep(250 * time.Millisecond)
					synctest.Wait()
					before := combined.String()
					if asJSON {
						if strings.Contains(before, "\x1b") {
							t.Errorf("JSON frame: %q", before)
						}
					} else if !strings.Contains(before, "waiting for "+kind) {
						t.Errorf("missing frame: %q", before)
					}
					close(gate)
					time.Sleep(3 * time.Second)
					synctest.Wait()
					var code int
					select {
					case code = <-done:
					default:
						cancel()
						<-done
						t.Fatal("poll did not finish")
					}
					got := combined.String()
					if scenario == "timeout" {
						if code != 1 || !strings.Contains(got, "did not finish in time") && !strings.Contains(got, "has not ended yet") {
							t.Errorf("timeout exit %d: %q", code, got)
						}
					} else if scenario == "failure" && kind != "cancellation" {
						if code != 1 || !strings.Contains(got, "status: failed") {
							t.Errorf("failure exit %d: %q", code, got)
						}
					} else if code != 0 {
						t.Errorf("exit %d: %q", code, got)
					}
					if scenario == "retry" && polls != 2 {
						t.Errorf("retry polls=%d", polls)
					}
					if asJSON {
						if strings.Contains(got, "\x1b") {
							t.Errorf("JSON escape: %q", got)
						}
						if kind != "cancellation" && (!strings.Contains(got, `"id": 7`) || !strings.Contains(got, `"status": "succeeded"`)) {
							t.Errorf("JSON result: %q", got)
						}
						if kind == "cancellation" && !strings.Contains(got, "operation op7 ended: failed") {
							t.Errorf("cancel result: %q", got)
						}
					} else {
						marker := "run: 7"
						if kind == "cancellation" {
							marker = "operation op7 ended:"
						}
						if scenario == "timeout" {
							marker = "run 7 did not finish"
							if kind == "cancellation" {
								marker = "operation op7 has not ended"
							}
						}
						if !strings.Contains(got, "\r\x1b[2K"+marker) {
							t.Errorf("clear before output: %q", got)
						}
					}
					final := combined.String()
					time.Sleep(time.Second)
					synctest.Wait()
					if combined.String() != final {
						t.Error("late write")
					}
				})
			})
		}
	}
}
