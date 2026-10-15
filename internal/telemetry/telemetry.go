// Package telemetry joins the agent to the control plane's distributed traces, breaking a
// deploy into clone, build, health and routing. Off unless OTEL_EXPORTER_OTLP_ENDPOINT is set.
package telemetry

import (
	"context"
	"os"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	// Must match resource.Default()'s schema or resource.Merge fails; bump both with the SDK.
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
)

const tracerName = "github.com/ownship/agent"

var enabled bool

// Start returns a shutdown func that flushes spans; always call it, since the agent re-execs
// on self-update and the last spans matter most.
func Start(ctx context.Context, version string) (func(context.Context) error, error) {
	if os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") == "" {
		return func(context.Context) error { return nil }, nil
	}

	exporter, err := otlptracehttp.New(ctx)
	if err != nil {
		return nil, err
	}
	res, err := resource.Merge(resource.Default(), resource.NewWithAttributes(
		semconv.SchemaURL,
		semconv.ServiceName("agent"),
		semconv.ServiceVersion(version),
	))
	if err != nil {
		return nil, err
	}
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(provider)
	// Command.trace_context carries W3C tracecontext.
	otel.SetTextMapPropagator(propagation.TraceContext{})
	enabled = true

	return func(c context.Context) error {
		shutdownCtx, cancel := context.WithTimeout(c, 5*time.Second)
		defer cancel()
		return provider.Shutdown(shutdownCtx)
	}, nil
}

func Enabled() bool { return enabled }

// FromTraceparent continues the command's trace; a bad value leaves ctx unchanged.
func FromTraceparent(ctx context.Context, traceparent string) context.Context {
	if traceparent == "" {
		return ctx
	}
	return otel.GetTextMapPropagator().Extract(
		ctx,
		propagation.MapCarrier{"traceparent": traceparent},
	)
}

// Traceparent lets an event land under the trace of the command that caused it.
func Traceparent(ctx context.Context) string {
	carrier := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, carrier)
	return carrier["traceparent"]
}

// Span starts a child span; the end func records the error, so `defer end(&err)` works.
func Span(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, func(*error)) {
	ctx, span := otel.Tracer(tracerName).Start(ctx, name, trace.WithAttributes(attrs...))
	return ctx, func(errp *error) {
		if errp != nil && *errp != nil {
			span.RecordError(*errp)
			span.SetStatus(codes.Error, (*errp).Error())
		}
		span.End()
	}
}

func String(k, v string) attribute.KeyValue { return attribute.String(k, v) }
