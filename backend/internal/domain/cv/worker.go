package cv

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/Joakim-animate90/myjobfyt/backend/internal/apperr"
	"github.com/Joakim-animate90/myjobfyt/backend/internal/platform/ai"
	"github.com/Joakim-animate90/myjobfyt/backend/internal/platform/antivirus"
	"github.com/Joakim-animate90/myjobfyt/backend/internal/platform/docextract"
	"github.com/Joakim-animate90/myjobfyt/backend/internal/platform/embed"
	"github.com/Joakim-animate90/myjobfyt/backend/internal/platform/events"
	"github.com/google/uuid"
)

// Scanner is the antivirus surface this domain needs.
type Scanner interface {
	Scan(ctx context.Context, r io.Reader) (antivirus.Result, error)
	Name() string
}

// Extractor performs structured CV extraction.
//
// Defined here as the narrow surface this domain needs, so cv does not depend on the AI
// client's generic shape and can be tested with a fake that returns scripted
// extractions.
type Extractor interface {
	ExtractCV(ctx context.Context, rawText string) (ExtractedCV, ExtractionMeta, error)
}

// Embedder is the vector surface this domain needs.
//
// Optional: when absent, matching falls back to lexical similarity and marks affected
// matches degraded. A matching engine that stops working without embeddings is a
// matching engine that stops working.
type Embedder interface {
	Embed(ctx context.Context, texts []string) ([]embed.Vector, string, error)
	Name() string
}

// ExtractionMeta is what the extractor reports about the call itself, for logging and
// cost attribution.
type ExtractionMeta struct {
	Model         string
	EngineVersion string
	TotalTokens   int64
	Attempts      int
}

// Worker runs the CV pipeline consumers.
type Worker struct {
	repo      Repository
	store     ObjectStore
	scanner   Scanner
	extractor Extractor
	embedder  Embedder
	bus       events.Publisher
	log       *slog.Logger
	now       func() time.Time
	limits    docextract.Limits

	// lastModel is the embedding model the provider last reported. Guarded because the
	// backfill goroutine reads it while consumer goroutines write it.
	modelMu   sync.RWMutex
	lastModel string
}

// WorkerDeps are the worker's collaborators.
type WorkerDeps struct {
	Repo    Repository
	Store   ObjectStore
	Scanner Scanner
	// Extractor may be nil, in which case the structure stage is skipped and versions
	// are marked parsed on extracted text alone. Deliberately permitted so the
	// pipeline runs end to end without an API key configured; the worker logs that the
	// stage is disabled on startup rather than leaving it to be discovered.
	Extractor Extractor
	// Embedder may be nil, which leaves matching on the lexical path.
	Embedder Embedder
	Bus      events.Publisher
	Logger   *slog.Logger
	Now      func() time.Time
	Limits   docextract.Limits
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
		repo:      d.Repo,
		store:     d.Store,
		scanner:   d.Scanner,
		extractor: d.Extractor,
		embedder:  d.Embedder,
		bus:       d.Bus,
		log:       d.Logger,
		now:       now,
		limits:    limits,
	}
}

// StructuredExtractionEnabled reports whether the structure stage will run, so the
// worker can say so at startup instead of letting it be discovered from missing data.
func (w *Worker) StructuredExtractionEnabled() bool { return w.extractor != nil }

