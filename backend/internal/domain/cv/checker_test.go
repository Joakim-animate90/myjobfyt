package cv_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Joakim-animate90/myjobfyt/backend/internal/domain/cv"
)

// goodCV is a CV with nothing obviously wrong with it, so a test that expects a finding
// is reporting the one thing it changed.
func goodCV() *cv.ParsedData {
	return &cv.ParsedData{
		Summary: "Backend engineer with six years building payment services in Go.",
		Contact: cv.Contact{
			FullName: "Joakim Candidate", Email: "joakim@example.com",
			Phone: "+254 700 000000", Location: "Nairobi, Kenya",
			Headline: "Senior Backend Engineer",
		},
		Experience: []cv.Role{
			{Title: "Senior Backend Engineer", Employer: "Acme Corporation",
				StartDate: "Jan 2021", EndDate: "Present", IsCurrent: true,
				Description:  "Owned the payments platform.",
				Achievements: []string{"Cut checkout latency by 35%", "Led a team of 4"}},
			{Title: "Backend Engineer", Employer: "Beta Payments Ltd",
				StartDate: "Mar 2018", EndDate: "Dec 2020",
				Description:  "Maintained a Go monolith.",
				Achievements: []string{"Migrated 50k users with no downtime"}},
		},
		Education: []cv.Study{{Qualification: "BSc", Field: "Computer Science",
			Institution: "University of Nairobi", StartDate: "2014", EndDate: "2017"}},
	}
}

// goodText is extracted text consistent with goodCV.
func goodText() cv.ExtractedText {
	body := strings.Repeat("Delivered payment services in Go and PostgreSQL across teams. ", 60)
	return cv.ExtractedText{Text: body, CharCount: len(body), PageCount: 2}
}

func findingsFor(report cv.CheckReport, category cv.CheckCategory) []cv.Finding {
	var out []cv.Finding
	for _, f := range report.Findings {
		if f.Category == category {
			out = append(out, f)
		}
	}
	return out
}

func scoreFor(t *testing.T, report cv.CheckReport, category cv.CheckCategory) cv.CategoryScore {
	t.Helper()
	for _, c := range report.Categories {
		if c.Category == category {
			return c
		}
	}
	t.Fatalf("no score reported for %s", category)
	return cv.CategoryScore{}
}

func TestTheSameCVAlwaysProducesTheSameReport(t *testing.T) {
	// A score that moves between refreshes is a score nobody can act on, and the
	// obvious way for it to happen in Go is ranging a map. Running the checker many
	// times and comparing the serialised reports catches that.
	first, err := json.Marshal(cv.CheckCV(goodCV(), goodText()))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for i := 0; i < 50; i++ {
		next, err := json.Marshal(cv.CheckCV(goodCV(), goodText()))
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if string(next) != string(first) {
			t.Fatalf("run %d produced a different report:\n%s\n%s", i, first, next)
		}
	}
}

func TestFindingOrderIsStableAcrossRuns(t *testing.T) {
	// Specifically the ordering, on a CV with many findings of several severities, so
	// ties are actually exercised.
	broken := &cv.ParsedData{
		Experience: []cv.Role{
			{Title: "Engineer", Employer: "A", StartDate: "2020-01", EndDate: "2019-01"},
			{Title: "Engineer", Employer: "B", StartDate: "Jan 2018"},
			{Title: "Engineer", Employer: "C"},
		},
	}
	text := cv.ExtractedText{Text: strings.Repeat("word ", 50), CharCount: 250, PageCount: 1}

	var reference []string
	for run := 0; run < 30; run++ {
		report := cv.CheckCV(broken, text)
		var order []string
		for _, f := range report.Findings {
			order = append(order, string(f.Severity)+"|"+string(f.Category)+"|"+f.Location)
		}
		if run == 0 {
			reference = order
			continue
		}
		if strings.Join(order, ",") != strings.Join(reference, ",") {
			t.Fatalf("finding order changed on run %d:\nwant %v\ngot  %v", run, reference, order)
		}
	}
}

func TestSubScoresAreReportedPerCategory(t *testing.T) {
	report := cv.CheckCV(goodCV(), goodText())

	if len(report.Categories) == 0 {
		t.Fatal("no category scores reported")
	}
	seen := map[cv.CheckCategory]bool{}
	for _, c := range report.Categories {
		seen[c.Category] = true
		if c.Weight <= 0 {
			t.Errorf("%s has weight %d", c.Category, c.Weight)
		}
		if c.Assessed && (c.Score < 0 || c.Score > 100) {
			t.Errorf("%s scored %d, outside 0-100", c.Category, c.Score)
		}
		if !c.Assessed && c.Reason == "" {
			t.Errorf("%s was not assessed but gives no reason", c.Category)
		}
	}
	for _, required := range []cv.CheckCategory{
		cv.CategoryContact, cv.CategoryStructure, cv.CategoryImpact,
		cv.CategoryConsistency, cv.CategoryParseability, cv.CategoryLength,
	} {
		if !seen[required] {
			t.Errorf("no score for %s", required)
		}
	}
}

