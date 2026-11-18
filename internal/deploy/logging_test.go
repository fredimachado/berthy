package deploy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"berthy/internal/manifest"
)

// loggingDockerSpecs is dockerSpecs that also reports the daemon's default log driver.
type loggingDockerSpecs struct {
	dockerSpecs
	driver string
	err    error
}

func (e loggingDockerSpecs) LoggingDriver(context.Context) (string, error) { return e.driver, e.err }

// loggingPositive is positiveEngine that also reports the daemon's default log driver.
type loggingPositive struct {
	*positiveEngine
	driver string
	err    error
}

func (e loggingPositive) LoggingDriver(context.Context) (string, error) { return e.driver, e.err }

// defaultRotation is the rotation Berthy gives a role that sets no logging, as a LogDriver decoded from JSON.
func defaultRotation() map[string]any {
	return map[string]any{"Name": "json-file", "Options": map[string]any{"max-size": "10m", "max-file": "3"}}
}

// customRotation is the rotation of a role whose logging sets max-size 50m and max-file 5.
func customRotation() map[string]any {
	return map[string]any{"Name": "json-file", "Options": map[string]any{"max-size": "50m", "max-file": "5"}}
}

// logDriverOf returns the TaskTemplate.LogDriver of a service spec, or nil when the spec has none.
func logDriverOf(t *testing.T, spec json.RawMessage) any {
	t.Helper()
	var s struct{ TaskTemplate struct{ LogDriver any } }
	if err := json.Unmarshal(spec, &s); err != nil {
		t.Fatalf("spec %s: %v", spec, err)
	}
	return s.TaskTemplate.LogDriver
}

func TestRoleLogDriver(t *testing.T) {
	custom := manifest.Role{Logging: &manifest.Logging{Driver: "json-file", Options: map[string]string{"max-size": "50m", "max-file": "5"}}}
	bare := manifest.Role{Logging: &manifest.Logging{Driver: "json-file"}}
	cases := []struct {
		name   string
		role   manifest.Role
		daemon string
		want   map[string]any
	}{
		{"default rotation on json-file", manifest.Role{}, "json-file", defaultRotation()},
		{"default rotation on local", manifest.Role{}, "local", map[string]any{"Name": "local", "Options": map[string]any{"max-size": "10m", "max-file": "3"}}},
		{"no rotation on journald", manifest.Role{}, "journald", nil},
		{"no rotation when the daemon names no driver", manifest.Role{}, "", nil},
		{"role logging replaces the default", custom, "json-file", customRotation()},
		{"role logging is used even on journald", custom, "journald", customRotation()},
		{"role logging without options has no Options", bare, "journald", map[string]any{"Name": "json-file"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := roleLogDriver(tc.role, tc.daemon); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %#v, want %#v", got, tc.want)
			}
		})
	}
}

// TestServiceSpecLogDriverIsByteStable: the same inputs give the same JSON every time, whatever order the option map is
// read in.
func TestServiceSpecLogDriverIsByteStable(t *testing.T) {
	role := manifest.Role{Health: &manifest.Health{Test: []string{"CMD", "true"}}, Logging: &manifest.Logging{Driver: "json-file", Options: map[string]string{"max-size": "50m", "max-file": "5", "a": "1", "b": "2", "c": "3", "z": "9"}}}
	first, err := serviceSpec("env", "proj", "app", "web", "op", "network", "image", nil, role, "10s", nil, "journald")
	if err != nil {
		t.Fatal(err)
	}
	if got := logDriverOf(t, first); !reflect.DeepEqual(got, map[string]any{"Name": "json-file", "Options": map[string]any{"max-size": "50m", "max-file": "5", "a": "1", "b": "2", "c": "3", "z": "9"}}) {
		t.Fatalf("log driver %#v in %s", got, first)
	}
	for i := 0; i < 50; i++ {
		again, err := serviceSpec("env", "proj", "app", "web", "op", "network", "image", nil, role, "10s", nil, "journald")
		if err != nil || !bytes.Equal(first, again) {
			t.Fatalf("spec changed between calls: %s and %s (%v)", first, again, err)
		}
	}
}

