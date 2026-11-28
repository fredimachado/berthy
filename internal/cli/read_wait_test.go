package cli

import (
	"berthy/internal/dialtest"
	"berthy/internal/scope"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"testing/synctest"
	"time"
)

type waitBody struct {
	ctx     context.Context
	entered chan struct{}
	release chan struct{}
	reader  io.Reader
}

func (b *waitBody) Read(p []byte) (int, error) {
	select {
	case b.entered <- struct{}{}:
	default:
	}
	select {
	case <-b.release:
		return b.reader.Read(p)
	case <-b.ctx.Done():
		return 0, b.ctx.Err()
	}
}
func (b *waitBody) Close() error { return nil }

func TestReadWaitStopsBeforeResponse(t *testing.T) {
	t.Setenv("TERM", "xterm")
	t.Setenv("NO_COLOR", "1")
	forceStderrTerminal(t, true)
	forceProgressVT(t)
	old := progressWidth
	progressWidth = func(io.Writer) (int, error) { return 80, nil }
	t.Cleanup(func() { progressWidth = old })
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		release := make(chan struct{})
		defer close(release)
		entered := make(chan struct{}, 1)
		client := &http.Client{Transport: progressRoundTrip(func(r *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: &waitBody{r.Context(), entered, release, strings.NewReader(`{"state":"healthy"}`)}, Request: r}, nil
		})}
		u, _ := url.Parse("http://agent.invalid/ps")
		var out, errout progressTestBuffer
		done := make(chan int, 1)
		go func() { done <- runReadRequest(ctx, client, u, "token", "ps", scope.ID{}, false, false, &out, &errout) }()
		<-entered
		time.Sleep(250 * time.Millisecond)
		synctest.Wait()
		if got := errout.String(); got != "\r\x1b[2K| reading ps (0.2s)" {
			t.Errorf("first frame: %q", got)
		}
		cancel()
		synctest.Wait()
		if code := <-done; code != 1 {
			t.Errorf("exit=%d", code)
		}
		if got := errout.String(); got != "\r\x1b[2K| reading ps (0.2s)\r\x1b[2Kagent response could not be read\n" {
			t.Errorf("clear before diagnostic: %q", got)
		}
		before := errout.String()
		time.Sleep(time.Second)
		synctest.Wait()
		if errout.String() != before {
			t.Error("write after return")
		}
	})
}

func TestReadWaitResultModes(t *testing.T) {
	t.Setenv("TERM", "xterm")
	t.Setenv("NO_COLOR", "1")
	forceStderrTerminal(t, true)
	forceProgressVT(t)
	old := progressWidth
	progressWidth = func(io.Writer) (int, error) { return 80, nil }
	t.Cleanup(func() { progressWidth = old })
	for _, tc := range []struct {
		name               string
		json, stream, fast bool
		body, want         string
		frame              bool
	}{
		{"slow", false, false, false, `{"state":"healthy"}`, "state: healthy\n", true},
		{"fast", false, false, true, `{"state":"healthy"}`, "state: healthy\n", false},
		{"json", true, false, false, `{"state":"healthy"}`, "{\n  \"state\": \"healthy\"\n}\n", false},
		{"stream", false, true, false, "event: done\ndata: {\"complete\":true}\n\n", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				release := make(chan struct{})
				released := false
				defer func() {
					if !released {
						close(release)
					}
				}()
				entered := make(chan struct{}, 1)
				client := &http.Client{Transport: progressRoundTrip(func(r *http.Request) (*http.Response, error) {
					return &http.Response{StatusCode: 200, Header: make(http.Header), Body: &waitBody{r.Context(), entered, release, strings.NewReader(tc.body)}, Request: r}, nil
				})}
				u, _ := url.Parse("http://agent.invalid/ps")
				var out, errs progressTestBuffer
				done := make(chan int, 1)
				go func() {
					done <- runReadRequest(ctx, client, u, "token", "ps", scope.ID{}, tc.stream, tc.json, &out, &errs)
				}()
				<-entered
				if !tc.fast {
					time.Sleep(250 * time.Millisecond)
					synctest.Wait()
				}
				wantErr := ""
				if tc.frame {
					wantErr = "\r\x1b[2K| reading ps (0.2s)"
				}
				if errs.String() != wantErr {
					t.Errorf("frame=%q want %q", errs.String(), wantErr)
				}
				close(release)
				released = true
				synctest.Wait()
				if code := <-done; code != 0 {
					t.Errorf("exit=%d", code)
				}
				if tc.frame {
					wantErr += "\r\x1b[2K"
				}
				if errs.String() != wantErr || out.String() != tc.want {
					t.Errorf("out=%q err=%q", out.String(), errs.String())
				}
				time.Sleep(time.Second)
				synctest.Wait()
				if errs.String() != wantErr {
					t.Errorf("late write=%q", errs.String())
				}
			})
		})
	}
}

