package manifest

import (
	"fmt"
	"sort"
	"strings"
)

// A template is the value of a variable under env.template: text that refers to secrets as ${NAME}. `$$` writes one `$`, and
// any other `$` is an error. There are no defaults, no nesting and no references to variables.

// templateParts walks t and calls text for each run of literal text, with `$$` already written as `$`, and ref for each
// ${NAME}. An error names the offset of the `$` that starts a malformed reference.
func templateParts(t string, text func(string), ref func(string)) error {
	var lit strings.Builder
	for i := 0; i < len(t); i++ {
		if t[i] != '$' {
			lit.WriteByte(t[i])
			continue
		}
		switch {
		case i+1 < len(t) && t[i+1] == '$':
			lit.WriteByte('$')
			i++
		case i+1 < len(t) && t[i+1] == '{':
			end := strings.IndexByte(t[i:], '}')
			if end < 0 || !envName.MatchString(t[i+2:i+end]) {
				return malformedReference(i)
			}
			text(lit.String())
			lit.Reset()
			ref(t[i+2 : i+end])
			i += end
		default:
			return malformedReference(i)
		}
	}
	text(lit.String())
	return nil
}

func malformedReference(offset int) error {
	return fmt.Errorf("malformed reference at offset %d: write ${NAME} with the name of a secret, or $$ for a dollar sign", offset)
}

// TemplateRefs returns the secrets a template refers to, in order of first use. An error names the offset of a malformed
// reference.
func TemplateRefs(t string) ([]string, error) {
	var refs []string
	seen := map[string]bool{}
	err := templateParts(t, func(string) {}, func(name string) {
		if !seen[name] {
			seen[name] = true
			refs = append(refs, name)
		}
	})
	if err != nil {
		return nil, err
	}
	return refs, nil
}

// EnvVar is one variable a container gets. Sensitive is set for a value that comes from a secret, whole or in part.
type EnvVar struct {
	Name, Value string
	Sensitive   bool
}

// Resolve turns the env into the container's variables, sorted by name: the plain values, the value of each secret, and each
// template filled in. get reads a secret's value. A secret it cannot read is an error that names the secret and wraps get's.
func (e Env) Resolve(get func(secret string) ([]byte, error)) ([]EnvVar, error) {
	read := func(name string) (string, error) {
		v, err := get(name)
		if err != nil {
			return "", fmt.Errorf("secret %s: %w", name, err)
		}
		return string(v), nil
	}
	var out []EnvVar
	for k, v := range e.Plain {
		out = append(out, EnvVar{Name: k, Value: v})
	}
	for _, s := range e.Secret {
		v, err := read(s.Secret)
		if err != nil {
			return nil, err
		}
		out = append(out, EnvVar{Name: s.Var, Value: v, Sensitive: true})
	}
	for k, t := range e.Template {
		var b strings.Builder
		var failed error
		err := templateParts(t, func(s string) { b.WriteString(s) }, func(name string) {
			v, err := read(name)
			if err != nil && failed == nil {
				failed = err
			}
			b.WriteString(v)
		})
		if err == nil {
			err = failed
		}
		if err != nil {
			return nil, err
		}
		out = append(out, EnvVar{Name: k, Value: b.String(), Sensitive: true})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// SensitiveVars returns the names of the variables whose values come from secrets, sorted: the env.secret variables and the
// templates.
func (e Env) SensitiveVars() []string {
	var names []string
	for _, s := range e.Secret {
		names = append(names, s.Var)
	}
	for k := range e.Template {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}
