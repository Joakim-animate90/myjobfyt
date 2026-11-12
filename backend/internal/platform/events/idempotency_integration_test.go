//go:build integration

package events_test

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/Joakim-animate90/myjobfyt/backend/internal/config"
	"github.com/Joakim-animate90/myjobfyt/backend/internal/platform/events"
	"github.com/Joakim-animate90/myjobfyt/backend/internal/platform/postgres"
	"github.com/google/uuid"
)

func openDB(t *testing.T) *postgres.DB {
	t.Helper()
	dsn := os.Getenv("POSTGRES_DSN")
	if dsn == "" {
		t.Skip("POSTGRES_DSN not set; run with the development stack up")
	}
	db, err := postgres.Open(context.Background(), config.PostgresConfig{
		// Several connections, because the concurrency tests below need genuinely
		// parallel claims rather than ones serialised by a single pooled connection.
		DSN: dsn, MaxConns: 8, MinConns: 2, ConnectTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	t.Cleanup(db.Close)
	return db
}

// newKey returns a key unique to this test run, so tests do not collide and no cleanup
// is needed between them.
func newKey(prefix string) string {
	return fmt.Sprintf("%s:%s", prefix, uuid.New())
}

func cleanupKey(t *testing.T, db *postgres.DB, key string) {
	t.Helper()
	t.Cleanup(func() {
		_, _ = db.Querier(context.Background()).Exec(context.Background(),
			`DELETE FROM processed_messages WHERE idempotency_key = $1`, key)
	})
}

func TestAFirstAttemptIsNew(t *testing.T) {
	db := openDB(t)
	store := events.NewPostgresIdempotencyStore(db, time.Hour)
	key := newKey("first")
	cleanupKey(t, db, key)

	state, err := store.Claim(context.Background(), key, "test-consumer", time.Minute)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if state != events.ClaimNew {
		t.Fatalf("state = %q, want %q", state, events.ClaimNew)
	}
}

func TestARedeliveryAfterCompletionIsSuppressed(t *testing.T) {
	// This is the property that stops a redelivery duplicating derived rows. JetStream
	// is at-least-once, so this path runs whenever an ack is lost — not only after a
	// crash.
	db := openDB(t)
	store := events.NewPostgresIdempotencyStore(db, time.Hour)
	ctx := context.Background()
	key := newKey("done")
	cleanupKey(t, db, key)

	if _, err := store.Claim(ctx, key, "test-consumer", time.Minute); err != nil {
		t.Fatalf("first Claim: %v", err)
	}
	if err := store.Complete(ctx, key); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	state, err := store.Claim(ctx, key, "test-consumer", time.Minute)
	if err != nil {
		t.Fatalf("second Claim: %v", err)
	}
	if state != events.ClaimDone {
		t.Fatalf("state = %q, want %q; the handler would redo completed work", state, events.ClaimDone)
	}
}

func TestAnUnresolvedClaimWithinItsLeaseIsInFlight(t *testing.T) {
	// A second worker picking up a redelivery while the first is still working must not
	// be told to proceed. What it does instead is the handler's choice, which is why
	// this state is distinct from both new and done.
	db := openDB(t)
	store := events.NewPostgresIdempotencyStore(db, time.Hour)
	ctx := context.Background()
	key := newKey("inflight")
	cleanupKey(t, db, key)

	if _, err := store.Claim(ctx, key, "worker-a", time.Minute); err != nil {
		t.Fatalf("first Claim: %v", err)
	}

	state, err := store.Claim(ctx, key, "worker-b", time.Minute)
	if err != nil {
		t.Fatalf("second Claim: %v", err)
	}
	if state != events.ClaimInFlight {
		t.Fatalf("state = %q, want %q", state, events.ClaimInFlight)
	}
}

func TestAnExpiredLeaseCanBeReclaimed(t *testing.T) {
	// The crash-recovery path. A worker that died holding a claim must not block the
	// message for ever; once its lease elapses another worker takes over.
	db := openDB(t)
	store := events.NewPostgresIdempotencyStore(db, time.Hour)
	ctx := context.Background()
	key := newKey("expired")
	cleanupKey(t, db, key)

	// A lease that has already elapsed stands in for a worker that crashed.
	if _, err := store.Claim(ctx, key, "crashed-worker", -time.Minute); err != nil {
		t.Fatalf("first Claim: %v", err)
	}

	state, err := store.Claim(ctx, key, "recovering-worker", time.Minute)
	if err != nil {
		t.Fatalf("second Claim: %v", err)
	}
	if state != events.ClaimNew {
		t.Fatalf("state = %q, want %q; a crashed worker would block this message for ever",
			state, events.ClaimNew)
	}

	// Reclaiming counts the attempt rather than resetting it, so a message that keeps
	// killing its worker is visible as such rather than looking perpetually fresh.
	var attempts int
	err = db.Querier(ctx).QueryRow(ctx,
		`SELECT attempts FROM processed_messages WHERE idempotency_key = $1`, key).Scan(&attempts)
	if err != nil {
		t.Fatalf("read attempts: %v", err)
	}
	if attempts < 1 {
		t.Errorf("attempts = %d after a reclaim, want at least 1", attempts)
	}

	// And the new holder is recorded, so an operator can see which worker has it.
	var consumer string
	err = db.Querier(ctx).QueryRow(ctx,
		`SELECT consumer FROM processed_messages WHERE idempotency_key = $1`, key).Scan(&consumer)
	if err != nil {
		t.Fatalf("read consumer: %v", err)
	}
	if consumer != "recovering-worker" {
		t.Errorf("consumer = %q, want the worker that took the claim over", consumer)
	}
}

func TestACompletedClaimIsNeverReclaimedWhateverTheLease(t *testing.T) {
	// Complete nulls the lease. If an expired-lease reclaim could also match a done
	// row, every completed key would become reclaimable and the whole suppression
	// mechanism would quietly stop working.
	db := openDB(t)
	store := events.NewPostgresIdempotencyStore(db, time.Hour)
	ctx := context.Background()
	key := newKey("done-not-reclaimable")
	cleanupKey(t, db, key)

	if _, err := store.Claim(ctx, key, "worker-a", -time.Hour); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := store.Complete(ctx, key); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	state, err := store.Claim(ctx, key, "worker-b", time.Minute)
	if err != nil {
		t.Fatalf("Claim after Complete: %v", err)
	}
	if state != events.ClaimDone {
		t.Fatalf("state = %q, want %q; completed work became repeatable", state, events.ClaimDone)
	}
}

func TestReleaseLetsARetryProceed(t *testing.T) {
	// Called when a handler failed before any side effect. Without it the message would
	// wait out the whole lease before anything could retry it.
	db := openDB(t)
	store := events.NewPostgresIdempotencyStore(db, time.Hour)
	ctx := context.Background()
	key := newKey("released")
	cleanupKey(t, db, key)

	if _, err := store.Claim(ctx, key, "worker-a", time.Hour); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := store.Release(ctx, key); err != nil {
		t.Fatalf("Release: %v", err)
	}

	state, err := store.Claim(ctx, key, "worker-a", time.Minute)
	if err != nil {
		t.Fatalf("Claim after Release: %v", err)
	}
	if state != events.ClaimNew {
		t.Fatalf("state = %q, want %q", state, events.ClaimNew)
	}
}

func TestReleaseRefusesToUndoACompletedClaim(t *testing.T) {
	// A Release reaching a completed row would delete the record suppressing a
	// duplicate, re-enabling the side effect it exists to prevent.
	db := openDB(t)
	store := events.NewPostgresIdempotencyStore(db, time.Hour)
	ctx := context.Background()
	key := newKey("release-vs-done")
	cleanupKey(t, db, key)

	if _, err := store.Claim(ctx, key, "worker-a", time.Minute); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := store.Complete(ctx, key); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	// Not an error: Release is best-effort cleanup, it simply must not take effect.
	if err := store.Release(ctx, key); err != nil {
		t.Fatalf("Release: %v", err)
	}

	state, err := store.Claim(ctx, key, "worker-a", time.Minute)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if state != events.ClaimDone {
		t.Fatalf("state = %q, want %q; Release erased a completed claim", state, events.ClaimDone)
	}
}

func TestOnlyOneOfManyConcurrentClaimsProceeds(t *testing.T) {
	// The real shape of the problem: several worker replicas share a durable consumer,
	// so a redelivery can land on a different replica while the first still holds the
	// claim. Exactly one must be told to proceed. A check-then-insert would let several
	// through here; the atomic upsert is what makes this hold.
	db := openDB(t)
	store := events.NewPostgresIdempotencyStore(db, time.Hour)
	key := newKey("concurrent")
	cleanupKey(t, db, key)

	const racers = 8
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		states  = map[events.ClaimState]int{}
		errs    []error
		barrier = make(chan struct{})
	)

	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			<-barrier // release them together
			state, err := store.Claim(context.Background(), key,
				fmt.Sprintf("worker-%d", n), time.Minute)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			states[state]++
		}(i)
	}

	close(barrier)
	wg.Wait()

	for _, err := range errs {
		t.Errorf("Claim: %v", err)
	}
	if states[events.ClaimNew] != 1 {
		t.Fatalf("%d of %d concurrent claims were told to proceed, want exactly 1: %v",
			states[events.ClaimNew], racers, states)
	}
	if states[events.ClaimInFlight] != racers-1 {
		t.Errorf("in-flight = %d, want %d", states[events.ClaimInFlight], racers-1)
	}
}

