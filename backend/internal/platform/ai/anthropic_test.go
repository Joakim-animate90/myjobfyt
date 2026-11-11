package ai_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Joakim-animate90/myjobfyt/backend/internal/platform/ai"
)

// These tests exercise the real AnthropicProvider against a local server speaking the
// Messages API shape. That is the only code in this package the fake provider does not
// cover, and it is where an SDK-binding mistake would hide — a misnamed field marshals
// silently and only fails against the real API, in production, costing money.

// recordedRequest is a captured request body.
type recordedRequest struct {
	System       []map[string]any `json:"system"`
	Messages     []map[string]any `json:"messages"`
	Model        string           `json:"model"`
	MaxTokens    int64            `json:"max_tokens"`
	OutputConfig map[string]any   `json:"output_config"`
}

// apiStub serves canned Messages API responses and records what was sent.
type apiStub struct {
	mu       sync.Mutex
	requests []recordedRequest
	// status and body are the scripted response.
	status int
	body   string
}

func (s *apiStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)

	var captured recordedRequest
	_ = json.Unmarshal(raw, &captured)

	s.mu.Lock()
	s.requests = append(s.requests, captured)
	status, body := s.status, s.body
	s.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("request-id", "req_test_123")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

func (s *apiStub) lastRequest(t *testing.T) recordedRequest {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.requests) == 0 {
		t.Fatal("no request was captured")
	}
	return s.requests[len(s.requests)-1]
}

func (s *apiStub) requestCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.requests)
}

// successBody builds a Messages API success response carrying the given text.
func successBody(text string, inputTokens, outputTokens int64) string {
	content, _ := json.Marshal([]map[string]any{{"type": "text", "text": text}})
	return fmt.Sprintf(`{
		"id": "msg_test",
		"type": "message",
		"role": "assistant",
		"model": "claude-opus-5-5",
		"content": %s,
		"stop_reason": "end_turn",
		"usage": {
			"input_tokens": %d,
			"output_tokens": %d,
			"cache_read_input_tokens": 0,
			"cache_creation_input_tokens": 0
		}
	}`, content, inputTokens, outputTokens)
}

func newStubbedProvider(t *testing.T, status int, body string) (*ai.AnthropicProvider, *apiStub) {
	t.Helper()
	stub := &apiStub{status: status, body: body}
	server := httptest.NewServer(stub)
	t.Cleanup(server.Close)

	provider := ai.NewAnthropicProvider(ai.AnthropicConfig{
		APIKey:  "test-key-not-real",
		BaseURL: server.URL,
		// No transport retries, so a scripted error surfaces once rather than being
		// retried into a timeout.
		MaxRetries: 1,
	})
	return provider, stub
}

func providerRequest() ai.Request {
	req := ai.Request{
		Purpose: ai.PurposeCVExtract,
		Prompt: ai.Prompt{
			Role: "You are a CV parser.",
			Task: "Extract the name.",
			Data: []ai.UntrustedBlock{{Label: "CANDIDATE_CV", Content: "Joakim, 6 years at Acme"}},
		},
		Schema:    testSchema(),
		Model:     ai.ModelDefault,
		Effort:    ai.EffortHigh,
		MaxTokens: 4096,
	}
	return req
}

func TestProviderSendsTheStructuredOutputSchema(t *testing.T) {
	// If this field is misnamed the request still marshals and the model returns prose,
	// which then fails schema validation for a reason that looks like a model problem.
	provider, stub := newStubbedProvider(t, http.StatusOK,
		successBody(`{"name":"Joakim","years":6}`, 1200, 80))

	req := providerRequest()
	if _, err := provider.Generate(context.Background(), req, ""); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	captured := stub.lastRequest(t)
	if captured.Model != string(ai.ModelDefault) {
		t.Errorf("model = %q, want %q", captured.Model, ai.ModelDefault)
	}
	if captured.MaxTokens != 4096 {
		t.Errorf("max_tokens = %d, want 4096", captured.MaxTokens)
	}

	format, ok := captured.OutputConfig["format"].(map[string]any)
	if !ok {
		t.Fatalf("output_config.format is %T, want an object: %+v", captured.OutputConfig["format"], captured.OutputConfig)
	}
	if format["type"] != "json_schema" {
		t.Errorf("format.type = %v, want json_schema", format["type"])
	}
	if _, present := format["schema"]; !present {
		t.Error("format.schema is absent; the response would be unconstrained")
	}
	if captured.OutputConfig["effort"] != "high" {
		t.Errorf("output_config.effort = %v, want high", captured.OutputConfig["effort"])
	}
}

