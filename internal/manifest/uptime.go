package manifest

import (
	"errors"
	"time"
)

// UptimeThreshold returns the required running age. Zero selects Docker health.
func (h *Health) UptimeThreshold() (time.Duration, error) {
	if h == nil || h.Uptime == nil {
		return 0, nil
	}
	threshold, err := time.ParseDuration(*h.Uptime)
	if err != nil || threshold <= 0 {
		return 0, errors.New("uptime must be a positive duration")
	}
	return threshold, nil
}

func (h Health) checkUptime(p *Problems, path, where string, app App) {
	if h.Test != nil {
		p.add(path, "%s: choose exactly one of health.test and health.uptime", where)
	}
	if app.Kind == "compose" {
		p.add(path+".uptime", "%s: uptime health is for image apps", where)
	}
	for _, option := range []struct {
		key string
		set bool
	}{
		{"interval", h.Interval != ""}, {"timeout", h.Timeout != ""},
		{"start_period", h.StartPeriod != ""}, {"start_interval", h.StartInterval != ""},
		{"retries", h.Retries != nil},
	} {
		if option.set {
			p.add(path+"."+option.key, "%s: %s is only allowed with health.test", where, option.key)
		}
	}
	threshold, err := h.UptimeThreshold()
	if err != nil {
		p.add(path+".uptime", "%s: %s", where, err)
		return
	}
	timeout := app.Deploy.DeployTimeout
	if timeout == "" {
		timeout = defaultDeployTimeout
	}
	if budget, err := time.ParseDuration(timeout); err == nil && budget > 0 && threshold >= budget {
		p.add(path+".uptime", "%s: uptime must be shorter than deploy_timeout", where)
	}
}
