package cv

// Internal tests: the date parser and the experience sum are where a wrong answer
// becomes a confidently wrong match score, so they are tested directly rather than only
// through the extraction path.

import (
	"fmt"
	"testing"
	"time"
)

func TestParseCVDateAcceptsTheFormsRealCVsUse(t *testing.T) {
	cases := map[string]yearMonth{
		"2019":          {year: 2019},
		"Jan 2019":      {year: 2019, month: 1},
		"January 2019":  {year: 2019, month: 1},
		"Sept 2019":     {year: 2019, month: 9},
		"2019 Jan":      {year: 2019, month: 1},
		"2019-01":       {year: 2019, month: 1},
		"2019/01":       {year: 2019, month: 1},
		"2019-1":        {year: 2019, month: 1},
		"01/2019":       {year: 2019, month: 1},
		"12/2019":       {year: 2019, month: 12},
		"2019-01-15":    {year: 2019, month: 1},
		"  Jan   2019 ": {year: 2019, month: 1},
		"Jan. 2019":     {year: 2019, month: 1},
		"January, 2019": {year: 2019, month: 1},
		"DECEMBER 2021": {year: 2021, month: 12},
	}
	for input, want := range cases {
		t.Run(input, func(t *testing.T) {
			got, ok := parseCVDate(input)
			if !ok {
				t.Fatalf("parseCVDate(%q) failed", input)
			}
			if got != want {
				t.Fatalf("parseCVDate(%q) = %+v, want %+v", input, got, want)
			}
		})
	}
}

func TestParseCVDateRejectsWhatIsNotADate(t *testing.T) {
	// A four-digit number is not necessarily a year. CVs are full of salaries,
	// postcodes, employee counts and version numbers, and reading one as a date would
	// fabricate an employment range.
	for _, input := range []string{
		"", "   ", "Present", "Current", "ongoing", "n/a", "-",
		"1900",           // before the plausible range
		"2500",           // a budget figure, not a year
		"3000",           // far future
		"13/2019",        // no such month
		"2019-13",        // no such month
		"0/2019",         // no such month
		"Managed a team", // prose
		"Summer 2019",    // a season, not a month we can place
		"Q3 2019",        // a quarter, not a month
		"2019 - 2021",    // a range, not a single date
		"about 2019",     // hedged
	} {
		t.Run(fmt.Sprintf("%q", input), func(t *testing.T) {
			if got, ok := parseCVDate(input); ok {
				t.Fatalf("parseCVDate(%q) = %+v, want rejection", input, got)
			}
		})
	}
}

func TestParseCVDateRejectsAFutureYearBeyondTolerance(t *testing.T) {
	// One year ahead is tolerated, because a CV can legitimately list a course
	// finishing next year. Beyond that it is a typo or a fabrication.
	nextYear := fmt.Sprintf("%d", time.Now().UTC().Year()+1)
	if _, ok := parseCVDate(nextYear); !ok {
		t.Errorf("parseCVDate(%q) rejected a plausible near-future year", nextYear)
	}
	farFuture := fmt.Sprintf("%d", time.Now().UTC().Year()+5)
	if _, ok := parseCVDate(farFuture); ok {
		t.Errorf("parseCVDate(%q) accepted a far-future year", farFuture)
	}
}

func TestIsPresent(t *testing.T) {
	for _, input := range []string{"Present", "present", "Current", "NOW", "ongoing", "Today", "to date"} {
		if !isPresent(input) {
			t.Errorf("isPresent(%q) = false", input)
		}
	}
	for _, input := range []string{"", "2019", "Jan 2019", "presently employed"} {
		if isPresent(input) {
			t.Errorf("isPresent(%q) = true, want false", input)
		}
	}
}

func TestRoleDurationIsInclusiveOfTheStartMonth(t *testing.T) {
	// A role from January to January of the same year is one month of work, not zero.
	months := roleDurationMonths(Role{StartDate: "Jan 2019", EndDate: "Jan 2019"})
	if months == nil {
		t.Fatal("duration was nil for a parseable range")
	}
	if *months != 1 {
		t.Fatalf("duration = %d, want 1", *months)
	}

	twelve := roleDurationMonths(Role{StartDate: "Jan 2019", EndDate: "Dec 2019"})
	if twelve == nil || *twelve != 12 {
		t.Fatalf("Jan-Dec 2019 = %v, want 12", twelve)
	}
}

