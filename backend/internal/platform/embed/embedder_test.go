package embed_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Joakim-animate90/myjobfyt/backend/internal/platform/embed"
)

// embedStub serves canned embedding responses and records the batches it was asked for.
type embedStub struct {
	mu      sync.Mutex
	batches [][]string

	status int
	// respond builds the response for a batch. Nil means a well-formed response of
	// unit vectors.
	respond func(texts []string) any
	// rawBody overrides the response entirely, for malformed-response cases.
	rawBody string
	model   string
	dims    int
}

func (s *embedStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Texts []string `json:"texts"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)

	s.mu.Lock()
	s.batches = append(s.batches, req.Texts)
	s.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	status := s.status
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)

	if s.rawBody != "" {
		_, _ = w.Write([]byte(s.rawBody))
		return
	}
	if status != http.StatusOK {
		return
	}

	if s.respond != nil {
		_ = json.NewEncoder(w).Encode(s.respond(req.Texts))
		return
	}

	// One distinct unit vector per text, so a caller mixing up the order is detectable.
	vectors := make([][]float32, 0, len(req.Texts))
	for i := range req.Texts {
		v := make([]float32, s.dimsOrDefault())
		v[i%len(v)] = 1
		vectors = append(vectors, v)
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"embeddings": vectors,
		"model":      s.modelOrDefault(),
		"dims":       s.dimsOrDefault(),
	})
}

func (s *embedStub) dimsOrDefault() int {
	if s.dims > 0 {
		return s.dims
	}
	return 8
}

func (s *embedStub) modelOrDefault() string {
	if s.model != "" {
		return s.model
	}
	return "all-MiniLM-L6-v2"
}

func (s *embedStub) batchCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.batches)
}

func newStubEmbedder(t *testing.T, stub *embedStub, maxBatch int) *embed.HTTPEmbedder {
	t.Helper()
	server := httptest.NewServer(stub)
	t.Cleanup(server.Close)
	return embed.NewHTTPEmbedder(embed.HTTPEmbedderConfig{
		BaseURL:  server.URL,
		Timeout:  2 * time.Second,
		MaxBatch: maxBatch,
	})
}

func TestEmbedReturnsOneVectorPerTextInOrder(t *testing.T) {
	stub := &embedStub{}
	embedder := newStubEmbedder(t, stub, 32)

	texts := []string{"Backend Engineer", "Go PostgreSQL Kubernetes", "Senior Data Scientist"}
	vectors, model, err := embedder.Embed(context.Background(), texts)
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}

	if len(vectors) != len(texts) {
		t.Fatalf("%d vectors for %d texts", len(vectors), len(texts))
	}
	if model != "all-MiniLM-L6-v2" {
		t.Errorf("model = %q", model)
	}
	// The model name is returned rather than configured, so a model change is recorded
	// on the stored vectors and a later comparison can detect the mismatch.
	for i, v := range vectors {
		if err := v.Validate(); err != nil {
			t.Errorf("vector %d is unusable: %v", i, err)
		}
	}
	// Each text got its own distinct vector, so order was preserved.
	if first, _ := embed.Cosine(vectors[0], vectors[1]); first != 0 {
		t.Errorf("vectors 0 and 1 are not distinct (cosine %.3f); order may be scrambled", first)
	}
}

func TestEmbedBatchesLargeInputs(t *testing.T) {
	// Embedding servers have their own limits and a request above them fails
	// wholesale, losing work that had already been batched.
	stub := &embedStub{}
	embedder := newStubEmbedder(t, stub, 4)

	texts := make([]string, 10)
	for i := range texts {
		texts[i] = fmt.Sprintf("text %d", i)
	}

	vectors, _, err := embedder.Embed(context.Background(), texts)
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(vectors) != 10 {
		t.Fatalf("%d vectors for 10 texts", len(vectors))
	}
	// 10 texts at 4 per batch is 3 requests.
	if stub.batchCount() != 3 {
		t.Fatalf("%d requests, want 3", stub.batchCount())
	}
}

func TestEmbedRejectsAnEmptyText(t *testing.T) {
	// An empty input produces a zero vector, which has no direction and cannot be
	// compared. Refused before storage rather than failing later.
	stub := &embedStub{}
	embedder := newStubEmbedder(t, stub, 32)

	if _, _, err := embedder.Embed(context.Background(), []string{"ok", ""}); err == nil {
		t.Fatal("Embed accepted an empty text")
	}
	if stub.batchCount() != 0 {
		t.Error("a request was sent despite invalid input")
	}
}

func TestEmbedTruncatesOverlongTextAtARuneBoundary(t *testing.T) {
	// Embedding models truncate at a few hundred tokens anyway, so sending a whole CV
	// wastes bandwidth to embed only its opening.
	stub := &embedStub{}
	embedder := newStubEmbedder(t, stub, 32)

	// Multi-byte characters, so a naive byte cut would split a rune and send invalid
	// UTF-8.
	long := strings.Repeat("é", embed.MaxTextLength)

	if _, _, err := embedder.Embed(context.Background(), []string{long}); err != nil {
		t.Fatalf("Embed: %v", err)
	}

	stub.mu.Lock()
	sent := stub.batches[0][0]
	stub.mu.Unlock()

	if len(sent) > embed.MaxTextLength {
		t.Fatalf("sent %d bytes, want at most %d", len(sent), embed.MaxTextLength)
	}
	for _, r := range sent {
		if r == '�' {
			t.Fatal("truncation split a multi-byte character")
		}
	}
}

func TestEmbedTreatsEveryFailureAsUnavailable(t *testing.T) {
	// A self-hosted embedding service returning 400 is a deployment problem, not
	// something a candidate's CV can cause, and the right response either way is to
	// fall back rather than fail the pipeline.
	cases := map[string]*embedStub{
		"server error":   {status: http.StatusInternalServerError},
		"bad request":    {status: http.StatusBadRequest},
		"not found":      {status: http.StatusNotFound},
		"unauthorized":   {status: http.StatusUnauthorized},
		"malformed body": {rawBody: `not json at all`},
		"wrong count": {respond: func(texts []string) any {
			return map[string]any{
				"embeddings": [][]float32{{1, 0, 0}},
				"model":      "m", "dims": 3,
			}
		}},
		"no model name": {respond: func(texts []string) any {
			vectors := make([][]float32, len(texts))
			for i := range vectors {
				vectors[i] = []float32{1, 0, 0}
			}
			return map[string]any{"embeddings": vectors, "dims": 3}
		}},
		"zero vector": {respond: func(texts []string) any {
			vectors := make([][]float32, len(texts))
			for i := range vectors {
				vectors[i] = []float32{0, 0, 0}
			}
			return map[string]any{"embeddings": vectors, "model": "m", "dims": 3}
		}},
		"dims disagree with the vector": {respond: func(texts []string) any {
			vectors := make([][]float32, len(texts))
			for i := range vectors {
				vectors[i] = []float32{1, 0, 0}
			}
			return map[string]any{"embeddings": vectors, "model": "m", "dims": 384}
		}},
	}

	for name, stub := range cases {
		t.Run(name, func(t *testing.T) {
			embedder := newStubEmbedder(t, stub, 32)

			_, _, err := embedder.Embed(context.Background(), []string{"Backend Engineer", "Go"})
			if err == nil {
				t.Fatal("Embed succeeded on a broken response")
			}
			if !embed.Unavailable(err) {
				t.Fatalf("err = %v, want it to report Unavailable so the caller falls back", err)
			}
		})
	}
}

func TestEmbedRejectsAMidBatchModelChange(t *testing.T) {
	// Vectors produced by two models are not comparable with each other, so a set that
	// spans a model change must not be stored.
	var calls int
	var mu sync.Mutex

	stub := &embedStub{respond: func(texts []string) any {
		mu.Lock()
		calls++
		model := "model-a"
		if calls > 1 {
			model = "model-b"
		}
		mu.Unlock()

		vectors := make([][]float32, len(texts))
		for i := range vectors {
			vectors[i] = []float32{1, 0, 0}
		}
		return map[string]any{"embeddings": vectors, "model": model, "dims": 3}
	}}

	embedder := newStubEmbedder(t, stub, 1)

	_, _, err := embedder.Embed(context.Background(), []string{"a", "b"})
	if err == nil {
		t.Fatal("Embed accepted vectors spanning a model change")
	}
	if !strings.Contains(err.Error(), "model") {
		t.Errorf("err = %v, want it to name the model change", err)
	}
}

func TestEmbedWithNoTextsDoesNothing(t *testing.T) {
	stub := &embedStub{}
	embedder := newStubEmbedder(t, stub, 32)

	vectors, model, err := embedder.Embed(context.Background(), nil)
	if err != nil {
		t.Fatalf("Embed(nil): %v", err)
	}
	if len(vectors) != 0 || model != "" {
		t.Fatalf("vectors = %v, model = %q, want nothing", vectors, model)
	}
	if stub.batchCount() != 0 {
		t.Error("a request was sent for no texts")
	}
}

func TestEmbedRespectsContextCancellation(t *testing.T) {
	stub := &embedStub{}
	embedder := newStubEmbedder(t, stub, 32)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, _, err := embedder.Embed(ctx, []string{"Backend Engineer"}); err == nil {
		t.Fatal("Embed ignored a cancelled context")
	}
}

func TestPingProvesTheModelIsLoadedNotJustListening(t *testing.T) {
	// A service can be listening and unable to load its model, and only a real
	// embedding proves otherwise.
	working := newStubEmbedder(t, &embedStub{}, 32)
	if err := working.Ping(context.Background()); err != nil {
		t.Fatalf("Ping against a working service: %v", err)
	}

	// Listening, 200, but returns nothing usable.
	broken := newStubEmbedder(t, &embedStub{rawBody: `{"embeddings": [], "model": ""}`}, 32)
	if err := broken.Ping(context.Background()); err == nil {
		t.Fatal("Ping passed against a service that returned nothing usable")
	}
}

func TestPingFailsWhenUnreachable(t *testing.T) {
	// Port 1 is reserved and nothing listens there.
	embedder := embed.NewHTTPEmbedder(embed.HTTPEmbedderConfig{
		BaseURL: "http://127.0.0.1:1", Timeout: time.Second,
	})
	err := embedder.Ping(context.Background())
	if err == nil {
		t.Fatal("Ping succeeded against an unreachable service")
	}
	if !embed.Unavailable(err) {
		t.Fatalf("err = %v, want it to report Unavailable", err)
	}
}

func TestUnavailableClassification(t *testing.T) {
	if !embed.Unavailable(fmt.Errorf("wrapped: %w", embed.ErrUnavailable)) {
		t.Error("a wrapped ErrUnavailable was not recognised")
	}
	if embed.Unavailable(errors.New("something else")) {
		t.Error("an unrelated error was reported Unavailable")
	}
	// A dimension mismatch is a data problem, not an availability one: falling back to
	// lexical would hide a model change that needs re-embedding.
	if embed.Unavailable(embed.ErrDimensionMismatch) {
		t.Error("a dimension mismatch was reported Unavailable")
	}
}

func TestEmbedderName(t *testing.T) {
	embedder := embed.NewHTTPEmbedder(embed.HTTPEmbedderConfig{BaseURL: "http://localhost"})
	if embedder.Name() == "" {
		t.Error("Name() is empty; it is used as a metric label")
	}
}
