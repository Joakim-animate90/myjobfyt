package jobs

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// SourceOutcome is why a source contributed what it did to a run.
type SourceOutcome string

const (
	// OutcomeOK means the source returned results.
	OutcomeOK SourceOutcome = "ok"
	// OutcomeSkippedCapability means the source cannot serve this query, so it was not
	// asked. Distinct from a failure: nothing is wrong with the source.
	OutcomeSkippedCapability SourceOutcome = "skipped_capability"
	// OutcomeSkippedUnusable means the source is disabled, has no legal basis, or has
	// no adapter.
	OutcomeSkippedUnusable SourceOutcome = "skipped_unusable"
	// OutcomeSkippedBreakerOpen means the source is in its cooldown.
	OutcomeSkippedBreakerOpen SourceOutcome = "skipped_breaker_open"
	// OutcomeFailed means the source was asked and errored.
	OutcomeFailed SourceOutcome = "failed"
	// OutcomePartial means some pages succeeded and then one failed. The postings
	// already retrieved are kept: discarding them would waste the source's rate limit
	// and leave the candidate with fewer matches for no benefit.
	OutcomePartial SourceOutcome = "partial"
)

// SourceResult is one source's contribution to a run.
type SourceResult struct {
	Slug     string
	Outcome  SourceOutcome
	Postings []RawPosting
	// Reason explains a skip or a failure, for the run record and for an operator.
	Reason string
	// Cursor is where to resume. Persisted so a crash resumes rather than restarting
	// and re-spending the source's rate limit.
	Cursor string
	// Pages counts the requests actually made, for cost and rate accounting.
	Pages int
	// Breaker is the source's state after this attempt, for the caller to persist.
	Breaker Breaker
	// BreakerOpened is true when this attempt is what opened the circuit, so the caller
	// knows to alert once rather than on every subsequent skip.
	BreakerOpened bool
}

// Executor runs one source's search, applying capabilities, the breaker and the
// adapter's own rate limit.
//
// This is where "a failing source degrades a run and never fails it" is implemented.
// Nothing here returns an error for a source problem: every outcome is reported in the
// result, so a caller iterating sources cannot accidentally abort the run by propagating
// one source's failure.
type Executor struct {
	cfg BreakerConfig
	log *slog.Logger
	now func() time.Time
	// maxPages bounds one source in one run, so a source with a broken cursor that
	// always returns a next page cannot loop for ever.
	maxPages int
}

// ExecutorConfig configures the executor.
type ExecutorConfig struct {
	Breaker BreakerConfig
	// MaxPages bounds one source per run. Defaults to 20.
	MaxPages int
	// Now is injectable so breaker timing can be tested without sleeping, and so every
	// source in one run is evaluated against a single consistent instant rather than a
	// clock that advances mid-run.
	Now func() time.Time
}

