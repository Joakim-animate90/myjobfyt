package cv

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// CheckCategory is one dimension of CV quality.
//
// Reported separately rather than folded into a single number, because "your CV scores
// 62" tells a candidate nothing they can act on, while "your contact details are
// incomplete and three roles have no achievements" tells them exactly what to do.
type CheckCategory string

const (
	// CategoryContact covers whether an employer can actually reach the candidate.
	CategoryContact CheckCategory = "contact"
	// CategoryStructure covers the sections a reader and an ATS both expect.
	CategoryStructure CheckCategory = "structure"
	// CategoryLength covers whether the CV is a readable length.
	CategoryLength CheckCategory = "length"
	// CategoryConsistency covers date formats and chronology.
	CategoryConsistency CheckCategory = "consistency"
	// CategoryImpact covers whether achievements are stated and quantified.
	CategoryImpact CheckCategory = "impact"
	// CategoryParseability covers whether an ATS can read the document at all.
	CategoryParseability CheckCategory = "parseability"
	// CategoryKeywords covers coverage of a target role's vocabulary. Assessed only by
	// the AI pass, so it is absent from a deterministic-only report rather than scored
	// as zero.
	CategoryKeywords CheckCategory = "keywords"
)

// Severity ranks a finding.
type Severity string

const (
	// SeverityCritical means an employer or an ATS may discard the CV outright.
	SeverityCritical Severity = "critical"
	// SeverityImportant means it materially weakens the application.
	SeverityImportant Severity = "important"
	// SeverityMinor means it is worth fixing but costs little as it stands.
	SeverityMinor Severity = "minor"
)

// Finding is one specific, located problem.
//
// Location is mandatory. MJF-008 requires every recommendation to name a place in the
// CV, because "add more detail" is advice a candidate cannot act on while "the Acme
// role lists no achievements" is.
type Finding struct {
	Category   CheckCategory
	Severity   Severity
	Location   string
	Message    string
	Suggestion string
}

// CategoryScore is one category's result.
type CategoryScore struct {
	Category CheckCategory
	// Score is 0–100 within this category.
	Score int
	// Weight is this category's share of the overall score.
	Weight int
	// Assessed is false when the category could not be evaluated — the AI pass was
	// unavailable, or the CV lacked the data. An unassessed category is reported as
	// such and excluded from the overall score, never scored as zero: a zero is a
	// judgement, and claiming one we did not make is a fabricated score.
	Assessed bool
	// Reason explains a category that was not assessed.
	Reason string
}

// CheckReport is a CV's full assessment.
type CheckReport struct {
	// Overall is the weighted mean of the assessed categories only.
	Overall int
	// Coverage is the share of total weight that was actually assessed, 0–100. A report
	// at 76% coverage is a different claim from one at 100%, and the consumer must be
	// able to tell.
	Coverage   int
	Categories []CategoryScore
	Findings   []Finding
	// Degraded is true when some categories could not be assessed. The response states
	// what is missing rather than quietly presenting a partial score as a whole one.
	Degraded bool
	// Missing names the categories that were not assessed, for the candidate.
	Missing []CheckCategory
	// RubricVersion identifies the scoring rules that produced this. Without it, a
	// score cannot be compared with one from last month, and a rubric change looks
	// like the candidate's CV got worse.
	RubricVersion string
}

// rubricVersion identifies the deterministic rubric below. Bump it whenever a weight,
// threshold or rule changes.
const rubricVersion = "cv-check-v1"

// categoryWeights is the scoring rubric.
//
// Parseability and structure dominate because a CV an ATS cannot read is not a weak
// application, it is a rejected one. Length is weighted lightly: it is the thing
// candidates most often obsess over and the thing that matters least.
var categoryWeights = map[CheckCategory]int{
	CategoryParseability: 20,
	CategoryStructure:    20,
	CategoryImpact:       20,
	CategoryContact:      15,
	CategoryConsistency:  15,
	CategoryKeywords:     10,
	CategoryLength:       10,
}

