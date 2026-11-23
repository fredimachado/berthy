package deploy

import (
	"context"
	"errors"
	"testing"

	"berthy/internal/engine"
	"berthy/internal/manifest"
	"berthy/internal/state"
)

func TestGuardSeesAFixedComposeVolume(t *testing.T) {
	for _, tc := range []struct{ file, volume string }{
		{"volumes: {pgdata: {external: true, name: shared-pg}}", "shared-pg"},
		{"volumes: {data: {name: pg-main}}", "pg-main"},
		{"volumes: {ext: {external: true}}", "ext"},
	} {
		t.Run(tc.volume, func(t *testing.T) {
			docker := newComposeDocker()
			docker.volumes = map[string]bool{tc.volume: true}
			c := &Controller{Store: openStore(t), Engine: docker, Environment: "test"}
			m := composeApp(false)
			a := m.Apps["db"]
			a.Env.Secret = []manifest.SecretVar{{Var: "PASS", Secret: "PASS"}}
			m.Apps["db"] = a
			finding, err := c.GuardSecret(context.Background(), "PASS", &Submitted{Manifest: m, ComposeFiles: map[string]string{"db": tc.file}})
			if err != nil || finding == nil || finding.App != "db" || finding.Volume != tc.volume {
				t.Fatalf("finding: %+v, %v", finding, err)
			}
		})
	}
}

func TestGuardRefusesWhatItCannotEstablish(t *testing.T) {
	docker := newComposeDocker()
	docker.volumes = nil
	c := &Controller{Store: openStore(t), Engine: docker, Environment: "test"}
	m := composeApp(false)
	a := m.Apps["db"]
	a.Env.Secret = []manifest.SecretVar{{Var: "PASS", Secret: "PASS"}}
	m.Apps["db"] = a
	for _, file := range []string{"", "volumes: [", "include: [other.yml]", "volumes: {data: {name: '$STACK'}}"} {
		_, err := c.GuardSecret(context.Background(), "PASS", &Submitted{Manifest: m, ComposeFiles: map[string]string{"db": file}})
		var refusal Refusal
		if !errors.As(err, &refusal) {
			t.Errorf("file %q: %v", file, err)
		}
	}
}

func TestGuardRefusesAForeignComposeProject(t *testing.T) {
	c := &Controller{Store: openStore(t), Environment: "test"}
	m := composeApp(false)
	a := m.Apps["db"]
	a.Env.Secret = []manifest.SecretVar{{Var: "PASS", Secret: "PASS"}}
	m.Apps["db"] = a
	project := m.ComposeProject("test", "db")
	c.Foreign = func(context.Context) (Foreign, error) {
		return Foreign{Claims: map[state.Claim]bool{{Kind: state.ClaimComposeProject, Name: project}: true}}, nil
	}
	_, err := c.GuardSecret(context.Background(), "PASS", &Submitted{Manifest: m})
	if err == nil || err.Error() != "Compose project name is taken by another project or environment on this server" {
		t.Fatalf("foreign project: %v", err)
	}
}

func TestGuardAllowsAReaderWithNoExistingFixedVolume(t *testing.T) {
	docker := newComposeDocker()
	docker.volumes = map[string]bool{}
	c := &Controller{Store: openStore(t), Engine: docker, Environment: "test"}
	m := composeApp(false)
	a := m.Apps["db"]
	a.Env.Secret = []manifest.SecretVar{{Var: "PASS", Secret: "PASS"}}
	m.Apps["db"] = a
	finding, err := c.GuardSecret(context.Background(), "PASS", &Submitted{Manifest: m, ComposeFiles: map[string]string{"db": "volumes: {data: {name: absent}}"}})
	if err != nil || finding != nil {
		t.Fatalf("finding: %+v, %v", finding, err)
	}
}