func TestProviderPutsTheCacheBreakpointOnTheStableSystemBlock(t *testing.T) {
	// The role and injection rules are identical across calls; the task is not.
	// Caching the whole system prompt would never hit, and the miss is silent — it
	// shows up only as cost.
	provider, stub := newStubbedProvider(t, http.StatusOK,
		successBody(`{"name":"Joakim","years":6}`, 1200, 80))

	if _, err := provider.Generate(context.Background(), providerRequest(), ""); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	captured := stub.lastRequest(t)
	if len(captured.System) != 2 {
		t.Fatalf("%d system blocks, want 2 (cacheable prefix + per-call task)", len(captured.System))
	}

	first := captured.System[0]
	if _, cached := first["cache_control"]; !cached {
		t.Error("the first system block carries no cache_control")
	}
	text, _ := first["text"].(string)
	if !strings.Contains(text, "You are a CV parser.") {
		t.Errorf("the cacheable block is missing the role: %q", text)
	}
	// The per-call task must NOT be in the cached block.
	if strings.Contains(text, "Extract the name.") {
		t.Error("the per-call task is inside the cached prefix; caching would never hit")
	}

	second := captured.System[1]
	if _, cached := second["cache_control"]; cached {
		t.Error("the per-call task block is marked cacheable")
	}
	if taskText, _ := second["text"].(string); !strings.Contains(taskText, "Extract the name.") {
		t.Errorf("the second block is missing the task: %q", taskText)
	}
}

func TestProviderSendsTheCVInANonceDelimitedUserBlock(t *testing.T) {
	// The CV must reach the model as data, never in the instruction stream.
	provider, stub := newStubbedProvider(t, http.StatusOK,
		successBody(`{"name":"Joakim","years":6}`, 1200, 80))

	if _, err := provider.Generate(context.Background(), providerRequest(), ""); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	captured := stub.lastRequest(t)
	if len(captured.Messages) != 1 {
		t.Fatalf("%d messages, want 1", len(captured.Messages))
	}

	body, _ := json.Marshal(captured.Messages[0])
	userText := string(body)

	if !strings.Contains(userText, "Joakim, 6 years at Acme") {
		t.Error("the CV text did not reach the user message")
	}
	if !strings.Contains(userText, "CANDIDATE_CV") {
		t.Error("the data block label is missing")
	}
	if !strings.Contains(userText, `id=\"`) && !strings.Contains(userText, `id="`) {
		t.Errorf("no nonce marker in the user content: %s", userText)
	}

	// And it must not have leaked into the system prompt.
	for _, block := range captured.System {
		if text, _ := block["text"].(string); strings.Contains(text, "Joakim, 6 years at Acme") {
			t.Fatal("the CV text leaked into the system prompt")
		}
	}
}

func TestProviderAppendsARepairHintToTheUserTurn(t *testing.T) {
	// The hint must not go in the system prompt: that would invalidate the cached
	// prefix on every repair, so a repair would cost more than the original call.
	provider, stub := newStubbedProvider(t, http.StatusOK,
		successBody(`{"name":"Joakim","years":6}`, 1200, 80))

	hint := "Your previous response was not valid JSON."
	if _, err := provider.Generate(context.Background(), providerRequest(), hint); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	captured := stub.lastRequest(t)
	body, _ := json.Marshal(captured.Messages[0])
	if !strings.Contains(string(body), "not valid JSON") {
		t.Error("the repair hint did not reach the user turn")
	}
	for _, block := range captured.System {
		if text, _ := block["text"].(string); strings.Contains(text, "not valid JSON") {
			t.Fatal("the repair hint went into the system prompt, invalidating the cache")
		}
	}
}

func TestProviderReportsUsage(t *testing.T) {
	provider, _ := newStubbedProvider(t, http.StatusOK,
		successBody(`{"name":"Joakim","years":6}`, 1234, 567))

	resp, err := provider.Generate(context.Background(), providerRequest(), "")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if resp.Usage.InputTokens != 1234 || resp.Usage.OutputTokens != 567 {
		t.Fatalf("usage = %+v, want 1234/567", resp.Usage)
	}
	if resp.Usage.TotalTokens() != 1801 {
		t.Errorf("TotalTokens = %d, want 1801", resp.Usage.TotalTokens())
	}
}

