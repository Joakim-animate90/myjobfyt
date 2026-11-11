package httpx

import (
	"context"
	"net/http"
	"strings"

	"github.com/Joakim-animate90/myjobfyt/backend/internal/apperr"
	"github.com/Joakim-animate90/myjobfyt/backend/internal/platform/telemetry"
)

// Principal is the authenticated caller as the transport layer sees it.
//
// Declared here, structurally identical to identity.Principal, so the HTTP layer does
// not import a domain package and domains do not import the transport. The adapter
// that converts between them lives in the identity handler.
type Principal struct {
	UserID   string
	Email    string
	Role     string
	PlanSlug string
	TokenID  string
}

// PrincipalParser validates a bearer token. Implemented by the identity service.
type PrincipalParser interface {
	ParseBearer(token, audience string) (Principal, error)
}

type principalKey struct{}

// WithPrincipal attaches an authenticated principal to the context.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// PrincipalFrom returns the authenticated principal, if any.
func PrincipalFrom(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok
}

// MustPrincipal returns the principal or an unauthenticated error.
//
// Handlers behind RequireAuth can rely on it being present; returning an error rather
// than panicking means a routing mistake produces a 401 instead of taking the process
// down.
func MustPrincipal(ctx context.Context) (Principal, error) {
	p, ok := PrincipalFrom(ctx)
	if !ok {
		return Principal{}, apperr.Unauthenticated("not_authenticated", "authentication is required")
	}
	return p, nil
}

// bearerToken extracts the token from the Authorization header.
func bearerToken(r *http.Request) string {
	header := r.Header.Get("Authorization")
	if header == "" {
		return ""
	}
	const prefix = "bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(header[len(prefix):])
}

// RequireAuth rejects unauthenticated requests.
//
// The audience is per-middleware, so the extension routes can be mounted with the
// extension audience and a leaked extension token is useless against the main API.
func RequireAuth(parser PrincipalParser, audience string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token := bearerToken(r)
			if token == "" {
				// WWW-Authenticate is what tells a standards-aware client this is an
				// authentication problem rather than an authorisation one.
				w.Header().Set("WWW-Authenticate", `Bearer realm="myjobfyt"`)
				Error(w, r, apperr.Unauthenticated("token_missing", "authentication is required"))
				return
			}

			principal, err := parser.ParseBearer(token, audience)
			if err != nil {
				w.Header().Set("WWW-Authenticate", `Bearer realm="myjobfyt", error="invalid_token"`)
				Error(w, r, err)
				return
			}

			ctx := WithPrincipal(r.Context(), principal)
			// Put the user id in the logging context so every subsequent log line and
			// span for this request is attributable.
			ctx = telemetry.WithUserID(ctx, principal.UserID)

			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// OptionalAuth attaches a principal when a valid token is present, without rejecting
// anonymous callers.
//
// Used on endpoints that behave differently when signed in but must still serve
// anonymous traffic. An invalid token is treated as absent rather than as an error, so
// a stale token in a browser cannot break a public page.
func OptionalAuth(parser PrincipalParser, audience string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token := bearerToken(r)
			if token == "" {
				next.ServeHTTP(w, r)
				return
			}
			principal, err := parser.ParseBearer(token, audience)
			if err != nil {
				next.ServeHTTP(w, r)
				return
			}
			ctx := WithPrincipal(r.Context(), principal)
			ctx = telemetry.WithUserID(ctx, principal.UserID)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// RequireRole rejects a caller whose role is not in the allowed set.
//
// Default deny: an empty allowed set rejects everyone, and an unrecognised role matches
// nothing. Both are deliberate — a mistake here must fail closed.
func RequireRole(allowed ...string) func(http.Handler) http.Handler {
	permitted := make(map[string]struct{}, len(allowed))
	for _, role := range allowed {
		permitted[role] = struct{}{}
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			principal, err := MustPrincipal(r.Context())
			if err != nil {
				Error(w, r, err)
				return
			}
			if _, ok := permitted[principal.Role]; !ok {
				// The message does not say which role is required: that is information
				// about the system's structure that an unauthorised caller has no need
				// for.
				Error(w, r, apperr.Forbidden("insufficient_role", "you do not have access to this resource"))
				return
			}
			next.ServeHTTP(w, r.WithContext(r.Context()))
		})
	}
}

// RequireVerifiedEmail is a placeholder for the verification gate.
//
// The access token does not carry verification state, deliberately: a candidate who
// verifies their email should not have to sign out and in again for it to take effect.
// Endpoints that need the gate check it in their service call, against the database.
// This is left documented rather than implemented so nobody adds a stale claim later.
