package cv

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

// ViolationKind classifies what a tailored CV tried to introduce.
type ViolationKind string

const (
	// ViolationEmployer means an employer appears that is not in the source.
	ViolationEmployer ViolationKind = "employer"
	// ViolationTitle means a job title appears that is not in the source.
	ViolationTitle ViolationKind = "title"
	// ViolationDate means a date appears that is not in the source.
	ViolationDate ViolationKind = "date"
	// ViolationCertification means a certification appears that is not in the source.
	ViolationCertification ViolationKind = "certification"
	// ViolationQualification means a degree or institution appears that is not in the
	// source.
	ViolationQualification ViolationKind = "qualification"
	// ViolationSkill means a skill appears that is not in the source.
	ViolationSkill ViolationKind = "skill"
	// ViolationMetric means a number or quantity appears in prose that is not in the
	// source. "Increased revenue by 40%" is a fabricated achievement even when every
	// employer and title is genuine, and it is the kind a candidate is least able to
	// defend in an interview.
	ViolationMetric ViolationKind = "metric"
	// ViolationStructure means the output has more roles, qualifications or
	// certifications than the source. Tailoring reorders and re-emphasises; it never
	// adds entries.
	ViolationStructure ViolationKind = "structure"
)

// Violation is one fabricated fact.
type Violation struct {
	Kind ViolationKind
	// Field locates it, e.g. "experience[1].employer".
	Field string
	// Value is what the output claimed.
	Value string
	// Detail explains why it was rejected, for the operator and the candidate.
	Detail string
}

func (v Violation) String() string {
	return fmt.Sprintf("%s at %s: %q — %s", v.Kind, v.Field, v.Value, v.Detail)
}

// ContainmentReport is the verdict on a tailored CV.
//
// This is the hard gate MJF-011 requires. Prompt instructions are necessary but not
// sufficient: models comply imperfectly, and a job description is a document a stranger
// wrote that may ask for exactly the fabrication the prompt forbids. The validator runs
// after generation, independent of the prompt, and its verdict is what decides whether
// the candidate ever sees the output.
type ContainmentReport struct {
	Passed     bool
	Violations []Violation
}

// Summary renders the report for a log line or an operator.
//
// Deliberately counts by kind rather than listing values: the values are the
// candidate's CV contents and a violation value can contain whatever a prompt injection
// put there, neither of which belongs in a shared log stream.
func (r ContainmentReport) Summary() string {
	if r.Passed {
		return "contained"
	}
	counts := map[ViolationKind]int{}
	for _, v := range r.Violations {
		counts[v.Kind]++
	}
	kinds := make([]string, 0, len(counts))
	for kind, n := range counts {
		kinds = append(kinds, fmt.Sprintf("%s=%d", kind, n))
	}
	sort.Strings(kinds)
	return "rejected: " + strings.Join(kinds, " ")
}

// CheckContainment verifies that a tailored CV introduces no fact absent from its
// source.
//
// The comparison is deliberately asymmetric. Dropping or shortening is safe — a
// tailored CV that omits a role or trims a title claims less than the candidate can
// support. Adding is not. So a tailored value passes when it matches a source value
// exactly, or is a shortening of one; anything that introduces a token the source never
// contained is a fabrication.
func CheckContainment(source *ParsedData, sourceSkills []Skill, tailored *ParsedData) ContainmentReport {
	var violations []Violation

	if source == nil || tailored == nil {
		// A missing side cannot be validated, and "cannot validate" must never read as
		// "valid". This is the direction every ambiguity in this file resolves.
		return ContainmentReport{
			Passed: false,
			Violations: []Violation{{
				Kind:   ViolationStructure,
				Field:  "cv",
				Value:  "",
				Detail: "the tailored CV or its source is missing, so containment cannot be verified",
			}},
		}
	}

	violations = append(violations, checkExperience(source, tailored)...)
	violations = append(violations, checkEducation(source, tailored)...)
	violations = append(violations, checkCertifications(source, tailored)...)
	violations = append(violations, checkSkills(sourceSkills, tailored)...)
	violations = append(violations, checkMetrics(source, tailored)...)

	return ContainmentReport{Passed: len(violations) == 0, Violations: violations}
}

