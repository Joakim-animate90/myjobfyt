package events

import (
	"context"

	"go.opentelemetry.io/otel/propagation"
)

// propagator carries W3C trace context across the broker.
var propagator = propagation.NewCompositeTextMapPropagator(
	propagation.TraceContext{},
	propagation.Baggage{},
)

// traceHeadersFromContext serialises the active span context into a header map for the
// envelope's Trace field.
func traceHeadersFromContext(ctx context.Context) map[string]string {
	carrier := propagation.MapCarrier{}
	propagator.Inject(ctx, carrier)
	if len(carrier) == 0 {
		return nil
	}
	return carrier
}

// ContextWithTrace restores trace context from an envelope's Trace field, so a
// consumer's spans become children of the publishing request's span rather than
// starting a disconnected trace.
func ContextWithTrace(ctx context.Context, headers map[string]string) context.Context {
	if len(headers) == 0 {
		return ctx
	}
	return propagator.Extract(ctx, propagation.MapCarrier(headers))
}
