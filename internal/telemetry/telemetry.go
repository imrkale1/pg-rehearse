// Package telemetry wires pg-rehearse to OpenTelemetry. The orchestrator emits
// spans for each phase of a rehearsal and metrics for the numbers that matter
// (migration duration, worst stall, verdict), exported over OTLP so they land
// in the Datadog Agent, an OpenTelemetry Collector, Jaeger, or any OTLP backend.
//
// Export is opt-in: Setup installs exporters only when OTEL_EXPORTER_OTLP_ENDPOINT
// is set. With it unset, the global providers stay the SDK's no-op defaults, so
// a normal CLI run (and the test suite) needs no collector and pays no cost.
package telemetry

import (
	"context"
	"os"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
)

// ScopeName is the instrumentation scope for pg-rehearse's spans and metrics.
const ScopeName = "github.com/pg-rehearse/pg-rehearse"

// Enabled reports whether an OTLP endpoint is configured.
func Enabled() bool { return os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != "" }

// Setup installs global OpenTelemetry trace and metric providers.
//
// When OTEL_EXPORTER_OTLP_ENDPOINT is set it exports over OTLP/HTTP (the
// otlptracehttp/otlpmetrichttp exporters read the standard OTEL_* environment
// variables, so endpoint, headers, and TLS are configured the usual way). When
// it is unset, Setup is a no-op and returns a no-op shutdown func. The returned
// func flushes and shuts the providers down; call it before the process exits.
func Setup(ctx context.Context, serviceName string) (func(context.Context) error, error) {
	noop := func(context.Context) error { return nil }
	if !Enabled() {
		return noop, nil
	}
	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceName(serviceName),
			semconv.ServiceVersion(version()),
		),
		resource.WithFromEnv(), // OTEL_RESOURCE_ATTRIBUTES, incl. Datadog's env/service/version
	)
	if err != nil {
		return noop, err
	}

	traceExp, err := otlptracehttp.New(ctx)
	if err != nil {
		return noop, err
	}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(traceExp),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)

	metricExp, err := otlpmetrichttp.New(ctx)
	if err != nil {
		_ = tp.Shutdown(ctx)
		return noop, err
	}
	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExp, sdkmetric.WithInterval(10*time.Second))),
		sdkmetric.WithResource(res),
	)
	otel.SetMeterProvider(mp)

	return func(c context.Context) error {
		err := mp.Shutdown(c)
		if terr := tp.Shutdown(c); err == nil {
			err = terr
		}
		return err
	}, nil
}

func version() string {
	if v := os.Getenv("PGR_VERSION"); v != "" {
		return v
	}
	return "dev"
}

// Tracer returns pg-rehearse's tracer from the global provider (a no-op tracer
// until Setup installs a real one).
func Tracer() trace.Tracer { return otel.Tracer(ScopeName) }

// Metrics holds the instruments the orchestrator records. Build once with
// NewMetrics and reuse; a nil *Metrics is safe and records nothing.
type Metrics struct {
	migrationMs metric.Float64Histogram
	maxStallMs  metric.Float64Histogram
	verdicts    metric.Int64Counter
}

// NewMetrics creates the pg-rehearse instruments. Instrument creation never
// fails fatally: on error the instrument is left nil and its Record is skipped.
func NewMetrics() *Metrics {
	m := otel.Meter(ScopeName)
	mig, _ := m.Float64Histogram("pgr.migration.duration",
		metric.WithUnit("ms"),
		metric.WithDescription("Wall-clock time to apply a migration under load"))
	stall, _ := m.Float64Histogram("pgr.rehearsal.max_stall",
		metric.WithUnit("ms"),
		metric.WithDescription("Longest any feature was stalled while the migration ran"))
	verdicts, _ := m.Int64Counter("pgr.rehearsal.verdicts",
		metric.WithDescription("Count of rehearsal verdicts by severity and phase"))
	return &Metrics{migrationMs: mig, maxStallMs: stall, verdicts: verdicts}
}

// RecordRehearsal records one rehearsal pass. phase is "original" or "fixed".
func (m *Metrics) RecordRehearsal(ctx context.Context, phase, verdict string, migrationMs, maxStallMs float64) {
	if m == nil {
		return
	}
	attrs := metric.WithAttributes(
		attribute.String("phase", phase),
		attribute.String("verdict", verdict),
	)
	if m.migrationMs != nil {
		m.migrationMs.Record(ctx, migrationMs, metric.WithAttributes(attribute.String("phase", phase)))
	}
	if m.maxStallMs != nil {
		m.maxStallMs.Record(ctx, maxStallMs, metric.WithAttributes(attribute.String("phase", phase)))
	}
	if m.verdicts != nil {
		m.verdicts.Add(ctx, 1, attrs)
	}
}

// StartSpan starts a child span named name under ctx, with optional string
// attributes given as key/value pairs.
func StartSpan(ctx context.Context, name string, kv ...string) (context.Context, trace.Span) {
	var attrs []attribute.KeyValue
	for i := 0; i+1 < len(kv); i += 2 {
		attrs = append(attrs, attribute.String(kv[i], kv[i+1]))
	}
	return Tracer().Start(ctx, name, trace.WithAttributes(attrs...))
}