// orderedCategories fixes the reporting order.
//
// Explicit rather than derived from the weights map: ranging a map is randomised in Go,
// and a report whose category order changed between identical runs would break the
// determinism this feature promises.
var orderedCategories = []CheckCategory{
	CategoryParseability, CategoryStructure, CategoryImpact,
	CategoryContact, CategoryConsistency, CategoryKeywords, CategoryLength,
}

// CheckCV runs the deterministic rules over a parsed CV.
//
// Pure: the same CV always produces the same report, which is what makes a score worth
// showing. Nothing here calls a model, so a score exists whether or not the AI provider
// is reachable.
func CheckCV(parsed *ParsedData, text ExtractedText) CheckReport {
	report := CheckReport{RubricVersion: rubricVersion}

	if parsed == nil {
		return CheckReport{
			RubricVersion: rubricVersion,
			Degraded:      true,
			Missing:       orderedCategories,
			Findings: []Finding{{
				Category: CategoryParseability, Severity: SeverityCritical,
				Location: "document",
				Message:  "This CV has not been parsed yet, so it cannot be assessed.",
				Suggestion: "Wait for processing to finish, or upload the CV again if it " +
					"has been stuck for more than a few minutes.",
			}},
		}
	}

	scores := map[CheckCategory]CategoryScore{}
	var findings []Finding

	for _, check := range []func(*ParsedData, ExtractedText) (CategoryScore, []Finding){
		checkParseability, checkStructure, checkImpact,
		checkContact, checkConsistency, checkLength,
	} {
		score, found := check(parsed, text)
		scores[score.Category] = score
		findings = append(findings, found...)
	}

	// Keywords needs a target role and a model, neither of which this layer has.
	scores[CategoryKeywords] = CategoryScore{
		Category: CategoryKeywords,
		Weight:   categoryWeights[CategoryKeywords],
		Assessed: false,
		Reason:   "keyword coverage is assessed against a specific role and was not run",
	}

	for _, category := range orderedCategories {
		score := scores[category]
		report.Categories = append(report.Categories, score)
		if !score.Assessed {
			report.Degraded = true
			report.Missing = append(report.Missing, category)
		}
	}

	report.Overall, report.Coverage = aggregate(report.Categories)
	report.Findings = sortFindings(findings)
	return report
}

// aggregate computes the overall score from the assessed categories only.
//
// Excluding unassessed categories rather than scoring them zero is the difference
// between an honest partial score and a fabricated one. Coverage is reported alongside
// so the consumer can see which it is.
func aggregate(categories []CategoryScore) (overall, coverage int) {
	var assessedWeight, totalWeight, weighted int
	for _, c := range categories {
		totalWeight += c.Weight
		if !c.Assessed {
			continue
		}
		assessedWeight += c.Weight
		weighted += c.Score * c.Weight
	}
	if assessedWeight == 0 || totalWeight == 0 {
		return 0, 0
	}
	return weighted / assessedWeight, assessedWeight * 100 / totalWeight
}

// sortFindings puts the most serious first, then orders deterministically.
//
// Severity, then category, then location, then message: enough keys that two findings
// can never tie and leave the order to chance, which would break the snapshot test and,
// worse, make the same CV look different on a refresh.
func sortFindings(findings []Finding) []Finding {
	rank := map[Severity]int{SeverityCritical: 0, SeverityImportant: 1, SeverityMinor: 2}
	sort.SliceStable(findings, func(i, j int) bool {
		a, b := findings[i], findings[j]
		if rank[a.Severity] != rank[b.Severity] {
			return rank[a.Severity] < rank[b.Severity]
		}
		if a.Category != b.Category {
			return a.Category < b.Category
		}
		if a.Location != b.Location {
			return a.Location < b.Location
		}
		return a.Message < b.Message
	})
	return findings
}

// --- the rules ------------------------------------------------------------------

