package cv

import (
	"strings"
	"unicode"
)

// Evidence verification.
//
// The model returns a verbatim quote for each extracted item. This file locates that
// quote in the source text, which does two jobs at once:
//
//   - Provenance. The offset lets the review UI show the candidate exactly where a value
//     came from, so they can judge it rather than take it on trust.
//   - Fabrication detection. A quote that is not in the source means the model
//     paraphrased or invented the entry. That is the cheapest hallucination check
//     available here, and it runs on every extraction rather than only when someone
//     thinks to look.
//
// Matching is tiered because PDF extraction introduces incidental whitespace: an exact
// match can fail for entirely legitimate reasons. But the tiers stop well short of fuzzy
// matching — a tier loose enough to match invented content would defeat the purpose.

// VerificationStatus is how well an evidence quote matched the source.
type VerificationStatus string

const (
	// VerifiedExact means the quote appears in the source character for character.
	VerifiedExact VerificationStatus = "exact"
	// VerifiedNormalised means it appears once incidental whitespace is collapsed. The
	// normal outcome for PDF-extracted text, and fully trustworthy.
	VerifiedNormalised VerificationStatus = "normalised"
	// VerifiedCaseInsensitive means it appears only when case is ignored. Accepted, but
	// worth recording: the model reformatted something it was told to copy.
	VerifiedCaseInsensitive VerificationStatus = "case_insensitive"
	// Unverified means the quote is not in the source at all. The entry is suspect.
	Unverified VerificationStatus = "unverified"
	// NoEvidence means the model supplied no quote for a non-empty value.
	NoEvidence VerificationStatus = "no_evidence"
)

// Trustworthy reports whether the quote was located well enough to trust the entry.
func (s VerificationStatus) Trustworthy() bool {
	switch s {
	case VerifiedExact, VerifiedNormalised, VerifiedCaseInsensitive:
		return true
	default:
		return false
	}
}

// FieldProvenance records where one extracted value came from.
type FieldProvenance struct {
	// Field is the dotted path, e.g. "experience[0].employer".
	Field string `json:"field"`
	// Status is how well the evidence matched.
	Status VerificationStatus `json:"status"`
	// StartOffset and EndOffset locate the evidence in the raw text. -1 when unlocated.
	StartOffset int `json:"start_offset"`
	EndOffset   int `json:"end_offset"`
	// Confidence is the stored score, after any downgrade for weak verification.
	Confidence float64 `json:"confidence"`
	// ModelConfidence is what the model claimed, kept so a model that is confidently
	// wrong is visible in the data rather than only in aggregate.
	ModelConfidence Confidence `json:"model_confidence"`
	// NeedsReview marks a field the candidate should check.
	NeedsReview bool `json:"needs_review"`
}

// VerificationReport summarises an extraction's trustworthiness.
type VerificationReport struct {
	Fields []FieldProvenance `json:"fields"`
	// Verified and Unverified count entries by outcome.
	Verified   int `json:"verified"`
	Unverified int `json:"unverified"`
	// NeedsReview counts fields flagged for the candidate.
	NeedsReview int `json:"needs_review"`
	// AverageConfidence is across verified fields only. Including unverified ones would
	// let fabricated entries drag the average down and disguise a mostly-good
	// extraction as a bad one, or vice versa.
	AverageConfidence float64 `json:"average_confidence"`
}

// UnverifiedRatio is the share of entries whose evidence could not be located.
func (r VerificationReport) UnverifiedRatio() float64 {
	total := r.Verified + r.Unverified
	if total == 0 {
		return 0
	}
	return float64(r.Unverified) / float64(total)
}

// maxUnverifiedRatio is the threshold above which an extraction is rejected outright.
//
// A few unlocatable quotes are expected — PDF extraction mangles text, and a model
// occasionally tidies a quote. A third of them means the model was not reading the
// document, and persisting that would give the candidate a profile full of invented
// detail that matching then acts on.
const maxUnverifiedRatio = 0.34

// minEntriesForRatioCheck avoids rejecting a sparse CV on the strength of one bad quote.
// With two entries, one failure is 50% and means very little.
const minEntriesForRatioCheck = 5

