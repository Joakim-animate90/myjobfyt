//go:build integration

package cv_test

import (
	"context"
	"testing"

	"github.com/Joakim-animate90/myjobfyt/backend/internal/apperr"
	"github.com/Joakim-animate90/myjobfyt/backend/internal/domain/cv"
	"github.com/Joakim-animate90/myjobfyt/backend/internal/platform/postgres"
	"github.com/google/uuid"
)

func TestCandidateEmailReturnsAVerifiedAddress(t *testing.T) {
	db := openDB(t)
	repo := cv.NewPostgresRepository(db)
	userID, _ := seedVersion(t, db)

	// seedVersion creates the user already verified.
	email, err := repo.CandidateEmail(context.Background(), userID)
	if err != nil {
		t.Fatalf("CandidateEmail: %v", err)
	}
	if email == "" {
		t.Fatal("CandidateEmail returned an empty address")
	}
}

func TestCandidateEmailRefusesAnUnverifiedAddress(t *testing.T) {
	// A parse-failure notice sent to an unverified address is mail to someone who may
	// not own it. Not an error worth retrying, so the handler treats it as "nothing to
	// send" — which only works if this reports not-found rather than the address.
	db := openDB(t)
	repo := cv.NewPostgresRepository(db)
	ctx := context.Background()

	userID, _ := seedVersion(t, db)
	mustExec(t, db, `UPDATE users SET email_verified_at = NULL WHERE id = $1`, userID)

	_, err := repo.CandidateEmail(ctx, userID)
	if apperr.KindOf(err) != apperr.KindNotFound {
		t.Fatalf("err = %v (kind %v), want not found", err, apperr.KindOf(err))
	}
}

func TestCandidateEmailRefusesADeletedAccount(t *testing.T) {
	db := openDB(t)
	repo := cv.NewPostgresRepository(db)
	ctx := context.Background()

	userID, _ := seedVersion(t, db)
	mustExec(t, db, `UPDATE users SET deleted_at = now() WHERE id = $1`, userID)

	_, err := repo.CandidateEmail(ctx, userID)
	if apperr.KindOf(err) != apperr.KindNotFound {
		t.Fatalf("err = %v (kind %v), want not found; a deleted account must not be contacted",
			err, apperr.KindOf(err))
	}
}

func TestCandidateEmailForAnUnknownUserReportsNotFound(t *testing.T) {
	db := openDB(t)
	repo := cv.NewPostgresRepository(db)

	_, err := repo.CandidateEmail(context.Background(), uuid.New())
	if apperr.KindOf(err) != apperr.KindNotFound {
		t.Fatalf("err = %v (kind %v), want not found", err, apperr.KindOf(err))
	}
}

func mustExec(t *testing.T, db *postgres.DB, sql string, args ...any) {
	t.Helper()
	if _, err := db.Querier(context.Background()).Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}
