// Package cv owns candidate documents: upload, versioning, text extraction, structured
// parsing, analysis and tailoring.
//
// A CVDocument is what the candidate thinks of as "my CV". A CVVersion is one immutable
// snapshot of it. The distinction matters because a version referenced by a submitted
// application must stay reproducible forever — the audit trail is only meaningful if the
// document that was sent still exists exactly as it was sent.
package cv

import (
	"strings"
	"time"

	"github.com/google/uuid"
)

// Source records how a document came to exist.
type Source string

const (
	SourceUploaded Source = "uploaded"
	SourceBuilder  Source = "builder"
)

// AVStatus is the antivirus verdict.
type AVStatus string

const (
	AVPending  AVStatus = "pending"
	AVClean    AVStatus = "clean"
	AVInfected AVStatus = "infected"
	AVError    AVStatus = "error"
)

// Usable reports whether a document may be read or parsed.
//
// Only a clean verdict qualifies. Pending is deliberately excluded: using a file before
// it has been scanned would make the scan decorative.
func (s AVStatus) Usable() bool { return s == AVClean }

// ParseStatus tracks progress through the extraction pipeline.
type ParseStatus string

const (
	ParsePending    ParseStatus = "pending"
	ParseScanning   ParseStatus = "scanning"
	ParseExtracting ParseStatus = "extracting"
	ParseParsing    ParseStatus = "parsing"
	ParseParsed     ParseStatus = "parsed"
	ParseFailed     ParseStatus = "failed"
)

// Terminal reports whether no further progress will occur without intervention.
func (s ParseStatus) Terminal() bool { return s == ParseParsed || s == ParseFailed }

// InProgress reports whether the pipeline is still working.
func (s ParseStatus) InProgress() bool {
	switch s {
	case ParsePending, ParseScanning, ParseExtracting, ParseParsing:
		return true
	default:
		return false
	}
}

// Stage returns a short, candidate-facing description of the current stage.
//
// An empty state that names the stage is the difference between a candidate who waits
// and a candidate who assumes the product is broken, so this text is product copy, not
// debug output.
func (s ParseStatus) Stage() string {
	switch s {
	case ParsePending:
		return "Queued"
	case ParseScanning:
		return "Checking the file"
	case ParseExtracting:
		return "Reading the document"
	case ParseParsing:
		return "Understanding your experience"
	case ParseParsed:
		return "Ready"
	case ParseFailed:
		return "Needs attention"
	default:
		return "Working"
	}
}

// Document is a candidate's CV.
type Document struct {
	ID               uuid.UUID
	UserID           uuid.UUID
	Source           Source
	Label            string
	OriginalFilename string
	ContentType      string
	SizeBytes        int64
	IsDefault        bool
	CreatedAt        time.Time
	UpdatedAt        time.Time
	DeletedAt        *time.Time

	// LatestVersion is populated by list and get queries so a client can render the
	// library in one request rather than N+1.
	LatestVersion *Version
	VersionCount  int
}

// Deleted reports whether the document is soft-deleted.
func (d *Document) Deleted() bool { return d.DeletedAt != nil }

// Version is one immutable snapshot of a document.
type Version struct {
	ID             uuid.UUID
	DocumentID     uuid.UUID
	UserID         uuid.UUID
	VersionNo      int
	StorageKey     string
	ChecksumSHA256 []byte
	SizeBytes      int64

	AVStatus    AVStatus
	AVScannedAt *time.Time

	ParseStatus ParseStatus
	ParseError  string
	ParseStage  string
	ParsedAt    *time.Time

	CharCount *int
	PageCount *int

	DerivedFromVersionID    *uuid.UUID
	TailoredForJobPostingID *uuid.UUID
	ContainmentPassed       *bool

	CreatedAt time.Time
}

// IsTailored reports whether this version was generated for a specific posting.
func (v *Version) IsTailored() bool { return v.TailoredForJobPostingID != nil }

// SafeToUse reports whether a version may be attached to an application.
//
// Three conditions, each of which has caused a real problem somewhere: the file must
// have passed antivirus, it must have parsed (an unparsed CV cannot be matched or
// tailored), and if it is tailored it must have passed factual containment. A tailored
// version whose containment check did not pass is a potentially fabricated CV, and
// attaching one to an application is the worst outcome this product has.
func (v *Version) SafeToUse() bool {
	if !v.AVStatus.Usable() {
		return false
	}
	if v.ParseStatus != ParseParsed {
		return false
	}
	if v.IsTailored() && (v.ContainmentPassed == nil || !*v.ContainmentPassed) {
		return false
	}
	return true
}

