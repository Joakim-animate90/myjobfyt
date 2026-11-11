// Package httpx holds the HTTP transport layer: the response envelope, the single
// error-to-status mapping, request binding and validation, and middleware.
//
// Keeping all of this in one package is deliberate. The mapping from apperr.Kind to an
// HTTP status exists exactly once, so no handler can invent its own error semantics and
// no internal error message can reach a client because someone returned a raw error.
package httpx

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/Joakim-animate90/myjobfyt/backend/internal/apperr"
	"github.com/Joakim-animate90/myjobfyt/backend/internal/platform/telemetry"
)

// Envelope is the shape of every non-empty response body.
type Envelope struct {
	Data  any            `json:"data,omitempty"`
	Error *ErrorBody     `json:"error,omitempty"`
	Meta  map[string]any `json:"meta,omitempty"`
}

// ErrorBody is the client-visible error. It never carries an internal message.
type ErrorBody struct {
	Code      string              `json:"code"`
	Message   string              `json:"message"`
	Fields    []apperr.FieldError `json:"fields,omitempty"`
	Meta      map[string]any      `json:"meta,omitempty"`
	RequestID string              `json:"request_id,omitempty"`
}

// Page describes a paginated collection for the meta block.
type Page struct {
	Limit  int   `json:"limit"`
	Offset int   `json:"offset"`
	Total  int64 `json:"total"`
}

// JSON writes a success response with the given status.
func JSON(w http.ResponseWriter, r *http.Request, status int, data any) {
	write(w, r, status, Envelope{Data: data})
}

// JSONPaged writes a paginated success response.
func JSONPaged(w http.ResponseWriter, r *http.Request, status int, data any, page Page) {
	write(w, r, status, Envelope{
		Data: data,
		Meta: map[string]any{"page": page},
	})
}

// NoContent writes 204.
func NoContent(w http.ResponseWriter) {
	w.WriteHeader(http.StatusNoContent)
}

func write(w http.ResponseWriter, r *http.Request, status int, body Envelope) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		// The status line is already on the wire; the only thing left is to record it.
		slog.ErrorContext(r.Context(), "failed to encode response body",
			slog.String("error", err.Error()),
			slog.Int("status", status),
		)
	}
}

// ValidationError writes a single-field validation failure. A convenience for the
// common case, so the field-error shape is not rebuilt at every call site.
func ValidationError(w http.ResponseWriter, r *http.Request, field, code, message string) {
	Error(w, r, apperr.ValidationFields([]apperr.FieldError{{
		Field: field, Code: code, Message: message,
	}}))
}

// StatusFor maps an application error Kind to an HTTP status code. This is the only
// place in the codebase where that mapping exists.
func StatusFor(kind apperr.Kind) int {
	switch kind {
	case apperr.KindValidation:
		return http.StatusUnprocessableEntity
	case apperr.KindUnauthenticated:
		return http.StatusUnauthorized
	case apperr.KindForbidden:
		return http.StatusForbidden
	case apperr.KindNotFound:
		return http.StatusNotFound
	case apperr.KindConflict:
		return http.StatusConflict
	case apperr.KindQuotaExceeded, apperr.KindRateLimited:
		return http.StatusTooManyRequests
	case apperr.KindPreconditionFailed:
		return http.StatusPreconditionFailed
	case apperr.KindUnprocessable:
		return http.StatusUnprocessableEntity
	case apperr.KindDependency:
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}

// Error writes an error response and logs the internal cause.
//
// Clients receive the Kind-derived status, a stable code, a safe message and the
// request id. Internal detail is logged only. A 5xx carries no message beyond the
// generic one, because that is where DSNs and constraint names escape.
func Error(w http.ResponseWriter, r *http.Request, err error) {
	ctx := r.Context()
	requestID := telemetry.RequestID(ctx)

	appErr := apperr.As(err)
	if appErr == nil {
		appErr = apperr.Internal(err)
	}
	status := StatusFor(appErr.Kind)

	attrs := []any{
		slog.String("error_kind", string(appErr.Kind)),
		slog.String("error_code", appErr.Code),
		slog.Int("status", status),
		slog.String("method", r.Method),
		slog.String("path", r.URL.Path),
	}
	if appErr.Internal != nil {
		attrs = append(attrs, slog.String("cause", appErr.Internal.Error()))
	}

	if status >= http.StatusInternalServerError {
		slog.ErrorContext(ctx, "request failed", attrs...)
	} else {
		slog.WarnContext(ctx, "request rejected", attrs...)
	}

	// Retry-After is actionable, so set it as a real header rather than hiding the
	// hint in the body where a client library will not look for it.
	if appErr.Kind == apperr.KindRateLimited {
		if secs, ok := appErr.Meta["retry_after_seconds"].(int); ok {
			w.Header().Set("Retry-After", strconv.Itoa(secs))
		}
	}

	body := &ErrorBody{
		Code:      appErr.Code,
		Message:   appErr.Message,
		Fields:    appErr.Fields,
		Meta:      appErr.Meta,
		RequestID: requestID,
	}
	if status >= http.StatusInternalServerError {
		// Strip everything that could carry internal detail.
		body.Message = "an unexpected error occurred"
		body.Fields = nil
		body.Meta = nil
	}

	write(w, r, status, Envelope{Error: body})
}
