package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/anthropics/anthropic-sdk-go/v2/option"
)

// AnthropicProvider calls the Claude API.
type AnthropicProvider struct {
	client anthropic.Client
}

// AnthropicConfig configures the provider.
type AnthropicConfig struct {
	// APIKey may be empty, in which case the SDK resolves credentials itself — an
	// environment variable, or an OAuth profile from `ant auth login`. An unset
	// ANTHROPIC_API_KEY does not mean there are no credentials.
	APIKey string
	// BaseURL overrides the endpoint, for a gateway or a test double.
	BaseURL string
	// MaxRetries bounds the SDK's own transport-level retries. Kept low because this
	// package implements its own retry policy on top, and two layers of unbounded retry
	// multiply into a long stall on a failing provider.
	MaxRetries int
}

// NewAnthropicProvider builds the Claude provider.
func NewAnthropicProvider(cfg AnthropicConfig) *AnthropicProvider {
	opts := []option.RequestOption{}
	if cfg.APIKey != "" {
		opts = append(opts, option.WithAPIKey(cfg.APIKey))
	}
	if cfg.BaseURL != "" {
		opts = append(opts, option.WithBaseURL(cfg.BaseURL))
	}
	maxRetries := cfg.MaxRetries
	if maxRetries <= 0 {
		maxRetries = 1
	}
	opts = append(opts, option.WithMaxRetries(maxRetries))

	return &AnthropicProvider{client: anthropic.NewClient(opts...)}
}

// Name identifies the provider.
func (p *AnthropicProvider) Name() string { return "anthropic" }

// Generate performs one structured-output call.
func (p *AnthropicProvider) Generate(ctx context.Context, req Request, repairHint string) (Response, error) {
	userContent, err := req.Prompt.UserContent()
	if err != nil {
		return Response{}, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}

	// The system prompt is split so the stable part can carry a cache breakpoint. The
	// role and injection rules are identical across calls of the same purpose; the task
	// varies. Caching the whole thing would never hit, and the miss would be silent.
	system := []anthropic.TextBlockParam{{
		Text:         req.Prompt.CacheablePrefix(),
		CacheControl: anthropic.NewCacheControlEphemeralParam(),
	}}
	if task := req.Prompt.TaskSuffix(); task != "" {
		system = append(system, anthropic.TextBlockParam{Text: task})
	}

	userBlocks := []anthropic.ContentBlockParamUnion{anthropic.NewTextBlock(userContent)}
	if repairHint != "" {
		// The hint goes in the user turn, after the data. Putting it in the system
		// prompt would invalidate the cached prefix on every repair.
		userBlocks = append(userBlocks, anthropic.NewTextBlock(repairHint))
	}

	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(req.Model),
		MaxTokens: req.MaxTokens,
		System:    system,
		Messages:  []anthropic.MessageParam{anthropic.NewUserMessage(userBlocks...)},
		OutputConfig: anthropic.OutputConfigParam{
			Effort: anthropic.OutputConfigEffort(req.Effort),
			// Structured outputs: the API constrains the response to this schema, which
			// removes the whole class of "the model wrapped the JSON in prose" failures
			// that prompt-only approaches never fully eliminate.
			Format: anthropic.JSONOutputFormatParam{Schema: req.Schema},
		},
	}

	message, err := p.client.Messages.New(ctx, params)
	if err != nil {
		return Response{}, translateError(err)
	}

	usage := Usage{
		InputTokens:         message.Usage.InputTokens,
		OutputTokens:        message.Usage.OutputTokens,
		CacheReadTokens:     message.Usage.CacheReadInputTokens,
		CacheCreationTokens: message.Usage.CacheCreationInputTokens,
	}
	resp := Response{Usage: usage, Model: req.Model}

	// Stop reason is checked BEFORE the content is read. A refusal returns HTTP 200
	// with no usable content, so reading content first would produce a confusing empty
	// result instead of a clear "the provider declined this".
	switch message.StopReason {
	case anthropic.StopReasonRefusal:
		category, explanation := refusalDetail(message)
		return resp, fmt.Errorf("%w (category %q): %s", ErrRefused, category, explanation)

	case anthropic.StopReasonMaxTokens:
		// A truncated JSON document cannot be repaired by asking again — the request
		// needs a larger budget or a smaller input, both of which are our decision.
		return resp, fmt.Errorf("%w: hit the %d token limit; the response is incomplete",
			ErrTruncated, req.MaxTokens)

	case anthropic.StopReasonModelContextWindowExceeded:
		return resp, fmt.Errorf("%w: the input exceeds the model's context window",
			ErrInvalidRequest)
	}

	body, err := extractJSON(message)
	if err != nil {
		return resp, err
	}
	resp.JSON = body
	return resp, nil
}

