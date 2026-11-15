package agent

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"berthy/internal/api"
	"berthy/internal/deploy"
	"berthy/internal/metrics"
	"berthy/internal/state"
	"berthy/internal/telemetry"
)

// missingHeadersSecret is why export stays off when the headers secret of the last applied manifest is not stored as a
// plain secret.
const missingHeadersSecret = "OTLP headers secret missing"

// telemetryWindow is how recent a successful export must be for the doctor to report the export ok.
const telemetryWindow = time.Minute

// NewTelemetry returns the OpenTelemetry export of an agent with state, or nil without state. The export reads its settings
// from the last applied manifest, and its samples and check states from the collector and the store. log receives
// operational messages, which never contain secrets.
func NewTelemetry(c Config, col *metrics.Collector, d *deploy.Controller, log func(string)) *telemetry.Exporter {
	if d == nil || d.Store == nil || col == nil {
		return nil
	}
	s := d.Store
	return &telemetry.Exporter{
		Env:       c.Environment,
		Collector: col,
		Checks:    s.Checks,
		Config:    func(ctx context.Context) (*telemetry.Config, error) { return telemetryConfig(ctx, s) },
		Log:       log,
	}
}

// telemetryConfig returns the export settings of the last applied manifest, or nil when it names no endpoint. The headers
// come from the secret the manifest names, which must be a plain secret. Its error never includes the secret's value.
func telemetryConfig(ctx context.Context, s *state.Store) (*telemetry.Config, error) {
	m, _, err := s.LatestManifest(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, errors.New("telemetry configuration unavailable")
	}
	if m.Telemetry == nil {
		return nil, nil
	}
	cfg := &telemetry.Config{Endpoint: m.Telemetry.OTLPEndpoint}
	if m.Telemetry.HeadersSecret == "" {
		return cfg, nil
	}
	info, value, err := s.Secret(ctx, m.Telemetry.HeadersSecret)
	if err != nil || info.Registry != "" || len(value) == 0 {
		return nil, errors.New(missingHeadersSecret)
	}
	headers, err := telemetry.ParseHeaders(string(value))
	if err != nil {
		return nil, err
	}
	cfg.Headers = headers
	return cfg, nil
}

// telemetryStatus is what the doctor reads from the export.
type telemetryStatus interface {
	Status() (configured bool, lastErr string, lastOK time.Time)
	Target() string
}

// telemetryDoctorCheck returns the doctor's telemetry check. It is not critical. The check is left out while no export is
// configured and none has failed, so an agent that exports nothing shows no telemetry line.
func telemetryDoctorCheck(t telemetryStatus, now time.Time) (api.Check, bool) {
	configured, lastErr, lastOK := t.Status()
	if !configured && lastErr == "" {
		return api.Check{}, false
	}
	c := api.Check{Name: "telemetry"}
	switch {
	case configured && !lastOK.IsZero() && now.Sub(lastOK) <= telemetryWindow:
		c.OK = true
		c.Detail = "exporting to " + t.Target()
	case lastErr != "":
		c.Detail = lastErr
	case lastOK.IsZero():
		c.Detail = "no export has succeeded yet"
	default:
		c.Detail = "last export succeeded at " + lastOK.UTC().Format(time.RFC3339)
	}
	return c, true
}