func TestEveryRecommendationNamesAPlaceInTheCV(t *testing.T) {
	// "Add more detail" is advice a candidate cannot act on. "The Acme role lists no
	// achievements" is. Every finding must therefore carry a location and a suggestion.
	for name, parsed := range map[string]*cv.ParsedData{
		"good CV":    goodCV(),
		"empty CV":   {},
		"no contact": {Experience: goodCV().Experience},
	} {
		t.Run(name, func(t *testing.T) {
			report := cv.CheckCV(parsed, goodText())
			for _, f := range report.Findings {
				if strings.TrimSpace(f.Location) == "" {
					t.Errorf("finding %q names no location", f.Message)
				}
				if strings.TrimSpace(f.Suggestion) == "" {
					t.Errorf("finding %q offers no suggestion", f.Message)
				}
				if strings.TrimSpace(f.Message) == "" {
					t.Errorf("finding at %q has no message", f.Location)
				}
			}
		})
	}
}

func TestAMissingEmailIsCritical(t *testing.T) {
	// An employer who cannot reach the candidate is the one failure that makes
	// everything else irrelevant.
	parsed := goodCV()
	parsed.Contact.Email = ""

	report := cv.CheckCV(parsed, goodText())
	findings := findingsFor(report, cv.CategoryContact)
	if len(findings) == 0 {
		t.Fatal("a CV with no email produced no contact finding")
	}
	if findings[0].Severity != cv.SeverityCritical {
		t.Errorf("severity = %s, want critical", findings[0].Severity)
	}
	if scoreFor(t, report, cv.CategoryContact).Score >= 100 {
		t.Error("the contact score was not reduced")
	}
}

func TestAScannedDocumentFailsParseabilityOutright(t *testing.T) {
	// An ATS reads nothing from it, so the application is rejected before a human sees
	// it. Nothing else about the CV matters.
	report := cv.CheckCV(goodCV(), cv.ExtractedText{Text: "  ", CharCount: 2, PageCount: 2})

	score := scoreFor(t, report, cv.CategoryParseability)
	if score.Score != 0 {
		t.Errorf("parseability = %d, want 0", score.Score)
	}
	findings := findingsFor(report, cv.CategoryParseability)
	if len(findings) == 0 || findings[0].Severity != cv.SeverityCritical {
		t.Fatalf("expected a critical parseability finding, got %v", findings)
	}
	if !strings.Contains(strings.ToLower(findings[0].Message), "scan") {
		t.Errorf("the message does not explain the likely cause: %q", findings[0].Message)
	}
}

func TestARoleWithNoAchievementsIsReportedAgainstThatRole(t *testing.T) {
	parsed := goodCV()
	parsed.Experience[0].Achievements = nil

	report := cv.CheckCV(parsed, goodText())
	findings := findingsFor(report, cv.CategoryImpact)
	if len(findings) == 0 {
		t.Fatal("no impact finding for a role with no achievements")
	}
	if !strings.Contains(findings[0].Location, "Acme Corporation") {
		t.Errorf("the finding does not name the role it is about: %q", findings[0].Location)
	}
}

func TestUnquantifiedAchievementsAreOnlyMinor(t *testing.T) {
	// Pushing candidates to add numbers they cannot support would be the product
	// encouraging the exact fabrication the rest of the system refuses. Worth
	// mentioning, never worth a critical.
	parsed := goodCV()
	for i := range parsed.Experience {
		parsed.Experience[i].Achievements = []string{"Improved the deployment process"}
	}

	report := cv.CheckCV(parsed, goodText())
	for _, f := range findingsFor(report, cv.CategoryImpact) {
		if f.Severity == cv.SeverityCritical {
			t.Errorf("a missing number was reported as critical: %q", f.Message)
		}
	}
	for _, f := range findingsFor(report, cv.CategoryImpact) {
		if strings.Contains(f.Suggestion, "stand behind") {
			return // the suggestion warns against inventing figures
		}
	}
	t.Error("the suggestion does not warn against inventing figures the candidate cannot support")
}

func TestReversedDatesAreReported(t *testing.T) {
	parsed := goodCV()
	parsed.Experience[1].StartDate = "Dec 2020"
	parsed.Experience[1].EndDate = "Mar 2018"

	report := cv.CheckCV(parsed, goodText())
	for _, f := range findingsFor(report, cv.CategoryConsistency) {
		if strings.Contains(f.Message, "before it starts") {
			return
		}
	}
	t.Errorf("a role ending before it starts was not reported: %v", findingsFor(report, cv.CategoryConsistency))
}

