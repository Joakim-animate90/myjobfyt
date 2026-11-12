package embed

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// Embedder produces vectors for text.
//
// Self-hosted rather than a hosted API: the Claude API has no embeddings endpoint, and
// a sentence-transformer model is cheap to run and fast enough at this volume. See
// docs/INTEGRATION_ANALYSIS.md.
type Embedder interface {
	// Embed returns one vector per input text, in order, plus the model that produced
	// them. The model name is returned rather than configured, so a model change is
	// recorded on the stored vectors and a later comparison can detect the mismatch
	// instead of silently comparing incompatible vectors.
	Embed(ctx context.Context, texts []string) ([]Vector, string, error)
	// Name identifies the embedder for logs and metrics.
	Name() string
	// Ping checks reachability for the readiness probe.
	Ping(ctx context.Context) error
}

// HTTPEmbedder calls a self-hosted embedding service.
//
// The expected contract, which matches the common sentence-transformers server shapes
// (text-embeddings-inference and the usual FastAPI wrappers):
//
//	POST {BaseURL}/embed
//	  {"texts": ["...", "..."]}
//	→ {"embeddings": [[...], [...]], "model": "all-MiniLM-L6-v2", "dims": 384}
//
// Deliberately minimal. A richer protocol would couple this to one server
// implementation, and the whole point of the lexical fallback is that this component is
// replaceable and allowed to be absent.
type HTTPEmbedder struct {
	baseURL string
	client  *http.Client
	// maxBatch bounds one request. Embedding servers have their own limits and a
	// request above them fails wholesale, losing work that had already been batched.
	maxBatch int
}

// HTTPEmbedderConfig configures the embedder.
type HTTPEmbedderConfig struct {
	BaseURL  string
	Timeout  time.Duration
	MaxBatch int
}

// NewHTTPEmbedder builds the embedder.
func NewHTTPEmbedder(cfg HTTPEmbedderConfig) *HTTPEmbedder {
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	maxBatch := cfg.MaxBatch
	if maxBatch <= 0 {
		maxBatch = 32
	}
	return &HTTPEmbedder{
		baseURL:  cfg.BaseURL,
		client:   &http.Client{Timeout: timeout},
		maxBatch: maxBatch,
	}
}

// Name identifies the embedder.
func (e *HTTPEmbedder) Name() string { return "http" }

type embedRequest struct {
	Texts []string `json:"texts"`
}

type embedResponse struct {
	Embeddings [][]float32 `json:"embeddings"`
	Model      string      `json:"model"`
	Dims       int         `json:"dims"`
}

// MaxTextLength bounds one input.
//
// Embedding models truncate at a few hundred tokens anyway, so sending a whole CV
// wastes bandwidth to embed only its opening. Callers should pass a focused extract —
// a title, a skill list — and this is the backstop.
const MaxTextLength = 8000

// Embed returns one vector per text, batching as needed.
func (e *HTTPEmbedder) Embed(ctx context.Context, texts []string) ([]Vector, string, error) {
	if len(texts) == 0 {
		return nil, "", nil
	}

	prepared := make([]string, 0, len(texts))
	for i, text := range texts {
		if text == "" {
			// An empty input would produce a zero vector, which has no direction and
			// cannot be compared. Refused here rather than stored and failing later.
			return nil, "", fmt.Errorf("text %d is empty", i)
		}
		if len(text) > MaxTextLength {
			text = truncateAtRune(text, MaxTextLength)
		}
		prepared = append(prepared, text)
	}

	vectors := make([]Vector, 0, len(prepared))
	var model string

	for start := 0; start < len(prepared); start += e.maxBatch {
		end := start + e.maxBatch
		if end > len(prepared) {
			end = len(prepared)
		}

		batch, batchModel, err := e.embedBatch(ctx, prepared[start:end])
		if err != nil {
			return nil, "", err
		}

		// A model change part-way through a batch set would mean the stored vectors
		// were produced by two models and are not comparable with each other.
		if model != "" && batchModel != model {
			return nil, "", fmt.Errorf("the embedding service changed model mid-batch (%q then %q); "+
				"the resulting vectors would not be comparable", model, batchModel)
		}
		model = batchModel
		vectors = append(vectors, batch...)
	}

	return vectors, model, nil
}

func (e *HTTPEmbedder) embedBatch(ctx context.Context, texts []string) ([]Vector, string, error) {
	body, err := json.Marshal(embedRequest{Texts: texts})
	if err != nil {
		return nil, "", fmt.Errorf("marshal embed request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.baseURL+"/embed", bytes.NewReader(body))
	if err != nil {
		return nil, "", fmt.Errorf("build embed request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := e.client.Do(req)
	if err != nil {
		// Unreachable: the caller falls back to lexical similarity rather than failing.
		return nil, "", fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		// Every non-200 is treated as unavailable, including a 4xx. A self-hosted
		// embedding service returning 400 is a deployment problem, not something a
		// candidate's CV can cause, and the right response either way is to fall back
		// rather than fail the pipeline.
		return nil, "", fmt.Errorf("%w: embedding service returned %d", ErrUnavailable, resp.StatusCode)
	}

	var decoded embedResponse
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		return nil, "", fmt.Errorf("%w: decode embed response: %v", ErrUnavailable, err)
	}

	if len(decoded.Embeddings) != len(texts) {
		// A mismatch means the vectors cannot be attributed to their inputs, so storing
		// them would associate a candidate's title vector with their skills.
		return nil, "", fmt.Errorf("%w: asked for %d embeddings, got %d",
			ErrUnavailable, len(texts), len(decoded.Embeddings))
	}
	if decoded.Model == "" {
		// Without a model name a stored vector cannot be invalidated on a model change,
		// and incompatible vectors would be compared silently.
		return nil, "", fmt.Errorf("%w: the embedding service reported no model name", ErrUnavailable)
	}

	vectors := make([]Vector, 0, len(decoded.Embeddings))
	for i, raw := range decoded.Embeddings {
		vector := Vector(raw)
		if err := vector.Validate(); err != nil {
			return nil, "", fmt.Errorf("%w: embedding %d is unusable: %v", ErrUnavailable, i, err)
		}
		if decoded.Dims > 0 && vector.Dims() != decoded.Dims {
			return nil, "", fmt.Errorf("%w: embedding %d has %d dimensions, the service reported %d",
				ErrUnavailable, i, vector.Dims(), decoded.Dims)
		}
		vectors = append(vectors, vector)
	}

	return vectors, decoded.Model, nil
}

// Ping checks the service answers.
func (e *HTTPEmbedder) Ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	// Embedding one short string rather than hitting a health endpoint, because a
	// service can be listening and unable to load its model, and only a real embedding
	// proves otherwise.
	vectors, model, err := e.Embed(ctx, []string{"ping"})
	if err != nil {
		return err
	}
	if len(vectors) != 1 || model == "" {
		return fmt.Errorf("%w: the service answered but returned nothing usable", ErrUnavailable)
	}
	return nil
}

// Unavailable reports whether an error means the embedder could not be reached, so the
// caller should fall back to lexical similarity.
func Unavailable(err error) bool { return errors.Is(err, ErrUnavailable) }

// truncateAtRune cuts to at most n bytes without splitting a multi-byte character,
// which would leave invalid UTF-8 in the request body.
func truncateAtRune(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n]
}
