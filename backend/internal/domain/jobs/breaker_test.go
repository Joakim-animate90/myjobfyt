package jobs_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Joakim-animate90/myjobfyt/backend/internal/domain/jobs"
)

var epoch = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

func testBreakerConfig() jobs.BreakerConfig {
	return jobs.BreakerConfig{
		FailureThreshold: 3,
		BaseCooldown:     time.Minute,
		MaxCooldown:      10 * time.Minute,
	}
}

func TestAFreshBreakerIsClosed(t *testing.T) {
	var b jobs.Breaker
	if got := b.StateAt(epoch, testBreakerConfig()); got != jobs.BreakerClosed {
		t.Errorf("state = %s, want closed", got)
	}
	if !b.Allows(epoch, testBreakerConfig()) {
		t.Error("a fresh breaker refused a request")
	}
}

func TestFailuresBelowTheThresholdLeaveItClosed(t *testing.T) {
	// Job APIs return transient 502s routinely. Opening on the first would take a
	// healthy source out of a run for no reason.
	cfg := testBreakerConfig()
	b := jobs.Breaker{}

	for i := 1; i < cfg.FailureThreshold; i++ {
		b = b.RecordFailure(epoch, cfg, errors.New("502 bad gateway"))
		if !b.Allows(epoch, cfg) {
			t.Fatalf("the circuit opened after %d failure(s), threshold is %d",
				i, cfg.FailureThreshold)
		}
	}
	if b.ConsecutiveFailures != cfg.FailureThreshold-1 {
		t.Errorf("consecutive failures = %d, want %d", b.ConsecutiveFailures, cfg.FailureThreshold-1)
	}
}

func TestTheThresholdOpensTheCircuit(t *testing.T) {
	cfg := testBreakerConfig()
	b := jobs.Breaker{}
	for i := 0; i < cfg.FailureThreshold; i++ {
		b = b.RecordFailure(epoch, cfg, errors.New("503"))
	}

	if got := b.StateAt(epoch, cfg); got != jobs.BreakerOpen {
		t.Fatalf("state = %s, want open", got)
	}
	if b.Allows(epoch, cfg) {
		t.Error("an open circuit allowed a request")
	}
	if b.OpenUntil == nil {
		t.Fatal("OpenUntil is nil on an open circuit, so nothing persists across restart")
	}
	if got := b.Cooldown(epoch); got != cfg.BaseCooldown {
		t.Errorf("cooldown = %s, want %s", got, cfg.BaseCooldown)
	}
}

func TestTheCircuitGoesHalfOpenWhenTheCooldownElapses(t *testing.T) {
	// Half-open rather than closed, because one success must not immediately restore a
	// whole run's traffic to a source that is still broken.
	cfg := testBreakerConfig()
	b := jobs.Breaker{}
	for i := 0; i < cfg.FailureThreshold; i++ {
		b = b.RecordFailure(epoch, cfg, errors.New("503"))
	}

	later := epoch.Add(cfg.BaseCooldown + time.Second)
	if got := b.StateAt(later, cfg); got != jobs.BreakerHalfOpen {
		t.Errorf("state after the cooldown = %s, want half_open", got)
	}
	if !b.Allows(later, cfg) {
		t.Error("a half-open circuit refused the probe")
	}
	if got := b.Cooldown(later); got != 0 {
		t.Errorf("cooldown = %s after it elapsed, want 0", got)
	}
}

func TestASuccessClosesTheCircuitAndClearsTheCount(t *testing.T) {
	// Cleared rather than decremented: a source that fails twice and succeeds once is
	// healthy, and carrying failures forward would open the circuit on a later
	// unrelated blip.
	cfg := testBreakerConfig()
	b := jobs.Breaker{}
	b = b.RecordFailure(epoch, cfg, errors.New("boom"))
	b = b.RecordFailure(epoch, cfg, errors.New("boom"))

	b = b.RecordSuccess(epoch.Add(time.Minute))

	if b.ConsecutiveFailures != 0 {
		t.Errorf("consecutive failures = %d after a success, want 0", b.ConsecutiveFailures)
	}
	if b.OpenUntil != nil {
		t.Error("OpenUntil is still set after a success")
	}
	if b.LastError != "" {
		t.Errorf("LastError = %q after a success", b.LastError)
	}
	if b.LastSuccessAt == nil {
		t.Fatal("LastSuccessAt was not recorded")
	}
	if got := b.StateAt(epoch.Add(time.Minute), cfg); got != jobs.BreakerClosed {
		t.Errorf("state = %s, want closed", got)
	}
}

