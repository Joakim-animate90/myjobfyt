package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// Client wraps a Provider with the policy every call must obey: quota enforcement before
// the call, schema validation with bounded repair after it, token accounting, and metrics.
//
// Domains use this, never a Provider directly, so none of that policy can be skipped by
// a new feature that forgets it.
type Client struct {
	provider   Provider
	accountant TokenAccountant
	log        *slog.Logger
}

// ClientDeps are the client's collaborators.
type ClientDeps struct {
	Provider Provider
	// Accountant may be nil, which disables quota enforcement. Only acceptable in tests
	// and in tools; the API and worker always supply one.
	Accountant TokenAccountant
	Logger     *slog.Logger
}

// NewClient builds the AI client.
func NewClient(d ClientDeps) *Client {
	return &Client{provider: d.Provider, accountant: d.Accountant, log: d.Logger}
}

// Name returns the underlying provider's name.
func (c *Client) Name() string { return c.provider.Name() }

// Ping checks provider reachability.
func (c *Client) Ping(ctx context.Context) error { return c.provider.Ping(ctx) }

// Generate performs a structured-output call and unmarshals the validated response.
//
// The generic parameter is the caller's result type. Unmarshalling here rather than in
// each domain means no caller can skip validation and read the raw text.
func Generate[T any](ctx context.Context, c *Client, req Request) (T, Response, error) {
	var zero T

	resp, err := c.generate(ctx, req)
	if err != nil {
		return zero, resp, err
	}

	var out T
	if uerr := json.Unmarshal(resp.JSON, &out); uerr != nil {
		// Should be impossible: the body already validated against the schema. If it
		// happens, the schema and the Go type disagree, which is a bug worth surfacing
		// loudly rather than papering over.
		return zero, resp, fmt.Errorf("%w: response validated against the schema but does not fit %T: %v",
			ErrSchemaViolation, out, uerr)
	}
	return out, resp, nil
}

// generate runs the call with quota enforcement, repair retries and accounting.
func (c *Client) generate(ctx context.Context, req Request) (Response, error) {
	if err := req.Validate(); err != nil {
		return Response{}, err
	}
	req.applyDefaults()

	// The monthly quota is checked BEFORE the call. Checking after would mean the
	// candidate has already been charged for tokens they were not entitled to, and the
	// platform has already paid for them.
	estimate := estimateTokens(req)
	if req.MaxTokenBudget > 0 && estimate > req.MaxTokenBudget {
		return Response{}, fmt.Errorf("%w: estimated %d tokens exceeds the per-call budget of %d",
			ErrBudgetExceeded, estimate, req.MaxTokenBudget)
	}
	if c.accountant != nil {
		if err := c.accountant.Reserve(ctx, estimate); err != nil {
			return Response{}, err
		}
	}

	callCtx, cancel := context.WithTimeout(ctx, req.Timeout)
	defer cancel()

	var (
		total      Usage
		lastErr    error
		repairHint string
	)

	for attempt := 1; attempt <= maxSchemaRepairs+1; attempt++ {
		start := time.Now()
		resp, err := c.provider.Generate(callCtx, req, repairHint)

		// Usage accumulates across repair attempts: a failed attempt still cost tokens,
		// and not counting them would understate spend precisely on the calls that
		// spend most.
		total.InputTokens += resp.Usage.InputTokens
		total.OutputTokens += resp.Usage.OutputTokens
		total.CacheReadTokens += resp.Usage.CacheReadTokens
		total.CacheCreationTokens += resp.Usage.CacheCreationTokens

		observeCall(req.Purpose, string(req.Model), err, time.Since(start), resp.Usage)

		if err == nil {
			resp.Usage = total
			resp.Attempts = attempt
			c.record(ctx, req, total, attempt, resp.Usage.CacheHit())
			return resp, nil
		}

		lastErr = err

		// Only a schema violation is worth repairing. A refusal, a truncation or an
		// outage will not be fixed by asking again with a hint.
		if !errors.Is(err, ErrSchemaViolation) {
			break
		}

		repairHint = schemaRepairHint(err)
		c.log.WarnContext(ctx, "model output failed schema validation; retrying with a repair hint",
			slog.String("purpose", string(req.Purpose)),
			slog.Int("attempt", attempt),
			slog.Int("max_attempts", maxSchemaRepairs+1),
		)
	}

	// Spend on failed attempts is still recorded. The candidate's quota is not charged
	// for our failure — Record is told the usage so cost reporting is accurate — but
	// a purpose that burns tokens without producing output needs to be visible.
	c.record(ctx, req, total, maxSchemaRepairs+1, false)

	return Response{Usage: total, Model: req.Model, Attempts: maxSchemaRepairs + 1}, lastErr
}

