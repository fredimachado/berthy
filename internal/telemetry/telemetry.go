// Package telemetry exports the agent's host and container samples, and its check states, over OpenTelemetry (OTLP over
// HTTP). The agent runs on the host, so the endpoint must be reachable from the host. The exporter reads its configuration
// from the last applied manifest, rebuilds its meter provider when that configuration changes, and keeps the outcome of its
// last export for the doctor. Its errors never include a header value.
package telemetry

import (
	"context"
	"errors"
	"maps"
	"os"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/resource"

	"berthy/internal/metrics"
	"berthy/internal/state"
)

const (
	// defaultExportInterval matches the collector's sampling interval, so each sample is exported once.
	defaultExportInterval = 10 * time.Second
	// defaultConfigInterval is how often the last applied manifest is read again.
	defaultConfigInterval = time.Minute
	exportTimeout         = 5 * time.Second
	shutdownTimeout       = 5 * time.Second
	configTimeout         = 10 * time.Second
	checkTimeout          = 2 * time.Second
	serviceName           = "berthy-agent"
	meterName             = "berthy"
)

// Exporter sends the agent's samples and check states to an OpenTelemetry backend. Config returns the endpoint and headers
// to export to, and a nil config turns the export off. Run applies the configuration and exports until its context ends.
type Exporter struct {
	// Env is the environment name, which becomes the deployment.environment.name resource attribute.
	Env string
	// Collector supplies the host and container samples. Nil leaves them out.
	Collector *metrics.Collector
	// Checks returns the stored check states. Nil leaves them out.
	Checks func(ctx context.Context) ([]state.CheckState, error)
	// Config returns the export settings of the last applied manifest. Nil, or a nil config, means export is off.
	Config func(ctx context.Context) (*Config, error)
	// Log receives operational messages. They never contain a header value. Nil discards them.
	Log func(string)

	// exportEvery and configEvery override the export and configuration intervals in tests. Zero means the defaults.
	exportEvery time.Duration
	configEvery time.Duration

	mu         sync.Mutex
	live       *pipeline // the pipeline whose results Status reports; nil while export is off
	configured bool
	target     string
	lastErr    string
	lastOK     time.Time
}

// pipeline is one meter provider built from one configuration.
type pipeline struct {
	cfg      Config
	provider *sdkmetric.MeterProvider
}

// Run applies the configuration when it starts, applies it again every minute, and exports until ctx ends. A change of
// configuration replaces the meter provider. When ctx ends, the last export is flushed for up to 5 seconds before Run
// returns.
func (e *Exporter) Run(ctx context.Context) {
	var cur *pipeline
	defer func() { e.stop(cur) }()
	cur = e.apply(ctx, cur)
	t := time.NewTicker(e.configInterval())
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			cur = e.apply(ctx, cur)
		}
	}
}

// apply makes the running pipeline match the configuration that is read now, and returns the pipeline to keep.
func (e *Exporter) apply(ctx context.Context, cur *pipeline) *pipeline {
	cfg, err := e.readConfig(ctx)
	if err != nil {
		e.stop(cur)
		e.setOff(err.Error())
		return nil
	}
	if cfg == nil {
		e.stop(cur)
		e.setOff("")
		return nil
	}
	if cur != nil && sameConfig(cur.cfg, *cfg) {
		return cur
	}
	e.stop(cur)
	next, err := e.start(ctx, *cfg)
	if err != nil {
		e.setOff(err.Error())
		return nil
	}
	return next
}

// readConfig calls Config with a deadline, so a slow store cannot hold up the exporter.
func (e *Exporter) readConfig(ctx context.Context) (*Config, error) {
	if e.Config == nil {
		return nil, nil
	}
	cctx, cancel := context.WithTimeout(ctx, configTimeout)
	defer cancel()
	return e.Config(cctx)
}

// start builds a meter provider for cfg and makes it the live one.
func (e *Exporter) start(ctx context.Context, cfg Config) (*pipeline, error) {
	metricsURL, err := MetricsURL(cfg.Endpoint)
	if err != nil {
		return nil, err
	}
	p := &pipeline{cfg: Config{Endpoint: cfg.Endpoint, Headers: maps.Clone(cfg.Headers)}}
	exporter, err := otlpmetrichttp.New(ctx,
		otlpmetrichttp.WithEndpointURL(metricsURL),
		otlpmetrichttp.WithHeaders(p.cfg.Headers),
		otlpmetrichttp.WithTimeout(exportTimeout),
	)
	if err != nil {
		return nil, errors.New("OTLP exporter could not be created")
	}
	host, _ := os.Hostname()
	res := resource.NewSchemaless(
		attribute.String("service.name", serviceName),
		attribute.String("deployment.environment.name", e.Env),
		attribute.String("host.name", host),
	)
	reader := sdkmetric.NewPeriodicReader(&reporter{
		Exporter: exporter,
		endpoint: metricsURL,
		secrets:  slices.Collect(maps.Values(p.cfg.Headers)),
		report:   func(err error) { e.report(p, err) },
	}, sdkmetric.WithInterval(e.exportInterval()), sdkmetric.WithTimeout(exportTimeout))
	p.provider = sdkmetric.NewMeterProvider(sdkmetric.WithResource(res), sdkmetric.WithReader(reader))
	if err := e.register(p.provider.Meter(meterName)); err != nil {
		_ = p.provider.Shutdown(context.Background())
		return nil, errors.New("OTLP instruments could not be registered")
	}
	e.mu.Lock()
	e.live = p
	e.configured = true
	e.target = endpointTarget(metricsURL)
	e.lastErr = ""
	e.lastOK = time.Time{}
	target := e.target
	e.mu.Unlock()
	e.log("telemetry: exporting metrics to " + target)
	return p, nil
}

