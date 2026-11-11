//go:build integration

package cv_test

import (
	"context"
	"os"
	"testing"

	"github.com/Joakim-animate90/myjobfyt/backend/internal/apperr"
	"github.com/Joakim-animate90/myjobfyt/backend/internal/config"
	"github.com/Joakim-animate90/myjobfyt/backend/internal/domain/cv"
	"github.com/Joakim-animate90/myjobfyt/backend/internal/platform/postgres"
	"github.com/google/uuid"
)

// These tests run against the real schema, which is where the value is: the jsonb CHECK
// constraints, the ownership triggers, the unique indexes and the correction-preserving
// transaction cannot be verified against a fake.

func openDB(t *testing.T) *postgres.DB {
	t.Helper()
	dsn := os.Getenv("POSTGRES_DSN")
	if dsn == "" {
		t.Skip("POSTGRES_DSN not set; run with the development stack up")
	}
	db, err := postgres.Open(context.Background(), config.PostgresConfig{
		DSN: dsn, MaxConns: 4, MinConns: 1, ConnectTimeout: 5_000_000_000,
	})
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	t.Cleanup(db.Close)
	return db
}

// seedVersion creates the user, document and version a parsed row must hang off, and
// returns their ids.
func seedVersion(t *testing.T, db *postgres.DB) (userID, versionID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	userID = uuid.New()
	documentID := uuid.New()
	versionID = uuid.New()

	email := "cvtest-" + userID.String()[:8] + "@example.com"

	_, err := db.Querier(ctx).Exec(ctx,
		`INSERT INTO users (id, email, password_hash, email_verified_at)
		 VALUES ($1, $2, 'x', now())`, userID, email)
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	_, err = db.Querier(ctx).Exec(ctx,
		`INSERT INTO candidate_profiles (user_id) VALUES ($1)`, userID)
	if err != nil {
		t.Fatalf("seed profile: %v", err)
	}
	_, err = db.Querier(ctx).Exec(ctx,
		`INSERT INTO cv_documents (id, user_id, content_type, size_bytes)
		 VALUES ($1, $2, 'application/pdf', 1024)`, documentID, userID)
	if err != nil {
		t.Fatalf("seed document: %v", err)
	}
	_, err = db.Querier(ctx).Exec(ctx,
		`INSERT INTO cv_versions (id, cv_document_id, user_id, version_no, storage_key,
		                          checksum_sha256, size_bytes, raw_text)
		 VALUES ($1, $2, $3, 1, $4, '\x00', 1024, 'Joakim Candidate at Acme Corporation')`,
		versionID, documentID, userID, "tenants/"+userID.String()+"/"+versionID.String())
	if err != nil {
		t.Fatalf("seed version: %v", err)
	}

	t.Cleanup(func() {
		// Cascades clear everything below the user.
		_, _ = db.Querier(context.Background()).Exec(context.Background(),
			`DELETE FROM users WHERE id = $1`, userID)
	})
	return userID, versionID
}

func sampleParsed(userID, versionID uuid.UUID) *cv.ParsedData {
	months := 24
	return &cv.ParsedData{
		CVVersionID: versionID,
		UserID:      userID,
		Summary:     "Backend engineer",
		Contact: cv.Contact{
			FullName: "Joakim Candidate",
			Email:    "joakim@example.com",
		},
		Experience: []cv.Role{{
			Employer: "Acme Corporation", Title: "Senior Engineer",
			StartDate: "Jan 2020", EndDate: "Dec 2021", DurationMonths: &months,
		}},
		Education:             []cv.Study{{Institution: "University of Nairobi", Qualification: "BSc"}},
		Certifications:        []cv.Certification{{Name: "CKA"}},
		Languages:             []cv.Language{{Language: "en", Proficiency: "native"}},
		CorrectedFields:       []string{},
		TotalExperienceMonths: &months,
		DetectedSeniority:     "Senior",
		EngineVersion:         "cv-extract-v1",
		Provenance: cv.VerificationReport{
			Verified: 2, NeedsReview: 1,
			Fields: []cv.FieldProvenance{
				{Field: "full_name", Status: cv.VerifiedExact, StartOffset: 0, EndOffset: 16, Confidence: 0.95},
				{Field: "experience[0].employer", Status: cv.VerifiedExact, StartOffset: 20, EndOffset: 36, Confidence: 0.95},
			},
		},
	}
}

