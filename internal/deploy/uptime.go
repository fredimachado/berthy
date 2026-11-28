package deploy

import (
	"encoding/json"
	"errors"
	"math"
	"time"

	"berthy/internal/manifest"
)

// retainedUptime requires both a threshold and an explicit Docker health override in the retained spec.
func retainedUptime(health *manifest.Health, spec json.RawMessage) (time.Duration, error) {
	threshold, err := health.UptimeThreshold()
	if err != nil || threshold == 0 {
		return 0, err
	}
	var service struct {
		TaskTemplate struct {
			ContainerSpec struct {
				Healthcheck struct{ Test []string }
			}
		}
	}
	if err := json.Unmarshal(spec, &service); err != nil {
		return 0, err
	}
	if test := service.TaskTemplate.ContainerSpec.Healthcheck.Test; len(test) == 1 && test[0] == "NONE" {
		return threshold, nil
	}
	return 0, nil
}

func deployStartupBudget(app manifest.App) (time.Duration, error) {
	value := app.Deploy.DeployTimeout
	if value == "" {
		value = "120s"
	}
	budget, err := time.ParseDuration(value)
	if err != nil || budget <= 0 {
		return 0, errors.New("invalid deploy timeout")
	}
	return budget, nil
}

type replacementSpec struct {
	Mode         struct{ Replicated *struct{ Replicas int } }
	UpdateConfig *struct{ Parallelism uint64 }
}

func (s replacementSpec) parallelism() uint64 {
	if s.UpdateConfig == nil {
		return 1
	}
	return s.UpdateConfig.Parallelism
}

func replacementBudget(app manifest.App, fromReplicas, toReplicas int, parallelism uint64) (time.Duration, error) {
	if fromReplicas < 0 || toReplicas < 0 {
		return 0, errors.New("invalid replica count")
	}
	if toReplicas == 0 {
		return 0, nil
	}
	startup, err := deployStartupBudget(app)
	if err != nil {
		return 0, err
	}
	// A scale from zero still needs one startup batch. Swarm can start new slots together while replacing existing slots.
	replacing := min(fromReplicas, toReplicas)
	batches := uint64(1)
	if parallelism > 0 && replacing > 0 {
		batches += uint64(replacing-1) / parallelism
	}
	if batches > uint64(math.MaxInt64/startup) {
		return 0, errors.New("replacement startup allowance is too large")
	}
	return startup * time.Duration(batches), nil
}

const serviceCleanupAllowance = 2 * time.Minute

func addStartupAllowance(total, budget time.Duration) (time.Duration, error) {
	if budget > math.MaxInt64-total {
		return 0, errors.New("replacement startup allowance is too large")
	}
	return total + budget, nil
}