func TestProviderStripsAMarkdownFence(t *testing.T) {
	// Should not happen with structured outputs, but stripping is cheap and the
	// alternative is a parse failure that reads as a model problem.
	provider, _ := newStubbedProvider(t, http.StatusOK,
		successBody("```json\n{\"name\":\"Joakim\",\"years\":6}\n```", 100, 20))

	resp, err := provider.Generate(context.Background(), providerRequest(), "")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	var out extracted
	if err := json.Unmarshal(resp.JSON, &out); err != nil {
		t.Fatalf("the fenced response did not parse: %v (body %q)", err, resp.JSON)
	}
	if out.Name != "Joakim" {
		t.Fatalf("decoded = %+v", out)
	}
}

func TestProviderRejectsANonJSONResponseAsASchemaViolation(t *testing.T) {
	// Must be a schema violation rather than a generic error, because only that is
	// worth a repair attempt.
	provider, _ := newStubbedProvider(t, http.StatusOK,
		successBody("I am afraid I cannot help with that.", 100, 20))

	_, err := provider.Generate(context.Background(), providerRequest(), "")
	if !errors.Is(err, ai.ErrSchemaViolation) {
		t.Fatalf("err = %v, want ErrSchemaViolation", err)
	}
	// The error becomes a repair hint sent back to the model, so it must not echo the
	// content — that content is the candidate's CV.
	if strings.Contains(err.Error(), "cannot help with that") {
		t.Fatalf("the error echoes the response content: %v", err)
	}
}

func TestProviderRejectsAnEmptyResponse(t *testing.T) {
	provider, _ := newStubbedProvider(t, http.StatusOK, successBody("", 100, 0))

	_, err := provider.Generate(context.Background(), providerRequest(), "")
	if !errors.Is(err, ai.ErrSchemaViolation) {
		t.Fatalf("err = %v, want ErrSchemaViolation", err)
	}
}

func TestProviderHandlesARefusalBeforeReadingContent(t *testing.T) {
	// A refusal returns HTTP 200 with no usable content. Reading content first would
	// produce a confusing empty result instead of a clear "the provider declined".
	body := `{
		"id": "msg_test", "type": "message", "role": "assistant",
		"model": "claude-opus-5-5",
		"content": [],
		"stop_reason": "refusal",
		"stop_details": {"type": "refusal", "category": "bio", "explanation": "declined by policy"},
		"usage": {"input_tokens": 500, "output_tokens": 0,
		          "cache_read_input_tokens": 0, "cache_creation_input_tokens": 0}
	}`
	provider, _ := newStubbedProvider(t, http.StatusOK, body)

	resp, err := provider.Generate(context.Background(), providerRequest(), "")
	if !errors.Is(err, ai.ErrRefused) {
		t.Fatalf("err = %v, want ErrRefused", err)
	}
	// The category must survive: on this platform a refusal usually means a CV or job
	// description contained something the classifier objected to, and an operator needs
	// to know which.
	if !strings.Contains(err.Error(), "bio") {
		t.Errorf("the error does not carry the refusal category: %v", err)
	}
	// Usage is still reported, because a refused call is still billed.
	if resp.Usage.InputTokens != 500 {
		t.Errorf("usage = %+v; a refused call is still billed and must be counted", resp.Usage)
	}
}

func TestProviderTreatsTruncationAsTerminal(t *testing.T) {
	// A truncated JSON document cannot be repaired by asking again: the request needs a
	// larger budget or a smaller input, both of which are our decision.
	body := `{
		"id": "msg_test", "type": "message", "role": "assistant",
		"model": "claude-opus-5-5",
		"content": [{"type": "text", "text": "{\"name\":\"Joak"}],
		"stop_reason": "max_tokens",
		"usage": {"input_tokens": 500, "output_tokens": 4096,
		          "cache_read_input_tokens": 0, "cache_creation_input_tokens": 0}
	}`
	provider, _ := newStubbedProvider(t, http.StatusOK, body)

	_, err := provider.Generate(context.Background(), providerRequest(), "")
	if !errors.Is(err, ai.ErrTruncated) {
		t.Fatalf("err = %v, want ErrTruncated", err)
	}
	if ai.Retryable(err) {
		t.Error("truncation was classified retryable; retrying costs money for the same outcome")
	}
}