// checkExperience rejects invented employers, titles and dates, and extra roles.
func checkExperience(source, tailored *ParsedData) []Violation {
	var violations []Violation

	if len(tailored.Experience) > len(source.Experience) {
		violations = append(violations, Violation{
			Kind:  ViolationStructure,
			Field: "experience",
			Value: fmt.Sprintf("%d roles", len(tailored.Experience)),
			Detail: fmt.Sprintf("the source has %d roles; tailoring reorders and re-emphasises, it never adds one",
				len(source.Experience)),
		})
	}

	employers := make([]string, 0, len(source.Experience))
	titles := make([]string, 0, len(source.Experience))
	for _, role := range source.Experience {
		employers = append(employers, role.Employer)
		titles = append(titles, role.Title)
	}
	dates := sourceDates(source)

	for i, role := range tailored.Experience {
		field := fmt.Sprintf("experience[%d]", i)

		if v, ok := checkValue(role.Employer, employers, ViolationEmployer, field+".employer",
			"no such employer appears in the candidate's CV"); !ok {
			violations = append(violations, v)
		}
		if v, ok := checkValue(role.Title, titles, ViolationTitle, field+".title",
			"no such job title appears in the candidate's CV"); !ok {
			violations = append(violations, v)
		}
		for label, value := range map[string]string{"start_date": role.StartDate, "end_date": role.EndDate} {
			if v, ok := checkValue(value, dates, ViolationDate, field+"."+label,
				"this date does not appear in the candidate's CV"); !ok {
				violations = append(violations, v)
			}
		}
	}
	return violations
}

// checkEducation rejects invented qualifications and institutions.
func checkEducation(source, tailored *ParsedData) []Violation {
	var violations []Violation

	if len(tailored.Education) > len(source.Education) {
		violations = append(violations, Violation{
			Kind:   ViolationStructure,
			Field:  "education",
			Value:  fmt.Sprintf("%d entries", len(tailored.Education)),
			Detail: fmt.Sprintf("the source has %d", len(source.Education)),
		})
	}

	quals := make([]string, 0, len(source.Education))
	institutions := make([]string, 0, len(source.Education))
	fields := make([]string, 0, len(source.Education))
	for _, study := range source.Education {
		quals = append(quals, study.Qualification)
		institutions = append(institutions, study.Institution)
		fields = append(fields, study.Field)
	}

	for i, study := range tailored.Education {
		at := fmt.Sprintf("education[%d]", i)
		if v, ok := checkValue(study.Qualification, quals, ViolationQualification, at+".qualification",
			"no such qualification appears in the candidate's CV"); !ok {
			violations = append(violations, v)
		}
		if v, ok := checkValue(study.Institution, institutions, ViolationQualification, at+".institution",
			"no such institution appears in the candidate's CV"); !ok {
			violations = append(violations, v)
		}
		if v, ok := checkValue(study.Field, fields, ViolationQualification, at+".field",
			"no such field of study appears in the candidate's CV"); !ok {
			violations = append(violations, v)
		}
	}
	return violations
}

// checkCertifications rejects invented certifications.
//
// A fabricated certification is the most damaging of all: it is checkable, and an
// employer who checks finds a lie told in the candidate's name.
func checkCertifications(source, tailored *ParsedData) []Violation {
	var violations []Violation

	if len(tailored.Certifications) > len(source.Certifications) {
		violations = append(violations, Violation{
			Kind:   ViolationStructure,
			Field:  "certifications",
			Value:  fmt.Sprintf("%d entries", len(tailored.Certifications)),
			Detail: fmt.Sprintf("the source has %d", len(source.Certifications)),
		})
	}

	names := make([]string, 0, len(source.Certifications))
	issuers := make([]string, 0, len(source.Certifications))
	for _, cert := range source.Certifications {
		names = append(names, cert.Name)
		issuers = append(issuers, cert.Issuer)
	}

	for i, cert := range tailored.Certifications {
		at := fmt.Sprintf("certifications[%d]", i)
		if v, ok := checkValue(cert.Name, names, ViolationCertification, at+".name",
			"no such certification appears in the candidate's CV"); !ok {
			violations = append(violations, v)
		}
		if v, ok := checkValue(cert.Issuer, issuers, ViolationCertification, at+".issuer",
			"no such certification issuer appears in the candidate's CV"); !ok {
			violations = append(violations, v)
		}
	}
	return violations
}

