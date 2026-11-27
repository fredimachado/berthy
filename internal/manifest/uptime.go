package manifest

import (
	"fmt"
	"math"
	"time"
)

const uptimeMargin = 100 * time.Millisecond

// UptimeTiming is the threshold and Docker schedule of an uptime health check.
type UptimeTiming struct {
	Threshold, Interval, StartPeriod time.Duration
}

// UptimeTiming calculates the schedule from an uptime health check's settings.
func (h Health) UptimeTiming() (UptimeTiming, error) {
	timing, problems := h.uptimeTiming()
	if len(problems) != 0 {
		return UptimeTiming{}, problems
	}
	return timing, nil
}

func (h Health) uptimeTiming() (UptimeTiming, Problems) {
	var value string
	if h.Uptime != nil {
		value = *h.Uptime
	}
	threshold, err := time.ParseDuration(value)
	if err != nil || threshold <= 0 {
		return UptimeTiming{}, Problems{{Path: "uptime", Message: "uptime must be a positive duration"}}
	}
	if threshold > math.MaxInt64-uptimeMargin {
		return UptimeTiming{}, Problems{{Path: "uptime", Message: "uptime is too large to add the 100ms scheduling margin"}}
	}
	first := threshold + uptimeMargin
	timing := UptimeTiming{Threshold: threshold, Interval: first}
	if h.Interval != "" {
		timing.Interval, err = time.ParseDuration(h.Interval)
		if err != nil || timing.Interval < time.Millisecond {
			return UptimeTiming{}, Problems{{Path: "interval", Message: "uptime health interval must be at least 1ms"}}
		}
	}
	if timing.Interval < first {
		timing.StartPeriod = first
	}
	if h.StartPeriod != "" {
		timing.StartPeriod, err = time.ParseDuration(h.StartPeriod)
		if err != nil || timing.StartPeriod < 0 {
			return UptimeTiming{}, Problems{{Path: "start_period", Message: "uptime health start_period must be a nonnegative duration"}}
		}
		if (timing.StartPeriod == 0 && timing.Interval < first) || (timing.StartPeriod > 0 && timing.StartPeriod < first) {
			return UptimeTiming{}, Problems{{Path: "start_period", Message: fmt.Sprintf("uptime health start_period must cover %s unless the interval already does and start_period is zero", first)}}
		}
	}
	if h.StartInterval != "" {
		interval, err := time.ParseDuration(h.StartInterval)
		if err != nil || interval != timing.Interval {
			return UptimeTiming{}, Problems{{Path: "start_interval", Message: "uptime health uses interval for portable Swarm scheduling, so start_interval must equal interval or be omitted"}}
		}
	}
	return timing, nil
}

func (h Health) checkUptime(p *Problems, path, where string, app App) {
	if h.Test != nil {
		p.add(path, "%s: choose exactly one of health.test and health.uptime", where)
		return
	}
	if app.Kind == "compose" {
		p.add(path+".uptime", "%s: uptime health is for image apps", where)
		return
	}
	timing, problems := h.uptimeTiming()
	for _, problem := range problems {
		p.add(path+"."+problem.Path, "%s: %s", where, problem.Message)
	}
	if h.Timeout != "" {
		timeout, err := time.ParseDuration(h.Timeout)
		if err != nil || timeout < time.Millisecond {
			p.add(path+".timeout", "%s: uptime health timeout must be at least 1ms", where)
		}
	}
	if h.Retries != nil && *h.Retries < 1 {
		p.add(path+".retries", "%s: uptime health retries must be positive", where)
	}
	if len(problems) != 0 {
		return
	}
	timeout := app.Deploy.DeployTimeout
	if timeout == "" {
		timeout = defaultDeployTimeout
	}
	budget, err := time.ParseDuration(timeout)
	if err != nil || budget <= 0 {
		return // checkApp reports the invalid deploy_timeout.
	}
	if !timing.fitsBudget(budget) {
		p.add(path+".interval", "%s: uptime health must have an eligible poll before deploy_timeout", where)
	}
}

func (u UptimeTiming) fitsBudget(budget time.Duration) bool {
	if u.StartPeriod == 0 {
		return u.Interval < budget
	}
	const startupCadence = 5 * time.Second
	first := u.Threshold + uptimeMargin
	return pollFits(first, u.Interval, budget) && pollFits(first, startupCadence, budget)
}

func pollFits(first, cadence, budget time.Duration) bool {
	polls := first / cadence
	if first%cadence != 0 {
		polls++
	}
	// Divide the exclusive deadline down instead of multiplying polls, which could overflow.
	return polls <= (budget-1)/cadence
}