func TestRequestWaitLabels(t *testing.T) {
	for _, tc := range []struct {
		cmd    string
		pos    []string
		method string
		dry    bool
		want   string
	}{
		{"wait", nil, "GET", false, "waiting for app"}, {"ingress", []string{"status"}, "GET", false, "reading ingress status"},
		{"alerts", []string{"test"}, "POST", false, "testing alerts"}, {"key", []string{"rotate"}, "POST", false, "rotating key"},
		{"secret", []string{"set", "name"}, "POST", false, "storing secret"}, {"deploy", nil, "POST", false, "deploying"},
		{"deploy", nil, "POST", true, "checking deploy"}, {"ingress", []string{"bogus"}, "GET", false, ""},
	} {
		if got := requestWaitLabel(tc.cmd, commandFlags{pos: tc.pos, dry: tc.dry}, tc.method); got != tc.want {
			t.Errorf("%s %v: %q want %q", tc.cmd, tc.pos, got, tc.want)
		}
	}
}

func TestWaitCommandClearsBeforeRefusal(t *testing.T) {
	t.Setenv("TERM", "xterm")
	t.Setenv("NO_COLOR", "1")
	forceStderrTerminal(t, true)
	forceProgressVT(t)
	old := progressWidth
	progressWidth = func(io.Writer) (int, error) { return 80, nil }
	t.Cleanup(func() { progressWidth = old })
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		release := make(chan struct{})
		released := false
		defer func() {
			if !released {
				close(release)
			}
		}()
		entered := make(chan struct{}, 1)
		c := &http.Client{Transport: progressRoundTrip(func(r *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 403, Header: make(http.Header), Body: &waitBody{r.Context(), entered, release, strings.NewReader(`{"message":"refused"}`)}, Request: r}, nil
		})}
		u, _ := url.Parse("http://agent.invalid")
		var out, errs progressTestBuffer
		done := make(chan int, 1)
		go func() {
			done <- sendCommand(ctx, c, u, "/v1/", "token", "GET", "wait", nil, commandFlags{}, "wait", &out, &errs)
		}()
		<-entered
		time.Sleep(250 * time.Millisecond)
		synctest.Wait()
		if got := errs.String(); got != "\r\x1b[2K| waiting for app (0.2s)" {
			t.Errorf("frame=%q", got)
		}
		close(release)
		released = true
		synctest.Wait()
		if code := <-done; code != 1 {
			t.Errorf("exit=%d", code)
		}
		want := "\r\x1b[2K| waiting for app (0.2s)\r\x1b[2Kagent returned HTTP 403: refused\n"
		if errs.String() != want || out.String() != "" {
			t.Errorf("out=%q err=%q", out.String(), errs.String())
		}
		time.Sleep(time.Second)
		synctest.Wait()
		if errs.String() != want {
			t.Error("late write")
		}
	})
}

