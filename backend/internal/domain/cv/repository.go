package cv

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/Joakim-animate90/myjobfyt/backend/internal/apperr"
	"github.com/Joakim-animate90/myjobfyt/backend/internal/platform/embed"
	"github.com/Joakim-animate90/myjobfyt/backend/internal/platform/postgres"
	"github.com/google/uuid"
)

// Repository is the cv domain's persistence contract.
//
// Every method takes userID, and every query scopes by it in the SQL. Not a convenience:
// it means a handler that forgets an ownership check still cannot read or mutate another
// candidate's CV, which is the one failure mode that would be unrecoverable here.
type Repository interface {
	CreateDocumentWithVersion(ctx context.Context, d *Document, v *Version) error
	AddVersion(ctx context.Context, userID uuid.UUID, v *Version) error
	DocumentByID(ctx context.Context, userID, documentID uuid.UUID) (*Document, error)
	ListDocuments(ctx context.Context, userID uuid.UUID) ([]*Document, error)
	VersionByID(ctx context.Context, userID, versionID uuid.UUID) (*Version, error)
	LatestVersion(ctx context.Context, userID, documentID uuid.UUID) (*Version, error)
	ListVersions(ctx context.Context, userID, documentID uuid.UUID) ([]*Version, error)
	NextVersionNo(ctx context.Context, userID, documentID uuid.UUID) (int, error)

	UpdateAVStatus(ctx context.Context, versionID uuid.UUID, status AVStatus, at time.Time) error
	UpdateParseStatus(ctx context.Context, versionID uuid.UUID, status ParseStatus, stage string) error
	StoreExtractedText(ctx context.Context, versionID uuid.UUID, text ExtractedText) error
	MarkParseFailed(ctx context.Context, versionID uuid.UUID, reason ParseFailureReason, stage string) error
	MarkParsed(ctx context.Context, versionID uuid.UUID, at time.Time) error

	RawText(ctx context.Context, versionID uuid.UUID) (string, error)
	StoreParsedData(ctx context.Context, parsed *ParsedData, skills []Skill) error
	StoreCandidateEdit(ctx context.Context, parsed *ParsedData, changedFields []string) error
	ParsedDataByVersion(ctx context.Context, userID, versionID uuid.UUID) (*ParsedData, error)
	SkillsByVersion(ctx context.Context, userID, versionID uuid.UUID) ([]Skill, error)
	RecordCorrection(ctx context.Context, userID, versionID uuid.UUID, field string) error

	MatchSkillLabels(ctx context.Context, labels []string) ([]NormalisedSkill, error)
	RecordUnmatchedLabels(ctx context.Context, labels []NormalisedSkill) error
	LinkSkillIDs(ctx context.Context, versionID uuid.UUID, resolved []NormalisedSkill) (int, error)
	SkillLabelsForVersion(ctx context.Context, versionID uuid.UUID) ([]string, error)

	StoreEmbeddings(ctx context.Context, embeddings []StoredEmbedding) error
	EmbeddingsForVersion(ctx context.Context, userID, versionID uuid.UUID, model string) ([]StoredEmbedding, error)
	VersionsNeedingEmbedding(ctx context.Context, model string, limit int) ([]uuid.UUID, error)
	VersionsWithoutAnyEmbedding(ctx context.Context, limit int) ([]uuid.UUID, error)
	CandidateEmail(ctx context.Context, userID uuid.UUID) (string, error)
	StoreTailoredVersion(ctx context.Context, v *Version, parsed *ParsedData, skills []Skill) error
	RawTextForVersion(ctx context.Context, userID, versionID uuid.UUID) (string, error)
	EmbeddingInputs(ctx context.Context, versionID uuid.UUID) (EmbeddingInput, error)

	SetLabel(ctx context.Context, userID, documentID uuid.UUID, label string) error
	SetDefault(ctx context.Context, userID, documentID uuid.UUID) error
	SoftDeleteDocument(ctx context.Context, userID, documentID uuid.UUID) error
	VersionIsReferenced(ctx context.Context, documentID uuid.UUID) (bool, error)
}

// PostgresRepository is the Postgres implementation.
type PostgresRepository struct{ db *postgres.DB }

// NewPostgresRepository builds the repository.
func NewPostgresRepository(db *postgres.DB) *PostgresRepository {
	return &PostgresRepository{db: db}
}

const versionColumns = `
	v.id, v.cv_document_id, v.user_id, v.version_no, v.storage_key, v.checksum_sha256,
	v.size_bytes, v.av_status, v.av_scanned_at, v.parse_status,
	coalesce(v.parse_error, ''), coalesce(v.parse_stage, ''), v.parsed_at,
	v.char_count, v.page_count, v.derived_from_version_id,
	v.tailored_for_job_posting_id, v.containment_passed, v.created_at`

type scanner interface{ Scan(dest ...any) error }

func scanVersion(row scanner) (*Version, error) {
	var v Version
	err := row.Scan(
		&v.ID, &v.DocumentID, &v.UserID, &v.VersionNo, &v.StorageKey, &v.ChecksumSHA256,
		&v.SizeBytes, &v.AVStatus, &v.AVScannedAt, &v.ParseStatus,
		&v.ParseError, &v.ParseStage, &v.ParsedAt,
		&v.CharCount, &v.PageCount, &v.DerivedFromVersionID,
		&v.TailoredForJobPostingID, &v.ContainmentPassed, &v.CreatedAt,
	)
	if err != nil {
		return nil, err
	}
	return &v, nil
}

// CreateDocumentWithVersion inserts a document and its first version atomically.
//
// One transaction because a document with no version is unusable and invisible: a crash
// between the two inserts would leave a row the candidate can see but cannot read,
// delete or replace.
func (r *PostgresRepository) CreateDocumentWithVersion(ctx context.Context, d *Document, v *Version) error {
	return r.db.InTx(ctx, func(ctx context.Context) error {
		const insertDoc = `
			INSERT INTO cv_documents (id, user_id, source, label, original_filename,
			                          content_type, size_bytes, is_default)
			VALUES ($1, $2, $3, nullif($4, ''), nullif($5, ''), $6, $7, $8)
			RETURNING created_at, updated_at`

		err := r.db.Querier(ctx).QueryRow(ctx, insertDoc,
			d.ID, d.UserID, d.Source, d.Label, d.OriginalFilename,
			d.ContentType, d.SizeBytes, d.IsDefault,
		).Scan(&d.CreatedAt, &d.UpdatedAt)
		if err != nil {
			if postgres.IsUniqueViolation(err, "cv_documents_one_default_per_user") {
				// Another upload won the race to become the default. Not an error worth
				// failing the upload for; the caller retries without the flag.
				return apperr.Conflict("default_cv_exists", "a default CV already exists")
			}
			return apperr.Internalf(err, "insert cv document")
		}

		v.DocumentID = d.ID
		v.UserID = d.UserID
		v.VersionNo = 1
		return r.insertVersion(ctx, v)
	})
}

