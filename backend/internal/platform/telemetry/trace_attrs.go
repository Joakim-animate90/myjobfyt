package telemetry

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/otel/trace"
)

// spanContextAttrs returns trace correlation attributes for the active span, so a log
// line can be pivoted directly to its trace in Grafana. Returns nil when there is no
// recording span, which keeps log volume down in tests and CLI tools.
func spanContextAttrs(ctx context.Context) []slog.Attr {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return nil
	}
	return []slog.Attr{
		slog.String("trace_id", sc.TraceID().String()),
		slog.String("span_id", sc.SpanID().String()),
	}
}