func TestDeployWaitClearsBeforeWatcher(t *testing.T) {
	t.Setenv("TERM", "xterm")
	t.Setenv("NO_COLOR", "1")
	forceStderrTerminal(t, true)
	forceProgressVT(t)
	old := progressWidth
	progressWidth = func(io.Writer) (int, error) { return 80, nil }
	t.Cleanup(func() { progressWidth = old })
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		release := make(chan struct{})
		released := false
		defer func() {
			if !released {
				close(release)
			}
		}()
		entered := make(chan struct{}, 1)
		var out, errs progressTestBuffer
		var atHandoff string
		c := &http.Client{Transport: progressRoundTrip(func(r *http.Request) (*http.Response, error) {
			status := 200
			var body io.ReadCloser
			switch r.URL.Path {
			case "/v1/deploys":
				status = 202
				body = &waitBody{r.Context(), entered, release, strings.NewReader(`{"operation_id":"op","events_url":"/v1/operations/op/events"}`)}
			case "/v1/operations/op/events":
				atHandoff = errs.String()
				body = io.NopCloser(strings.NewReader("id: 1\nevent: succeeded\ndata: {}\n\nevent: terminal\ndata: {}\n\n"))
			default:
				body = io.NopCloser(strings.NewReader(`{"state":"succeeded"}`))
			}
			return &http.Response{StatusCode: status, Header: make(http.Header), Body: body, Request: r}, nil
		})}
		u, _ := url.Parse("http://agent.invalid")
		done := make(chan int, 1)
		go func() {
			done <- sendCommand(ctx, c, u, "/v1/", "token", "POST", "deploys", nil, commandFlags{}, "deploy", &out, &errs)
		}()
		<-entered
		time.Sleep(250 * time.Millisecond)
		synctest.Wait()
		close(release)
		released = true
		synctest.Wait()
		if code := <-done; code != 0 {
			t.Errorf("exit=%d err=%q", code, errs.String())
		}
		want := "\r\x1b[2K| deploying (0.2s)\r\x1b[2K"
		if atHandoff != want {
			t.Errorf("handoff=%q", atHandoff)
		}
		if got := out.String(); got != "state: succeeded\n" {
			t.Errorf("result=%q", got)
		}
		before := errs.String()
		time.Sleep(time.Second)
		synctest.Wait()
		if errs.String() != before {
			t.Error("late write")
		}
	})
}

type brokenWaitReader struct{}

func (brokenWaitReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestReadWaitClearsBeforeReadFailure(t *testing.T) {
	t.Setenv("TERM", "xterm")
	t.Setenv("NO_COLOR", "1")
	forceStderrTerminal(t, true)
	forceProgressVT(t)
	old := progressWidth
	progressWidth = func(io.Writer) (int, error) { return 80, nil }
	t.Cleanup(func() { progressWidth = old })
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		release := make(chan struct{})
		released := false
		defer func() {
			if !released {
				close(release)
			}
		}()
		entered := make(chan struct{}, 1)
		c := &http.Client{Transport: progressRoundTrip(func(r *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: &waitBody{r.Context(), entered, release, brokenWaitReader{}}, Request: r}, nil
		})}
		u, _ := url.Parse("http://agent.invalid")
		var out, errs progressTestBuffer
		done := make(chan int, 1)
		go func() { done <- runReadRequest(ctx, c, u, "token", "ps", scope.ID{}, false, false, &out, &errs) }()
		<-entered
		time.Sleep(250 * time.Millisecond)
		synctest.Wait()
		close(release)
		released = true
		synctest.Wait()
		if code := <-done; code != 1 {
			t.Errorf("exit=%d", code)
		}
		want := "\r\x1b[2K| reading ps (0.2s)\r\x1b[2Kagent response could not be read\n"
		if errs.String() != want || out.String() != "" {
			t.Errorf("out=%q err=%q", out.String(), errs.String())
		}
		time.Sleep(time.Second)
		synctest.Wait()
		if errs.String() != want {
			t.Error("late write")
		}
	})
}