// AddVersion appends a version to an existing document, verifying ownership first.
func (r *PostgresRepository) AddVersion(ctx context.Context, userID uuid.UUID, v *Version) error {
	return r.db.InTx(ctx, func(ctx context.Context) error {
		// Ownership is re-checked here, inside the transaction, rather than trusted
		// from the caller: this is the layer that must not be bypassable.
		const owns = `
			SELECT 1 FROM cv_documents
			WHERE id = $1 AND user_id = $2 AND deleted_at IS NULL`
		var one int
		if err := r.db.Querier(ctx).QueryRow(ctx, owns, v.DocumentID, userID).Scan(&one); err != nil {
			if postgres.IsNoRows(err) {
				return apperr.NotFound("CV")
			}
			return apperr.Internalf(err, "verify cv document ownership")
		}

		next, err := r.nextVersionNo(ctx, v.DocumentID)
		if err != nil {
			return err
		}
		v.VersionNo = next
		v.UserID = userID
		return r.insertVersion(ctx, v)
	})
}

func (r *PostgresRepository) insertVersion(ctx context.Context, v *Version) error {
	const q = `
		INSERT INTO cv_versions (id, cv_document_id, user_id, version_no, storage_key,
		                         checksum_sha256, size_bytes, av_status, parse_status,
		                         derived_from_version_id, tailored_for_job_posting_id,
		                         containment_passed)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		RETURNING created_at`

	err := r.db.Querier(ctx).QueryRow(ctx, q,
		v.ID, v.DocumentID, v.UserID, v.VersionNo, v.StorageKey,
		v.ChecksumSHA256, v.SizeBytes, v.AVStatus, v.ParseStatus,
		v.DerivedFromVersionID, v.TailoredForJobPostingID, v.ContainmentPassed,
	).Scan(&v.CreatedAt)
	if err != nil {
		if postgres.IsUniqueViolation(err, "cv_versions_storage_key_uniq") {
			return apperr.Conflict("storage_key_reused", "this document has already been stored")
		}
		if postgres.IsCheckViolation(err) {
			// A check violation here means a tailored version arrived without its
			// containment verdict, which is a bug in the caller, not bad user input.
			return apperr.Internalf(err, "cv version violates a schema invariant (%s)",
				postgres.ConstraintName(err))
		}
		return apperr.Internalf(err, "insert cv version")
	}
	return nil
}

// StoreTailoredVersion persists a tailored CV and its content in one transaction.
//
// Atomic because a tailored version row without its parsed content is a CV the
// candidate can attach to an application and that nothing can validate afterwards. The
// containment verdict is required by the caller rather than defaulted: the schema's
// cv_versions_tailored_requires_containment refuses a tailored version without one, and
// a default here would be a way to satisfy that constraint without having run the check.
func (r *PostgresRepository) StoreTailoredVersion(ctx context.Context, v *Version, parsed *ParsedData, skills []Skill) error {
	if v == nil || parsed == nil {
		return apperr.Internalf(nil, "a tailored version needs both a version row and its content")
	}
	if v.ContainmentPassed == nil || !*v.ContainmentPassed {
		// Belt and braces with the database constraint. Reaching here with an unset or
		// false verdict means a caller bypassed the gate, which is worth failing loudly
		// rather than letting the constraint produce a less specific error.
		return apperr.Internalf(nil,
			"refusing to store a tailored version whose factual containment did not pass")
	}
	if v.DerivedFromVersionID == nil {
		return apperr.Internalf(nil, "a tailored version must record the version it came from")
	}

	return r.db.InTx(ctx, func(ctx context.Context) error {
		next, err := r.nextVersionNo(ctx, v.DocumentID)
		if err != nil {
			return err
		}
		v.VersionNo = next
		if err := r.insertVersion(ctx, v); err != nil {
			return err
		}

		parsed.CVVersionID = v.ID
		parsed.UserID = v.UserID
		// InTx joins the transaction already in this context rather than opening a
		// second one, so the version and its content commit or roll back together.
		return r.StoreParsedData(ctx, parsed, skills)
	})
}

// NextVersionNo returns the next version number for a document the user owns.
func (r *PostgresRepository) NextVersionNo(ctx context.Context, userID, documentID uuid.UUID) (int, error) {
	const owns = `SELECT 1 FROM cv_documents WHERE id = $1 AND user_id = $2 AND deleted_at IS NULL`
	var one int
	if err := r.db.Querier(ctx).QueryRow(ctx, owns, documentID, userID).Scan(&one); err != nil {
		if postgres.IsNoRows(err) {
			return 0, apperr.NotFound("CV")
		}
		return 0, apperr.Internalf(err, "verify cv document ownership")
	}
	return r.nextVersionNo(ctx, documentID)
}

func (r *PostgresRepository) nextVersionNo(ctx context.Context, documentID uuid.UUID) (int, error) {
	const q = `SELECT coalesce(max(version_no), 0) + 1 FROM cv_versions WHERE cv_document_id = $1`
	var next int
	if err := r.db.Querier(ctx).QueryRow(ctx, q, documentID).Scan(&next); err != nil {
		return 0, apperr.Internalf(err, "compute next cv version number")
	}
	return next, nil
}

// DocumentByID loads a document the user owns, with its latest version.
func (r *PostgresRepository) DocumentByID(ctx context.Context, userID, documentID uuid.UUID) (*Document, error) {
	const q = `
		SELECT d.id, d.user_id, d.source, coalesce(d.label, ''),
		       coalesce(d.original_filename, ''), d.content_type, d.size_bytes,
		       d.is_default, d.created_at, d.updated_at, d.deleted_at,
		       (SELECT count(*) FROM cv_versions WHERE cv_document_id = d.id)
		FROM cv_documents d
		WHERE d.id = $1 AND d.user_id = $2 AND d.deleted_at IS NULL`

	var d Document
	err := r.db.Querier(ctx).QueryRow(ctx, q, documentID, userID).Scan(
		&d.ID, &d.UserID, &d.Source, &d.Label, &d.OriginalFilename,
		&d.ContentType, &d.SizeBytes, &d.IsDefault,
		&d.CreatedAt, &d.UpdatedAt, &d.DeletedAt, &d.VersionCount,
	)
	if err != nil {
		if postgres.IsNoRows(err) {
			// Indistinguishable from "belongs to someone else", deliberately.
			return nil, apperr.NotFound("CV")
		}
		return nil, apperr.Internalf(err, "select cv document")
	}

	latest, err := r.LatestVersion(ctx, userID, documentID)
	if err != nil && apperr.KindOf(err) != apperr.KindNotFound {
		return nil, err
	}
	d.LatestVersion = latest
	return &d, nil
}

