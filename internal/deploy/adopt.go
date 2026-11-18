package deploy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"berthy/internal/manifest"
)

// adoptableService refuses a live service that Berthy must not take over: unsafe container settings (capabilities,
// privileges, secrets, host DNS and the like, isolation other than the default), host mounts, published ports, or a network
// other than the shared one. It is called only by planAdoption, which reads the replica count and so refuses a service that
// is not replicated itself.
func adoptableService(live json.RawMessage, network string) error {
	if err := validateInheritedRuntime(live); err != nil {
		return err
	}
	var x struct {
		TaskTemplate struct {
			Networks      []struct{ Target string }
			ContainerSpec struct{ Isolation string }
		}
	}
	if json.Unmarshal(live, &x) != nil {
		return errors.New("invalid live service spec")
	}
	if iso := x.TaskTemplate.ContainerSpec.Isolation; iso != "" && iso != "default" {
		return errors.New("unsafe inherited container settings")
	}
	if len(x.TaskTemplate.Networks) != 1 || x.TaskTemplate.Networks[0].Target != network {
		return errors.New("incompatible service network")
	}
	return nil
}

// adoptionEnv is the environment of an adopted role's release as far as an adoption plan can tell, for specChanges: the
// manifest's plain variables, and for each secret variable the service's own value of it, because a secret is read only
// when a deploy runs.
func adoptionEnv(env manifest.Env, live []string) []string {
	have := map[string]string{}
	for _, e := range live {
		name, value, _ := strings.Cut(e, "=")
		have[name] = value
	}
	var out []string
	for k, v := range env.Plain {
		out = append(out, k+"="+v)
	}
	for _, s := range env.Secret {
		if value, ok := have[s.Var]; ok {
			out = append(out, s.Var+"="+value)
		}
	}
	sort.Strings(out)
	return out
}

// specField names a field of a service spec the way a manifest does, and says which role key keeps it ("" for none).
type specField struct{ name, key string }

// specFields are the fields of a spec that have a manifest name. Every other field is shown by its Swarm JSON path.
var specFields = map[string]specField{
	"TaskTemplate.ContainerSpec.User":             {"user", "user"},
	"TaskTemplate.ContainerSpec.Command":          {"entrypoint", "entrypoint"},
	"TaskTemplate.ContainerSpec.Args":             {"command", "command"},
	"TaskTemplate.ContainerSpec.Dir":              {"dir", ""},
	"TaskTemplate.ContainerSpec.Groups":           {"groups", ""},
	"TaskTemplate.ContainerSpec.ReadOnly":         {"read_only", ""},
	"TaskTemplate.ContainerSpec.Hostname":         {"hostname", ""},
	"TaskTemplate.ContainerSpec.Mounts":           {"volumes", "volumes"},
	"TaskTemplate.ContainerSpec.Healthcheck.Test": {"health.test", "health.test"},
	"TaskTemplate.Placement":                      {"placement", ""},
	"TaskTemplate.Resources.Limits.NanoCPUs":      {"cpu limit (nano CPUs)", "resources.cpu"},
	"TaskTemplate.Resources.Limits.MemoryBytes":   {"memory limit (bytes)", "resources.memory"},
	"Mode.Replicated.Replicas":                    {"replicas", "replicas"},
}

// specUnits are the fields shown whole, not field by field.
var specUnits = map[string]bool{"TaskTemplate.Placement": true}

const shownValueLimit = 60

// specChanges lists, one line each, what the release's spec does to the live spec of a role: a field of the live spec the
// release does not produce, which the deploy drops, and one the release produces differently, which the deploy sets. A field
// only the release produces is not listed, because nothing is lost. Both specs are compared as sameSpec does, with what
// Docker adds on its own, the Berthy labels and the Swarm force-update counter set aside. Variables of the environment are
// listed by name and their values never shown, as are the BERTHY_* ones, which every deploy sets. The log driver has its own
// line, as it always had.
func specChanges(role string, live, desired json.RawMessage) []string {
	l, ok1 := comparableSpec(live)
	d, ok2 := comparableSpec(desired)
	if !ok1 || !ok2 {
		return nil
	}
	var lines []string
	have, want := flattenSpec(l), flattenSpec(d)
	paths := make([]string, 0, len(have))
	for p := range have {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		f := have[p]
		name, key := specFieldName(f.path)
		next, produced := want[p]
		switch {
		case !produced:
			line := fmt.Sprintf("role %s: %s %s is not in the manifest; the first deploy drops it", role, name, shownValue(f.value))
			if key != "" {
				line += fmt.Sprintf(" (set roles.%s.%s to keep it)", role, key)
			}
			lines = append(lines, line)
		case valueJSON(next.value) != valueJSON(f.value):
			lines = append(lines, fmt.Sprintf("role %s: %s %s (now %s), applied by the next deploy", role, name, shownValue(next.value), shownValue(f.value)))
		}
	}
	liveEnv, wantEnv := envOf(l), envOf(d)
	var dropped, changed []string
	for name, value := range liveEnv {
		next, produced := wantEnv[name]
		switch {
		case strings.HasPrefix(name, "BERTHY_"):
		case !produced:
			dropped = append(dropped, name)
		case next != value:
			changed = append(changed, name)
		}
	}
	sort.Strings(dropped)
	sort.Strings(changed)
	if len(dropped) > 0 {
		lines = append(lines, fmt.Sprintf("role %s: env %s %s not in the manifest; the first deploy drops %s", role, strings.Join(dropped, ", "), pick(len(dropped), "is", "are"), pick(len(dropped), "it", "them")))
	}
	if len(changed) > 0 {
		lines = append(lines, fmt.Sprintf("role %s: env %s %s, applied by the next deploy", role, strings.Join(changed, ", "), pick(len(changed), "changes value", "change value")))
	}
	if want, have := specLogDriver(d), specLogDriver(l); !sameLogDriver(want, have) {
		lines = append(lines, fmt.Sprintf("role %s: logging %s (now %s), applied by the next deploy", role, describeLogDriver(want), describeLogDriver(have)))
	}
	return lines
}

