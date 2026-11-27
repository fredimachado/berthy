package deploy

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"berthy/internal/manifest"
)

// changedByHand is the service of role as adoptedService makes it, edited as a person would with docker service update.
func changedByHand(t *testing.T, role manifest.Role, edit func(spec, container, task map[string]any)) json.RawMessage {
	t.Helper()
	var spec map[string]any
	if err := json.Unmarshal(adoptedService(t, role, "json-file"), &spec); err != nil {
		t.Fatal(err)
	}
	task := spec["TaskTemplate"].(map[string]any)
	edit(spec, task["ContainerSpec"].(map[string]any), task)
	out, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func adoptionRole() manifest.Role {
	one := 1
	return manifest.Role{Service: "existing", Replicas: &one, Health: &manifest.Health{Test: []string{"CMD", "true"}}}
}

func planChanges(t *testing.T, live json.RawMessage, role manifest.Role) ([]string, error) {
	t.Helper()
	c := Controller{Engine: loggingPositive{&positiveEngine{spec: live}, "json-file", nil}, Environment: "test"}
	p, _, err := c.planAdoption(context.Background(), adoptionRequest(role))
	return p.Changes, err
}

func TestAdoptionPlanNamesInheritedFields(t *testing.T) {
	role := adoptionRole()
	live := changedByHand(t, role, func(_, container, task map[string]any) {
		container["User"] = "1000"
		container["Command"] = []any{"/entry", "--serve"}
		container["Hostname"] = "handmade"
		task["Placement"] = map[string]any{"Constraints": []any{"node.role==manager"}}
	})
	got, err := planChanges(t, live, role)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		`role web: entrypoint ["/entry","--serve"] is not in the manifest; the first deploy drops it (set roles.web.entrypoint to keep it)`,
		`role web: hostname "handmade" is not in the manifest; the first deploy drops it`,
		`role web: user "1000" is not in the manifest; the first deploy drops it (set roles.web.user to keep it)`,
		`role web: placement {"Constraints":["node.role==manager"]} is not in the manifest; the first deploy drops it`,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("changes:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestAdoptionPlanHasNoLineForAFieldTheManifestSets(t *testing.T) {
	role := adoptionRole()
	live := changedByHand(t, role, func(_, container, _ map[string]any) {
		container["User"] = "1000"
		container["Command"] = []any{"/entry"}
	})
	role.User, role.Entrypoint = "1000", []string{"/entry"}
	got, err := planChanges(t, live, role)
	if err != nil || len(got) != 0 {
		t.Fatalf("changes %q: %v", got, err)
	}
	// A manifest that sets another value changes it, and says so.
	role.User = "2000"
	got, err = planChanges(t, live, role)
	want := []string{`role web: user "2000" (now "1000"), applied by the next deploy`}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("changes %q, want %q: %v", got, want, err)
	}
}

func TestAdoptionStillRefusesUnsafeSettings(t *testing.T) {
	for name, edit := range map[string]func(spec, container, task map[string]any){
		"capabilities": func(_, c, _ map[string]any) { c["CapabilityAdd"] = []any{"SYS_ADMIN"} },
		"privileges": func(_, c, _ map[string]any) {
			c["Privileges"] = map[string]any{"NoNewPrivileges": false, "SELinuxContext": map[string]any{"Disable": true}}
		},
		"host mount": func(_, c, _ map[string]any) {
			c["Mounts"] = []any{map[string]any{"Type": "bind", "Source": "/", "Target": "/host"}}
		},
		"published port": func(spec, _, _ map[string]any) {
			spec["EndpointSpec"] = map[string]any{"Ports": []any{map[string]any{"TargetPort": 80, "PublishedPort": 8080}}}
		},
		"another network": func(_, _, task map[string]any) {
			task["Networks"] = []any{map[string]any{"Target": "other"}}
		},
		"two networks": func(_, _, task map[string]any) {
			task["Networks"] = []any{map[string]any{"Target": "net"}, map[string]any{"Target": "other"}}
		},
		"isolation": func(_, c, _ map[string]any) { c["Isolation"] = "hyperv" },
		"host DNS":  func(_, c, _ map[string]any) { c["DNSConfig"] = map[string]any{"Nameservers": []any{"1.1.1.1"}} },
		"global mode": func(spec, _, _ map[string]any) {
			spec["Mode"] = map[string]any{"Global": map[string]any{}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			role := adoptionRole()
			if _, err := planChanges(t, changedByHand(t, role, edit), role); err == nil {
				t.Fatal("adoption planned")
			}
		})
	}
	// The same service without the edit is adopted.
	role := adoptionRole()
	if _, err := planChanges(t, changedByHand(t, role, func(_, _, _ map[string]any) {}), role); err != nil {
		t.Fatal(err)
	}
}

func TestSpecChangesNeverShowsAnEnvValue(t *testing.T) {
	live := json.RawMessage(`{"Name":"svc","TaskTemplate":{"ContainerSpec":{"Image":"i","Env":["API_KEY=hunter2-live","PLAIN=1","SAME=x","BERTHY_VERSION=old"]}}}`)
	desired := json.RawMessage(`{"Name":"svc","TaskTemplate":{"ContainerSpec":{"Image":"i","Env":["API_KEY=hunter2-new","SAME=x","NEW=y"]}}}`)
	got := specChanges("web", live, desired, "")
	want := []string{
		"role web: env PLAIN is not in the manifest; the first deploy drops it",
		"role web: env API_KEY changes value, applied by the next deploy",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("changes %q, want %q", got, want)
	}
	if strings.Contains(strings.Join(got, "\n"), "hunter2") {
		t.Fatal("an environment value was shown")
	}
}

func TestSpecChangesSetsAsideWhatDockerAndBerthyAdd(t *testing.T) {
	live := json.RawMessage(`{"Name":"svc","Labels":{"berthy.operation":"old","team":"x"},"TaskTemplate":{"Runtime":"container","ForceUpdate":3,"ContainerSpec":{"Image":"i","Isolation":"default","Labels":{"berthy.operation":"old"},"DNSConfig":{}}},"EndpointSpec":{"Mode":"vip"}}`)
	desired := json.RawMessage(`{"Name":"svc","Labels":{"berthy.operation":"new"},"TaskTemplate":{"ContainerSpec":{"Image":"i","Labels":{"berthy.operation":"new"}}}}`)
	got := specChanges("web", live, desired, "")
	want := []string{"role web: service label team \"x\" is not in the manifest; the first deploy drops it"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("changes %q, want %q", got, want)
	}
}

func TestSpecChangesShortensALongValue(t *testing.T) {
	long := strings.Repeat("a", 200)
	live := json.RawMessage(`{"TaskTemplate":{"ContainerSpec":{"Dir":"` + long + `"}}}`)
	got := specChanges("web", live, json.RawMessage(`{}`), "")
	if len(got) != 1 || strings.Contains(got[0], long) || !strings.Contains(got[0], "...") {
		t.Fatalf("changes %q", got)
	}
}