func TestWaitCommandClearsBeforeReadFailure(t *testing.T) {
	t.Setenv("TERM", "xterm")
	t.Setenv("NO_COLOR", "1")
	forceStderrTerminal(t, true)
	forceProgressVT(t)
	old := progressWidth
	progressWidth = func(io.Writer) (int, error) { return 80, nil }
	t.Cleanup(func() { progressWidth = old })
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		release := make(chan struct{})
		released := false
		defer func() {
			if !released {
				close(release)
			}
		}()
		entered := make(chan struct{}, 1)
		c := &http.Client{Transport: progressRoundTrip(func(r *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: &waitBody{r.Context(), entered, release, brokenWaitReader{}}, Request: r}, nil
		})}
		u, _ := url.Parse("http://agent.invalid")
		var out, errs progressTestBuffer
		done := make(chan int, 1)
		go func() {
			done <- sendCommand(ctx, c, u, "/v1/", "token", "GET", "wait", nil, commandFlags{}, "wait", &out, &errs)
		}()
		<-entered
		time.Sleep(250 * time.Millisecond)
		synctest.Wait()
		if got := errs.String(); got != "\r\x1b[2K| waiting for app (0.2s)" {
			t.Errorf("frame=%q", got)
		}
		close(release)
		released = true
		synctest.Wait()
		if code := <-done; code != 1 {
			t.Errorf("exit=%d", code)
		}
		want := "\r\x1b[2K| waiting for app (0.2s)\r\x1b[2Kinvalid agent response\n"
		if got := errs.String(); got != want {
			t.Errorf("stderr=%q want %q", got, want)
		}
		if got := out.String(); got != "" {
			t.Errorf("stdout=%q", got)
		}
		before := errs.String()
		time.Sleep(time.Second)
		synctest.Wait()
		if got := errs.String(); got != before {
			t.Errorf("late stderr=%q", got)
		}
		if got := out.String(); got != "" {
			t.Errorf("late stdout=%q", got)
		}
	})
}

func TestReadRequestConnectionFailures(t *testing.T) {
	cases := []struct {
		name, address, reason string
		cause                 error
		refused               bool
	}{
		{"refused", "http://" + dialtest.Refused, "connection refused", nil, true},
		{"not found", "http://agent.invalid:7710", "name not found", &net.DNSError{IsNotFound: true}, false},
		{"dns", "http://agent.invalid", "DNS lookup failed", &net.DNSError{Err: "broken"}, false},
		{"dns timeout", "http://agent.invalid", "connection timed out", &net.DNSError{IsTimeout: true}, false},
		{"timeout", "http://agent.invalid", "connection timed out", context.DeadlineExceeded, false},
		{"network", "http://agent.invalid", "network unreachable", syscall.ENETUNREACH, false},
		{"host", "http://agent.invalid", "network unreachable", syscall.EHOSTUNREACH, false},
		{"fallback", "http://agent.invalid", `failure [redacted]  \x1b`, errors.New("failure fixture-token\n \x1b"), false},
	}
	if runtime.GOOS == "windows" {
		cases = append(cases, struct {
			name, address, reason string
			cause                 error
			refused               bool
		}{"winsock refused", "http://agent.invalid", "connection refused", syscall.Errno(10061), false},
			struct {
				name, address, reason string
				cause                 error
				refused               bool
			}{"winsock network", "http://agent.invalid", "network unreachable", syscall.Errno(10051), false},
			struct {
				name, address, reason string
				cause                 error
				refused               bool
			}{"winsock host", "http://agent.invalid", "network unreachable", syscall.Errno(10065), false})
	}
	for _, tc := range cases {
		for _, jsonMode := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/json=%v", tc.name, jsonMode), func(t *testing.T) {
				u, _ := url.Parse(tc.address + "/ps?grep=private-query")
				client := dialtest.Client()
				if !tc.refused {
					client = &http.Client{Transport: progressRoundTrip(func(*http.Request) (*http.Response, error) { return nil, tc.cause })}
				}
				var out, errs bytes.Buffer
				code := runReadRequest(context.Background(), client, u, "fixture-token", "ps", scope.ID{}, false, jsonMode, &out, &errs)
				port := u.Port()
				if port == "" {
					port = "80"
				}
				want := "agent connection failed at " + net.JoinHostPort(u.Hostname(), port) + ": " + tc.reason
				if code != 1 {
					t.Errorf("exit=%d", code)
				}
				if jsonMode {
					var got struct {
						Code   string `json:"code"`
						Reason string `json:"reason"`
					}
					if err := json.Unmarshal(out.Bytes(), &got); err != nil || got.Code != "agent_connection_failed" || got.Reason != want || errs.Len() != 0 {
						t.Errorf("stdout=%q stderr=%q decoded=%+v err=%v", out.String(), errs.String(), got, err)
					}
				} else if out.Len() != 0 || errs.String() != want+"\n" {
					t.Errorf("stdout=%q stderr=%q want=%q", out.String(), errs.String(), want)
				}
				if strings.Contains(out.String()+errs.String(), "fixture-token") || strings.Contains(out.String()+errs.String(), "private-query") {
					t.Error("credential or query leaked")
				}
			})
		}
	}
}