// UnsafeReason explains why a version cannot be used, for a client-facing message.
func (v *Version) UnsafeReason() string {
	switch {
	case v.AVStatus == AVInfected:
		return "this file did not pass our security scan"
	case v.AVStatus == AVPending:
		return "this file is still being checked"
	case v.AVStatus == AVError:
		return "this file could not be scanned"
	case v.ParseStatus == ParseFailed:
		return "we could not read this CV"
	case v.ParseStatus != ParseParsed:
		return "this CV is still being processed"
	case v.IsTailored() && (v.ContainmentPassed == nil || !*v.ContainmentPassed):
		return "this tailored CV did not pass our accuracy check"
	default:
		return ""
	}
}

// ParseFailureReason classifies why extraction failed, so the candidate gets an
// actionable message rather than a generic error.
type ParseFailureReason string

const (
	// ReasonNoExtractableText means the document contains no machine-readable text,
	// almost always a scanned image. Telling the candidate this specifically is the
	// difference between them fixing it in a minute and giving up.
	ReasonNoExtractableText ParseFailureReason = "no_extractable_text"
	// ReasonEncrypted means the document is password-protected.
	ReasonEncrypted ParseFailureReason = "encrypted"
	// ReasonCorrupt means the document could not be opened.
	ReasonCorrupt ParseFailureReason = "corrupt"
	// ReasonTooLong means the document exceeds the page limit for extraction.
	ReasonTooLong ParseFailureReason = "too_long"
	// ReasonAIUnavailable means structured extraction could not run.
	ReasonAIUnavailable ParseFailureReason = "ai_unavailable"
	// ReasonSchemaViolation means the model's output never validated.
	ReasonSchemaViolation ParseFailureReason = "schema_violation"
	// ReasonInfected means antivirus rejected the file.
	ReasonInfected ParseFailureReason = "infected"
)

// Message returns candidate-facing copy for a failure reason.
func (r ParseFailureReason) Message() string {
	switch r {
	case ReasonNoExtractableText:
		return "This looks like a scanned image rather than a text document. Please upload a CV exported directly from Word, Google Docs or a similar tool."
	case ReasonEncrypted:
		return "This document is password-protected. Please remove the password and upload it again."
	case ReasonCorrupt:
		return "We could not open this document. Please check it opens correctly and upload it again."
	case ReasonTooLong:
		return "This document is longer than we can process. Please upload a CV of 20 pages or fewer."
	case ReasonAIUnavailable, ReasonSchemaViolation:
		return "We could not finish analysing this CV. We will retry automatically, and you can also enter your details manually."
	case ReasonInfected:
		return "This file did not pass our security scan and has been removed."
	default:
		return "We could not process this CV."
	}
}

// Retryable reports whether retrying could succeed without the candidate changing
// anything. A scanned PDF will never parse on a retry; an AI outage will.
func (r ParseFailureReason) Retryable() bool {
	switch r {
	case ReasonAIUnavailable, ReasonSchemaViolation:
		return true
	default:
		return false
	}
}

// MaxExtractionPages bounds extraction work. A CV is a handful of pages; anything far
// beyond that is either not a CV or an attempt to exhaust the worker.
const MaxExtractionPages = 20

// ExtractedText is the output of deterministic text extraction.
type ExtractedText struct {
	Text      string
	CharCount int
	PageCount int
}

// MinExtractableChars is the floor below which a document is treated as having no
// extractable text.
//
// Not zero: a scanned PDF often yields a few stray characters from an OCR layer or a
// footer, and treating those as success would produce an empty profile and a candidate
// wondering why their CV matched nothing.
const MinExtractableChars = 200

// HasUsableText reports whether extraction produced enough text to parse.
func (e ExtractedText) HasUsableText() bool {
	return len(strings.TrimSpace(e.Text)) >= MinExtractableChars
}

// Label returns a display label for a document, falling back to the filename and then
// to a generic name, so the UI never has to render an empty string.
func (d *Document) DisplayLabel() string {
	if l := strings.TrimSpace(d.Label); l != "" {
		return l
	}
	if f := strings.TrimSpace(d.OriginalFilename); f != "" {
		return f
	}
	return "Untitled CV"
}
