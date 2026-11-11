package cv

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/Joakim-animate90/myjobfyt/backend/internal/apperr"
	"github.com/Joakim-animate90/myjobfyt/backend/internal/platform/antivirus"
	"github.com/Joakim-animate90/myjobfyt/backend/internal/platform/docextract"
	"github.com/Joakim-animate90/myjobfyt/backend/internal/platform/events"
	"github.com/google/uuid"
)

// Scanner is the antivirus surface this domain needs.
type Scanner interface {
	Scan(ctx context.Context, r io.Reader) (antivirus.Result, error)
	Name() string
}

// Worker runs the CV pipeline consumers.
type Worker struct {
	repo    Repository
	store   ObjectStore
	scanner Scanner
	bus     events.Publisher
	log     *slog.Logger
	now     func() time.Time
	limits  docextract.Limits
}

// WorkerDeps are the worker's collaborators.
type WorkerDeps struct {
	Repo    Repository
	Store   ObjectStore
	Scanner Scanner
	Bus     events.Publisher
	Logger  *slog.Logger
	Now     func() time.Time
	Limits  docextract.Limits
}

// NewWorker builds the CV worker.
func NewWorker(d WorkerDeps) *Worker {
	now := d.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	limits := d.Limits
	if limits.MaxPages == 0 && limits.MaxChars == 0 {
		limits = docextract.DefaultLimits()
	}
	return &Worker{
		repo:    d.Repo,
		store:   d.Store,
		scanner: d.Scanner,
		bus:     d.Bus,
		log:     d.Logger,
		now:     now,
		limits:  limits,
	}
}

// uploadedPayload is the cv.uploaded event body.
type uploadedPayload struct {
	CVID        string `json:"cv_id"`
	VersionID   string `json:"version_id"`
	UserID      string `json:"user_id"`
	StorageKey  string `json:"storage_key"`
	ContentType string `json:"content_type"`
	SizeBytes   int64  `json:"size_bytes"`
}

// scannedPayload is the cv.scanned event body.
type scannedPayload struct {
	CVID        string `json:"cv_id"`
	VersionID   string `json:"version_id"`
	UserID      string `json:"user_id"`
	StorageKey  string `json:"storage_key"`
	ContentType string `json:"content_type"`
	AVStatus    string `json:"av_status"`
}

// Registrations returns the consumers this domain contributes.
//
// Both stages use InFlightRetry: re-scanning and re-extracting are internally
// idempotent — they overwrite derived columns on the same version — so repeating them
// after a crashed attempt is harmless. The irreversible-side-effect policy belongs to
// the submission consumers, not here.
func (w *Worker) Registrations() []events.Registration {
	return []events.Registration{
		{
			Name:     "cv-av-scan",
			Group:    "cv",
			Stream:   "CV",
			Subjects: []string{events.SubjectFor(events.TypeCVUploaded)},
			Handler:  w.handleUploaded,
			IdempotencyKey: func(env *events.Envelope) string {
				var p uploadedPayload
				if err := env.Decode(&p); err != nil {
					return ""
				}
				return "cv.scan:" + p.VersionID
			},
			InFlight:   events.InFlightRetry,
			AckWait:    2 * time.Minute,
			MaxDeliver: 4,
		},
		{
			Name:     "cv-text-extract",
			Group:    "cv",
			Stream:   "CV",
			Subjects: []string{events.SubjectFor(events.TypeCVScanned)},
			Handler:  w.handleScanned,
			IdempotencyKey: func(env *events.Envelope) string {
				var p scannedPayload
				if err := env.Decode(&p); err != nil {
					return ""
				}
				return "cv.extract:" + p.VersionID
			},
			InFlight:   events.InFlightRetry,
			AckWait:    3 * time.Minute,
			MaxDeliver: 4,
		},
	}
}

