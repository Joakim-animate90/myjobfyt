package events

import (
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// noopTracer returns a tracer that records nothing, so the delivery-protocol tests do
// not depend on a configured exporter.
func noopTracer() trace.Tracer {
	return noop.NewTracerProvider().Tracer("test")
}