func TestStoreAndReadParsedData(t *testing.T) {
	db := openDB(t)
	repo := cv.NewPostgresRepository(db)
	ctx := context.Background()
	userID, versionID := seedVersion(t, db)

	parsed := sampleParsed(userID, versionID)
	skills := []cv.Skill{
		{RawLabel: "Go", Source: cv.SkillSourceAI},
		{RawLabel: "PostgreSQL", Source: cv.SkillSourceAI},
	}

	if err := repo.StoreParsedData(ctx, parsed, skills); err != nil {
		t.Fatalf("StoreParsedData: %v", err)
	}

	read, err := repo.ParsedDataByVersion(ctx, userID, versionID)
	if err != nil {
		t.Fatalf("ParsedDataByVersion: %v", err)
	}

	if read.Contact.FullName != "Joakim Candidate" {
		t.Errorf("full name = %q", read.Contact.FullName)
	}
	if len(read.Experience) != 1 || read.Experience[0].Employer != "Acme Corporation" {
		t.Errorf("experience = %+v", read.Experience)
	}
	if read.DetectedSeniority != "Senior" {
		t.Errorf("seniority = %q", read.DetectedSeniority)
	}
	if read.TotalExperienceMonths == nil || *read.TotalExperienceMonths != 24 {
		t.Errorf("total months = %v", read.TotalExperienceMonths)
	}
	// Provenance must survive the round trip, or the review UI has nothing to show.
	if len(read.Provenance.Fields) != 2 {
		t.Errorf("provenance fields = %d, want 2", len(read.Provenance.Fields))
	}
	if read.Provenance.Fields[0].StartOffset != 0 || read.Provenance.Fields[0].EndOffset != 16 {
		t.Errorf("offsets did not survive: %+v", read.Provenance.Fields[0])
	}

	storedSkills, err := repo.SkillsByVersion(ctx, userID, versionID)
	if err != nil {
		t.Fatalf("SkillsByVersion: %v", err)
	}
	if len(storedSkills) != 2 {
		t.Fatalf("%d skills stored, want 2", len(storedSkills))
	}
}

func TestEmptySectionsSatisfyTheDatabaseChecks(t *testing.T) {
	// experience and education have jsonb_typeof = 'array' CHECKs, so a nil slice
	// marshalled as null would reject the whole write. A CV with no listed education
	// is entirely normal.
	db := openDB(t)
	repo := cv.NewPostgresRepository(db)
	ctx := context.Background()
	userID, versionID := seedVersion(t, db)

	parsed := &cv.ParsedData{
		CVVersionID:     versionID,
		UserID:          userID,
		CorrectedFields: []string{},
		EngineVersion:   "cv-extract-v1",
	}

	if err := repo.StoreParsedData(ctx, parsed, nil); err != nil {
		t.Fatalf("StoreParsedData with empty sections: %v", err)
	}

	read, err := repo.ParsedDataByVersion(ctx, userID, versionID)
	if err != nil {
		t.Fatalf("ParsedDataByVersion: %v", err)
	}
	if len(read.Experience) != 0 || len(read.Education) != 0 {
		t.Fatalf("empty sections came back non-empty: %+v", read)
	}
}

func TestReParsePreservesACorrectionThroughTheRealTransaction(t *testing.T) {
	// The whole point of MJF-005's correction rule, exercised against the real
	// read-lock-merge-write path rather than against a fake.
	db := openDB(t)
	repo := cv.NewPostgresRepository(db)
	ctx := context.Background()
	userID, versionID := seedVersion(t, db)

	original := sampleParsed(userID, versionID)
	if err := repo.StoreParsedData(ctx, original, []cv.Skill{{RawLabel: "Go", Source: cv.SkillSourceAI}}); err != nil {
		t.Fatalf("first store: %v", err)
	}

	// The candidate edits their email. This goes through the candidate path, which
	// APPLIES the value and records the field — not the re-parse path, which preserves
	// what is already stored.
	corrected := sampleParsed(userID, versionID)
	corrected.Contact.Email = "the-real-address@example.com"
	if err := repo.StoreCandidateEdit(ctx, corrected, []string{"email"}); err != nil {
		t.Fatalf("StoreCandidateEdit: %v", err)
	}

	// Confirm the edit actually landed before testing that a re-parse respects it —
	// otherwise a failure below cannot be told apart from the edit never applying.
	afterEdit, err := repo.ParsedDataByVersion(ctx, userID, versionID)
	if err != nil {
		t.Fatalf("read after the edit: %v", err)
	}
	if afterEdit.Contact.Email != "the-real-address@example.com" {
		t.Fatalf("the candidate's edit did not apply: email = %q", afterEdit.Contact.Email)
	}
	if !afterEdit.IsCorrected("email") {
		t.Fatalf("the edit did not record the field as corrected: %v", afterEdit.CorrectedFields)
	}

	// A re-parse arrives with a different email.
	reparsed := sampleParsed(userID, versionID)
	reparsed.Contact.Email = "model-guessed@example.com"
	reparsed.Contact.FullName = "J. Candidate"
	if err := repo.StoreParsedData(ctx, reparsed, nil); err != nil {
		t.Fatalf("re-parse store: %v", err)
	}

	read, err := repo.ParsedDataByVersion(ctx, userID, versionID)
	if err != nil {
		t.Fatalf("ParsedDataByVersion: %v", err)
	}

	if read.Contact.Email != "the-real-address@example.com" {
		t.Fatalf("email = %q; the candidate's correction was overwritten by the re-parse", read.Contact.Email)
	}
	// The un-corrected field must take the new value.
	if read.Contact.FullName != "J. Candidate" {
		t.Errorf("full name = %q; the re-parse did not update an un-corrected field", read.Contact.FullName)
	}
	// And the correction must still be recorded, so a third parse also respects it.
	if !read.IsCorrected("email") {
		t.Fatalf("the correction list was lost: %v", read.CorrectedFields)
	}
}

