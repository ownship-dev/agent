package telemetry

import (
	"context"
	"testing"
)

// Guards against a semconv/resource.Default() schema mismatch, which makes resource.Merge
// fail and tracing never start.
func TestStartBuildsAResourceWithoutSchemaConflict(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:59999")
	shutdown, err := Start(context.Background(), "test")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !Enabled() {
		t.Fatal("telemetry should report enabled once an endpoint is set")
	}
	// Shutdown flushes to an unreachable collector; that must not be fatal here.
	_ = shutdown(context.Background())
}

// The default: no endpoint means no SDK at all, and a shutdown that is a no-op.
func TestStartIsOffWithoutAnEndpoint(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	shutdown, err := Start(context.Background(), "test")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("no-op shutdown returned %v", err)
	}
}
