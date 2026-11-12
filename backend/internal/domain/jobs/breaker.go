package jobs

import (
	"fmt"
	"time"
)

// BreakerState is a source's circuit state.
type BreakerState string

const (
	// BreakerClosed means requests flow normally.
	BreakerClosed BreakerState = "closed"
	// BreakerOpen means the source is being left alone until its cooldown elapses.
	BreakerOpen BreakerState = "open"
	// BreakerHalfOpen means the cooldown has elapsed and one probe is allowed through.
	// Distinct from closed because a single success must not immediately restore full
	// traffic to a source that is still broken.
	BreakerHalfOpen BreakerState = "half_open"
)

// BreakerConfig tunes the breaker.
type BreakerConfig struct {
	// FailureThreshold is the number of consecutive failures that opens the circuit.
	FailureThreshold int
	// BaseCooldown is the first open period. Subsequent opens back off from it.
	BaseCooldown time.Duration
	// MaxCooldown caps the backoff, so a source that has been down for a week is still
	// retried daily rather than never.
	MaxCooldown time.Duration
}

// DefaultBreakerConfig is the policy for a job source.
//
// Five failures rather than one or two: job APIs return transient 502s routinely, and
// opening on the first would take a healthy source out of a run for no reason. The
// cooldown doubles per open so a genuinely dead source stops being polled, and caps at
// an hour so a recovered one is noticed within the hour without anyone intervening.
func DefaultBreakerConfig() BreakerConfig {
	return BreakerConfig{
		FailureThreshold: 5,
		BaseCooldown:     2 * time.Minute,
		MaxCooldown:      time.Hour,
	}
}

func (c BreakerConfig) withDefaults() BreakerConfig {
	if c.FailureThreshold <= 0 {
		c.FailureThreshold = DefaultBreakerConfig().FailureThreshold
	}
	if c.BaseCooldown <= 0 {
		c.BaseCooldown = DefaultBreakerConfig().BaseCooldown
	}
	if c.MaxCooldown < c.BaseCooldown {
		c.MaxCooldown = c.BaseCooldown
	}
	return c
}

// Breaker is a source's persisted circuit-breaker state.
//
// The fields map onto job_sources columns rather than living in memory, because a
// restart is exactly when a failing source would otherwise be hammered: a process that
// forgets the circuit was open sends a full run's traffic at a source that was already
// returning errors, which is how a rate-limit ban starts.
type Breaker struct {
	ConsecutiveFailures int
	OpenUntil           *time.Time
	LastSuccessAt       *time.Time
	LastError           string
}

// StateAt reports the circuit's state at a given time.
//
// Takes the time rather than reading the clock, so the transitions are testable without
// sleeping and so a run evaluates every source against one consistent instant.
func (b Breaker) StateAt(now time.Time, cfg BreakerConfig) BreakerState {
	if b.OpenUntil == nil {
		return BreakerClosed
	}
	if now.Before(*b.OpenUntil) {
		return BreakerOpen
	}
	// The cooldown has elapsed but nothing has succeeded yet, so one probe is allowed
	// and full traffic is not.
	return BreakerHalfOpen
}

// Allows reports whether a request may be made.
func (b Breaker) Allows(now time.Time, cfg BreakerConfig) bool {
	return b.StateAt(now, cfg) != BreakerOpen
}

// RecordSuccess closes the circuit.
//
// Clears the failure count rather than decrementing it. A source that fails four times
// and succeeds once is healthy; carrying the four forward would open the circuit on a
// later unrelated blip.
func (b Breaker) RecordSuccess(now time.Time) Breaker {
	return Breaker{
		ConsecutiveFailures: 0,
		OpenUntil:           nil,
		LastSuccessAt:       &now,
		LastError:           "",
	}
}

// RecordFailure counts a failure and opens the circuit once the threshold is reached.
//
// The cooldown doubles with each threshold multiple, so a source failing persistently is
// polled progressively less. Capped, so recovery is always noticed eventually.
func (b Breaker) RecordFailure(now time.Time, cfg BreakerConfig, cause error) Breaker {
	cfg = cfg.withDefaults()

	next := Breaker{
		ConsecutiveFailures: b.ConsecutiveFailures + 1,
		LastSuccessAt:       b.LastSuccessAt,
		LastError:           truncateError(cause),
	}

	if next.ConsecutiveFailures < cfg.FailureThreshold {
		// Still closed. Keeping OpenUntil nil matters: a half-open probe that fails
		// must re-open rather than leave a stale past timestamp that reads as closed.
		return next
	}

	// How many times the threshold has been crossed, so the backoff grows with
	// persistence rather than with every individual failure.
	opens := next.ConsecutiveFailures / cfg.FailureThreshold
	cooldown := cfg.BaseCooldown
	for i := 1; i < opens && cooldown < cfg.MaxCooldown; i++ {
		cooldown *= 2
	}
	if cooldown > cfg.MaxCooldown {
		cooldown = cfg.MaxCooldown
	}

	until := now.Add(cooldown)
	next.OpenUntil = &until
	return next
}

// Cooldown reports how long until the circuit allows traffic again. Zero when it already
// does.
func (b Breaker) Cooldown(now time.Time) time.Duration {
	if b.OpenUntil == nil || !now.Before(*b.OpenUntil) {
		return 0
	}
	return b.OpenUntil.Sub(now)
}

// maxErrorLength bounds what is stored. last_error is operator-facing and the text comes
// from a third party's response body, which can be a whole HTML error page.
const maxErrorLength = 500

func truncateError(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	if len(msg) <= maxErrorLength {
		return msg
	}
	return msg[:maxErrorLength] + fmt.Sprintf("… (%d bytes truncated)", len(msg)-maxErrorLength)
}