// SemanticMatchingEnabled reports whether embeddings will be generated.
func (w *Worker) SemanticMatchingEnabled() bool { return w.embedder != nil }

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
			Name:     "cv-embed",
			Group:    "cv",
			Stream:   "CV",
			Subjects: []string{events.SubjectFor(events.TypeCVParsed)},
			Handler:  w.handleParsedForEmbedding,
			IdempotencyKey: func(env *events.Envelope) string {
				var p parsedPayload
				if err := env.Decode(&p); err != nil {
					return ""
				}
				return "cv.embed:" + p.VersionID
			},
			// Re-embedding is idempotent: the same text under the same model overwrites
			// the same row. Safe to redo after a crash.
			InFlight:   events.InFlightRetry,
			AckWait:    2 * time.Minute,
			MaxDeliver: 4,
		},
		{
			Name:     "cv-skill-normalise",
			Group:    "cv",
			Stream:   "CV",
			Subjects: []string{events.SubjectFor(events.TypeCVParsed)},
			Handler:  w.handleParsed,
			IdempotencyKey: func(env *events.Envelope) string {
				var p parsedPayload
				if err := env.Decode(&p); err != nil {
					return ""
				}
				return "cv.skills:" + p.VersionID
			},
			// Linking is idempotent: it sets the same skill_id on the same rows, and
			// never touches a row the candidate owns.
			InFlight:   events.InFlightRetry,
			AckWait:    1 * time.Minute,
			MaxDeliver: 4,
		},
		{
			Name:     "cv-structure",
			Group:    "cv",
			Stream:   "CV",
			Subjects: []string{events.SubjectFor(events.TypeCVTextExtracted)},
			Handler:  w.handleTextExtracted,
			IdempotencyKey: func(env *events.Envelope) string {
				var p textExtractedPayload
				if err := env.Decode(&p); err != nil {
					return ""
				}
				return "cv.structure:" + p.VersionID
			},
			// Re-extracting is internally idempotent: it overwrites the same version's
			// derived rows, and correction preservation means a repeat cannot clobber a
			// candidate's edit. So a crashed attempt is safe to redo.
			InFlight: events.InFlightRetry,
			// Generous: a thinking model on a long CV legitimately takes a minute or
			// more, and an AckWait below the handler's real duration would have
			// JetStream redeliver work that is still running.
			AckWait:    5 * time.Minute,
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

	// With no extractor configured the pipeline stops here and the version is marked
	// parsed on text alone. Not an error: the CV is downloadable and usable, and this
	// keeps the platform working without an API key. The structure consumer will pick
	// up the stored text whenever one is configured.
	if w.extractor == nil {
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
	}
	return nil
}

// textExtractedPayload is the cv.text_extracted event body.
type textExtractedPayload struct {
	CVID      string `json:"cv_id"`
	VersionID string `json:"version_id"`
	UserID    string `json:"user_id"`
	CharCount int    `json:"char_count"`
}

// handleTextExtracted runs structured extraction over a version's stored text.
func (w *Worker) handleTextExtracted(ctx context.Context, env *events.Envelope) error {
	if w.extractor == nil {
		// The consumer should not be registered without an extractor, but acknowledging
		// is better than dead-lettering a message nothing can process.
		return nil
	}

	var p textExtractedPayload
	if err := env.Decode(&p); err != nil {
		return apperr.Unprocessable("undecodable_payload", "cv.text_extracted payload could not be decoded").
			WithInternal(err)
	}

	versionID, err := uuid.Parse(p.VersionID)
	if err != nil {
		return apperr.Unprocessable("invalid_version_id", "cv.text_extracted carried an invalid version id").
			WithInternal(err)
	}
	userID, err := uuid.Parse(p.UserID)
	if err != nil {
		return apperr.Unprocessable("invalid_user_id", "cv.text_extracted carried an invalid user id").
			WithInternal(err)
	}

	log := w.log.With(slog.String("cv_version_id", p.VersionID))

	if err := w.repo.UpdateParseStatus(ctx, versionID, ParseParsing, "structure"); err != nil {
		if apperr.KindOf(err) == apperr.KindNotFound {
			log.InfoContext(ctx, "cv version no longer exists; dropping the extraction")
			return nil
		}
		return err
	}

	rawText, err := w.repo.RawText(ctx, versionID)
	if err != nil {
		return err
	}
	if strings.TrimSpace(rawText) == "" {
		// The text-extract stage should have caught this. Reaching here means the row
		// was changed underneath us, so fail terminally rather than calling a model
		// with an empty document.
		return w.failParse(ctx, env, p, versionID, ReasonNoExtractableText, "structure")
	}

	extracted, meta, err := w.extractor.ExtractCV(ctx, rawText)
	if err != nil {
		return w.handleExtractionError(ctx, env, p, versionID, err, log)
	}

	// Verification before persistence. An extraction whose evidence cannot be located
	// is not trustworthy, and storing it would give the candidate a profile of invented
	// detail that matching then acts on.
	report := VerifyExtraction(extracted, rawText)

	log.InfoContext(ctx, "cv extraction verified",
		slog.String("model", meta.Model),
		slog.Int("verified_fields", report.Verified),
		slog.Int("unverified_fields", report.Unverified),
		slog.Int("needs_review", report.NeedsReview),
		slog.Float64("unverified_ratio", report.UnverifiedRatio()),
		slog.Float64("average_confidence", report.AverageConfidence),
		slog.Int64("total_tokens", meta.TotalTokens),
		slog.Int("attempts", meta.Attempts),
	)

	if !report.Acceptable() {
		// Terminal, and loud. This is the model failing to read the document, not a
		// transient fault, and an operator needs to see it rather than have it retried.
		log.ErrorContext(ctx, "cv extraction rejected: too few evidence quotes could be located",
			slog.Float64("unverified_ratio", report.UnverifiedRatio()),
			slog.Int("verified", report.Verified),
			slog.Int("unverified", report.Unverified),
		)
		return w.failParse(ctx, env, p, versionID, ReasonSchemaViolation, "structure")
	}

	parsed := FromExtraction(versionID, userID, extracted, report, meta.EngineVersion)
	skills := SkillsFromExtraction(extracted, report)

	if err := w.repo.StoreParsedData(ctx, parsed, skills); err != nil {
		if apperr.KindOf(err) == apperr.KindNotFound {
			log.InfoContext(ctx, "cv version was deleted during extraction; dropping the result")
			return nil
		}
		return err
	}

	if err := w.repo.MarkParsed(ctx, versionID, w.now()); err != nil {
		return err
	}

	log.InfoContext(ctx, "cv parsed",
		slog.Int("roles", len(parsed.Experience)),
		slog.Int("skills", len(skills)),
		slog.String("seniority", parsed.DetectedSeniority),
		slog.Bool("needs_review", parsed.NeedsReview()),
	)

	w.publish(ctx, env, events.TypeCVParsed, map[string]any{
		"cv_id":            p.CVID,
		"version_id":       p.VersionID,
		"user_id":          p.UserID,
		"structured":       true,
		"skill_count":      len(skills),
		"role_count":       len(parsed.Experience),
		"needs_review":     parsed.NeedsReview(),
		"confidence_avg":   report.AverageConfidence,
		"unverified_count": report.Unverified,
	})
	return nil
}