// Acceptable reports whether an extraction is trustworthy enough to persist.
func (r VerificationReport) Acceptable() bool {
	if r.Verified+r.Unverified < minEntriesForRatioCheck {
		// Too few entries to judge by ratio; require at least one verified entry so a
		// wholly invented extraction is still caught.
		return r.Verified > 0 || r.Unverified == 0
	}
	return r.UnverifiedRatio() <= maxUnverifiedRatio
}

// reviewConfidenceThreshold is the stored-confidence level below which a field is
// surfaced for candidate review rather than silently trusted.
const reviewConfidenceThreshold = 0.75

// sourceIndex precomputes the normalisation needed to locate quotes efficiently.
//
// Built once per extraction rather than per field: a CV with 60 extracted entries would
// otherwise renormalise the whole document 60 times.
type sourceIndex struct {
	raw string
	// normalised is the source with runs of whitespace collapsed to single spaces.
	normalised string
	// offsets maps each index in normalised back to its index in raw, so an offset
	// found in the normalised text can be reported against the real document.
	offsets []int
	lowered string
}

// newSourceIndex builds the index for a document.
func newSourceIndex(raw string) *sourceIndex {
	var (
		b       strings.Builder
		offsets = make([]int, 0, len(raw))
		inSpace bool
	)

	for i, r := range raw {
		if unicode.IsSpace(r) {
			if !inSpace {
				b.WriteByte(' ')
				offsets = append(offsets, i)
				inSpace = true
			}
			continue
		}
		inSpace = false
		start := b.Len()
		b.WriteRune(r)
		// A multi-byte rune occupies several bytes in the builder; every one maps back
		// to the same source offset, so an offset lookup never lands mid-rune.
		for range b.Len() - start {
			offsets = append(offsets, i)
		}
	}

	normalised := b.String()
	return &sourceIndex{
		raw:        raw,
		normalised: normalised,
		offsets:    offsets,
		lowered:    strings.ToLower(normalised),
	}
}

// locate finds an evidence quote, returning its status and offsets in the raw text.
func (s *sourceIndex) locate(evidence string) (VerificationStatus, int, int) {
	quote := strings.TrimSpace(evidence)
	if quote == "" {
		return NoEvidence, -1, -1
	}

	// Tier 1: exact. The common case for DOCX, and the strongest signal.
	if i := strings.Index(s.raw, quote); i >= 0 {
		return VerifiedExact, i, i + len(quote)
	}

	normalisedQuote := collapseWhitespace(quote)
	if normalisedQuote == "" {
		return NoEvidence, -1, -1
	}

	// Tier 2: whitespace-normalised. The common case for PDF, where extraction inserts
	// incidental spacing the model did not reproduce.
	if i := strings.Index(s.normalised, normalisedQuote); i >= 0 {
		return VerifiedNormalised, s.rawOffset(i), s.rawOffset(i + len(normalisedQuote) - 1)
	}

	// Tier 3: case-insensitive. Accepted, but recorded, because the model reformatted
	// something it was instructed to copy verbatim.
	if i := strings.Index(s.lowered, strings.ToLower(normalisedQuote)); i >= 0 {
		return VerifiedCaseInsensitive, s.rawOffset(i), s.rawOffset(i + len(normalisedQuote) - 1)
	}

	// No tier matched. Deliberately no fuzzy fallback: a tier loose enough to match
	// paraphrased content would also match invented content, which is the thing this
	// check exists to find.
	return Unverified, -1, -1
}

// rawOffset maps a normalised-text index back to the raw document.
func (s *sourceIndex) rawOffset(i int) int {
	if i < 0 {
		return -1
	}
	if i >= len(s.offsets) {
		if len(s.offsets) == 0 {
			return -1
		}
		return s.offsets[len(s.offsets)-1]
	}
	return s.offsets[i]
}

// collapseWhitespace reduces every run of whitespace to a single space.
func collapseWhitespace(s string) string {
	var (
		b       strings.Builder
		inSpace bool
	)
	for _, r := range s {
		if unicode.IsSpace(r) {
			if !inSpace {
				b.WriteByte(' ')
				inSpace = true
			}
			continue
		}
		inSpace = false
		b.WriteRune(r)
	}
	return strings.TrimSpace(b.String())
}

