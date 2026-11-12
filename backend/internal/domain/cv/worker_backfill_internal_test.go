package cv

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/Joakim-animate90/myjobfyt/backend/internal/apperr"
	"github.com/Joakim-animate90/myjobfyt/backend/internal/platform/embed"
	"github.com/google/uuid"
)

// backfillRepo implements only the methods the backfill touches. The embedded
// interface is nil, so a method this test does not expect to be called panics rather
// than returning a zero value that would quietly make the test pass.
type backfillRepo struct {
	Repository

	mu sync.Mutex

	withoutAnyModel []uuid.UUID
	forModel        map[string][]uuid.UUID
	inputs          map[uuid.UUID]EmbeddingInput
	inputErr        map[uuid.UUID]error

	stored               [][]StoredEmbedding
	askedWithoutAny      int
	askedForModel        []string
	askedWithBatchSizeOf []int
}

func (r *backfillRepo) VersionsWithoutAnyEmbedding(_ context.Context, limit int) ([]uuid.UUID, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.askedWithoutAny++
	r.askedWithBatchSizeOf = append(r.askedWithBatchSizeOf, limit)
	return r.withoutAnyModel, nil
}

func (r *backfillRepo) VersionsNeedingEmbedding(_ context.Context, model string, limit int) ([]uuid.UUID, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.askedForModel = append(r.askedForModel, model)
	r.askedWithBatchSizeOf = append(r.askedWithBatchSizeOf, limit)
	return r.forModel[model], nil
}

func (r *backfillRepo) EmbeddingInputs(_ context.Context, versionID uuid.UUID) (EmbeddingInput, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err, ok := r.inputErr[versionID]; ok {
		return EmbeddingInput{}, err
	}
	if in, ok := r.inputs[versionID]; ok {
		in.CVVersionID = versionID
		return in, nil
	}
	return EmbeddingInput{CVVersionID: versionID, MostRecentTitle: "Backend Engineer"}, nil
}

func (r *backfillRepo) StoreEmbeddings(_ context.Context, embeddings []StoredEmbedding) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stored = append(r.stored, embeddings)
	return nil
}

func (r *backfillRepo) storedCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.stored)
}

// backfillEmbedder returns a fixed vector, optionally failing for the first n calls.
type backfillEmbedder struct {
	mu          sync.Mutex
	model       string
	calls       int
	failWith    error
	failedCalls int
}

func (e *backfillEmbedder) Name() string { return "stub" }

func (e *backfillEmbedder) Embed(_ context.Context, texts []string) ([]embed.Vector, string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls++
	if e.failWith != nil {
		e.failedCalls++
		return nil, "", e.failWith
	}
	vectors := make([]embed.Vector, 0, len(texts))
	for range texts {
		vectors = append(vectors, embed.Vector{1, 0, 0})
	}
	model := e.model
	if model == "" {
		model = "stub-model-v1"
	}
	return vectors, model, nil
}

func (e *backfillEmbedder) callCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.calls
}

func newBackfillWorker(repo Repository, embedder Embedder) *Worker {
	return &Worker{
		repo:     repo,
		embedder: embedder,
		log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		now:      func() time.Time { return time.Unix(1700000000, 0).UTC() },
	}
}

func TestBackfillColdStartAsksForVersionsWithNoVectorsAtAll(t *testing.T) {
	// The model is reported by the provider rather than configured, so after a restart
	// the worker does not know it. The versions that matter most are exactly the ones
	// an outage left with nothing, and that set can be found without a model name.
	missed := uuid.New()
	repo := &backfillRepo{withoutAnyModel: []uuid.UUID{missed}}
	embedder := &backfillEmbedder{}
	w := newBackfillWorker(repo, embedder)

	if err := w.backfillOnce(context.Background(), 50); err != nil {
		t.Fatalf("backfillOnce: %v", err)
	}

	if repo.askedWithoutAny != 1 {
		t.Errorf("asked the no-vectors query %d times, want 1", repo.askedWithoutAny)
	}
	if len(repo.askedForModel) != 0 {
		t.Errorf("asked the model-scoped query before any model was known: %v", repo.askedForModel)
	}
	if repo.storedCount() != 1 {
		t.Fatalf("stored %d embedding sets, want 1", repo.storedCount())
	}
	if got := repo.stored[0][0].CVVersionID; got != missed {
		t.Errorf("embedded version %s, want the missed one %s", got, missed)
	}
}

