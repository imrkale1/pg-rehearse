package telemetry

import (
	"context"
	"testing"
)

func TestSetupNoopWhenUnset(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	if Enabled() {
		t.Fatal("Enabled() should be false with no endpoint")
	}
	shutdown, err := Setup(context.Background(), "pg-rehearse")
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if shutdown == nil {
		t.Fatal("shutdown func must never be nil")
	}
	if err := shutdown(context.Background()); err != nil {
		t.Errorf("no-op shutdown: %v", err)
	}
}

func TestEnabledReadsEnv(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://localhost:4318")
	if !Enabled() {
		t.Error("Enabled() should be true when the endpoint is set")
	}
}

func TestNilMetricsAndNoopRecord(t *testing.T) {
	var m *Metrics
	m.RecordRehearsal(context.Background(), "original", "safe", 10, 0) // must not panic

	// With no provider installed, NewMetrics returns usable no-op instruments.
	m = NewMetrics()
	if m == nil {
		t.Fatal("NewMetrics returned nil")
	}
	m.RecordRehearsal(context.Background(), "fixed", "risky", 42, 100)
}

func TestStartSpanPairsAttributes(t *testing.T) {
	// Odd trailing key is ignored rather than panicking.
	ctx, span := StartSpan(context.Background(), "t", "phase", "original", "dangling")
	if ctx == nil || span == nil {
		t.Fatal("StartSpan returned nil")
	}
	span.End()
}