// VerifyExtraction locates every evidence quote and reports the extraction's provenance.
//
// Values whose evidence cannot be located are downgraded rather than deleted: the
// candidate may recognise a correct value the model simply quoted badly, and silently
// dropping it would lose real information. What must not happen is treating it as
// verified, which is why the confidence is lowered and the field flagged.
func VerifyExtraction(extracted ExtractedCV, rawText string) VerificationReport {
	index := newSourceIndex(rawText)
	report := VerificationReport{Fields: make([]FieldProvenance, 0, 32)}

	var confidenceSum float64

	check := func(field, value string, evidence string, claimed Confidence) {
		// An absent value needs no evidence; the model was told to report it in
		// not_found instead.
		if strings.TrimSpace(value) == "" {
			return
		}

		status, start, end := index.locate(evidence)

		confidence := claimed.Score()
		switch status {
		case VerifiedCaseInsensitive:
			// Verified, but the model did not copy exactly. A small penalty keeps that
			// visible without discarding a usable value.
			confidence *= 0.9
		case Unverified, NoEvidence:
			// Cannot be trusted at any claimed confidence. Floored low so the review UI
			// surfaces it regardless of what the model asserted.
			confidence = ConfidenceLow.Score() * 0.5
		}

		provenance := FieldProvenance{
			Field:           field,
			Status:          status,
			StartOffset:     start,
			EndOffset:       end,
			Confidence:      confidence,
			ModelConfidence: claimed,
			NeedsReview:     !status.Trustworthy() || confidence < reviewConfidenceThreshold,
		}
		report.Fields = append(report.Fields, provenance)

		if status.Trustworthy() {
			report.Verified++
			confidenceSum += confidence
		} else {
			report.Unverified++
		}
		if provenance.NeedsReview {
			report.NeedsReview++
		}
	}

	check("full_name", extracted.FullName.Value, extracted.FullName.Evidence, extracted.FullName.Confidence)
	check("email", extracted.Email.Value, extracted.Email.Evidence, extracted.Email.Confidence)
	check("phone", extracted.Phone.Value, extracted.Phone.Evidence, extracted.Phone.Confidence)
	check("location", extracted.Location.Value, extracted.Location.Evidence, extracted.Location.Confidence)
	check("headline", extracted.Headline.Value, extracted.Headline.Evidence, extracted.Headline.Confidence)
	check("summary", extracted.Summary.Value, extracted.Summary.Evidence, extracted.Summary.Confidence)

	for i, role := range extracted.Experience {
		// The employer and title are checked as the role's identity: those are the two
		// facts an application is built on, and a fabricated employer is the single
		// worst extraction error possible here.
		check(fieldPath("experience", i, "employer"), role.Employer, role.Evidence, role.Confidence)
		check(fieldPath("experience", i, "title"), role.Title, role.Evidence, role.Confidence)
		check(fieldPath("experience", i, "start_date"), role.StartDate, role.Evidence, role.Confidence)
	}
	for i, study := range extracted.Education {
		check(fieldPath("education", i, "institution"), study.Institution, study.Evidence, study.Confidence)
		check(fieldPath("education", i, "qualification"), study.Qualification, study.Evidence, study.Confidence)
	}
	for i, skill := range extracted.Skills {
		check(fieldPath("skills", i, "name"), skill.Name, skill.Evidence, skill.Confidence)
	}
	for i, cert := range extracted.Certifications {
		check(fieldPath("certifications", i, "name"), cert.Name, cert.Evidence, cert.Confidence)
	}
	for i, lang := range extracted.Languages {
		check(fieldPath("languages", i, "language"), lang.Language, lang.Evidence, lang.Confidence)
	}

	if report.Verified > 0 {
		report.AverageConfidence = confidenceSum / float64(report.Verified)
	}
	return report
}

// fieldPath builds a dotted path for a list item's field.
func fieldPath(list string, index int, field string) string {
	return list + "[" + itoa(index) + "]." + field
}

// itoa avoids a strconv import for a value that is always a small non-negative index.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits [8]byte
	i := len(digits)
	for n > 0 && i > 0 {
		i--
		digits[i] = byte('0' + n%10)
		n /= 10
	}
	return string(digits[i:])
}
