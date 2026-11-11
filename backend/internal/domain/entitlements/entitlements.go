// Package entitlements resolves a candidate's plan and enforces its quotas.
//
// Tier logic lives here and nowhere else. Scattering "if plan == premium" through the
// domain packages is how a tier change becomes a code change in nine places, one of
// which gets missed. Every other domain asks this package a question and acts on the
// answer.
package entitlements

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/Joakim-animate90/myjobfyt/backend/internal/apperr"
	"github.com/Joakim-animate90/myjobfyt/backend/internal/platform/postgres"
	"github.com/google/uuid"
)

// Quota names a counted allowance.
type Quota string

const (
	// QuotaDailyApplications caps submissions per UTC day.
	QuotaDailyApplications Quota = "daily_applications"
	// QuotaDailyMatches caps new matches surfaced per UTC day.
	QuotaDailyMatches Quota = "daily_matches"
	// QuotaAITokensMonth caps AI spend per calendar month.
	QuotaAITokensMonth Quota = "ai_tokens_month"
	// QuotaActiveLoops caps concurrently active loops. Unlike the others this is a
	// level, not a flow: it is checked against a live count rather than a counter.
	QuotaActiveLoops Quota = "active_loops"
	// QuotaCVSlots caps stored CVs. Also a level.
	QuotaCVSlots Quota = "cv_slots"
)

// IsLevel reports whether the quota measures a current level rather than usage over a
// period. Levels are checked against a live count, so they cannot drift out of step
// with reality the way a counter can.
func (q Quota) IsLevel() bool {
	return q == QuotaActiveLoops || q == QuotaCVSlots
}

// Period returns the reset period for a flow quota.
func (q Quota) Period() Period {
	switch q {
	case QuotaAITokensMonth:
		return PeriodMonth
	case QuotaDailyApplications, QuotaDailyMatches:
		return PeriodDay
	default:
		return PeriodNone
	}
}

// Period is a quota's reset cadence.
type Period string

const (
	PeriodNone  Period = "none"
	PeriodDay   Period = "day"
	PeriodMonth Period = "month"
)

// Feature names a boolean capability.
type Feature string

const (
	FeatureWorldwideRemote   Feature = "worldwide_remote"
	FeatureCustomSMTP        Feature = "smtp_custom"
	FeatureCVTailoring       Feature = "cv_tailoring"
	FeatureAIQuestionAnswers Feature = "ai_question_answering"
	FeatureCareerPageSearch  Feature = "career_page_search"
	FeatureFullAnalytics     Feature = "analytics_full"
	FeatureManagedService    Feature = "managed_service"
)

// Plan is a resolved plan with its quotas and features.
type Plan struct {
	Slug     string
	Name     string
	Quotas   map[string]int64
	Features map[string]bool
}

// Limit returns the quota's limit and whether the plan defines it. A quota the plan does
// not mention is treated as absent rather than as unlimited: defaulting an unknown quota
// to infinity is how a missing seed value becomes an unbounded bill.
func (p Plan) Limit(q Quota) (int64, bool) {
	v, ok := p.Quotas[string(q)]
	return v, ok
}

// Has reports whether a feature is enabled. An undefined feature is disabled, so adding
// a feature flag does not retroactively enable it on every existing plan.
func (p Plan) Has(f Feature) bool { return p.Features[string(f)] }

// Allowance is the answer to a quota question.
type Allowance struct {
	Quota     Quota
	Limit     int64
	Used      int64
	Remaining int64
	ResetsAt  time.Time
}

// Counter is the fast usage counter, satisfied by Redis.
type Counter interface {
	IncrCounter(ctx context.Context, key string, by int64, ttl time.Duration) (int64, error)
	GetCounter(ctx context.Context, key string) (int64, error)
	DecrCounter(ctx context.Context, key string, by int64) error
}

// Repository reads plans and reconciles usage.
type Repository interface {
	PlanForUser(ctx context.Context, userID uuid.UUID) (Plan, error)
	UsageFor(ctx context.Context, userID uuid.UUID, quota Quota, periodStart time.Time) (int64, error)
	RecordUsage(ctx context.Context, userID uuid.UUID, quota Quota, periodStart time.Time, used, limit int64) error
	CountLevel(ctx context.Context, userID uuid.UUID, quota Quota) (int64, error)
}

// Service answers entitlement questions.
type Service struct {
	repo    Repository
	counter Counter
	log     *slog.Logger
	now     func() time.Time
}

// NewService builds the entitlements service.
func NewService(repo Repository, counter Counter, log *slog.Logger, now func() time.Time) *Service {
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &Service{repo: repo, counter: counter, log: log, now: now}
}

