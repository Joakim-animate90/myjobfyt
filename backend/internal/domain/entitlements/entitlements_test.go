package entitlements_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/Joakim-animate90/myjobfyt/backend/internal/apperr"
	"github.com/Joakim-animate90/myjobfyt/backend/internal/domain/entitlements"
	"github.com/google/uuid"
)

// fakeCounter is an in-memory Counter with injectable failure.
type fakeCounter struct {
	mu     sync.Mutex
	values map[string]int64
	err    error
}

func newFakeCounter() *fakeCounter {
	return &fakeCounter{values: map[string]int64{}}
}

func (f *fakeCounter) IncrCounter(_ context.Context, key string, by int64, _ time.Duration) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return 0, f.err
	}
	f.values[key] += by
	return f.values[key], nil
}

func (f *fakeCounter) GetCounter(_ context.Context, key string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return 0, f.err
	}
	return f.values[key], nil
}

func (f *fakeCounter) DecrCounter(_ context.Context, key string, by int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.values[key] -= by
	if f.values[key] < 0 {
		f.values[key] = 0
	}
	return nil
}

func (f *fakeCounter) get(key string) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.values[key]
}

// fakeRepo is an in-memory Repository.
type fakeRepo struct {
	mu       sync.Mutex
	plan     entitlements.Plan
	planErr  error
	rollup   map[string]int64
	levels   map[entitlements.Quota]int64
	recorded map[string]int64
}

func newFakeRepo(plan entitlements.Plan) *fakeRepo {
	return &fakeRepo{
		plan:     plan,
		rollup:   map[string]int64{},
		levels:   map[entitlements.Quota]int64{},
		recorded: map[string]int64{},
	}
}

func (f *fakeRepo) PlanForUser(context.Context, uuid.UUID) (entitlements.Plan, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.planErr != nil {
		return entitlements.Plan{}, f.planErr
	}
	return f.plan, nil
}

func (f *fakeRepo) UsageFor(_ context.Context, _ uuid.UUID, q entitlements.Quota, _ time.Time) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rollup[string(q)], nil
}

func (f *fakeRepo) RecordUsage(_ context.Context, _ uuid.UUID, q entitlements.Quota, _ time.Time, used, _ int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	// Mirrors the GREATEST in the SQL: a late smaller write must not lower usage.
	if used > f.recorded[string(q)] {
		f.recorded[string(q)] = used
	}
	f.rollup[string(q)] = f.recorded[string(q)]
	return nil
}

func (f *fakeRepo) CountLevel(_ context.Context, _ uuid.UUID, q entitlements.Quota) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.levels[q], nil
}

func freePlan() entitlements.Plan {
	return entitlements.Plan{
		Slug: "free", Name: "Free",
		Quotas: map[string]int64{
			"daily_applications": 5,
			"daily_matches":      25,
			"ai_tokens_month":    50000,
			"active_loops":       1,
			"cv_slots":           2,
		},
		Features: map[string]bool{
			"cv_tailoring": false, "smtp_custom": false,
		},
	}
}

func proPlan() entitlements.Plan {
	p := freePlan()
	p.Slug, p.Name = "pro", "Pro"
	p.Quotas["daily_applications"] = 25
	p.Quotas["cv_slots"] = 10
	p.Features["cv_tailoring"] = true
	return p
}

func newService(t *testing.T, repo *fakeRepo, counter *fakeCounter) *entitlements.Service {
	t.Helper()
	now := func() time.Time { return time.Date(2026, 11, 11, 15, 0, 0, 0, time.UTC) }
	return entitlements.NewService(repo, counter, slog.New(slog.NewTextHandler(io.Discard, nil)), now)
}

func TestConsumeAllowsUpToTheLimitThenRefuses(t *testing.T) {
	repo := newFakeRepo(freePlan())
	counter := newFakeCounter()
	svc := newService(t, repo, counter)
	user := uuid.New()
	ctx := context.Background()

	for i := 1; i <= 5; i++ {
		a, err := svc.Consume(ctx, user, entitlements.QuotaDailyApplications, 1)
		if err != nil {
			t.Fatalf("application %d refused: %v", i, err)
		}
		if a.Used != int64(i) {
			t.Errorf("after %d applications Used = %d", i, a.Used)
		}
		if a.Remaining != int64(5-i) {
			t.Errorf("after %d applications Remaining = %d, want %d", i, a.Remaining, 5-i)
		}
	}

	_, err := svc.Consume(ctx, user, entitlements.QuotaDailyApplications, 1)
	if apperr.KindOf(err) != apperr.KindQuotaExceeded {
		t.Fatalf("the sixth application returned %v, want a quota error", err)
	}
}

