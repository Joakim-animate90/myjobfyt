// Package telemetry provides structured logging, tracing and metrics wiring.
//
// The logging layer enforces redaction. MyJobFyt handles CVs, passwords, tokens, SMTP
// credentials and AI prompts containing personal data; a log line is the easiest way for
// any of that to escape into a third-party log aggregator and stay there. Redaction is
// therefore implemented in the logging handler itself rather than left to call sites:
// forgetting it at one call site must not be able to cause a disclosure.
package telemetry

import (
	"context"
	"log/slog"
	"os"
	"strings"

	"github.com/Joakim-animate90/myjobfyt/backend/internal/config"
)

// Context keys for values that must appear on every log line.
type ctxKey int

const (
	ctxKeyRequestID ctxKey = iota
	ctxKeyUserID
	ctxKeyCorrelationID
)

// WithRequestID attaches the request id used to correlate logs, traces and the
// request_id returned to clients in error responses.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxKeyRequestID, id)
}

// RequestID returns the request id, or "".
func RequestID(ctx context.Context) string {
	v, _ := ctx.Value(ctxKeyRequestID).(string)
	return v
}

// WithUserID attaches the authenticated principal's id.
func WithUserID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxKeyUserID, id)
}

// UserID returns the authenticated principal's id, or "".
func UserID(ctx context.Context) string {
	v, _ := ctx.Value(ctxKeyUserID).(string)
	return v
}

// WithCorrelationID attaches the workflow-wide correlation id. It is created at the
// originating HTTP request and copied into every descendant event, which is what makes
// one candidate action traceable across async hops.
func WithCorrelationID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxKeyCorrelationID, id)
}

// CorrelationID returns the workflow correlation id, or "".
func CorrelationID(ctx context.Context) string {
	v, _ := ctx.Value(ctxKeyCorrelationID).(string)
	return v
}

// Redacted is the placeholder substituted for sensitive values.
const Redacted = "[REDACTED]"

// redactedKeys are attribute keys whose values are never logged, in any environment.
// Matching is case-insensitive and substring-based on the normalised key, so
// "smtp_password", "SMTPPassword" and "user.password" are all caught.
var redactedKeys = []string{
	"password",
	"passwd",
	"secret",
	"token",
	"authorization",
	"auth_header",
	"cookie",
	"set_cookie",
	"api_key",
	"apikey",
	"access_key",
	"secret_key",
	"private_key",
	"credential",
	"session",
	"jwt",
	"bearer",
	"otp",
	"pin",

	// Personal data specific to this product.
	"cv_text",
	"raw_text",
	"resume_text",
	"cv_content",
	"parsed_cv",
	"cover_letter",
	"email_body",
	"prompt",
	"completion",
	"llm_input",
	"llm_output",
	"answer",
	"phone",
	"ssn",
	"dob",
	"date_of_birth",
	"card",
	"iban",
}

// allowedKeySuffixes are keys that contain a redacted substring but are safe and
// operationally necessary. Without this list, redaction would blind the very metrics
// needed to diagnose the pipeline: "prompt_tokens" is a count, not a prompt, and
// "token_count" is a number, not a credential.
var allowedExactKeys = map[string]struct{}{
	"prompt_tokens":     {},
	"completion_tokens": {},
	"total_tokens":      {},
	"token_count":       {},
	"answer_count":      {},
	"answer_source":     {},
	"has_password":      {},
	"password_set":      {},
	"session_count":     {},
	"cover_letter_mode": {},
	"phone_present":     {},
	"secret_ref":        {},
}

func normaliseKey(key string) string {
	k := strings.ToLower(key)
	k = strings.ReplaceAll(k, "-", "_")
	k = strings.ReplaceAll(k, ".", "_")
	return k
}

// ShouldRedact reports whether an attribute key must have its value suppressed.
func ShouldRedact(key string) bool {
	k := normaliseKey(key)
	if _, ok := allowedExactKeys[k]; ok {
		return false
	}
	for _, needle := range redactedKeys {
		if strings.Contains(k, needle) {
			return true
		}
	}
	return false
}

// redactingHandler wraps a slog.Handler, rewriting sensitive attribute values and
// injecting the request, user and correlation ids from context onto every record.
type redactingHandler struct {
	inner slog.Handler
}

// NewRedactingHandler wraps h so that sensitive attributes are suppressed and
// contextual identifiers are added automatically.
func NewRedactingHandler(h slog.Handler) slog.Handler {
	return &redactingHandler{inner: h}
}

func (h *redactingHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.inner.Enabled(ctx, l)
}

func (h *redactingHandler) Handle(ctx context.Context, r slog.Record) error {
	out := slog.NewRecord(r.Time, r.Level, r.Message, r.PC)

	if id := RequestID(ctx); id != "" {
		out.AddAttrs(slog.String("request_id", id))
	}
	if id := UserID(ctx); id != "" {
		out.AddAttrs(slog.String("user_id", id))
	}
	if id := CorrelationID(ctx); id != "" {
		out.AddAttrs(slog.String("correlation_id", id))
	}
	if sc := spanContextAttrs(ctx); len(sc) > 0 {
		out.AddAttrs(sc...)
	}

	r.Attrs(func(a slog.Attr) bool {
		out.AddAttrs(redactAttr(a))
		return true
	})
	return h.inner.Handle(ctx, out)
}

func (h *redactingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	redacted := make([]slog.Attr, 0, len(attrs))
	for _, a := range attrs {
		redacted = append(redacted, redactAttr(a))
	}
	return &redactingHandler{inner: h.inner.WithAttrs(redacted)}
}

func (h *redactingHandler) WithGroup(name string) slog.Handler {
	return &redactingHandler{inner: h.inner.WithGroup(name)}
}

// redactAttr suppresses a sensitive value, recursing into groups so that nesting an
// attribute does not evade redaction.
func redactAttr(a slog.Attr) slog.Attr {
	if a.Value.Kind() == slog.KindGroup {
		group := a.Value.Group()
		out := make([]any, 0, len(group))
		for _, inner := range group {
			out = append(out, redactAttr(inner))
		}
		return slog.Group(a.Key, out...)
	}
	if ShouldRedact(a.Key) {
		return slog.String(a.Key, Redacted)
	}
	return a
}

// NewLogger builds the process logger from configuration.
func NewLogger(cfg config.LogConfig, service, version string) *slog.Logger {
	var level slog.Level
	switch strings.ToLower(cfg.Level) {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}

	opts := &slog.HandlerOptions{Level: level, AddSource: cfg.AddSource}

	var base slog.Handler
	if strings.EqualFold(cfg.Format, "text") {
		base = slog.NewTextHandler(os.Stdout, opts)
	} else {
		base = slog.NewJSONHandler(os.Stdout, opts)
	}

	return slog.New(NewRedactingHandler(base)).With(
		slog.String("service", service),
		slog.String("version", version),
	)
}