// Plan resolves the candidate's effective plan.
func (s *Service) Plan(ctx context.Context, userID uuid.UUID) (Plan, error) {
	return s.repo.PlanForUser(ctx, userID)
}

// HasFeature reports whether the candidate's plan includes a feature.
func (s *Service) HasFeature(ctx context.Context, userID uuid.UUID, f Feature) (bool, error) {
	plan, err := s.repo.PlanForUser(ctx, userID)
	if err != nil {
		return false, err
	}
	return plan.Has(f), nil
}

// RequireFeature returns a forbidden error naming the feature when the plan lacks it.
//
// The error carries the feature and the current plan so the UI can show a targeted
// upgrade prompt at the point of friction rather than a generic "upgrade" dead end.
func (s *Service) RequireFeature(ctx context.Context, userID uuid.UUID, f Feature) error {
	plan, err := s.repo.PlanForUser(ctx, userID)
	if err != nil {
		return err
	}
	if plan.Has(f) {
		return nil
	}
	return apperr.Forbidden("feature_not_in_plan", "this feature is not included in your plan").
		WithMeta("feature", string(f)).
		WithMeta("plan", plan.Slug)
}

// Check reports the allowance for a quota without consuming any of it.
func (s *Service) Check(ctx context.Context, userID uuid.UUID, q Quota) (Allowance, error) {
	plan, err := s.repo.PlanForUser(ctx, userID)
	if err != nil {
		return Allowance{}, err
	}

	limit, defined := plan.Limit(q)
	if !defined {
		return Allowance{}, apperr.Internalf(
			fmt.Errorf("plan %q does not define quota %q", plan.Slug, q),
			"entitlement lookup")
	}

	used, err := s.usage(ctx, userID, q)
	if err != nil {
		return Allowance{}, err
	}

	remaining := limit - used
	if remaining < 0 {
		remaining = 0
	}

	return Allowance{
		Quota:     q,
		Limit:     limit,
		Used:      used,
		Remaining: remaining,
		ResetsAt:  s.periodEnd(q),
	}, nil
}

// Require returns a quota error when the candidate cannot afford n more units.
func (s *Service) Require(ctx context.Context, userID uuid.UUID, q Quota, n int64) error {
	allowance, err := s.Check(ctx, userID, q)
	if err != nil {
		return err
	}
	if allowance.Remaining < n {
		return s.quotaError(q, allowance)
	}
	return nil
}

// quotaError builds the client-facing quota error.
//
// A level quota has no reset time, so it reports what the candidate must do instead.
// Emitting a zero timestamp would have the UI tell them their limit resets on
// 1 January, year 1.
func (s *Service) quotaError(q Quota, a Allowance) error {
	resets := ""
	if !a.ResetsAt.IsZero() {
		resets = a.ResetsAt.Format(time.RFC3339)
	}

	err := apperr.QuotaExceeded(string(q), a.Limit, resets).
		WithMeta("used", a.Used).
		WithMeta("remaining", a.Remaining)

	if q.IsLevel() {
		// Nothing frees a level quota except the candidate removing something or
		// upgrading, so say so rather than implying they can wait it out.
		return err.WithMeta("resolution", "remove_one_or_upgrade")
	}
	return err.WithMeta("resolution", "wait_or_upgrade")
}