func TestBackfillSwitchesToTheModelScopedQueryOnceTheModelIsKnown(t *testing.T) {
	// Which additionally catches versions left behind by a model upgrade, where the
	// version does have vectors — just not comparable ones.
	first := uuid.New()
	stale := uuid.New()
	repo := &backfillRepo{
		withoutAnyModel: []uuid.UUID{first},
		forModel:        map[string][]uuid.UUID{"stub-model-v1": {stale}},
	}
	embedder := &backfillEmbedder{}
	w := newBackfillWorker(repo, embedder)
	ctx := context.Background()

	// First sweep: cold, learns the model from the successful embedding.
	if err := w.backfillOnce(ctx, 50); err != nil {
		t.Fatalf("first sweep: %v", err)
	}
	if w.currentModel() != "stub-model-v1" {
		t.Fatalf("the model was not learned: %q", w.currentModel())
	}

	// Second sweep: now model-scoped.
	if err := w.backfillOnce(ctx, 50); err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if len(repo.askedForModel) != 1 || repo.askedForModel[0] != "stub-model-v1" {
		t.Errorf("model-scoped queries = %v, want one for stub-model-v1", repo.askedForModel)
	}
	if repo.askedWithoutAny != 1 {
		t.Errorf("the no-vectors query ran %d times, want only the cold start", repo.askedWithoutAny)
	}
	if repo.storedCount() != 2 {
		t.Errorf("stored %d sets across two sweeps, want 2", repo.storedCount())
	}
}

func TestBackfillStopsTheSweepWhileTheProviderIsStillDown(t *testing.T) {
	// Otherwise a backlog of a thousand versions becomes a thousand failing requests
	// per sweep against a service that is already struggling.
	pending := []uuid.UUID{uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()}
	repo := &backfillRepo{withoutAnyModel: pending}
	embedder := &backfillEmbedder{failWith: embed.ErrUnavailable}
	w := newBackfillWorker(repo, embedder)

	// Not an error: the sweep gave up cleanly and the next one retries.
	if err := w.backfillOnce(context.Background(), 50); err != nil {
		t.Fatalf("backfillOnce: %v", err)
	}

	if got := embedder.callCount(); got != 1 {
		t.Errorf("called the embedder %d times for %d pending versions, want 1 before giving up",
			got, len(pending))
	}
	if repo.storedCount() != 0 {
		t.Errorf("stored %d sets despite the provider being down", repo.storedCount())
	}
}

func TestBackfillKeepsGoingPastOneUnembeddableVersion(t *testing.T) {
	// A single corrupt or vanished version must not block the recovery of every
	// version queued behind it.
	broken, healthy := uuid.New(), uuid.New()
	repo := &backfillRepo{
		withoutAnyModel: []uuid.UUID{broken, healthy},
		inputErr: map[uuid.UUID]error{
			broken: apperr.Internalf(errors.New("corrupt jsonb"), "gather embedding inputs"),
		},
	}
	embedder := &backfillEmbedder{}
	w := newBackfillWorker(repo, embedder)

	if err := w.backfillOnce(context.Background(), 50); err != nil {
		t.Fatalf("backfillOnce: %v", err)
	}

	if repo.storedCount() != 1 {
		t.Fatalf("stored %d sets, want 1 (the healthy version)", repo.storedCount())
	}
	if got := repo.stored[0][0].CVVersionID; got != healthy {
		t.Errorf("embedded %s, want the healthy version %s", got, healthy)
	}
}

