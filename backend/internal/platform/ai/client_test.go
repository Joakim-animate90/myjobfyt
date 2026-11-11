package ai_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/Joakim-animate90/myjobfyt/backend/internal/platform/ai"
)

// fakeProvider returns scripted responses, so the client's policy — quota, repair,
// accounting — is tested without spending money or depending on the network.
type fakeProvider struct {
	mu sync.Mutex
	// responses are returned in order; the last one repeats once exhausted.
	responses []fakeResult
	calls     []fakeCall
}

type fakeResult struct {
	body  string
	usage ai.Usage
	err   error
}

type fakeCall struct {
	purpose    ai.Purpose
	model      ai.Model
	repairHint string
}

func (f *fakeProvider) Name() string { return "fake" }

func (f *fakeProvider) Generate(_ context.Context, req ai.Request, repairHint string) (ai.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.calls = append(f.calls, fakeCall{
		purpose: req.Purpose, model: req.Model, repairHint: repairHint,
	})

	i := len(f.calls) - 1
	if i >= len(f.responses) {
		i = len(f.responses) - 1
	}
	result := f.responses[i]

	return ai.Response{
		JSON:  []byte(result.body),
		Usage: result.usage,
		Model: req.Model,
	}, result.err
}

func (f *fakeProvider) Ping(context.Context) error { return nil }

func (f *fakeProvider) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeProvider) callAt(i int) fakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[i]
}

// fakeAccountant records quota activity.
type fakeAccountant struct {
	mu         sync.Mutex
	reserveErr error
	reserved   []int64
	recorded   []int64
	purposes   []string
}

func (f *fakeAccountant) Reserve(_ context.Context, tokens int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.reserveErr != nil {
		return f.reserveErr
	}
	f.reserved = append(f.reserved, tokens)
	return nil
}

func (f *fakeAccountant) Record(_ context.Context, purpose string, totalTokens int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.purposes = append(f.purposes, purpose)
	f.recorded = append(f.recorded, totalTokens)
	return nil
}

func (f *fakeAccountant) totalRecorded() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	var total int64
	for _, n := range f.recorded {
		total += n
	}
	return total
}

type extracted struct {
	Name  string   `json:"name"`
	Years int      `json:"years"`
	Tags  []string `json:"tags,omitempty"`
}

func testSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name":  map[string]any{"type": "string"},
			"years": map[string]any{"type": "integer"},
		},
		"required":             []string{"name", "years"},
		"additionalProperties": false,
	}
}

func testRequest() ai.Request {
	return ai.Request{
		Purpose: ai.PurposeCVExtract,
		Prompt: ai.Prompt{
			Role: "You are a CV parser.",
			Task: "Extract the name and years of experience.",
			Data: []ai.UntrustedBlock{{Label: "CV", Content: "Joakim, 6 years"}},
		},
		Schema: testSchema(),
	}
}

