package cv

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/Joakim-animate90/myjobfyt/backend/internal/apperr"
	"github.com/Joakim-animate90/myjobfyt/backend/internal/domain/entitlements"
	"github.com/Joakim-animate90/myjobfyt/backend/internal/platform/events"
	"github.com/Joakim-animate90/myjobfyt/backend/internal/platform/storage"
	"github.com/google/uuid"
)

// ObjectStore is the storage surface this domain needs.
type ObjectStore interface {
	Put(ctx context.Context, tenantID uuid.UUID, declared string, r io.Reader) (storage.Object, error)
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	PresignedGet(ctx context.Context, key, downloadName string) (string, error)
	Delete(ctx context.Context, key string) error
}

// Entitlements is the quota surface this domain needs.
type Entitlements interface {
	Require(ctx context.Context, userID uuid.UUID, q entitlements.Quota, n int64) error
	Check(ctx context.Context, userID uuid.UUID, q entitlements.Quota) (entitlements.Allowance, error)
}

// Service holds the cv domain's business logic.
type Service struct {
	repo  Repository
	store ObjectStore
	ents  Entitlements
	bus   events.Publisher
	log   *slog.Logger
	now   func() time.Time
}

// ServiceDeps are the service's collaborators.
type ServiceDeps struct {
	Repo         Repository
	Store        ObjectStore
	Entitlements Entitlements
	Bus          events.Publisher
	Logger       *slog.Logger
	Now          func() time.Time
}

// NewService builds the cv service.
func NewService(d ServiceDeps) *Service {
	now := d.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &Service{
		repo:  d.Repo,
		store: d.Store,
		ents:  d.Entitlements,
		bus:   d.Bus,
		log:   d.Logger,
		now:   now,
	}
}

// UploadInput describes a CV upload.
type UploadInput struct {
	UserID uuid.UUID
	// DocumentID is set when uploading a new version of an existing CV; zero creates a
	// new document.
	DocumentID       uuid.UUID
	Label            string
	OriginalFilename string
	DeclaredType     string
	Body             io.Reader
	// MakeDefault requests that this become the candidate's default CV. Ignored when
	// they already have one, rather than silently stealing the flag.
	MakeDefault bool
}

// UploadResult is what the client needs after an upload.
type UploadResult struct {
	Document *Document
	Version  *Version
}

// maxLabelLength bounds the display label.
const maxLabelLength = 120

