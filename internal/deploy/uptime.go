package deploy

import (
	"encoding/json"
	"errors"
	"path"
	"strings"
	"time"

	"berthy/internal/engine"
	"berthy/internal/manifest"
)

const (
	errUptimeProbeSource Refusal = "uptime probe unavailable: run the installed static Berthy executable from a protected, world-executable path"
	errUptimeImageTiming Refusal = "uptime health cannot use this image's health settings: check the image healthcheck and deploy_timeout"
	errUptimeImageVolume Refusal = "uptime health cannot use an image that declares a volume overlapping /.berthy"
)

func isUptimeRefusal(err Refusal) bool {
	return err == errUptimeProbeSource || err == errUptimeImageTiming || err == errUptimeImageVolume
}

func uptimePreflightMessage(err error, fallback string) string {
	var refusal Refusal
	if errors.As(err, &refusal) && isUptimeRefusal(refusal) {
		return string(refusal)
	}
	return fallback
}

func (c *Controller) resolveProbeSource() (string, error) {
	if c.ProbeSource != nil {
		source, err := c.ProbeSource()
		if err == nil && path.IsAbs(source) && path.Clean(source) == source {
			return source, nil
		}
	}
	return "", errUptimeProbeSource
}

func (c *Controller) appProbeSource(app manifest.App) (string, error) {
	for _, role := range app.Roles {
		if role.Health != nil && role.Health.Uptime != nil {
			return c.resolveProbeSource()
		}
	}
	return "", nil
}

func uptimeCommand(duration string) []string {
	return []string{"CMD", manifest.UptimeProbeTarget, "probe", "uptime", duration}
}

func uptimeMount(source string) map[string]any {
	return map[string]any{"Type": "bind", "Source": source, "Target": manifest.UptimeProbeTarget, "ReadOnly": true}
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

func checkUptimeImage(app manifest.App, role string, img engine.Image) error {
	h := app.Roles[role].Health
	if h == nil || h.Uptime == nil {
		return nil
	}
	reserved := path.Dir(manifest.UptimeProbeTarget)
	for target := range img.Volumes {
		target = path.Clean(target)
		if target == "/" || target == reserved || strings.HasPrefix(target, reserved+"/") {
			return errUptimeImageVolume
		}
	}
	var image struct {
		Test                                          []string
		Interval, Timeout, StartPeriod, StartInterval time.Duration
		Retries                                       int
	}
	if len(img.Healthcheck) > 0 && json.Unmarshal(img.Healthcheck, &image) != nil ||
		image.Interval < 0 || image.Timeout < 0 || image.StartPeriod < 0 || image.StartInterval < 0 || image.Retries < 0 {
		return errUptimeImageTiming
	}
	timing, err := h.UptimeTiming()
	if err != nil {
		return errUptimeImageTiming
	}
	grace := timing.StartPeriod
	if grace == 0 {
		grace = image.StartPeriod
	}
	if grace == 0 {
		return nil
	}
	budget, err := deployStartupBudget(app)
	if err != nil {
		return errUptimeImageTiming
	}
	first := timing.Threshold + 100*time.Millisecond
	if grace < first {
		return errUptimeImageTiming
	}
	for _, cadence := range []time.Duration{timing.Interval, 5 * time.Second, image.StartInterval} {
		if cadence == 0 {
			continue
		}
		polls := (first-1)/cadence + 1
		if polls > (budget-1)/cadence {
			return errUptimeImageTiming
		}
	}
	return nil
}
