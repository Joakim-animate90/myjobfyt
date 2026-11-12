//go:build integration

package cv_test

import (
	"context"
	"testing"

	"github.com/Joakim-animate90/myjobfyt/backend/internal/apperr"
	"github.com/Joakim-animate90/myjobfyt/backend/internal/domain/cv"
	"github.com/Joakim-animate90/myjobfyt/backend/internal/platform/postgres"
	"github.com/google/uuid"
)

// tailoredVersionFor builds a version row derived from an existing one.
func tailoredVersionFor(t *testing.T, db *postgres.DB, userID, sourceVersionID uuid.UUID, passed *bool) *cv.Version {
	t.Helper()
	ctx := context.Background()

	var documentID uuid.UUID
	err := db.Querier(ctx).QueryRow(ctx,
		`SELECT cv_document_id FROM cv_versions WHERE id = $1`, sourceVersionID).Scan(&documentID)
	if err != nil {
		t.Fatalf("read document id: %v", err)
	}

	id := uuid.New()
	jobPostingID := seedJobPosting(t, db)
	return &cv.Version{
		ID: id, DocumentID: documentID, UserID: userID,
		StorageKey:     "tenants/" + userID.String() + "/" + id.String(),
		ChecksumSHA256: make([]byte, 32), SizeBytes: 2048,
		AVStatus: cv.AVClean, ParseStatus: cv.ParseParsed,
		DerivedFromVersionID:    &sourceVersionID,
		TailoredForJobPostingID: &jobPostingID,
		ContainmentPassed:       passed,
	}
}

func TestATailoredVersionIsStoredWithItsContainmentVerdict(t *testing.T) {
	db := openDB(t)
	repo := cv.NewPostgresRepository(db)
	ctx := context.Background()

	userID, sourceID := seedVersion(t, db)
	if err := repo.StoreParsedData(ctx, sampleParsed(userID, sourceID), nil); err != nil {
		t.Fatalf("seed source parsed data: %v", err)
	}

	passed := true
	version := tailoredVersionFor(t, db, userID, sourceID, &passed)

	tailored := &cv.ParsedData{
		Summary:         "Backend engineer focused on payments",
		Contact:         cv.Contact{Headline: "Senior Engineer"},
		Experience:      []cv.Role{{Title: "Senior Engineer", Employer: "Acme Corporation"}},
		CorrectedFields: []string{},
		EngineVersion:   "cv-tailor-v1",
	}

	if err := repo.StoreTailoredVersion(ctx, version, tailored, nil); err != nil {
		t.Fatalf("StoreTailoredVersion: %v", err)
	}

	var (
		containmentPassed *bool
		derivedFrom       *uuid.UUID
		versionNo         int
	)
	err := db.Querier(ctx).QueryRow(ctx,
		`SELECT containment_passed, derived_from_version_id, version_no
		 FROM cv_versions WHERE id = $1`, version.ID).
		Scan(&containmentPassed, &derivedFrom, &versionNo)
	if err != nil {
		t.Fatalf("read tailored version: %v", err)
	}

	if containmentPassed == nil || !*containmentPassed {
		t.Error("containment_passed was not recorded on the tailored version")
	}
	if derivedFrom == nil || *derivedFrom != sourceID {
		t.Error("the tailored version does not record the version it came from, so its provenance is unverifiable")
	}
	if versionNo <= 1 {
		t.Errorf("version_no = %d, want a number after the source's", versionNo)
	}

	// The content committed with the row rather than separately.
	stored, err := repo.ParsedDataByVersion(ctx, userID, version.ID)
	if err != nil {
		t.Fatalf("ParsedDataByVersion: %v", err)
	}
	if stored.Summary != tailored.Summary {
		t.Errorf("summary = %q, want the tailored text", stored.Summary)
	}
}

func TestStoringATailoredVersionWithoutAVerdictIsRefused(t *testing.T) {
	// The verdict is the only thing standing between a candidate and a fabricated CV.
	// A caller reaching here without one has bypassed the gate.
	db := openDB(t)
	repo := cv.NewPostgresRepository(db)
	ctx := context.Background()

	userID, sourceID := seedVersion(t, db)
	failed := false

	for name, passed := range map[string]*bool{
		"no verdict":     nil,
		"failed verdict": &failed,
	} {
		t.Run(name, func(t *testing.T) {
			version := tailoredVersionFor(t, db, userID, sourceID, passed)
			err := repo.StoreTailoredVersion(ctx, version,
				&cv.ParsedData{CorrectedFields: []string{}, EngineVersion: "cv-tailor-v1"}, nil)
			if err == nil {
				t.Fatal("a tailored version was stored without a passing containment verdict")
			}

			var exists bool
			if qerr := db.Querier(ctx).QueryRow(ctx,
				`SELECT EXISTS (SELECT 1 FROM cv_versions WHERE id = $1)`, version.ID).Scan(&exists); qerr != nil {
				t.Fatalf("check row: %v", qerr)
			}
			if exists {
				t.Error("the rejected version was written anyway")
			}
		})
	}
}