// Upload validates, stores and registers a CV, then starts the parse pipeline.
//
// The quota is checked before a byte is read: accepting a 10MB upload and then refusing
// it wastes the candidate's bandwidth and ours. New versions of an existing document do
// not consume a slot, since the slot is the document.
func (s *Service) Upload(ctx context.Context, in UploadInput) (*UploadResult, error) {
	label := strings.TrimSpace(in.Label)
	if len([]rune(label)) > maxLabelLength {
		return nil, apperr.ValidationFields([]apperr.FieldError{{
			Field: "label", Code: "too_long", Message: "must be at most 120 characters",
		}})
	}

	isNewDocument := in.DocumentID == uuid.Nil

	if isNewDocument {
		if err := s.ents.Require(ctx, in.UserID, entitlements.QuotaCVSlots, 1); err != nil {
			return nil, err
		}
	} else {
		// Confirm ownership before storing anything, so a mis-addressed upload does
		// not leave an orphaned object behind.
		if _, err := s.repo.NextVersionNo(ctx, in.UserID, in.DocumentID); err != nil {
			return nil, err
		}
	}

	object, err := s.store.Put(ctx, in.UserID, in.DeclaredType, in.Body)
	if err != nil {
		return nil, s.mapStorageError(err)
	}

	// From here on a failure must remove the stored object, or every failed upload
	// leaves a paid-for orphan in the bucket.
	cleanup := func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if derr := s.store.Delete(cleanupCtx, object.Key); derr != nil {
			s.log.ErrorContext(ctx, "failed to remove an orphaned object after a failed upload",
				slog.String("storage_key", object.Key), slog.Any("error", derr))
		}
	}

	checksum := object.Checksum
	version := &Version{
		ID:             uuid.New(),
		StorageKey:     object.Key,
		ChecksumSHA256: checksum[:],
		SizeBytes:      object.SizeBytes,
		AVStatus:       AVPending,
		ParseStatus:    ParsePending,
	}

	var document *Document

	if isNewDocument {
		makeDefault := in.MakeDefault
		if !makeDefault {
			// Default the very first CV so a candidate who uploads one and never
			// touches the setting still has a usable default.
			allowance, aerr := s.ents.Check(ctx, in.UserID, entitlements.QuotaCVSlots)
			if aerr == nil && allowance.Used == 0 {
				makeDefault = true
			}
		}

		document = &Document{
			ID:               uuid.New(),
			UserID:           in.UserID,
			Source:           SourceUploaded,
			Label:            label,
			OriginalFilename: sanitiseFilename(in.OriginalFilename),
			ContentType:      object.ContentType.String(),
			SizeBytes:        object.SizeBytes,
			IsDefault:        makeDefault,
		}

		err = s.repo.CreateDocumentWithVersion(ctx, document, version)
		if err != nil && apperr.As(err) != nil && apperr.As(err).Code == "default_cv_exists" {
			// Lost the race to be the default. Retry without the flag rather than
			// failing an upload over a cosmetic setting.
			document.IsDefault = false
			err = s.repo.CreateDocumentWithVersion(ctx, document, version)
		}
		if err != nil {
			cleanup()
			return nil, err
		}
	} else {
		version.DocumentID = in.DocumentID
		if err := s.repo.AddVersion(ctx, in.UserID, version); err != nil {
			cleanup()
			return nil, err
		}
		document, err = s.repo.DocumentByID(ctx, in.UserID, in.DocumentID)
		if err != nil {
			// The version is committed, so this is a read failure rather than a write
			// failure; do not clean up a stored object that is now referenced.
			return nil, err
		}
	}

	// Published after commit. A publish failure leaves a version in pending that the
	// reaper will re-enqueue, which is preferable to announcing work that rolled back.
	s.publish(ctx, events.TypeCVUploaded, map[string]any{
		"cv_id":        document.ID.String(),
		"version_id":   version.ID.String(),
		"user_id":      in.UserID.String(),
		"storage_key":  object.Key,
		"content_type": object.ContentType.String(),
		"size_bytes":   object.SizeBytes,
		"checksum":     object.ChecksumHex(),
	})

	document.LatestVersion = version
	if isNewDocument {
		// Set explicitly: the create path builds the document in memory rather than
		// reading it back, so the count would otherwise be reported as zero.
		document.VersionCount = 1
	}
	return &UploadResult{Document: document, Version: version}, nil
}

// mapStorageError converts a storage rejection into a client-facing error.
//
// Each case gets its own message because "upload failed" tells a candidate nothing
// about what to do differently.
func (s *Service) mapStorageError(err error) error {
	switch {
	case errors.Is(err, storage.ErrEmpty):
		return apperr.ValidationFields([]apperr.FieldError{{
			Field: "file", Code: "empty", Message: "the file is empty",
		}})
	case errors.Is(err, storage.ErrTooLarge):
		return apperr.ValidationFields([]apperr.FieldError{{
			Field: "file", Code: "too_large", Message: "the file is larger than 10MB",
		}})
	case errors.Is(err, storage.ErrUnsupportedType):
		return apperr.ValidationFields([]apperr.FieldError{{
			Field: "file", Code: "unsupported_type",
			Message: "only PDF and DOCX files are accepted",
		}})
	case errors.Is(err, storage.ErrTypeMismatch):
		return apperr.ValidationFields([]apperr.FieldError{{
			Field: "file", Code: "type_mismatch",
			Message: "the file's contents do not match its stated type",
		}})
	default:
		return apperr.Dependency("object_storage", err)
	}
}

// List returns the candidate's CV library.
func (s *Service) List(ctx context.Context, userID uuid.UUID) ([]*Document, error) {
	return s.repo.ListDocuments(ctx, userID)
}

// Get returns one document the candidate owns.
func (s *Service) Get(ctx context.Context, userID, documentID uuid.UUID) (*Document, error) {
	return s.repo.DocumentByID(ctx, userID, documentID)
}