// handleUploaded scans a newly uploaded document.
func (w *Worker) handleUploaded(ctx context.Context, env *events.Envelope) error {
	var p uploadedPayload
	if err := env.Decode(&p); err != nil {
		// Undecodable: no retry can fix it. Returning a non-retryable error
		// dead-letters immediately rather than burning four attempts.
		return apperr.Unprocessable("undecodable_payload", "cv.uploaded payload could not be decoded").
			WithInternal(err)
	}

	versionID, err := uuid.Parse(p.VersionID)
	if err != nil {
		return apperr.Unprocessable("invalid_version_id", "cv.uploaded carried an invalid version id").
			WithInternal(err)
	}

	log := w.log.With(
		slog.String("cv_version_id", p.VersionID),
		slog.String("scanner", w.scanner.Name()),
	)

	if err := w.repo.UpdateParseStatus(ctx, versionID, ParseScanning, "av_scan"); err != nil {
		if apperr.KindOf(err) == apperr.KindNotFound {
			// The version was deleted while queued. Nothing to do, and retrying would
			// never succeed.
			log.InfoContext(ctx, "cv version no longer exists; dropping the scan")
			return nil
		}
		return err
	}

	reader, err := w.store.Get(ctx, p.StorageKey)
	if err != nil {
		// Storage may be briefly unavailable, so this is retryable.
		return apperr.Dependency("object_storage", err)
	}
	defer func() { _ = reader.Close() }()

	result, scanErr := w.scanner.Scan(ctx, reader)

	switch {
	case scanErr != nil:
		// An unscannable file is not a safe file. The version stays un-usable and the
		// message is retried; only after MaxDeliver does it dead-letter for an
		// operator, which is the correct escalation for a scanner outage.
		if uerr := w.repo.UpdateAVStatus(ctx, versionID, AVError, w.now()); uerr != nil {
			log.ErrorContext(ctx, "could not record the av error", slog.Any("error", uerr))
		}
		return apperr.Dependency("antivirus", scanErr)

	case result.Verdict == antivirus.VerdictInfected:
		// Terminal, and the object is destroyed. The candidate is told; the file is
		// not kept, because retaining known malware to be helpful is not helpful.
		log.WarnContext(ctx, "upload rejected by antivirus", slog.String("signature", result.Signature))

		if uerr := w.repo.UpdateAVStatus(ctx, versionID, AVInfected, w.now()); uerr != nil {
			return uerr
		}
		if perr := w.repo.MarkParseFailed(ctx, versionID, ReasonInfected, "av_scan"); perr != nil {
			return perr
		}

		// Delete on a detached context: the request context may already be cancelled,
		// and leaving malware in the bucket because of a cancellation is not acceptable.
		deleteCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if derr := w.store.Delete(deleteCtx, p.StorageKey); derr != nil {
			log.ErrorContext(ctx, "could not delete an infected object", slog.Any("error", derr))
		}

		w.publish(ctx, env, events.TypeCVParseFailed, map[string]any{
			"cv_id":      p.CVID,
			"version_id": p.VersionID,
			"user_id":    p.UserID,
			"reason":     string(ReasonInfected),
			"stage":      "av_scan",
		})
		return nil

	case result.Verdict != antivirus.VerdictClean:
		// Only an explicit clean verdict proceeds. Any other value — including one a
		// future scanner adds — must not be treated as a pass.
		return apperr.Dependency("antivirus",
			fmt.Errorf("unexpected verdict %q", result.Verdict))
	}

	if err := w.repo.UpdateAVStatus(ctx, versionID, AVClean, w.now()); err != nil {
		return err
	}

	w.publish(ctx, env, events.TypeCVScanned, map[string]any{
		"cv_id":        p.CVID,
		"version_id":   p.VersionID,
		"user_id":      p.UserID,
		"storage_key":  p.StorageKey,
		"content_type": p.ContentType,
		"av_status":    string(AVClean),
	})
	return nil
}