// parsedPayload is the cv.parsed event body.
type parsedPayload struct {
	CVID       string `json:"cv_id"`
	VersionID  string `json:"version_id"`
	UserID     string `json:"user_id"`
	Structured bool   `json:"structured"`
	SkillCount int    `json:"skill_count"`
}

// handleParsed normalises a parsed CV's skills against the canonical taxonomy.
//
// A separate stage from extraction rather than part of it, so the CV is usable the
// moment it is parsed and the canonical links arrive shortly after. Matching is a
// database lookup, not a model call, so this stage costs nothing and cannot fail for
// provider reasons.
func (w *Worker) handleParsed(ctx context.Context, env *events.Envelope) error {
	var p parsedPayload
	if err := env.Decode(&p); err != nil {
		return apperr.Unprocessable("undecodable_payload", "cv.parsed payload could not be decoded").
			WithInternal(err)
	}

	// A version parsed on text alone has no skills to normalise.
	if !p.Structured {
		return nil
	}

	versionID, err := uuid.Parse(p.VersionID)
	if err != nil {
		return apperr.Unprocessable("invalid_version_id", "cv.parsed carried an invalid version id").
			WithInternal(err)
	}

	log := w.log.With(slog.String("cv_version_id", p.VersionID))

	labels, err := w.repo.SkillLabelsForVersion(ctx, versionID)
	if err != nil {
		if apperr.KindOf(err) == apperr.KindNotFound {
			return nil
		}
		return err
	}
	if len(labels) == 0 {
		return nil
	}

	resolved, err := w.repo.MatchSkillLabels(ctx, labels)
	if err != nil {
		return err
	}

	linked, err := w.repo.LinkSkillIDs(ctx, versionID, resolved)
	if err != nil {
		return err
	}

	// Best-effort: losing one triage row costs a little curation signal, while failing
	// here would cost the candidate their skill links for a bookkeeping problem.
	if rerr := w.repo.RecordUnmatchedLabels(ctx, resolved); rerr != nil {
		log.WarnContext(ctx, "unmatched skill labels were not recorded for triage",
			slog.Any("error", rerr))
	}

	unmatched := make([]string, 0, len(resolved))
	for _, skill := range resolved {
		if !skill.Outcome.Matched() {
			unmatched = append(unmatched, skill.Normalised)
		}
	}

	log.InfoContext(ctx, "cv skills normalised",
		slog.Int("labels", len(labels)),
		slog.Int("linked", linked),
		slog.Int("unmatched", len(unmatched)),
		// The unmatched labels themselves are safe to log: they are skill names, not
		// personal data, and seeing them is how the taxonomy gaps get noticed.
		slog.Any("unmatched_labels", unmatched),
	)
	return nil
}