func TestConcurrentClaimsOnAnExpiredLeaseStillAdmitOnlyOne(t *testing.T) {
	// The nastier race: a worker crashed, its lease elapsed, and several replicas
	// notice at once. Taking over must be as exclusive as claiming fresh, or a crash
	// turns into several workers redoing the same side effect simultaneously.
	db := openDB(t)
	store := events.NewPostgresIdempotencyStore(db, time.Hour)
	key := newKey("concurrent-expired")
	cleanupKey(t, db, key)

	if _, err := store.Claim(context.Background(), key, "crashed", -time.Hour); err != nil {
		t.Fatalf("seed claim: %v", err)
	}

	const racers = 8
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		states  = map[events.ClaimState]int{}
		barrier = make(chan struct{})
	)

	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			<-barrier
			state, err := store.Claim(context.Background(), key,
				fmt.Sprintf("recovering-%d", n), time.Minute)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				t.Errorf("Claim: %v", err)
				return
			}
			states[state]++
		}(i)
	}

	close(barrier)
	wg.Wait()

	if states[events.ClaimNew] != 1 {
		t.Fatalf("%d workers took over the same expired claim, want exactly 1: %v",
			states[events.ClaimNew], states)
	}
}

func TestPurgeRemovesOnlyCompletedRecordsPastRetention(t *testing.T) {
	// Retention must exceed the longest stream retention: a replayed message that
	// outlived the record suppressing it would be processed a second time. Purging too
	// eagerly is therefore a correctness bug, not a housekeeping preference.
	db := openDB(t)
	ctx := context.Background()
	store := events.NewPostgresIdempotencyStore(db, 45*24*time.Hour)

	liveKey := newKey("purge-live")
	staleKey := newKey("purge-stale")
	cleanupKey(t, db, liveKey)
	cleanupKey(t, db, staleKey)

	for _, key := range []string{liveKey, staleKey} {
		if _, err := store.Claim(ctx, key, "worker", time.Minute); err != nil {
			t.Fatalf("Claim %s: %v", key, err)
		}
		if err := store.Complete(ctx, key); err != nil {
			t.Fatalf("Complete %s: %v", key, err)
		}
	}

	// Aged directly, because the constructor deliberately clamps a non-positive
	// retention to its 45-day default and so cannot be used to write an expired row.
	expire(t, db, staleKey)

	if _, err := store.PurgeExpired(ctx, 1000); err != nil {
		t.Fatalf("PurgeExpired: %v", err)
	}

	if !keyExists(t, db, liveKey) {
		t.Error("purge deleted a completed record still within retention; a replay would be reprocessed")
	}
	if keyExists(t, db, staleKey) {
		t.Error("purge left a completed record past its retention")
	}
}

