package httpx_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Joakim-animate90/myjobfyt/backend/internal/httpx"
	"github.com/Joakim-animate90/myjobfyt/backend/internal/platform/telemetry"
)

func TestRequestIDGeneratesWhenAbsent(t *testing.T) {
	var seen string
	h := httpx.RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = telemetry.RequestID(r.Context())
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if seen == "" {
		t.Fatal("no request id placed in the context")
	}
	if got := rec.Header().Get(httpx.HeaderRequestID); got != seen {
		t.Fatalf("response header %q != context value %q", got, seen)
	}
}

func TestRequestIDPropagatesASaneClientValue(t *testing.T) {
	var seen, corr string
	h := httpx.RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = telemetry.RequestID(r.Context())
		corr = telemetry.CorrelationID(r.Context())
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(httpx.HeaderRequestID, "client-supplied-123")
	h.ServeHTTP(httptest.NewRecorder(), req)

	if seen != "client-supplied-123" {
		t.Fatalf("request id = %q, want the client value", seen)
	}
	// Correlation defaults to the request id so a single-hop request is still traceable.
	if corr != "client-supplied-123" {
		t.Fatalf("correlation id = %q, want it to default to the request id", corr)
	}
}

func TestRequestIDRejectsHostileClientValues(t *testing.T) {
	// Echoing arbitrary client input into a response header invites header injection
	// and log poisoning, so anything that does not look like an id is discarded.
	hostile := []string{
		"abc\r\nX-Injected: yes",
		"abc\ndef",
		`<script>alert(1)</script>`,
		strings.Repeat("a", 65),
		"has spaces",
		"semi;colon",
	}
	for _, value := range hostile {
		t.Run(strings.ReplaceAll(truncateForName(value), "\n", "\\n"), func(t *testing.T) {
			var seen string
			h := httpx.RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seen = telemetry.RequestID(r.Context())
			}))

			req := httptest.NewRequest(http.MethodGet, "/", nil)
			// Set directly on the map to bypass net/http's own header sanitisation,
			// which would otherwise hide the injection case from this test.
			req.Header[http.CanonicalHeaderKey(httpx.HeaderRequestID)] = []string{value}
			h.ServeHTTP(httptest.NewRecorder(), req)

			if seen == value {
				t.Fatalf("hostile request id %q was accepted verbatim", value)
			}
			if seen == "" {
				t.Fatal("expected a generated id to replace the rejected one")
			}
		})
	}
}

func truncateForName(s string) string {
	if len(s) > 20 {
		return s[:20]
	}
	return s
}

func TestRequestIDAcceptsASeparateCorrelationID(t *testing.T) {
	var reqID, corrID string
	h := httpx.RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqID = telemetry.RequestID(r.Context())
		corrID = telemetry.CorrelationID(r.Context())
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(httpx.HeaderRequestID, "req-1")
	req.Header.Set(httpx.HeaderCorrelationID, "workflow-7")
	h.ServeHTTP(httptest.NewRecorder(), req)

	if reqID != "req-1" || corrID != "workflow-7" {
		t.Fatalf("got request=%q correlation=%q, want req-1/workflow-7", reqID, corrID)
	}
}

func TestRecovererTurnsAPanicIntoA500WithoutLeakingIt(t *testing.T) {
	h := httpx.RequestID(httpx.Recoverer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("secret internal detail")
	})))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "secret internal detail") {
		t.Fatalf("panic value leaked to the client: %s", rec.Body.String())
	}
}

func TestRecovererRepanicsOnErrAbortHandler(t *testing.T) {
	// http.ErrAbortHandler is the documented way to abort a response; swallowing it
	// would turn an intentional abort into a bogus 500.
	defer func() {
		if rec := recover(); rec != http.ErrAbortHandler {
			t.Fatalf("recovered %v, want http.ErrAbortHandler to propagate", rec)
		}
	}()

	h := httpx.Recoverer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic(http.ErrAbortHandler)
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
}

func TestSecurityHeadersAreSet(t *testing.T) {
	h := httpx.SecurityHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	want := map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"Referrer-Policy":        "no-referrer",
		// Responses may contain personal data, so they must never be cached.
		"Cache-Control": "no-store",
	}
	for k, v := range want {
		if got := rec.Header().Get(k); got != v {
			t.Errorf("header %s = %q, want %q", k, got, v)
		}
	}
	if rec.Header().Get("Content-Security-Policy") == "" {
		t.Error("Content-Security-Policy is unset")
	}
}