// refusalDetail reads the structured refusal reason, which is populated only when the
// stop reason is a refusal.
func refusalDetail(message *anthropic.Message) (category, explanation string) {
	category = string(message.StopDetails.Category)
	explanation = message.StopDetails.Explanation
	if category == "" {
		category = "unspecified"
	}
	if explanation == "" {
		explanation = "the provider gave no explanation"
	}
	return category, explanation
}

// extractJSON pulls the structured body out of the response.
//
// With a response schema set, the body arrives as text that is already constrained to
// the schema. It is still parsed here: "the API said it would be valid" is not the same
// as "it is valid", and this layer's contract is that nothing unvalidated reaches a
// domain package.
func extractJSON(message *anthropic.Message) ([]byte, error) {
	var collected strings.Builder
	for _, block := range message.Content {
		if text, ok := block.AsAny().(anthropic.TextBlock); ok {
			collected.WriteString(text.Text)
		}
		// Thinking blocks are ignored. They are not part of the answer, and with the
		// default display setting they are empty anyway.
	}

	raw := strings.TrimSpace(collected.String())
	if raw == "" {
		return nil, fmt.Errorf("%w: the response contained no text", ErrSchemaViolation)
	}

	// Strip a markdown fence if one appears. It should not with structured outputs, but
	// stripping is cheap and the alternative is a confusing parse failure.
	raw = stripCodeFence(raw)

	if !json.Valid([]byte(raw)) {
		// The message is kept short and structural. It becomes a repair hint sent back
		// to the model, and it must not echo the content — that content is the
		// candidate's CV.
		return nil, fmt.Errorf("%w: the response was not valid JSON (%d bytes)",
			ErrSchemaViolation, len(raw))
	}
	return []byte(raw), nil
}

// stripCodeFence removes a surrounding markdown code fence.
func stripCodeFence(s string) string {
	if !strings.HasPrefix(s, "```") {
		return s
	}
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[i+1:]
	}
	return strings.TrimSuffix(strings.TrimSpace(s), "```")
}

// Ping checks reachability with the cheapest possible call.
//
// A one-token request against the fast model: enough to prove credentials and network
// work, cheap enough to run on every readiness probe without the probe becoming a
// line item.
func (p *AnthropicProvider) Ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	_, err := p.client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     anthropic.Model(ModelFast),
		MaxTokens: 1,
		Messages:  []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("ping"))},
	})
	if err != nil {
		return translateError(err)
	}
	return nil
}

// translateError maps an SDK error onto this package's taxonomy.
//
// Branching on the status code rather than the message, because the SDK documents that
// the message wording can change. Each case maps to a different reaction: retry, fix the
// request, or alert an operator.
func translateError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%w: the call timed out", ErrUnavailable)
	}
	if errors.Is(err, context.Canceled) {
		return err
	}

	var apiErr *anthropic.Error
	if !errors.As(err, &apiErr) {
		// No status code: a transport failure. Retryable.
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}

	switch apiErr.StatusCode {
	case http.StatusTooManyRequests:
		return fmt.Errorf("%w: request %s", ErrRateLimited, apiErr.RequestID)

	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		// Our fault — a malformed request or a parameter the model rejects. Retrying
		// an identical request cannot help, so this must not be retryable.
		return fmt.Errorf("%w: %s (request %s)", ErrInvalidRequest, apiErr.Message, apiErr.RequestID)

	case http.StatusUnauthorized, http.StatusForbidden:
		// A credential problem needs an operator, not a retry. Marked non-retryable so
		// it surfaces immediately rather than after exhausting redelivery.
		return fmt.Errorf("%w: provider rejected our credentials: %s",
			ErrInvalidRequest, apiErr.Message)

	case http.StatusRequestEntityTooLarge:
		return fmt.Errorf("%w: the request is too large for the provider", ErrInvalidRequest)

	default:
		if apiErr.StatusCode >= 500 {
			return fmt.Errorf("%w: provider returned %d (request %s)",
				ErrUnavailable, apiErr.StatusCode, apiErr.RequestID)
		}
		return fmt.Errorf("%w: provider returned %d: %s",
			ErrInvalidRequest, apiErr.StatusCode, apiErr.Message)
	}
}
