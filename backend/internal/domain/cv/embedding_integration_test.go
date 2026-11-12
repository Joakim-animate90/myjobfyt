//go:build integration

package cv_test

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/Joakim-animate90/myjobfyt/backend/internal/apperr"
	"github.com/Joakim-animate90/myjobfyt/backend/internal/domain/cv"
	"github.com/Joakim-animate90/myjobfyt/backend/internal/platform/embed"
	"github.com/google/uuid"
)

// unitVector builds a deterministic test vector with a single hot component, which is
// enough to be valid (non-zero, finite) and distinguishable.
func unitVector(dims, hot int) embed.Vector {
	v := make(embed.Vector, dims)
	v[hot%dims] = 1
	return v
}

func markEmbeddable(t *testing.T, repo *cv.PostgresRepository, userID, versionID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	if err := repo.StoreParsedData(ctx, sampleParsed(userID, versionID), nil); err != nil {
		t.Fatalf("StoreParsedData: %v", err)
	}
	// seedVersion leaves the version pending; VersionsNeedingEmbedding only considers
	// versions that are parsed and scanned clean, so move it there as the pipeline does.
	now := time.Now().UTC()
	if err := repo.UpdateAVStatus(ctx, versionID, cv.AVClean, now); err != nil {
		t.Fatalf("UpdateAVStatus: %v", err)
	}
	if err := repo.MarkParsed(ctx, versionID, now); err != nil {
		t.Fatalf("MarkParsed: %v", err)
	}
}

func TestEmbeddingRoundTripKeepsEveryComponentExactly(t *testing.T) {
	// The column is real[], so the round trip is float32 throughout. A float64 hop
	// anywhere in the path would perturb the components and quietly change every
	// similarity score computed afterwards.
	db := openDB(t)
	repo := cv.NewPostgresRepository(db)
	ctx := context.Background()
	userID, versionID := seedVersion(t, db)

	original := embed.Vector{0.125, -0.5, 0.75, -0.0625, 1}

	err := repo.StoreEmbeddings(ctx, []cv.StoredEmbedding{{
		CVVersionID: versionID,
		Kind:        embed.KindTitle,
		Model:       "test-model-v1",
		Vector:      original,
	}})
	if err != nil {
		t.Fatalf("StoreEmbeddings: %v", err)
	}

	stored, err := repo.EmbeddingsForVersion(ctx, userID, versionID, "test-model-v1")
	if err != nil {
		t.Fatalf("EmbeddingsForVersion: %v", err)
	}
	if len(stored) != 1 {
		t.Fatalf("%d embeddings, want 1", len(stored))
	}

	read := stored[0]
	if read.Kind != embed.KindTitle {
		t.Errorf("kind = %q, want title", read.Kind)
	}
	if read.Model != "test-model-v1" {
		t.Errorf("model = %q", read.Model)
	}
	if read.Vector.Dims() != original.Dims() {
		t.Fatalf("dims = %d, want %d", read.Vector.Dims(), original.Dims())
	}
	for i := range original {
		if read.Vector[i] != original[i] {
			t.Fatalf("component %d came back as %v, want %v; the round trip lost precision",
				i, read.Vector[i], original[i])
		}
	}

	similarity, err := embed.Cosine(original, read.Vector)
	if err != nil {
		t.Fatalf("Cosine: %v", err)
	}
	if similarity < 0.999999 {
		t.Fatalf("the stored vector scores %.9f against the original, want 1", similarity)
	}
}

func TestStoreEmbeddingsDerivesDimsFromTheVector(t *testing.T) {
	// cv_embeddings has a CHECK that dims matches array_length(vector, 1). The
	// repository derives dims rather than trusting a caller to pass it consistently,
	// so every real model's width has to survive the write.
	db := openDB(t)
	repo := cv.NewPostgresRepository(db)
	ctx := context.Background()
	userID, versionID := seedVersion(t, db)

	for _, dims := range []int{8, 64, 384, 768, 1536, 3072} {
		model := "model-" + strconv.Itoa(dims)
		err := repo.StoreEmbeddings(ctx, []cv.StoredEmbedding{{
			CVVersionID: versionID,
			Kind:        embed.KindTitle,
			Model:       model,
			Vector:      unitVector(dims, 0),
		}})
		if err != nil {
			t.Fatalf("StoreEmbeddings with %d dims: %v", dims, err)
		}

		stored, err := repo.EmbeddingsForVersion(ctx, userID, versionID, model)
		if err != nil {
			t.Fatalf("EmbeddingsForVersion: %v", err)
		}
		if len(stored) != 1 {
			t.Fatalf("%d embeddings for %s, want 1", len(stored), model)
		}
		if got := stored[0].Vector.Dims(); got != dims {
			t.Errorf("a %d-dim vector came back with %d dims", dims, got)
		}
	}
}

