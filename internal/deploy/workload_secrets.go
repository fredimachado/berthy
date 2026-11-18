package deploy

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"sort"

	"berthy/internal/manifest"
)

// secretMessage matches the refusal that names a secret an app's variables read but cannot get. A secret's name is not
// secret, so these two texts are public.
var secretMessage = regexp.MustCompile(`^secret [A-Za-z_][A-Za-z0-9_]* (is not set|is a registry password, not a workload secret)$`)

// workloadSecret reads the value of a secret that an app's variables use, from env.secret or from a template. A secret that
// is not stored, or that holds a registry password, is a Refusal that names it. Any other failure of the store, such as an
// I/O error or a value sealed with another key, is a plain error: it is not a missing secret.
func (c *Controller) workloadSecret(ctx context.Context, name string) ([]byte, error) {
	if c.Store == nil {
		return nil, errors.New("workload secret unavailable")
	}
	info, value, err := c.Store.Secret(ctx, name)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, Refusal("secret " + name + " is not set")
	}
	if err != nil {
		return nil, errors.New("workload secret unavailable")
	}
	if info.Registry != "" {
		return nil, Refusal("secret " + name + " is a registry password, not a workload secret")
	}
	return value, nil
}

// resolveEnv is the variables an app's containers get: the one place that fills in an app's env. A secret it cannot get is
// the Refusal workloadSecret gives; anything else is a plain error.
func (c *Controller) resolveEnv(ctx context.Context, env manifest.Env) ([]manifest.EnvVar, error) {
	vars, err := env.Resolve(func(name string) ([]byte, error) { return c.workloadSecret(ctx, name) })
	var r Refusal
	if errors.As(err, &r) {
		return nil, r
	}
	return vars, err
}

// redactionValues are the values no output may show: every stored secret, and the filled value of every template of every
// stored release, because a template's value is a secret as a whole. They come longest first, so a filled value is replaced
// whole before the secrets inside it are. A template whose secrets are not all stored never ran, so it adds nothing.
func (c *Controller) redactionValues(ctx context.Context) ([][]byte, error) {
	infos, err := c.Store.Secrets(ctx)
	if err != nil {
		return nil, err
	}
	stored := map[string][]byte{}
	var values [][]byte
	for _, info := range infos {
		_, value, e := c.Store.Secret(ctx, info.Name)
		if e != nil {
			return nil, e
		}
		stored[info.Name] = value
		values = append(values, value)
	}
	get := func(name string) ([]byte, error) {
		if v, ok := stored[name]; ok {
			return v, nil
		}
		return nil, errors.New("not stored")
	}
	history, err := c.Store.History(ctx, "")
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, release := range history {
		m, e := manifest.DecodeRelease(release.Manifest, release.App == manifest.ConnectorAppName)
		if e != nil {
			// A release that cannot be read was never deployed with a template, and the stored secrets are redacted anyway.
			continue
		}
		for _, app := range m.Apps {
			for name, template := range app.Env.Template {
				if seen[template] {
					continue
				}
				seen[template] = true
				vars, e := manifest.Env{Template: map[string]string{name: template}}.Resolve(get)
				if e == nil && len(vars) == 1 {
					values = append(values, []byte(vars[0].Value))
				}
			}
		}
	}
	sort.SliceStable(values, func(i, j int) bool { return len(values[i]) > len(values[j]) })
	return values, nil
}
