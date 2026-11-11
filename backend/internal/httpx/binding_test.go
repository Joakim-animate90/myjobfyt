package httpx_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Joakim-animate90/myjobfyt/backend/internal/apperr"
	"github.com/Joakim-animate90/myjobfyt/backend/internal/httpx"
)

type registerRequest struct {
	Email    string `json:"email" validate:"required,email"`
	Password string `json:"password" validate:"required,min=12,max=128"`
	Accepted bool   `json:"accepted_terms" validate:"required"`
}

func post(body string) *http.Request {
	return httptest.NewRequest(http.MethodPost, "/v1/auth/register", strings.NewReader(body))
}

func TestBindAcceptsAValidBody(t *testing.T) {
	var in registerRequest
	err := httpx.Bind(post(`{"email":"a@b.com","password":"correct-horse-battery","accepted_terms":true}`), &in)
	if err != nil {
		t.Fatalf("Bind returned %v", err)
	}
	if in.Email != "a@b.com" {
		t.Errorf("Email = %q", in.Email)
	}
}

func TestBindRejectsUnknownFields(t *testing.T) {
	// Silently ignoring a misspelled field turns a client bug into a data bug that
	// surfaces much later, so this must be an error.
	var in registerRequest
	err := httpx.Bind(post(`{"email":"a@b.com","password":"correct-horse-battery","accepted_terms":true,"role":"admin"}`), &in)
	if err == nil {
		t.Fatal("Bind accepted an unknown field")
	}
	if apperr.KindOf(err) != apperr.KindValidation {
		t.Fatalf("kind = %q, want validation", apperr.KindOf(err))
	}
}

func TestBindRejectsTrailingData(t *testing.T) {
	var in registerRequest
	err := httpx.Bind(post(`{"email":"a@b.com","password":"correct-horse-battery","accepted_terms":true}{"email":"c@d.com"}`), &in)
	if err == nil {
		t.Fatal("Bind accepted two JSON objects in one body")
	}
	if e := apperr.As(err); e == nil || e.Code != "trailing_data" {
		t.Fatalf("code = %v, want trailing_data", e)
	}
}

func TestBindRejectsEmptyBody(t *testing.T) {
	var in registerRequest
	err := httpx.Bind(post(``), &in)
	if e := apperr.As(err); e == nil || e.Code != "empty_body" {
		t.Fatalf("err = %v, want empty_body", err)
	}
}

func TestBindReportsWrongTypeWithTheFieldPath(t *testing.T) {
	var in registerRequest
	err := httpx.Bind(post(`{"email":123,"password":"correct-horse-battery","accepted_terms":true}`), &in)
	e := apperr.As(err)
	if e == nil {
		t.Fatalf("err = %v, want a field error", err)
	}
	if len(e.Fields) != 1 || e.Fields[0].Field != "email" || e.Fields[0].Code != "wrong_type" {
		t.Fatalf("fields = %+v, want a wrong_type error on email", e.Fields)
	}
}

func TestBindReportsValidationFailuresUsingJSONFieldNames(t *testing.T) {
	// A client maps errors onto its own form fields by JSON name; returning the Go
	// field name would make the error unusable in the UI.
	var in registerRequest
	err := httpx.Bind(post(`{"email":"not-an-email","password":"short","accepted_terms":false}`), &in)
	e := apperr.As(err)
	if e == nil {
		t.Fatalf("err = %v, want validation fields", err)
	}

	got := map[string]string{}
	for _, f := range e.Fields {
		got[f.Field] = f.Code
	}
	for field, code := range map[string]string{
		"email":          "email",
		"password":       "min",
		"accepted_terms": "required",
	} {
		if got[field] != code {
			t.Errorf("field %q has code %q, want %q (all fields: %+v)", field, got[field], code, e.Fields)
		}
	}
}

func TestParsePaginationDefaults(t *testing.T) {
	p, err := httpx.ParsePagination(httptest.NewRequest(http.MethodGet, "/v1/matches", nil))
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if p.Limit != httpx.DefaultPageLimit || p.Offset != 0 {
		t.Fatalf("got limit=%d offset=%d, want %d/0", p.Limit, p.Offset, httpx.DefaultPageLimit)
	}
}

func TestParsePaginationClampsRatherThanRejects(t *testing.T) {
	// A client asking for 500 items wants as many as possible; failing the request
	// helps nobody. Negative values are nonsense and clamp to the floor.
	cases := []struct {
		query       string
		wantLimit   int
		wantOffset  int
		description string
	}{
		{"limit=500", httpx.MaxPageLimit, 0, "over the ceiling"},
		{"limit=0", 1, 0, "below the floor"},
		{"limit=-5", 1, 0, "negative limit"},
		{"offset=-5", httpx.DefaultPageLimit, 0, "negative offset"},
		{"limit=50&offset=100", 50, 100, "in range"},
	}
	for _, tc := range cases {
		t.Run(tc.description, func(t *testing.T) {
			p, err := httpx.ParsePagination(httptest.NewRequest(http.MethodGet, "/v1/matches?"+tc.query, nil))
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if p.Limit != tc.wantLimit || p.Offset != tc.wantOffset {
				t.Fatalf("got limit=%d offset=%d, want %d/%d", p.Limit, p.Offset, tc.wantLimit, tc.wantOffset)
			}
		})
	}
}

func TestParsePaginationRejectsNonNumeric(t *testing.T) {
	_, err := httpx.ParsePagination(httptest.NewRequest(http.MethodGet, "/v1/matches?limit=abc", nil))
	if apperr.KindOf(err) != apperr.KindValidation {
		t.Fatalf("err = %v, want a validation error", err)
	}
}

func TestBindNamesTheUnknownField(t *testing.T) {
	// "not valid JSON" would send a client hunting for a syntax error that is not
	// there; naming the field is actionable.
	var in registerRequest
	err := httpx.Bind(post(`{"email":"a@b.com","password":"correct-horse-battery","accepted_terms":true,"role":"admin"}`), &in)

	e := apperr.As(err)
	if e == nil {
		t.Fatalf("err = %v, want an *apperr.Error", err)
	}
	if len(e.Fields) != 1 {
		t.Fatalf("fields = %+v, want exactly one", e.Fields)
	}
	if e.Fields[0].Field != "role" {
		t.Errorf("field = %q, want role", e.Fields[0].Field)
	}
	if e.Fields[0].Code != "unknown_field" {
		t.Errorf("code = %q, want unknown_field", e.Fields[0].Code)
	}
}

func TestBindStillRejectsGenuinelyMalformedJSON(t *testing.T) {
	var in registerRequest
	err := httpx.Bind(post(`{"email": "a@b.com", oops}`), &in)
	e := apperr.As(err)
	if e == nil || e.Code != "malformed_json" {
		t.Fatalf("err = %v, want malformed_json", err)
	}
}