// handleScanned extracts text from a scanned-clean document.
func (w *Worker) handleScanned(ctx context.Context, env *events.Envelope) error {
	var p scannedPayload
	if err := env.Decode(&p); err != nil {
		return apperr.Unprocessable("undecodable_payload", "cv.scanned payload could not be decoded").
			WithInternal(err)
	}

	versionID, err := uuid.Parse(p.VersionID)
	if err != nil {
		return apperr.Unprocessable("invalid_version_id", "cv.scanned carried an invalid version id").
			WithInternal(err)
	}

	log := w.log.With(slog.String("cv_version_id", p.VersionID))

	if err := w.repo.UpdateParseStatus(ctx, versionID, ParseExtracting, "text_extract"); err != nil {
		if apperr.KindOf(err) == apperr.KindNotFound {
			log.InfoContext(ctx, "cv version no longer exists; dropping the extraction")
			return nil
		}
		return err
	}

	reader, err := w.store.Get(ctx, p.StorageKey)
	if err != nil {
		return apperr.Dependency("object_storage", err)
	}
	content, err := io.ReadAll(reader)
	_ = reader.Close()
	if err != nil {
		return apperr.Dependency("object_storage", err)
	}

	extracted, err := docextract.Extract(content, p.ContentType, w.limits)
	if err != nil {
		reason := classifyExtractionError(err)

		// Only a genuinely transient cause is retried. A scanned PDF will never parse
		// on a retry, so retrying it three more times just delays telling the
		// candidate something they can act on.
		if reason.Retryable() {
			return apperr.Dependency("text_extraction", err)
		}

		log.InfoContext(ctx, "text extraction failed terminally",
			slog.String("reason", string(reason)),
			slog.Int("page_count", extracted.PageCount),
		)

		if perr := w.repo.MarkParseFailed(ctx, versionID, reason, "text_extract"); perr != nil {
			return perr
		}
		w.publish(ctx, env, events.TypeCVParseFailed, map[string]any{
			"cv_id":      p.CVID,
			"version_id": p.VersionID,
			"user_id":    p.UserID,
			"reason":     string(reason),
			"stage":      "text_extract",
		})
		return nil
	}

	// Belt and braces alongside docextract's own check: a document that opened and
	// yielded a handful of characters from a footer is still unusable, and letting it
	// through would produce an empty profile and a baffled candidate.
	if !ExtractedText(extracted).HasUsableText() {
		log.InfoContext(ctx, "extracted text is below the usable threshold",
			slog.Int("char_count", extracted.CharCount))

		if perr := w.repo.MarkParseFailed(ctx, versionID, ReasonNoExtractableText, "text_extract"); perr != nil {
			return perr
		}
		w.publish(ctx, env, events.TypeCVParseFailed, map[string]any{
			"cv_id":      p.CVID,
			"version_id": p.VersionID,
			"user_id":    p.UserID,
			"reason":     string(ReasonNoExtractableText),
			"stage":      "text_extract",
		})
		return nil
	}

	if err := w.repo.StoreExtractedText(ctx, versionID, ExtractedText(extracted)); err != nil {
		return err
	}

	log.InfoContext(ctx, "text extracted",
		slog.Int("char_count", extracted.CharCount),
		slog.Int("page_count", extracted.PageCount),
	)

	w.publish(ctx, env, events.TypeCVTextExtracted, map[string]any{
		"cv_id":      p.CVID,
		"version_id": p.VersionID,
		"user_id":    p.UserID,
		"char_count": extracted.CharCount,
		"page_count": extracted.PageCount,
	})

	// Structured extraction is not implemented yet (MJF-005), so the version is marked
	// parsed on text alone. This is a deliberate interim state rather than leaving
	// every CV stuck in 'extracting': the candidate's CV is downloadable and usable,
	// and the structured pass will re-run over stored text when it lands.
	if err := w.repo.MarkParsed(ctx, versionID, w.now()); err != nil {
		return err
	}

	w.publish(ctx, env, events.TypeCVParsed, map[string]any{
		"cv_id":      p.CVID,
		"version_id": p.VersionID,
		"user_id":    p.UserID,
		"char_count": extracted.CharCount,
		"structured": false,
	})
	return nil
}

// classifyExtractionError maps an extraction failure to a candidate-facing reason.
func classifyExtractionError(err error) ParseFailureReason {
	switch {
	case errors.Is(err, docextract.ErrNoText):
		return ReasonNoExtractableText
	case errors.Is(err, docextract.ErrEncrypted):
		return ReasonEncrypted
	case errors.Is(err, docextract.ErrTooManyPages):
		return ReasonTooLong
	case errors.Is(err, docextract.ErrCorrupt), errors.Is(err, docextract.ErrUnsupported):
		return ReasonCorrupt
	default:
		// An unrecognised failure is treated as corrupt rather than retryable: an
		// unknown error retried four times is four times the cost for the same
		// outcome, and the candidate waits longer to hear about it.
		return ReasonCorrupt
	}
}

// publish emits a follow-on event, carrying the correlation chain forward so the whole
// pipeline remains one queryable workflow.
func (w *Worker) publish(ctx context.Context, parent *events.Envelope, eventType string, payload any) {
	if w.bus == nil {
		return
	}
	if err := w.bus.PublishWithMeta(ctx, eventType, 1, parent.DerivedMeta(), payload); err != nil {
		w.log.ErrorContext(ctx, "event publish failed",
			slog.String("event_type", eventType), slog.Any("error", err))
	}
}