func TestReEmbeddingWithTheSameModelOverwrites(t *testing.T) {
	// Otherwise a re-parse would accumulate a second vector for the same aspect and
	// matching would score against whichever row the query happened to return first.
	db := openDB(t)
	repo := cv.NewPostgresRepository(db)
	ctx := context.Background()
	userID, versionID := seedVersion(t, db)

	superseded := unitVector(8, 0)
	current := unitVector(8, 3)

	for _, v := range []embed.Vector{superseded, current} {
		err := repo.StoreEmbeddings(ctx, []cv.StoredEmbedding{{
			CVVersionID: versionID, Kind: embed.KindTitle, Model: "m1", Vector: v,
		}})
		if err != nil {
			t.Fatalf("StoreEmbeddings: %v", err)
		}
	}

	stored, err := repo.EmbeddingsForVersion(ctx, userID, versionID, "m1")
	if err != nil {
		t.Fatalf("EmbeddingsForVersion: %v", err)
	}
	if len(stored) != 1 {
		t.Fatalf("%d embeddings after re-embedding with the same model, want 1", len(stored))
	}
	if similarity, _ := embed.Cosine(stored[0].Vector, current); similarity < 0.999999 {
		t.Error("the re-embed did not replace the earlier vector")
	}
}

func TestANewModelAddsAlongsideRatherThanReplacing(t *testing.T) {
	// A comparison against a posting still embedded by the old model has to keep
	// working until that posting is re-embedded too. Deleting the old vectors on
	// upgrade would leave a window in which nothing could be compared at all.
	db := openDB(t)
	repo := cv.NewPostgresRepository(db)
	ctx := context.Background()
	userID, versionID := seedVersion(t, db)

	err := repo.StoreEmbeddings(ctx, []cv.StoredEmbedding{
		{CVVersionID: versionID, Kind: embed.KindTitle, Model: "old-model", Vector: unitVector(8, 0)},
	})
	if err != nil {
		t.Fatalf("StoreEmbeddings (old): %v", err)
	}
	err = repo.StoreEmbeddings(ctx, []cv.StoredEmbedding{
		// A different width, as a real model change usually is.
		{CVVersionID: versionID, Kind: embed.KindTitle, Model: "new-model", Vector: unitVector(64, 0)},
	})
	if err != nil {
		t.Fatalf("StoreEmbeddings (new): %v", err)
	}

	old, err := repo.EmbeddingsForVersion(ctx, userID, versionID, "old-model")
	if err != nil {
		t.Fatalf("EmbeddingsForVersion (old): %v", err)
	}
	if len(old) != 1 || old[0].Vector.Dims() != 8 {
		t.Fatalf("the old model's vector was lost: %+v", old)
	}

	fresh, err := repo.EmbeddingsForVersion(ctx, userID, versionID, "new-model")
	if err != nil {
		t.Fatalf("EmbeddingsForVersion (new): %v", err)
	}
	if len(fresh) != 1 || fresh[0].Vector.Dims() != 64 {
		t.Fatalf("the new model's vector is wrong: %+v", fresh)
	}

	// And the two must not silently compare. This is why reads are scoped by model: a
	// caller that mixed them would get scores that look valid and mean nothing.
	if _, err := embed.Cosine(old[0].Vector, fresh[0].Vector); err == nil {
		t.Error("vectors from two different models compared without error")
	}
}

