package apperr_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/Joakim-animate90/myjobfyt/backend/internal/apperr"
)

func TestKindOfDefaultsToInternalForUnclassifiedErrors(t *testing.T) {
	// An error without a Kind is by definition unexpected. Defaulting to internal
	// ensures it is logged and reported generically rather than leaking its message.
	if got := apperr.KindOf(errors.New("boom")); got != apperr.KindInternal {
		t.Fatalf("KindOf(plain error) = %q, want %q", got, apperr.KindInternal)
	}
	if got := apperr.KindOf(nil); got != apperr.KindInternal {
		t.Fatalf("KindOf(nil) = %q, want %q", got, apperr.KindInternal)
	}
}

func TestKindOfUnwrapsThroughWrapping(t *testing.T) {
	base := apperr.NotFound("job posting")
	wrapped := fmt.Errorf("service layer: %w", fmt.Errorf("repository: %w", base))

	if got := apperr.KindOf(wrapped); got != apperr.KindNotFound {
		t.Fatalf("KindOf(double-wrapped) = %q, want %q", got, apperr.KindNotFound)
	}
	if apperr.As(wrapped) != base {
		t.Fatal("As did not recover the original *Error through two layers of wrapping")
	}
}

func TestInternalCauseIsNotInTheClientMessage(t *testing.T) {
	// The client-facing Message must never carry the underlying cause: that is how
	// DSNs, table names and stack detail leak out of a 500.
	cause := errors.New("pq: duplicate key value violates unique constraint \"users_email_key\"")
	e := apperr.Internal(cause)

	if e.Message == "" {
		t.Fatal("Internal produced an empty client message")
	}
	if e.Message == cause.Error() {
		t.Fatal("client message is the raw cause")
	}
	if !errors.Is(e, cause) {
		t.Fatal("Internal lost the cause; it must remain unwrappable for logs")
	}
}

func TestNotFoundRevealsOnlyTheResourceType(t *testing.T) {
	// Not-found and not-yours must be indistinguishable to a client.
	e := apperr.NotFound("application")
	if e.Kind != apperr.KindNotFound {
		t.Fatalf("Kind = %q, want %q", e.Kind, apperr.KindNotFound)
	}
	if e.Message != "application not found" {
		t.Fatalf("Message = %q, want %q", e.Message, "application not found")
	}
}

func TestQuotaExceededCarriesTheDetailAClientNeeds(t *testing.T) {
	e := apperr.QuotaExceeded("daily_applications", 25, "2026-11-12T00:00:00Z")

	if e.Kind != apperr.KindQuotaExceeded {
		t.Fatalf("Kind = %q, want %q", e.Kind, apperr.KindQuotaExceeded)
	}
	for _, key := range []string{"quota_key", "limit", "resets_at"} {
		if _, ok := e.Meta[key]; !ok {
			t.Errorf("Meta is missing %q; the UI cannot explain the limit without it", key)
		}
	}
}

func TestIsRetryableClassification(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"dependency failure", apperr.Dependency("adzuna", errors.New("503")), true},
		{"rate limited", apperr.RateLimited(30), true},
		{"internal", apperr.Internal(errors.New("nil map")), true},
		{"unclassified", errors.New("boom"), true},
		{"validation", apperr.Validation("bad_title", "title is required"), false},
		{"not found", apperr.NotFound("loop"), false},
		{"forbidden", apperr.Forbidden("not_owner", "no"), false},
		{"conflict", apperr.Conflict("already_applied", "already applied"), false},
		{"unprocessable", apperr.Unprocessable("no_text", "no extractable text"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := apperr.IsRetryable(tc.err); got != tc.want {
				t.Fatalf("IsRetryable = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestDependencyRecordsTheDependencyButHidesIt(t *testing.T) {
	cause := errors.New("connection refused")
	e := apperr.Dependency("anthropic", cause)

	if e.Meta["dependency"] != "anthropic" {
		t.Fatalf("Meta[dependency] = %v, want %q", e.Meta["dependency"], "anthropic")
	}
	if !errors.Is(e, cause) {
		t.Fatal("Dependency lost the cause")
	}
	if e.Message == cause.Error() {
		t.Fatal("client message exposes the raw cause")
	}
}

func TestValidationFieldsRoundTripsFieldDetail(t *testing.T) {
	e := apperr.ValidationFields([]apperr.FieldError{
		{Field: "email", Code: "required", Message: "email is required"},
		{Field: "password", Code: "too_short", Message: "password must be at least 12 characters"},
	})
	if e.Kind != apperr.KindValidation {
		t.Fatalf("Kind = %q, want %q", e.Kind, apperr.KindValidation)
	}
	if len(e.Fields) != 2 {
		t.Fatalf("len(Fields) = %d, want 2", len(e.Fields))
	}
	if e.Fields[0].Field != "email" {
		t.Fatalf("Fields[0].Field = %q, want %q", e.Fields[0].Field, "email")
	}
}
