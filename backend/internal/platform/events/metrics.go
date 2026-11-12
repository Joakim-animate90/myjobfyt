package events

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	eventsPublished = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "myjobfyt",
		Subsystem: "events",
		Name:      "published_total",
		Help:      "Events successfully published to JetStream.",
	}, []string{"event_type"})

	eventsPublishFailures = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "myjobfyt",
		Subsystem: "events",
		Name:      "publish_failures_total",
		Help:      "Failed attempts to publish an event.",
	}, []string{"event_type"})

	eventsProcessed = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "myjobfyt",
		Subsystem: "events",
		Name:      "processed_total",
		Help:      "Events processed by a consumer, labelled by outcome.",
	}, []string{"consumer", "event_type", "outcome"})

	eventProcessingDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "myjobfyt",
		Subsystem: "events",
		Name:      "processing_duration_seconds",
		Help:      "Wall-clock time a consumer spent handling an event.",
		// Buckets span sub-second bookkeeping through two-minute AI calls, because this
		// pipeline legitimately contains both.
		Buckets: []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120},
	}, []string{"consumer", "event_type"})

	eventsDeadLettered = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "myjobfyt",
		Subsystem: "events",
		Name:      "dead_lettered_total",
		Help:      "Events sent to a dead-letter subject after exhausting redelivery.",
	}, []string{"consumer", "event_type", "reason"})

	// eventsDeadLetterDepth is a gauge, not a counter, because the alerting question is
	// "is anything still sitting unhandled" rather than "how many ever arrived". A
	// counter cannot answer it: dead_lettered_total stays high for ever after a single
	// incident, so an alert on it either fires permanently or needs a rate window that
	// goes quiet while the messages are still there, unexamined.
	eventsDeadLetterDepth = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "myjobfyt",
		Subsystem: "events",
		Name:      "dead_letter_depth",
		Help:      "Messages currently sitting in a dead-letter stream, awaiting an operator.",
	}, []string{"stream"})

	// eventsDeadLetterDepthStale reports that the depth gauge could not be refreshed.
	// Without it, a monitor that silently stopped would leave the depth reading at its
	// last value and the alert would conclude all was well.
	eventsDeadLetterDepthStale = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "myjobfyt",
		Subsystem: "events",
		Name:      "dead_letter_depth_stale",
		Help:      "1 when the dead-letter depth could not be read, so the depth gauge is not trustworthy.",
	}, []string{"stream"})

	eventsDuplicatesDropped = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "myjobfyt",
		Subsystem: "events",
		Name:      "duplicates_dropped_total",
		Help:      "Redelivered events skipped because their idempotency key was already completed.",
	}, []string{"consumer", "event_type"})
)