func TestEmbeddingReadsAreScopedByModel(t *testing.T) {
	db := openDB(t)
	repo := cv.NewPostgresRepository(db)
	ctx := context.Background()
	userID, versionID := seedVersion(t, db)

	err := repo.StoreEmbeddings(ctx, []cv.StoredEmbedding{
		{CVVersionID: versionID, Kind: embed.KindTitle, Model: "model-a", Vector: unitVector(8, 0)},
		{CVVersionID: versionID, Kind: embed.KindSkills, Model: "model-a", Vector: unitVector(8, 1)},
		{CVVersionID: versionID, Kind: embed.KindTitle, Model: "model-b", Vector: unitVector(8, 2)},
	})
	if err != nil {
		t.Fatalf("StoreEmbeddings: %v", err)
	}

	a, err := repo.EmbeddingsForVersion(ctx, userID, versionID, "model-a")
	if err != nil {
		t.Fatalf("EmbeddingsForVersion: %v", err)
	}
	if len(a) != 2 {
		t.Fatalf("%d embeddings for model-a, want 2", len(a))
	}
	for _, e := range a {
		if e.Model != "model-a" {
			t.Errorf("a read scoped to model-a returned a %q vector", e.Model)
		}
	}
}

func TestEmbeddingsAreTenantScoped(t *testing.T) {
	// Every query touching candidate data is scoped by user_id. An embedding is derived
	// from the CV's contents, so leaking one leaks the candidate's profile in vector form.
	db := openDB(t)
	repo := cv.NewPostgresRepository(db)
	ctx := context.Background()

	_, versionID := seedVersion(t, db)
	intruderID, _ := seedVersion(t, db)

	err := repo.StoreEmbeddings(ctx, []cv.StoredEmbedding{
		{CVVersionID: versionID, Kind: embed.KindTitle, Model: "m1", Vector: unitVector(8, 0)},
	})
	if err != nil {
		t.Fatalf("StoreEmbeddings: %v", err)
	}

	stolen, err := repo.EmbeddingsForVersion(ctx, intruderID, versionID, "m1")
	if err != nil {
		t.Fatalf("EmbeddingsForVersion: %v", err)
	}
	if len(stolen) != 0 {
		t.Fatalf("another candidate read %d of the owner's embeddings", len(stolen))
	}
}

func TestStoreEmbeddingsRejectsAnUnknownKind(t *testing.T) {
	// The database CHECK would reject it too; failing in the repository names the kind,
	// which a constraint violation does not.
	db := openDB(t)
	repo := cv.NewPostgresRepository(db)
	_, versionID := seedVersion(t, db)

	err := repo.StoreEmbeddings(context.Background(), []cv.StoredEmbedding{
		{CVVersionID: versionID, Kind: embed.Kind("experience"), Model: "m1", Vector: unitVector(8, 0)},
	})
	if err == nil {
		t.Fatal("StoreEmbeddings accepted the kind \"experience\"")
	}
}

func TestStoreEmbeddingsRejectsAnUnusableVector(t *testing.T) {
	// A zero vector has no direction, so cosine similarity against it is undefined.
	// Storing one would create a row that fails every later comparison, and the failure
	// would surface during matching rather than here.
	db := openDB(t)
	repo := cv.NewPostgresRepository(db)
	_, versionID := seedVersion(t, db)

	for name, v := range map[string]embed.Vector{
		"empty":     {},
		"all zeros": {0, 0, 0},
	} {
		t.Run(name, func(t *testing.T) {
			err := repo.StoreEmbeddings(context.Background(), []cv.StoredEmbedding{
				{CVVersionID: versionID, Kind: embed.KindTitle, Model: "m1", Vector: v},
			})
			if err == nil {
				t.Fatalf("StoreEmbeddings accepted a vector that is %s", name)
			}
		})
	}
}

func TestStoreEmbeddingsForADeletedVersionReportsNotFound(t *testing.T) {
	// The version can be deleted while embedding is in flight. The consumer treats
	// not-found as "drop this result and ack", so it must be distinguishable from a
	// transient failure that should be retried.
	db := openDB(t)
	repo := cv.NewPostgresRepository(db)

	err := repo.StoreEmbeddings(context.Background(), []cv.StoredEmbedding{
		{CVVersionID: uuid.New(), Kind: embed.KindTitle, Model: "m1", Vector: unitVector(8, 0)},
	})
	if apperr.KindOf(err) != apperr.KindNotFound {
		t.Fatalf("err = %v (kind %v), want not found", err, apperr.KindOf(err))
	}
}