// checkSkills rejects any skill the source never claimed.
//
// Skills are compared with the taxonomy's own normalisation, so "Node.js" and "nodejs"
// are one skill rather than a false violation. Anything genuinely new is rejected: a
// skill invented to fit a posting is a claim the candidate has to defend in an
// interview.
func checkSkills(sourceSkills []Skill, tailored *ParsedData) []Violation {
	known := make(map[string]struct{}, len(sourceSkills))
	for _, skill := range sourceSkills {
		if n := NormaliseLabel(skill.RawLabel); n != "" {
			known[n] = struct{}{}
		}
	}

	var violations []Violation
	for i, claimed := range tailoredSkillLabels(tailored) {
		n := NormaliseLabel(claimed)
		if n == "" {
			continue
		}
		if _, ok := known[n]; !ok {
			violations = append(violations, Violation{
				Kind:   ViolationSkill,
				Field:  fmt.Sprintf("skills[%d]", i),
				Value:  claimed,
				Detail: "this skill does not appear in the candidate's CV",
			})
		}
	}
	return violations
}

// tailoredSkillLabels reads the skills a tailored CV claims.
//
// ParsedData carries no skill list of its own — skills live in cv_skills — so a
// tailored CV states them in its summary structure. Kept as a seam so the caller can
// supply them explicitly rather than this guessing.
func tailoredSkillLabels(tailored *ParsedData) []string {
	return tailored.ClaimedSkills
}

// metricPattern matches a number with optional decimals, percent, or scale suffix.
//
// Deliberately broad: a missed number is a fabricated metric that reaches an employer,
// while an over-matched one costs a retry.
var metricPattern = regexp.MustCompile(`\d+(?:[.,]\d+)*\s*(?:%|k|m|bn|b|x)?`)

// checkMetrics rejects quantities in prose that the source never stated.
//
// Employers, titles and dates are structured and exactly checkable. Prose is where
// tailoring is supposed to do its work, and so is where a fabrication hides most
// easily: "reduced latency by 60%" is a lie even when every employer is genuine. The
// numbers must already be in the candidate's own words.
func checkMetrics(source, tailored *ParsedData) []Violation {
	known := map[string]struct{}{}
	for _, text := range sourceProse(source) {
		for _, m := range metricPattern.FindAllString(text, -1) {
			known[normaliseMetric(m)] = struct{}{}
		}
	}

	var violations []Violation
	for field, text := range tailoredProse(tailored) {
		for _, m := range metricPattern.FindAllString(text, -1) {
			n := normaliseMetric(m)
			if n == "" {
				continue
			}
			if _, ok := known[n]; !ok {
				violations = append(violations, Violation{
					Kind:   ViolationMetric,
					Field:  field,
					Value:  m,
					Detail: "this figure does not appear in the candidate's CV",
				})
			}
		}
	}
	sort.Slice(violations, func(i, j int) bool { return violations[i].Field < violations[j].Field })
	return violations
}

// normaliseMetric reduces a figure to a canonical value so that formatting differences
// are not mistaken for fabrications.
//
// "50k", "50,000" and "50000" are one claim; "35%" and "35 %" are one claim. Getting
// this wrong in the lenient direction lets a fabricated number through, and in the
// strict direction rejects honest re-wording until whoever maintains this loosens the
// check — which is how a real fabrication gets through later. So the scale suffix is
// resolved rather than compared as text.
func normaliseMetric(m string) string {
	lower := strings.ToLower(strings.TrimSpace(m))

	isPercent := strings.Contains(lower, "%")

	multiplier := 1.0
	switch {
	case strings.HasSuffix(lower, "bn"):
		multiplier, lower = 1e9, strings.TrimSuffix(lower, "bn")
	case strings.HasSuffix(lower, "b"):
		multiplier, lower = 1e9, strings.TrimSuffix(lower, "b")
	case strings.HasSuffix(lower, "m"):
		multiplier, lower = 1e6, strings.TrimSuffix(lower, "m")
	case strings.HasSuffix(lower, "k"):
		multiplier, lower = 1e3, strings.TrimSuffix(lower, "k")
	}

	// Strip everything that is not part of the number. A comma is a thousands
	// separator here; a full stop is a decimal point.
	var digits strings.Builder
	for _, r := range lower {
		switch {
		case unicode.IsDigit(r):
			digits.WriteRune(r)
		case r == '.':
			digits.WriteRune(r)
		}
	}
	if digits.Len() == 0 {
		return ""
	}

	value, err := strconv.ParseFloat(digits.String(), 64)
	if err != nil {
		// Unparseable, so compare the raw digits rather than silently treating it as
		// absent — absent would mean "no figure here", which is the lenient direction.
		return digits.String()
	}
	value *= multiplier

	canonical := strconv.FormatFloat(value, 'f', -1, 64)
	if isPercent {
		canonical += "%"
	}
	return canonical
}