// checkParseability asks whether an ATS can read the document at all.
func checkParseability(parsed *ParsedData, text ExtractedText) (CategoryScore, []Finding) {
	score := CategoryScore{Category: CategoryParseability, Weight: categoryWeights[CategoryParseability], Assessed: true, Score: 100}
	var findings []Finding

	if !text.HasUsableText() {
		score.Score = 0
		findings = append(findings, Finding{
			Category: CategoryParseability, Severity: SeverityCritical, Location: "document",
			Message: "Almost no text could be read from this document, which usually means it is a scan or an image.",
			Suggestion: "Export the CV directly from Word, Google Docs or a similar tool " +
				"rather than scanning or photographing a printed copy.",
		})
		return score, findings
	}

	if text.PageCount > 3 {
		score.Score -= 20
		findings = append(findings, Finding{
			Category: CategoryParseability, Severity: SeverityImportant,
			Location: "document",
			Message:  fmt.Sprintf("This CV is %d pages. Most recruiters read the first page and skim the second.", text.PageCount),
			Suggestion: "Cut to two pages by removing roles over ten years old and " +
				"shortening older entries to a single line.",
		})
	}

	// Very little text across many pages means the content is probably trapped in
	// tables, text boxes or images, which parse unreliably.
	if text.PageCount > 0 && text.CharCount/text.PageCount < 400 {
		score.Score -= 30
		findings = append(findings, Finding{
			Category: CategoryParseability, Severity: SeverityImportant,
			Location: "document",
			Message: "Very little readable text was found for the number of pages, which usually " +
				"means content is inside tables, text boxes or images.",
			Suggestion: "Move the content into ordinary paragraphs and bullet lists. " +
				"Applicant tracking systems frequently drop anything inside a text box.",
		})
	}

	return clamp(score), findings
}

// checkStructure asks whether the sections a reader and an ATS expect are present.
func checkStructure(parsed *ParsedData, _ ExtractedText) (CategoryScore, []Finding) {
	score := CategoryScore{Category: CategoryStructure, Weight: categoryWeights[CategoryStructure], Assessed: true, Score: 100}
	var findings []Finding

	if len(parsed.Experience) == 0 {
		score.Score -= 50
		findings = append(findings, Finding{
			Category: CategoryStructure, Severity: SeverityCritical, Location: "experience",
			Message:    "No employment history could be found.",
			Suggestion: "Add a section listing each role with its job title, employer and dates.",
		})
	}
	if len(parsed.Education) == 0 {
		score.Score -= 15
		findings = append(findings, Finding{
			Category: CategoryStructure, Severity: SeverityMinor, Location: "education",
			Message:    "No education section could be found.",
			Suggestion: "Add your qualifications, even briefly. Some employers filter on them.",
		})
	}
	if strings.TrimSpace(parsed.Summary) == "" {
		score.Score -= 20
		findings = append(findings, Finding{
			Category: CategoryStructure, Severity: SeverityImportant, Location: "summary",
			Message: "There is no summary at the top of the CV.",
			Suggestion: "Add two or three sentences saying what you do and what you are " +
				"looking for. It is the only part many recruiters read in full.",
		})
	}
	if strings.TrimSpace(parsed.Contact.Headline) == "" {
		score.Score -= 10
		findings = append(findings, Finding{
			Category: CategoryStructure, Severity: SeverityMinor, Location: "headline",
			Message:    "There is no headline or current title under your name.",
			Suggestion: "Add your current or target job title directly under your name.",
		})
	}

	return clamp(score), findings
}

// quantified matches a figure in an achievement.
var quantified = regexp.MustCompile(`\d`)