func TestBackfillSkipsAVersionWithNothingToEmbed(t *testing.T) {
	// An empty aspect embeds to a zero vector, which cannot be compared. Such a version
	// must be skipped rather than stored, and must not abort the sweep.
	empty, healthy := uuid.New(), uuid.New()
	repo := &backfillRepo{
		withoutAnyModel: []uuid.UUID{empty, healthy},
		inputs:          map[uuid.UUID]EmbeddingInput{empty: {}},
	}
	embedder := &backfillEmbedder{}
	w := newBackfillWorker(repo, embedder)

	if err := w.backfillOnce(context.Background(), 50); err != nil {
		t.Fatalf("backfillOnce: %v", err)
	}

	if got := embedder.callCount(); got != 1 {
		t.Errorf("embedder called %d times, want 1; the empty version should not reach it", got)
	}
	if repo.storedCount() != 1 {
		t.Errorf("stored %d sets, want 1", repo.storedCount())
	}
}

func TestBackfillWithNothingPendingDoesNotCallTheProvider(t *testing.T) {
	// The sweep runs on a timer forever. If an idle sweep cost a request, an idle
	// deployment would bill for embeddings it never needed.
	repo := &backfillRepo{}
	embedder := &backfillEmbedder{}
	w := newBackfillWorker(repo, embedder)

	if err := w.backfillOnce(context.Background(), 50); err != nil {
		t.Fatalf("backfillOnce: %v", err)
	}
	if got := embedder.callCount(); got != 0 {
		t.Errorf("embedder called %d times with nothing pending, want 0", got)
	}
}

func TestBackfillPassesTheBatchSizeToTheQuery(t *testing.T) {
	// The limit is the only thing stopping a large backlog from starving live uploads,
	// so it has to actually reach the query.
	repo := &backfillRepo{}
	w := newBackfillWorker(repo, &backfillEmbedder{})

	if err := w.backfillOnce(context.Background(), 7); err != nil {
		t.Fatalf("backfillOnce: %v", err)
	}
	if len(repo.askedWithBatchSizeOf) != 1 || repo.askedWithBatchSizeOf[0] != 7 {
		t.Errorf("query limits = %v, want [7]", repo.askedWithBatchSizeOf)
	}
}

func TestBackfillStopsPromptlyWhenCancelled(t *testing.T) {
	// A shutdown must not wait out the sweep interval.
	repo := &backfillRepo{}
	w := newBackfillWorker(repo, &backfillEmbedder{})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- w.RunEmbeddingBackfill(ctx, EmbeddingBackfillConfig{
			Interval:  time.Hour, // far longer than this test will wait
			BatchSize: 10,
		})
	}()

	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("RunEmbeddingBackfill returned %v on cancellation, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RunEmbeddingBackfill did not return within 2s of cancellation")
	}
}

func TestBackfillWithoutAnEmbedderDoesNothing(t *testing.T) {
	// Running without an embedder is a supported mode, not a failure: matching stays on
	// the lexical path. The backfill must return rather than spin on a nil embedder.
	w := newBackfillWorker(&backfillRepo{}, nil)

	done := make(chan error, 1)
	go func() {
		done <- w.RunEmbeddingBackfill(context.Background(), EmbeddingBackfillConfig{
			Interval: time.Millisecond, BatchSize: 10,
		})
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("err = %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RunEmbeddingBackfill did not return with a nil embedder")
	}
}

func TestBackfillConfigDefaultsAreUsable(t *testing.T) {
	// A zero config must not mean a zero ticker interval, which panics, or a zero batch
	// size, which would sweep forever and embed nothing.
	cfg := EmbeddingBackfillConfig{}.withDefaults()

	if cfg.Interval <= 0 {
		t.Errorf("Interval = %v; time.NewTicker panics on a non-positive duration", cfg.Interval)
	}
	if cfg.BatchSize <= 0 {
		t.Errorf("BatchSize = %d, want a positive default", cfg.BatchSize)
	}

	// An explicit value is respected.
	explicit := EmbeddingBackfillConfig{Interval: 30 * time.Second, BatchSize: 5}.withDefaults()
	if explicit.Interval != 30*time.Second || explicit.BatchSize != 5 {
		t.Errorf("withDefaults overrode explicit values: %+v", explicit)
	}
}