// sourceProse returns every free-text field a figure may legitimately come from,
// including the dates, which contain years.
func sourceProse(source *ParsedData) []string {
	texts := []string{source.Summary, source.Contact.Headline}
	for _, role := range source.Experience {
		texts = append(texts, role.Title, role.Employer, role.Description,
			role.StartDate, role.EndDate)
		texts = append(texts, role.Achievements...)
	}
	for _, study := range source.Education {
		texts = append(texts, study.Qualification, study.Field, study.Institution,
			study.Grade, study.StartDate, study.EndDate)
	}
	for _, cert := range source.Certifications {
		texts = append(texts, cert.Name, cert.Issuer, cert.IssuedDate)
	}
	return texts
}

// tailoredProse returns the free text a tailored CV produced, keyed by field.
func tailoredProse(tailored *ParsedData) map[string]string {
	out := map[string]string{
		"summary":          tailored.Summary,
		"contact.headline": tailored.Contact.Headline,
	}
	for i, role := range tailored.Experience {
		out[fmt.Sprintf("experience[%d].description", i)] = role.Description
		for j, achievement := range role.Achievements {
			out[fmt.Sprintf("experience[%d].achievements[%d]", i, j)] = achievement
		}
	}
	return out
}

// sourceDates collects every date string the source states.
func sourceDates(source *ParsedData) []string {
	var dates []string
	for _, role := range source.Experience {
		dates = append(dates, role.StartDate, role.EndDate)
	}
	for _, study := range source.Education {
		dates = append(dates, study.StartDate, study.EndDate)
	}
	for _, cert := range source.Certifications {
		dates = append(dates, cert.IssuedDate)
	}
	return dates
}

// checkValue reports whether a tailored value is supported by any source value.
//
// An empty tailored value is always fine: omitting is not fabricating.
func checkValue(value string, allowed []string, kind ViolationKind, field, detail string) (Violation, bool) {
	if strings.TrimSpace(value) == "" {
		return Violation{}, true
	}
	for _, candidate := range allowed {
		if supports(candidate, value) {
			return Violation{}, true
		}
	}
	return Violation{Kind: kind, Field: field, Value: value, Detail: detail}, false
}

// supports reports whether a source value backs a tailored one.
//
// True when they are equal after normalisation, or when the tailored value is a
// shortening of the source: every one of its words appears in the source, in order.
// The asymmetry is the whole point. "Senior Engineer" from "Senior Backend Engineer"
// claims less than the source and is safe. "Senior Backend Engineer" from "Senior
// Engineer" adds a claim about backend work and is not.
func supports(source, tailored string) bool {
	src := normaliseWords(source)
	out := normaliseWords(tailored)

	if len(out) == 0 {
		return true
	}
	if len(src) == 0 {
		return false
	}
	return isSubsequence(out, src)
}

// isSubsequence reports whether every word of want appears in have, in order.
func isSubsequence(want, have []string) bool {
	i := 0
	for _, w := range have {
		if i < len(want) && want[i] == w {
			i++
		}
	}
	return i == len(want)
}

// normaliseWords lowercases and splits a value into comparable words, dropping
// punctuation so "Acme Corp." and "acme corp" agree.
func normaliseWords(s string) []string {
	fields := strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '+' && r != '#'
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if f != "" {
			out = append(out, f)
		}
	}
	return out
}