func TestGuardSeesContainersServicesAndLabelledVolumes(t *testing.T) {
	for _, tc := range []struct {
		name   string
		docker *composeDocker
		kind   string
		volume string
	}{
		{"stopped container", &composeDocker{positiveEngine: &positiveEngine{}, containers: []engine.ComposeContainer{{State: "exited"}}}, "compose", ""},
		{"labelled volume", &composeDocker{positiveEngine: &positiveEngine{}, projectVolumes: []string{"project-data"}}, "compose", "project-data"},
		{"image service", &composeDocker{positiveEngine: &positiveEngine{spec: []byte(`{}`)}}, "image", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &Controller{Store: openStore(t), Engine: tc.docker, Environment: "test"}
			m := composeApp(false)
			a := m.Apps["db"]
			a.Kind = tc.kind
			a.Env.Secret = []manifest.SecretVar{{Var: "PASS", Secret: "PASS"}}
			if tc.kind == "image" {
				a.Roles = map[string]manifest.Role{"web": {Service: "existing"}}
			}
			m.Apps["db"] = a
			finding, err := c.GuardSecret(context.Background(), "PASS", &Submitted{Manifest: m})
			if err != nil || finding == nil || finding.App != "db" || finding.Volume != tc.volume {
				t.Fatalf("finding: %+v %v", finding, err)
			}
		})
	}
}

type brokenGuardEngine struct {
	*composeDocker
	fail string
}

func (e *brokenGuardEngine) ComposeContainers(ctx context.Context, p string) ([]engine.ComposeContainer, error) {
	if e.fail == "containers" {
		return nil, errors.New("offline")
	}
	return e.composeDocker.ComposeContainers(ctx, p)
}
func (e *brokenGuardEngine) ProjectVolumes(ctx context.Context, p string) ([]string, error) {
	if e.fail == "projects" {
		return nil, errors.New("offline")
	}
	return e.composeDocker.ProjectVolumes(ctx, p)
}
func (e *brokenGuardEngine) VolumeExists(ctx context.Context, n string) (bool, error) {
	if e.fail == "volumes" {
		return false, errors.New("offline")
	}
	return e.composeDocker.VolumeExists(ctx, n)
}
func (e *brokenGuardEngine) InspectService(ctx context.Context, n string) (engine.ServiceSnapshot, error) {
	if e.fail == "service" {
		return engine.ServiceSnapshot{}, &engine.StatusError{Code: 500}
	}
	return e.composeDocker.InspectService(ctx, n)
}

func TestGuardRefusesFailedInventories(t *testing.T) {
	for _, fail := range []string{"containers", "projects", "volumes", "service", "foreign"} {
		t.Run(fail, func(t *testing.T) {
			docker := &brokenGuardEngine{composeDocker: newComposeDocker(), fail: fail}
			c := &Controller{Store: openStore(t), Engine: docker, Environment: "test"}
			m := composeApp(false)
			a := m.Apps["db"]
			a.Env.Secret = []manifest.SecretVar{{Var: "PASS", Secret: "PASS"}}
			if fail == "service" {
				a.Kind = "image"
				a.Roles = map[string]manifest.Role{"web": {}}
			}
			m.Apps["db"] = a
			if fail == "foreign" {
				c.Foreign = func(context.Context) (Foreign, error) { return Foreign{}, errors.New("offline") }
			}
			_, err := c.GuardSecret(context.Background(), "PASS", &Submitted{Manifest: m, ComposeFiles: map[string]string{"db": "volumes: {data: {name: shared}}"}})
			var refusal Refusal
			if !errors.As(err, &refusal) || err.Error() != "the state of app db cannot be established, so nothing is generated" {
				t.Fatalf("refusal: %v", err)
			}
		})
	}
}

func TestGuardSeesImageReaderVolumeWithoutDeployment(t *testing.T) {
	docker := newComposeDocker()
	docker.volumes = map[string]bool{"shop-test_data": true}
	c := &Controller{Store: openStore(t), Engine: docker, Environment: "test"}
	m := manifest.Manifest{Project: "shop", Apps: map[string]manifest.App{"db": {Kind: "image", Env: manifest.Env{Secret: []manifest.SecretVar{{Var: "PASS", Secret: "PASS"}}}, Roles: map[string]manifest.Role{"web": {Volumes: []string{"data:/data"}}}}}}
	finding, err := c.GuardSecret(context.Background(), "PASS", &Submitted{Manifest: m})
	if err != nil || finding == nil || finding.App != "db" || finding.Volume != "shop-test_data" {
		t.Fatalf("finding: %+v, %v", finding, err)
	}
}