func TestProviderTranslatesStatusCodes(t *testing.T) {
	// Branching on the status code rather than the message, which the SDK documents as
	// changeable. Each case maps to a different reaction.
	cases := []struct {
		name      string
		status    int
		body      string
		want      error
		retryable bool
	}{
		{"rate limited", http.StatusTooManyRequests,
			`{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`,
			ai.ErrRateLimited, true},
		{"bad request", http.StatusBadRequest,
			`{"type":"error","error":{"type":"invalid_request_error","message":"bad schema"}}`,
			ai.ErrInvalidRequest, false},
		{"unauthorized", http.StatusUnauthorized,
			`{"type":"error","error":{"type":"authentication_error","message":"bad key"}}`,
			ai.ErrInvalidRequest, false},
		{"forbidden", http.StatusForbidden,
			`{"type":"error","error":{"type":"permission_error","message":"no access"}}`,
			ai.ErrInvalidRequest, false},
		{"server error", http.StatusInternalServerError,
			`{"type":"error","error":{"type":"api_error","message":"boom"}}`,
			ai.ErrUnavailable, true},
		{"bad gateway", http.StatusBadGateway,
			`{"type":"error","error":{"type":"api_error","message":"boom"}}`,
			ai.ErrUnavailable, true},
		{"overloaded", http.StatusServiceUnavailable,
			`{"type":"error","error":{"type":"overloaded_error","message":"overloaded"}}`,
			ai.ErrUnavailable, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			provider, _ := newStubbedProvider(t, tc.status, tc.body)

			_, err := provider.Generate(context.Background(), providerRequest(), "")
			if !errors.Is(err, tc.want) {
				t.Fatalf("status %d gave %v, want %v", tc.status, err, tc.want)
			}
			if got := ai.Retryable(err); got != tc.retryable {
				t.Errorf("Retryable = %v, want %v", got, tc.retryable)
			}
		})
	}
}

func TestACredentialFailureIsNotRetryable(t *testing.T) {
	// It needs an operator, not a retry. Marked non-retryable so it surfaces
	// immediately rather than after exhausting redelivery.
	provider, _ := newStubbedProvider(t, http.StatusUnauthorized,
		`{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`)

	_, err := provider.Generate(context.Background(), providerRequest(), "")
	if ai.Retryable(err) {
		t.Fatal("a credential failure was classified retryable")
	}
}

func TestProviderRespectsAnAlreadyCancelledContext(t *testing.T) {
	provider, stub := newStubbedProvider(t, http.StatusOK,
		successBody(`{"name":"Joakim","years":6}`, 100, 20))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := provider.Generate(ctx, providerRequest(), ""); err == nil {
		t.Fatal("Generate ignored a cancelled context")
	}
	if stub.requestCount() != 0 {
		t.Errorf("%d requests were sent on a cancelled context", stub.requestCount())
	}
}

func TestProviderNameIsStable(t *testing.T) {
	// Used as a metric label and in logs, so it must not drift.
	provider, _ := newStubbedProvider(t, http.StatusOK, successBody(`{}`, 1, 1))
	if got := provider.Name(); got != "anthropic" {
		t.Fatalf("Name() = %q, want anthropic", got)
	}
}

func TestClientAndProviderWorkTogether(t *testing.T) {
	// End to end through the real provider: the client's quota, repair and accounting
	// policy over the real request construction and response parsing.
	provider, stub := newStubbedProvider(t, http.StatusOK,
		successBody(`{"name":"Joakim","years":6}`, 1200, 80))

	accountant := &fakeAccountant{}
	client := newClient(t, provider, accountant)

	out, resp, err := ai.Generate[extracted](context.Background(), client, providerRequest())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if out.Name != "Joakim" || out.Years != 6 {
		t.Fatalf("decoded = %+v", out)
	}
	if resp.Usage.TotalTokens() != 1280 {
		t.Errorf("TotalTokens = %d, want 1280", resp.Usage.TotalTokens())
	}
	if stub.requestCount() != 1 {
		t.Errorf("%d requests, want 1", stub.requestCount())
	}
	if recorded := accountant.totalRecorded(); recorded != 1280 {
		t.Errorf("recorded tokens = %d, want 1280", recorded)
	}
}