// Consume reserves n units of a flow quota, returning a quota error if it would exceed
// the limit.
//
// The increment happens first and is rolled back on refusal, rather than checking then
// incrementing. A check-then-increment leaves a window in which two concurrent requests
// both see room and both proceed, which on the applications quota means submitting more
// applications than the candidate paid for.
func (s *Service) Consume(ctx context.Context, userID uuid.UUID, q Quota, n int64) (Allowance, error) {
	if q.IsLevel() {
		return Allowance{}, apperr.Internalf(
			fmt.Errorf("quota %q is a level and cannot be consumed", q),
			"entitlement misuse")
	}

	plan, err := s.repo.PlanForUser(ctx, userID)
	if err != nil {
		return Allowance{}, err
	}
	limit, defined := plan.Limit(q)
	if !defined {
		return Allowance{}, apperr.Internalf(
			fmt.Errorf("plan %q does not define quota %q", plan.Slug, q),
			"entitlement lookup")
	}

	key := s.counterKey(userID, q)
	ttl := s.periodTTL(q)

	used, err := s.counter.IncrCounter(ctx, key, n, ttl)
	if err != nil {
		// Redis is unreachable. Fall back to the Postgres rollup, which lags but is
		// authoritative, rather than failing the operation: a counter outage must not
		// stop a candidate applying for jobs.
		s.log.ErrorContext(ctx, "quota counter unavailable; falling back to the rollup",
			slog.String("quota", string(q)), slog.Any("error", err))

		rollup, rerr := s.repo.UsageFor(ctx, userID, q, s.periodStart(q))
		if rerr != nil {
			return Allowance{}, apperr.Dependency("quota_counter", err)
		}
		if rollup+n > limit {
			return Allowance{}, s.quotaError(q, Allowance{
				Quota: q, Limit: limit, Used: rollup,
				Remaining: max64(limit-rollup, 0), ResetsAt: s.periodEnd(q),
			})
		}
		// Record directly so the reservation is not lost while Redis is down.
		if rerr := s.repo.RecordUsage(ctx, userID, q, s.periodStart(q), rollup+n, limit); rerr != nil {
			return Allowance{}, rerr
		}
		return Allowance{Quota: q, Limit: limit, Used: rollup + n,
			Remaining: max64(limit-(rollup+n), 0), ResetsAt: s.periodEnd(q)}, nil
	}

	if used > limit {
		// Over the limit: return the reservation so a refused request does not
		// permanently consume allowance the candidate never got to use.
		if derr := s.counter.DecrCounter(ctx, key, n); derr != nil {
			s.log.ErrorContext(ctx, "failed to return a refused quota reservation",
				slog.String("quota", string(q)), slog.Any("error", derr))
		}
		return Allowance{}, s.quotaError(q, Allowance{
			Quota: q, Limit: limit, Used: used - n,
			Remaining: max64(limit-(used-n), 0), ResetsAt: s.periodEnd(q),
		})
	}

	// Mirror into Postgres so the counter is reconstructible after a Redis flush.
	// Best-effort: the authoritative decision has already been made.
	if rerr := s.repo.RecordUsage(ctx, userID, q, s.periodStart(q), used, limit); rerr != nil {
		s.log.WarnContext(ctx, "quota usage rollup not updated",
			slog.String("quota", string(q)), slog.Any("error", rerr))
	}

	return Allowance{
		Quota:     q,
		Limit:     limit,
		Used:      used,
		Remaining: max64(limit-used, 0),
		ResetsAt:  s.periodEnd(q),
	}, nil
}

// Release returns n units of a flow quota.
//
// Called when work a reservation was taken for fails. Without it, a failed submission
// permanently costs the candidate a slot they never used — which they will notice, and
// reasonably resent.
func (s *Service) Release(ctx context.Context, userID uuid.UUID, q Quota, n int64) {
	if q.IsLevel() {
		return
	}
	if err := s.counter.DecrCounter(ctx, s.counterKey(userID, q), n); err != nil {
		s.log.ErrorContext(ctx, "failed to release a quota reservation",
			slog.String("quota", string(q)), slog.Any("error", err))
	}
}

// usage returns current consumption for a quota.
func (s *Service) usage(ctx context.Context, userID uuid.UUID, q Quota) (int64, error) {
	if q.IsLevel() {
		// Levels are counted live. A counter would drift the moment a row was deleted
		// by anything other than the path that decremented it.
		return s.repo.CountLevel(ctx, userID, q)
	}

	used, err := s.counter.GetCounter(ctx, s.counterKey(userID, q))
	if err != nil {
		s.log.WarnContext(ctx, "quota counter read failed; using the rollup",
			slog.String("quota", string(q)), slog.Any("error", err))
		return s.repo.UsageFor(ctx, userID, q, s.periodStart(q))
	}
	return used, nil
}

func (s *Service) counterKey(userID uuid.UUID, q Quota) string {
	return fmt.Sprintf("quota:%s:%s:%s", userID, q, s.periodStart(q).Format("20060102"))
}

// periodStart returns the start of the current period in UTC.
//
// UTC rather than the candidate's timezone: a per-candidate day boundary would let
// someone reset their daily allowance by changing their profile timezone.
func (s *Service) periodStart(q Quota) time.Time {
	now := s.now().UTC()
	switch q.Period() {
	case PeriodMonth:
		return time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	case PeriodDay:
		return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	default:
		return time.Time{}
	}
}

func (s *Service) periodEnd(q Quota) time.Time {
	start := s.periodStart(q)
	switch q.Period() {
	case PeriodMonth:
		return start.AddDate(0, 1, 0)
	case PeriodDay:
		return start.AddDate(0, 0, 1)
	default:
		return time.Time{}
	}
}

