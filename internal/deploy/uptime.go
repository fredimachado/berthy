package deploy

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
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

func describeUptimeBind(source string) string {
	return fmt.Sprintf("read-only bind %s -> %s", source, manifest.UptimeProbeTarget)
}

type expectedUptimeProbe struct {
	mount map[string]any
	test  []string
}

func (c *Controller) expectedProbe(role manifest.Role) (*expectedUptimeProbe, error) {
	if role.Health == nil || role.Health.Uptime == nil {
		return nil, nil
	}
	timing, err := role.Health.UptimeTiming()
	if err != nil {
		return nil, err
	}
	source, err := c.resolveProbeSource()
	if err != nil {
		return nil, err
	}
	return &expectedUptimeProbe{mount: uptimeMount(source), test: uptimeCommand(timing.Threshold.String())}, nil
}

func uptimeMountOverlap(target string) bool {
	target = path.Clean(target)
	reserved := path.Dir(manifest.UptimeProbeTarget)
	return target == "/" || target == reserved || strings.HasPrefix(target, reserved+"/")
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

func checkUptimeImage(app manifest.App, role string, img engine.Image) error {
	h := app.Roles[role].Health
	if h == nil || h.Uptime == nil {
		return nil
	}
	if !img.ConfigPresent {
		return errUptimeImageTiming
	}
	for target := range img.Volumes {
		if uptimeMountOverlap(target) {
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
	budget, err := deployStartupBudget(app)
	if err != nil || !timing.FitsBudget(budget, image.StartPeriod, image.StartInterval) {
		return errUptimeImageTiming
	}
	return nil
}
