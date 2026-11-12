package jobs

import (
	"context"
	"encoding/json"
	"time"

	"github.com/Joakim-animate90/myjobfyt/backend/internal/apperr"
	"github.com/Joakim-animate90/myjobfyt/backend/internal/platform/postgres"
)

// SourceRepository reads and writes job source configuration and breaker state.
type SourceRepository interface {
	// Sources returns every configured source.
	Sources(ctx context.Context) ([]SourceRecord, error)
	// EnabledSources returns only the sources that may be used.
	EnabledSources(ctx context.Context) ([]SourceRecord, error)
	// SaveBreaker persists a source's circuit state. Called after every attempt, so a
	// restart does not immediately hammer a source that was already failing.
	SaveBreaker(ctx context.Context, slug string, breaker Breaker) error
	// MarkDegraded records that a source is failing often, for admin visibility.
	MarkDegraded(ctx context.Context, slug string, degraded bool) error
}

// PostgresSourceRepository implements SourceRepository.
type PostgresSourceRepository struct {
	db *postgres.DB
}

// NewPostgresSourceRepository builds the repository.
func NewPostgresSourceRepository(db *postgres.DB) *PostgresSourceRepository {
	return &PostgresSourceRepository{db: db}
}

const sourceColumns = `
	slug, name, kind::text, state::text, regions, rate_limit_per_min,
	coalesce(legal_basis, ''), coalesce(terms_url, ''),
	consecutive_failures, breaker_open_until, last_success_at, coalesce(last_error, '')`

// Sources returns every configured source, ordered by slug.
func (r *PostgresSourceRepository) Sources(ctx context.Context) ([]SourceRecord, error) {
	return r.query(ctx, `SELECT `+sourceColumns+` FROM job_sources ORDER BY slug`)
}

// EnabledSources returns the sources that may be queried.
//
// Filtered in SQL rather than in Go so the legal-basis requirement cannot be bypassed
// by a caller that forgets to check. The executor checks again anyway; a rule this
// consequential is worth enforcing at every layer that could skip it.
func (r *PostgresSourceRepository) EnabledSources(ctx context.Context) ([]SourceRecord, error) {
	return r.query(ctx, `
		SELECT `+sourceColumns+`
		FROM job_sources
		WHERE state IN ('enabled', 'degraded')
		  AND legal_basis IS NOT NULL
		  AND length(trim(legal_basis)) > 0
		ORDER BY slug`)
}

func (r *PostgresSourceRepository) query(ctx context.Context, q string) ([]SourceRecord, error) {
	rows, err := r.db.Querier(ctx).Query(ctx, q)
	if err != nil {
		return nil, apperr.Internalf(err, "list job sources")
	}
	defer rows.Close()

	var out []SourceRecord
	for rows.Next() {
		var (
			rec       SourceRecord
			kind      string
			state     string
			openUntil *time.Time
			lastOK    *time.Time
		)
		err := rows.Scan(
			&rec.Slug, &rec.Name, &kind, &state, &rec.Regions, &rec.RateLimit,
			&rec.LegalBasis, &rec.TermsURL,
			&rec.Breaker.ConsecutiveFailures, &openUntil, &lastOK, &rec.Breaker.LastError,
		)
		if err != nil {
			return nil, apperr.Internalf(err, "scan job source")
		}
		rec.Kind = SourceKind(kind)
		rec.State = SourceState(state)
		rec.Breaker.OpenUntil = openUntil
		rec.Breaker.LastSuccessAt = lastOK
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, apperr.Internalf(err, "iterate job sources")
	}
	return out, nil
}

// SaveBreaker persists circuit state.
func (r *PostgresSourceRepository) SaveBreaker(ctx context.Context, slug string, breaker Breaker) error {
	const q = `
		UPDATE job_sources
		SET consecutive_failures = $2,
		    breaker_open_until   = $3,
		    last_success_at      = $4,
		    last_error           = nullif($5, ''),
		    updated_at           = now()
		WHERE slug = $1`

	tag, err := r.db.Querier(ctx).Exec(ctx, q, slug,
		breaker.ConsecutiveFailures, breaker.OpenUntil, breaker.LastSuccessAt, breaker.LastError)
	if err != nil {
		return apperr.Internalf(err, "save breaker state for %s", slug)
	}
	if tag.RowsAffected() == 0 {
		// A breaker written for a source that does not exist means the registry and the
		// database disagree, which is worth surfacing rather than discarding.
		return apperr.NotFound("job source")
	}
	return nil
}

// MarkDegraded moves a source between enabled and degraded.
//
// Never touches disabled or partnership_required: those are deliberate decisions, and a
// breaker tripping must not quietly enable a source somebody switched off.
func (r *PostgresSourceRepository) MarkDegraded(ctx context.Context, slug string, degraded bool) error {
	const q = `
		UPDATE job_sources
		SET state = CASE WHEN $2 THEN 'degraded'::source_state ELSE 'enabled'::source_state END,
		    updated_at = now()
		WHERE slug = $1 AND state IN ('enabled', 'degraded')`

	if _, err := r.db.Querier(ctx).Exec(ctx, q, slug, degraded); err != nil {
		return apperr.Internalf(err, "mark %s degraded", slug)
	}
	return nil
}

// CapabilitiesJSON renders capabilities for the job_sources.capabilities column.
//
// Stored so an operator can see what a source claims without reading the code, and so
// an admin UI can show why a search skipped a source.
func CapabilitiesJSON(caps Capabilities) ([]byte, error) {
	body, err := json.Marshal(caps)
	if err != nil {
		return nil, apperr.Internalf(err, "marshal capabilities")
	}
	return body, nil
}
