package engine

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestImportImageSendsTheArchiveWithItsInstructions(t *testing.T) {
	var got *http.Request
	var body []byte
	e := fakeEngine(func(r *http.Request) (*http.Response, error) {
		got = r
		body, _ = io.ReadAll(r.Body)
		return response(200, "{\"status\":\"sha256:abc\"}\n"), nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	changes := []string{`ENTRYPOINT ["/berthy","probe-helper"]`, "USER 65532:65532"}
	if err := e.ImportImage(ctx, "berthy-helper", "0123456789ab", changes, []byte("tar-bytes")); err != nil {
		t.Fatal(err)
	}
	q := got.URL.Query()
	if got.Method != "POST" || got.URL.Path != "/v1.44/images/create" || q.Get("fromSrc") != "-" || q.Get("repo") != "berthy-helper" || q.Get("tag") != "0123456789ab" {
		t.Fatalf("request %s %s", got.Method, got.URL)
	}
	if len(q["changes"]) != 2 || q["changes"][0] != changes[0] || q["changes"][1] != changes[1] {
		t.Fatalf("changes %v", q["changes"])
	}
	if string(body) != "tar-bytes" {
		t.Fatalf("body %q", body)
	}
}

func TestImportImageReportsTheFailureWithoutDockersText(t *testing.T) {
	e := fakeEngine(func(r *http.Request) (*http.Response, error) {
		return response(200, "{\"status\":\"importing\"}\n{\"error\":\"sensitive docker error\"}\n"), nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	err := e.ImportImage(ctx, "berthy-helper", "0123456789ab", nil, []byte("x"))
	if err == nil || strings.Contains(err.Error(), "sensitive") {
		t.Fatalf("import error %v", err)
	}
	if err := e.ImportImage(ctx, "berthy-helper", "bad?tag", nil, []byte("x")); err == nil {
		t.Fatal("invalid tag accepted")
	}
}

func TestRemoveUnusedImageNeverForces(t *testing.T) {
	e := fakeEngine(func(r *http.Request) (*http.Response, error) {
		if r.Method != "DELETE" || r.URL.Path != "/v1.44/images/berthy-helper:0123456789ab" || r.URL.Query().Get("force") != "" {
			t.Errorf("request %s %s", r.Method, r.URL)
		}
		return response(409, `{"message":"in use"}`), nil
	})
	err := e.RemoveUnusedImage(context.Background(), "berthy-helper:0123456789ab")
	var status *StatusError
	if !errors.As(err, &status) || status.Code != 409 {
		t.Fatalf("in-use image: %v", err)
	}
}

func TestInspectContainerDetailReadsWhatTheHelperIsCheckedAgainst(t *testing.T) {
	e := fakeEngine(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/v1.44/containers/berthy-helper-lab/json" {
			t.Errorf("path %s", r.URL.Path)
		}
		return response(200, `{"Id":"c1","Image":"sha256:img","Config":{"User":"65532:65532","Labels":{"berthy.helper":"probe"}},`+
			`"State":{"Running":true},"HostConfig":{"ReadonlyRootfs":true,"CapDrop":["ALL"],"SecurityOpt":["no-new-privileges"],`+
			`"Memory":67108864,"RestartPolicy":{"Name":"unless-stopped"},"PortBindings":{"7079/tcp":[{"HostIp":"127.0.0.1","HostPort":""}]}},`+
			`"NetworkSettings":{"Ports":{"7079/tcp":[{"HostIp":"127.0.0.1","HostPort":"41234"}]},"Networks":{"lab-net":{"NetworkID":"net1"}}}}`), nil
	})
	c, err := e.InspectContainerDetail(context.Background(), "berthy-helper-lab")
	if err != nil {
		t.Fatal(err)
	}
	if c.ID != "c1" || c.ImageID != "sha256:img" || c.User != "65532:65532" || c.Labels["berthy.helper"] != "probe" || !c.Running || !c.ReadonlyRootfs {
		t.Fatalf("identity and state: %+v", c)
	}
	if len(c.CapDrop) != 1 || c.CapDrop[0] != "ALL" || len(c.SecurityOpt) != 1 || c.Memory != 67108864 || c.RestartPolicy != "unless-stopped" {
		t.Fatalf("host config: %+v", c)
	}
	if b := c.PortBindings["7079/tcp"]; len(b) != 1 || b[0].HostIP != "127.0.0.1" || b[0].HostPort != "" {
		t.Fatalf("requested ports: %+v", c.PortBindings)
	}
	if b := c.Ports["7079/tcp"]; len(b) != 1 || b[0].HostIP != "127.0.0.1" || b[0].HostPort != "41234" {
		t.Fatalf("published ports: %+v", c.Ports)
	}
	if c.Networks["lab-net"] != "net1" || len(c.Networks) != 1 {
		t.Fatalf("networks: %+v", c.Networks)
	}
}