func TestDeployGivesNewServicesDefaultLogRotation(t *testing.T) {
	s := openStore(t)
	pe := &positiveEngine{}
	c := Controller{Engine: loggingDockerSpecs{dockerSpecs{pe}, "json-file", nil}, Store: s, Environment: "test"}
	op, _, err := admit(t, &c, context.Background(), Actor{ID: "admin"}, Request{RequestID: "first", App: "web", Version: "sha256:" + strings.Repeat("a", 64), Manifest: dockerManifest(nil)})
	if err != nil {
		t.Fatal(err)
	}
	if got := waitOperation(t, s, op.ID); got.State != "succeeded" {
		t.Fatalf("state %s: %s", got.State, got.Err)
	}
	pe.mu.Lock()
	defer pe.mu.Unlock()
	if got := logDriverOf(t, pe.spec); !reflect.DeepEqual(got, defaultRotation()) {
		t.Fatalf("log driver %#v in %s", got, pe.spec)
	}
}

func TestDeployReplacesLogRotationWithTheRolesLogging(t *testing.T) {
	s := openStore(t)
	pe := &positiveEngine{}
	c := Controller{Engine: loggingDockerSpecs{dockerSpecs{pe}, "json-file", nil}, Store: s, Environment: "test"}
	withLogging := dockerManifest(nil)
	r := withLogging.Apps["web"].Roles["web"]
	r.Logging = &manifest.Logging{Driver: "json-file", Options: map[string]string{"max-size": "50m", "max-file": "5"}}
	withLogging.Apps["web"].Roles["web"] = r
	for _, req := range []Request{
		{RequestID: "first", App: "web", Version: "sha256:" + strings.Repeat("a", 64), Manifest: dockerManifest(nil)},
		{RequestID: "second", App: "web", Version: "sha256:" + strings.Repeat("b", 64), Manifest: withLogging},
	} {
		op, _, err := admit(t, &c, context.Background(), Actor{ID: "admin"}, req)
		if err != nil {
			t.Fatal(err)
		}
		if got := waitOperation(t, s, op.ID); got.State != "succeeded" {
			t.Fatalf("%s: %s %s", req.RequestID, got.State, got.Err)
		}
	}
	pe.mu.Lock()
	defer pe.mu.Unlock()
	if got := logDriverOf(t, pe.spec); !reflect.DeepEqual(got, customRotation()) {
		t.Fatalf("log driver %#v in %s", got, pe.spec)
	}
}

// TestDeployFailsWithoutTheDaemonLogDriver: a deploy that cannot read the daemon's driver must not drop the rotation
// silently, so it fails before it creates anything.
func TestDeployFailsWithoutTheDaemonLogDriver(t *testing.T) {
	s := openStore(t)
	pe := &positiveEngine{}
	c := Controller{Engine: loggingDockerSpecs{dockerSpecs{pe}, "", errors.New("daemon down")}, Store: s, Environment: "test"}
	op, _, err := admit(t, &c, context.Background(), Actor{ID: "admin"}, Request{RequestID: "no-driver", App: "web", Version: "sha256:" + strings.Repeat("a", 64), Manifest: dockerManifest(nil)})
	if err != nil {
		t.Fatal(err)
	}
	if got := waitOperation(t, s, op.ID); got.State == "succeeded" {
		t.Fatal("deploy succeeded without the daemon's log driver")
	}
	pe.mu.Lock()
	defer pe.mu.Unlock()
	if pe.spec != nil {
		t.Fatalf("service created: %s", pe.spec)
	}
}