// checkImpact asks whether the CV says what the candidate achieved, not only what they
// were responsible for.
func checkImpact(parsed *ParsedData, _ ExtractedText) (CategoryScore, []Finding) {
	score := CategoryScore{Category: CategoryImpact, Weight: categoryWeights[CategoryImpact], Assessed: true, Score: 100}
	var findings []Finding

	if len(parsed.Experience) == 0 {
		return CategoryScore{
			Category: CategoryImpact, Weight: categoryWeights[CategoryImpact],
			Assessed: false,
			Reason:   "there is no employment history to assess",
		}, nil
	}

	var withAchievements, withNumbers int
	for i, role := range parsed.Experience {
		location := fmt.Sprintf("experience[%d] (%s at %s)", i, role.Title, role.Employer)

		if len(role.Achievements) == 0 && strings.TrimSpace(role.Description) == "" {
			findings = append(findings, Finding{
				Category: CategoryImpact, Severity: SeverityImportant, Location: location,
				Message:    "This role has no description and no achievements.",
				Suggestion: "Add two or three bullets describing what you did and what changed as a result.",
			})
			continue
		}
		if len(role.Achievements) == 0 {
			findings = append(findings, Finding{
				Category: CategoryImpact, Severity: SeverityImportant, Location: location,
				Message: "This role describes responsibilities but no achievements.",
				Suggestion: "Add what changed because you were there — something delivered, " +
					"improved, reduced or built.",
			})
			continue
		}
		withAchievements++

		if anyMatch(role.Achievements, quantified) {
			withNumbers++
		} else {
			findings = append(findings, Finding{
				Category: CategoryImpact, Severity: SeverityMinor, Location: location,
				Message: "None of the achievements for this role include a figure.",
				Suggestion: "Where you can, say how much: a percentage, a count, a " +
					"timescale. Only use numbers you can stand behind in an interview.",
			})
		}
	}

	total := len(parsed.Experience)
	// Two thirds of the category is having achievements at all, one third is
	// quantifying them. Having none is the far bigger problem.
	score.Score = (withAchievements*67)/total + (withNumbers*33)/total

	return clamp(score), findings
}

// checkContact asks whether an employer can reach the candidate.
func checkContact(parsed *ParsedData, _ ExtractedText) (CategoryScore, []Finding) {
	score := CategoryScore{Category: CategoryContact, Weight: categoryWeights[CategoryContact], Assessed: true, Score: 100}
	var findings []Finding

	if strings.TrimSpace(parsed.Contact.Email) == "" {
		score.Score -= 50
		findings = append(findings, Finding{
			Category: CategoryContact, Severity: SeverityCritical, Location: "contact.email",
			Message:    "No email address could be found.",
			Suggestion: "Add an email address near the top of the CV, as plain text rather than an image.",
		})
	}
	if strings.TrimSpace(parsed.Contact.FullName) == "" {
		score.Score -= 25
		findings = append(findings, Finding{
			Category: CategoryContact, Severity: SeverityCritical, Location: "contact.full_name",
			Message:    "No name could be found.",
			Suggestion: "Put your full name at the top as ordinary text, not inside a header or an image.",
		})
	}
	if strings.TrimSpace(parsed.Contact.Phone) == "" {
		score.Score -= 15
		findings = append(findings, Finding{
			Category: CategoryContact, Severity: SeverityImportant, Location: "contact.phone",
			Message:    "No phone number could be found.",
			Suggestion: "Add a phone number. Many recruiters call before they email.",
		})
	}
	if strings.TrimSpace(parsed.Contact.Location) == "" {
		score.Score -= 10
		findings = append(findings, Finding{
			Category: CategoryContact, Severity: SeverityMinor, Location: "contact.location",
			Message:    "No location could be found.",
			Suggestion: "Add at least a city and country. Many searches filter on location.",
		})
	}

	return clamp(score), findings
}