// handleParsedForEmbedding generates and stores a version's vectors.
//
// A separate consumer from skill normalisation, both fed by cv.parsed, because they
// fail independently: an embedding outage must not stop skills being linked, and a
// taxonomy problem must not stop vectors being stored.
func (w *Worker) handleParsedForEmbedding(ctx context.Context, env *events.Envelope) error {
	if w.embedder == nil {
		// The consumer should not be registered without an embedder, but acknowledging
		// beats dead-lettering a message nothing can process.
		return nil
	}

	var p parsedPayload
	if err := env.Decode(&p); err != nil {
		return apperr.Unprocessable("undecodable_payload", "cv.parsed payload could not be decoded").
			WithInternal(err)
	}
	if !p.Structured {
		// Nothing to embed: a version parsed on text alone has no structured fields.
		return nil
	}

	versionID, err := uuid.Parse(p.VersionID)
	if err != nil {
		return apperr.Unprocessable("invalid_version_id", "cv.parsed carried an invalid version id").
			WithInternal(err)
	}

	log := w.log.With(slog.String("cv_version_id", p.VersionID))

	result, err := w.embedVersion(ctx, versionID, log)
	if err != nil {
		return err
	}
	if result == nil {
		// Nothing embeddable, or the version went away. Already logged.
		return nil
	}

	// Published so matching can rescore anything it had to score lexically.
	w.publish(ctx, env, events.TypeCVEmbedded, map[string]any{
		"cv_id":      p.CVID,
		"version_id": p.VersionID,
		"user_id":    p.UserID,
		"model":      result.Model,
		"kinds":      result.Kinds,
		"dims":       result.Dims,
	})
	return nil
}

// embedResult describes a stored embedding set.
type embedResult struct {
	Model string
	Kinds []string
	Dims  int
}

// embedVersion generates and stores one version's vectors.
//
// Shared by the live consumer and the backfill so an outage recovery goes through
// exactly the same validation, model handling and storage as a first-time embedding. A
// nil result with a nil error means there was nothing to embed, which is not a failure.
func (w *Worker) embedVersion(ctx context.Context, versionID uuid.UUID, log *slog.Logger) (*embedResult, error) {
	input, err := w.repo.EmbeddingInputs(ctx, versionID)
	if err != nil {
		if apperr.KindOf(err) == apperr.KindNotFound {
			log.InfoContext(ctx, "parsed data is gone; dropping the embedding")
			return nil, nil
		}
		return nil, err
	}
	if !input.HasAnything() {
		log.InfoContext(ctx, "nothing to embed for this version")
		return nil, nil
	}

	kinds, texts := input.Texts()

	vectors, model, err := w.embedder.Embed(ctx, texts)
	if err != nil {
		if embed.Unavailable(err) {
			// Retryable. Matching continues lexically in the meantime, marked degraded,
			// and the backfill re-embeds this version once the service returns. Nothing
			// is lost, it is only temporarily less precise.
			log.WarnContext(ctx, "embedding service unavailable; matching will use the lexical fallback",
				slog.Any("error", err))
			return nil, apperr.Dependency("embedding_service", err)
		}
		// A non-availability failure is our bug — an empty input, a bad batch. Terminal,
		// because retrying produces the same result.
		log.ErrorContext(ctx, "embedding failed terminally", slog.Any("error", err))
		return nil, apperr.Unprocessable("embedding_failed", "embeddings could not be generated").
			WithInternal(err)
	}

	if len(vectors) != len(kinds) {
		return nil, apperr.Internalf(
			fmt.Errorf("embedder returned %d vectors for %d kinds", len(vectors), len(kinds)),
			"embedding count mismatch")
	}

	stored := make([]StoredEmbedding, 0, len(vectors))
	for i, vector := range vectors {
		stored = append(stored, StoredEmbedding{
			CVVersionID: versionID,
			Kind:        kinds[i],
			Model:       model,
			Vector:      vector,
		})
	}

	if err := w.repo.StoreEmbeddings(ctx, stored); err != nil {
		if apperr.KindOf(err) == apperr.KindNotFound {
			log.InfoContext(ctx, "cv version was deleted during embedding; dropping the result")
			return nil, nil
		}
		return nil, err
	}

	kindNames := make([]string, 0, len(kinds))
	for _, kind := range kinds {
		kindNames = append(kindNames, string(kind))
	}

	w.rememberModel(model)

	log.InfoContext(ctx, "cv embedded",
		slog.String("model", model),
		slog.Any("kinds", kindNames),
		slog.Int("dims", vectors[0].Dims()),
	)

	return &embedResult{Model: model, Kinds: kindNames, Dims: vectors[0].Dims()}, nil
}

