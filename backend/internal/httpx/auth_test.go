package httpx_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Joakim-animate90/myjobfyt/backend/internal/apperr"
	"github.com/Joakim-animate90/myjobfyt/backend/internal/httpx"
	"github.com/Joakim-animate90/myjobfyt/backend/internal/platform/telemetry"
)

// stubParser accepts exactly one token value, for one audience.
type stubParser struct {
	token     string
	audience  string
	principal httpx.Principal
	calls     int
}

func (s *stubParser) ParseBearer(token, audience string) (httpx.Principal, error) {
	s.calls++
	if token != s.token {
		return httpx.Principal{}, apperr.Unauthenticated("token_invalid", "the token is not valid")
	}
	if s.audience != "" && audience != s.audience {
		return httpx.Principal{}, apperr.Unauthenticated("token_wrong_audience", "wrong audience")
	}
	return s.principal, nil
}

func authedHandler(t *testing.T, seen *httpx.Principal) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, err := httpx.MustPrincipal(r.Context())
		if err != nil {
			t.Errorf("MustPrincipal behind RequireAuth returned %v", err)
		}
		*seen = p
		w.WriteHeader(http.StatusOK)
	})
}

func TestRequireAuthAcceptsAValidBearerToken(t *testing.T) {
	parser := &stubParser{
		token:     "good-token",
		principal: httpx.Principal{UserID: "user-1", Email: "a@example.com", Role: "candidate", PlanSlug: "pro"},
	}
	var seen httpx.Principal
	h := httpx.RequireAuth(parser, "myjobfyt-api")(authedHandler(t, &seen))

	req := httptest.NewRequest(http.MethodGet, "/v1/me", nil)
	req.Header.Set("Authorization", "Bearer good-token")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if seen.UserID != "user-1" || seen.Role != "candidate" || seen.PlanSlug != "pro" {
		t.Fatalf("principal = %+v", seen)
	}
}

func TestRequireAuthAcceptsAnyCaseOfTheBearerPrefix(t *testing.T) {
	// The scheme is case-insensitive per RFC 7235, and real clients send all of these.
	parser := &stubParser{token: "good-token", principal: httpx.Principal{UserID: "u"}}
	for _, header := range []string{"Bearer good-token", "bearer good-token", "BEARER good-token", "BeArEr  good-token"} {
		var seen httpx.Principal
		h := httpx.RequireAuth(parser, "myjobfyt-api")(authedHandler(t, &seen))

		req := httptest.NewRequest(http.MethodGet, "/v1/me", nil)
		req.Header.Set("Authorization", header)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("header %q gave status %d, want 200", header, rec.Code)
		}
	}
}

func TestRequireAuthRejectsMissingAndMalformedHeaders(t *testing.T) {
	parser := &stubParser{token: "good-token", principal: httpx.Principal{UserID: "u"}}
	var reached bool
	h := httpx.RequireAuth(parser, "myjobfyt-api")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
	}))

	cases := map[string]string{
		"absent":             "",
		"no scheme":          "good-token",
		"wrong scheme":       "Basic dXNlcjpwYXNz",
		"scheme only":        "Bearer",
		"scheme with spaces": "Bearer    ",
		"wrong token":        "Bearer not-the-token",
	}
	for name, header := range cases {
		t.Run(name, func(t *testing.T) {
			reached = false
			req := httptest.NewRequest(http.MethodGet, "/v1/me", nil)
			if header != "" {
				req.Header.Set("Authorization", header)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", rec.Code)
			}
			if reached {
				t.Fatal("the handler ran despite failed authentication")
			}
			// Without WWW-Authenticate a client cannot tell authentication from
			// authorisation failure.
			if rec.Header().Get("WWW-Authenticate") == "" {
				t.Error("no WWW-Authenticate header on a 401")
			}
		})
	}
}

func TestRequireAuthPassesTheAudienceThrough(t *testing.T) {
	// This is what keeps a leaked extension token useless against the main API.
	parser := &stubParser{token: "good-token", audience: "myjobfyt-extension", principal: httpx.Principal{UserID: "u"}}

	apiHandler := httpx.RequireAuth(parser, "myjobfyt-api")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/v1/me", nil)
	req.Header.Set("Authorization", "Bearer good-token")
	rec := httptest.NewRecorder()
	apiHandler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("an extension-audience token was accepted on the API surface (status %d)", rec.Code)
	}

	extHandler := httpx.RequireAuth(parser, "myjobfyt-extension")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodGet, "/v1/extension/queue", nil)
	req2.Header.Set("Authorization", "Bearer good-token")
	extHandler.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("status = %d on the extension surface, want 200", rec2.Code)
	}
}

