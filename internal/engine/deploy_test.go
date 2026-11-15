package engine

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

func TestServiceUpdateUsesVersionAndAuth(t *testing.T) {
	e := fakeEngine(func(r *http.Request) (*http.Response, error) {
		if r.Method != "POST" || r.URL.Path != "/v1.44/services/service1/update" || r.URL.Query().Get("version") != "42" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
		}
		b, err := base64.URLEncoding.DecodeString(r.Header.Get("X-Registry-Auth"))
		if err != nil {
			t.Error(err)
		}
		var auth map[string]string
		if err = json.Unmarshal(b, &auth); err != nil || auth["password"] != "secret" {
			t.Error("bad auth")
		}
		data, _ := io.ReadAll(r.Body)
		if string(data) != `{"Name":"web"}` {
			t.Errorf("bad spec %s", data)
		}
		return response(200, `{}`), nil
	})
	if err := e.UpdateService(context.Background(), "service1", 42, json.RawMessage(`{"Name":"web"}`), &RegistryAuth{Username: "u", Password: "secret"}); err != nil {
		t.Fatal(err)
	}
}
func TestPullReportsDockerProgressError(t *testing.T) {
	e := fakeEngine(func(r *http.Request) (*http.Response, error) {
		return response(200, "{\"status\":\"pulling\"}\n{\"error\":\"sensitive docker error\"}\n"), nil
	})
	err := e.PullImage(context.Background(), "ghcr.io/acme/api:tag", nil)
	if err == nil || strings.Contains(err.Error(), "sensitive") {
		t.Fatalf("unexpected error %v", err)
	}
}
func TestInspectServicePreservesSpecs(t *testing.T) {
	e := fakeEngine(func(r *http.Request) (*http.Response, error) {
		return response(200, `{"ID":"id1","Version":{"Index":9},"Spec":{"Name":"web","TaskTemplate":{"Networks":[{"Target":"net"}]}},"PreviousSpec":{"Name":"old"},"UpdateStatus":{"State":"updating"}}`), nil
	})
	s, err := e.InspectService(context.Background(), "web")
	if err != nil || s.ID != "id1" || s.Version != 9 || !strings.Contains(string(s.Spec), `"Networks"`) || !strings.Contains(string(s.PreviousSpec), `"old"`) {
		t.Fatalf("%+v %v", s, err)
	}
}

func TestTasksFilterServiceAndRetainVersion(t *testing.T) {
	e := fakeEngine(func(r *http.Request) (*http.Response, error) {
		if !strings.Contains(r.URL.Query().Get("filters"), "service1") {
			t.Error("missing service filter")
		}
		return response(200, `[{"ID":"task1","ServiceID":"service1","NodeID":"n1","Slot":2,"Version":{"Index":8},"SpecVersion":{"Index":7},"Status":{"State":"running","ContainerStatus":{"ContainerID":"container1"}}},{"ID":"foreign","ServiceID":"service2"}]`), nil
	})
	tasks, err := e.ServiceTasks(context.Background(), "service1")
	if err != nil || len(tasks) != 1 || tasks[0].SpecVersion != 7 || tasks[0].ContainerID != "container1" {
		t.Fatalf("%+v %v", tasks, err)
	}
}

func TestNativeRegistryRead(t *testing.T) {
	socket := os.Getenv("BERTHY_TEST_DOCKER_SOCKET")
	if socket == "" {
		t.Skip("isolated Docker socket not selected")
	}
	password, err := os.ReadFile("/var/tmp/berthy-phase2/auth/cli-password")
	if err != nil {
		t.Fatal("cannot read fixture credential")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	e := New(socket)
	auth := &RegistryAuth{Username: "deploy-test", Password: strings.TrimSpace(string(password)), ServerAddress: "127.0.0.1:15001"}
	digest, err := e.ResolveImage(ctx, "127.0.0.1:15001/berthy-phase2/alpine:v1", auth)
	if err != nil || !strings.HasPrefix(digest, "sha256:") {
		t.Fatalf("registry resolution failed: %v", err)
	}
	image, err := e.InspectImage(ctx, "127.0.0.1:15001/berthy-phase2/alpine:v1")
	if err != nil || image.ID == "" {
		t.Fatalf("image inspect failed: %v", err)
	}
}

func TestStopContainerUsesBoundedTimeout(t *testing.T) {
	e := fakeEngine(func(r *http.Request) (*http.Response, error) {
		if r.Method != "POST" || r.URL.Path != "/v1.44/containers/c1/stop" || r.URL.Query().Get("t") != "5" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
		}
		return response(204, ``), nil
	})
	if err := e.StopContainer(context.Background(), "c1", 5); err != nil {
		t.Fatal(err)
	}
}

func TestServiceTasksIncludesTaskGenerationLabel(t *testing.T) {
	e := fakeEngine(func(*http.Request) (*http.Response, error) {
		return response(200, `[{"ID":"task","ServiceID":"service","Spec":{"ContainerSpec":{"Labels":{"berthy.operation":"op1"}}}}]`), nil
	})
	tasks, err := e.ServiceTasks(context.Background(), "service")
	if err != nil || len(tasks) != 1 || tasks[0].Labels["berthy.operation"] != "op1" {
		t.Fatalf("task generation not returned: %+v, %v", tasks, err)
	}
}

func TestServiceTaskIncludesImage(t *testing.T) {
	e := fakeEngine(func(*http.Request) (*http.Response, error) {
		return response(200, `[{"ID":"task","ServiceID":"service","Spec":{"ContainerSpec":{"Image":"repo@sha256:abc"}}}]`), nil
	})
	tasks, err := e.ServiceTasks(context.Background(), "service")
	if err != nil || len(tasks) != 1 || tasks[0].Image != "repo@sha256:abc" {
		t.Fatalf("task image: %+v %v", tasks, err)
	}
}