func TestCorrectedSkillsAreNotReplacedByAReParse(t *testing.T) {
	db := openDB(t)
	repo := cv.NewPostgresRepository(db)
	ctx := context.Background()
	userID, versionID := seedVersion(t, db)

	parsed := sampleParsed(userID, versionID)
	if err := repo.StoreParsedData(ctx, parsed, []cv.Skill{
		{RawLabel: "Go", Source: cv.SkillSourceCandidate},
		{RawLabel: "Kubernetes", Source: cv.SkillSourceCandidate},
	}); err != nil {
		t.Fatalf("first store: %v", err)
	}
	if err := repo.RecordCorrection(ctx, userID, versionID, "skills"); err != nil {
		t.Fatalf("RecordCorrection: %v", err)
	}
	_ = parsed

	// A re-parse finds a completely different skill set.
	reparsed := sampleParsed(userID, versionID)
	reparsed.CorrectedFields = []string{"skills"}
	if err := repo.StoreParsedData(ctx, reparsed, []cv.Skill{
		{RawLabel: "COBOL", Source: cv.SkillSourceAI},
	}); err != nil {
		t.Fatalf("re-parse store: %v", err)
	}

	skills, err := repo.SkillsByVersion(ctx, userID, versionID)
	if err != nil {
		t.Fatalf("SkillsByVersion: %v", err)
	}
	if len(skills) != 2 {
		t.Fatalf("%d skills, want the candidate's 2 preserved: %+v", len(skills), skills)
	}
	for _, s := range skills {
		if s.RawLabel == "COBOL" {
			t.Fatal("the re-parse replaced the candidate's corrected skills")
		}
	}
}

func TestDuplicateSkillLabelsDoNotFailTheWrite(t *testing.T) {
	// The unique index on (cv_version_id, raw_label) would otherwise reject the second
	// row and abort the whole extraction.
	db := openDB(t)
	repo := cv.NewPostgresRepository(db)
	ctx := context.Background()
	userID, versionID := seedVersion(t, db)

	err := repo.StoreParsedData(ctx, sampleParsed(userID, versionID), []cv.Skill{
		{RawLabel: "Go", Source: cv.SkillSourceAI},
		{RawLabel: "Go", Source: cv.SkillSourceAI},
	})
	if err != nil {
		t.Fatalf("StoreParsedData with duplicate labels: %v", err)
	}

	skills, err := repo.SkillsByVersion(ctx, userID, versionID)
	if err != nil {
		t.Fatalf("SkillsByVersion: %v", err)
	}
	if len(skills) != 1 {
		t.Fatalf("%d skills stored, want 1 after the duplicate was skipped", len(skills))
	}
}

func TestParsedDataIsTenantScoped(t *testing.T) {
	db := openDB(t)
	repo := cv.NewPostgresRepository(db)
	ctx := context.Background()

	ownerID, versionID := seedVersion(t, db)
	intruderID, _ := seedVersion(t, db)

	if err := repo.StoreParsedData(ctx, sampleParsed(ownerID, versionID), nil); err != nil {
		t.Fatalf("StoreParsedData: %v", err)
	}

	// Another candidate must not be able to read it, and must not be able to tell it
	// exists.
	if _, err := repo.ParsedDataByVersion(ctx, intruderID, versionID); apperr.KindOf(err) != apperr.KindNotFound {
		t.Fatalf("cross-tenant read returned %v, want not found", err)
	}
	if skills, err := repo.SkillsByVersion(ctx, intruderID, versionID); err != nil || len(skills) != 0 {
		t.Fatalf("cross-tenant skills read returned %d rows (err %v), want 0", len(skills), err)
	}
	if err := repo.RecordCorrection(ctx, intruderID, versionID, "email"); apperr.KindOf(err) != apperr.KindNotFound {
		t.Fatalf("cross-tenant correction returned %v, want not found", err)
	}

	// The owner's data is untouched.
	read, err := repo.ParsedDataByVersion(ctx, ownerID, versionID)
	if err != nil {
		t.Fatalf("owner read: %v", err)
	}
	if read.IsCorrected("email") {
		t.Fatal("the intruder's correction was applied to the owner's data")
	}
}

