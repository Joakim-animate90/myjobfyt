package cv

import (
	"strings"
	"unicode"
)

// Skill normalisation.
//
// Raw skill strings are not comparable: "JS", "Javascript" and "JavaScript" are one
// skill written three ways, and skill overlap is one of the heaviest match dimensions.
// If overlap is noise, matching is noise.
//
// # Why there is no fuzzy matching here
//
// The obvious design is trigram similarity above some threshold, and it cannot be made
// safe for this data. Measured against the development database (pg_trgm):
//
//	postgre / postgres      0.700   should match
//	javscript / javascript  0.615   should match (typo)
//	kubernets / kubernetes  0.615   should match (typo)
//	scala / scale           0.500   MUST NOT match
//	pyton / python          0.444   should match (typo)
//	terrafrom / terraform   0.429   should match (typo)
//	kotlin / koltin         0.273   should match (typo)
//	ruby / rust             0.250   MUST NOT match
//
// A pair that must not match scores higher than three that should. There is no
// threshold that admits the typos and rejects scala/scale, and a length guard does not
// help because those two are the same length.
//
// Since a wrong skill match corrupts overlap scoring — and the candidate then acts on
// the resulting match score — the trade is not worth taking. Matching is therefore
// exact-only, over a normalisation that removes separators but never content, plus a
// curated alias list. Labels that match nothing are retained verbatim with a null
// skill_id and recorded, so the taxonomy grows from what candidates actually write
// rather than from guesses.
//
// If anyone is tempted to add fuzzy matching later: re-run the measurement in
// TestTrigramCannotSeparateCorrectFromIncorrectMatches first.

// NormaliseLabel reduces a skill label to its comparison form.
//
// Lowercases, collapses whitespace, and removes separator characters — dots, hyphens,
// underscores, slashes and spaces — so "Node.js", "node js", "node-js" and "NodeJS" all
// become "nodejs".
//
// It deliberately keeps '+' and '#', because those carry meaning: stripping them would
// collapse C, C++ and C# into one skill, which is three different languages and
// three different sets of jobs.
func NormaliseLabel(label string) string {
	var b strings.Builder
	b.Grow(len(label))

	for _, r := range strings.ToLower(strings.TrimSpace(label)) {
		switch {
		case r == '+' || r == '#':
			// Meaningful: C vs C++ vs C#.
			b.WriteRune(r)
		case r == '.' || r == '-' || r == '_' || r == '/' || r == '\\':
			// Separators carry no meaning in a skill name.
		case unicode.IsSpace(r):
			// Also a separator: "react native" and "reactnative" are one skill.
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
		default:
			// Anything else — brackets, commas, quotes — is punctuation a CV picked up
			// from its own formatting.
		}
	}

	return b.String()
}

// MatchOutcome records how a label was resolved.
type MatchOutcome string

const (
	// MatchedCanonical means the normalised label equalled a canonical skill name.
	MatchedCanonical MatchOutcome = "canonical"
	// MatchedAlias means it equalled one of a skill's curated aliases.
	MatchedAlias MatchOutcome = "alias"
	// Unmatched means nothing in the taxonomy equalled it. The label is still kept.
	Unmatched MatchOutcome = "unmatched"
)

// Matched reports whether the label resolved to a canonical skill.
func (o MatchOutcome) Matched() bool {
	return o == MatchedCanonical || o == MatchedAlias
}

// NormalisedSkill is one label after normalisation.
type NormalisedSkill struct {
	// RawLabel is exactly what the CV said, always retained. The candidate wrote it and
	// may recognise it even when the taxonomy does not.
	RawLabel string
	// Normalised is the comparison form.
	Normalised string
	// Skill is the canonical match, or nil when unmatched.
	Skill   *TaxonomySkill
	Outcome MatchOutcome
}

// TaxonomySkill is a canonical skill from the taxonomy.
type TaxonomySkill struct {
	ID            string
	CanonicalName string
	Category      string
}

// MaxSkillLabelLength bounds a label.
//
// A real skill name is short. Anything longer is a sentence the model mistakenly put in
// the skills array, and storing it would pollute both the taxonomy feedback and the
// candidate's profile.
const MaxSkillLabelLength = 64

// UsableSkillLabel reports whether a label is worth storing at all.
func UsableSkillLabel(label string) bool {
	normalised := NormaliseLabel(label)
	if normalised == "" {
		return false
	}
	if len([]rune(label)) > MaxSkillLabelLength {
		return false
	}
	// A label with no letters is a number or a fragment of punctuation, not a skill.
	hasLetter := false
	for _, r := range normalised {
		if unicode.IsLetter(r) {
			hasLetter = true
			break
		}
	}
	return hasLetter
}

// DedupeLabels reduces a list of raw labels to the distinct ones by normalised form,
// keeping the first spelling of each.
//
// The first spelling is kept because it is the one the candidate wrote highest in their
// CV, which is usually their preferred rendering.
func DedupeLabels(labels []string) []string {
	seen := make(map[string]struct{}, len(labels))
	out := make([]string, 0, len(labels))

	for _, label := range labels {
		if !UsableSkillLabel(label) {
			continue
		}
		key := NormaliseLabel(label)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, strings.TrimSpace(label))
	}
	return out
}
