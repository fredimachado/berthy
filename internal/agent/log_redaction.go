package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"regexp"
	"sort"
	"strings"

	"berthy/internal/api"
	"berthy/internal/deploy"
	"berthy/internal/engine"
	"berthy/internal/logs"
	"berthy/internal/manifest"
)

type managedLogEngine interface {
	InspectService(context.Context, string) (engine.ServiceSnapshot, error)
	ContainerEnv(context.Context, string) ([]string, error)
}

func managedLogSecrets(ctx context.Context, d *deploy.Controller, e managedLogEngine, app api.App, ids []logs.Identity) ([]string, error) {
	if d == nil || d.Store == nil || (app.Ref.Kind != "swarm" && app.Ref.Kind != "image") {
		return nil, nil
	}
	bindings, err := d.Store.Bindings(ctx, "")
	if err != nil {
		return nil, err
	}
	var bindingApp string
	if app.Ref.Kind == "image" {
		roles := map[string]bool{}
		for _, b := range bindings {
			if b.App == app.Ref.Name {
				live, err := e.InspectService(ctx, b.Name)
				if err != nil || live.ID != b.ServiceID {
					return nil, errors.New("managed identity mismatch")
				}
				roles[b.Role] = true
			}
		}
		if len(roles) == 0 {
			return nil, errors.New("managed identity unavailable")
		}
		for _, id := range ids {
			if !roles[id.Role] {
				return nil, errors.New("managed role unavailable")
			}
		}
		bindingApp = app.Ref.Name
	} else {
		live, err := e.InspectService(ctx, app.Ref.Name)
		if err != nil {
			return nil, err
		}
		for _, b := range bindings {
			if b.Name == app.Ref.Name || b.ServiceID == live.ID {
				if b.Name != app.Ref.Name || b.ServiceID != live.ID {
					return nil, errors.New("managed identity mismatch")
				}
				bindingApp = b.App
				break
			}
		}
		if bindingApp == "" {
			var spec struct{ Labels map[string]string }
			if json.Unmarshal(live.Spec, &spec) != nil {
				return nil, errors.New("service identity unavailable")
			}
			if spec.Labels["berthy.environment"] != "" || spec.Labels["berthy.app"] != "" {
				return nil, errors.New("unbound managed service")
			}
			return nil, nil
		}
	}
	active, err := d.Store.Active(ctx)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, errors.New("managed operation unavailable")
	}
	if err == nil && active.App == bindingApp {
		return nil, errors.New("managed operation in progress")
	}
	history, err := d.Store.History(ctx, bindingApp)
	if err != nil || len(history) == 0 {
		return nil, errors.New("managed configuration unavailable")
	}
	// secretNames are the secrets the releases read; variables are the containers' names that take their values.
	secretNames := map[string]bool{}
	variables := map[string]bool{}
	for _, release := range history {
		m, err := manifest.DecodeRelease(release.Manifest, bindingApp == manifest.ConnectorAppName)
		if err != nil {
			return nil, errors.New("managed configuration unavailable")
		}
		a, ok := m.Apps[bindingApp]
		if !ok {
			return nil, errors.New("managed configuration unavailable")
		}
		for _, name := range a.Env.SecretNames() {
			secretNames[name] = true
		}
		for _, name := range a.Env.SensitiveVars() {
			variables[name] = true
		}
	}
	var values []string
	add := func(value string) error {
		if value == "" {
			return nil
		}
		if strings.ContainsAny(value, "\r\n") {
			return errors.New("unsupported multiline secret")
		}
		values = append(values, value)
		return nil
	}
	for name := range secretNames {
		_, value, err := d.Store.Secret(ctx, name)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, errors.New("secret unavailable")
		}
		if err == nil {
			if err = add(string(value)); err != nil {
				return nil, err
			}
		}
	}
	for _, id := range ids {
		env, err := e.ContainerEnv(ctx, id.ContainerID)
		if err != nil {
			return nil, errors.New("container configuration unavailable")
		}
		if len(secretNames) > 0 && env == nil {
			return nil, errors.New("container configuration unavailable")
		}
		for _, v := range env {
			name, value, ok := strings.Cut(v, "=")
			if ok && variables[name] {
				if err = add(value); err != nil {
					return nil, err
				}
			}
		}
	}
	sort.Slice(values, func(i, j int) bool { return len(values[i]) > len(values[j]) })
	return values, nil
}
func redactRecords(rows []logs.Record, secrets []string, re *regexp.Regexp) []logs.Record {
	out := make([]logs.Record, 0, len(rows))
	for _, r := range rows {
		if r.Truncated && len(secrets) > 0 {
			prefix := strings.TrimSuffix(r.Message, logs.TruncationMarker)
			r.Message = redactCutPrefix(prefix, secrets) + logs.TruncationMarker
		} else {
			for _, s := range secrets {
				if s != "" {
					r.Message = strings.ReplaceAll(r.Message, s, "[REDACTED]")
				}
			}
		}
		if re == nil || re.MatchString(r.Message) {
			out = append(out, r)
		}
	}
	return out
}

func redactCutPrefix(prefix string, secrets []string) string {
	var out strings.Builder
	coveredUntil := 0
	for i := 0; i < len(prefix); i++ {
		end := coveredUntil
		for _, secret := range secrets {
			if secret == "" {
				continue
			}
			remaining := prefix[i:]
			if strings.HasPrefix(remaining, secret) {
				end = max(end, i+len(secret))
			} else if len(remaining) < len(secret) && strings.HasPrefix(secret, remaining) {
				end = len(prefix)
			}
		}
		if end > i {
			if i >= coveredUntil {
				out.WriteString("[REDACTED]")
			}
			coveredUntil = end
		} else {
			out.WriteByte(prefix[i])
		}
	}
	return out.String()
}