func TestRecordCorrectionIsIdempotent(t *testing.T) {
	db := openDB(t)
	repo := cv.NewPostgresRepository(db)
	ctx := context.Background()
	userID, versionID := seedVersion(t, db)

	if err := repo.StoreParsedData(ctx, sampleParsed(userID, versionID), nil); err != nil {
		t.Fatalf("StoreParsedData: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := repo.RecordCorrection(ctx, userID, versionID, "email"); err != nil {
			t.Fatalf("RecordCorrection %d: %v", i, err)
		}
	}

	read, err := repo.ParsedDataByVersion(ctx, userID, versionID)
	if err != nil {
		t.Fatalf("ParsedDataByVersion: %v", err)
	}
	if len(read.CorrectedFields) != 1 {
		t.Fatalf("CorrectedFields = %v, want one entry after three identical corrections", read.CorrectedFields)
	}
}

func TestRawTextIsReadableForTheStructureStage(t *testing.T) {
	db := openDB(t)
	repo := cv.NewPostgresRepository(db)
	ctx := context.Background()
	_, versionID := seedVersion(t, db)

	text, err := repo.RawText(ctx, versionID)
	if err != nil {
		t.Fatalf("RawText: %v", err)
	}
	if text == "" {
		t.Fatal("RawText returned nothing for a version with stored text")
	}

	if _, err := repo.RawText(ctx, uuid.New()); apperr.KindOf(err) != apperr.KindNotFound {
		t.Fatalf("RawText for an unknown version = %v, want not found", err)
	}
}

func TestStoreParsedDataForADeletedVersionReportsNotFound(t *testing.T) {
	// The version can be deleted while an extraction is in flight. The worker treats
	// not-found as "drop the result", so the error must be distinguishable.
	db := openDB(t)
	repo := cv.NewPostgresRepository(db)
	ctx := context.Background()
	userID := uuid.New()

	parsed := &cv.ParsedData{
		CVVersionID:     uuid.New(),
		UserID:          userID,
		CorrectedFields: []string{},
		EngineVersion:   "cv-extract-v1",
	}
	if err := repo.StoreParsedData(ctx, parsed, nil); apperr.KindOf(err) != apperr.KindNotFound {
		t.Fatalf("StoreParsedData for a missing version = %v, want not found", err)
	}
}

func TestCandidateEditAppliesWhileReParsePreserves(t *testing.T) {
	// The two paths exist because they must behave oppositely on the same field. This
	// pins that difference: conflating them silently discarded the candidate's edit.
	db := openDB(t)
	repo := cv.NewPostgresRepository(db)
	ctx := context.Background()
	userID, versionID := seedVersion(t, db)

	if err := repo.StoreParsedData(ctx, sampleParsed(userID, versionID), nil); err != nil {
		t.Fatalf("initial parse: %v", err)
	}

	edit := sampleParsed(userID, versionID)
	edit.Summary = "What the candidate actually wrote"
	if err := repo.StoreCandidateEdit(ctx, edit, []string{"summary"}); err != nil {
		t.Fatalf("StoreCandidateEdit: %v", err)
	}

	applied, err := repo.ParsedDataByVersion(ctx, userID, versionID)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if applied.Summary != "What the candidate actually wrote" {
		t.Fatalf("the edit did not apply: summary = %q", applied.Summary)
	}

	reparse := sampleParsed(userID, versionID)
	reparse.Summary = "What the model thinks"
	if err := repo.StoreParsedData(ctx, reparse, nil); err != nil {
		t.Fatalf("re-parse: %v", err)
	}

	preserved, err := repo.ParsedDataByVersion(ctx, userID, versionID)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if preserved.Summary != "What the candidate actually wrote" {
		t.Fatalf("the re-parse overwrote the edit: summary = %q", preserved.Summary)
	}
}

func TestCandidateEditOnAMissingRecordReportsNotFound(t *testing.T) {
	db := openDB(t)
	repo := cv.NewPostgresRepository(db)
	ctx := context.Background()
	userID, versionID := seedVersion(t, db)

	// No parse has run, so there is nothing to edit.
	err := repo.StoreCandidateEdit(ctx, sampleParsed(userID, versionID), []string{"summary"})
	if apperr.KindOf(err) != apperr.KindNotFound {
		t.Fatalf("StoreCandidateEdit with no existing record = %v, want not found", err)
	}
}
