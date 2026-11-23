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
