package telemetry_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/Joakim-animate90/myjobfyt/backend/internal/platform/telemetry"
)

// newTestLogger returns a logger writing JSON into buf through the redacting handler.
func newTestLogger(buf *bytes.Buffer) *slog.Logger {
	base := slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	return slog.New(telemetry.NewRedactingHandler(base))
}

func decode(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	line := strings.TrimSpace(buf.String())
	if line == "" {
		t.Fatal("no log output produced")
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		t.Fatalf("log line is not valid JSON: %v\nline: %s", err, line)
	}
	return m
}

func TestSensitiveKeysAreRedacted(t *testing.T) {
	// Each of these would be a real disclosure if it reached a log aggregator.
	cases := []struct {
		key   string
		value string
	}{
		{"password", "hunter2"},
		{"user_password", "hunter2"},
		{"smtp_password", "app-specific-password"},
		{"access_token", "eyJhbGciOi"},
		{"refresh_token", "opaque-256-bit"},
		{"Authorization", "Bearer eyJ"},
		{"cookie", "refresh=abc"},
		{"api_key", "sk-ant-123"},
		{"secret_key", "minio-secret"},
		{"private_key", "-----BEGIN"},
		{"cv_text", "Joakim — Senior Engineer, Acme Corp 2019-2024"},
		{"raw_text", "full resume body"},
		{"cover_letter", "Dear hiring manager"},
		{"prompt", "You are a CV parser. CV: ..."},
		{"llm_output", "{\"experience\":[...]}"},
		{"phone", "+254700000000"},
		{"answer", "I have 6 years of Go experience"},
	}

	for _, tc := range cases {
		t.Run(tc.key, func(t *testing.T) {
			var buf bytes.Buffer
			newTestLogger(&buf).Info("event", slog.String(tc.key, tc.value))

			raw := buf.String()
			if strings.Contains(raw, tc.value) {
				t.Fatalf("value for key %q leaked into the log line: %s", tc.key, raw)
			}
			m := decode(t, &buf)
			if got := m[tc.key]; got != telemetry.Redacted {
				t.Fatalf("log[%q] = %v, want %q", tc.key, got, telemetry.Redacted)
			}
		})
	}
}

func TestOperationalCountersAreNotRedacted(t *testing.T) {
	// Over-redaction is its own failure: these are the attributes needed to diagnose
	// AI cost and pipeline behaviour, and they contain no personal data.
	var buf bytes.Buffer
	newTestLogger(&buf).Info("ai call",
		slog.Int("prompt_tokens", 1840),
		slog.Int("completion_tokens", 320),
		slog.Int("total_tokens", 2160),
		slog.String("answer_source", "candidate"),
		slog.Bool("phone_present", true),
		slog.String("cover_letter_mode", "default"),
	)

	m := decode(t, &buf)
	if m["prompt_tokens"] != float64(1840) {
		t.Errorf("prompt_tokens was redacted or altered: %v", m["prompt_tokens"])
	}
	if m["completion_tokens"] != float64(320) {
		t.Errorf("completion_tokens was redacted or altered: %v", m["completion_tokens"])
	}
	if m["answer_source"] != "candidate" {
		t.Errorf("answer_source was redacted: %v", m["answer_source"])
	}
	if m["phone_present"] != true {
		t.Errorf("phone_present was redacted: %v", m["phone_present"])
	}
	if m["cover_letter_mode"] != "default" {
		t.Errorf("cover_letter_mode was redacted: %v", m["cover_letter_mode"])
	}
}

func TestRedactionAppliesInsideGroups(t *testing.T) {
	// Nesting an attribute inside a group must not evade redaction.
	var buf bytes.Buffer
	newTestLogger(&buf).Info("smtp configured",
		slog.Group("smtp",
			slog.String("host", "smtp.gmail.com"),
			slog.String("password", "super-secret"),
			slog.Group("inner", slog.String("token", "nested-secret")),
		),
	)

	raw := buf.String()
	if strings.Contains(raw, "super-secret") {
		t.Fatalf("grouped password leaked: %s", raw)
	}
	if strings.Contains(raw, "nested-secret") {
		t.Fatalf("doubly-nested token leaked: %s", raw)
	}
	if !strings.Contains(raw, "smtp.gmail.com") {
		t.Fatalf("non-sensitive grouped attribute was lost: %s", raw)
	}
}

func TestRedactionAppliesToWithAttrs(t *testing.T) {
	// A logger built with .With() must redact too, otherwise a long-lived request
	// logger could carry a secret on every line it ever writes.
	var buf bytes.Buffer
	logger := newTestLogger(&buf).With(slog.String("session_token", "leak-me"))
	logger.Info("request")

	if strings.Contains(buf.String(), "leak-me") {
		t.Fatalf("value attached via With() leaked: %s", buf.String())
	}
}

func TestContextIdentifiersAreInjected(t *testing.T) {
	var buf bytes.Buffer
	ctx := telemetry.WithRequestID(context.Background(), "req-123")
	ctx = telemetry.WithUserID(ctx, "user-456")
	ctx = telemetry.WithCorrelationID(ctx, "corr-789")

	newTestLogger(&buf).InfoContext(ctx, "handled")

	m := decode(t, &buf)
	if m["request_id"] != "req-123" {
		t.Errorf("request_id = %v, want req-123", m["request_id"])
	}
	if m["user_id"] != "user-456" {
		t.Errorf("user_id = %v, want user-456", m["user_id"])
	}
	if m["correlation_id"] != "corr-789" {
		t.Errorf("correlation_id = %v, want corr-789", m["correlation_id"])
	}
}

func TestContextIdentifiersAreOmittedWhenAbsent(t *testing.T) {
	var buf bytes.Buffer
	newTestLogger(&buf).InfoContext(context.Background(), "no ids")

	m := decode(t, &buf)
	for _, k := range []string{"request_id", "user_id", "correlation_id", "trace_id"} {
		if _, ok := m[k]; ok {
			t.Errorf("%q present with no value in context", k)
		}
	}
}

func TestShouldRedactIsCaseAndSeparatorInsensitive(t *testing.T) {
	for _, key := range []string{"Password", "PASSWORD", "user-password", "user.password", "SMTPPassword", "X-Api-Key"} {
		if !telemetry.ShouldRedact(key) {
			t.Errorf("ShouldRedact(%q) = false, want true", key)
		}
	}
	for _, key := range []string{"email", "user_id", "loop_id", "match_score", "method", "status"} {
		if telemetry.ShouldRedact(key) {
			t.Errorf("ShouldRedact(%q) = true, want false", key)
		}
	}
}
