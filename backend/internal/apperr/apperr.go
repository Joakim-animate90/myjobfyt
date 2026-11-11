// Package apperr defines the application's error taxonomy.
//
// Every error that crosses a service boundary carries a Kind. The HTTP layer maps Kind
// to a status code in exactly one place (internal/httpx), so a new endpoint cannot
// invent its own error semantics, and an internal message cannot leak to a client
// because someone returned a raw error from a handler.
package apperr

import (
	"errors"
	"fmt"
)

// Kind classifies an error by how the caller should react to it.
type Kind string

const (
	// KindValidation means the request was well-formed but semantically invalid.
	KindValidation Kind = "validation"
	// KindUnauthenticated means no valid credential was presented.
	KindUnauthenticated Kind = "unauthenticated"
	// KindForbidden means the credential is valid but not permitted.
	KindForbidden Kind = "forbidden"
	// KindNotFound means the resource does not exist, or does not exist for this tenant.
	// These two cases are deliberately indistinguishable to the client: telling a
	// caller that a resource exists but belongs to someone else is an information leak.
	KindNotFound Kind = "not_found"
	// KindConflict means the request collides with current state.
	KindConflict Kind = "conflict"
	// KindQuotaExceeded means a plan entitlement is exhausted.
	KindQuotaExceeded Kind = "quota_exceeded"
	// KindRateLimited means the caller is sending too fast.
	KindRateLimited Kind = "rate_limited"
	// KindPreconditionFailed means a prerequisite is unmet (e.g. form auto-fill
	// requested before phone and city were supplied).
	KindPreconditionFailed Kind = "precondition_failed"
	// KindDependency means a third party failed. Retrying may help.
	KindDependency Kind = "dependency_failure"
	// KindUnprocessable means the input is structurally valid but cannot be processed
	// (e.g. an image-only PDF with no extractable text).
	KindUnprocessable Kind = "unprocessable"
	// KindInternal is an unexpected failure. The message is never shown to a client.
	KindInternal Kind = "internal"
)

// FieldError locates a validation failure precisely enough for a form to highlight it.
type FieldError struct {
	Field   string `json:"field"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Error is the application error type. Message is safe to show to a client; Internal
// carries the underlying cause for logs only and is never serialised.
type Error struct {
	Kind     Kind
	Code     string // stable machine-readable code, e.g. "email_already_registered"
	Message  string // human-readable, client-safe
	Fields   []FieldError
	Meta     map[string]any // client-safe structured detail (quota limits, retry-after)
	Internal error          // never serialised to a client
}

func (e *Error) Error() string {
	if e.Internal != nil {
		return fmt.Sprintf("%s/%s: %s: %v", e.Kind, e.Code, e.Message, e.Internal)
	}
	return fmt.Sprintf("%s/%s: %s", e.Kind, e.Code, e.Message)
}

func (e *Error) Unwrap() error { return e.Internal }

// WithMeta attaches client-safe structured detail.
func (e *Error) WithMeta(k string, v any) *Error {
	if e.Meta == nil {
		e.Meta = map[string]any{}
	}
	e.Meta[k] = v
	return e
}

// WithInternal attaches the underlying cause, for logging only.
func (e *Error) WithInternal(err error) *Error {
	e.Internal = err
	return e
}

func newf(kind Kind, code, format string, args ...any) *Error {
	return &Error{Kind: kind, Code: code, Message: fmt.Sprintf(format, args...)}
}

// Validation reports a semantically invalid request, optionally with field detail.
func Validation(code, format string, args ...any) *Error {
	return newf(KindValidation, code, format, args...)
}

// ValidationFields reports field-level validation failures.
func ValidationFields(fields []FieldError) *Error {
	e := newf(KindValidation, "invalid_request", "the request contains invalid fields")
	e.Fields = fields
	return e
}

func Unauthenticated(code, format string, args ...any) *Error {
	return newf(KindUnauthenticated, code, format, args...)
}

func Forbidden(code, format string, args ...any) *Error {
	return newf(KindForbidden, code, format, args...)
}

// NotFound reports a missing resource. The message intentionally reveals only the
// resource type, never whether it exists under a different owner.
func NotFound(resource string) *Error {
	return newf(KindNotFound, "not_found", "%s not found", resource)
}

func Conflict(code, format string, args ...any) *Error {
	return newf(KindConflict, code, format, args...)
}

// QuotaExceeded reports an exhausted entitlement, including the information the client
// needs to explain the limit and when it resets.
func QuotaExceeded(quotaKey string, limit int64, resetsAt string) *Error {
	e := newf(KindQuotaExceeded, "quota_exceeded", "your plan limit for %s has been reached", quotaKey)
	return e.WithMeta("quota_key", quotaKey).WithMeta("limit", limit).WithMeta("resets_at", resetsAt)
}

// RateLimited reports throttling, with the retry hint in seconds.
func RateLimited(retryAfterSeconds int) *Error {
	e := newf(KindRateLimited, "rate_limited", "too many requests")
	return e.WithMeta("retry_after_seconds", retryAfterSeconds)
}

func PreconditionFailed(code, format string, args ...any) *Error {
	return newf(KindPreconditionFailed, code, format, args...)
}

func Unprocessable(code, format string, args ...any) *Error {
	return newf(KindUnprocessable, code, format, args...)
}

// Dependency reports a third-party failure. The dependency name is recorded for
// metrics and logs; the client sees only a generic message.
func Dependency(dependency string, cause error) *Error {
	e := newf(KindDependency, "dependency_failure", "a required service is temporarily unavailable")
	e.Internal = fmt.Errorf("%s: %w", dependency, cause)
	return e.WithMeta("dependency", dependency)
}

// Internal wraps an unexpected failure. The cause is logged, never returned.
func Internal(cause error) *Error {
	return &Error{
		Kind:     KindInternal,
		Code:     "internal_error",
		Message:  "an unexpected error occurred",
		Internal: cause,
	}
}

// Internalf wraps an unexpected failure with context.
func Internalf(cause error, format string, args ...any) *Error {
	return &Error{
		Kind:     KindInternal,
		Code:     "internal_error",
		Message:  "an unexpected error occurred",
		Internal: fmt.Errorf(format+": %w", append(args, cause)...),
	}
}

// KindOf extracts the Kind from any error, defaulting to KindInternal. An error that
// does not carry a Kind is by definition unexpected, so treating it as internal is both
// correct and the safe default: it will be logged and reported generically.
func KindOf(err error) Kind {
	var e *Error
	if errors.As(err, &e) {
		return e.Kind
	}
	return KindInternal
}

// As extracts the *Error from an error chain, or nil.
func As(err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return nil
}

// IsRetryable reports whether retrying the same operation could plausibly succeed.
// Workers use this to decide between a backoff retry and a terminal failure.
func IsRetryable(err error) bool {
	switch KindOf(err) {
	case KindDependency, KindRateLimited, KindInternal:
		return true
	default:
		return false
	}
}
