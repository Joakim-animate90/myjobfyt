package jobs_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/Joakim-animate90/myjobfyt/backend/internal/domain/jobs"
)

// fakeAdapter is a scriptable source.
type fakeAdapter struct {
	slug  string
	caps  jobs.Capabilities
	pages []jobs.Page
	// errAfter makes Search fail once this many calls have been made. Zero never fails.
	errAfter    int
	err         error
	healthErr   error
	searchCalls int
	healthCalls int
	lastCursor  string
}

func (f *fakeAdapter) Slug() string                    { return f.slug }
func (f *fakeAdapter) Capabilities() jobs.Capabilities { return f.caps }

func (f *fakeAdapter) Search(ctx context.Context, _ jobs.SearchSpec, cursor string) (jobs.Page, error) {
	if err := ctx.Err(); err != nil {
		return jobs.Page{}, err
	}
	f.searchCalls++
	f.lastCursor = cursor

	if f.errAfter > 0 && f.searchCalls > f.errAfter {
		return jobs.Page{}, f.err
	}
	if f.errAfter == 0 && f.err != nil {
		return jobs.Page{}, f.err
	}
	idx := f.searchCalls - 1
	if idx >= len(f.pages) {
		return jobs.Page{}, nil
	}
	return f.pages[idx], nil
}

func (f *fakeAdapter) Fetch(context.Context, string) (jobs.RawPosting, error) {
	return jobs.RawPosting{}, nil
}

func (f *fakeAdapter) HealthCheck(context.Context) error {
	f.healthCalls++
	return f.healthErr
}

func postings(ids ...string) []jobs.RawPosting {
	out := make([]jobs.RawPosting, 0, len(ids))
	for _, id := range ids {
		payload, _ := json.Marshal(map[string]string{"id": id})
		out = append(out, jobs.RawPosting{
			ExternalID: id, Payload: payload, FetchedAt: epoch,
		})
	}
	return out
}

func fullCaps() jobs.Capabilities {
	return jobs.Capabilities{
		Keywords: true, Location: true, Radius: true, RemoteFilter: true,
		WorkModeFilter: true, EmploymentTypeFilter: true, SalaryFilter: true,
		PostedWithinFilter: true, Pagination: true, MaxPageSize: 50,
	}
}

func testSource(adapter jobs.Adapter, mutate ...func(*jobs.SourceRecord)) jobs.Source {
	rec := jobs.SourceRecord{
		Slug: adapter.Slug(), Name: adapter.Slug(), Kind: jobs.KindOfficialAPI,
		State: jobs.StateEnabled, RateLimit: 60,
		LegalBasis: "documented public API, terms reviewed 2026-03-01",
	}
	for _, m := range mutate {
		m(&rec)
	}
	return rec.Bind(adapter)
}