// rememberModel records the model the provider last reported.
//
// The model is discovered from a response rather than configured, so the worker does
// not know it until an embedding succeeds. The backfill needs it to ask which versions
// are missing vectors for the current model.
func (w *Worker) rememberModel(model string) {
	if model == "" {
		return
	}
	w.modelMu.Lock()
	w.lastModel = model
	w.modelMu.Unlock()
}

func (w *Worker) currentModel() string {
	w.modelMu.RLock()
	defer w.modelMu.RUnlock()
	return w.lastModel
}

// handleExtractionError decides whether an AI failure is worth retrying.
func (w *Worker) handleExtractionError(ctx context.Context, env *events.Envelope, p textExtractedPayload, versionID uuid.UUID, err error, log *slog.Logger) error {
	if ai.Retryable(err) {
		// Provider outage or throttling. Retryable, so the version stays in 'parsing'
		// and redelivery picks it up.
		log.WarnContext(ctx, "cv extraction failed transiently; will retry", slog.Any("error", err))
		return apperr.Dependency("ai_provider", err)
	}

	reason := ReasonSchemaViolation
	switch {
	case errors.Is(err, ai.ErrRefused):
		// Worth its own log line: on this platform a refusal usually means the CV or a
		// job description contained something the classifier objected to, which an
		// operator needs to see rather than have silently retried.
		log.ErrorContext(ctx, "the provider declined to process this CV", slog.Any("error", err))
	case errors.Is(err, ai.ErrTruncated):
		reason = ReasonTooLong
		log.InfoContext(ctx, "cv extraction output was truncated", slog.Any("error", err))
	default:
		log.ErrorContext(ctx, "cv extraction failed terminally", slog.Any("error", err))
	}

	return w.failParse(ctx, env, p, versionID, reason, "structure")
}