func pick(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// comparableSpec decodes a spec and sets aside what is not a setting of the role: the name, what Docker adds, the Berthy
// labels and the force-update counter, which every restart changes.
func comparableSpec(raw json.RawMessage) (map[string]any, bool) {
	var v map[string]any
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if d.Decode(&v) != nil {
		return nil, false
	}
	dropEngineDefaults(v)
	delete(v, "Name")
	if task, ok := v["TaskTemplate"].(map[string]any); ok {
		delete(task, "ForceUpdate")
		if c, ok := task["ContainerSpec"].(map[string]any); ok {
			dropBerthyLabels(c)
		}
	}
	dropBerthyLabels(v)
	if ep, ok := v["EndpointSpec"].(map[string]any); ok && ep["Mode"] == "vip" {
		delete(ep, "Mode")
	}
	canonicalSpec(v)
	return v, true
}

func dropBerthyLabels(holder map[string]any) {
	labels, _ := holder["Labels"].(map[string]any)
	for k := range labels {
		if strings.HasPrefix(k, "berthy.") {
			delete(labels, k)
		}
	}
}

type specLeaf struct {
	path  []string
	value any
}

// flattenSpec lists the fields of a spec that hold a value, by dotted path. The log driver and the environment are left out:
// they are compared on their own. A list is one value.
func flattenSpec(spec map[string]any) map[string]specLeaf {
	out := map[string]specLeaf{}
	var walk func(v any, path []string)
	walk = func(v any, path []string) {
		joined := strings.Join(path, ".")
		if joined == "TaskTemplate.LogDriver" || joined == "TaskTemplate.ContainerSpec.Env" {
			return
		}
		if m, ok := v.(map[string]any); ok && !specUnits[joined] {
			for k, e := range m {
				walk(e, append(path[:len(path):len(path)], k))
			}
			return
		}
		out[joined] = specLeaf{path, v}
	}
	for k, e := range spec {
		walk(e, []string{k})
	}
	return out
}

func specFieldName(path []string) (name, key string) {
	if f, ok := specFields[strings.Join(path, ".")]; ok {
		return f.name, f.key
	}
	switch {
	case len(path) == 2 && path[0] == "Labels":
		return "service label " + path[1], ""
	case len(path) == 4 && path[0] == "TaskTemplate" && path[1] == "ContainerSpec" && path[2] == "Labels":
		return "container label " + path[3], ""
	}
	return strings.Join(path, "."), ""
}

// valueJSON is a field's value as compact JSON.
func valueJSON(v any) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if enc.Encode(v) != nil {
		return "?"
	}
	return strings.TrimSpace(b.String())
}

// shownValue is a field's value in one short line.
func shownValue(v any) string {
	s := valueJSON(v)
	if r := []rune(s); len(r) > shownValueLimit {
		s = string(r[:shownValueLimit]) + "..."
	}
	return s
}

// envOf is the environment of a decoded spec, by variable name.
func envOf(spec map[string]any) map[string]string {
	out := map[string]string{}
	task, _ := spec["TaskTemplate"].(map[string]any)
	c, _ := task["ContainerSpec"].(map[string]any)
	list, _ := c["Env"].([]any)
	for _, e := range list {
		s, _ := e.(string)
		name, value, _ := strings.Cut(s, "=")
		out[name] = value
	}
	return out
}

// specLogDriver is the log driver of a decoded spec, nil when it has none.
func specLogDriver(spec map[string]any) map[string]any {
	task, _ := spec["TaskTemplate"].(map[string]any)
	ld, _ := task["LogDriver"].(map[string]any)
	return ld
}