func TestMixedDateFormatsAreReported(t *testing.T) {
	parsed := goodCV()
	parsed.Experience[0].StartDate = "Jan 2021"
	parsed.Experience[1].StartDate = "2018-03"
	parsed.Experience[1].EndDate = "2020-12"

	report := cv.CheckCV(parsed, goodText())
	for _, f := range findingsFor(report, cv.CategoryConsistency) {
		if strings.Contains(f.Message, "different formats") {
			return
		}
	}
	t.Error("mixed date formats were not reported")
}

func TestConsistentDateFormatsAreNotReported(t *testing.T) {
	// The flip side: "Jan 2021" and "Mar 2018" are the same shape, and flagging them
	// would make the check noise that candidates learn to ignore.
	report := cv.CheckCV(goodCV(), goodText())
	for _, f := range findingsFor(report, cv.CategoryConsistency) {
		if strings.Contains(f.Message, "different formats") {
			t.Errorf("consistent dates were reported as mixed: %q", f.Message)
		}
	}
}

func TestAnUnassessableCategoryIsNotScoredZero(t *testing.T) {
	// A zero is a judgement. Claiming one we did not make is a fabricated score, which
	// MJF-008 forbids outright.
	parsed := &cv.ParsedData{Contact: cv.Contact{FullName: "A", Email: "a@example.com"}}

	report := cv.CheckCV(parsed, goodText())
	impact := scoreFor(t, report, cv.CategoryImpact)

	if impact.Assessed {
		t.Fatal("impact was assessed for a CV with no employment history")
	}
	if impact.Score != 0 || impact.Reason == "" {
		t.Errorf("an unassessed category should carry a reason and no claim: %+v", impact)
	}
	// And it must not drag the overall score down as though it had scored zero.
	if report.Overall == 0 {
		t.Error("the overall score was dragged to zero by a category that was never assessed")
	}
}

func TestADeterministicOnlyReportDeclaresWhatIsMissing(t *testing.T) {
	// The degraded-mode requirement. Keyword coverage needs a target role and a model;
	// with neither, the report must say so rather than present a partial score as whole.
	report := cv.CheckCV(goodCV(), goodText())

	if !report.Degraded {
		t.Error("a report with no AI pass does not declare itself incomplete")
	}
	var sawKeywords bool
	for _, missing := range report.Missing {
		if missing == cv.CategoryKeywords {
			sawKeywords = true
		}
	}
	if !sawKeywords {
		t.Errorf("Missing does not name keyword coverage: %v", report.Missing)
	}
	if report.Coverage >= 100 {
		t.Errorf("coverage = %d, want less than 100 when a category was not assessed", report.Coverage)
	}
	if report.Coverage <= 0 {
		t.Errorf("coverage = %d, want the share actually assessed", report.Coverage)
	}
}

func TestCoverageReflectsTheWeightActuallyAssessed(t *testing.T) {
	// Coverage is what lets a consumer tell a 76%-assessed report from a complete one.
	full := cv.CheckCV(goodCV(), goodText())
	sparse := cv.CheckCV(&cv.ParsedData{Contact: cv.Contact{Email: "a@example.com"}}, goodText())

	if sparse.Coverage >= full.Coverage {
		t.Errorf("a CV with fewer assessable categories reported coverage %d, not below %d",
			sparse.Coverage, full.Coverage)
	}
}

func TestAnUnparsedCVIsReportedAsSuchRatherThanScored(t *testing.T) {
	report := cv.CheckCV(nil, cv.ExtractedText{})

	if !report.Degraded {
		t.Error("an unparsed CV produced a report that does not declare itself incomplete")
	}
	if report.Overall != 0 || report.Coverage != 0 {
		t.Errorf("an unparsed CV was given a score: overall=%d coverage=%d", report.Overall, report.Coverage)
	}
	if len(report.Findings) == 0 {
		t.Fatal("no finding explains why nothing could be assessed")
	}
	if report.Findings[0].Suggestion == "" {
		t.Error("the finding offers the candidate nothing to do")
	}
}

func TestAGoodCVScoresWell(t *testing.T) {
	// The rubric has to be able to say "this is fine". A checker that always finds
	// something teaches candidates to ignore it.
	report := cv.CheckCV(goodCV(), goodText())

	if report.Overall < 80 {
		t.Errorf("a sound CV scored %d; findings: %v", report.Overall, report.Findings)
	}
	for _, f := range report.Findings {
		if f.Severity == cv.SeverityCritical {
			t.Errorf("a sound CV produced a critical finding: %q at %s", f.Message, f.Location)
		}
	}
}

func TestTheRubricVersionIsRecorded(t *testing.T) {
	// Without it a score cannot be compared with one from last month, and a rubric
	// change looks to the candidate like their CV got worse.
	if cv.CheckCV(goodCV(), goodText()).RubricVersion == "" {
		t.Error("no rubric version recorded on the report")
	}
}
