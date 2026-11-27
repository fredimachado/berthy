package deploy

import (
	"bytes"
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"berthy/internal/engine"
	"berthy/internal/manifest"
	"berthy/internal/state"
)

// storedAsDocker returns a service spec as Docker keeps it: its API types omit every zero-valued field (null, false, 0,
// "", empty lists and empty objects), so none of them comes back from an inspect. List elements are kept.
func storedAsDocker(s json.RawMessage) json.RawMessage {
	var v any
	dec := json.NewDecoder(bytes.NewReader(s))
	dec.UseNumber()
	if dec.Decode(&v) != nil {
		return s
	}
	out, _ := json.Marshal(dropZeroFields(v))
	return out
}

// dropZeroFields removes the zero-valued fields of every object in v, innermost first.
func dropZeroFields(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			if e = dropZeroFields(e); zeroValue(e) {
				delete(x, k)
			} else {
				x[k] = e
			}
		}
	case []any:
		for i, e := range x {
			x[i] = dropZeroFields(e)
		}
	}
	return v
}

func zeroValue(v any) bool {
	switch y := v.(type) {
	case nil:
		return true
	case bool:
		return !y
	case string:
		return y == ""
	case json.Number:
		f, _ := y.Float64()
		return f == 0
	case []any:
		return len(y) == 0
	case map[string]any:
		return len(y) == 0
	}
	return false
}

// withDockerDefaults adds the non-zero defaults Docker gives a container service when it stores it, on create and on update
// alike: the "container" runtime and the "default" isolation.
func withDockerDefaults(s json.RawMessage) json.RawMessage {
	var v map[string]any
	if json.Unmarshal(s, &v) != nil {
		return s
	}
	if task, ok := v["TaskTemplate"].(map[string]any); ok {
		task["Runtime"] = "container"
		if c, ok := task["ContainerSpec"].(map[string]any); ok {
			c["Isolation"] = "default"
		}
	}
	out, _ := json.Marshal(v)
	return out
}

// dockerSpecs is positiveEngine with service specs stored the way Docker stores them.
type dockerSpecs struct{ *positiveEngine }

func (e dockerSpecs) CreateService(ctx context.Context, spec json.RawMessage, auth *engine.RegistryAuth) (string, error) {
	return e.positiveEngine.CreateService(ctx, withDockerDefaults(storedAsDocker(spec)), auth)
}

func (e dockerSpecs) UpdateService(ctx context.Context, id string, version uint64, spec json.RawMessage, auth *engine.RegistryAuth) error {
	return e.positiveEngine.UpdateService(ctx, id, version, withDockerDefaults(storedAsDocker(spec)), auth)
}

// testWait is how long a test waits for something that must happen; testDeployTimeout is the deploy timeout of a fixture whose
// deploy has to succeed. Both are far beyond what a deploy against the fakes takes, so that a loaded machine does not fail a
// test that is not about a timeout. A test of a timeout sets its own short one.
const (
	testWait          = 30 * time.Second
	testDeployTimeout = "30s"
)

// dockerManifest declares one web role with the given plain environment, which may be empty.
func dockerManifest(plain map[string]string) manifest.Manifest {
	one := 1
	return manifest.Manifest{Project: "proj", Network: "overlay", Apps: map[string]manifest.App{"web": {Kind: "image", Image: "example/web", Env: manifest.Env{Plain: plain}, Deploy: manifest.Deploy{DeployTimeout: testDeployTimeout, DrainTimeout: "1s"}, Roles: map[string]manifest.Role{"web": {Replicas: &one, Health: &manifest.Health{Test: []string{"CMD", "true"}}}}}}}
}