func TestMergeServiceSpecSetsAndRemovesTheLogDriver(t *testing.T) {
	without := json.RawMessage(`{"Name":"svc","TaskTemplate":{"ContainerSpec":{"Image":"new"},"Networks":[{"Target":"net"}]},"Mode":{"Replicated":{"Replicas":1}}}`)
	with := json.RawMessage(`{"Name":"svc","TaskTemplate":{"ContainerSpec":{"Image":"new"},"Networks":[{"Target":"net"}],"LogDriver":{"Name":"json-file","Options":{"max-size":"50m","max-file":"5"}}},"Mode":{"Replicated":{"Replicas":1}}}`)
	merged, err := mergeServiceSpec(without, with, "net")
	if err != nil || !reflect.DeepEqual(logDriverOf(t, merged), customRotation()) {
		t.Fatalf("merged %s: %v", merged, err)
	}
	merged, err = mergeServiceSpec(with, without, "net")
	if err != nil || logDriverOf(t, merged) != nil {
		t.Fatalf("log driver kept in %s: %v", merged, err)
	}
}

// adoptedService is the service of role as Berthy creates it on a daemon whose default driver is daemonDriver, under the
// name the role adopts. A driver that gets no rotation leaves the service with no log driver, as one Berthy did not make.
func adoptedService(t *testing.T, role manifest.Role, daemonDriver string) json.RawMessage {
	t.Helper()
	digest := "docker.io/example/web@sha256:" + strings.Repeat("a", 64)
	spec, err := serviceSpec("test", "proj", "web", "web", "prior", "net", digest, nil, role, "1s", nil, daemonDriver)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err = json.Unmarshal(spec, &body); err != nil {
		t.Fatal(err)
	}
	body["Name"] = role.Service
	spec, err = json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return spec
}

func adoptionRequest(role manifest.Role) Request {
	return Request{Adopt: true, App: "web", Manifest: manifest.Manifest{Project: "proj", Network: "overlay", Apps: map[string]manifest.App{"web": {Kind: "image", Image: "example/web", Roles: map[string]manifest.Role{"web": role}}}}}
}

func TestAdoptionPlanNamesTheLogRotationItWillChange(t *testing.T) {
	one := 1
	health := &manifest.Health{Test: []string{"CMD", "true"}}
	adopted := manifest.Role{Service: "existing", Replicas: &one, Health: health}
	withLogging := manifest.Role{Service: "existing", Replicas: &one, Health: health, Logging: &manifest.Logging{Driver: "json-file", Options: map[string]string{"max-size": "50m", "max-file": "5"}}}
	for _, tc := range []struct {
		name   string
		live   manifest.Role
		daemon string
		want   []string
	}{
		{
			name:   "a service with no log driver",
			live:   adopted,
			daemon: "journald",
			want:   []string{"role web: logging json-file max-file=3 max-size=10m (now Docker's default), applied by the next deploy"},
		},
		{
			name:   "a service that already has the default",
			live:   adopted,
			daemon: "json-file",
			want:   nil,
		},
		{
			name:   "a service whose logging the manifest no longer sets",
			live:   withLogging,
			daemon: "json-file",
			want:   []string{"role web: logging json-file max-file=3 max-size=10m (now json-file max-file=5 max-size=50m), applied by the next deploy"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := Controller{Engine: loggingPositive{&positiveEngine{spec: adoptedService(t, tc.live, tc.daemon)}, "json-file", nil}, Environment: "test"}
			p, _, err := c.planAdoption(context.Background(), adoptionRequest(adopted))
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(p.Changes, tc.want) {
				t.Fatalf("changes %q, want %q", p.Changes, tc.want)
			}
		})
	}
}

func TestAdoptionPlanFailsWithoutTheDaemonLogDriver(t *testing.T) {
	one := 1
	role := manifest.Role{Service: "existing", Replicas: &one, Health: &manifest.Health{Test: []string{"CMD", "true"}}}
	c := Controller{Engine: loggingPositive{&positiveEngine{spec: adoptedService(t, role, "json-file")}, "", errors.New("daemon down")}, Environment: "test"}
	if _, _, err := c.planAdoption(context.Background(), adoptionRequest(role)); err == nil {
		t.Fatal("adoption planned without the daemon's log driver")
	}
}