// failParse records a terminal parse failure and notifies.
func (w *Worker) failParse(ctx context.Context, env *events.Envelope, p textExtractedPayload, versionID uuid.UUID, reason ParseFailureReason, stage string) error {
	if err := w.repo.MarkParseFailed(ctx, versionID, reason, stage); err != nil {
		return err
	}
	w.publish(ctx, env, events.TypeCVParseFailed, map[string]any{
		"cv_id":      p.CVID,
		"version_id": p.VersionID,
		"user_id":    p.UserID,
		"reason":     string(reason),
		"stage":      stage,
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

// publishNew emits an event that is not a reaction to another event.
//
// The backfill has no parent envelope: it is a recovery sweep driven by a timer rather
// than by a candidate's upload, so there is no correlation chain to carry forward and
// inventing one would make the trace claim a causal link that does not exist. A fresh
// correlation id keeps the sweep's own events queryable together.
func (w *Worker) publishNew(ctx context.Context, eventType string, payload any) {
	if w.bus == nil {
		return
	}
	if err := w.bus.Publish(ctx, eventType, 1, payload); err != nil {
		w.log.ErrorContext(ctx, "event publish failed",
			slog.String("event_type", eventType), slog.Any("error", err))
	}
}

// EmbeddingBackfillConfig bounds the backfill.
type EmbeddingBackfillConfig struct {
	// Interval between sweeps. A sweep is two cheap indexed queries when there is
	// nothing to do, so this can be short without being expensive.
	Interval time.Duration
	// BatchSize bounds one sweep, so a large backlog drains over several sweeps rather
	// than monopolising the worker and starving live uploads.
	BatchSize int
}

func (c EmbeddingBackfillConfig) withDefaults() EmbeddingBackfillConfig {
	if c.Interval <= 0 {
		c.Interval = 5 * time.Minute
	}
	if c.BatchSize <= 0 {
		c.BatchSize = 50
	}
	return c
}

// RunEmbeddingBackfill re-embeds versions that have no vectors, until ctx is cancelled.
//
// This is what makes an embedding outage temporary rather than permanent. While the
// provider is down the consumer retries, exhausts its deliveries and dead-letters; the
// CV is still parsed, still has its skills linked and still matches lexically, marked
// degraded. Without this sweep that version would stay unembedded forever and the
// candidate would be permanently matched on the weaker signal through no fault of
// their own — the recovery query would exist and nothing would ever call it.
//
// Blocks, so callers run it in a goroutine. Returns nil on cancellation.
func (w *Worker) RunEmbeddingBackfill(ctx context.Context, cfg EmbeddingBackfillConfig) error {
	if w.embedder == nil {
		return nil
	}
	cfg = cfg.withDefaults()

	w.log.InfoContext(ctx, "embedding backfill started",
		slog.Duration("interval", cfg.Interval),
		slog.Int("batch_size", cfg.BatchSize),
	)

	ticker := time.NewTicker(cfg.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := w.backfillOnce(ctx, cfg.BatchSize); err != nil {
				if ctx.Err() != nil {
					return nil
				}
				// Never fatal: the next sweep tries again. A backfill that killed the
				// worker on a transient failure would take the live pipeline down with it.
				w.log.WarnContext(ctx, "embedding backfill sweep failed; will retry",
					slog.Any("error", err))
			}
		}
	}
}

// backfillOnce embeds one batch of versions that are missing vectors.
func (w *Worker) backfillOnce(ctx context.Context, batchSize int) error {
	pending, err := w.pendingForBackfill(ctx, batchSize)
	if err != nil {
		return err
	}
	if len(pending) == 0 {
		return nil
	}

	w.log.InfoContext(ctx, "embedding backfill found versions to re-embed",
		slog.Int("count", len(pending)),
	)

	var embedded, skipped int
	for _, versionID := range pending {
		if ctx.Err() != nil {
			return nil
		}

		log := w.log.With(slog.String("cv_version_id", versionID.String()))
		result, err := w.embedVersion(ctx, versionID, log)
		if err != nil {
			if apperr.KindOf(err) == apperr.KindDependency {
				// Still down. Stop the sweep rather than hammering the provider once
				// per pending version; the next sweep picks up where this left off.
				w.log.WarnContext(ctx, "embedding still unavailable; ending this sweep",
					slog.Int("embedded", embedded),
					slog.Int("remaining", len(pending)-embedded-skipped),
				)
				return nil
			}
			// One unembeddable version must not stop the rest of the batch.
			log.WarnContext(ctx, "backfill could not embed this version", slog.Any("error", err))
			skipped++
			continue
		}
		if result == nil {
			skipped++
			continue
		}
		embedded++

		// Published so matching rescores what it had to score lexically. The envelope
		// has no parent event — this is a recovery sweep, not a reaction to an upload.
		w.publishNew(ctx, events.TypeCVEmbedded, map[string]any{
			"version_id": versionID.String(),
			"model":      result.Model,
			"kinds":      result.Kinds,
			"dims":       result.Dims,
			"backfilled": true,
		})
	}

	w.log.InfoContext(ctx, "embedding backfill sweep complete",
		slog.Int("embedded", embedded),
		slog.Int("skipped", skipped),
	)
	return nil
}

// pendingForBackfill picks the right recovery query for what the worker knows.
//
// Until an embedding has succeeded the worker does not know the provider's model name,
// because the model is reported in a response rather than configured. On a cold start
// it therefore asks for versions with no vectors at all, which is exactly the set an
// outage leaves behind. Once the model is known it uses the model-scoped query, which
// additionally catches versions left behind by a model upgrade.
func (w *Worker) pendingForBackfill(ctx context.Context, batchSize int) ([]uuid.UUID, error) {
	if model := w.currentModel(); model != "" {
		return w.repo.VersionsNeedingEmbedding(ctx, model, batchSize)
	}
	return w.repo.VersionsWithoutAnyEmbedding(ctx, batchSize)
}