func TestRefusedConsumeReturnsTheReservation(t *testing.T) {
	// A refused request must not permanently cost allowance the candidate never used.
	// Without the rollback, one over-limit attempt would silently reduce tomorrow's
	// effective limit as the counter drifted upward.
	repo := newFakeRepo(freePlan())
	counter := newFakeCounter()
	svc := newService(t, repo, counter)
	user := uuid.New()
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		if _, err := svc.Consume(ctx, user, entitlements.QuotaDailyApplications, 1); err != nil {
			t.Fatalf("setup: %v", err)
		}
	}

	// Three refused attempts.
	for i := 0; i < 3; i++ {
		if _, err := svc.Consume(ctx, user, entitlements.QuotaDailyApplications, 1); err == nil {
			t.Fatal("an over-limit consume succeeded")
		}
	}

	a, err := svc.Check(ctx, user, entitlements.QuotaDailyApplications)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if a.Used != 5 {
		t.Fatalf("Used = %d after 5 successes and 3 refusals, want 5", a.Used)
	}
}

func TestConsumeRejectsAnOversizedSingleRequest(t *testing.T) {
	repo := newFakeRepo(freePlan())
	svc := newService(t, repo, newFakeCounter())

	_, err := svc.Consume(context.Background(), uuid.New(), entitlements.QuotaDailyApplications, 6)
	if apperr.KindOf(err) != apperr.KindQuotaExceeded {
		t.Fatalf("err = %v, want a quota error", err)
	}
}

func TestConcurrentConsumeNeverExceedsTheLimit(t *testing.T) {
	// The reason Consume increments first and rolls back on refusal rather than
	// checking then incrementing. A check-then-increment leaves a window where two
	// requests both see room, which on this quota means submitting more applications
	// than the candidate paid for.
	repo := newFakeRepo(proPlan()) // 25 daily applications
	counter := newFakeCounter()
	svc := newService(t, repo, counter)
	user := uuid.New()
	ctx := context.Background()

	const attempts = 100
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		accepted int
	)
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := svc.Consume(ctx, user, entitlements.QuotaDailyApplications, 1); err == nil {
				mu.Lock()
				accepted++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if accepted != 25 {
		t.Fatalf("%d of %d concurrent requests were accepted, want exactly 25", accepted, attempts)
	}
}

func TestReleaseReturnsAllowanceForFailedWork(t *testing.T) {
	// A submission that fails must not cost the candidate a slot. They notice.
	repo := newFakeRepo(freePlan())
	counter := newFakeCounter()
	svc := newService(t, repo, counter)
	user := uuid.New()
	ctx := context.Background()

	if _, err := svc.Consume(ctx, user, entitlements.QuotaDailyApplications, 1); err != nil {
		t.Fatalf("Consume: %v", err)
	}
	svc.Release(ctx, user, entitlements.QuotaDailyApplications, 1)

	a, err := svc.Check(ctx, user, entitlements.QuotaDailyApplications)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if a.Used != 0 {
		t.Fatalf("Used = %d after a release, want 0", a.Used)
	}
}

func TestLevelQuotasAreCountedLiveNotTracked(t *testing.T) {
	// A counter would drift the moment a CV was deleted by any path that did not
	// decrement it — a cascade, an erasure job, a manual fix.
	repo := newFakeRepo(freePlan()) // cv_slots: 2
	svc := newService(t, repo, newFakeCounter())
	user := uuid.New()
	ctx := context.Background()

	repo.levels[entitlements.QuotaCVSlots] = 2

	if err := svc.Require(ctx, user, entitlements.QuotaCVSlots, 1); apperr.KindOf(err) != apperr.KindQuotaExceeded {
		t.Fatalf("Require at the limit = %v, want a quota error", err)
	}

	// A CV is deleted elsewhere. The live count must immediately reflect it.
	repo.levels[entitlements.QuotaCVSlots] = 1
	if err := svc.Require(ctx, user, entitlements.QuotaCVSlots, 1); err != nil {
		t.Fatalf("Require after a deletion = %v, want nil", err)
	}
}

func TestConsumeRefusesLevelQuotas(t *testing.T) {
	// Consuming a level is a programming error: there is no counter to increment.
	repo := newFakeRepo(freePlan())
	svc := newService(t, repo, newFakeCounter())

	for _, q := range []entitlements.Quota{entitlements.QuotaCVSlots, entitlements.QuotaActiveLoops} {
		if _, err := svc.Consume(context.Background(), uuid.New(), q, 1); apperr.KindOf(err) != apperr.KindInternal {
			t.Errorf("Consume(%q) = %v, want an internal error", q, err)
		}
	}
}