// ListDocuments returns the candidate's library, newest first, each with its latest
// version.
//
// One query with a lateral join rather than a query per document: a library of ten CVs
// should not be eleven round trips.
func (r *PostgresRepository) ListDocuments(ctx context.Context, userID uuid.UUID) ([]*Document, error) {
	const q = `
		SELECT d.id, d.user_id, d.source, coalesce(d.label, ''),
		       coalesce(d.original_filename, ''), d.content_type, d.size_bytes,
		       d.is_default, d.created_at, d.updated_at, d.deleted_at,
		       (SELECT count(*) FROM cv_versions WHERE cv_document_id = d.id),
		       ` + versionColumns + `
		FROM cv_documents d
		LEFT JOIN LATERAL (
			SELECT * FROM cv_versions
			WHERE cv_document_id = d.id
			ORDER BY version_no DESC
			LIMIT 1
		) v ON true
		WHERE d.user_id = $1 AND d.deleted_at IS NULL
		ORDER BY d.is_default DESC, d.created_at DESC`

	rows, err := r.db.Querier(ctx).Query(ctx, q, userID)
	if err != nil {
		return nil, apperr.Internalf(err, "list cv documents")
	}
	defer rows.Close()

	var out []*Document
	for rows.Next() {
		var (
			d Document
			v Version
			// A document always has a version, but LEFT JOIN LATERAL means the scan
			// targets must tolerate nulls rather than assuming it.
			vID         *uuid.UUID
			vDocID      *uuid.UUID
			vUserID     *uuid.UUID
			vNo         *int
			vKey        *string
			vChecksum   []byte
			vSize       *int64
			vAV         *AVStatus
			vAVAt       *time.Time
			vParse      *ParseStatus
			vParseErr   *string
			vParseStage *string
			vParsedAt   *time.Time
			vCreatedAt  *time.Time
		)

		err := rows.Scan(
			&d.ID, &d.UserID, &d.Source, &d.Label, &d.OriginalFilename,
			&d.ContentType, &d.SizeBytes, &d.IsDefault,
			&d.CreatedAt, &d.UpdatedAt, &d.DeletedAt, &d.VersionCount,
			&vID, &vDocID, &vUserID, &vNo, &vKey, &vChecksum,
			&vSize, &vAV, &vAVAt, &vParse, &vParseErr, &vParseStage, &vParsedAt,
			&v.CharCount, &v.PageCount, &v.DerivedFromVersionID,
			&v.TailoredForJobPostingID, &v.ContainmentPassed, &vCreatedAt,
		)
		if err != nil {
			return nil, apperr.Internalf(err, "scan cv document row")
		}

		if vID != nil {
			v.ID, v.DocumentID, v.UserID = *vID, *vDocID, *vUserID
			v.VersionNo, v.StorageKey, v.ChecksumSHA256 = *vNo, *vKey, vChecksum
			v.SizeBytes, v.AVStatus, v.AVScannedAt = *vSize, *vAV, vAVAt
			v.ParseStatus, v.ParsedAt, v.CreatedAt = *vParse, vParsedAt, *vCreatedAt
			if vParseErr != nil {
				v.ParseError = *vParseErr
			}
			if vParseStage != nil {
				v.ParseStage = *vParseStage
			}
			copyV := v
			d.LatestVersion = &copyV
		}

		out = append(out, &d)
	}
	if err := rows.Err(); err != nil {
		return nil, apperr.Internalf(err, "iterate cv documents")
	}
	return out, nil
}

// VersionByID loads a version the user owns.
func (r *PostgresRepository) VersionByID(ctx context.Context, userID, versionID uuid.UUID) (*Version, error) {
	q := `SELECT ` + versionColumns + ` FROM cv_versions v WHERE v.id = $1 AND v.user_id = $2`

	v, err := scanVersion(r.db.Querier(ctx).QueryRow(ctx, q, versionID, userID))
	if err != nil {
		if postgres.IsNoRows(err) {
			return nil, apperr.NotFound("CV version")
		}
		return nil, apperr.Internalf(err, "select cv version")
	}
	return v, nil
}

// LatestVersion loads the newest version of a document the user owns.
func (r *PostgresRepository) LatestVersion(ctx context.Context, userID, documentID uuid.UUID) (*Version, error) {
	q := `SELECT ` + versionColumns + ` FROM cv_versions v
	      WHERE v.cv_document_id = $1 AND v.user_id = $2
	      ORDER BY v.version_no DESC LIMIT 1`

	v, err := scanVersion(r.db.Querier(ctx).QueryRow(ctx, q, documentID, userID))
	if err != nil {
		if postgres.IsNoRows(err) {
			return nil, apperr.NotFound("CV version")
		}
		return nil, apperr.Internalf(err, "select latest cv version")
	}
	return v, nil
}