func TestEmbeddingInputsGathersTheThreeFocusedAspects(t *testing.T) {
	db := openDB(t)
	repo := cv.NewPostgresRepository(db)
	ctx := context.Background()
	userID, versionID := seedVersion(t, db)

	parsed := sampleParsed(userID, versionID)
	parsed.Contact.Headline = "Senior Backend Engineer"
	parsed.Summary = "Backend engineer with eight years building distributed Go services"
	err := repo.StoreParsedData(ctx, parsed, []cv.Skill{
		{RawLabel: "Go", Source: cv.SkillSourceAI},
		{RawLabel: "PostgreSQL", Source: cv.SkillSourceAI},
	})
	if err != nil {
		t.Fatalf("StoreParsedData: %v", err)
	}

	input, err := repo.EmbeddingInputs(ctx, versionID)
	if err != nil {
		t.Fatalf("EmbeddingInputs: %v", err)
	}

	// The title comes from the most recent role, not the headline, when both exist.
	if input.MostRecentTitle != "Senior Engineer" {
		t.Errorf("MostRecentTitle = %q, want the first experience entry's title", input.MostRecentTitle)
	}
	if input.TitleText() != "Senior Engineer" {
		t.Errorf("TitleText = %q", input.TitleText())
	}
	if input.Summary == "" {
		t.Error("Summary is empty")
	}
	// Raw labels are used when nothing in the taxonomy matched, so skills are never
	// silently empty just because normalisation has not run.
	if input.Skills != "Go, PostgreSQL" {
		t.Errorf("Skills = %q, want \"Go, PostgreSQL\"", input.Skills)
	}

	kinds, texts := input.Texts()
	if len(kinds) != len(texts) {
		t.Fatalf("%d kinds for %d texts", len(kinds), len(texts))
	}
	if len(kinds) != 3 {
		t.Fatalf("%d kinds, want title, skills and summary: %v", len(kinds), kinds)
	}
	if !input.HasAnything() {
		t.Error("HasAnything = false despite all three aspects being present")
	}
}

func TestEmbeddingInputsFallsBackToTheHeadlineForTheTitle(t *testing.T) {
	// A candidate whose CV lists no roles should still get a title vector rather than
	// none, or they drop out of title-based matching entirely.
	db := openDB(t)
	repo := cv.NewPostgresRepository(db)
	ctx := context.Background()
	userID, versionID := seedVersion(t, db)

	parsed := sampleParsed(userID, versionID)
	parsed.Experience = nil
	parsed.Contact.Headline = "Freelance Data Scientist"
	if err := repo.StoreParsedData(ctx, parsed, nil); err != nil {
		t.Fatalf("StoreParsedData: %v", err)
	}

	input, err := repo.EmbeddingInputs(ctx, versionID)
	if err != nil {
		t.Fatalf("EmbeddingInputs: %v", err)
	}
	if input.TitleText() != "Freelance Data Scientist" {
		t.Fatalf("TitleText = %q, want the headline", input.TitleText())
	}
}

func TestEmbeddingInputsSkipsEmptyAspects(t *testing.T) {
	// An empty string embeds to a zero vector, which cannot be compared, so an empty
	// aspect must never reach the embedder.
	db := openDB(t)
	repo := cv.NewPostgresRepository(db)
	ctx := context.Background()
	userID, versionID := seedVersion(t, db)

	err := repo.StoreParsedData(ctx, &cv.ParsedData{
		CVVersionID:     versionID,
		UserID:          userID,
		CorrectedFields: []string{},
		EngineVersion:   "cv-extract-v1",
	}, nil)
	if err != nil {
		t.Fatalf("StoreParsedData: %v", err)
	}

	input, err := repo.EmbeddingInputs(ctx, versionID)
	if err != nil {
		t.Fatalf("EmbeddingInputs: %v", err)
	}

	kinds, texts := input.Texts()
	if len(kinds) != 0 || len(texts) != 0 {
		t.Fatalf("a CV with nothing in it produced %d kinds: %v", len(kinds), kinds)
	}
	if input.HasAnything() {
		t.Error("HasAnything = true for a CV with no title, skills or summary")
	}
}