// NewExecutor builds the executor.
func NewExecutor(cfg ExecutorConfig, log *slog.Logger) *Executor {
	maxPages := cfg.MaxPages
	if maxPages <= 0 {
		maxPages = 20
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &Executor{
		cfg:      cfg.Breaker.withDefaults(),
		log:      log,
		now:      now,
		maxPages: maxPages,
	}
}

// Search collects postings from one source.
//
// Returns no error, ever. A source problem is an outcome, not an error, because the
// caller's job is to run every source and the easiest way to break that is for one
// source's failure to look like the run's failure.
func (e *Executor) Search(ctx context.Context, source Source, spec SearchSpec, cursor string) SourceResult {
	now := e.now()
	result := SourceResult{Slug: source.Slug, Cursor: cursor, Breaker: source.Breaker}

	if ok, reason := source.Usable(); !ok {
		result.Outcome, result.Reason = OutcomeSkippedUnusable, reason
		return result
	}

	if !source.Breaker.Allows(now, e.cfg) {
		result.Outcome = OutcomeSkippedBreakerOpen
		result.Reason = "the circuit is open for another " +
			source.Breaker.Cooldown(now).Round(time.Second).String()
		return result
	}

	adapter := source.Adapter()
	if ok, reason := adapter.Capabilities().Supports(spec); !ok {
		// Not asked rather than asked and failed. A source that silently ignores an
		// unsupported filter returns everything, and the run looks fine while the
		// results are wrong.
		result.Outcome, result.Reason = OutcomeSkippedCapability, reason
		return result
	}

	log := e.log.With(slog.String("source", source.Slug))
	caps := adapter.Capabilities()

	for result.Pages < e.maxPages {
		if err := ctx.Err(); err != nil {
			// Shutdown, not a source fault. The cursor is already recorded, so the run
			// resumes here; counting this against the breaker would penalise a healthy
			// source for our deploy.
			result.Reason = "run cancelled"
			if len(result.Postings) > 0 {
				result.Outcome = OutcomePartial
			} else {
				result.Outcome = OutcomeSkippedUnusable
			}
			return result
		}

		page, err := adapter.Search(ctx, spec, result.Cursor)
		result.Pages++

		if err != nil {
			return e.recordFailure(result, now, err, log)
		}

		result.Postings = append(result.Postings, page.Postings...)
		result.Cursor = page.NextCursor

		if page.NextCursor == "" || !caps.Pagination {
			break
		}
		if spec.Limit > 0 && len(result.Postings) >= spec.Limit {
			// The caller asked for a bounded number; stop rather than spending the
			// source's rate limit on results nobody wanted.
			break
		}
		if len(page.Postings) == 0 {
			// A next cursor with no results is a source bug. Following it is how a
			// discovery run spins.
			log.WarnContext(ctx, "source returned a next cursor with no postings; stopping",
				slog.Int("pages", result.Pages))
			break
		}
	}

	result.Outcome = OutcomeOK
	result.Breaker = source.Breaker.RecordSuccess(now)
	return result
}

// recordFailure turns an adapter error into a degraded outcome.
func (e *Executor) recordFailure(result SourceResult, now time.Time, cause error, log *slog.Logger) SourceResult {
	wasOpen := result.Breaker.OpenUntil != nil

	var limited *ErrRateLimited
	if errors.As(cause, &limited) {
		// Our own pacing was wrong, or the source tightened its limit. Counted towards
		// the breaker — backing off is exactly the right response — but logged
		// distinctly, because the fix is a configuration change rather than waiting for
		// a third party to recover.
		log.Warn("source rate-limited us; backing off",
			slog.Duration("retry_after", limited.RetryAfter))
	}

	result.Breaker = result.Breaker.RecordFailure(now, e.cfg, cause)
	result.Reason = result.Breaker.LastError
	result.BreakerOpened = !wasOpen && result.Breaker.OpenUntil != nil

	if len(result.Postings) > 0 {
		// Keep what was already retrieved. Discarding it would waste the source's rate
		// limit and leave the candidate with fewer matches for no benefit.
		result.Outcome = OutcomePartial
	} else {
		result.Outcome = OutcomeFailed
	}

	if result.BreakerOpened {
		log.Error("opening the circuit for this source",
			slog.Int("consecutive_failures", result.Breaker.ConsecutiveFailures),
			slog.Duration("cooldown", result.Breaker.Cooldown(now)),
			slog.String("last_error", result.Breaker.LastError),
		)
	} else {
		log.Warn("source failed; degrading the run",
			slog.Int("consecutive_failures", result.Breaker.ConsecutiveFailures),
			slog.String("error", result.Breaker.LastError),
		)
	}
	return result
}

// Probe runs a half-open health check.
//
// Used before restoring a source to full traffic: a single search succeeding is weaker
// evidence than the source's own health endpoint agreeing, and restoring a whole run's
// traffic to a source that is still broken is how the circuit ends up flapping.
func (e *Executor) Probe(ctx context.Context, source Source) (Breaker, bool) {
	now := e.now()
	if source.Adapter() == nil {
		return source.Breaker, false
	}
	if source.Breaker.StateAt(now, e.cfg) == BreakerOpen {
		return source.Breaker, false
	}

	if err := source.Adapter().HealthCheck(ctx); err != nil {
		return source.Breaker.RecordFailure(now, e.cfg, err), false
	}
	return source.Breaker.RecordSuccess(now), true
}
