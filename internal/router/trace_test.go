package router

import (
	"context"
	"testing"

	agentv1 "github.com/ownship/agent/internal/gen/ownship/agent/v1"
	"github.com/ownship/agent/internal/telemetry"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// The wire hop that makes the agent part of the deploy's trace instead of a black
// box on the timeline. Asserted on the real propagator, not a stub.
func TestCommandTraceContextResumesTheControlPlaneTrace(t *testing.T) {
	otel.SetTracerProvider(sdktrace.NewTracerProvider())
	otel.SetTextMapPropagator(propagation.TraceContext{})

	const controlPlane = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	ctx := telemetry.FromTraceparent(context.Background(), controlPlane)

	ctx, span := otel.Tracer("test").Start(ctx, "agent.command")
	defer span.End()

	got := trace.SpanContextFromContext(ctx).TraceID().String()
	if want := "4bf92f3577b34da6a3ce929d0e0e4736"; got != want {
		t.Fatalf("agent joined trace %s, want %s", got, want)
	}

	// Events must carry the agent-side context back, or the gateway's handling
	// spans detach from the deploy they belong to.
	emitted := []*agentv1.Event{}
	emit := tracedEmit(ctx, func(e *agentv1.Event) error {
		emitted = append(emitted, e)
		return nil
	})
	if err := emit(ack("cmd-1")); err != nil {
		t.Fatalf("emit: %v", err)
	}
	if len(emitted) != 1 || emitted[0].GetTraceContext() == "" {
		t.Fatal("event left the agent without a trace context")
	}
	back := propagation.TraceContext{}.Extract(
		context.Background(),
		propagation.MapCarrier{"traceparent": emitted[0].GetTraceContext()},
	)
	if got := trace.SpanContextFromContext(back).TraceID().String(); got != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Fatalf("event carried trace %s, want the original", got)
	}
}

// An agent that never got a trace context (tracing off control-plane side, or an
// older gateway) must still work — the span becomes a root, nothing drops.
func TestMissingTraceContextIsNotAnError(t *testing.T) {
	ctx := telemetry.FromTraceparent(context.Background(), "")
	if ctx == nil {
		t.Fatal("empty traceparent must yield a usable context")
	}
	emit := tracedEmit(ctx, func(*agentv1.Event) error { return nil })
	if err := emit(ack("cmd-1")); err != nil {
		t.Fatalf("emit: %v", err)
	}
}