func TestRequireAuthPutsTheUserIDIntoTheLoggingContext(t *testing.T) {
	// Every log line and span for an authenticated request must be attributable.
	parser := &stubParser{token: "good-token", principal: httpx.Principal{UserID: "user-42"}}
	var loggedUser string
	h := httpx.RequireAuth(parser, "myjobfyt-api")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		loggedUser = telemetry.UserID(r.Context())
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/me", nil)
	req.Header.Set("Authorization", "Bearer good-token")
	h.ServeHTTP(httptest.NewRecorder(), req)

	if loggedUser != "user-42" {
		t.Fatalf("telemetry.UserID = %q, want user-42", loggedUser)
	}
}

func TestOptionalAuthServesAnonymousAndStaleTokens(t *testing.T) {
	// A stale token left in a browser must not break a public page.
	parser := &stubParser{token: "good-token", principal: httpx.Principal{UserID: "user-1"}}
	var hadPrincipal bool
	h := httpx.OptionalAuth(parser, "myjobfyt-api")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, hadPrincipal = httpx.PrincipalFrom(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	// Anonymous.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/plans", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("anonymous request got %d, want 200", rec.Code)
	}
	if hadPrincipal {
		t.Error("a principal was attached to an anonymous request")
	}

	// Invalid token: served, without a principal.
	req := httptest.NewRequest(http.MethodGet, "/v1/plans", nil)
	req.Header.Set("Authorization", "Bearer stale-token")
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req)
	if rec2.Code != http.StatusOK {
		t.Fatalf("a request with a stale token got %d, want 200", rec2.Code)
	}
	if hadPrincipal {
		t.Error("a principal was attached from an invalid token")
	}

	// Valid token: served, with a principal.
	req3 := httptest.NewRequest(http.MethodGet, "/v1/plans", nil)
	req3.Header.Set("Authorization", "Bearer good-token")
	rec3 := httptest.NewRecorder()
	h.ServeHTTP(rec3, req3)
	if !hadPrincipal {
		t.Error("no principal attached despite a valid token")
	}
}

func TestRequireRoleDefaultDeny(t *testing.T) {
	cases := []struct {
		name    string
		allowed []string
		role    string
		want    int
	}{
		{"allowed role", []string{"admin"}, "admin", http.StatusOK},
		{"one of several", []string{"admin", "support"}, "support", http.StatusOK},
		{"disallowed role", []string{"admin"}, "candidate", http.StatusForbidden},
		// An empty allowlist must reject everyone: a mistake here has to fail closed.
		{"empty allowlist", nil, "admin", http.StatusForbidden},
		// An unrecognised role matches nothing.
		{"unknown role", []string{"admin"}, "superuser", http.StatusForbidden},
		{"empty role", []string{"admin"}, "", http.StatusForbidden},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var reached bool
			h := httpx.RequireRole(tc.allowed...)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				reached = true
				w.WriteHeader(http.StatusOK)
			}))

			req := httptest.NewRequest(http.MethodGet, "/v1/admin/metrics", nil)
			req = req.WithContext(httpx.WithPrincipal(req.Context(), httpx.Principal{UserID: "u", Role: tc.role}))
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d", rec.Code, tc.want)
			}
			if tc.want != http.StatusOK && reached {
				t.Fatal("the handler ran despite a role rejection")
			}
		})
	}
}

func TestRequireRoleWithoutAPrincipalIs401(t *testing.T) {
	// A routing mistake that puts RequireRole before RequireAuth must produce a 401,
	// not a panic.
	h := httpx.RequireRole("admin")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("the handler ran with no principal")
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/admin/metrics", nil))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestRequireRoleDoesNotRevealWhichRoleIsNeeded(t *testing.T) {
	h := httpx.RequireRole("admin")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/metrics", nil)
	req = req.WithContext(httpx.WithPrincipal(req.Context(), httpx.Principal{UserID: "u", Role: "candidate"}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if body := rec.Body.String(); containsAny(body, "admin", "support", "system") {
		t.Fatalf("the 403 body names privileged roles: %s", body)
	}
}

func containsAny(haystack string, needles ...string) bool {
	for _, n := range needles {
		for i := 0; i+len(n) <= len(haystack); i++ {
			if haystack[i:i+len(n)] == n {
				return true
			}
		}
	}
	return false
}

func TestMustPrincipalOnAnAnonymousContext(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	_, err := httpx.MustPrincipal(req.Context())
	if err == nil {
		t.Fatal("MustPrincipal succeeded with no principal")
	}
	if apperr.KindOf(err) != apperr.KindUnauthenticated {
		t.Fatalf("Kind = %q, want unauthenticated", apperr.KindOf(err))
	}
	var target *apperr.Error
	if !errors.As(err, &target) {
		t.Fatal("the error is not an *apperr.Error")
	}
}