func newClient(t *testing.T, provider ai.Provider, accountant ai.TokenAccountant) *ai.Client {
	t.Helper()
	return ai.NewClient(ai.ClientDeps{
		Provider:   provider,
		Accountant: accountant,
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
}

func TestGenerateUnmarshalsAValidatedResponse(t *testing.T) {
	provider := &fakeProvider{responses: []fakeResult{{
		body:  `{"name":"Joakim","years":6}`,
		usage: ai.Usage{InputTokens: 1200, OutputTokens: 80},
	}}}
	accountant := &fakeAccountant{}
	client := newClient(t, provider, accountant)

	out, resp, err := ai.Generate[extracted](context.Background(), client, testRequest())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	if out.Name != "Joakim" || out.Years != 6 {
		t.Fatalf("decoded = %+v", out)
	}
	if resp.Attempts != 1 {
		t.Errorf("Attempts = %d, want 1", resp.Attempts)
	}
	if resp.Usage.TotalTokens() != 1280 {
		t.Errorf("TotalTokens = %d, want 1280", resp.Usage.TotalTokens())
	}
}

func TestQuotaIsReservedBeforeTheCall(t *testing.T) {
	// Checking after would mean the candidate has already been charged for tokens they
	// were not entitled to, and we have already paid for them.
	provider := &fakeProvider{responses: []fakeResult{{body: `{"name":"x","years":1}`}}}
	accountant := &fakeAccountant{reserveErr: errors.New("quota exhausted")}
	client := newClient(t, provider, accountant)

	_, _, err := ai.Generate[extracted](context.Background(), client, testRequest())
	if err == nil {
		t.Fatal("Generate succeeded despite an exhausted quota")
	}
	if provider.callCount() != 0 {
		t.Fatalf("the provider was called %d times despite the quota refusal", provider.callCount())
	}
}

func TestSchemaViolationIsRepairedWithAHintThatNamesTheProblem(t *testing.T) {
	// A model told "field X was missing" fixes that field; one told "match the schema"
	// often returns the same output.
	provider := &fakeProvider{responses: []fakeResult{
		{body: `not json at all`, usage: ai.Usage{InputTokens: 1000, OutputTokens: 50},
			err: errors.New("model output did not match the schema: the response was not valid JSON (14 bytes)")},
		{body: `{"name":"Joakim","years":6}`, usage: ai.Usage{InputTokens: 1100, OutputTokens: 60}},
	}}
	client := newClient(t, provider, &fakeAccountant{})

	// The first response's error must wrap ErrSchemaViolation for the repair to trigger.
	provider.responses[0].err = wrapSchema(provider.responses[0].err)

	out, resp, err := ai.Generate[extracted](context.Background(), client, testRequest())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if out.Name != "Joakim" {
		t.Fatalf("decoded = %+v", out)
	}
	if resp.Attempts != 2 {
		t.Errorf("Attempts = %d, want 2", resp.Attempts)
	}

	// The first call carries no hint; the repair does, and it names the problem.
	if hint := provider.callAt(0).repairHint; hint != "" {
		t.Errorf("the first call carried a repair hint: %q", hint)
	}
	hint := provider.callAt(1).repairHint
	if hint == "" {
		t.Fatal("the repair attempt carried no hint")
	}
	if !contains(hint, "not valid JSON") {
		t.Errorf("the hint does not name the specific problem: %q", hint)
	}
	// And it must not invite invention when a value is genuinely absent.
	if !contains(hint, "rather than inventing one") {
		t.Errorf("the hint does not warn against inventing values: %q", hint)
	}
}

func TestTokensFromFailedAttemptsAreStillCounted(t *testing.T) {
	// A failed attempt still cost money. Not counting it understates spend precisely on
	// the calls that spend most.
	schemaErr := wrapSchema(errors.New("model output did not match the schema: bad"))
	provider := &fakeProvider{responses: []fakeResult{
		{usage: ai.Usage{InputTokens: 1000, OutputTokens: 100}, err: schemaErr},
		{usage: ai.Usage{InputTokens: 1000, OutputTokens: 100}, err: schemaErr},
		{usage: ai.Usage{InputTokens: 1000, OutputTokens: 100}, err: schemaErr},
	}}
	accountant := &fakeAccountant{}
	client := newClient(t, provider, accountant)

	_, resp, err := ai.Generate[extracted](context.Background(), client, testRequest())
	if !errors.Is(err, ai.ErrSchemaViolation) {
		t.Fatalf("err = %v, want ErrSchemaViolation", err)
	}

	// Three attempts: the original plus two repairs.
	if provider.callCount() != 3 {
		t.Fatalf("provider called %d times, want 3", provider.callCount())
	}
	if resp.Usage.TotalTokens() != 3300 {
		t.Fatalf("accumulated tokens = %d, want 3300 across all three attempts", resp.Usage.TotalTokens())
	}
	if recorded := accountant.totalRecorded(); recorded != 3300 {
		t.Fatalf("recorded tokens = %d, want 3300", recorded)
	}
}

func TestNonSchemaErrorsAreNotRepaired(t *testing.T) {
	// A refusal, an outage or a truncation will not be fixed by asking again with a
	// hint. Retrying costs money for the same outcome.
	cases := map[string]error{
		"refusal":     ai.ErrRefused,
		"truncation":  ai.ErrTruncated,
		"unavailable": ai.ErrUnavailable,
		"rate limit":  ai.ErrRateLimited,
	}
	for name, sentinel := range cases {
		t.Run(name, func(t *testing.T) {
			provider := &fakeProvider{responses: []fakeResult{{err: sentinel}}}
			client := newClient(t, provider, &fakeAccountant{})

			_, _, err := ai.Generate[extracted](context.Background(), client, testRequest())
			if !errors.Is(err, sentinel) {
				t.Fatalf("err = %v, want %v", err, sentinel)
			}
			if provider.callCount() != 1 {
				t.Fatalf("provider called %d times, want 1 (no repair)", provider.callCount())
			}
		})
	}
}

func TestRetryableClassification(t *testing.T) {
	// Drives the worker's backoff-versus-dead-letter decision.
	retryable := []error{ai.ErrUnavailable, ai.ErrRateLimited}
	for _, err := range retryable {
		if !ai.Retryable(err) {
			t.Errorf("Retryable(%v) = false, want true", err)
		}
	}
	terminal := []error{
		ai.ErrSchemaViolation, ai.ErrRefused, ai.ErrTruncated,
		ai.ErrBudgetExceeded, ai.ErrInvalidRequest, errors.New("unknown"),
	}
	for _, err := range terminal {
		if ai.Retryable(err) {
			t.Errorf("Retryable(%v) = true, want false", err)
		}
	}
}

func TestRequestValidation(t *testing.T) {
	client := newClient(t, &fakeProvider{responses: []fakeResult{{body: `{}`}}}, nil)

	cases := map[string]ai.Request{
		"no purpose": {
			Prompt: ai.Prompt{Role: "role"}, Schema: testSchema(),
		},
		"unknown purpose": {
			Purpose: "mystery", Prompt: ai.Prompt{Role: "role"}, Schema: testSchema(),
		},
		// No free-text mode: unvalidated model output reaching business logic is the
		// failure this package exists to prevent.
		"no schema": {
			Purpose: ai.PurposeCVExtract, Prompt: ai.Prompt{Role: "role"},
		},
		"no role": {
			Purpose: ai.PurposeCVExtract, Schema: testSchema(),
		},
		"empty data block": {
			Purpose: ai.PurposeCVExtract, Schema: testSchema(),
			Prompt: ai.Prompt{Role: "role", Data: []ai.UntrustedBlock{{Label: "CV", Content: ""}}},
		},
	}

	for name, req := range cases {
		t.Run(name, func(t *testing.T) {
			_, _, err := ai.Generate[extracted](context.Background(), client, req)
			if !errors.Is(err, ai.ErrInvalidRequest) {
				t.Fatalf("err = %v, want ErrInvalidRequest", err)
			}
		})
	}
}

func TestPerCallBudgetIsEnforcedBeforeTheCall(t *testing.T) {
	// Without this, one pathological document could consume a large share of a
	// candidate's monthly allowance in minutes.
	provider := &fakeProvider{responses: []fakeResult{{body: `{"name":"x","years":1}`}}}
	client := newClient(t, provider, &fakeAccountant{})

	req := testRequest()
	req.Prompt.Data = []ai.UntrustedBlock{{
		Label: "CV", Content: string(make([]byte, 400_000)),
	}}
	req.MaxTokenBudget = 1000

	_, _, err := ai.Generate[extracted](context.Background(), client, req)
	if !errors.Is(err, ai.ErrBudgetExceeded) {
		t.Fatalf("err = %v, want ErrBudgetExceeded", err)
	}
	if provider.callCount() != 0 {
		t.Fatal("the provider was called despite exceeding the per-call budget")
	}
}

func TestDefaultsAreApplied(t *testing.T) {
	provider := &fakeProvider{responses: []fakeResult{{body: `{"name":"x","years":1}`}}}
	client := newClient(t, provider, &fakeAccountant{})

	req := testRequest()
	req.Model = ""
	req.Effort = ""

	if _, _, err := ai.Generate[extracted](context.Background(), client, req); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if got := provider.callAt(0).model; got != ai.ModelDefault {
		t.Fatalf("model = %q, want the default %q", got, ai.ModelDefault)
	}
}

func TestAccountingFailureDoesNotFailTheCall(t *testing.T) {
	// The work is already done. Discarding a successful CV extraction because a counter
	// was unreachable would be the wrong trade.
	provider := &fakeProvider{responses: []fakeResult{{body: `{"name":"Joakim","years":6}`}}}
	client := newClient(t, provider, &failingRecordAccountant{})

	out, _, err := ai.Generate[extracted](context.Background(), client, testRequest())
	if err != nil {
		t.Fatalf("Generate failed because accounting failed: %v", err)
	}
	if out.Name != "Joakim" {
		t.Fatalf("decoded = %+v", out)
	}
}

type failingRecordAccountant struct{}

func (failingRecordAccountant) Reserve(context.Context, int64) error { return nil }
func (failingRecordAccountant) Record(context.Context, string, int64) error {
	return errors.New("counter unavailable")
}

func TestNilAccountantSkipsQuotaEnforcement(t *testing.T) {
	// Acceptable in tests and tools; the API and worker always supply one.
	provider := &fakeProvider{responses: []fakeResult{{body: `{"name":"x","years":1}`}}}
	client := newClient(t, provider, nil)

	if _, _, err := ai.Generate[extracted](context.Background(), client, testRequest()); err != nil {
		t.Fatalf("Generate: %v", err)
	}
}

func TestSchemaAndGoTypeMismatchIsReportedLoudly(t *testing.T) {
	// If the schema and the Go type disagree, that is a bug. It must surface rather
	// than produce a silently empty struct.
	provider := &fakeProvider{responses: []fakeResult{{body: `["not","an","object"]`}}}
	client := newClient(t, provider, nil)

	_, _, err := ai.Generate[extracted](context.Background(), client, testRequest())
	if err == nil {
		t.Fatal("a response that does not fit the Go type was accepted")
	}
	if !errors.Is(err, ai.ErrSchemaViolation) {
		t.Fatalf("err = %v, want ErrSchemaViolation", err)
	}
}

func TestUsageHelpers(t *testing.T) {
	u := ai.Usage{InputTokens: 1000, OutputTokens: 250, CacheReadTokens: 800}
	if u.TotalTokens() != 1250 {
		t.Errorf("TotalTokens = %d, want 1250", u.TotalTokens())
	}
	if !u.CacheHit() {
		t.Error("CacheHit = false despite cache reads")
	}
	if (ai.Usage{InputTokens: 10}).CacheHit() {
		t.Error("CacheHit = true with no cache reads")
	}
}

func TestClientRespectsContextCancellation(t *testing.T) {
	provider := &slowProvider{delay: 2 * time.Second}
	client := newClient(t, provider, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	if _, _, err := ai.Generate[extracted](ctx, client, testRequest()); err == nil {
		t.Fatal("Generate ignored the cancelled context")
	}
}

type slowProvider struct{ delay time.Duration }

func (slowProvider) Name() string               { return "slow" }
func (slowProvider) Ping(context.Context) error { return nil }
func (s slowProvider) Generate(ctx context.Context, _ ai.Request, _ string) (ai.Response, error) {
	select {
	case <-time.After(s.delay):
		return ai.Response{JSON: []byte(`{"name":"x","years":1}`)}, nil
	case <-ctx.Done():
		return ai.Response{}, ctx.Err()
	}
}

// wrapSchema makes an error satisfy errors.Is(err, ErrSchemaViolation), as the real
// provider's does.
func wrapSchema(err error) error {
	return schemaWrapped{err}
}

type schemaWrapped struct{ err error }

func (s schemaWrapped) Error() string { return s.err.Error() }
func (s schemaWrapped) Unwrap() error { return ai.ErrSchemaViolation }

func contains(haystack, needle string) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) &&
		func() bool {
			for i := 0; i+len(needle) <= len(haystack); i++ {
				if haystack[i:i+len(needle)] == needle {
					return true
				}
			}
			return false
		}()
}
