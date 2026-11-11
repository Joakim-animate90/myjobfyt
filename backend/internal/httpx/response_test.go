package httpx_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Joakim-animate90/myjobfyt/backend/internal/apperr"
	"github.com/Joakim-animate90/myjobfyt/backend/internal/httpx"
	"github.com/Joakim-animate90/myjobfyt/backend/internal/platform/telemetry"
)

func TestStatusForCoversEveryKind(t *testing.T) {
	cases := map[apperr.Kind]int{
		apperr.KindValidation:         http.StatusUnprocessableEntity,
		apperr.KindUnauthenticated:    http.StatusUnauthorized,
		apperr.KindForbidden:          http.StatusForbidden,
		apperr.KindNotFound:           http.StatusNotFound,
		apperr.KindConflict:           http.StatusConflict,
		apperr.KindQuotaExceeded:      http.StatusTooManyRequests,
		apperr.KindRateLimited:        http.StatusTooManyRequests,
		apperr.KindPreconditionFailed: http.StatusPreconditionFailed,
		apperr.KindUnprocessable:      http.StatusUnprocessableEntity,
		apperr.KindDependency:         http.StatusServiceUnavailable,
		apperr.KindInternal:           http.StatusInternalServerError,
	}
	for kind, want := range cases {
		if got := httpx.StatusFor(kind); got != want {
			t.Errorf("StatusFor(%q) = %d, want %d", kind, got, want)
		}
	}
	// An unknown kind must be treated as internal, never as a success.
	if got := httpx.StatusFor(apperr.Kind("something_new")); got != http.StatusInternalServerError {
		t.Errorf("StatusFor(unknown) = %d, want 500", got)
	}
}

func decodeEnvelope(t *testing.T, body string) httpx.Envelope {
	t.Helper()
	var env httpx.Envelope
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("response is not a valid envelope: %v\nbody: %s", err, body)
	}
	return env
}

func TestErrorDoesNotLeakInternalDetailOn500(t *testing.T) {
	// This is the test that matters most in this file: a 500 must not carry the cause.
	cause := errors.New(`pq: duplicate key value violates unique constraint "users_email_key"`)
	req := httptest.NewRequest(http.MethodPost, "/v1/auth/register", nil)
	req = req.WithContext(telemetry.WithRequestID(req.Context(), "req-abc"))
	rec := httptest.NewRecorder()

	httpx.Error(rec, req, apperr.Internalf(cause, "creating user"))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	body := rec.Body.String()
	for _, leak := range []string{"users_email_key", "duplicate key", "creating user", "pq:"} {
		if strings.Contains(body, leak) {
			t.Fatalf("500 response leaked %q: %s", leak, body)
		}
	}

	env := decodeEnvelope(t, body)
	if env.Error == nil {
		t.Fatal("no error body")
	}
	if env.Error.RequestID != "req-abc" {
		t.Errorf("request_id = %q, want req-abc; without it a user report cannot be traced", env.Error.RequestID)
	}
	if env.Error.Message != "an unexpected error occurred" {
		t.Errorf("message = %q, want the generic message", env.Error.Message)
	}
}

func TestErrorPreservesClientSafeDetailOn4xx(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/loops", nil)
	rec := httptest.NewRecorder()

	httpx.Error(rec, req, apperr.ValidationFields([]apperr.FieldError{
		{Field: "titles", Code: "required", Message: "at least one job title is required"},
	}))

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", rec.Code)
	}
	env := decodeEnvelope(t, rec.Body.String())
	if env.Error == nil || len(env.Error.Fields) != 1 {
		t.Fatalf("field detail was dropped: %+v", env.Error)
	}
	if env.Error.Fields[0].Field != "titles" {
		t.Errorf("field = %q, want titles", env.Error.Fields[0].Field)
	}
}

func TestErrorSetsRetryAfterHeaderWhenRateLimited(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/v1/matches", nil)
	rec := httptest.NewRecorder()

	httpx.Error(rec, req, apperr.RateLimited(42))

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != "42" {
		t.Errorf("Retry-After = %q, want 42; clients look for the header, not the body", got)
	}
}

func TestErrorFromUnclassifiedErrorIs500(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/v1/anything", nil)
	rec := httptest.NewRecorder()

	httpx.Error(rec, req, errors.New("some raw repository error"))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "repository error") {
		t.Fatalf("raw error text reached the client: %s", rec.Body.String())
	}
}

func TestQuotaExceededResponseCarriesTheLimitDetail(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/applications/x/authorise", nil)
	rec := httptest.NewRecorder()

	httpx.Error(rec, req, apperr.QuotaExceeded("daily_applications", 25, "2026-11-12T00:00:00Z"))

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
	env := decodeEnvelope(t, rec.Body.String())
	if env.Error.Meta["quota_key"] != "daily_applications" {
		t.Errorf("meta.quota_key = %v, want daily_applications", env.Error.Meta["quota_key"])
	}
	if env.Error.Meta["resets_at"] != "2026-11-12T00:00:00Z" {
		t.Errorf("meta.resets_at = %v; the UI cannot say when the limit lifts without it", env.Error.Meta["resets_at"])
	}
}

func TestJSONPagedIncludesPageMeta(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/v1/matches", nil)
	rec := httptest.NewRecorder()

	httpx.JSONPaged(rec, req, http.StatusOK, []string{"a", "b"}, httpx.Page{Limit: 20, Offset: 40, Total: 137})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q", ct)
	}
	env := decodeEnvelope(t, rec.Body.String())
	page, ok := env.Meta["page"].(map[string]any)
	if !ok {
		t.Fatalf("meta.page missing: %+v", env.Meta)
	}
	if page["total"] != float64(137) {
		t.Errorf("meta.page.total = %v, want 137", page["total"])
	}
}
