package engine

import (
	"context"
	"net/http"
	"testing"
)

func TestExitCodeFromStatusReadsOnlyAnExitedContainer(t *testing.T) {
	for status, want := range map[string]struct {
		code int
		ok   bool
	}{
		"Exited (0) 3 minutes ago":      {0, true},
		"Exited (137) 2 minutes ago":    {137, true},
		"Exited (1) About a minute ago": {1, true},
		"Up 2 hours (healthy)":          {0, false},
		"Created":                       {0, false},
		"Restarting (1) 5 seconds ago":  {0, false},
		"Exited (x) ago":                {0, false},
		"":                              {0, false},
	} {
		code, ok := ExitCodeFromStatus(status)
		if code != want.code || ok != want.ok {
			t.Errorf("%q: got %d %v, want %d %v", status, code, ok, want.code, want.ok)
		}
	}
}

func TestAppsGivesAStoppedComposeContainerItsExitCode(t *testing.T) {
	e := fakeEngine(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/v1.44/containers/json":
			return response(200, `[{"Id":"c1","Image":"img","State":"exited","Status":"Exited (137) 2 minutes ago","Labels":{"com.docker.compose.project":"shop-db","com.docker.compose.service":"db"}},{"Id":"c2","Image":"img","State":"exited","Status":"Exited (0) 9 minutes ago","Labels":{"com.docker.compose.project":"shop-db","com.docker.compose.service":"job"}},{"Id":"c3","Image":"img","State":"running","Status":"Up 3 minutes","Labels":{"com.docker.compose.project":"shop-db","com.docker.compose.service":"web"}}]`), nil
		case "/v1.44/services":
			return response(200, `[]`), nil
		}
		return response(404, `{}`), nil
	})
	apps, err := e.Apps(context.Background())
	if err != nil || len(apps) != 1 {
		t.Fatalf("apps %#v err %v", apps, err)
	}
	codes := map[string]*int{}
	for _, role := range apps[0].Roles {
		for _, rep := range role.Replicas {
			codes[role.Name] = rep.ExitCode
		}
	}
	if c := codes["db"]; c == nil || *c != 137 {
		t.Fatalf("db exit code %v, want 137", c)
	}
	if c := codes["job"]; c == nil || *c != 0 {
		t.Fatalf("job exit code %v, want 0", c)
	}
	if c, ok := codes["web"]; !ok || c != nil {
		t.Fatalf("running container has an exit code: %v", c)
	}
}
