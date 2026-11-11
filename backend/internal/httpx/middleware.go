package httpx

import (
	"context"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/Joakim-animate90/myjobfyt/backend/internal/apperr"
	"github.com/Joakim-animate90/myjobfyt/backend/internal/platform/telemetry"
	"github.com/oklog/ulid/v2"
)

// HeaderRequestID is the inbound/outbound correlation header.
const HeaderRequestID = "X-Request-ID"

// HeaderCorrelationID lets a client or an internal caller join an existing workflow.
const HeaderCorrelationID = "X-Correlation-ID"

// RequestID assigns or propagates a request id and seeds the correlation id.
//
// A client-supplied request id is accepted only when it looks like an id. Echoing
// arbitrary client input into a response header invites header injection and log
// poisoning, so the value is length-capped and character-restricted.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := sanitiseID(r.Header.Get(HeaderRequestID))
		if id == "" {
			id = ulid.Make().String()
		}

		correlationID := sanitiseID(r.Header.Get(HeaderCorrelationID))
		if correlationID == "" {
			correlationID = id
		}

		ctx := telemetry.WithRequestID(r.Context(), id)
		ctx = telemetry.WithCorrelationID(ctx, correlationID)

		w.Header().Set(HeaderRequestID, id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

const maxIDLength = 64

func sanitiseID(v string) string {
	if v == "" || len(v) > maxIDLength {
		return ""
	}
	for _, c := range v {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
		default:
			return ""
		}
	}
	return v
}

// Recoverer converts a panic into a 500 without taking the process down, and logs the
// stack. A panic is always a bug, so it is logged at error level with the stack intact.
func Recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				// http.ErrAbortHandler is the documented way to abort a response
				// without logging noise; honour it.
				if rec == http.ErrAbortHandler {
					panic(rec)
				}
				slog.ErrorContext(r.Context(), "panic recovered in handler",
					slog.Any("panic", rec),
					slog.String("stack", string(debug.Stack())),
					slog.String("method", r.Method),
					slog.String("path", r.URL.Path),
				)
				Error(w, r, apperr.Internal(nil))
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// SecurityHeaders sets defensive response headers.
//
// The API serves only JSON, so a restrictive CSP plus nosniff removes the entire class
// of attacks that rely on a browser deciding to render an API response as a document.
func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'; sandbox")
		h.Set("Permissions-Policy", "geolocation=(), microphone=(), camera=()")
		// Never let a proxy or browser cache a response that may contain personal data.
		h.Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

// HSTS adds Strict-Transport-Security. Applied only when TLS terminates upstream in an
// environment that is actually served over HTTPS; setting it in development would pin
// localhost to HTTPS in the developer's browser.
func HSTS(enabled bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if enabled {
				w.Header().Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
			}
			next.ServeHTTP(w, r)
		})
	}
}

// MaxBodyBytes caps the request body for non-upload endpoints.
//
// http.MaxBytesReader fails the read rather than buffering, so an oversized body is
// rejected during transfer instead of after it has been accepted into memory.
func MaxBodyBytes(limit int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Body != nil {
				r.Body = http.MaxBytesReader(w, r.Body, limit)
			}
			next.ServeHTTP(w, r)
		})
	}
}

// statusRecorder captures the status and byte count for the access log.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
		s.ResponseWriter.WriteHeader(code)
	}
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	n, err := s.ResponseWriter.Write(b)
	s.bytes += n
	return n, err
}

// Unwrap lets http.ResponseController reach the underlying writer for flushing and
// hijacking, which matters for streaming endpoints.
func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// AccessLog records one line per request.
//
// Query strings are deliberately not logged: they carry search terms, which on this
// platform are candidate job-search data.
func AccessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}

		next.ServeHTTP(rec, r)

		if rec.status == 0 {
			rec.status = http.StatusOK
		}
		level := slog.LevelInfo
		switch {
		case rec.status >= 500:
			level = slog.LevelError
		case rec.status >= 400:
			level = slog.LevelWarn
		}

		slog.Log(r.Context(), level, "http request",
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Int("status", rec.status),
			slog.Int("bytes", rec.bytes),
			slog.Duration("duration", time.Since(start)),
			slog.String("remote_ip", ClientIP(r, false)),
			slog.String("user_agent", truncate(r.UserAgent(), 200)),
		)
	})
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// ClientIP resolves the caller's address.
//
// X-Forwarded-For is trusted only when trustProxy is set, because an untrusted
// forwarded header lets any caller spoof its identity and defeat per-IP rate limiting.
func ClientIP(r *http.Request, trustProxy bool) string {
	if trustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			// Left-most entry is the original client when the chain is trusted.
			if i := strings.IndexByte(xff, ','); i > 0 {
				return strings.TrimSpace(xff[:i])
			}
			return strings.TrimSpace(xff)
		}
		if xrip := r.Header.Get("X-Real-IP"); xrip != "" {
			return strings.TrimSpace(xrip)
		}
	}
	host := r.RemoteAddr
	if i := strings.LastIndexByte(host, ':'); i > 0 {
		// Strip the port, but leave a bare IPv6 literal intact.
		if !strings.Contains(host[:i], ":") || strings.HasPrefix(host, "[") {
			host = strings.TrimSuffix(strings.TrimPrefix(host[:i], "["), "]")
		}
	}
	return host
}

// Limiter is the rate-limiting contract, satisfied by the Redis implementation and by
// test doubles. Returning the retry hint lets the HTTP layer set Retry-After.
type Limiter interface {
	Allow(ctx context.Context, key string, limit int, window time.Duration) (allowed bool, retryAfter time.Duration, err error)
}

// RateLimitConfig configures one rate-limit middleware instance.
type RateLimitConfig struct {
	Limiter    Limiter
	Limit      int
	Window     time.Duration
	KeyPrefix  string
	TrustProxy bool
	// FailOpen decides behaviour when the limiter itself fails. Public auth endpoints
	// should fail closed; general API traffic should fail open, because a Redis outage
	// must not become a full outage.
	FailOpen bool
}

// RateLimit throttles by authenticated user when available, otherwise by client IP.
//
// Keying by user first matters: several candidates behind one corporate NAT must not
// throttle each other.
func RateLimit(cfg RateLimitConfig) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()

			subject := telemetry.UserID(ctx)
			scope := "user"
			if subject == "" {
				subject = ClientIP(r, cfg.TrustProxy)
				scope = "ip"
			}
			key := cfg.KeyPrefix + ":" + scope + ":" + subject

			allowed, retryAfter, err := cfg.Limiter.Allow(ctx, key, cfg.Limit, cfg.Window)
			if err != nil {
				slog.ErrorContext(ctx, "rate limiter unavailable",
					slog.String("error", err.Error()),
					slog.String("key_prefix", cfg.KeyPrefix),
					slog.Bool("fail_open", cfg.FailOpen),
				)
				if !cfg.FailOpen {
					Error(w, r, apperr.Dependency("rate_limiter", err))
					return
				}
				next.ServeHTTP(w, r)
				return
			}

			if !allowed {
				secs := int(retryAfter.Seconds())
				if secs < 1 {
					secs = 1
				}
				Error(w, r, apperr.RateLimited(secs))
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// Timeout bounds handler execution. The handler observes ctx.Done() and should stop;
// this guards the server against a handler that ignores cancellation forever.
func Timeout(d time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), d)
			defer cancel()
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