// record reports usage, logging rather than failing on error.
//
// An accounting failure must not fail an operation whose work is already done: the
// alternative is discarding a successful CV extraction because a counter was
// unreachable.
func (c *Client) record(ctx context.Context, req Request, usage Usage, attempts int, cacheHit bool) {
	// Token counts are deliberately loggable — they are allowlisted in the redaction
	// layer precisely so AI spend stays visible. The prompt and completion bodies are
	// not, and never appear here.
	c.log.InfoContext(ctx, "ai call completed",
		slog.String("purpose", string(req.Purpose)),
		slog.String("model", string(req.Model)),
		slog.String("effort", string(req.Effort)),
		slog.Int64("prompt_tokens", usage.InputTokens),
		slog.Int64("completion_tokens", usage.OutputTokens),
		slog.Int64("total_tokens", usage.TotalTokens()),
		slog.Int64("cache_read_tokens", usage.CacheReadTokens),
		slog.Bool("cache_hit", cacheHit),
		slog.Int("attempts", attempts),
	)

	if c.accountant == nil {
		return
	}
	if err := c.accountant.Record(ctx, string(req.Purpose), usage.TotalTokens()); err != nil {
		c.log.ErrorContext(ctx, "ai token usage was not recorded",
			slog.String("purpose", string(req.Purpose)), slog.Any("error", err))
	}
}

// estimateTokens approximates a call's cost for the pre-call quota check.
//
// Roughly four characters per token for English prose. Deliberately approximate: the
// true figure is unknown until the response arrives, and the alternative — a
// count_tokens round trip before every call — doubles the request count to refine a
// number that Record corrects moments later anyway.
func estimateTokens(req Request) int64 {
	chars := len(req.Prompt.SystemPrompt())
	for _, block := range req.Prompt.Data {
		chars += len(block.Content)
	}
	// Schema tokens count too; it is sent on every request.
	if schema, err := json.Marshal(req.Schema); err == nil {
		chars += len(schema)
	}
	return int64(chars/4) + req.MaxTokens
}

// schemaRepairHint turns a validation failure into an instruction for the next attempt.
//
// Naming the specific problem works far better than repeating the schema: a model told
// "field experience[0].start_date was missing" fixes that field, while one told "match
// the schema" often returns the same output.
func schemaRepairHint(err error) string {
	return fmt.Sprintf(
		"Your previous response did not satisfy the required schema: %s\n\n"+
			"Return only a JSON document matching the schema exactly. Do not add "+
			"commentary, markdown fences, or fields the schema does not define. If a "+
			"value is genuinely not present in the data, use the schema's null or empty "+
			"representation rather than inventing one.",
		cleanValidationMessage(err))
}

// cleanValidationMessage strips our own error wrapping so the hint reads as a statement
// about the output rather than about our code.
func cleanValidationMessage(err error) string {
	msg := err.Error()
	const prefix = "model output did not match the schema: "
	if len(msg) > len(prefix) && msg[:len(prefix)] == prefix {
		return msg[len(prefix):]
	}
	return msg
}