// stop flushes a pipeline and shuts it down. It is first cleared as the live one, so the results of its final export are
// dropped rather than reported.
func (e *Exporter) stop(p *pipeline) {
	if p == nil {
		return
	}
	e.mu.Lock()
	if e.live == p {
		e.live = nil
		e.configured = false
		e.target = ""
		e.lastOK = time.Time{}
	}
	e.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	_ = p.provider.Shutdown(ctx)
}

// setOff records that no export runs. reason is why, or empty when the manifest names no endpoint.
func (e *Exporter) setOff(reason string) {
	e.mu.Lock()
	changed := reason != e.lastErr
	e.configured = false
	e.target = ""
	e.lastErr = reason
	e.lastOK = time.Time{}
	e.mu.Unlock()
	if reason != "" && changed {
		e.log("telemetry: " + reason)
	}
}

// report records the outcome of one export by the pipeline p. Outcomes of a pipeline that is no longer live are dropped.
func (e *Exporter) report(p *pipeline, err error) {
	var logged string
	e.mu.Lock()
	if e.live == p {
		if err == nil {
			if e.lastErr != "" {
				logged = "telemetry: export succeeded again"
			}
			e.lastErr = ""
			e.lastOK = time.Now()
		} else if msg := err.Error(); msg != e.lastErr {
			e.lastErr = msg
			logged = "telemetry: export failed: " + msg
		}
	}
	e.mu.Unlock()
	if logged != "" {
		e.log(logged)
	}
}

// Status reports whether an export is configured, the last problem recorded (empty when there is none), and when an
// export last succeeded (zero when none has since the current configuration started).
func (e *Exporter) Status() (configured bool, lastErr string, lastOK time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.configured, e.lastErr, e.lastOK
}

// Target returns scheme://host of the endpoint that is exported to, or empty while export is off.
func (e *Exporter) Target() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.target
}

func (e *Exporter) log(msg string) {
	if e.Log != nil {
		e.Log(msg)
	}
}

func (e *Exporter) exportInterval() time.Duration {
	if e.exportEvery > 0 {
		return e.exportEvery
	}
	return defaultExportInterval
}

func (e *Exporter) configInterval() time.Duration {
	if e.configEvery > 0 {
		return e.configEvery
	}
	return defaultConfigInterval
}

// sameConfig reports whether two configurations export to the same place with the same headers.
func sameConfig(a, b Config) bool {
	return a.Endpoint == b.Endpoint && maps.Equal(a.Headers, b.Headers)
}

// reporter is the SDK exporter with its outcomes passed on for Status. Its errors are made safe before it returns them,
// because the SDK hands them to the global error handler, which may print them.
type reporter struct {
	sdkmetric.Exporter
	endpoint string   // the full metrics URL, which an error may name
	secrets  []string // header values, which an error must not contain
	report   func(error)
}

func (r *reporter) Export(ctx context.Context, rm *metricdata.ResourceMetrics) error {
	err := r.Exporter.Export(ctx, rm)
	if err != nil {
		err = errors.New(redact(err.Error(), r.endpoint, r.secrets))
	}
	r.report(err)
	return err
}

// maxErrorText bounds an export error that is kept for Status or logged. A collector can answer with a long body, and the
// SDK copies that body into its error.
const maxErrorText = 300

// redact replaces the metrics URL in an error text with its scheme and host, and every header value with a marker. The
// longest values go first, so a value that contains another does not leave part of it behind. The result is one line,
// bounded in length. The values are removed before the bound is applied, so a cut never leaves part of one.
func redact(msg, metricsURL string, values []string) string {
	if metricsURL != "" {
		msg = strings.ReplaceAll(msg, metricsURL, endpointTarget(metricsURL))
	}
	sorted := slices.Clone(values)
	slices.SortFunc(sorted, func(a, b string) int { return len(b) - len(a) })
	for _, v := range sorted {
		if v != "" {
			msg = strings.ReplaceAll(msg, v, "[redacted]")
		}
	}
	msg = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, msg)
	if len(msg) > maxErrorText {
		msg = msg[:maxErrorText] + "..."
	}
	return msg
}
