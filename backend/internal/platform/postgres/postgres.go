// Package postgres owns the connection pool and the transaction manager.
//
// The transaction manager passes a transaction through context, so a service can
// declare a transaction boundary without every repository method growing a tx
// parameter. Repositories call Querier(ctx) and transparently get either the ambient
// transaction or the pool. The alternative — threading pgx.Tx through every signature —
// makes the boundary explicit at the cost of making it impossible to compose
// repositories, which is the thing transactions exist for.
package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Joakim-animate90/myjobfyt/backend/internal/config"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Querier is the subset of pgx used by repositories. Both *pgxpool.Pool and pgx.Tx
// satisfy it, which is what lets Querier(ctx) return either transparently.
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// DB wraps the pool and provides transaction management.
type DB struct {
	pool *pgxpool.Pool
}

type txKey struct{}

// Open creates and verifies the connection pool.
func Open(ctx context.Context, cfg config.PostgresConfig) (*DB, error) {
	poolCfg, err := pgxpool.ParseConfig(cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("parse postgres dsn: %w", err)
	}

	poolCfg.MaxConns = cfg.MaxConns
	poolCfg.MinConns = cfg.MinConns
	poolCfg.MaxConnLifetime = cfg.MaxConnLifetime
	poolCfg.MaxConnIdleTime = cfg.MaxConnIdleTime
	poolCfg.ConnConfig.ConnectTimeout = cfg.ConnectTimeout

	// A per-session statement timeout bounds the damage one pathological query can do
	// to the whole pool. Set on the connection rather than per query so it cannot be
	// forgotten at a call site.
	if cfg.StatementTimeout > 0 {
		if poolCfg.ConnConfig.RuntimeParams == nil {
			poolCfg.ConnConfig.RuntimeParams = map[string]string{}
		}
		poolCfg.ConnConfig.RuntimeParams["statement_timeout"] =
			fmt.Sprintf("%d", cfg.StatementTimeout.Milliseconds())
	}

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("create postgres pool: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, cfg.ConnectTimeout)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}

	return &DB{pool: pool}, nil
}

// Pool exposes the underlying pool for health checks and migrations.
func (db *DB) Pool() *pgxpool.Pool { return db.pool }

// Close drains the pool.
func (db *DB) Close() { db.pool.Close() }

// Ping verifies connectivity for readiness probes.
func (db *DB) Ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return db.pool.Ping(ctx)
}

// Querier returns the ambient transaction if one is in scope, otherwise the pool.
// Repositories always call this rather than touching the pool directly.
func (db *DB) Querier(ctx context.Context) Querier {
	if tx, ok := ctx.Value(txKey{}).(pgx.Tx); ok {
		return tx
	}
	return db.pool
}

// InTx runs fn inside a transaction.
//
// A nested InTx joins the existing transaction rather than opening a second one:
// Postgres has no true nested transactions, and savepoint semantics would make a
// partial rollback silently differ from what the caller expects.
func (db *DB) InTx(ctx context.Context, fn func(ctx context.Context) error) error {
	if _, ok := ctx.Value(txKey{}).(pgx.Tx); ok {
		return fn(ctx)
	}

	tx, err := db.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}

	committed := false
	defer func() {
		if !committed {
			// Use a fresh context: if ctx is already cancelled, a rollback issued on
			// it is dropped and the connection is returned to the pool mid-transaction.
			rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			_ = tx.Rollback(rollbackCtx)
		}
	}()

	if err := fn(context.WithValue(ctx, txKey{}, tx)); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	committed = true
	return nil
}

// InTxSerializable runs fn at SERIALIZABLE isolation, for the few operations where a
// read-then-write race would corrupt an invariant — quota reservation and the
// application authorisation check being the two that matter.
func (db *DB) InTxSerializable(ctx context.Context, fn func(ctx context.Context) error) error {
	if _, ok := ctx.Value(txKey{}).(pgx.Tx); ok {
		return fn(ctx)
	}

	tx, err := db.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return fmt.Errorf("begin serializable transaction: %w", err)
	}

	committed := false
	defer func() {
		if !committed {
			rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			_ = tx.Rollback(rollbackCtx)
		}
	}()

	if err := fn(context.WithValue(ctx, txKey{}, tx)); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit serializable transaction: %w", err)
	}
	committed = true
	return nil
}

// Postgres error codes we interpret rather than pass through.
const (
	codeUniqueViolation     = "23505"
	codeForeignKeyViolation = "23503"
	codeCheckViolation      = "23514"
	codeNotNullViolation    = "23502"
	codeSerializationFail   = "40001"
	codeDeadlockDetected    = "40P01"
	codeLockNotAvailable    = "55P03"
	codeQueryCanceled       = "57014"
)

// IsNoRows reports whether err is pgx's no-rows sentinel.
func IsNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }

func pgCode(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

// IsUniqueViolation reports a unique-constraint violation. When constraint is
// non-empty, it must also match, so a caller can distinguish "email already taken"
// from any other unique index on the same table.
func IsUniqueViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != codeUniqueViolation {
		return false
	}
	return constraint == "" || pgErr.ConstraintName == constraint
}

// IsForeignKeyViolation reports a foreign-key violation.
func IsForeignKeyViolation(err error) bool { return pgCode(err) == codeForeignKeyViolation }

// IsCheckViolation reports a check-constraint violation. These represent a broken
// domain invariant, so a caller should treat one as a bug rather than as bad input.
func IsCheckViolation(err error) bool { return pgCode(err) == codeCheckViolation }

// IsNotNullViolation reports a not-null violation.
func IsNotNullViolation(err error) bool { return pgCode(err) == codeNotNullViolation }

// IsRetryable reports whether the error is a transient concurrency failure that will
// plausibly succeed on an immediate retry. Serialization failures and deadlocks are
// the expected cost of SERIALIZABLE isolation, not bugs.
func IsRetryable(err error) bool {
	switch pgCode(err) {
	case codeSerializationFail, codeDeadlockDetected, codeLockNotAvailable:
		return true
	default:
		return false
	}
}

// IsQueryCanceled reports a statement-timeout or cancellation.
func IsQueryCanceled(err error) bool { return pgCode(err) == codeQueryCanceled }

// ConstraintName returns the violated constraint, or "".
func ConstraintName(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.ConstraintName
	}
	return ""
}

// RetryOnConflict runs fn, retrying serialization failures and deadlocks with a short
// backoff. Used with InTxSerializable where the transaction body is idempotent.
func RetryOnConflict(ctx context.Context, attempts int, fn func(ctx context.Context) error) error {
	if attempts < 1 {
		attempts = 1
	}
	var err error
	for i := 0; i < attempts; i++ {
		err = fn(ctx)
		if err == nil || !IsRetryable(err) {
			return err
		}
		// Linear backoff with a small base: these conflicts resolve in microseconds,
		// so waiting long enough to matter would cost more than the retry saves.
		delay := time.Duration(i+1) * 5 * time.Millisecond
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
	}
	return fmt.Errorf("exhausted %d attempts on a retryable conflict: %w", attempts, err)
}
