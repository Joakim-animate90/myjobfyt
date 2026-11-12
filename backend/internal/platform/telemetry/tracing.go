package telemetry

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
)

// TracingOptions configures the tracer provider.
type TracingOptions struct {
	// ServiceName distinguishes the API from the worker in a trace that spans both.
	ServiceName string
	Version     string
	Environment string
	// OTLPEndpoint is the collector's HTTP endpoint, e.g. localhost:4318. Empty
	// disables exporting.
	OTLPEndpoint string
	// SampleRatio is the fraction of root traces recorded. Child spans follow their
	// parent's decision, so a sampled workflow is never recorded in part.
	SampleRatio float64
	// Insecure sends plaintext OTLP, which is right for a collector on localhost or
	// inside a cluster and wrong across the internet.
	Insecure bool
}

// SetupTracing installs a global tracer provider and propagator, returning a shutdown
// function that flushes pending spans.
//
// Without this, otel.Tracer() hands back a no-op: every span the code creates is
// discarded, and the trace context the event envelopes carry is empty because there is
// no span context to serialise. The instrumentation reads as working and records
// nothing, so this is the difference between the pipeline being traceable and merely
// looking traceable.
//
// The propagator is registered even when exporting is disabled. It costs nothing and
// means a service that does export still joins traces started by one that does not.
func SetupTracing(ctx context.Context, opts TracingOptions, log *slog.Logger) (func(context.Context) error, error) {
	// W3C trace context plus baggage, matching what the event envelopes carry.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	if opts.OTLPEndpoint == "" {
		log.Warn("no OTLP endpoint configured; traces are not recorded",
			slog.String("hint", "set OTEL_EXPORTER_OTLP_ENDPOINT to a collector, e.g. localhost:4318"))
		return func(context.Context) error { return nil }, nil
	}

	clientOpts := []otlptracehttp.Option{otlptracehttp.WithEndpoint(opts.OTLPEndpoint)}
	if opts.Insecure {
		clientOpts = append(clientOpts, otlptracehttp.WithInsecure())
	}

	exporter, err := otlptracehttp.New(ctx, clientOpts...)
	if err != nil {
		return nil, fmt.Errorf("create OTLP trace exporter: %w", err)
	}

	identity := resource.NewWithAttributes(
		semconv.SchemaURL,
		semconv.ServiceName(opts.ServiceName),
		semconv.ServiceVersion(opts.Version),
		attribute.String("deployment.environment", opts.Environment),
	)

	// Merge requires both resources to declare the same schema URL, so the semconv
	// package above must track whatever resource.Default() uses — a dependency bump can
	// silently break that and it fails at startup, not at build time:
	//
	//	conflicting Schema URL: .../1.43.0 and .../1.26.0
	//
	// A mismatch costs the host and SDK attributes Default() contributes, which are
	// conveniences. It must not stop the service from starting or lose service.name,
	// which is what makes a trace attributable at all, so fall back to identity alone.
	res, err := resource.Merge(resource.Default(), identity)
	if err != nil {
		log.Warn("could not merge the default trace resource; continuing with service identity only",
			slog.Any("error", err))
		res = identity
	}

	ratio := opts.SampleRatio
	if ratio <= 0 {
		// Zero would record nothing while appearing configured, which is the failure
		// this whole function exists to avoid.
		ratio = 1
	}
	if ratio > 1 {
		ratio = 1
	}

	provider := sdktrace.NewTracerProvider(
		// Batched: one span export per pipeline stage on the hot path would add broker
		// latency to every CV.
		sdktrace.WithBatcher(exporter,
			sdktrace.WithBatchTimeout(5*time.Second),
			sdktrace.WithMaxExportBatchSize(512),
		),
		sdktrace.WithResource(res),
		// ParentBased so a decision made at the start of a workflow is honoured by every
		// later stage. Sampling each stage independently would produce traces missing
		// their middle, which is worse than not sampling at all: a gap looks like a
		// stage that never ran.
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(ratio))),
	)
	otel.SetTracerProvider(provider)

	log.Info("tracing enabled",
		slog.String("service", opts.ServiceName),
		slog.String("otlp_endpoint", opts.OTLPEndpoint),
		slog.Float64("sample_ratio", ratio),
	)

	return func(shutdownCtx context.Context) error {
		// Flushes whatever is batched. Without this a short-lived process, or the last
		// few seconds before a deploy, exports nothing — and those are exactly the
		// spans someone is looking for.
		return provider.Shutdown(shutdownCtx)
	}, nil
}