func TestEmbeddingInputsForAnUnparsedVersionReportsNotFound(t *testing.T) {
	db := openDB(t)
	repo := cv.NewPostgresRepository(db)
	_, versionID := seedVersion(t, db)

	_, err := repo.EmbeddingInputs(context.Background(), versionID)
	if apperr.KindOf(err) != apperr.KindNotFound {
		t.Fatalf("err = %v (kind %v), want not found", err, apperr.KindOf(err))
	}
}

func TestVersionsNeedingEmbeddingIsTheOutageRecoveryPath(t *testing.T) {
	// This is how matching recovers from an embedding outage. While the provider is
	// down CVs still parse and still match — lexically, marked degraded. When it comes
	// back this query finds everything that was missed, so no candidate is left
	// permanently unembedded by an outage they never saw.
	db := openDB(t)
	repo := cv.NewPostgresRepository(db)
	ctx := context.Background()

	userID, versionID := seedVersion(t, db)
	markEmbeddable(t, repo, userID, versionID)

	const model = "recovery-test-model"

	pending, err := repo.VersionsNeedingEmbedding(ctx, model, 500)
	if err != nil {
		t.Fatalf("VersionsNeedingEmbedding: %v", err)
	}
	if !containsID(pending, versionID) {
		t.Fatal("a parsed, clean version with no vectors was not reported as needing embedding")
	}

	// Embed it, as the recovered provider would.
	err = repo.StoreEmbeddings(ctx, []cv.StoredEmbedding{
		{CVVersionID: versionID, Kind: embed.KindTitle, Model: model, Vector: unitVector(8, 0)},
	})
	if err != nil {
		t.Fatalf("StoreEmbeddings: %v", err)
	}

	pending, err = repo.VersionsNeedingEmbedding(ctx, model, 500)
	if err != nil {
		t.Fatalf("VersionsNeedingEmbedding: %v", err)
	}
	if containsID(pending, versionID) {
		t.Fatal("an embedded version is still reported as needing embedding; the backfill would loop")
	}

	// A model change makes it pending again, which is right: its old vectors are not
	// comparable with postings embedded by the new model.
	pending, err = repo.VersionsNeedingEmbedding(ctx, "a-different-model", 500)
	if err != nil {
		t.Fatalf("VersionsNeedingEmbedding: %v", err)
	}
	if !containsID(pending, versionID) {
		t.Fatal("a model change did not make the version pending again")
	}
}

func TestVersionsNeedingEmbeddingSkipsVersionsThatCannotBeMatched(t *testing.T) {
	// Embedding an unparsed or unscanned version spends the budget on a CV that cannot
	// be matched anyway, and in the unscanned case on one that may yet be quarantined.
	db := openDB(t)
	repo := cv.NewPostgresRepository(db)
	ctx := context.Background()

	t.Run("parsed but never scanned clean", func(t *testing.T) {
		userID, versionID := seedVersion(t, db)
		if err := repo.StoreParsedData(ctx, sampleParsed(userID, versionID), nil); err != nil {
			t.Fatalf("StoreParsedData: %v", err)
		}
		if err := repo.MarkParsed(ctx, versionID, time.Now().UTC()); err != nil {
			t.Fatalf("MarkParsed: %v", err)
		}

		pending, err := repo.VersionsNeedingEmbedding(ctx, "some-model", 500)
		if err != nil {
			t.Fatalf("VersionsNeedingEmbedding: %v", err)
		}
		if containsID(pending, versionID) {
			t.Error("an unscanned version was reported as needing embedding")
		}
	})

	t.Run("scanned clean but not parsed", func(t *testing.T) {
		_, versionID := seedVersion(t, db)
		if err := repo.UpdateAVStatus(ctx, versionID, cv.AVClean, time.Now().UTC()); err != nil {
			t.Fatalf("UpdateAVStatus: %v", err)
		}

		pending, err := repo.VersionsNeedingEmbedding(ctx, "some-model", 500)
		if err != nil {
			t.Fatalf("VersionsNeedingEmbedding: %v", err)
		}
		if containsID(pending, versionID) {
			t.Error("an unparsed version was reported as needing embedding")
		}
	})
}

