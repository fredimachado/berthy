package deploy

import (
	"encoding/json"
	"errors"
	"time"

	"berthy/internal/manifest"
)

func serviceSpec(env, project, app, role, operation, network, image string, values []string, r manifest.Role, drain string) (json.RawMessage, error) {
	if r.Resources != nil || len(r.Volumes) > 0 || r.Port != 0 {
		return nil, errors.New("role settings not supported by rollout")
	}
	if r.Health == nil {
		return nil, errors.New("role healthcheck required")
	}
	grace, err := time.ParseDuration(drain)
	if err != nil {
		return nil, errors.New("invalid drain timeout")
	}
	labels := map[string]string{"berthy.environment": env, "berthy.project": project, "berthy.app": app, "berthy.role": role, "berthy.operation": operation}
	health := map[string]any{"Test": r.Health.Test}
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
	replicas := 1
	if r.Replicas != nil {
		replicas = *r.Replicas
	}
	container := map[string]any{"Image": image, "Env": values, "Healthcheck": health, "StopGracePeriod": int64(grace), "Labels": labels}
	if len(r.Command) > 0 {
		container["Args"] = r.Command
	}
	config := map[string]any{"Parallelism": 1, "FailureAction": "rollback", "Order": "start-first", "MaxFailureRatio": 0, "Monitor": int64(30 * time.Second)}
	spec := map[string]any{"Name": project + "-" + app + "-" + role, "Labels": labels, "TaskTemplate": map[string]any{"ContainerSpec": container, "Networks": []any{map[string]any{"Target": network}}}, "Mode": map[string]any{"Replicated": map[string]any{"Replicas": replicas}}, "UpdateConfig": config, "RollbackConfig": config}
	return json.Marshal(spec)
}