func TestRoleDurationIsNilWhenTheDatesCannotBeRead(t *testing.T) {
	// Nil rather than zero: "we could not tell" and "no experience" are different
	// facts, and experience scoring must not treat them alike.
	cases := map[string]Role{
		"no start":         {EndDate: "2021"},
		"unreadable start": {StartDate: "Summer 2019", EndDate: "2021"},
		"no end":           {StartDate: "2019"},
		"unreadable end":   {StartDate: "2019", EndDate: "recently"},
		"end before start": {StartDate: "Jan 2021", EndDate: "Jan 2019"},
	}
	for name, role := range cases {
		t.Run(name, func(t *testing.T) {
			if got := roleDurationMonths(role); got != nil {
				t.Fatalf("duration = %d, want nil", *got)
			}
		})
	}
}

func TestCurrentRoleRunsToToday(t *testing.T) {
	for _, role := range []Role{
		{StartDate: "Jan 2024", IsCurrent: true},
		{StartDate: "Jan 2024", EndDate: "Present"},
	} {
		months := roleDurationMonths(role)
		if months == nil {
			t.Fatal("a current role produced no duration")
		}
		// Sanity bound rather than an exact figure, so the test does not rot with time.
		if *months < 1 {
			t.Fatalf("duration = %d, want at least 1", *months)
		}
	}
}

func TestYearOnlyRangesAreAnchoredMidYear(t *testing.T) {
	// "2019 - 2021" read as January-to-January is 25 months, which systematically
	// overstates every year-only range on every CV. Mid-year is the least-wrong
	// assumption, and the imprecision is inherent to the source.
	months := roleDurationMonths(Role{StartDate: "2019", EndDate: "2021"})
	if months == nil {
		t.Fatal("duration was nil")
	}
	if *months != 25 {
		t.Logf("year-only 2019-2021 = %d months", *months)
	}
	// What matters is that it is consistent and bounded, not the exact figure.
	if *months < 20 || *months > 30 {
		t.Fatalf("duration = %d, want roughly two years", *months)
	}
}

func TestTotalExperienceMergesOverlappingRoles(t *testing.T) {
	// Summing naively double-counts concurrent roles. A contractor with three
	// simultaneous clients would appear to have three times their real experience, and
	// seniority scoring would place them far above where they belong.
	roles := []Role{
		{StartDate: "Jan 2020", EndDate: "Dec 2021"}, // 24 months
		{StartDate: "Jun 2020", EndDate: "Jun 2021"}, // entirely inside the first
		{StartDate: "Mar 2021", EndDate: "Dec 2021"}, // overlaps the tail of the first
	}

	total := totalExperienceMonths(roles)
	if total == nil {
		t.Fatal("total was nil")
	}
	if *total != 24 {
		t.Fatalf("total = %d, want 24; overlapping roles were double-counted", *total)
	}
}

func TestTotalExperienceCountsSeparatePeriodsSeparately(t *testing.T) {
	roles := []Role{
		{StartDate: "Jan 2015", EndDate: "Dec 2015"}, // 12
		{StartDate: "Jan 2020", EndDate: "Dec 2020"}, // 12, with a four-year gap
	}
	total := totalExperienceMonths(roles)
	if total == nil || *total != 24 {
		t.Fatalf("total = %v, want 24", total)
	}
}

func TestAdjacentRolesCountAsContinuous(t *testing.T) {
	// A role ending in June and the next starting in July is unbroken employment, not
	// a one-month gap.
	roles := []Role{
		{StartDate: "Jan 2020", EndDate: "Jun 2020"},
		{StartDate: "Jul 2020", EndDate: "Dec 2020"},
	}
	total := totalExperienceMonths(roles)
	if total == nil || *total != 12 {
		t.Fatalf("total = %v, want 12", total)
	}
}