func TestVersionsNeedingEmbeddingRespectsTheLimit(t *testing.T) {
	// A backfill after an outage must not monopolise the worker and starve live uploads.
	db := openDB(t)
	repo := cv.NewPostgresRepository(db)

	for i := 0; i < 3; i++ {
		userID, versionID := seedVersion(t, db)
		markEmbeddable(t, repo, userID, versionID)
	}

	pending, err := repo.VersionsNeedingEmbedding(context.Background(), "limit-test-model", 2)
	if err != nil {
		t.Fatalf("VersionsNeedingEmbedding: %v", err)
	}
	if len(pending) > 2 {
		t.Fatalf("%d versions returned, want at most the limit of 2", len(pending))
	}
}

func containsID(ids []uuid.UUID, want uuid.UUID) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

func TestRecoveryQueriesExcludeVersionsThatCanNeverBeEmbedded(t *testing.T) {
	// A version marked parsed on extracted text alone has no structured data, which is
	// the normal state wherever the AI extractor is disabled. The backfill can never do
	// anything with it, so it must not be queued for recovery: observed live, a sweep
	// reported "found 7, embedded 1, skipped 6" and then re-examined those same 6 on
	// every sweep for ever.
	db := openDB(t)
	repo := cv.NewPostgresRepository(db)
	ctx := context.Background()

	// Parsed and clean, but no cv_parsed_data row.
	_, textOnlyID := seedVersion(t, db)
	now := time.Now().UTC()
	if err := repo.UpdateAVStatus(ctx, textOnlyID, cv.AVClean, now); err != nil {
		t.Fatalf("UpdateAVStatus: %v", err)
	}
	if err := repo.MarkParsed(ctx, textOnlyID, now); err != nil {
		t.Fatalf("MarkParsed: %v", err)
	}

	// And one that is genuinely embeddable, to prove the clause is not excluding
	// everything.
	userID, embeddableID := seedVersion(t, db)
	markEmbeddable(t, repo, userID, embeddableID)

	t.Run("model scoped", func(t *testing.T) {
		pending, err := repo.VersionsNeedingEmbedding(ctx, "never-used-model", 500)
		if err != nil {
			t.Fatalf("VersionsNeedingEmbedding: %v", err)
		}
		if containsID(pending, textOnlyID) {
			t.Error("a version with no structured data was queued for recovery; the sweep would never drain")
		}
		if !containsID(pending, embeddableID) {
			t.Error("an embeddable version was excluded")
		}
	})

	t.Run("cold start", func(t *testing.T) {
		pending, err := repo.VersionsWithoutAnyEmbedding(ctx, 500)
		if err != nil {
			t.Fatalf("VersionsWithoutAnyEmbedding: %v", err)
		}
		if containsID(pending, textOnlyID) {
			t.Error("a version with no structured data was queued for recovery")
		}
		if !containsID(pending, embeddableID) {
			t.Error("an embeddable version was excluded")
		}
	})
}

func TestVersionsWithoutAnyEmbeddingIgnoresTheModel(t *testing.T) {
	// The cold-start query exists precisely because the worker does not yet know the
	// provider's model name, so a version with vectors under any model must drop out.
	db := openDB(t)
	repo := cv.NewPostgresRepository(db)
	ctx := context.Background()

	userID, versionID := seedVersion(t, db)
	markEmbeddable(t, repo, userID, versionID)

	pending, err := repo.VersionsWithoutAnyEmbedding(ctx, 500)
	if err != nil {
		t.Fatalf("VersionsWithoutAnyEmbedding: %v", err)
	}
	if !containsID(pending, versionID) {
		t.Fatal("a version with no vectors at all was not returned")
	}

	err = repo.StoreEmbeddings(ctx, []cv.StoredEmbedding{
		{CVVersionID: versionID, Kind: embed.KindTitle, Model: "some-unknown-model", Vector: unitVector(8, 0)},
	})
	if err != nil {
		t.Fatalf("StoreEmbeddings: %v", err)
	}

	pending, err = repo.VersionsWithoutAnyEmbedding(ctx, 500)
	if err != nil {
		t.Fatalf("VersionsWithoutAnyEmbedding: %v", err)
	}
	if containsID(pending, versionID) {
		t.Fatal("a version with vectors under some model is still reported as having none")
	}
}
