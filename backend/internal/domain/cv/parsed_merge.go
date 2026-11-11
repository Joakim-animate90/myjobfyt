package cv

import "strings"

// Correction preservation.
//
// A candidate correction is permanently authoritative: a re-parse must never overwrite
// it. Silently reverting someone's edit teaches them the product cannot be trusted,
// which is far more expensive to fix than a stale field.
//
// Preservation is deliberately coarse. A correction is recorded as a field path such as
// "experience[0].employer", but merging one item of a re-extracted list is not
// well-defined: the new extraction may have found a different number of roles, or
// ordered them differently, so index 0 need not be the same role. Rather than guess,
// a correction anywhere inside a section preserves that whole section.
//
// That keeps an un-corrected sibling stale, which is a real cost. It is the right trade:
// losing a correction is worse than keeping a neighbour slightly out of date, and the
// candidate can re-run the parse after clearing the correction if they want the new
// values.

// correctedSection maps a field path to the section it belongs to.
//
// Returns "" for a path that names no known section, which is then ignored rather than
// treated as a wildcard — an unrecognised path must not silently preserve everything.
func correctedSection(fieldPath string) string {
	if i := strings.IndexAny(fieldPath, "[."); i > 0 {
		fieldPath = fieldPath[:i]
	}
	switch fieldPath {
	case "experience", "education", "skills", "certifications", "languages":
		return fieldPath
	case "full_name", "email", "phone", "location", "headline":
		return "contact"
	case "summary":
		return "summary"
	default:
		return ""
	}
}

// correctedScalarField maps a contact or summary path to its specific field, so a
// correction to one contact field does not freeze the others.
//
// Scalars are preserved individually because, unlike list items, they are
// unambiguously identifiable across re-parses: "email" is the same field every time.
func correctedScalarField(fieldPath string) string {
	switch fieldPath {
	case "full_name", "email", "phone", "location", "headline", "summary":
		return fieldPath
	default:
		return ""
	}
}

// MergePreservingCorrections returns the freshly-parsed data with the candidate's
// corrections carried over from the existing record.
//
// existing may be nil, which is the first-parse case and needs no merge.
func MergePreservingCorrections(fresh, existing *ParsedData) *ParsedData {
	if existing == nil || len(existing.CorrectedFields) == 0 {
		return fresh
	}

	// Carry the correction list forward: the candidate still owns these fields after a
	// re-parse. Dropping the list would mean the next re-parse overwrote them.
	fresh.CorrectedFields = append([]string(nil), existing.CorrectedFields...)

	sections := make(map[string]struct{}, len(existing.CorrectedFields))
	scalars := make(map[string]struct{}, len(existing.CorrectedFields))

	for _, path := range existing.CorrectedFields {
		if scalar := correctedScalarField(path); scalar != "" {
			scalars[scalar] = struct{}{}
			continue
		}
		if section := correctedSection(path); section != "" {
			sections[section] = struct{}{}
		}
	}

	// Scalars, preserved individually.
	if _, ok := scalars["full_name"]; ok {
		fresh.Contact.FullName = existing.Contact.FullName
	}
	if _, ok := scalars["email"]; ok {
		fresh.Contact.Email = existing.Contact.Email
	}
	if _, ok := scalars["phone"]; ok {
		fresh.Contact.Phone = existing.Contact.Phone
	}
	if _, ok := scalars["location"]; ok {
		fresh.Contact.Location = existing.Contact.Location
	}
	if _, ok := scalars["headline"]; ok {
		fresh.Contact.Headline = existing.Contact.Headline
	}
	if _, ok := scalars["summary"]; ok {
		fresh.Summary = existing.Summary
	}

	// Sections, preserved whole.
	if _, ok := sections["experience"]; ok {
		fresh.Experience = existing.Experience
		// Derived values must follow the data they were derived from, or the stored
		// total would describe roles that are no longer there.
		fresh.TotalExperienceMonths = existing.TotalExperienceMonths
		fresh.DetectedSeniority = existing.DetectedSeniority
	}
	if _, ok := sections["education"]; ok {
		fresh.Education = existing.Education
	}
	if _, ok := sections["certifications"]; ok {
		fresh.Certifications = existing.Certifications
	}
	if _, ok := sections["languages"]; ok {
		fresh.Languages = existing.Languages
	}

	return fresh
}

// SkillsAreCorrected reports whether the candidate has edited their skills, in which
// case a re-parse must not replace the stored skill rows.
//
// Skills live in their own table rather than in the parsed-data jsonb, so they are
// handled separately from the merge above.
func SkillsAreCorrected(existing *ParsedData) bool {
	if existing == nil {
		return false
	}
	for _, path := range existing.CorrectedFields {
		if correctedSection(path) == "skills" {
			return true
		}
	}
	return false
}