func TestTotalExperienceIsNilWhenNothingParses(t *testing.T) {
	roles := []Role{
		{StartDate: "Summer 2019", EndDate: "recently"},
		{StartDate: "", EndDate: ""},
	}
	if got := totalExperienceMonths(roles); got != nil {
		t.Fatalf("total = %d, want nil", *got)
	}
	if got := totalExperienceMonths(nil); got != nil {
		t.Fatalf("total for no roles = %d, want nil", *got)
	}
}

func TestTotalExperienceSkipsUnparseableRolesWithoutLosingTheRest(t *testing.T) {
	roles := []Role{
		{StartDate: "Jan 2020", EndDate: "Dec 2020"}, // 12
		{StartDate: "sometime", EndDate: "later"},    // skipped
	}
	total := totalExperienceMonths(roles)
	if total == nil || *total != 12 {
		t.Fatalf("total = %v, want 12", total)
	}
}

func TestSeniorityFromTitleBeatsDuration(t *testing.T) {
	// A title is the candidate's own statement of their level; duration is our
	// inference. Where they disagree, the title wins, since that is what an employer
	// reads.
	fiveYears := 60
	parsed := &ParsedData{
		Experience:            []Role{{Title: "Head of Engineering"}},
		TotalExperienceMonths: &fiveYears,
	}
	if got := detectSeniority(parsed); got != "Executive" {
		t.Fatalf("seniority = %q, want Executive from the title", got)
	}
}

func TestSeniorityKeywordOrderingHandlesCombinedTitles(t *testing.T) {
	cases := map[string]string{
		"Senior Engineering Manager":  "Senior",
		"Junior Developer":            "Junior",
		"Graduate Software Engineer":  "Junior",
		"Principal Engineer":          "Executive",
		"VP of Engineering":           "Executive",
		"Chief Technology Officer":    "Executive",
		"Staff Software Engineer":     "Senior",
		"Lead Backend Engineer":       "Senior",
		"Software Engineering Intern": "Junior",
		"Solutions Architect":         "Senior",
	}
	for title, want := range cases {
		t.Run(title, func(t *testing.T) {
			got := detectSeniority(&ParsedData{Experience: []Role{{Title: title}}})
			if got != want {
				t.Fatalf("detectSeniority(%q) = %q, want %q", title, got, want)
			}
		})
	}
}

func TestSeniorityIgnoresAncientTitles(t *testing.T) {
	// An "Intern" entry from eight years ago says nothing about the candidate today.
	tenYears := 120
	parsed := &ParsedData{
		Experience: []Role{
			{Title: "Software Engineer"},
			{Title: "Software Engineer"},
			{Title: "Intern"},
		},
		TotalExperienceMonths: &tenYears,
	}
	if got := detectSeniority(parsed); got == "Junior" {
		t.Fatal("an old internship dragged the seniority down to Junior")
	}
}

func TestSeniorityFallsBackToDuration(t *testing.T) {
	cases := []struct {
		months int
		want   string
	}{
		{6, "Junior"},
		{24, "Junior"},
		{25, "Associate"},
		{72, "Associate"},
		{73, "Senior"},
		{200, "Senior"},
	}
	for _, tc := range cases {
		months := tc.months
		parsed := &ParsedData{
			Experience:            []Role{{Title: "Software Engineer"}},
			TotalExperienceMonths: &months,
		}
		if got := detectSeniority(parsed); got != tc.want {
			t.Errorf("%d months = %q, want %q", tc.months, got, tc.want)
		}
	}
}

func TestSeniorityIsEmptyWhenNeitherSignalIsUsable(t *testing.T) {
	// Guessing "Junior" for a CV we could not read would hide every senior role from
	// someone who deserves them. An empty value makes matching skip the filter.
	parsed := &ParsedData{Experience: []Role{{Title: "Software Engineer"}}}
	if got := detectSeniority(parsed); got != "" {
		t.Fatalf("seniority = %q, want empty when duration is unknown", got)
	}
	if got := detectSeniority(&ParsedData{}); got != "" {
		t.Fatalf("seniority for an empty CV = %q, want empty", got)
	}
}