// waitOperation polls until the operation leaves the running state.
func waitOperation(t *testing.T, s *state.Store, id string) state.Operation {
	t.Helper()
	deadline := time.Now().Add(testWait)
	for {
		got, err := s.Operation(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if got.State != "running" {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatal("operation did not finish")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestDockerStoredSpecDeploysAppWithoutEnv(t *testing.T) {
	s := openStore(t)
	c := Controller{rolloutHold: zeroTestRolloutHold(), Engine: dockerSpecs{&positiveEngine{}}, Store: s, Environment: "test"}
	op, _, err := admit(t, &c, context.Background(), Actor{ID: "admin"}, Request{RequestID: "first", App: "web", Version: "sha256:" + strings.Repeat("a", 64), Manifest: dockerManifest(nil)})
	if err != nil {
		t.Fatal(err)
	}
	if got := waitOperation(t, s, op.ID); got.State != "succeeded" {
		t.Fatalf("state %s: %s", got.State, got.Err)
	}
	if _, err = s.Applied(context.Background(), "web"); err != nil {
		t.Fatalf("no release recorded: %v", err)
	}
}

func TestDockerStoredSpecUpdatesToAppWithoutEnv(t *testing.T) {
	s := openStore(t)
	e := dockerSpecs{&positiveEngine{}}
	c := Controller{rolloutHold: zeroTestRolloutHold(), Engine: e, Store: s, Environment: "test"}
	for _, req := range []Request{
		{RequestID: "first", App: "web", Version: "sha256:" + strings.Repeat("a", 64), Manifest: dockerManifest(map[string]string{"MODE": "fast"})},
		{RequestID: "second", App: "web", Version: "sha256:" + strings.Repeat("b", 64), Manifest: dockerManifest(nil)},
	} {
		op, _, err := admit(t, &c, context.Background(), Actor{ID: "admin"}, req)
		if err != nil {
			t.Fatal(err)
		}
		if got := waitOperation(t, s, op.ID); got.State != "succeeded" {
			t.Fatalf("%s: %s %s", req.RequestID, got.State, got.Err)
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	// The app's own variables are gone. The Berthy identity stays, so the container's environment is never empty.
	env := specEnv(t, e.spec)
	if slices.Contains(env, "MODE=fast") || !slices.Contains(env, "BERTHY_IMAGE_DIGEST=sha256:"+strings.Repeat("b", 64)) {
		t.Fatalf("environment after update: %q", env)
	}
}

func TestRecoveryAcceptsJournaledNullEnv(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	prior, _, err := s.Admit(ctx, state.Admission{RequestID: "prior", ActorID: "a", Kind: "deploy", App: "web", PublicRequest: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Finish(ctx, prior.ID, state.Outcome{State: "succeeded", Release: &state.Release{App: "web", Digest: "docker.io/example/web@sha256:" + strings.Repeat("a", 64), Manifest: json.RawMessage(`{}`)}}); err != nil {
		t.Fatal(err)
	}
	op, _, err := s.Admit(ctx, state.Admission{RequestID: "update", ActorID: "a", Kind: "deploy", App: "web", PublicRequest: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	before := withDockerDefaults(json.RawMessage(`{"Name":"svc","Labels":{"berthy.operation":"old"},"TaskTemplate":{"ContainerSpec":{"Image":"docker.io/example/web@sha256:` + strings.Repeat("a", 64) + `","Labels":{"berthy.operation":"old"}}},"Mode":{"Replicated":{"Replicas":1}}}`))
	desired := json.RawMessage(`{"Name":"svc","Labels":{"berthy.operation":"new"},"TaskTemplate":{"ContainerSpec":{"Image":"docker.io/example/web@sha256:` + strings.Repeat("b", 64) + `","Env":null,"Labels":{"berthy.operation":"new"}}},"Mode":{"Replicated":{"Replicas":1}}}`)
	if err = s.RecordIntent(ctx, op.ID, state.Intent{Kind: "update", Name: "svc", ServiceID: "id", Generation: "new", Before: before, Desired: desired}); err != nil {
		t.Fatal(err)
	}
	// Swarm applied the journaled update, and Docker stored it without the null field.
	e := dockerSpecs{&positiveEngine{spec: withDockerDefaults(storedAsDocker(desired))}}
	c := Controller{Engine: e, Store: s}
	if err = c.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := s.Operation(ctx, op.ID)
	if err != nil || got.State != "interrupted" {
		t.Fatalf("operation %+v: %v", got, err)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if !sameSpec(e.spec, before) {
		t.Fatalf("service not restored: %s", e.spec)
	}
}

func TestSpecComparisonIgnoresZeroMapFields(t *testing.T) {
	if !sameSpec(json.RawMessage(`{"a":null,"b":1}`), json.RawMessage(`{"b":1}`)) {
		t.Fatal("null map field changes the comparison")
	}
	if sameSpec(json.RawMessage(`[null]`), json.RawMessage(`[]`)) {
		t.Fatal("null array element is ignored")
	}
	if !matchesStoredSpec(json.RawMessage(`{"Name":"svc","TaskTemplate":{"ContainerSpec":{"Env":null}}}`), json.RawMessage(`{"Name":"svc","TaskTemplate":{"ContainerSpec":{}}}`)) {
		t.Fatal("created spec with a null env is rejected")
	}
}

func TestServiceSpecOmitsEmptyEnv(t *testing.T) {
	b, err := serviceSpec("env", "proj", "app", "web", (&manifest.Manifest{Project: "proj"}).ServiceName("env", "app", "web"), "op", "network", "image", func(v string) string { return (&manifest.Manifest{Project: "proj"}).VolumeName("env", v) }, nil, manifest.Role{Health: &manifest.Health{Test: []string{"CMD", "true"}}}, "start-first", "10s", nil, "")
	if err != nil || strings.Contains(string(b), `"Env"`) {
		t.Fatalf("spec %s: %v", b, err)
	}
}

// TestRoleHealthWithoutStartPeriodDeploys: a role whose health check omits start_period gets the manifest default "0s".
// Docker does not store a zero start period, so the created service must still verify and the deploy must succeed.
func TestRoleHealthWithoutStartPeriodDeploys(t *testing.T) {
	m, err := manifest.DecodeJSON([]byte(`{"project":"proj","network":"overlay","apps":{"web":{"kind":"image","image":"example/web","deploy":{"deploy_timeout":"2s","drain_timeout":"1s"},"roles":{"web":{"health":{"test":["CMD","true"],"interval":"5s","timeout":"2s","retries":3}}}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := m.Apps["web"].Roles["web"].Health.StartPeriod; got != "0s" {
		t.Fatalf("default start period %q", got)
	}
	s := openStore(t)
	c := Controller{rolloutHold: zeroTestRolloutHold(), Engine: dockerSpecs{&positiveEngine{}}, Store: s, Environment: "test"}
	op, _, err := admit(t, &c, context.Background(), Actor{ID: "admin"}, Request{RequestID: "no-start-period", App: "web", Version: "sha256:" + strings.Repeat("a", 64), Manifest: *m})
	if err != nil {
		t.Fatal(err)
	}
	if got := waitOperation(t, s, op.ID); got.State != "succeeded" {
		t.Fatalf("state %s: %s", got.State, got.Err)
	}
}

// TestServiceSpecWritesOnlyNonZeroHealthDurations: a zero start period is left out of the health check, a non-zero one is
// written in nanoseconds, and the other durations are written as configured.
func TestServiceSpecWritesOnlyNonZeroHealthDurations(t *testing.T) {
	retries := 3
	for _, tc := range []struct {
		start   string
		present bool
	}{{"0s", false}, {"1s", true}} {
		role := manifest.Role{Health: &manifest.Health{Test: []string{"CMD", "true"}, Interval: "5s", Timeout: "2s", StartPeriod: tc.start, Retries: &retries}}
		b, err := serviceSpec("env", "proj", "app", "web", (&manifest.Manifest{Project: "proj"}).ServiceName("env", "app", "web"), "op", "network", "image", func(v string) string { return (&manifest.Manifest{Project: "proj"}).VolumeName("env", v) }, nil, role, "start-first", "10s", nil, "")
		if err != nil {
			t.Fatal(err)
		}
		var spec struct {
			TaskTemplate struct {
				ContainerSpec struct {
					Healthcheck map[string]any
				}
			}
		}
		if err := json.Unmarshal(b, &spec); err != nil {
			t.Fatal(err)
		}
		health := spec.TaskTemplate.ContainerSpec.Healthcheck
		got, present := health["StartPeriod"]
		if present != tc.present || present && got != float64(time.Second) {
			t.Fatalf("start period %s: %v (present %v)", tc.start, got, present)
		}
		if health["Interval"] != float64(5*time.Second) || health["Timeout"] != float64(2*time.Second) || health["Retries"] != float64(3) {
			t.Fatalf("health check %v", health)
		}
	}
}

func TestCanonicalSpecDropsTheZeroFieldsDockerOmits(t *testing.T) {
	for name, tc := range map[string]struct{ in, want string }{
		"null":                  {`{"a":null,"b":1}`, `{"b":1}`},
		"false":                 {`{"a":false,"b":true}`, `{"b":true}`},
		"zero numbers":          {`{"a":0,"b":0.0,"c":2}`, `{"c":2}`},
		"empty string":          {`{"a":"","b":"x"}`, `{"b":"x"}`},
		"empty list":            {`{"a":[],"b":[1]}`, `{"b":[1]}`},
		"empty object":          {`{"a":{},"b":{"c":1}}`, `{"b":{"c":1}}`},
		"object that empties":   {`{"a":{"b":null,"c":{"d":false}}}`, `{}`},
		"nested in objects":     {`{"a":{"b":{"c":0,"d":"x"}}}`, `{"a":{"b":{"d":"x"}}}`},
		"list elements stay":    {`{"a":[null,0,false,""]}`, `{"a":[null,0,false,""]}`},
		"object in a list":      {`{"a":[{"b":false,"c":1},{"d":null}]}`, `{"a":[{"c":1},{}]}`},
		"mount as journaled":    {`{"Mounts":[{"Type":"volume","Source":"data","Target":"/data","ReadOnly":false}]}`, `{"Mounts":[{"Source":"data","Target":"/data","Type":"volume"}]}`},
		"large integer is kept": {`{"a":9007199254740993}`, `{"a":9007199254740993}`},
	} {
		var v any
		dec := json.NewDecoder(strings.NewReader(tc.in))
		dec.UseNumber()
		if err := dec.Decode(&v); err != nil {
			t.Fatal(err)
		}
		got, _ := json.Marshal(canonicalSpec(v))
		var wantV any
		dec = json.NewDecoder(strings.NewReader(tc.want))
		dec.UseNumber()
		if err := dec.Decode(&wantV); err != nil {
			t.Fatal(err)
		}
		want, _ := json.Marshal(wantV)
		if string(got) != string(want) {
			t.Errorf("%s: got %s, want %s", name, got, want)
		}
	}
	if sameSpec(json.RawMessage(`{"Replicas":1}`), json.RawMessage(`{"Replicas":2}`)) || sameSpec(json.RawMessage(`{"ReadOnly":true}`), json.RawMessage(`{}`)) {
		t.Fatal("a real difference is ignored")
	}
	if !sameSpec(json.RawMessage(`{"Mounts":[{"ReadOnly":false,"Source":"data"}],"Env":null}`), json.RawMessage(`{"Mounts":[{"Source":"data"}]}`)) {
		t.Fatal("zero fields change the comparison")
	}
}

// volumeManifest declares one web role that mounts a named volume.
func volumeManifest() manifest.Manifest {
	m := dockerManifest(nil)
	role := m.Apps["web"].Roles["web"]
	role.Volumes = []string{"data:/data"}
	m.Apps["web"].Roles["web"] = role
	return m
}

func TestRoleWithANamedVolumeDeploysAndUpdates(t *testing.T) {
	s := openStore(t)
	e := dockerSpecs{&positiveEngine{}}
	c := Controller{rolloutHold: zeroTestRolloutHold(), Engine: e, Store: s, Environment: "test"}
	for _, req := range []Request{
		{RequestID: "first", App: "web", Version: "sha256:" + strings.Repeat("a", 64), Manifest: volumeManifest()},
		{RequestID: "second", App: "web", Version: "sha256:" + strings.Repeat("b", 64), Manifest: volumeManifest()},
	} {
		op, _, err := admit(t, &c, context.Background(), Actor{ID: "admin"}, req)
		if err != nil {
			t.Fatal(err)
		}
		if got := waitOperation(t, s, op.ID); got.State != "succeeded" {
			t.Fatalf("%s: %s %s", req.RequestID, got.State, got.Err)
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if !strings.Contains(string(e.spec), `"Source":"proj-test_data"`) || strings.Contains(string(e.spec), "ReadOnly") {
		t.Fatalf("stored spec: %s", e.spec)
	}
}

func TestRecoveryAcceptsAJournaledVolumeMount(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	prior, _, err := s.Admit(ctx, state.Admission{RequestID: "prior", ActorID: "a", Kind: "deploy", App: "web", PublicRequest: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Finish(ctx, prior.ID, state.Outcome{State: "succeeded", Release: &state.Release{App: "web", Digest: "docker.io/example/web@sha256:" + strings.Repeat("a", 64), Manifest: json.RawMessage(`{}`)}}); err != nil {
		t.Fatal(err)
	}
	op, _, err := s.Admit(ctx, state.Admission{RequestID: "update", ActorID: "a", Kind: "deploy", App: "web", PublicRequest: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	mount := `"Mounts":[{"Type":"volume","Source":"data","Target":"/data","ReadOnly":false}]`
	before := withDockerDefaults(json.RawMessage(`{"Name":"svc","Labels":{"berthy.operation":"old"},"TaskTemplate":{"ContainerSpec":{"Image":"docker.io/example/web@sha256:` + strings.Repeat("a", 64) + `",` + mount + `,"Labels":{"berthy.operation":"old"}}},"Mode":{"Replicated":{"Replicas":1}}}`))
	desired := json.RawMessage(`{"Name":"svc","Labels":{"berthy.operation":"new"},"TaskTemplate":{"ContainerSpec":{"Image":"docker.io/example/web@sha256:` + strings.Repeat("b", 64) + `",` + mount + `,"Labels":{"berthy.operation":"new"}}},"Mode":{"Replicated":{"Replicas":1}}}`)
	if err = s.RecordIntent(ctx, op.ID, state.Intent{Kind: "update", Name: "svc", ServiceID: "id", Generation: "new", Before: before, Desired: desired}); err != nil {
		t.Fatal(err)
	}
	e := dockerSpecs{&positiveEngine{spec: withDockerDefaults(storedAsDocker(desired))}}
	c := Controller{Engine: e, Store: s}
	if err = c.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := s.Operation(ctx, op.ID)
	if err != nil || got.State != "interrupted" {
		t.Fatalf("operation %+v: %v", got, err)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if !sameSpec(e.spec, before) {
		t.Fatalf("service not restored: %s", e.spec)
	}
}

func TestAServiceOfTwoEnvironmentsOfOneProjectHasTwoNames(t *testing.T) {
	role := manifest.Role{Health: &manifest.Health{Test: []string{"CMD", "true"}}}
	for _, env := range []string{"dev", "prod"} {
		b, err := serviceSpec(env, "proj", "api", "web", (&manifest.Manifest{Project: "proj"}).ServiceName(env, "api", "web"), "op", "net", "image", func(v string) string { return (&manifest.Manifest{Project: "proj"}).VolumeName(env, v) }, nil, role, "start-first", "1s", nil, "")
		if err != nil {
			t.Fatal(err)
		}
		var spec struct{ Name string }
		if err := json.Unmarshal(b, &spec); err != nil {
			t.Fatal(err)
		}
		if want := "proj-" + env + "-api-web"; spec.Name != want {
			t.Errorf("got %q, want %q", spec.Name, want)
		}
	}
}

func TestARoleVolumeIsMountedUnderTheEnvironmentsName(t *testing.T) {
	role := manifest.Role{Volumes: []string{"data:/data"}, Health: &manifest.Health{Test: []string{"CMD", "true"}}}
	for _, env := range []string{"dev", "prod"} {
		b, err := serviceSpec(env, "proj", "api", "web", (&manifest.Manifest{Project: "proj"}).ServiceName(env, "api", "web"), "op", "net", "image", func(v string) string { return (&manifest.Manifest{Project: "proj"}).VolumeName(env, v) }, nil, role, "start-first", "1s", nil, "")
		if err != nil {
			t.Fatal(err)
		}
		var spec struct {
			TaskTemplate struct {
				ContainerSpec struct{ Mounts []struct{ Source string } }
			}
		}
		if err := json.Unmarshal(b, &spec); err != nil {
			t.Fatal(err)
		}
		if got, want := spec.TaskTemplate.ContainerSpec.Mounts[0].Source, "proj-"+env+"_data"; got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
}

func TestARoleIsReachableByAppAndRoleOnItsNetwork(t *testing.T) {
	b, err := serviceSpec("prod", "shop", "api", "web", "shop-prod-api-web", "op", "shop-prod", "image", func(v string) string { return v }, nil, manifest.Role{Health: &manifest.Health{Test: []string{"CMD", "true"}}}, "start-first", "10s", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	var s struct {
		Task struct {
			Networks []struct {
				Target  string
				Aliases []string
			}
		} `json:"TaskTemplate"`
	}
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	if len(s.Task.Networks) != 1 || s.Task.Networks[0].Target != "shop-prod" || !slices.Contains(s.Task.Networks[0].Aliases, "api-web") {
		t.Fatalf("networks: %+v", s.Task.Networks)
	}
}
