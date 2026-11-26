package agentclient

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

type f5Transport func(*http.Request) (*http.Response, error)

func (f f5Transport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type f5Body struct {
	io.Reader
	closed *int
}

func (b f5Body) Close() error { *b.closed++; return nil }

func TestResultKeepsProbeWithItsResponse(t *testing.T) {
	for _, confirmed := range []bool{false, true} {
		t.Run(fmt.Sprint(confirmed), func(t *testing.T) {
			originalClosed, probeClosed, calls := 0, 0, 0
			client := &http.Client{Transport: f5Transport(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.Header.Get("Authorization") != "Bearer fixture" {
					t.Errorf("authorization changed")
				}
				if calls == 1 {
					if r.Method != "GET" || r.URL.Path != "/v1/projects/p/environments/dev/backups" || r.URL.RawQuery != "backup=x" {
						t.Errorf("request %s %s", r.Method, r.URL.String())
					}
					return &http.Response{StatusCode: 404, Body: f5Body{strings.NewReader(`{"message":"` + OlderAgentMessage + `"}`), &originalClosed}, Header: make(http.Header)}, nil
				}
				if r.Method != "GET" || r.URL.Path != "/v1/environments/dev/agent" || r.URL.RawQuery != "" {
					t.Errorf("probe %s %s", r.Method, r.URL.String())
				}
				status := 404
				if confirmed {
					status = 200
				}
				return &http.Response{StatusCode: status, Body: f5Body{strings.NewReader(`{}`), &probeClosed}, Header: make(http.Header)}, nil
			})}
			base, _ := url.Parse("http://fixture.invalid")
			result, err := DoResult(context.Background(), client, base, "/v1/projects/p/environments/dev/", "fixture", "GET", "backups", "backup=x", nil)
			if err != nil || result.Status != 404 || result.Older != confirmed || Message(result.Status, result.Data) != "agent returned HTTP 404: "+OlderAgentMessage || calls != 2 || originalClosed != 1 || probeClosed != 1 {
				t.Fatalf("result %+v err %v calls %d closes %d %d", result, err, calls, originalClosed, probeClosed)
			}
		})
	}
}