// periodTTL gives the counter an expiry slightly past the period end, so a key cannot
// outlive its period and leak usage into the next one.
func (s *Service) periodTTL(q Quota) time.Duration {
	end := s.periodEnd(q)
	if end.IsZero() {
		return 24 * time.Hour
	}
	return end.Sub(s.now().UTC()) + time.Hour
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// PostgresRepository reads plans and usage from Postgres.
type PostgresRepository struct{ db *postgres.DB }

// NewPostgresRepository builds the repository.
func NewPostgresRepository(db *postgres.DB) *PostgresRepository {
	return &PostgresRepository{db: db}
}

// PlanForUser resolves the effective plan, defaulting to free.
//
// Only active and trialing subscriptions confer a paid plan. past_due and incomplete
// fall back to free deliberately, so a failed payment cannot keep granting paid
// entitlements indefinitely.
func (r *PostgresRepository) PlanForUser(ctx context.Context, userID uuid.UUID) (Plan, error) {
	const q = `
		SELECT p.slug, p.name, p.quotas, p.features
		FROM plans p
		WHERE p.id = COALESCE(
			(SELECT s.plan_id FROM subscriptions s
			  WHERE s.user_id = $1 AND s.status IN ('active', 'trialing')),
			(SELECT id FROM plans WHERE slug = 'free')
		)`

	var (
		plan                   Plan
		quotasRaw, featuresRaw []byte
	)
	err := r.db.Querier(ctx).QueryRow(ctx, q, userID).
		Scan(&plan.Slug, &plan.Name, &quotasRaw, &featuresRaw)
	if err != nil {
		if postgres.IsNoRows(err) {
			// The free plan is missing from the seed data, which is a deployment fault
			// rather than a user error. Fail loudly rather than inventing limits.
			return Plan{}, apperr.Internalf(err, "no plan resolved for user and no free plan exists")
		}
		return Plan{}, apperr.Internalf(err, "select plan")
	}

	if err := json.Unmarshal(quotasRaw, &plan.Quotas); err != nil {
		return Plan{}, apperr.Internalf(err, "parse plan quotas for %q", plan.Slug)
	}
	if err := json.Unmarshal(featuresRaw, &plan.Features); err != nil {
		return Plan{}, apperr.Internalf(err, "parse plan features for %q", plan.Slug)
	}
	return plan, nil
}

// UsageFor reads the Postgres usage rollup for a period.
func (r *PostgresRepository) UsageFor(ctx context.Context, userID uuid.UUID, quota Quota, periodStart time.Time) (int64, error) {
	const q = `
		SELECT used FROM quota_usage
		WHERE user_id = $1 AND quota_key = $2 AND period_start = $3`

	var used int64
	err := r.db.Querier(ctx).QueryRow(ctx, q, userID, string(quota), periodStart).Scan(&used)
	if err != nil {
		if postgres.IsNoRows(err) {
			return 0, nil
		}
		return 0, apperr.Internalf(err, "select quota usage")
	}
	return used, nil
}

// RecordUsage upserts the usage rollup.
//
// GREATEST on the update so a late-arriving smaller value cannot lower recorded usage:
// with Redis as the fast path and Postgres as the mirror, writes can arrive out of
// order, and the higher figure is always the safe one.
func (r *PostgresRepository) RecordUsage(ctx context.Context, userID uuid.UUID, quota Quota, periodStart time.Time, used, limit int64) error {
	const q = `
		INSERT INTO quota_usage (user_id, quota_key, period_start, used, limit_snapshot)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (user_id, quota_key, period_start) DO UPDATE
		SET used = GREATEST(quota_usage.used, EXCLUDED.used)`

	if _, err := r.db.Querier(ctx).Exec(ctx, q, userID, string(quota), periodStart, used, limit); err != nil {
		return apperr.Internalf(err, "record quota usage")
	}
	return nil
}

// CountLevel counts the live rows backing a level quota.
func (r *PostgresRepository) CountLevel(ctx context.Context, userID uuid.UUID, quota Quota) (int64, error) {
	var query string
	switch quota {
	case QuotaCVSlots:
		query = `SELECT count(*) FROM cv_documents WHERE user_id = $1 AND deleted_at IS NULL`
	case QuotaActiveLoops:
		query = `SELECT count(*) FROM loops WHERE user_id = $1 AND status = 'active'`
	default:
		return 0, apperr.Internalf(
			fmt.Errorf("quota %q is not a level", quota), "entitlement misuse")
	}

	var n int64
	if err := r.db.Querier(ctx).QueryRow(ctx, query, userID).Scan(&n); err != nil {
		return 0, apperr.Internalf(err, "count level quota %q", quota)
	}
	return n, nil
}
