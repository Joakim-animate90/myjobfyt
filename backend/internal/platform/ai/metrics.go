package ai

import (
	"errors"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	aiCalls = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "myjobfyt",
		Subsystem: "ai",
		Name:      "calls_total",
		Help:      "AI calls, labelled by purpose, model and outcome.",
	}, []string{"purpose", "model", "outcome"})

	aiDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "myjobfyt",
		Subsystem: "ai",
		Name:      "call_duration_seconds",
		Help:      "Wall-clock time per AI call.",
		// A thinking model on a long CV legitimately takes tens of seconds, so the
		// buckets run well past the usual web-request range.
		Buckets: []float64{0.5, 1, 2.5, 5, 10, 20, 30, 60, 90, 120},
	}, []string{"purpose", "model"})

	// Tokens are counted separately for input and output because they are priced
	// differently — an alert on total spend that cannot tell them apart is not
	// actionable.
	aiTokens = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "myjobfyt",
		Subsystem: "ai",
		Name:      "tokens_total",
		Help:      "Tokens consumed, labelled by purpose, model and kind.",
	}, []string{"purpose", "model", "kind"})

	// Cache effectiveness is worth its own metric: a silently-missing cache is the
	// normal failure mode, and it shows up only as a cost increase otherwise.
	aiCacheReads = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "myjobfyt",
		Subsystem: "ai",
		Name:      "cache_read_tokens_total",
		Help:      "Prompt tokens served from the provider's cache.",
	}, []string{"purpose", "model"})
)

// observeCall records metrics for one provider call, including failed attempts.
func observeCall(purpose Purpose, model string, err error, elapsed time.Duration, usage Usage) {
	aiCalls.WithLabelValues(string(purpose), model, outcomeLabel(err)).Inc()
	aiDuration.WithLabelValues(string(purpose), model).Observe(elapsed.Seconds())

	if usage.InputTokens > 0 {
		aiTokens.WithLabelValues(string(purpose), model, "input").Add(float64(usage.InputTokens))
	}
	if usage.OutputTokens > 0 {
		aiTokens.WithLabelValues(string(purpose), model, "output").Add(float64(usage.OutputTokens))
	}
	if usage.CacheReadTokens > 0 {
		aiCacheReads.WithLabelValues(string(purpose), model).Add(float64(usage.CacheReadTokens))
	}
}

// outcomeLabel maps an error to a bounded label value.
//
// Bounded deliberately: using the error message would produce unbounded label
// cardinality and eventually take Prometheus down, which is a well-known way to turn a
// metrics addition into an outage.
func outcomeLabel(err error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, ErrSchemaViolation):
		return "schema_violation"
	case errors.Is(err, ErrRefused):
		return "refused"
	case errors.Is(err, ErrRateLimited):
		return "rate_limited"
	case errors.Is(err, ErrTruncated):
		return "truncated"
	case errors.Is(err, ErrBudgetExceeded):
		return "budget_exceeded"
	case errors.Is(err, ErrUnavailable):
		return "unavailable"
	case errors.Is(err, ErrInvalidRequest):
		return "invalid_request"
	default:
		return "error"
	}
}