func TestTheDatabaseItselfRefusesATailoredVersionWithoutAVerdict(t *testing.T) {
	// Independently of the repository's own guard. The constraint is the last line of
	// defence: a future code path that forgets the check must still fail.
	db := openDB(t)
	ctx := context.Background()

	userID, sourceID := seedVersion(t, db)
	var documentID uuid.UUID
	if err := db.Querier(ctx).QueryRow(ctx,
		`SELECT cv_document_id FROM cv_versions WHERE id = $1`, sourceID).Scan(&documentID); err != nil {
		t.Fatalf("read document id: %v", err)
	}

	id := uuid.New()
	_, err := db.Querier(ctx).Exec(ctx, `
		INSERT INTO cv_versions (id, cv_document_id, user_id, version_no, storage_key,
		                         checksum_sha256, size_bytes,
		                         derived_from_version_id, tailored_for_job_posting_id,
		                         containment_passed)
		VALUES ($1, $2, $3, 99, $4, '\x00', 2048, $5, $6, NULL)`,
		id, documentID, userID, "tenants/"+userID.String()+"/"+id.String(), sourceID, uuid.New())

	if err == nil {
		t.Fatal("the database accepted a tailored version with no containment verdict")
	}
	if name := postgres.ConstraintName(err); name != "cv_versions_tailored_requires_containment" {
		t.Errorf("violated constraint = %q, want cv_versions_tailored_requires_containment", name)
	}
}

func TestATailoredVersionMustRecordWhereItCameFrom(t *testing.T) {
	// Without the base version its provenance is unverifiable: nothing can later check
	// what it was tailored from, which is the whole basis of the containment claim.
	db := openDB(t)
	repo := cv.NewPostgresRepository(db)
	ctx := context.Background()

	userID, sourceID := seedVersion(t, db)
	passed := true
	version := tailoredVersionFor(t, db, userID, sourceID, &passed)
	version.DerivedFromVersionID = nil

	err := repo.StoreTailoredVersion(ctx, version, &cv.ParsedData{CorrectedFields: []string{}, EngineVersion: "cv-tailor-v1"}, nil)
	if err == nil {
		t.Fatal("a tailored version was stored with no base version")
	}
	if apperr.KindOf(err) != apperr.KindInternal {
		t.Errorf("err kind = %v, want internal; this is a caller bug, not bad user input", apperr.KindOf(err))
	}
}

// seedJobPosting creates the posting a tailored version points at.
//
// cv_versions.tailored_for_job_posting_id is a real foreign key, which is the point:
// a tailored CV must be attributable to the posting it was tailored for, or its
// provenance claim means nothing.
func seedJobPosting(t *testing.T, db *postgres.DB) uuid.UUID {
	t.Helper()
	ctx := context.Background()

	companyID := uuid.New()
	suffix := companyID.String()[:8]
	_, err := db.Querier(ctx).Exec(ctx,
		`INSERT INTO companies (id, normalised_name, display_name)
		 VALUES ($1, $2, $3)`, companyID, "acme"+suffix, "Acme "+suffix)
	if err != nil {
		t.Fatalf("seed company: %v", err)
	}

	postingID := uuid.New()
	_, err = db.Querier(ctx).Exec(ctx,
		`INSERT INTO job_postings (id, fingerprint, content_hash, company_id, title,
		                           normalised_title, description)
		 VALUES ($1, $2, $2, $3, $4, $5, $6)`,
		postingID, []byte(postingID.String()), companyID,
		"Senior Backend Engineer", "senior backend engineer",
		"We are looking for a backend engineer with payments experience.")
	if err != nil {
		t.Fatalf("seed job posting: %v", err)
	}

	t.Cleanup(func() {
		bg := context.Background()
		_, _ = db.Querier(bg).Exec(bg, `DELETE FROM job_postings WHERE id = $1`, postingID)
		_, _ = db.Querier(bg).Exec(bg, `DELETE FROM companies WHERE id = $1`, companyID)
	})
	return postingID
}
