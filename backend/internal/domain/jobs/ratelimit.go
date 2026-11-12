package jobs

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Limiter paces requests to one source.
//
// Deliberately an interface the adapter holds rather than something the scheduler
// applies. MJF-020 requires rate limiting enforced inside the adapter, because a
// scheduling bug then cannot cause abuse: the limit is on the only code path that can
// make a request, not on the code that is supposed to remember to ask.
type Limiter interface {
	// Wait blocks until a request may be made, or returns ctx.Err().
	Wait(ctx context.Context) error
}

// TokenLimiter paces requests to a fixed rate.
//
// A leaky bucket with a burst of one: requests are spaced evenly rather than allowed in
// a clump at the top of each minute. Sources that advertise "60 per minute" frequently
// mean "not more than one per second", and a burst of sixty is how an integration gets
// a 429 and then a ban.
type TokenLimiter struct {
	mu       sync.Mutex
	interval time.Duration
	next     time.Time
	// now is injectable so the spacing can be tested without sleeping.
	now   func() time.Time
	sleep func(context.Context, time.Duration) error
}

// NewTokenLimiter builds a limiter for a per-minute rate.
func NewTokenLimiter(perMinute int) *TokenLimiter {
	if perMinute <= 0 {
		// A zero or negative rate must not mean "unlimited". Defaulting to something
		// conservative is the safe reading of a misconfiguration.
		perMinute = 30
	}
	return &TokenLimiter{
		interval: time.Minute / time.Duration(perMinute),
		now:      time.Now,
		sleep:    sleepCtx,
	}
}

// NewIntervalLimiter builds a limiter from a minimum spacing between requests.
//
// Needed because some sources ask for a rate slower than one per minute: Remotive's own
// terms advise at most four requests a day and say excessive requests will be blocked,
// which a per-minute figure cannot express.
func NewIntervalLimiter(interval time.Duration) *TokenLimiter {
	if interval <= 0 {
		interval = time.Minute / 30
	}
	return &TokenLimiter{interval: interval, now: time.Now, sleep: sleepCtx}
}

// Wait blocks until the next request is due.
func (l *TokenLimiter) Wait(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	l.mu.Lock()
	now := l.now()
	wait := time.Duration(0)
	if now.Before(l.next) {
		wait = l.next.Sub(now)
	}
	// Reserve this slot before unlocking, so concurrent callers queue behind each other
	// rather than all measuring the same gap and firing together.
	l.next = now.Add(wait + l.interval)
	l.mu.Unlock()

	if wait <= 0 {
		return nil
	}
	return l.sleep(ctx, wait)
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// Interval is the spacing between permitted requests.
func (l *TokenLimiter) Interval() time.Duration { return l.interval }

// unlimited is used only where a source genuinely has no limit, which in practice is
// never: it exists so a test can opt out explicitly rather than by passing a huge number.
type unlimited struct{}

func (unlimited) Wait(context.Context) error { return nil }

// Unlimited returns a limiter that never waits.
//
// For tests and for in-process sources. Using it for a real HTTP source is a mistake
// this comment exists to discourage.
func Unlimited() Limiter { return unlimited{} }

// ErrRateLimited reports that a source rejected a request for rate reasons.
//
// Distinguished from other failures because the response is different: back off and
// retry later rather than counting it towards the breaker as a fault of the source.
type ErrRateLimited struct {
	Slug string
	// RetryAfter is the source's own advice where it gave any.
	RetryAfter time.Duration
}

func (e *ErrRateLimited) Error() string {
	if e.RetryAfter > 0 {
		return fmt.Sprintf("%s rate-limited this request; retry after %s", e.Slug, e.RetryAfter)
	}
	return fmt.Sprintf("%s rate-limited this request", e.Slug)
}