func newExecutor(t *testing.T) *jobs.Executor {
	t.Helper()
	return jobs.NewExecutor(jobs.ExecutorConfig{
		Breaker: testBreakerConfig(),
		// Pinned to the same instant the breaker fixtures use, so a cooldown opened at
		// epoch is still open when the executor evaluates it.
		Now: func() time.Time { return epoch },
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestASuccessfulSearchCollectsEveryPage(t *testing.T) {
	adapter := &fakeAdapter{slug: "fake", caps: fullCaps(), pages: []jobs.Page{
		{Postings: postings("a", "b"), NextCursor: "p2"},
		{Postings: postings("c"), NextCursor: ""},
	}}
	result := newExecutor(t).Search(context.Background(), testSource(adapter),
		jobs.SearchSpec{Keywords: []string{"go"}}, "")

	if result.Outcome != jobs.OutcomeOK {
		t.Fatalf("outcome = %s (%s)", result.Outcome, result.Reason)
	}
	if len(result.Postings) != 3 {
		t.Errorf("collected %d postings, want 3", len(result.Postings))
	}
	if result.Pages != 2 {
		t.Errorf("made %d requests, want 2", result.Pages)
	}
	if adapter.lastCursor != "p2" {
		t.Errorf("the second request used cursor %q, want p2", adapter.lastCursor)
	}
	if result.Breaker.LastSuccessAt == nil {
		t.Error("a successful run did not record a success on the breaker")
	}
}

func TestAFailingSourceDegradesAndNeverReturnsAnError(t *testing.T) {
	// The central guarantee of MJF-020. The executor's signature has no error, so a
	// caller iterating sources cannot abort the run by propagating one source's
	// failure. This asserts the outcome carries the problem instead.
	adapter := &fakeAdapter{slug: "broken", caps: fullCaps(), err: errors.New("503 service unavailable")}
	result := newExecutor(t).Search(context.Background(), testSource(adapter),
		jobs.SearchSpec{Keywords: []string{"go"}}, "")

	if result.Outcome != jobs.OutcomeFailed {
		t.Fatalf("outcome = %s, want failed", result.Outcome)
	}
	if result.Reason == "" {
		t.Error("the failure carries no reason for the run record")
	}
	if result.Breaker.ConsecutiveFailures != 1 {
		t.Errorf("consecutive failures = %d, want 1", result.Breaker.ConsecutiveFailures)
	}
}

func TestPagesAlreadyRetrievedSurviveALaterFailure(t *testing.T) {
	// Discarding them would waste the source's rate limit and leave the candidate with
	// fewer matches for no benefit.
	adapter := &fakeAdapter{
		slug: "flaky", caps: fullCaps(),
		pages:    []jobs.Page{{Postings: postings("a", "b"), NextCursor: "p2"}},
		errAfter: 1, err: errors.New("504 gateway timeout"),
	}
	result := newExecutor(t).Search(context.Background(), testSource(adapter),
		jobs.SearchSpec{Keywords: []string{"go"}}, "")

	if result.Outcome != jobs.OutcomePartial {
		t.Fatalf("outcome = %s, want partial", result.Outcome)
	}
	if len(result.Postings) != 2 {
		t.Errorf("kept %d postings, want the 2 already retrieved", len(result.Postings))
	}
	if result.Cursor != "p2" {
		t.Errorf("cursor = %q, want p2 so the run resumes rather than restarting", result.Cursor)
	}
}

func TestAnUnsupportedSearchIsSkippedNotIssued(t *testing.T) {
	// A source that silently ignores an unsupported filter returns everything, and the
	// run looks successful while the results are wrong.
	adapter := &fakeAdapter{slug: "keywords-only", caps: jobs.Capabilities{Keywords: true}}
	result := newExecutor(t).Search(context.Background(), testSource(adapter),
		jobs.SearchSpec{Keywords: []string{"go"}, MinSalary: 100000}, "")

	if result.Outcome != jobs.OutcomeSkippedCapability {
		t.Fatalf("outcome = %s, want skipped_capability", result.Outcome)
	}
	if adapter.searchCalls != 0 {
		t.Errorf("the adapter was called %d times for a query it cannot serve", adapter.searchCalls)
	}
	if result.Reason == "" {
		t.Error("the skip has no reason; the run record would show a missing source and no explanation")
	}
}

func TestAnOpenBreakerSkipsTheSourceWithoutCallingIt(t *testing.T) {
	cfg := testBreakerConfig()
	breaker := jobs.Breaker{}
	for i := 0; i < cfg.FailureThreshold; i++ {
		breaker = breaker.RecordFailure(epoch, cfg, errors.New("down"))
	}

	adapter := &fakeAdapter{slug: "down", caps: fullCaps()}
	source := testSource(adapter, func(r *jobs.SourceRecord) { r.Breaker = breaker })

	result := newExecutor(t).Search(context.Background(), source, jobs.SearchSpec{Keywords: []string{"go"}}, "")

	if result.Outcome != jobs.OutcomeSkippedBreakerOpen {
		t.Fatalf("outcome = %s, want skipped_breaker_open", result.Outcome)
	}
	if adapter.searchCalls != 0 {
		t.Errorf("an open circuit still made %d request(s)", adapter.searchCalls)
	}
	if result.Reason == "" {
		t.Error("the skip does not say how long the cooldown has left")
	}
}

func TestASourceWithNoLegalBasisIsNeverCalled(t *testing.T) {
	// The database refuses to enable one, and this refuses to use one constructed in
	// memory. That check is the difference between an integration and a scraping
	// incident, so it is enforced in both places.
	adapter := &fakeAdapter{slug: "unvetted", caps: fullCaps()}
	source := testSource(adapter, func(r *jobs.SourceRecord) { r.LegalBasis = "   " })

	result := newExecutor(t).Search(context.Background(), source, jobs.SearchSpec{Keywords: []string{"go"}}, "")

	if result.Outcome != jobs.OutcomeSkippedUnusable {
		t.Fatalf("outcome = %s, want skipped_unusable", result.Outcome)
	}
	if adapter.searchCalls != 0 {
		t.Errorf("a source with no recorded legal basis was queried %d time(s)", adapter.searchCalls)
	}
}

func TestAPartnershipRequiredSourceIsNeverCalled(t *testing.T) {
	adapter := &fakeAdapter{slug: "needs-deal", caps: fullCaps()}
	source := testSource(adapter, func(r *jobs.SourceRecord) { r.State = jobs.StatePartnershipRequired })

	result := newExecutor(t).Search(context.Background(), source, jobs.SearchSpec{Keywords: []string{"go"}}, "")

	if result.Outcome != jobs.OutcomeSkippedUnusable {
		t.Fatalf("outcome = %s, want skipped_unusable", result.Outcome)
	}
	if adapter.searchCalls != 0 {
		t.Error("a source needing a partnership agreement was queried")
	}
}

func TestRepeatedFailuresOpenTheBreakerAndSignalItOnce(t *testing.T) {
	// The caller alerts on BreakerOpened, so it must be true exactly once rather than
	// on every subsequent skip.
	cfg := testBreakerConfig()
	exec := newExecutor(t)
	adapter := &fakeAdapter{slug: "dying", caps: fullCaps(), err: errors.New("503")}

	breaker := jobs.Breaker{}
	var openedCount int
	for i := 0; i < cfg.FailureThreshold; i++ {
		source := testSource(adapter, func(r *jobs.SourceRecord) { r.Breaker = breaker })
		result := exec.Search(context.Background(), source, jobs.SearchSpec{Keywords: []string{"go"}}, "")
		breaker = result.Breaker
		if result.BreakerOpened {
			openedCount++
		}
	}

	if openedCount != 1 {
		t.Errorf("BreakerOpened was true %d times, want exactly 1", openedCount)
	}
	if breaker.OpenUntil == nil {
		t.Fatal("the circuit did not open")
	}

	// And the next attempt is skipped rather than signalling again.
	source := testSource(adapter, func(r *jobs.SourceRecord) { r.Breaker = breaker })
	next := exec.Search(context.Background(), source, jobs.SearchSpec{Keywords: []string{"go"}}, "")
	if next.BreakerOpened {
		t.Error("BreakerOpened was signalled again on a skip, which would alert repeatedly")
	}
}

func TestACursorLoopIsBounded(t *testing.T) {
	// A source whose cursor never terminates would otherwise spin until the run's
	// deadline, consuming its whole rate limit.
	page := jobs.Page{Postings: postings("a"), NextCursor: "always-more"}
	adapter := &fakeAdapter{slug: "looping", caps: fullCaps()}
	for i := 0; i < 500; i++ {
		adapter.pages = append(adapter.pages, page)
	}

	exec := jobs.NewExecutor(jobs.ExecutorConfig{
		Breaker: testBreakerConfig(), MaxPages: 5,
		Now: func() time.Time { return epoch },
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	result := exec.Search(context.Background(), testSource(adapter), jobs.SearchSpec{Keywords: []string{"go"}}, "")

	if result.Pages > 5 {
		t.Errorf("made %d requests despite a 5-page cap", result.Pages)
	}
	if result.Outcome != jobs.OutcomeOK {
		t.Errorf("outcome = %s; hitting the page cap is not a failure", result.Outcome)
	}
}

func TestACursorWithNoPostingsStopsTheRun(t *testing.T) {
	// A source bug. Following it is the other way a discovery run spins.
	adapter := &fakeAdapter{slug: "empty-cursor", caps: fullCaps(), pages: []jobs.Page{
		{Postings: postings("a"), NextCursor: "p2"},
		{Postings: nil, NextCursor: "p3"},
	}}
	result := newExecutor(t).Search(context.Background(), testSource(adapter),
		jobs.SearchSpec{Keywords: []string{"go"}}, "")

	if result.Pages != 2 {
		t.Errorf("made %d requests, want 2 then stop", result.Pages)
	}
	if result.Outcome != jobs.OutcomeOK {
		t.Errorf("outcome = %s", result.Outcome)
	}
}

func TestTheLimitStopsPagingEarly(t *testing.T) {
	adapter := &fakeAdapter{slug: "many", caps: fullCaps(), pages: []jobs.Page{
		{Postings: postings("a", "b", "c"), NextCursor: "p2"},
		{Postings: postings("d", "e", "f"), NextCursor: "p3"},
	}}
	result := newExecutor(t).Search(context.Background(), testSource(adapter),
		jobs.SearchSpec{Keywords: []string{"go"}, Limit: 3}, "")

	if result.Pages != 1 {
		t.Errorf("made %d requests for a limit of 3 with 3 on the first page", result.Pages)
	}
}

func TestANonPaginatingSourceIsAskedOnce(t *testing.T) {
	// It declared no pagination, so a next cursor it returned anyway must not be
	// followed — the executor would be acting on a capability the adapter disclaimed.
	caps := fullCaps()
	caps.Pagination = false
	adapter := &fakeAdapter{slug: "single", caps: caps, pages: []jobs.Page{
		{Postings: postings("a"), NextCursor: "p2"},
		{Postings: postings("b"), NextCursor: ""},
	}}
	result := newExecutor(t).Search(context.Background(), testSource(adapter),
		jobs.SearchSpec{Keywords: []string{"go"}}, "")

	if result.Pages != 1 {
		t.Errorf("made %d requests of a non-paginating source", result.Pages)
	}
}

func TestCancellationIsNotCountedAgainstTheSource(t *testing.T) {
	// A deploy must not penalise a healthy source and open its circuit.
	adapter := &fakeAdapter{slug: "fine", caps: fullCaps(), pages: []jobs.Page{
		{Postings: postings("a"), NextCursor: "p2"},
	}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	result := newExecutor(t).Search(ctx, testSource(adapter), jobs.SearchSpec{Keywords: []string{"go"}}, "")

	if result.Breaker.ConsecutiveFailures != 0 {
		t.Errorf("cancellation counted %d failure(s) against the source",
			result.Breaker.ConsecutiveFailures)
	}
	if result.Outcome == jobs.OutcomeFailed {
		t.Error("cancellation was reported as a source failure")
	}
}

func TestResumingUsesTheSuppliedCursor(t *testing.T) {
	adapter := &fakeAdapter{slug: "resumable", caps: fullCaps(), pages: []jobs.Page{
		{Postings: postings("c"), NextCursor: ""},
	}}
	newExecutor(t).Search(context.Background(), testSource(adapter),
		jobs.SearchSpec{Keywords: []string{"go"}}, "saved-cursor")

	if adapter.lastCursor != "saved-cursor" {
		t.Errorf("the adapter was called with cursor %q, want the persisted one", adapter.lastCursor)
	}
}

func TestARateLimitErrorStillCountsTowardsTheBreaker(t *testing.T) {
	// Backing off is exactly the right response to a 429, even though the cause is our
	// pacing rather than the source being broken.
	adapter := &fakeAdapter{slug: "throttled", caps: fullCaps(),
		err: &jobs.ErrRateLimited{Slug: "throttled", RetryAfter: 30 * time.Second}}

	result := newExecutor(t).Search(context.Background(), testSource(adapter),
		jobs.SearchSpec{Keywords: []string{"go"}}, "")

	if result.Breaker.ConsecutiveFailures != 1 {
		t.Errorf("consecutive failures = %d after a 429, want 1", result.Breaker.ConsecutiveFailures)
	}
	if result.Outcome != jobs.OutcomeFailed {
		t.Errorf("outcome = %s", result.Outcome)
	}
}

func TestAHalfOpenProbeClosesTheCircuitOnSuccess(t *testing.T) {
	cfg := testBreakerConfig()
	breaker := jobs.Breaker{}
	for i := 0; i < cfg.FailureThreshold; i++ {
		breaker = breaker.RecordFailure(epoch, cfg, errors.New("down"))
	}

	adapter := &fakeAdapter{slug: "recovering", caps: fullCaps()}
	source := testSource(adapter, func(r *jobs.SourceRecord) { r.Breaker = breaker })

	exec := jobs.NewExecutor(jobs.ExecutorConfig{
		Breaker: cfg, Now: func() time.Time { return epoch },
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	// Still inside the cooldown: the probe must be refused rather than made.
	updated, ok := exec.Probe(context.Background(), source)
	if ok {
		t.Error("a probe was allowed while the circuit was still open")
	}
	if adapter.healthCalls != 0 {
		t.Errorf("the health check ran %d time(s) during the cooldown", adapter.healthCalls)
	}
	_ = updated
}

func TestAFailedProbeKeepsTheCircuitShut(t *testing.T) {
	adapter := &fakeAdapter{slug: "still-down", caps: fullCaps(),
		healthErr: fmt.Errorf("connection refused")}

	updated, ok := newExecutor(t).Probe(context.Background(), testSource(adapter))
	if ok {
		t.Error("a failed health check reported the source as recovered")
	}
	if updated.ConsecutiveFailures == 0 {
		t.Error("a failed probe did not count against the breaker")
	}
}

func TestASuccessfulProbeRecordsRecovery(t *testing.T) {
	adapter := &fakeAdapter{slug: "healthy", caps: fullCaps()}

	updated, ok := newExecutor(t).Probe(context.Background(), testSource(adapter))
	if !ok {
		t.Fatal("a healthy source was not reported as recovered")
	}
	if updated.LastSuccessAt == nil {
		t.Error("recovery was not recorded on the breaker")
	}
	if adapter.healthCalls != 1 {
		t.Errorf("the health check ran %d time(s), want 1", adapter.healthCalls)
	}
}