func TestPurgeKeepsUnresolvedRecordsHoweverOld(t *testing.T) {
	// Purge is restricted to completed records on purpose. An in-flight row past its
	// retention is a claim some worker took and never resolved, and that row is the
	// only evidence the attempt happened at all. Deleting it would discard exactly what
	// an operator needs in order to find out what went wrong.
	//
	// The trade-off is that such rows are never removed by the sweep. They are bounded
	// by how often a worker dies mid-handler, which is rare and worth noticing.
	db := openDB(t)
	ctx := context.Background()
	store := events.NewPostgresIdempotencyStore(db, 45*24*time.Hour)

	key := newKey("purge-unresolved")
	cleanupKey(t, db, key)

	if _, err := store.Claim(ctx, key, "crashed-worker", time.Minute); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	expire(t, db, key) // left in_flight, aged past retention

	if _, err := store.PurgeExpired(ctx, 1000); err != nil {
		t.Fatalf("PurgeExpired: %v", err)
	}

	if !keyExists(t, db, key) {
		t.Error("purge deleted an unresolved claim, discarding the evidence that the " +
			"attempt ever happened")
	}
}

func TestPurgeRespectsItsLimit(t *testing.T) {
	// Purging runs against a table with a row per processed message. An unbounded
	// delete would lock it long enough to stall the pipeline it serves.
	db := openDB(t)
	ctx := context.Background()
	store := events.NewPostgresIdempotencyStore(db, 45*24*time.Hour)

	for i := 0; i < 3; i++ {
		key := newKey("purge-limit")
		cleanupKey(t, db, key)
		if _, err := store.Claim(ctx, key, "worker", time.Minute); err != nil {
			t.Fatalf("Claim: %v", err)
		}
		if err := store.Complete(ctx, key); err != nil {
			t.Fatalf("Complete: %v", err)
		}
		expire(t, db, key)
	}

	deleted, err := store.PurgeExpired(ctx, 2)
	if err != nil {
		t.Fatalf("PurgeExpired: %v", err)
	}
	if deleted > 2 {
		t.Fatalf("purge deleted %d rows, want at most the limit of 2", deleted)
	}
}