func TestCounterOutageFallsBackToTheRollupRatherThanFailing(t *testing.T) {
	// A Redis outage must not stop a candidate applying for jobs.
	repo := newFakeRepo(freePlan())
	counter := newFakeCounter()
	svc := newService(t, repo, counter)
	user := uuid.New()
	ctx := context.Background()

	counter.err = errors.New("dial tcp: connection refused")
	repo.rollup["daily_applications"] = 2

	a, err := svc.Consume(ctx, user, entitlements.QuotaDailyApplications, 1)
	if err != nil {
		t.Fatalf("Consume during a counter outage = %v, want success via the rollup", err)
	}
	if a.Used != 3 {
		t.Fatalf("Used = %d, want 3 (rollup 2 + 1)", a.Used)
	}
}

func TestCounterOutageStillEnforcesTheLimit(t *testing.T) {
	// Failing open on availability must not mean failing open on the limit itself.
	repo := newFakeRepo(freePlan())
	counter := newFakeCounter()
	svc := newService(t, repo, counter)

	counter.err = errors.New("connection refused")
	repo.rollup["daily_applications"] = 5 // already at the limit

	if _, err := svc.Consume(context.Background(), uuid.New(), entitlements.QuotaDailyApplications, 1); apperr.KindOf(err) != apperr.KindQuotaExceeded {
		t.Fatalf("err = %v, want a quota error even during a counter outage", err)
	}
}

func TestQuotaExceededErrorCarriesWhatTheUICanShow(t *testing.T) {
	repo := newFakeRepo(freePlan())
	svc := newService(t, repo, newFakeCounter())
	user := uuid.New()
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		if _, err := svc.Consume(ctx, user, entitlements.QuotaDailyApplications, 1); err != nil {
			t.Fatalf("setup: %v", err)
		}
	}
	_, err := svc.Consume(ctx, user, entitlements.QuotaDailyApplications, 1)
	e := apperr.As(err)
	if e == nil {
		t.Fatalf("err = %v, want an *apperr.Error", err)
	}
	for _, key := range []string{"quota_key", "limit", "resets_at"} {
		if _, ok := e.Meta[key]; !ok {
			t.Errorf("Meta is missing %q; the UI cannot explain the limit or when it lifts", key)
		}
	}
}

func TestPeriodBoundaryResetsTheDailyCounter(t *testing.T) {
	// The counter key includes the period start, so crossing midnight UTC gives a
	// fresh key rather than relying on a TTL having fired.
	repo := newFakeRepo(freePlan())
	counter := newFakeCounter()
	user := uuid.New()
	ctx := context.Background()

	day1 := time.Date(2026, 11, 11, 23, 59, 0, 0, time.UTC)
	svc1 := entitlements.NewService(repo, counter, slog.New(slog.NewTextHandler(io.Discard, nil)),
		func() time.Time { return day1 })
	for i := 0; i < 5; i++ {
		if _, err := svc1.Consume(ctx, user, entitlements.QuotaDailyApplications, 1); err != nil {
			t.Fatalf("day 1 application %d: %v", i, err)
		}
	}
	if _, err := svc1.Consume(ctx, user, entitlements.QuotaDailyApplications, 1); err == nil {
		t.Fatal("day 1 allowed a sixth application")
	}

	day2 := day1.Add(2 * time.Minute) // past midnight UTC
	svc2 := entitlements.NewService(repo, counter, slog.New(slog.NewTextHandler(io.Discard, nil)),
		func() time.Time { return day2 })
	if _, err := svc2.Consume(ctx, user, entitlements.QuotaDailyApplications, 1); err != nil {
		t.Fatalf("day 2 refused the first application: %v", err)
	}
}

func TestFeatureGating(t *testing.T) {
	ctx := context.Background()
	user := uuid.New()

	free := newService(t, newFakeRepo(freePlan()), newFakeCounter())
	if err := free.RequireFeature(ctx, user, entitlements.FeatureCVTailoring); apperr.KindOf(err) != apperr.KindForbidden {
		t.Fatalf("free plan tailoring = %v, want forbidden", err)
	}
	// The error must name the feature and the plan so the UI can prompt precisely
	// rather than showing a generic upgrade dead end.
	e := apperr.As(free.RequireFeature(ctx, user, entitlements.FeatureCVTailoring))
	if e.Meta["feature"] != "cv_tailoring" || e.Meta["plan"] != "free" {
		t.Errorf("Meta = %v, want the feature and plan named", e.Meta)
	}

	pro := newService(t, newFakeRepo(proPlan()), newFakeCounter())
	if err := pro.RequireFeature(ctx, user, entitlements.FeatureCVTailoring); err != nil {
		t.Fatalf("pro plan tailoring = %v, want nil", err)
	}
}

