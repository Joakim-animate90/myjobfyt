//go:build integration

package jobs_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/Joakim-animate90/myjobfyt/backend/internal/apperr"
	"github.com/Joakim-animate90/myjobfyt/backend/internal/config"
	"github.com/Joakim-animate90/myjobfyt/backend/internal/domain/jobs"
	"github.com/Joakim-animate90/myjobfyt/backend/internal/platform/postgres"
)

func openDB(t *testing.T) *postgres.DB {
	t.Helper()
	dsn := os.Getenv("POSTGRES_DSN")
	if dsn == "" {
		t.Skip("POSTGRES_DSN not set; run with the development stack up")
	}
	db, err := postgres.Open(context.Background(), config.PostgresConfig{
		DSN: dsn, MaxConns: 4, MinConns: 1, ConnectTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	t.Cleanup(db.Close)
	return db
}

// seedSource inserts a source and removes it afterwards, so a test never depends on or
// disturbs the reference data the migration ships.
func seedSource(t *testing.T, db *postgres.DB, slug string, state jobs.SourceState, legalBasis string) {
	t.Helper()
	ctx := context.Background()

	_, err := db.Querier(ctx).Exec(ctx, `
		INSERT INTO job_sources (slug, name, kind, state, regions, rate_limit_per_min, legal_basis)
		VALUES ($1, $2, 'official_api', $3, '{GB,US}', 20, nullif($4, ''))`,
		slug, slug, string(state), legalBasis)
	if err != nil {
		t.Fatalf("seed source %s: %v", slug, err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = db.Querier(bg).Exec(bg, `DELETE FROM job_sources WHERE slug = $1`, slug)
	})
}

func uniqueSlug(prefix string) string {
	return prefix + "-" + time.Now().UTC().Format("150405.000000000")
}

func TestBreakerStateSurvivesARestart(t *testing.T) {
	// The reason the breaker lives in Postgres rather than in memory. A process that
	// forgets the circuit was open sends a full run's traffic at a source that was
	// already returning errors, which is how a rate-limit ban starts.
	db := openDB(t)
	repo := jobs.NewPostgresSourceRepository(db)
	ctx := context.Background()

	slug := uniqueSlug("restart")
	seedSource(t, db, slug, jobs.StateEnabled, "documented API terms")

	cfg := jobs.BreakerConfig{FailureThreshold: 3, BaseCooldown: 5 * time.Minute, MaxCooldown: time.Hour}
	breaker := jobs.Breaker{}
	now := time.Now().UTC()
	for i := 0; i < cfg.FailureThreshold; i++ {
		breaker = breaker.RecordFailure(now, cfg, errContext("503 service unavailable"))
	}
	if breaker.OpenUntil == nil {
		t.Fatal("the fixture did not open the circuit")
	}

	if err := repo.SaveBreaker(ctx, slug, breaker); err != nil {
		t.Fatalf("SaveBreaker: %v", err)
	}

	// A fresh repository, as a restarted process would have.
	restored := findSource(t, jobs.NewPostgresSourceRepository(db), slug)

	if restored.Breaker.ConsecutiveFailures != cfg.FailureThreshold {
		t.Errorf("consecutive failures = %d, want %d",
			restored.Breaker.ConsecutiveFailures, cfg.FailureThreshold)
	}
	if restored.Breaker.OpenUntil == nil {
		t.Fatal("OpenUntil was not persisted; a restart would hammer the failing source")
	}
	if !restored.Breaker.OpenUntil.Equal(breaker.OpenUntil.Truncate(time.Microsecond)) &&
		restored.Breaker.OpenUntil.Sub(*breaker.OpenUntil).Abs() > time.Millisecond {
		t.Errorf("OpenUntil = %s, want %s", restored.Breaker.OpenUntil, breaker.OpenUntil)
	}
	if restored.Breaker.LastError == "" {
		t.Error("LastError was not persisted, so an operator cannot see why the circuit opened")
	}

	// And the restored state still refuses traffic.
	if restored.Breaker.Allows(now, cfg) {
		t.Error("the restored breaker allowed a request while the circuit was open")
	}
}

func TestASuccessClearsThePersistedCircuit(t *testing.T) {
	db := openDB(t)
	repo := jobs.NewPostgresSourceRepository(db)
	ctx := context.Background()

	slug := uniqueSlug("recover")
	seedSource(t, db, slug, jobs.StateEnabled, "documented API terms")

	cfg := jobs.DefaultBreakerConfig()
	now := time.Now().UTC()
	breaker := jobs.Breaker{}
	for i := 0; i < cfg.FailureThreshold; i++ {
		breaker = breaker.RecordFailure(now, cfg, errContext("down"))
	}
	if err := repo.SaveBreaker(ctx, slug, breaker); err != nil {
		t.Fatalf("SaveBreaker (open): %v", err)
	}

	if err := repo.SaveBreaker(ctx, slug, breaker.RecordSuccess(now)); err != nil {
		t.Fatalf("SaveBreaker (recovered): %v", err)
	}

	restored := findSource(t, repo, slug)
	if restored.Breaker.OpenUntil != nil {
		t.Error("the circuit is still open after a recorded success")
	}
	if restored.Breaker.ConsecutiveFailures != 0 {
		t.Errorf("consecutive failures = %d after a success", restored.Breaker.ConsecutiveFailures)
	}
	if restored.Breaker.LastError != "" {
		t.Errorf("LastError = %q after a success", restored.Breaker.LastError)
	}
	if restored.Breaker.LastSuccessAt == nil {
		t.Error("LastSuccessAt was not persisted")
	}
}

func TestEnabledSourcesExcludesOnesWithNoLegalBasis(t *testing.T) {
	// Enforced in SQL as well as in the executor. A rule this consequential — it is the
	// difference between an integration and a scraping incident — is worth enforcing at
	// every layer that could skip it.
	db := openDB(t)
	repo := jobs.NewPostgresSourceRepository(db)
	ctx := context.Background()

	withBasis := uniqueSlug("vetted")
	noBasis := uniqueSlug("unvetted")
	seedSource(t, db, withBasis, jobs.StateEnabled, "documented API terms")
	// state must be disabled: the database refuses an enabled source with no basis, and
	// that constraint is itself the first line of this defence.
	seedSource(t, db, noBasis, jobs.StateDisabled, "")

	enabled, err := repo.EnabledSources(ctx)
	if err != nil {
		t.Fatalf("EnabledSources: %v", err)
	}

	var sawVetted, sawUnvetted bool
	for _, source := range enabled {
		switch source.Slug {
		case withBasis:
			sawVetted = true
		case noBasis:
			sawUnvetted = true
		}
	}
	if !sawVetted {
		t.Error("a properly configured source was excluded")
	}
	if sawUnvetted {
		t.Error("a source with no legal basis was returned as enabled")
	}
}

func TestTheDatabaseRefusesToEnableASourceWithNoLegalBasis(t *testing.T) {
	// The constraint, independently of the repository. A future code path that forgets
	// the check must still fail.
	db := openDB(t)
	ctx := context.Background()
	slug := uniqueSlug("direct")

	_, err := db.Querier(ctx).Exec(ctx, `
		INSERT INTO job_sources (slug, name, kind, state, legal_basis)
		VALUES ($1, $1, 'career_page', 'enabled', NULL)`, slug)
	if err == nil {
		_, _ = db.Querier(ctx).Exec(ctx, `DELETE FROM job_sources WHERE slug = $1`, slug)
		t.Fatal("the database enabled a source with no recorded legal basis")
	}
	if name := postgres.ConstraintName(err); name != "job_sources_enabled_requires_legal_basis" {
		t.Errorf("violated constraint = %q, want job_sources_enabled_requires_legal_basis", name)
	}

	// Whitespace must not satisfy it either.
	_, err = db.Querier(ctx).Exec(ctx, `
		INSERT INTO job_sources (slug, name, kind, state, legal_basis)
		VALUES ($1, $1, 'career_page', 'enabled', '   ')`, slug)
	if err == nil {
		_, _ = db.Querier(ctx).Exec(ctx, `DELETE FROM job_sources WHERE slug = $1`, slug)
		t.Error("whitespace was accepted as a legal basis")
	}
}

func TestADegradedSourceIsStillReturnedForUse(t *testing.T) {
	// Degraded means failing often, not unusable. Excluding it would turn a flaky
	// source into no source at all, which is worse for the candidate than some results.
	db := openDB(t)
	repo := jobs.NewPostgresSourceRepository(db)
	slug := uniqueSlug("flaky")
	seedSource(t, db, slug, jobs.StateDegraded, "documented API terms")

	enabled, err := repo.EnabledSources(context.Background())
	if err != nil {
		t.Fatalf("EnabledSources: %v", err)
	}
	for _, source := range enabled {
		if source.Slug == slug {
			return
		}
	}
	t.Error("a degraded source was excluded from use")
}

func TestMarkDegradedNeverRevivesADisabledSource(t *testing.T) {
	// A breaker recovering must not quietly enable a source somebody switched off, nor
	// one that needs a partnership agreement.
	db := openDB(t)
	repo := jobs.NewPostgresSourceRepository(db)
	ctx := context.Background()

	for _, state := range []jobs.SourceState{jobs.StateDisabled, jobs.StatePartnershipRequired} {
		t.Run(string(state), func(t *testing.T) {
			slug := uniqueSlug("protected")
			seedSource(t, db, slug, state, "documented API terms")

			if err := repo.MarkDegraded(ctx, slug, false); err != nil {
				t.Fatalf("MarkDegraded: %v", err)
			}

			got := findSource(t, repo, slug)
			if got.State != state {
				t.Errorf("state = %s, want it left as %s", got.State, state)
			}
		})
	}
}

func TestMarkDegradedMovesAnEnabledSourceBothWays(t *testing.T) {
	db := openDB(t)
	repo := jobs.NewPostgresSourceRepository(db)
	ctx := context.Background()

	slug := uniqueSlug("toggling")
	seedSource(t, db, slug, jobs.StateEnabled, "documented API terms")

	if err := repo.MarkDegraded(ctx, slug, true); err != nil {
		t.Fatalf("MarkDegraded(true): %v", err)
	}
	if got := findSource(t, repo, slug); got.State != jobs.StateDegraded {
		t.Fatalf("state = %s, want degraded", got.State)
	}

	if err := repo.MarkDegraded(ctx, slug, false); err != nil {
		t.Fatalf("MarkDegraded(false): %v", err)
	}
	if got := findSource(t, repo, slug); got.State != jobs.StateEnabled {
		t.Errorf("state = %s, want enabled", got.State)
	}
}

func TestSavingABreakerForAnUnknownSourceReportsNotFound(t *testing.T) {
	// It means the registry and the database disagree, which is worth surfacing rather
	// than silently discarding.
	db := openDB(t)
	repo := jobs.NewPostgresSourceRepository(db)

	err := repo.SaveBreaker(context.Background(), "no-such-source", jobs.Breaker{})
	if apperr.KindOf(err) != apperr.KindNotFound {
		t.Fatalf("err = %v (kind %v), want not found", err, apperr.KindOf(err))
	}
}

func TestEverySeededSourceRecordsEitherABasisOrWhyItHasNone(t *testing.T) {
	// The invariant runs in both directions, and the second is the one worth guarding.
	//
	// A source we can access must say on what basis. A source marked
	// partnership_required must NOT carry one: it is listed precisely because we have no
	// agreement, and a recorded basis for such a source would be a false justification
	// sitting in the database for whoever enables it next. The constraint alone cannot
	// express that, since it only forbids an enabled source without a basis.
	db := openDB(t)
	repo := jobs.NewPostgresSourceRepository(db)

	sources, err := repo.Sources(context.Background())
	if err != nil {
		t.Fatalf("Sources: %v", err)
	}
	if len(sources) == 0 {
		t.Fatal("no reference sources are seeded")
	}

	var accessible, blocked int
	for _, source := range sources {
		if source.State == jobs.StatePartnershipRequired {
			blocked++
			if source.LegalBasis != "" {
				t.Errorf("source %q needs a partnership agreement we do not have, yet records "+
					"a legal basis (%q); that is a false justification for whoever enables it next",
					source.Slug, source.LegalBasis)
			}
			continue
		}
		accessible++
		if source.LegalBasis == "" {
			t.Errorf("source %q is not marked partnership_required but records no legal basis, "+
				"so nothing says how we are entitled to access it", source.Slug)
		}
		if !source.Kind.Valid() {
			t.Errorf("source %q has kind %q, which the domain does not recognise",
				source.Slug, source.Kind)
		}
		if source.RateLimit <= 0 {
			t.Errorf("source %q has rate limit %d", source.Slug, source.RateLimit)
		}
	}

	// Both groups must be non-empty, or the test is passing vacuously: all-blocked means
	// no source is usable, all-accessible means the partnership list was dropped.
	if accessible == 0 {
		t.Error("no accessible sources are seeded")
	}
	if blocked == 0 {
		t.Error("no partnership_required sources are seeded; the list of sources we must " +
			"not scrape is itself the record of that decision")
	}
}

func findSource(t *testing.T, repo jobs.SourceRepository, slug string) jobs.SourceRecord {
	t.Helper()
	sources, err := repo.Sources(context.Background())
	if err != nil {
		t.Fatalf("Sources: %v", err)
	}
	for _, source := range sources {
		if source.Slug == slug {
			return source
		}
	}
	t.Fatalf("source %q not found", slug)
	return jobs.SourceRecord{}
}

// errContext is a tiny helper so the tests read without importing errors for one call.
type errContext string

func (e errContext) Error() string { return string(e) }
