package events

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	apitrace "go.opentelemetry.io/otel/trace"
)

// tracerFor returns a tracer backed by an in-memory exporter, plus the recorder.
func tracerFor(t *testing.T) (apitrace.Tracer, *tracetest.SpanRecorder) {
	t.Helper()
	recorder := tracetest.NewSpanRecorder()
	provider := trace.NewTracerProvider(trace.WithSpanProcessor(recorder))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	return provider.Tracer("test"), recorder
}

func TestTraceContextSurvivesTheBroker(t *testing.T) {
	// OpenTelemetry context does not cross a message broker by itself. Without the
	// envelope's Trace field every asynchronous hop would start a fresh trace, and the
	// pipeline would appear as eight unrelated traces rather than one workflow.
	tracer, _ := tracerFor(t)

	publishCtx, publishSpan := tracer.Start(context.Background(), "publish")
	headers := traceHeadersFromContext(publishCtx)
	publishSpan.End()

	if len(headers) == 0 {
		t.Fatal("no trace headers were produced, so the trace cannot cross the broker")
	}
	if _, ok := headers["traceparent"]; !ok {
		t.Errorf("no W3C traceparent header: %v", headers)
	}

	// The consumer side, starting from a context with no span of its own.
	consumeCtx := ContextWithTrace(context.Background(), headers)
	restored := apitrace.SpanContextFromContext(consumeCtx)

	if !restored.IsValid() {
		t.Fatal("the restored span context is invalid")
	}
	published := publishSpan.SpanContext()
	if restored.TraceID() != published.TraceID() {
		t.Errorf("trace id = %s, want %s; the consumer would start a disconnected trace",
			restored.TraceID(), published.TraceID())
	}
	if restored.SpanID() != published.SpanID() {
		t.Errorf("parent span id = %s, want the publishing span %s",
			restored.SpanID(), published.SpanID())
	}
}

func TestAConsumerSpanIsAChildOfThePublishingSpan(t *testing.T) {
	tracer, recorder := tracerFor(t)

	publishCtx, publishSpan := tracer.Start(context.Background(), "publish cv.uploaded")
	headers := traceHeadersFromContext(publishCtx)
	publishSpan.End()

	consumeCtx := ContextWithTrace(context.Background(), headers)
	_, consumeSpan := tracer.Start(consumeCtx, "consume cv.uploaded")
	consumeSpan.End()

	spans := recorder.Ended()
	if len(spans) != 2 {
		t.Fatalf("recorded %d spans, want 2", len(spans))
	}

	var consume trace.ReadOnlySpan
	for _, s := range spans {
		if s.Name() == "consume cv.uploaded" {
			consume = s
		}
	}
	if consume == nil {
		t.Fatal("the consumer span was not recorded")
	}
	if consume.Parent().SpanID() != publishSpan.SpanContext().SpanID() {
		t.Errorf("consumer parent = %s, want the publishing span %s",
			consume.Parent().SpanID(), publishSpan.SpanContext().SpanID())
	}
	if consume.SpanContext().TraceID() != publishSpan.SpanContext().TraceID() {
		t.Error("the consumer span is in a different trace from the publisher")
	}
}

func TestDerivedMetaDoesNotCarryTheIncomingTraceForward(t *testing.T) {
	// The incoming headers name the span that published THIS event. Copying them into
	// the next event made every pipeline stage a direct child of that one span, so the
	// chain appeared as a flat fan of siblings under an already-ended span and both the
	// stage order and the time between stages were lost. PublishWithMeta fills Trace
	// from the live context instead, where the active span is the running consumer.
	env := &Envelope{
		EventID:       "01ABCDEF",
		Type:          TypeCVUploaded,
		CorrelationID: "corr-1",
		UserID:        "user-1",
		Trace:         map[string]string{"traceparent": "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"},
	}

	meta := env.DerivedMeta()

	if len(meta.Trace) != 0 {
		t.Errorf("DerivedMeta carried the incoming trace forward: %v; stages would be "+
			"siblings under the publishing span rather than a chain", meta.Trace)
	}

	// The correlation chain must still be carried: it is what makes the whole workflow
	// queryable, and causation is what records which event triggered this one.
	if meta.CorrelationID != "corr-1" {
		t.Errorf("CorrelationID = %q, want it carried forward", meta.CorrelationID)
	}
	if meta.CausationID != "01ABCDEF" {
		t.Errorf("CausationID = %q, want the parent event's id", meta.CausationID)
	}
	if meta.UserID != "user-1" {
		t.Errorf("UserID = %q, want it carried forward", meta.UserID)
	}
}

func TestAChainOfStagesNestsRatherThanFlattening(t *testing.T) {
	// The shape MJF-007 asks for: the whole chain as one trace, with each stage a child
	// of the stage that caused it, so an operator can see the order and the gaps.
	tracer, recorder := tracerFor(t)

	// The HTTP request that accepted the upload.
	requestCtx, requestSpan := tracer.Start(context.Background(), "POST /cv")
	uploaded := traceHeadersFromContext(requestCtx)
	requestSpan.End()

	// Stage one consumes cv.uploaded and publishes cv.scanned.
	scanCtx, scanSpan := tracer.Start(ContextWithTrace(context.Background(), uploaded),
		"consume cv.uploaded")
	scanned := traceHeadersFromContext(scanCtx) // from the live context, as the fix does
	scanSpan.End()

	// Stage two consumes cv.scanned.
	_, extractSpan := tracer.Start(ContextWithTrace(context.Background(), scanned),
		"consume cv.scanned")
	extractSpan.End()

	byName := map[string]trace.ReadOnlySpan{}
	for _, s := range recorder.Ended() {
		byName[s.Name()] = s
	}

	request, scan, extract := byName["POST /cv"], byName["consume cv.uploaded"], byName["consume cv.scanned"]
	if request == nil || scan == nil || extract == nil {
		t.Fatalf("missing spans: %v", byName)
	}

	traceID := request.SpanContext().TraceID()
	for name, s := range byName {
		if s.SpanContext().TraceID() != traceID {
			t.Errorf("%s is in a different trace", name)
		}
	}

	if scan.Parent().SpanID() != request.SpanContext().SpanID() {
		t.Error("the first stage is not a child of the request")
	}
	if extract.Parent().SpanID() != scan.SpanContext().SpanID() {
		t.Errorf("the second stage's parent is %s, want the first stage %s; the chain is "+
			"flattened and the stage order is invisible",
			extract.Parent().SpanID(), scan.SpanContext().SpanID())
	}
	// Specifically not a sibling of the first stage.
	if extract.Parent().SpanID() == request.SpanContext().SpanID() {
		t.Error("the second stage hangs off the request rather than the stage before it")
	}
}

func TestMissingTraceHeadersStartAFreshTraceRatherThanFailing(t *testing.T) {
	// An event published before tracing was enabled, or by a producer with no active
	// span, must still be consumable.
	if got := ContextWithTrace(context.Background(), nil); got == nil {
		t.Fatal("ContextWithTrace returned a nil context for nil headers")
	}
	if got := ContextWithTrace(context.Background(), map[string]string{}); got == nil {
		t.Fatal("ContextWithTrace returned a nil context for empty headers")
	}
	if headers := traceHeadersFromContext(context.Background()); headers != nil {
		t.Errorf("headers = %v for a context with no span, want nil", headers)
	}
}