func TestHSTSOnlyWhenEnabled(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		rec := httptest.NewRecorder()
		httpx.HSTS(enabled)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})).
			ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

		got := rec.Header().Get("Strict-Transport-Security")
		if enabled && got == "" {
			t.Error("HSTS(true) did not set the header")
		}
		// Setting HSTS in development would pin localhost to HTTPS in the browser.
		if !enabled && got != "" {
			t.Errorf("HSTS(false) set the header: %q", got)
		}
	}
}

// fakeLimiter is a Limiter test double with scripted behaviour.
type fakeLimiter struct {
	allow      bool
	retryAfter time.Duration
	err        error
	lastKey    string
	calls      int
}

func (f *fakeLimiter) Allow(_ context.Context, key string, _ int, _ time.Duration) (bool, time.Duration, error) {
	f.calls++
	f.lastKey = key
	return f.allow, f.retryAfter, f.err
}

func rateLimited(t *testing.T, l *fakeLimiter, failOpen bool, ctxUser string) *httptest.ResponseRecorder {
	t.Helper()
	var reached bool
	mw := httpx.RateLimit(httpx.RateLimitConfig{
		Limiter: l, Limit: 5, Window: time.Minute, KeyPrefix: "api", FailOpen: failOpen,
	})
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/matches", nil)
	req.RemoteAddr = "203.0.113.7:51234"
	if ctxUser != "" {
		req = req.WithContext(telemetry.WithUserID(req.Context(), ctxUser))
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	t.Logf("handler reached: %v", reached)
	return rec
}

func TestRateLimitAllowsUnderTheLimit(t *testing.T) {
	l := &fakeLimiter{allow: true}
	rec := rateLimited(t, l, false, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

func TestRateLimitBlocksAndSetsRetryAfter(t *testing.T) {
	l := &fakeLimiter{allow: false, retryAfter: 17 * time.Second}
	rec := rateLimited(t, l, false, "")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != "17" {
		t.Fatalf("Retry-After = %q, want 17", got)
	}
}

func TestRateLimitKeysByUserWhenAuthenticated(t *testing.T) {
	// Several candidates behind one corporate NAT must not throttle each other.
	l := &fakeLimiter{allow: true}
	rateLimited(t, l, false, "user-42")
	if l.lastKey != "api:user:user-42" {
		t.Fatalf("key = %q, want api:user:user-42", l.lastKey)
	}

	l2 := &fakeLimiter{allow: true}
	rateLimited(t, l2, false, "")
	if l2.lastKey != "api:ip:203.0.113.7" {
		t.Fatalf("anonymous key = %q, want api:ip:203.0.113.7", l2.lastKey)
	}
}

func TestRateLimitFailOpenKeepsServingWhenRedisIsDown(t *testing.T) {
	// A Redis outage must not become a full API outage.
	l := &fakeLimiter{err: errors.New("dial tcp: connection refused")}
	rec := rateLimited(t, l, true, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 when failing open", rec.Code)
	}
}

func TestRateLimitFailClosedRejectsWhenRedisIsDown(t *testing.T) {
	// Auth endpoints fail closed: without a limiter they are a brute-force target.
	l := &fakeLimiter{err: errors.New("dial tcp: connection refused")}
	rec := rateLimited(t, l, false, "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 when failing closed", rec.Code)
	}
}

func TestClientIPIgnoresForwardedHeadersWhenProxyIsUntrusted(t *testing.T) {
	// Trusting X-Forwarded-For unconditionally lets any caller spoof its identity and
	// defeat per-IP rate limiting entirely.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "203.0.113.7:51234"
	req.Header.Set("X-Forwarded-For", "1.2.3.4")

	if got := httpx.ClientIP(req, false); got != "203.0.113.7" {
		t.Fatalf("ClientIP(untrusted) = %q, want the socket address", got)
	}
	if got := httpx.ClientIP(req, true); got != "1.2.3.4" {
		t.Fatalf("ClientIP(trusted) = %q, want the forwarded address", got)
	}
}

func TestClientIPTakesTheLeftmostForwardedEntry(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.0.0.1:443"
	req.Header.Set("X-Forwarded-For", "1.2.3.4, 10.0.0.5, 10.0.0.6")

	if got := httpx.ClientIP(req, true); got != "1.2.3.4" {
		t.Fatalf("ClientIP = %q, want the original client", got)
	}
}

func TestMaxBodyBytesRejectsOversizedBodies(t *testing.T) {
	h := httpx.MaxBodyBytes(16)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var sink [1024]byte
		if _, err := r.Body.Read(sink[:]); err != nil {
			httpx.Error(w, r, err)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(strings.Repeat("x", 1024)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code == http.StatusOK {
		t.Fatal("oversized body was accepted")
	}
}