// ListVersions returns every version of a document the user owns, newest first.
//
// Version history is user-visible, not just an implementation detail: a candidate who
// uploads a revised CV needs to see which version a given application actually used,
// and that is the whole point of pinning versions rather than overwriting.
func (r *PostgresRepository) ListVersions(ctx context.Context, userID, documentID uuid.UUID) ([]*Version, error) {
	// Ownership is established by the user_id predicate on cv_versions itself rather
	// than by a prior lookup, so there is no window between the check and the read.
	q := `SELECT ` + versionColumns + ` FROM cv_versions v
	      WHERE v.cv_document_id = $1 AND v.user_id = $2
	      ORDER BY v.version_no DESC`

	rows, err := r.db.Querier(ctx).Query(ctx, q, documentID, userID)
	if err != nil {
		return nil, apperr.Internalf(err, "list cv versions")
	}
	defer rows.Close()

	var out []*Version
	for rows.Next() {
		v, serr := scanVersion(rows)
		if serr != nil {
			return nil, apperr.Internalf(serr, "scan cv version row")
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, apperr.Internalf(err, "iterate cv versions")
	}
	return out, nil
}

// UpdateAVStatus records the antivirus verdict.
//
// Not user-scoped: this runs in a worker holding only a version id from an event
// envelope, and the version id is itself unguessable. Every user-facing read path is
// scoped.
func (r *PostgresRepository) UpdateAVStatus(ctx context.Context, versionID uuid.UUID, status AVStatus, at time.Time) error {
	const q = `UPDATE cv_versions SET av_status = $2, av_scanned_at = $3 WHERE id = $1`
	tag, err := r.db.Querier(ctx).Exec(ctx, q, versionID, status, at)
	if err != nil {
		return apperr.Internalf(err, "update av status")
	}
	if tag.RowsAffected() == 0 {
		return apperr.NotFound("CV version")
	}
	return nil
}

// UpdateParseStatus advances the pipeline stage.
func (r *PostgresRepository) UpdateParseStatus(ctx context.Context, versionID uuid.UUID, status ParseStatus, stage string) error {
	const q = `UPDATE cv_versions SET parse_status = $2, parse_stage = nullif($3, '') WHERE id = $1`
	tag, err := r.db.Querier(ctx).Exec(ctx, q, versionID, status, stage)
	if err != nil {
		return apperr.Internalf(err, "update parse status")
	}
	if tag.RowsAffected() == 0 {
		return apperr.NotFound("CV version")
	}
	return nil
}

// StoreExtractedText persists the extracted text and its counts.
func (r *PostgresRepository) StoreExtractedText(ctx context.Context, versionID uuid.UUID, text ExtractedText) error {
	const q = `
		UPDATE cv_versions
		SET raw_text = $2, char_count = $3, page_count = $4, parse_status = 'parsing'
		WHERE id = $1`
	tag, err := r.db.Querier(ctx).Exec(ctx, q, versionID, text.Text, text.CharCount, text.PageCount)
	if err != nil {
		return apperr.Internalf(err, "store extracted text")
	}
	if tag.RowsAffected() == 0 {
		return apperr.NotFound("CV version")
	}
	return nil
}

// MarkParseFailed records a terminal parse failure with its reason.
func (r *PostgresRepository) MarkParseFailed(ctx context.Context, versionID uuid.UUID, reason ParseFailureReason, stage string) error {
	const q = `
		UPDATE cv_versions
		SET parse_status = 'failed', parse_error = $2, parse_stage = nullif($3, '')
		WHERE id = $1`
	tag, err := r.db.Querier(ctx).Exec(ctx, q, versionID, string(reason), stage)
	if err != nil {
		return apperr.Internalf(err, "mark parse failed")
	}
	if tag.RowsAffected() == 0 {
		return apperr.NotFound("CV version")
	}
	return nil
}

// MarkParsed records successful parsing. Idempotent on parsed_at so a redelivery keeps
// the first completion time.
func (r *PostgresRepository) MarkParsed(ctx context.Context, versionID uuid.UUID, at time.Time) error {
	const q = `
		UPDATE cv_versions
		SET parse_status = 'parsed', parsed_at = coalesce(parsed_at, $2),
		    parse_error = NULL, parse_stage = NULL
		WHERE id = $1`
	tag, err := r.db.Querier(ctx).Exec(ctx, q, versionID, at)
	if err != nil {
		return apperr.Internalf(err, "mark parsed")
	}
	if tag.RowsAffected() == 0 {
		return apperr.NotFound("CV version")
	}
	return nil
}

// SetLabel renames a document.
func (r *PostgresRepository) SetLabel(ctx context.Context, userID, documentID uuid.UUID, label string) error {
	const q = `
		UPDATE cv_documents SET label = nullif($3, '')
		WHERE id = $1 AND user_id = $2 AND deleted_at IS NULL`
	tag, err := r.db.Querier(ctx).Exec(ctx, q, documentID, userID, label)
	if err != nil {
		return apperr.Internalf(err, "set cv label")
	}
	if tag.RowsAffected() == 0 {
		return apperr.NotFound("CV")
	}
	return nil
}

// SetDefault makes one document the default, clearing any previous default.
//
// Both statements in one transaction because the partial unique index permits exactly
// one default per candidate: clearing must commit with setting, or the second statement
// collides with the first's own row.
func (r *PostgresRepository) SetDefault(ctx context.Context, userID, documentID uuid.UUID) error {
	return r.db.InTx(ctx, func(ctx context.Context) error {
		const clear = `
			UPDATE cv_documents SET is_default = false
			WHERE user_id = $1 AND is_default AND deleted_at IS NULL`
		if _, err := r.db.Querier(ctx).Exec(ctx, clear, userID); err != nil {
			return apperr.Internalf(err, "clear previous default cv")
		}

		const set = `
			UPDATE cv_documents SET is_default = true
			WHERE id = $1 AND user_id = $2 AND deleted_at IS NULL`
		tag, err := r.db.Querier(ctx).Exec(ctx, set, documentID, userID)
		if err != nil {
			return apperr.Internalf(err, "set default cv")
		}
		if tag.RowsAffected() == 0 {
			return apperr.NotFound("CV")
		}
		return nil
	})
}

// SoftDeleteDocument marks a document deleted and clears its default flag.
//
// The default flag must be cleared, or the partial unique index would still consider the
// deleted row the candidate's default and block setting a new one.
func (r *PostgresRepository) SoftDeleteDocument(ctx context.Context, userID, documentID uuid.UUID) error {
	const q = `
		UPDATE cv_documents SET deleted_at = now(), is_default = false
		WHERE id = $1 AND user_id = $2 AND deleted_at IS NULL`
	tag, err := r.db.Querier(ctx).Exec(ctx, q, documentID, userID)
	if err != nil {
		return apperr.Internalf(err, "soft delete cv document")
	}
	if tag.RowsAffected() == 0 {
		return apperr.NotFound("CV")
	}
	return nil
}

// VersionIsReferenced reports whether any version of a document is referenced by an
// application or an active loop.
//
// Checked before deletion because the schema's RESTRICT would otherwise surface as a
// foreign-key error the candidate cannot interpret. Answering the question first lets
// the service explain exactly why the CV cannot be removed.
func (r *PostgresRepository) VersionIsReferenced(ctx context.Context, documentID uuid.UUID) (bool, error) {
	const q = `
		SELECT EXISTS (
			SELECT 1 FROM applications a
			JOIN cv_versions v ON v.id = a.cv_version_id
			WHERE v.cv_document_id = $1
			UNION ALL
			SELECT 1 FROM loops l
			JOIN cv_versions v ON v.id = l.cv_version_id
			WHERE v.cv_document_id = $1 AND l.status <> 'archived'
		)`

	var referenced bool
	if err := r.db.Querier(ctx).QueryRow(ctx, q, documentID).Scan(&referenced); err != nil {
		return false, apperr.Internalf(err, "check cv references")
	}
	return referenced, nil
}

// --- parsed data ------------------------------------------------------------

// StoreCandidateEdit persists a candidate's own edit of their parsed CV.
//
// Distinct from StoreParsedData, and the distinction is the whole point. A machine
// re-parse must PRESERVE corrections; a candidate edit must APPLY them. Running an edit
// through the preserving path would read the stored value, decide it was corrected, and
// write it back — silently discarding the change the candidate just made.
//
// changedFields are added to the correction list, so every later re-parse respects them.
func (r *PostgresRepository) StoreCandidateEdit(ctx context.Context, parsed *ParsedData, changedFields []string) error {
	return r.db.InTx(ctx, func(ctx context.Context) error {
		existing, err := r.parsedDataForUpdate(ctx, parsed.CVVersionID)
		if err != nil {
			return err
		}
		if existing == nil {
			return apperr.NotFound("parsed CV data")
		}

		// Values are taken as given — no merge. The correction list is the union of
		// what was already corrected and what this edit touched.
		parsed.CorrectedFields = unionFields(existing.CorrectedFields, changedFields)

		return r.writeParsedData(ctx, parsed)
	})
}

// unionFields merges two field lists, preserving order and dropping duplicates and
// blanks.
func unionFields(existing, added []string) []string {
	seen := make(map[string]struct{}, len(existing)+len(added))
	out := make([]string, 0, len(existing)+len(added))

	for _, list := range [][]string{existing, added} {
		for _, f := range list {
			if f == "" {
				continue
			}
			if _, dup := seen[f]; dup {
				continue
			}
			seen[f] = struct{}{}
			out = append(out, f)
		}
	}
	return out
}

// StoreParsedData persists an extraction, preserving any candidate corrections.
//
// The read and the write happen in one transaction: without it, a candidate correcting
// a field while a re-parse is in flight could have their edit read as absent and then
// overwritten. SERIALIZABLE is not needed — the version id is the only key and
// concurrent writes to the same version are already serialised by the row lock the
// SELECT FOR UPDATE takes.
func (r *PostgresRepository) StoreParsedData(ctx context.Context, parsed *ParsedData, skills []Skill) error {
	return r.db.InTx(ctx, func(ctx context.Context) error {
		existing, err := r.parsedDataForUpdate(ctx, parsed.CVVersionID)
		if err != nil {
			return err
		}

		merged := MergePreservingCorrections(parsed, existing)

		if err := r.writeParsedData(ctx, merged); err != nil {
			return err
		}

		// Skills live in their own table and are replaced wholesale, unless the
		// candidate has edited them — in which case the stored rows are theirs.
		if SkillsAreCorrected(existing) {
			return nil
		}
		return r.replaceSkills(ctx, merged.CVVersionID, merged.UserID, skills)
	})
}

// writeParsedData upserts a parsed-data row. Shared by the re-parse and candidate-edit
// paths so there is exactly one copy of the column list.
func (r *PostgresRepository) writeParsedData(ctx context.Context, merged *ParsedData) error {
	{
		contact, err := merged.MarshalContact()
		if err != nil {
			return apperr.Internalf(err, "marshal contact")
		}
		experience, err := merged.MarshalExperience()
		if err != nil {
			return apperr.Internalf(err, "marshal experience")
		}
		education, err := merged.MarshalEducation()
		if err != nil {
			return apperr.Internalf(err, "marshal education")
		}
		certifications, err := merged.MarshalCertifications()
		if err != nil {
			return apperr.Internalf(err, "marshal certifications")
		}
		languages, err := merged.MarshalLanguages()
		if err != nil {
			return apperr.Internalf(err, "marshal languages")
		}
		provenance, err := merged.MarshalProvenance()
		if err != nil {
			return apperr.Internalf(err, "marshal provenance")
		}

		const upsert = `
			INSERT INTO cv_parsed_data (
				cv_version_id, user_id, summary, contact, experience, education,
				certifications, languages, confidence, corrected_fields,
				total_experience_months, detected_seniority, engine_version
			) VALUES ($1, $2, nullif($3, ''), $4, $5, $6, $7, $8, $9, $10, $11, nullif($12, ''), $13)
			ON CONFLICT (cv_version_id) DO UPDATE SET
				summary                 = EXCLUDED.summary,
				contact                 = EXCLUDED.contact,
				experience              = EXCLUDED.experience,
				education               = EXCLUDED.education,
				certifications          = EXCLUDED.certifications,
				languages               = EXCLUDED.languages,
				confidence              = EXCLUDED.confidence,
				corrected_fields        = EXCLUDED.corrected_fields,
				total_experience_months = EXCLUDED.total_experience_months,
				detected_seniority      = EXCLUDED.detected_seniority,
				engine_version          = EXCLUDED.engine_version
			RETURNING created_at, updated_at`

		err = r.db.Querier(ctx).QueryRow(ctx, upsert,
			merged.CVVersionID, merged.UserID, merged.Summary, contact, experience,
			education, certifications, languages, provenance, merged.CorrectedFields,
			merged.TotalExperienceMonths, merged.DetectedSeniority, merged.EngineVersion,
		).Scan(&merged.CreatedAt, &merged.UpdatedAt)
		if err != nil {
			if postgres.IsForeignKeyViolation(err) {
				// The version was deleted while this extraction was in flight.
				return apperr.NotFound("CV version")
			}
			return apperr.Internalf(err, "upsert parsed cv data")
		}
		return nil
	}
}

// parsedDataForUpdate loads the existing record and locks the row, returning nil when
// there is none.
//
// Only the fields the merge needs are selected. Loading the provenance report as well
// would pull a large jsonb blob that is about to be replaced.
func (r *PostgresRepository) parsedDataForUpdate(ctx context.Context, versionID uuid.UUID) (*ParsedData, error) {
	const q = `
		SELECT coalesce(summary, ''), contact, experience, education, certifications,
		       languages, corrected_fields, total_experience_months,
		       coalesce(detected_seniority, '')
		FROM cv_parsed_data
		WHERE cv_version_id = $1
		FOR UPDATE`

	var (
		existing                                                  ParsedData
		contact, experience, education, certifications, languages []byte
	)
	err := r.db.Querier(ctx).QueryRow(ctx, q, versionID).Scan(
		&existing.Summary, &contact, &experience, &education, &certifications,
		&languages, &existing.CorrectedFields, &existing.TotalExperienceMonths,
		&existing.DetectedSeniority,
	)
	if err != nil {
		if postgres.IsNoRows(err) {
			return nil, nil
		}
		return nil, apperr.Internalf(err, "select existing parsed cv data")
	}

	// Only unmarshal what a correction could preserve. A malformed stored blob is
	// treated as absent rather than failing the write: the alternative is a CV that can
	// never be re-parsed because of a bad row written by an older version of this code.
	_ = json.Unmarshal(contact, &existing.Contact)
	_ = json.Unmarshal(experience, &existing.Experience)
	_ = json.Unmarshal(education, &existing.Education)
	_ = json.Unmarshal(certifications, &existing.Certifications)
	_ = json.Unmarshal(languages, &existing.Languages)

	existing.CVVersionID = versionID
	return &existing, nil
}

// replaceSkills swaps a version's skill rows for a new set.
//
// Delete-then-insert rather than a diff: the set is small, it runs inside the caller's
// transaction, and a diff would have to decide what "the same skill" means across
// re-parses — which is exactly the ambiguity that makes list-item correction merging
// unsafe.
func (r *PostgresRepository) replaceSkills(ctx context.Context, versionID, userID uuid.UUID, skills []Skill) error {
	const clear = `DELETE FROM cv_skills WHERE cv_version_id = $1`
	if _, err := r.db.Querier(ctx).Exec(ctx, clear, versionID); err != nil {
		return apperr.Internalf(err, "clear cv skills")
	}
	if len(skills) == 0 {
		return nil
	}

	// ON CONFLICT DO NOTHING rather than catching the unique violation in Go.
	//
	// Inside a transaction a failed statement aborts the whole transaction: Postgres
	// rejects every subsequent statement and the commit fails with "commit
	// unexpectedly resulted in rollback". Catching the error and continuing looks like
	// it works and does not — the conflict has to be handled by the statement itself.
	const insert = `
		INSERT INTO cv_skills (cv_version_id, user_id, skill_id, raw_label, years, confidence, source)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (cv_version_id, raw_label) DO NOTHING`

	for _, skill := range skills {
		source := skill.Source
		if source == "" {
			source = SkillSourceAI
		}
		_, err := r.db.Querier(ctx).Exec(ctx, insert,
			versionID, userID, skill.SkillID, skill.RawLabel,
			skill.Years, skill.Confidence, string(source))
		if err != nil {
			return apperr.Internalf(err, "insert cv skill %q", skill.RawLabel)
		}
	}
	return nil
}

// ParsedDataByVersion loads a version's parsed data, scoped to its owner.
func (r *PostgresRepository) ParsedDataByVersion(ctx context.Context, userID, versionID uuid.UUID) (*ParsedData, error) {
	const q = `
		SELECT cv_version_id, user_id, coalesce(summary, ''), contact, experience,
		       education, certifications, languages, confidence, corrected_fields,
		       total_experience_months, coalesce(detected_seniority, ''),
		       coalesce(engine_version, ''), created_at, updated_at
		FROM cv_parsed_data
		WHERE cv_version_id = $1 AND user_id = $2`

	var (
		parsed                                                                ParsedData
		contact, experience, education, certifications, languages, provenance []byte
	)
	err := r.db.Querier(ctx).QueryRow(ctx, q, versionID, userID).Scan(
		&parsed.CVVersionID, &parsed.UserID, &parsed.Summary, &contact, &experience,
		&education, &certifications, &languages, &provenance, &parsed.CorrectedFields,
		&parsed.TotalExperienceMonths, &parsed.DetectedSeniority,
		&parsed.EngineVersion, &parsed.CreatedAt, &parsed.UpdatedAt,
	)
	if err != nil {
		if postgres.IsNoRows(err) {
			return nil, apperr.NotFound("parsed CV data")
		}
		return nil, apperr.Internalf(err, "select parsed cv data")
	}

	if err := json.Unmarshal(contact, &parsed.Contact); err != nil {
		return nil, apperr.Internalf(err, "parse stored contact")
	}
	if err := json.Unmarshal(experience, &parsed.Experience); err != nil {
		return nil, apperr.Internalf(err, "parse stored experience")
	}
	if err := json.Unmarshal(education, &parsed.Education); err != nil {
		return nil, apperr.Internalf(err, "parse stored education")
	}
	if err := json.Unmarshal(certifications, &parsed.Certifications); err != nil {
		return nil, apperr.Internalf(err, "parse stored certifications")
	}
	if err := json.Unmarshal(languages, &parsed.Languages); err != nil {
		return nil, apperr.Internalf(err, "parse stored languages")
	}
	// Provenance is advisory for the UI, so a malformed blob degrades the review screen
	// rather than failing the read of otherwise-good CV data.
	_ = json.Unmarshal(provenance, &parsed.Provenance)

	return &parsed, nil
}

// SkillsByVersion loads a version's skills, scoped to its owner.
func (r *PostgresRepository) SkillsByVersion(ctx context.Context, userID, versionID uuid.UUID) ([]Skill, error) {
	const q = `
		SELECT skill_id, raw_label, years, confidence, source
		FROM cv_skills
		WHERE cv_version_id = $1 AND user_id = $2
		ORDER BY confidence DESC NULLS LAST, raw_label`

	rows, err := r.db.Querier(ctx).Query(ctx, q, versionID, userID)
	if err != nil {
		return nil, apperr.Internalf(err, "list cv skills")
	}
	defer rows.Close()

	var out []Skill
	for rows.Next() {
		var skill Skill
		if err := rows.Scan(&skill.SkillID, &skill.RawLabel, &skill.Years,
			&skill.Confidence, &skill.Source); err != nil {
			return nil, apperr.Internalf(err, "scan cv skill")
		}
		out = append(out, skill)
	}
	if err := rows.Err(); err != nil {
		return nil, apperr.Internalf(err, "iterate cv skills")
	}
	return out, nil
}

// RecordCorrection marks a field as candidate-owned.
//
// Appends rather than replaces, and is idempotent, so a candidate editing the same
// field twice does not accumulate duplicates.
func (r *PostgresRepository) RecordCorrection(ctx context.Context, userID, versionID uuid.UUID, field string) error {
	const q = `
		UPDATE cv_parsed_data
		SET corrected_fields = (
			SELECT array_agg(DISTINCT f)
			FROM unnest(corrected_fields || ARRAY[$3]::text[]) AS f
		)
		WHERE cv_version_id = $1 AND user_id = $2`

	tag, err := r.db.Querier(ctx).Exec(ctx, q, versionID, userID, field)
	if err != nil {
		return apperr.Internalf(err, "record cv correction")
	}
	if tag.RowsAffected() == 0 {
		return apperr.NotFound("parsed CV data")
	}
	return nil
}

// RawTextForVersion loads a version's extracted text for a candidate-facing read.
//
// Separate from RawText, which is deliberately unscoped for the worker. Every path a
// candidate can reach is scoped by user_id in the query itself rather than relying on
// an ownership check made earlier by a caller, because that check is one refactor away
// from being dropped.
func (r *PostgresRepository) RawTextForVersion(ctx context.Context, userID, versionID uuid.UUID) (string, error) {
	const q = `SELECT coalesce(raw_text, '') FROM cv_versions WHERE id = $1 AND user_id = $2`

	var text string
	if err := r.db.Querier(ctx).QueryRow(ctx, q, versionID, userID).Scan(&text); err != nil {
		if postgres.IsNoRows(err) {
			return "", apperr.NotFound("CV version")
		}
		return "", apperr.Internalf(err, "select cv raw text")
	}
	return text, nil
}

// RawText loads a version's extracted text for the structure stage.
//
// Not user-scoped: this runs in a worker holding only a version id from an event
// envelope, and that id is an unguessable UUID. Every candidate-facing read path is
// scoped.
func (r *PostgresRepository) RawText(ctx context.Context, versionID uuid.UUID) (string, error) {
	const q = `SELECT coalesce(raw_text, '') FROM cv_versions WHERE id = $1`

	var text string
	if err := r.db.Querier(ctx).QueryRow(ctx, q, versionID).Scan(&text); err != nil {
		if postgres.IsNoRows(err) {
			return "", apperr.NotFound("CV version")
		}
		return "", apperr.Internalf(err, "select cv raw text")
	}
	return text, nil
}

// --- skill taxonomy ---------------------------------------------------------

// MatchSkillLabels resolves raw labels against the canonical taxonomy.
//
// One query for the whole batch rather than one per label: a CV with 40 skills should
// not be 40 round trips. The lookup is a single array containment check against the
// gin index on aliases, which holds the canonical name as well as the synonyms — so
// there is no separate canonical-versus-alias query.
//
// Labels that match nothing come back with a nil Skill. They are not dropped: the
// candidate wrote them and may recognise them even when the taxonomy does not.
func (r *PostgresRepository) MatchSkillLabels(ctx context.Context, labels []string) ([]NormalisedSkill, error) {
	out := make([]NormalisedSkill, 0, len(labels))
	if len(labels) == 0 {
		return out, nil
	}

	normalised := make([]string, 0, len(labels))
	for _, label := range labels {
		normalised = append(normalised, NormaliseLabel(label))
	}

	const q = `
		SELECT n.normalised, s.id, s.canonical_name, coalesce(s.category, ''),
		       (s.canonical_name IS NOT NULL
		        AND normalise_skill_label(s.canonical_name) = n.normalised) AS is_canonical
		FROM unnest($1::text[]) AS n(normalised)
		LEFT JOIN skills s ON n.normalised = ANY(s.aliases)`

	rows, err := r.db.Querier(ctx).Query(ctx, q, normalised)
	if err != nil {
		return nil, apperr.Internalf(err, "match skill labels")
	}
	defer rows.Close()

	// Keyed by normalised form so the result can be re-associated with the original
	// spellings, which is what gets stored.
	matches := make(map[string]*TaxonomySkill, len(labels))
	canonical := make(map[string]bool, len(labels))

	for rows.Next() {
		var (
			key         string
			id          *string
			name        *string
			category    string
			isCanonical *bool
		)
		if err := rows.Scan(&key, &id, &name, &category, &isCanonical); err != nil {
			return nil, apperr.Internalf(err, "scan skill match")
		}
		if id != nil && name != nil {
			matches[key] = &TaxonomySkill{ID: *id, CanonicalName: *name, Category: category}
			canonical[key] = isCanonical != nil && *isCanonical
		}
	}
	if err := rows.Err(); err != nil {
		return nil, apperr.Internalf(err, "iterate skill matches")
	}

	for i, label := range labels {
		key := normalised[i]
		resolved := NormalisedSkill{
			RawLabel:   label,
			Normalised: key,
			Outcome:    Unmatched,
		}
		if skill, found := matches[key]; found {
			resolved.Skill = skill
			if canonical[key] {
				resolved.Outcome = MatchedCanonical
			} else {
				resolved.Outcome = MatchedAlias
			}
		}
		out = append(out, resolved)
	}
	return out, nil
}

// RecordUnmatchedLabels adds labels the taxonomy does not know to the triage list.
//
// This is the feedback loop that replaces fuzzy matching. Without it the taxonomy would
// only grow by guesswork; with it, an operator can see that 400 candidates wrote a
// label and add the alias, instead of inventing a similarity threshold.
//
// Best-effort by design: a failure here must not fail a CV extraction. Losing one
// triage row costs a little curation signal; failing the extraction costs the candidate
// their profile.
func (r *PostgresRepository) RecordUnmatchedLabels(ctx context.Context, labels []NormalisedSkill) error {
	pending := make([]NormalisedSkill, 0, len(labels))
	for _, label := range labels {
		if label.Outcome == Unmatched && label.Normalised != "" {
			pending = append(pending, label)
		}
	}
	if len(pending) == 0 {
		return nil
	}

	const q = `
		INSERT INTO unmatched_skill_labels (normalised, sample_label)
		VALUES ($1, $2)
		ON CONFLICT (normalised) DO UPDATE
		SET occurrences  = unmatched_skill_labels.occurrences + 1,
		    last_seen_at = now()`

	for _, label := range pending {
		if _, err := r.db.Querier(ctx).Exec(ctx, q, label.Normalised, label.RawLabel); err != nil {
			return apperr.Internalf(err, "record unmatched skill label")
		}
	}
	return nil
}

// LinkSkillIDs attaches resolved canonical skills to a version's stored skill rows.
//
// Run after matching rather than during the initial insert, because extraction and
// normalisation are separate pipeline stages: the skills are persisted as soon as they
// are extracted, so the CV is usable immediately, and the canonical links are filled in
// when normalisation runs.
func (r *PostgresRepository) LinkSkillIDs(ctx context.Context, versionID uuid.UUID, resolved []NormalisedSkill) (int, error) {
	const q = `
		UPDATE cv_skills
		SET skill_id = $3
		WHERE cv_version_id = $1 AND raw_label = $2
		  -- Never overwrite a link the candidate set. Their correction is authoritative.
		  AND source <> 'candidate'`

	linked := 0
	for _, skill := range resolved {
		if skill.Skill == nil {
			continue
		}
		tag, err := r.db.Querier(ctx).Exec(ctx, q, versionID, skill.RawLabel, skill.Skill.ID)
		if err != nil {
			return linked, apperr.Internalf(err, "link skill %q", skill.RawLabel)
		}
		linked += int(tag.RowsAffected())
	}
	return linked, nil
}

// SkillLabelsForVersion returns a version's raw skill labels, for normalisation.
func (r *PostgresRepository) SkillLabelsForVersion(ctx context.Context, versionID uuid.UUID) ([]string, error) {
	const q = `
		SELECT raw_label FROM cv_skills
		WHERE cv_version_id = $1 AND source <> 'candidate'
		ORDER BY raw_label`

	rows, err := r.db.Querier(ctx).Query(ctx, q, versionID)
	if err != nil {
		return nil, apperr.Internalf(err, "list skill labels")
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var label string
		if err := rows.Scan(&label); err != nil {
			return nil, apperr.Internalf(err, "scan skill label")
		}
		out = append(out, label)
	}
	if err := rows.Err(); err != nil {
		return nil, apperr.Internalf(err, "iterate skill labels")
	}
	return out, nil
}

// --- embeddings -------------------------------------------------------------

// StoredEmbedding is a persisted vector.
type StoredEmbedding struct {
	CVVersionID uuid.UUID
	Kind        embed.Kind
	Model       string
	Vector      embed.Vector
}

// StoreEmbeddings replaces a version's vectors for the models given.
//
// Keyed on (version, kind, model), so re-embedding with the same model overwrites and
// re-embedding with a NEW model adds alongside. The old vectors are deliberately kept:
// a comparison against a posting embedded by the old model still works until that
// posting is re-embedded too, and deleting them first would leave a window where
// nothing could be compared at all.
func (r *PostgresRepository) StoreEmbeddings(ctx context.Context, embeddings []StoredEmbedding) error {
	if len(embeddings) == 0 {
		return nil
	}

	const q = `
		INSERT INTO cv_embeddings (cv_version_id, kind, model, dims, vector)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (cv_version_id, kind, model) DO UPDATE
		SET vector     = EXCLUDED.vector,
		    dims       = EXCLUDED.dims,
		    created_at = now()`

	return r.db.InTx(ctx, func(ctx context.Context) error {
		for _, e := range embeddings {
			if !e.Kind.Valid() {
				// The database CHECK would reject it; failing here names the kind.
				return apperr.Internalf(
					fmt.Errorf("embedding kind %q is not one of title, skills, summary, full", e.Kind),
					"store embeddings")
			}
			if err := e.Vector.Validate(); err != nil {
				return apperr.Internalf(err, "embedding for kind %q is unusable", e.Kind)
			}

			_, err := r.db.Querier(ctx).Exec(ctx, q,
				e.CVVersionID, string(e.Kind), e.Model, e.Vector.Dims(), []float32(e.Vector))
			if err != nil {
				if postgres.IsForeignKeyViolation(err) {
					// The version was deleted while embedding was in flight.
					return apperr.NotFound("CV version")
				}
				return apperr.Internalf(err, "insert embedding for kind %q", e.Kind)
			}
		}
		return nil
	})
}

// EmbeddingsForVersion loads a version's vectors for a given model.
//
// Scoped by model rather than returning everything, because vectors from two models
// are not comparable and a caller that mixed them would produce similarity scores that
// look valid and mean nothing.
func (r *PostgresRepository) EmbeddingsForVersion(ctx context.Context, userID, versionID uuid.UUID, model string) ([]StoredEmbedding, error) {
	const q = `
		SELECT e.kind, e.model, e.vector
		FROM cv_embeddings e
		JOIN cv_versions v ON v.id = e.cv_version_id
		WHERE e.cv_version_id = $1 AND v.user_id = $2 AND e.model = $3
		ORDER BY e.kind`

	rows, err := r.db.Querier(ctx).Query(ctx, q, versionID, userID, model)
	if err != nil {
		return nil, apperr.Internalf(err, "list embeddings")
	}
	defer rows.Close()

	var out []StoredEmbedding
	for rows.Next() {
		var (
			kind   string
			stored string
			vector []float32
		)
		if err := rows.Scan(&kind, &stored, &vector); err != nil {
			return nil, apperr.Internalf(err, "scan embedding")
		}
		out = append(out, StoredEmbedding{
			CVVersionID: versionID,
			Kind:        embed.Kind(kind),
			Model:       stored,
			Vector:      embed.Vector(vector),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, apperr.Internalf(err, "iterate embeddings")
	}
	return out, nil
}

// VersionsNeedingEmbedding returns parsed versions with no vectors for the given model.
//
// This is how matching recovers from an embedding outage. While the provider is down,
// CVs still parse and still match — lexically, marked degraded. When it returns, this
// query finds everything that was missed and the backfill re-embeds it, after which the
// degraded matches are rescored. Nothing is lost, it is only temporarily less precise.
//
// Ordered oldest first so a backlog drains in the order it accumulated, and limited so
// a backfill cannot monopolise the worker.
func (r *PostgresRepository) VersionsNeedingEmbedding(ctx context.Context, model string, limit int) ([]uuid.UUID, error) {
	if limit <= 0 {
		limit = 100
	}

	const q = `
		SELECT v.id
		FROM cv_versions v
		WHERE v.parse_status = 'parsed'
		  AND v.av_status = 'clean'
		  -- Must have structured data to embed from. A version parsed on extracted text
		  -- alone has none, which is the normal state wherever the AI extractor is
		  -- disabled. Without this clause every such version would sit in the recovery
		  -- queue permanently and be re-examined by every sweep, for ever, because
		  -- there is nothing the backfill could ever do about it.
		  AND EXISTS (
		    SELECT 1 FROM cv_parsed_data p WHERE p.cv_version_id = v.id
		  )
		  AND NOT EXISTS (
		    SELECT 1 FROM cv_embeddings e
		    WHERE e.cv_version_id = v.id AND e.model = $1
		  )
		ORDER BY v.created_at
		LIMIT $2`

	rows, err := r.db.Querier(ctx).Query(ctx, q, model, limit)
	if err != nil {
		return nil, apperr.Internalf(err, "list versions needing embedding")
	}
	defer rows.Close()

	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, apperr.Internalf(err, "scan version id")
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, apperr.Internalf(err, "iterate versions needing embedding")
	}
	return out, nil
}

// VersionsWithoutAnyEmbedding returns parsed versions that have no vectors at all,
// under any model.
//
// The backfill's cold-start query. VersionsNeedingEmbedding needs a model name, but the
// model is reported by the provider rather than configured, so after a restart the
// worker does not yet know it — and the versions that matter most are precisely the
// ones an outage left with nothing. Once a successful embedding has revealed the
// current model, the backfill switches to the model-scoped query, which also catches a
// model upgrade.
func (r *PostgresRepository) VersionsWithoutAnyEmbedding(ctx context.Context, limit int) ([]uuid.UUID, error) {
	if limit <= 0 {
		limit = 100
	}

	const q = `
		SELECT v.id
		FROM cv_versions v
		WHERE v.parse_status = 'parsed'
		  AND v.av_status = 'clean'
		  -- See VersionsNeedingEmbedding: a version with no structured data can never
		  -- be embedded, so it must not be queued for recovery.
		  AND EXISTS (
		    SELECT 1 FROM cv_parsed_data p WHERE p.cv_version_id = v.id
		  )
		  AND NOT EXISTS (
		    SELECT 1 FROM cv_embeddings e WHERE e.cv_version_id = v.id
		  )
		ORDER BY v.created_at
		LIMIT $1`

	rows, err := r.db.Querier(ctx).Query(ctx, q, limit)
	if err != nil {
		return nil, apperr.Internalf(err, "list versions without embeddings")
	}
	defer rows.Close()

	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, apperr.Internalf(err, "scan version id")
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, apperr.Internalf(err, "iterate versions without embeddings")
	}
	return out, nil
}

// CandidateEmail returns the address to notify a candidate at.
//
// Scoped to a verified, active account: a parse-failure notice sent to an unverified
// address would be mail to someone who may not own it, and this domain has no business
// reaching a deleted account.
func (r *PostgresRepository) CandidateEmail(ctx context.Context, userID uuid.UUID) (string, error) {
	const q = `
		SELECT email
		FROM users
		WHERE id = $1
		  AND email_verified_at IS NOT NULL
		  AND deleted_at IS NULL`

	var email string
	if err := r.db.Querier(ctx).QueryRow(ctx, q, userID).Scan(&email); err != nil {
		if postgres.IsNoRows(err) {
			return "", apperr.NotFound("candidate")
		}
		return "", apperr.Internalf(err, "look up candidate email")
	}
	return email, nil
}

// EmbeddingInputs gathers the text a version's vectors are built from.
//
// Returns the focused extracts rather than the whole CV: an embedding model truncates
// at a few hundred tokens, so embedding a full CV would capture only its opening and
// the title vector would end up describing the candidate's address.
func (r *PostgresRepository) EmbeddingInputs(ctx context.Context, versionID uuid.UUID) (EmbeddingInput, error) {
	const q = `
		SELECT coalesce(p.summary, ''),
		       coalesce(p.contact->>'headline', ''),
		       coalesce((p.experience->0->>'title'), ''),
		       coalesce((
		         SELECT string_agg(coalesce(s.canonical_name, cs.raw_label), ', '
		                           ORDER BY coalesce(s.canonical_name, cs.raw_label))
		         FROM cv_skills cs
		         LEFT JOIN skills s ON s.id = cs.skill_id
		         WHERE cs.cv_version_id = p.cv_version_id
		       ), '')
		FROM cv_parsed_data p
		WHERE p.cv_version_id = $1`

	var in EmbeddingInput
	err := r.db.Querier(ctx).QueryRow(ctx, q, versionID).Scan(
		&in.Summary, &in.Headline, &in.MostRecentTitle, &in.Skills,
	)
	if err != nil {
		if postgres.IsNoRows(err) {
			return EmbeddingInput{}, apperr.NotFound("parsed CV data")
		}
		return EmbeddingInput{}, apperr.Internalf(err, "gather embedding inputs")
	}
	in.CVVersionID = versionID
	return in, nil
}