// checkConsistency asks whether the dates make sense and are written the same way
// throughout.
func checkConsistency(parsed *ParsedData, _ ExtractedText) (CategoryScore, []Finding) {
	if len(parsed.Experience) == 0 {
		return CategoryScore{
			Category: CategoryConsistency, Weight: categoryWeights[CategoryConsistency],
			Assessed: false, Reason: "there are no dated entries to compare",
		}, nil
	}

	score := CategoryScore{Category: CategoryConsistency, Weight: categoryWeights[CategoryConsistency], Assessed: true, Score: 100}
	var findings []Finding

	shapes := map[string]bool{}
	for i, role := range parsed.Experience {
		location := fmt.Sprintf("experience[%d] (%s at %s)", i, role.Title, role.Employer)

		for _, raw := range []string{role.StartDate, role.EndDate} {
			if shape := dateShape(raw); shape != "" {
				shapes[shape] = true
			}
		}

		if strings.TrimSpace(role.StartDate) == "" {
			score.Score -= 20
			findings = append(findings, Finding{
				Category: CategoryConsistency, Severity: SeverityImportant, Location: location,
				Message:    "This role has no start date.",
				Suggestion: "Add the month and year you started. Unexplained gaps invite questions.",
			})
			continue
		}

		start, okStart := parseCVDate(role.StartDate)
		end, okEnd := parseCVDate(role.EndDate)
		if okStart && okEnd && absoluteMonth(end) < absoluteMonth(start) {
			score.Score -= 25
			findings = append(findings, Finding{
				Category: CategoryConsistency, Severity: SeverityImportant, Location: location,
				Message:    fmt.Sprintf("This role ends (%s) before it starts (%s).", role.EndDate, role.StartDate),
				Suggestion: "Correct the dates. A reader will assume it is careless rather than a typo.",
			})
		}
	}

	if len(shapes) > 1 {
		score.Score -= 15
		written := make([]string, 0, len(shapes))
		for shape := range shapes {
			written = append(written, shape)
		}
		sort.Strings(written) // deterministic message
		findings = append(findings, Finding{
			Category: CategoryConsistency, Severity: SeverityMinor, Location: "experience",
			Message: fmt.Sprintf("Dates are written in %d different formats (%s).",
				len(shapes), strings.Join(written, ", ")),
			Suggestion: "Pick one format and use it throughout. Mixed formats read as carelessness " +
				"and some parsers only recognise one of them.",
		})
	}

	return clamp(score), findings
}

// checkLength asks whether the CV is a readable length.
func checkLength(parsed *ParsedData, text ExtractedText) (CategoryScore, []Finding) {
	if !text.HasUsableText() {
		return CategoryScore{
			Category: CategoryLength, Weight: categoryWeights[CategoryLength],
			Assessed: false, Reason: "the document's text could not be read",
		}, nil
	}

	score := CategoryScore{Category: CategoryLength, Weight: categoryWeights[CategoryLength], Assessed: true, Score: 100}
	var findings []Finding

	words := len(strings.Fields(text.Text))
	switch {
	case words < 200:
		score.Score = 40
		findings = append(findings, Finding{
			Category: CategoryLength, Severity: SeverityImportant, Location: "document",
			Message:    fmt.Sprintf("This CV is about %d words, which is very short.", words),
			Suggestion: "Add detail to your recent roles: what you worked on and what resulted from it.",
		})
	case words > 1200:
		score.Score = 60
		findings = append(findings, Finding{
			Category: CategoryLength, Severity: SeverityMinor, Location: "document",
			Message:    fmt.Sprintf("This CV is about %d words, which is long for most applications.", words),
			Suggestion: "Trim older roles to one or two lines so the recent, relevant work stands out.",
		})
	}

	return clamp(score), findings
}

// --- helpers --------------------------------------------------------------------

// clamp keeps a score inside 0–100. Deductions can otherwise take it negative, and a
// negative sub-score is not something a candidate can interpret.
func clamp(score CategoryScore) CategoryScore {
	if score.Score < 0 {
		score.Score = 0
	}
	if score.Score > 100 {
		score.Score = 100
	}
	return score
}

func anyMatch(items []string, re *regexp.Regexp) bool {
	for _, item := range items {
		if re.MatchString(item) {
			return true
		}
	}
	return false
}

// dateShape reduces a written date to its format, so "Jan 2019" and "Mar 2020" are one
// shape while "2019-01" is another.
func dateShape(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || isPresent(raw) {
		return ""
	}
	var b strings.Builder
	for _, r := range raw {
		switch {
		case r >= '0' && r <= '9':
			b.WriteByte('9')
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
			b.WriteByte('A')
		default:
			b.WriteRune(r)
		}
	}
	// Collapse runs so "Jan" and "January" are the same shape: the distinction that
	// matters is "month name" versus "month number", not how it is abbreviated.
	return collapseRuns(b.String())
}

func collapseRuns(s string) string {
	var b strings.Builder
	var last rune
	for i, r := range s {
		if i > 0 && r == last && (r == '9' || r == 'A') {
			continue
		}
		b.WriteRune(r)
		last = r
	}
	return b.String()
}