func TestAFailedHalfOpenProbeReopensTheCircuit(t *testing.T) {
	// The transition most likely to be wrong. After the cooldown the circuit reads
	// half-open; a probe that fails must put it back to open rather than leaving a past
	// OpenUntil that reads as half-open for ever and lets every request through.
	cfg := testBreakerConfig()
	b := jobs.Breaker{}
	for i := 0; i < cfg.FailureThreshold; i++ {
		b = b.RecordFailure(epoch, cfg, errors.New("503"))
	}

	probeAt := epoch.Add(cfg.BaseCooldown + time.Second)
	if got := b.StateAt(probeAt, cfg); got != jobs.BreakerHalfOpen {
		t.Fatalf("state = %s, want half_open before the probe", got)
	}

	b = b.RecordFailure(probeAt, cfg, errors.New("still down"))

	if got := b.StateAt(probeAt, cfg); got != jobs.BreakerOpen {
		t.Fatalf("state = %s after a failed probe, want open", got)
	}
	if b.Cooldown(probeAt) <= 0 {
		t.Error("the circuit reopened with no cooldown, so the source is hammered immediately")
	}
}

func TestTheCooldownBacksOffWithPersistentFailure(t *testing.T) {
	// A source down for a week should stop being polled every two minutes, but must
	// still be retried eventually, so the backoff is capped.
	cfg := testBreakerConfig()
	b := jobs.Breaker{}

	var cooldowns []time.Duration
	at := epoch
	for round := 0; round < 6; round++ {
		for i := 0; i < cfg.FailureThreshold; i++ {
			b = b.RecordFailure(at, cfg, errors.New("down"))
		}
		cooldowns = append(cooldowns, b.Cooldown(at))
		at = at.Add(b.Cooldown(at) + time.Second)
	}

	if !(cooldowns[0] < cooldowns[1]) {
		t.Errorf("the cooldown did not grow: %v", cooldowns)
	}
	for _, cooldown := range cooldowns {
		if cooldown > cfg.MaxCooldown {
			t.Errorf("cooldown %s exceeds the cap %s; a recovered source would never be retried",
				cooldown, cfg.MaxCooldown)
		}
	}
	last := cooldowns[len(cooldowns)-1]
	if last != cfg.MaxCooldown {
		t.Errorf("the final cooldown is %s, want the cap %s", last, cfg.MaxCooldown)
	}
}

func TestBreakerStateIsPlainDataSoItCanPersist(t *testing.T) {
	// Every field has to round-trip through job_sources columns. A breaker that lived
	// only in memory would hammer a failing source on every restart, which is how a
	// rate-limit ban starts.
	cfg := testBreakerConfig()
	b := jobs.Breaker{}
	for i := 0; i < cfg.FailureThreshold; i++ {
		b = b.RecordFailure(epoch, cfg, errors.New("429 too many requests"))
	}

	// Exactly what the repository would write and read back.
	restored := jobs.Breaker{
		ConsecutiveFailures: b.ConsecutiveFailures,
		OpenUntil:           b.OpenUntil,
		LastSuccessAt:       b.LastSuccessAt,
		LastError:           b.LastError,
	}

	if restored.StateAt(epoch, cfg) != jobs.BreakerOpen {
		t.Error("a restored breaker does not report the circuit as open")
	}
	if restored.Cooldown(epoch) != b.Cooldown(epoch) {
		t.Error("the restored cooldown differs from the original")
	}
}

func TestALongErrorBodyIsTruncated(t *testing.T) {
	// last_error is operator-facing and the text comes from a third party's response,
	// which is routinely a whole HTML error page.
	cfg := testBreakerConfig()
	huge := errors.New(strings.Repeat("x", 50_000))

	b := jobs.Breaker{}.RecordFailure(epoch, cfg, huge)

	if len(b.LastError) > 700 {
		t.Errorf("LastError is %d bytes; an HTML error page would be stored whole", len(b.LastError))
	}
	if !strings.Contains(b.LastError, "truncated") {
		t.Error("the truncation is not signposted, so the message reads as the whole error")
	}
}

func TestADefaultConfigIsUsable(t *testing.T) {
	cfg := jobs.DefaultBreakerConfig()
	if cfg.FailureThreshold <= 1 {
		t.Errorf("FailureThreshold = %d; opening on one transient 502 takes healthy "+
			"sources out of runs", cfg.FailureThreshold)
	}
	if cfg.BaseCooldown <= 0 || cfg.MaxCooldown < cfg.BaseCooldown {
		t.Errorf("cooldowns are not sane: %+v", cfg)
	}
}

func TestAZeroConfigFallsBackRatherThanOpeningImmediately(t *testing.T) {
	// A zero threshold read literally would open the circuit on the first failure and
	// take every source out of every run.
	b := jobs.Breaker{}.RecordFailure(epoch, jobs.BreakerConfig{}, errors.New("boom"))
	if b.OpenUntil != nil {
		t.Error("a zero config opened the circuit on the first failure")
	}
}
