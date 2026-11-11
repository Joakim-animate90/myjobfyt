package events

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Joakim-animate90/myjobfyt/backend/internal/platform/postgres"
)

// ClaimState is the outcome of attempting to claim an idempotency key.
type ClaimState string

const (
	// ClaimNew means this is the first attempt; proceed.
	ClaimNew ClaimState = "new"
	// ClaimDone means a previous attempt completed successfully; skip the work.
	ClaimDone ClaimState = "done"
	// ClaimInFlight means a previous attempt claimed the key and never resolved it.
	// The prior attempt's outcome is unknown: it may have crashed before doing
	// anything, or after completing an irreversible side effect. Only the handler
	// knows which assumption is safe, which is why InFlightPolicy exists.
	ClaimInFlight ClaimState = "in_flight"
)

// IdempotencyStore records which units of work have been attempted and completed.
//
// This is the backbone of at-least-once safety. Without it, every redelivery repeats
// every side effect, and JetStream's own duplicate window (two minutes) is far too
// short to protect a pipeline whose stages legitimately take longer than that.
type IdempotencyStore interface {
	// Claim atomically records an attempt and reports what was already known.
	Claim(ctx context.Context, key, consumer string, leaseFor time.Duration) (ClaimState, error)
	// Complete marks the work finished. Subsequent claims return ClaimDone.
	Complete(ctx context.Context, key string) error
	// Release removes the claim so a retry can proceed. Called only when the handler
	// failed *before* performing any side effect.
	Release(ctx context.Context, key string) error
}

// PostgresIdempotencyStore stores claims in the processed_messages table.
//
// Postgres rather than Redis, deliberately: a flushed or evicted Redis key would
// silently re-enable a duplicate side effect, and "we submitted the same application
// twice" is not an acceptable failure mode for a candidate's job search.
type PostgresIdempotencyStore struct {
	db  *postgres.DB
	ttl time.Duration
}

// NewPostgresIdempotencyStore builds the store. retention bounds how long completed
// keys are kept; it must exceed the longest stream retention so a replayed message
// cannot outlive the record that would have suppressed it.
func NewPostgresIdempotencyStore(db *postgres.DB, retention time.Duration) *PostgresIdempotencyStore {
	if retention <= 0 {
		retention = 45 * 24 * time.Hour
	}
	return &PostgresIdempotencyStore{db: db, ttl: retention}
}

// Claim inserts the key, or reports the existing state.
//
// The expired-lease case is the subtle one: an in_flight row whose lease has elapsed
// means the worker that held it is gone. Taking the claim over is what stops one
// crashed worker from blocking a message forever.
func (s *PostgresIdempotencyStore) Claim(ctx context.Context, key, consumer string, leaseFor time.Duration) (ClaimState, error) {
	const q = `
		INSERT INTO processed_messages (idempotency_key, consumer, state, lease_expires_at, expires_at)
		VALUES ($1, $2, 'in_flight', now() + $3::interval, now() + $4::interval)
		ON CONFLICT (idempotency_key) DO UPDATE
		SET consumer         = EXCLUDED.consumer,
		    lease_expires_at = EXCLUDED.lease_expires_at,
		    attempts         = processed_messages.attempts + 1
		WHERE processed_messages.state = 'in_flight'
		  AND processed_messages.lease_expires_at < now()
		RETURNING state`

	var state string
	err := s.db.Querier(ctx).QueryRow(ctx, q,
		key, consumer,
		fmt.Sprintf("%d milliseconds", leaseFor.Milliseconds()),
		fmt.Sprintf("%d milliseconds", s.ttl.Milliseconds()),
	).Scan(&state)

	switch {
	case err == nil:
		// Either a fresh insert or a reclaimed expired lease; both mean "proceed".
		return ClaimNew, nil

	case postgres.IsNoRows(err):
		// The row exists and the WHERE clause blocked the update, so read why.
		return s.readState(ctx, key)

	default:
		return "", fmt.Errorf("claim idempotency key: %w", err)
	}
}

func (s *PostgresIdempotencyStore) readState(ctx context.Context, key string) (ClaimState, error) {
	const q = `SELECT state FROM processed_messages WHERE idempotency_key = $1`
	var state string
	if err := s.db.Querier(ctx).QueryRow(ctx, q, key).Scan(&state); err != nil {
		if postgres.IsNoRows(err) {
			// Raced with a retention sweep; treating it as new is correct because the
			// record that would have suppressed it is gone either way.
			return ClaimNew, nil
		}
		return "", fmt.Errorf("read idempotency state: %w", err)
	}
	switch state {
	case "done":
		return ClaimDone, nil
	case "in_flight":
		return ClaimInFlight, nil
	default:
		return "", fmt.Errorf("unknown idempotency state %q for key %q", state, key)
	}
}

// Complete marks the key done.
func (s *PostgresIdempotencyStore) Complete(ctx context.Context, key string) error {
	const q = `
		UPDATE processed_messages
		SET state = 'done', lease_expires_at = NULL, processed_at = now()
		WHERE idempotency_key = $1`
	if _, err := s.db.Querier(ctx).Exec(ctx, q, key); err != nil {
		return fmt.Errorf("complete idempotency key: %w", err)
	}
	return nil
}

// Release deletes an in-flight claim so the message can be retried immediately.
// It refuses to delete a completed row, because that would re-enable a duplicate.
func (s *PostgresIdempotencyStore) Release(ctx context.Context, key string) error {
	const q = `DELETE FROM processed_messages WHERE idempotency_key = $1 AND state = 'in_flight'`
	if _, err := s.db.Querier(ctx).Exec(ctx, q, key); err != nil {
		return fmt.Errorf("release idempotency key: %w", err)
	}
	return nil
}

// PurgeExpired deletes records past their retention. Run from the scheduler.
func (s *PostgresIdempotencyStore) PurgeExpired(ctx context.Context, limit int) (int64, error) {
	const q = `
		DELETE FROM processed_messages
		WHERE idempotency_key IN (
		  SELECT idempotency_key FROM processed_messages
		  WHERE expires_at < now() AND state = 'done'
		  LIMIT $1
		)`
	tag, err := s.db.Querier(ctx).Exec(ctx, q, limit)
	if err != nil {
		return 0, fmt.Errorf("purge expired idempotency keys: %w", err)
	}
	return tag.RowsAffected(), nil
}

// ErrOutcomeUnknown is returned by a handler that found its idempotency key in flight
// and cannot safely determine whether its side effect already happened.
//
// It is deliberately terminal: nothing retries it automatically. The whole point is to
// surface uncertainty to a human instead of resolving it by guessing.
var ErrOutcomeUnknown = errors.New("previous attempt outcome unknown; not repeating a side effect")
