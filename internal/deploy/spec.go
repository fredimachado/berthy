package deploy

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"berthy/internal/manifest"
)

// defaultLogOptions is the rotation Berthy gives a role that sets no logging, when the daemon's driver accepts it.
var defaultLogOptions = map[string]string{"max-size": "10m", "max-file": "3"}

// roleLogDriver is the Swarm TaskTemplate.LogDriver of a role: the role's own logging when it has one; otherwise the
// daemon's driver with defaultLogOptions when that driver is json-file or local; otherwise nil, which leaves Docker's
// default in place with no options.
func roleLogDriver(r manifest.Role, daemonDriver string) map[string]any {
	if r.Logging != nil {
		return logDriverSpec(r.Logging.Driver, r.Logging.Options)
	}
	if daemonDriver != "json-file" && daemonDriver != "local" {
		return nil
	}
	return logDriverSpec(daemonDriver, defaultLogOptions)
}

// logDriverSpec is a LogDriver with its own copy of the options. Options is left out when there are none, as Docker omits
// an empty map when it stores a service.
func logDriverSpec(name string, options map[string]string) map[string]any {
	ld := map[string]any{"Name": name}
	if len(options) > 0 {
		opts := make(map[string]any, len(options))
		for k, v := range options {
			opts[k] = v
		}
		ld["Options"] = opts
	}
	return ld
}

// sameLogDriver reports whether two LogDrivers are the same. A missing one and an empty one are the same, as are null and
// absent options, because Docker stores neither.
func sameLogDriver(a, b map[string]any) bool {
	x, _ := json.Marshal(map[string]any{"LogDriver": a})
	y, _ := json.Marshal(map[string]any{"LogDriver": b})
	return sameSpec(x, y)
}

// describeLogDriver renders a LogDriver as "json-file max-file=3 max-size=10m" (options sorted), or "Docker's default" for
// none.
func describeLogDriver(ld map[string]any) string {
	name, _ := ld["Name"].(string)
	if name == "" {
		return "Docker's default"
	}
	opts, _ := ld["Options"].(map[string]any)
	keys := make([]string, 0, len(opts))
	for k := range opts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := []string{name}
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%v", k, opts[k]))
	}
	return strings.Join(parts, " ")
}

// serviceSpec is the Swarm service of a role at one release. order is how an update replaces the old version, from
// App.UpdateOrder; the update and its rollback config both carry it.
func serviceSpec(env, project, app, role, operation, network, image string, values []string, r manifest.Role, order, drain string, imageHealth json.RawMessage, daemonLogDriver string) (json.RawMessage, error) {

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
		mounts, e := volumeMounts(r.Volumes)
		if e != nil {
			return nil, e
		}
		container["Mounts"] = mounts
	}
	if len(r.Command) > 0 {
		container["Args"] = r.Command
	}
	if len(r.Entrypoint) > 0 {
		container["Command"] = r.Entrypoint
	}
	if r.User != "" {
		container["User"] = r.User
	}
	// A rollout gives the new version a bounded number of failed starts, and Berthy, not Swarm, decides when that is used
	// up: "continue" keeps Swarm replacing a failing new task under the restart policy while Berthy counts the failures
	// and restores the previous spec itself. Swarm's own "rollback" would give up after the first failure.
	update := map[string]any{"Parallelism": 1, "FailureAction": "continue", "Order": order, "MaxFailureRatio": 0, "Monitor": int64(5 * time.Second)}
	rollback := map[string]any{"Parallelism": 1, "FailureAction": "rollback", "Order": order, "MaxFailureRatio": 0, "Monitor": int64(5 * time.Second)}
	// Written out so the cadence is documented: a task that exits is replaced after 5 seconds, without limit. In steady
	// state a crashed app always comes back; only a rollout stops early.
	restart := map[string]any{"Condition": "any", "Delay": int64(5 * time.Second), "MaxAttempts": 0}
	spec := map[string]any{"Name": project + "-" + app + "-" + role, "Labels": labels, "TaskTemplate": map[string]any{"ContainerSpec": container, "Networks": []any{map[string]any{"Target": network}}, "RestartPolicy": restart}, "Mode": map[string]any{"Replicated": map[string]any{"Replicas": replicas}}, "UpdateConfig": update, "RollbackConfig": rollback}
	if r.Resources != nil {
		nano, memory, e := resourceLimits(r.Resources)
		if e != nil {
			return nil, e
		}
		limits := map[string]any{}
		if nano != 0 {
			limits["NanoCPUs"] = nano
		}
		if memory != 0 {
			limits["MemoryBytes"] = memory
		}
		spec["TaskTemplate"].(map[string]any)["Resources"] = map[string]any{"Limits": limits}
	}
	if ld := roleLogDriver(r, daemonLogDriver); ld != nil {
		spec["TaskTemplate"].(map[string]any)["LogDriver"] = ld
	}
	return json.Marshal(spec)
}

// resourceLimits parses a role's limits: NanoCPUs and bytes of memory, each 0 when the role sets none. A service takes them
// as Resources.Limits and a one-off container as HostConfig.
func resourceLimits(r *manifest.Resources) (nanoCPUs, memory int64, err error) {
	if r == nil {
		return 0, 0, nil
	}
	if r.CPU != "" {
		if nanoCPUs, err = manifest.ParseCPU(r.CPU); err != nil {
			return 0, 0, err
		}
	}
	if r.Memory != "" {
		if memory, err = manifest.ParseMemory(r.Memory); err != nil {
			return 0, 0, err
		}
	}
	return nanoCPUs, memory, nil
}

// volumeMounts is the Docker mounts of a role's named volumes, each written as name:/path or name:/path:ro. A service takes
// them in its container spec and a one-off container in its host config, so a command of the role sees the files the role does.
func volumeMounts(volumes []string) ([]any, error) {
	mounts := make([]any, 0, len(volumes))
	for _, raw := range volumes {
		m, err := manifest.ParseMount(raw)
		if err != nil {
			return nil, err
		}
		mounts = append(mounts, map[string]any{"Type": "volume", "Source": m.Source, "Target": m.Target, "ReadOnly": m.ReadOnly})
	}
	return mounts, nil
}

// hostConfig is the HostConfig of a one-off container that has limits or volumes.
type hostConfig struct {
	NanoCPUs int64 `json:"NanoCpus,omitempty"`
	Memory   int64 `json:"Memory,omitempty"`
	Mounts   []any `json:"Mounts,omitempty"`
	// LogConfig is set to the driver "none" for a container whose output is the product of its command.
	LogConfig *logConfig `json:"LogConfig,omitempty"`
}

// logConfig is the log driver of a container.
type logConfig struct {
	Type string `json:"Type"`
}
