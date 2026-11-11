package cv

import (
	"context"
	"time"

	"github.com/Joakim-animate90/myjobfyt/backend/internal/apperr"
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
	NextVersionNo(ctx context.Context, userID, documentID uuid.UUID) (int, error)

	UpdateAVStatus(ctx context.Context, versionID uuid.UUID, status AVStatus, at time.Time) error
	UpdateParseStatus(ctx context.Context, versionID uuid.UUID, status ParseStatus, stage string) error
	StoreExtractedText(ctx context.Context, versionID uuid.UUID, text ExtractedText) error
	MarkParseFailed(ctx context.Context, versionID uuid.UUID, reason ParseFailureReason, stage string) error
	MarkParsed(ctx context.Context, versionID uuid.UUID, at time.Time) error

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
