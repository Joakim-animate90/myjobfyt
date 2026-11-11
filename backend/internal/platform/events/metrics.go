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

	eventsDuplicatesDropped = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "myjobfyt",
		Subsystem: "events",
		Name:      "duplicates_dropped_total",
		Help:      "Redelivered events skipped because their idempotency key was already completed.",
	}, []string{"consumer", "event_type"})
)
