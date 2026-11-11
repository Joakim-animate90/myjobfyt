package ai

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Model identifies a model. Plain strings rather than an enum, because the SDK's typed
// constants lag model launches and a stale constant list would pin us to an older model.
type Model string

const (
	// ModelDefault is the default for everything. The most capable widely-available
	// model, because the work here — reading a stranger's CV correctly, refusing to
	// fabricate under pressure from an injected instruction — is exactly the kind that
	// rewards capability.
	ModelDefault Model = "claude-opus-5-5"
	// ModelFast is for high-volume, low-judgement work such as skill normalisation.
	ModelFast Model = "claude-haiku-5-5"
	// ModelBalanced sits between them, for bulk work that still needs judgement.
	ModelBalanced Model = "claude-sonnet-5-5"
)

// Effort controls how much thinking the model does, trading tokens for thoroughness.
type Effort string

const (
	EffortLow    Effort = "low"
	EffortMedium Effort = "medium"
	EffortHigh   Effort = "high"
	EffortXHigh  Effort = "xhigh"
	EffortMax    Effort = "max"
)

// Usage reports what a call consumed.
type Usage struct {
	InputTokens         int64
	OutputTokens        int64
	CacheReadTokens     int64
	CacheCreationTokens int64
}

// TotalTokens is what the quota is denominated in.
func (u Usage) TotalTokens() int64 { return u.InputTokens + u.OutputTokens }

// CacheHit reports whether any of the prompt was served from cache, which is the only
// reliable signal that caching is actually working. A silent cache miss is the normal
// failure mode, so this is worth surfacing.
func (u Usage) CacheHit() bool { return u.CacheReadTokens > 0 }

// Request describes one structured-output call.
type Request struct {
	// Purpose attributes the cost and must be set.
	Purpose Purpose
	// Prompt carries the instructions and any untrusted data.
	Prompt Prompt
	// Schema is the JSON Schema the response must satisfy. Required: this layer has no
	// free-text mode, because unvalidated model output reaching business logic is the
	// failure this package exists to prevent.
	Schema map[string]any
	// Model defaults to ModelDefault.
	Model Model
	// Effort defaults to EffortMedium, which is also the model's own default.
	Effort Effort
	// MaxTokens bounds the response. Defaults to defaultMaxTokens.
	MaxTokens int64
	// Timeout bounds the call. Defaults to defaultTimeout.
	Timeout time.Duration
	// MaxTokenBudget caps what this single call may consume, independent of the monthly
	// quota. Without it one pathological document — a 200-page PDF, or a retry loop —
	// could consume a large share of a candidate's month in minutes.
	MaxTokenBudget int64
}

// Response is a validated structured response.
type Response struct {
	// JSON is the raw response body, already validated against the schema.
	JSON  []byte
	Usage Usage
	Model Model
	// Attempts is how many model calls were made, including schema repairs. Above 1
	// means the first response did not validate, which is worth knowing: a purpose
	// whose attempts climb has a prompt or schema problem.
	Attempts int
	// Degraded is set when the result came from a reduced path and the caller must say
	// so rather than presenting it as complete.
	Degraded bool
}

// Errors this package returns. Each is distinguished because the correct reaction
// differs: retry, escalate to a human, or tell the candidate something specific.
var (
	// ErrUnavailable means the provider could not be reached or is failing. Retryable.
	ErrUnavailable = errors.New("ai provider unavailable")
	// ErrRateLimited means the provider is throttling us. Retryable after a delay.
	ErrRateLimited = errors.New("ai provider rate limited")
	// ErrSchemaViolation means the model never produced output matching the schema,
	// after repairs. Terminal: retrying the same prompt will not help.
	ErrSchemaViolation = errors.New("model output did not match the schema")
	// ErrRefused means the provider's safety classifiers declined the request. Terminal,
	// and worth surfacing distinctly — on this platform it most often means a CV or job
	// description contained something the classifier objected to, which an operator
	// needs to see rather than have retried silently.
	ErrRefused = errors.New("provider declined the request")
	// ErrBudgetExceeded means the call would exceed its per-call token budget.
	ErrBudgetExceeded = errors.New("call exceeds its token budget")
	// ErrTruncated means the response hit MaxTokens mid-output. Terminal for a
	// structured call: a truncated JSON document is not repairable by retrying the same
	// request, it needs a larger budget or a smaller input.
	ErrTruncated = errors.New("model output was truncated")
	// ErrInvalidRequest means we built a bad request. A bug, not a transient failure.
	ErrInvalidRequest = errors.New("invalid ai request")
)

// Retryable reports whether an error could plausibly succeed on retry.
//
// Used by the worker runtime to decide between backoff and dead-lettering. Schema
// violations, refusals and truncation are deliberately not retryable: retrying them
// costs money for the same outcome and delays telling the candidate something actionable.
func Retryable(err error) bool {
	switch {
	case errors.Is(err, ErrUnavailable), errors.Is(err, ErrRateLimited):
		return true
	default:
		return false
	}
}

// Defaults applied to a Request.
const (
	// defaultMaxTokens is generous enough that a structured CV extraction does not
	// truncate. A truncated JSON document is worse than a slower call.
	defaultMaxTokens = 16000
	// defaultTimeout covers a thinking model working through a long CV.
	defaultTimeout = 120 * time.Second
	// maxSchemaRepairs bounds repair attempts. Two is enough to recover a model that
	// got the shape slightly wrong; beyond that the prompt or schema is at fault and
	// more attempts just cost money.
	maxSchemaRepairs = 2
)

// Provider is one model backend.
type Provider interface {
	// Name identifies the provider, for logs and metrics.
	Name() string
	// Generate performs one structured-output call. It must not retry internally —
	// retry policy belongs to the Client, which also owns the metrics.
	Generate(ctx context.Context, req Request, repairHint string) (Response, error)
	// Ping checks reachability for the readiness probe.
	Ping(ctx context.Context) error
}

// TokenAccountant records spend. Satisfied by the entitlements service.
type TokenAccountant interface {
	// Reserve checks and consumes the monthly token quota before the call. Called with
	// an estimate, because the true cost is unknown until the response arrives.
	Reserve(ctx context.Context, tokens int64) error
	// Record adjusts the reservation once actual usage is known.
	Record(ctx context.Context, purpose string, usage Usage) error
}

// Validate checks a request before any provider is called.
func (r *Request) Validate() error {
	if !r.Purpose.Valid() {
		return fmt.Errorf("%w: purpose %q is not recognised, so its cost would be unattributable",
			ErrInvalidRequest, r.Purpose)
	}
	if len(r.Schema) == 0 {
		return fmt.Errorf("%w: a response schema is required; this layer has no free-text mode",
			ErrInvalidRequest)
	}
	if r.Prompt.Role == "" {
		return fmt.Errorf("%w: a prompt role is required", ErrInvalidRequest)
	}
	for i, block := range r.Prompt.Data {
		if block.Content == "" {
			return fmt.Errorf("%w: data block %d (%q) is empty",
				ErrInvalidRequest, i, block.Label)
		}
	}
	return nil
}

// applyDefaults fills unset fields.
func (r *Request) applyDefaults() {
	if r.Model == "" {
		r.Model = ModelDefault
	}
	if r.Effort == "" {
		r.Effort = EffortMedium
	}
	if r.MaxTokens <= 0 {
		r.MaxTokens = defaultMaxTokens
	}
	if r.Timeout <= 0 {
		r.Timeout = defaultTimeout
	}
}
