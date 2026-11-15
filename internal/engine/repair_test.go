package engine

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func fakeEngine(f transportFunc) *Engine                                  { return &Engine{client: &http.Client{Transport: f}} }
func response(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Status: fmt.Sprint(status), Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}
}
func TestInventoryRolloutAndInspectFailure(t *testing.T) {
	fail := false
	e := fakeEngine(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/v1.44/containers/json":
			return response(200, `[{"Id":"local-old","Labels":{"com.docker.swarm.task.id":"old"}}]`), nil
		case "/v1.44/services":
			return response(200, `[{"ID":"s","Spec":{"Name":"web","Mode":{"Replicated":{"Replicas":1}}}},{"ID":"z","Spec":{"Name":"zero","Mode":{"Replicated":{"Replicas":0}}}}]`), nil
		case "/v1.44/tasks":
			return response(200, `[{"ID":"old","ServiceID":"s","DesiredState":"shutdown","Spec":{"ContainerSpec":{"Image":"old"}},"Status":{"State":"running"}},{"ID":"new","ServiceID":"s","DesiredState":"ready","Spec":{"ContainerSpec":{"Image":"new"}},"Status":{"State":"ready"}},{"ID":"history","ServiceID":"s","DesiredState":"shutdown","Status":{"State":"complete"}}]`), nil
		case "/v1.44/containers/local-old/json":
			if fail {
				return nil, context.DeadlineExceeded
			}
			return response(200, `{"State":{}}`), nil
		}
		return response(404, `{}`), nil
	})
	apps, err := e.Apps(context.Background())
	if err != nil || len(apps) != 2 || len(apps[0].Roles[0].Replicas) != 2 || apps[0].Roles[0].Replicas[0].ContainerID != "local-old" || len(apps[1].Roles[0].Replicas) != 0 {
		t.Fatalf("apps %#v err %v", apps, err)
	}
	fail = true
	if _, err = e.Apps(context.Background()); err == nil {
		t.Fatal("inspect deadline ignored")
	}
}
