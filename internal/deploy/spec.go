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
		// Docker keeps no zero duration in a health check. A zero one, such as the default start period, is left out
		// rather than written, so the created service matches what Docker stores.
		for key, value := range map[string]string{"Interval": r.Health.Interval, "Timeout": r.Health.Timeout, "StartPeriod": r.Health.StartPeriod, "StartInterval": r.Health.StartInterval} {
			if value != "" {
				d, e := time.ParseDuration(value)
				if e != nil {
					return nil, e
				}
				if d != 0 {
					health[key] = int64(d)
				}
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
	container := map[string]any{"Image": image, "Healthcheck": health, "StopGracePeriod": int64(grace), "Labels": labels}
	if len(values) > 0 {
		container["Env"] = values
	}
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
	// A rollout gives the new version a bounded number of failed starts, and Berthy, not Swarm, decides when that is used
	// up: "continue" keeps Swarm replacing a failing new task under the restart policy while Berthy counts the failures
	// and restores the previous spec itself. Swarm's own "rollback" would give up after the first failure.
	update := map[string]any{"Parallelism": 1, "FailureAction": "continue", "Order": "start-first", "MaxFailureRatio": 0, "Monitor": int64(5 * time.Second)}
	rollback := map[string]any{"Parallelism": 1, "FailureAction": "rollback", "Order": "start-first", "MaxFailureRatio": 0, "Monitor": int64(5 * time.Second)}
	// Written out so the cadence is documented: a task that exits is replaced after 5 seconds, without limit. In steady
	// state a crashed app always comes back; only a rollout stops early.
	restart := map[string]any{"Condition": "any", "Delay": int64(5 * time.Second), "MaxAttempts": 0}
	spec := map[string]any{"Name": project + "-" + app + "-" + role, "Labels": labels, "TaskTemplate": map[string]any{"ContainerSpec": container, "Networks": []any{map[string]any{"Target": network}}, "RestartPolicy": restart}, "Mode": map[string]any{"Replicated": map[string]any{"Replicas": replicas}}, "UpdateConfig": update, "RollbackConfig": rollback}
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