func TestUndefinedFeatureIsDisabled(t *testing.T) {
	// Adding a flag must not retroactively enable it on every existing plan.
	plan := freePlan()
	delete(plan.Features, "cv_tailoring")
	svc := newService(t, newFakeRepo(plan), newFakeCounter())

	has, err := svc.HasFeature(context.Background(), uuid.New(), entitlements.FeatureCVTailoring)
	if err != nil {
		t.Fatalf("HasFeature: %v", err)
	}
	if has {
		t.Fatal("an undefined feature was reported as enabled")
	}
}

func TestUndefinedQuotaIsAnErrorNotUnlimited(t *testing.T) {
	// Defaulting an unknown quota to infinity is how a missing seed value becomes an
	// unbounded AI bill.
	plan := freePlan()
	delete(plan.Quotas, "ai_tokens_month")
	svc := newService(t, newFakeRepo(plan), newFakeCounter())

	if _, err := svc.Check(context.Background(), uuid.New(), entitlements.QuotaAITokensMonth); apperr.KindOf(err) != apperr.KindInternal {
		t.Fatalf("err = %v, want an internal error rather than unlimited access", err)
	}
}

func TestPlanLookupFailurePropagates(t *testing.T) {
	repo := newFakeRepo(freePlan())
	repo.planErr = errors.New("subscriptions unavailable")
	svc := newService(t, repo, newFakeCounter())

	if _, err := svc.Check(context.Background(), uuid.New(), entitlements.QuotaDailyApplications); err == nil {
		t.Fatal("a plan lookup failure was swallowed")
	}
}

func TestQuotaPeriodClassification(t *testing.T) {
	cases := map[entitlements.Quota]entitlements.Period{
		entitlements.QuotaDailyApplications: entitlements.PeriodDay,
		entitlements.QuotaDailyMatches:      entitlements.PeriodDay,
		entitlements.QuotaAITokensMonth:     entitlements.PeriodMonth,
		entitlements.QuotaCVSlots:           entitlements.PeriodNone,
		entitlements.QuotaActiveLoops:       entitlements.PeriodNone,
	}
	for q, want := range cases {
		if got := q.Period(); got != want {
			t.Errorf("Quota(%q).Period() = %q, want %q", q, got, want)
		}
	}
	if !entitlements.QuotaCVSlots.IsLevel() || !entitlements.QuotaActiveLoops.IsLevel() {
		t.Error("cv_slots and active_loops must be levels")
	}
	if entitlements.QuotaDailyApplications.IsLevel() {
		t.Error("daily_applications must not be a level")
	}
}

func TestLevelQuotaErrorOmitsAResetTimeAndSaysWhatToDo(t *testing.T) {
	// A level quota never resets on a clock. Emitting a zero timestamp would have the
	// UI tell the candidate their limit resets on 1 January, year 1.
	repo := newFakeRepo(freePlan()) // cv_slots: 2
	repo.levels[entitlements.QuotaCVSlots] = 2
	svc := newService(t, repo, newFakeCounter())

	err := svc.Require(context.Background(), uuid.New(), entitlements.QuotaCVSlots, 1)
	e := apperr.As(err)
	if e == nil {
		t.Fatalf("err = %v, want an *apperr.Error", err)
	}
	if _, present := e.Meta["resets_at"]; present {
		t.Errorf("resets_at is present for a level quota: %v", e.Meta["resets_at"])
	}
	if e.Meta["resolution"] != "remove_one_or_upgrade" {
		t.Errorf("resolution = %v, want remove_one_or_upgrade", e.Meta["resolution"])
	}
}

func TestFlowQuotaErrorCarriesARealResetTime(t *testing.T) {
	repo := newFakeRepo(freePlan())
	svc := newService(t, repo, newFakeCounter())
	user := uuid.New()
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		if _, err := svc.Consume(ctx, user, entitlements.QuotaDailyApplications, 1); err != nil {
			t.Fatalf("setup: %v", err)
		}
	}
	e := apperr.As(svc.Require(ctx, user, entitlements.QuotaDailyApplications, 1))
	if e == nil {
		t.Fatal("expected a quota error")
	}

	resets, ok := e.Meta["resets_at"].(string)
	if !ok || resets == "" {
		t.Fatalf("resets_at = %v, want an RFC3339 timestamp", e.Meta["resets_at"])
	}
	parsed, perr := time.Parse(time.RFC3339, resets)
	if perr != nil {
		t.Fatalf("resets_at is not RFC3339: %v", perr)
	}
	// Midnight UTC the following day, not the zero time.
	if parsed.Year() < 2020 {
		t.Fatalf("resets_at = %v, want a real future time", parsed)
	}
	if e.Meta["resolution"] != "wait_or_upgrade" {
		t.Errorf("resolution = %v, want wait_or_upgrade", e.Meta["resolution"])
	}
}