// DownloadURL issues a short-lived presigned download link.
//
// Refuses while antivirus is pending or has failed: handing back a link to an unscanned
// or infected file would make the scan pointless.
func (s *Service) DownloadURL(ctx context.Context, userID, documentID uuid.UUID) (string, error) {
	document, err := s.repo.DocumentByID(ctx, userID, documentID)
	if err != nil {
		return "", err
	}
	version := document.LatestVersion
	if version == nil {
		return "", apperr.NotFound("CV version")
	}

	if !version.AVStatus.Usable() {
		return "", apperr.PreconditionFailed("file_not_available",
			"this file is not available for download: %s", version.UnsafeReason())
	}

	name := document.OriginalFilename
	if name == "" {
		name = document.DisplayLabel() + "." + storage.ContentType(document.ContentType).Extension()
	}
	return s.store.PresignedGet(ctx, version.StorageKey, name)
}

// Rename changes a document's label.
func (s *Service) Rename(ctx context.Context, userID, documentID uuid.UUID, label string) error {
	label = strings.TrimSpace(label)
	if label == "" {
		return apperr.ValidationFields([]apperr.FieldError{{
			Field: "label", Code: "required", Message: "a name is required",
		}})
	}
	if len([]rune(label)) > maxLabelLength {
		return apperr.ValidationFields([]apperr.FieldError{{
			Field: "label", Code: "too_long", Message: "must be at most 120 characters",
		}})
	}
	return s.repo.SetLabel(ctx, userID, documentID, label)
}

// SetDefault makes a document the candidate's default CV.
func (s *Service) SetDefault(ctx context.Context, userID, documentID uuid.UUID) error {
	document, err := s.repo.DocumentByID(ctx, userID, documentID)
	if err != nil {
		return err
	}
	// A default CV that cannot be used would silently break loop creation later, so
	// refuse at the point the candidate makes the choice.
	if v := document.LatestVersion; v != nil && !v.SafeToUse() {
		return apperr.PreconditionFailed("cv_not_usable",
			"this CV cannot be used as your default: %s", v.UnsafeReason())
	}
	return s.repo.SetDefault(ctx, userID, documentID)
}

// Delete soft-deletes a document.
//
// Refuses when any version is referenced by an application or a live loop. The schema
// would reject it anyway via RESTRICT, but a foreign-key error is not something a
// candidate can act on, whereas "this CV was used in 3 applications" is.
func (s *Service) Delete(ctx context.Context, userID, documentID uuid.UUID) error {
	if _, err := s.repo.DocumentByID(ctx, userID, documentID); err != nil {
		return err
	}

	referenced, err := s.repo.VersionIsReferenced(ctx, documentID)
	if err != nil {
		return err
	}
	if referenced {
		return apperr.Conflict("cv_in_use",
			"this CV has been used in an application or is attached to a live search, so it cannot be deleted")
	}

	return s.repo.SoftDeleteDocument(ctx, userID, documentID)
}

// VersionForUse loads a version and confirms it may be attached to an application.
//
// Used by the loops and applications domains, so the SafeToUse rule — scanned, parsed,
// and containment-validated if tailored — is enforced in one place rather than
// re-derived by each caller.
func (s *Service) VersionForUse(ctx context.Context, userID, versionID uuid.UUID) (*Version, error) {
	version, err := s.repo.VersionByID(ctx, userID, versionID)
	if err != nil {
		return nil, err
	}
	if !version.SafeToUse() {
		return nil, apperr.PreconditionFailed("cv_not_usable", "%s", version.UnsafeReason())
	}
	return version, nil
}

// sanitiseFilename keeps a filename for display without letting it influence anything.
//
// The original filename is shown back to the candidate, so it must not carry a path or
// control characters. It never reaches an object key.
func sanitiseFilename(name string) string {
	name = strings.ReplaceAll(name, "\x00", "")
	name = strings.ReplaceAll(name, "\r", "")
	name = strings.ReplaceAll(name, "\n", "")
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	name = strings.TrimSpace(name)
	if len([]rune(name)) > 200 {
		name = string([]rune(name)[:200])
	}
	return name
}

// publish emits a domain event, logging rather than failing on error.
func (s *Service) publish(ctx context.Context, eventType string, payload any) {
	if s.bus == nil {
		return
	}
	if err := s.bus.Publish(ctx, eventType, 1, payload); err != nil {
		s.log.ErrorContext(ctx, "event publish failed",
			slog.String("event_type", eventType), slog.Any("error", err))
	}
}