func TestRetentionIsNeverZeroOrNegative(t *testing.T) {
	// A zero or negative retention would write records already expired, so the first
	// purge would delete the very records suppressing duplicates and every redelivery
	// would be reprocessed. The constructor clamps rather than trusting its caller.
	db := openDB(t)
	ctx := context.Background()

	for name, retention := range map[string]time.Duration{
		"zero":     0,
		"negative": -time.Hour,
	} {
		t.Run(name, func(t *testing.T) {
			key := newKey("retention-" + name)
			cleanupKey(t, db, key)

			store := events.NewPostgresIdempotencyStore(db, retention)
			if _, err := store.Claim(ctx, key, "worker", time.Minute); err != nil {
				t.Fatalf("Claim: %v", err)
			}
			if err := store.Complete(ctx, key); err != nil {
				t.Fatalf("Complete: %v", err)
			}

			var expired bool
			err := db.Querier(ctx).QueryRow(ctx,
				`SELECT expires_at < now() FROM processed_messages WHERE idempotency_key = $1`,
				key).Scan(&expired)
			if err != nil {
				t.Fatalf("read expires_at: %v", err)
			}
			if expired {
				t.Fatalf("a %s retention wrote an already-expired record", name)
			}
		})
	}
}

// expire ages a record past its retention. Done in SQL because the constructor clamps a
// non-positive retention to its default, so an expired row cannot be produced through
// the store's own API.
func expire(t *testing.T, db *postgres.DB, key string) {
	t.Helper()
	_, err := db.Querier(context.Background()).Exec(context.Background(),
		`UPDATE processed_messages SET expires_at = now() - interval '1 hour'
		 WHERE idempotency_key = $1`, key)
	if err != nil {
		t.Fatalf("expire %s: %v", key, err)
	}
}

func keyExists(t *testing.T, db *postgres.DB, key string) bool {
	t.Helper()
	var exists bool
	err := db.Querier(context.Background()).QueryRow(context.Background(),
		`SELECT EXISTS (SELECT 1 FROM processed_messages WHERE idempotency_key = $1)`, key).Scan(&exists)
	if err != nil {
		t.Fatalf("check key: %v", err)
	}
	return exists
}
