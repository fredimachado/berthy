package deploy

import (
	"encoding/json"
	"errors"
	"time"

	"berthy/internal/manifest"
)

func serviceSpec(env, project, app, role, operation, network, image string, values []string, r manifest.Role, drain string, imageHealth json.RawMessage) (json.RawMessage, error) {

	grace, err := time.ParseDuration(drain)
	if err != nil {
		return nil, errors.New("invalid drain timeout")
	}
	labels := map[string]string{"berthy.environment": env, "berthy.project": project, "berthy.app": app, "berthy.role": role, "berthy.operation": operation}
	health := map[string]any{}
	if r.Health == nil {
		if json.Unmarshal(imageHealth, &health) != nil {
			return nil, errors.New("role healthcheck required")
		}
		test, ok := health["Test"].([]any)
		if !ok || len(test) == 0 || test[0] == "NONE" {
			return nil, errors.New("role healthcheck required")
		}
	} else {
		health["Test"] = r.Health.Test
		for key, value := range map[string]string{"Interval": r.Health.Interval, "Timeout": r.Health.Timeout, "StartPeriod": r.Health.StartPeriod, "StartInterval": r.Health.StartInterval} {
			if value != "" {
				d, e := time.ParseDuration(value)
				if e != nil {
					return nil, e
				}
				health[key] = int64(d)
			}
		}
		if r.Health.Retries != nil {
			health["Retries"] = *r.Health.Retries
		}
	}
	replicas := 1
	if r.Replicas != nil {
		replicas = *r.Replicas
	}
	container := map[string]any{"Image": image, "Env": values, "Healthcheck": health, "StopGracePeriod": int64(grace), "Labels": labels}
	if len(r.Volumes) > 0 {
		mounts := make([]any, 0, len(r.Volumes))
		for _, raw := range r.Volumes {
			m, e := manifest.ParseMount(raw)
			if e != nil {
				return nil, e
			}
			mounts = append(mounts, map[string]any{"Type": "volume", "Source": m.Source, "Target": m.Target, "ReadOnly": m.ReadOnly})
		}
		container["Mounts"] = mounts
	}
	if len(r.Command) > 0 {
		container["Args"] = r.Command
	}
	config := map[string]any{"Parallelism": 1, "FailureAction": "rollback", "Order": "start-first", "MaxFailureRatio": 0, "Monitor": int64(5 * time.Second)}
	spec := map[string]any{"Name": project + "-" + app + "-" + role, "Labels": labels, "TaskTemplate": map[string]any{"ContainerSpec": container, "Networks": []any{map[string]any{"Target": network}}}, "Mode": map[string]any{"Replicated": map[string]any{"Replicas": replicas}}, "UpdateConfig": config, "RollbackConfig": config}
	if r.Resources != nil {
		limits := map[string]any{}
		if r.Resources.CPU != "" {
			v, e := manifest.ParseCPU(r.Resources.CPU)
			if e != nil {
				return nil, e
			}
			limits["NanoCPUs"] = v
		}
		if r.Resources.Memory != "" {
			v, e := manifest.ParseMemory(r.Resources.Memory)
			if e != nil {
				return nil, e
			}
			limits["MemoryBytes"] = v
		}
		spec["TaskTemplate"].(map[string]any)["Resources"] = map[string]any{"Limits": limits}
	}
	return json.Marshal(spec)
}
