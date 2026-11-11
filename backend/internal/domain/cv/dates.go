package cv

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Date interpretation for CV entries.
//
// The model is instructed NOT to normalise dates — "Jan 2019" stays "Jan 2019" — because
// a model asked to normalise will also silently invent a month for a year-only entry.
// So the as-written forms are parsed here, where the rules are explicit and testable.
//
// Every function in this file returns nil rather than a guess when it cannot tell. That
// matters because the output feeds years-of-experience scoring: a fabricated duration
// produces a confidently wrong match score, which is worse than an absent one because
// the candidate acts on it.

// yearMonth is a parsed CV date, precise to a month at best.
type yearMonth struct {
	year  int
	month int // 1-12, or 0 when the date gave only a year
}

// monthNames maps the month spellings that appear in real CVs.
var monthNames = map[string]int{
	"jan": 1, "january": 1,
	"feb": 2, "february": 2,
	"mar": 3, "march": 3,
	"apr": 4, "april": 4,
	"may": 5,
	"jun": 6, "june": 6,
	"jul": 7, "july": 7,
	"aug": 8, "august": 8,
	"sep": 9, "sept": 9, "september": 9,
	"oct": 10, "october": 10,
	"nov": 11, "november": 11,
	"dec": 12, "december": 12,
}

// presentWords mark an open-ended end date.
var presentWords = map[string]struct{}{
	"present": {}, "current": {}, "now": {}, "ongoing": {}, "to date": {}, "today": {},
}

var (
	// isoish matches 2019-01 and 2019/01.
	isoish = regexp.MustCompile(`^(\d{4})[-/](\d{1,2})$`)
	// slashedFirst matches 01/2019 and 1-2019.
	slashedFirst = regexp.MustCompile(`^(\d{1,2})[-/](\d{4})$`)
	// yearOnly matches a bare year, bounded so a salary or a postcode is not read as one.
	yearOnly = regexp.MustCompile(`^(\d{4})$`)
	// fullIso matches 2019-01-15, where the day is present but irrelevant here.
	fullIso = regexp.MustCompile(`^(\d{4})-(\d{1,2})-\d{1,2}$`)
)

// minPlausibleYear and maxFutureYears bound what is accepted as a CV date.
//
// A four-digit number is not necessarily a year: CVs contain salaries, postcodes,
// employee counts and version numbers. Bounding the range is what stops "Managed a
// budget of 2500" becoming an employment date.
const (
	minPlausibleYear = 1950
	maxFutureYears   = 1
)

// parseCVDate interprets a date as written in a CV, returning false when it cannot.
func parseCVDate(raw string) (yearMonth, bool) {
	s := strings.ToLower(strings.TrimSpace(raw))
	if s == "" {
		return yearMonth{}, false
	}

	// Strip ordinal and separator noise that CVs are full of: "Jan. 2019", "Jan, 2019".
	s = strings.NewReplacer(".", "", ",", " ").Replace(s)
	s = collapseWhitespace(s)

	if _, present := presentWords[s]; present {
		return yearMonth{}, false
	}

	if m := fullIso.FindStringSubmatch(s); m != nil {
		return buildYearMonth(m[1], m[2])
	}
	if m := isoish.FindStringSubmatch(s); m != nil {
		return buildYearMonth(m[1], m[2])
	}
	if m := slashedFirst.FindStringSubmatch(s); m != nil {
		// Month first: the 01/2019 form. A value above 12 cannot be a month, so the
		// date is not in this form and is rejected rather than reinterpreted.
		return buildYearMonth(m[2], m[1])
	}
	if m := yearOnly.FindStringSubmatch(s); m != nil {
		year, err := strconv.Atoi(m[1])
		if err != nil || !plausibleYear(year) {
			return yearMonth{}, false
		}
		return yearMonth{year: year}, true
	}

	// "Jan 2019" / "January 2019" / "2019 Jan", in either order.
	fields := strings.Fields(s)
	if len(fields) == 2 {
		if month, ok := monthNames[fields[0]]; ok {
			if year, err := strconv.Atoi(fields[1]); err == nil && plausibleYear(year) {
				return yearMonth{year: year, month: month}, true
			}
		}
		if month, ok := monthNames[fields[1]]; ok {
			if year, err := strconv.Atoi(fields[0]); err == nil && plausibleYear(year) {
				return yearMonth{year: year, month: month}, true
			}
		}
	}

	return yearMonth{}, false
}

func buildYearMonth(yearStr, monthStr string) (yearMonth, bool) {
	year, err := strconv.Atoi(yearStr)
	if err != nil || !plausibleYear(year) {
		return yearMonth{}, false
	}
	month, err := strconv.Atoi(monthStr)
	if err != nil || month < 1 || month > 12 {
		return yearMonth{}, false
	}
	return yearMonth{year: year, month: month}, true
}

func plausibleYear(year int) bool {
	return year >= minPlausibleYear && year <= time.Now().UTC().Year()+maxFutureYears
}

// isPresent reports whether an end date means "still there".
func isPresent(raw string) bool {
	s := strings.ToLower(strings.TrimSpace(strings.NewReplacer(".", "", ",", " ").Replace(raw)))
	s = collapseWhitespace(s)
	if s == "" {
		return false
	}
	_, present := presentWords[s]
	return present
}

// monthsBetween returns the inclusive month span between two dates.
//
// A year-only date is treated as mid-year rather than January. "2019 - 2021" as January
// to January is 24 months, but it almost always means somewhere inside 2019 to
// somewhere inside 2021, and anchoring to January systematically overstates every
// year-only range. Mid-year is the least-wrong assumption, and the imprecision is
// inherent to the source rather than introduced here.
func monthsBetween(start, end yearMonth) int {
	startMonth := start.month
	if startMonth == 0 {
		startMonth = 7
	}
	endMonth := end.month
	if endMonth == 0 {
		endMonth = 7
	}

	months := (end.year-start.year)*12 + (endMonth - startMonth)
	if months < 0 {
		return 0
	}
	// Inclusive of the start month: a role from Jan to Jan of the same year is one
	// month of work, not zero.
	return months + 1
}

// roleDurationMonths computes a role's length, or nil when the dates do not allow it.
func roleDurationMonths(role Role) *int {
	start, ok := parseCVDate(role.StartDate)
	if !ok {
		return nil
	}

	var end yearMonth
	switch {
	case role.IsCurrent || isPresent(role.EndDate):
		now := time.Now().UTC()
		end = yearMonth{year: now.Year(), month: int(now.Month())}
	default:
		parsed, parsedOK := parseCVDate(role.EndDate)
		if !parsedOK {
			return nil
		}
		end = parsed
	}

	// A start after the end means the CV is inconsistent or we misread it. Returning
	// nil is honest; returning 0 would read as "no experience" in scoring.
	if end.year < start.year ||
		(end.year == start.year && end.month != 0 && start.month != 0 && end.month < start.month) {
		return nil
	}

	months := monthsBetween(start, end)
	return &months
}

// totalExperienceMonths sums employment, merging overlapping roles.
//
// Summing durations naively double-counts concurrent roles — a contractor with three
// simultaneous clients would appear to have three times their real experience, and
// seniority scoring would then place them far above where they belong. Merging
// overlapping intervals is the fix.
//
// Returns nil when no role had parseable dates, because zero and unknown must not be
// conflated.
func totalExperienceMonths(roles []Role) *int {
	type interval struct{ start, end int } // absolute months since year 0

	intervals := make([]interval, 0, len(roles))
	for _, role := range roles {
		start, ok := parseCVDate(role.StartDate)
		if !ok {
			continue
		}

		var end yearMonth
		switch {
		case role.IsCurrent || isPresent(role.EndDate):
			now := time.Now().UTC()
			end = yearMonth{year: now.Year(), month: int(now.Month())}
		default:
			parsed, parsedOK := parseCVDate(role.EndDate)
			if !parsedOK {
				continue
			}
			end = parsed
		}

		startAbs := absoluteMonth(start)
		endAbs := absoluteMonth(end)
		if endAbs < startAbs {
			continue
		}
		intervals = append(intervals, interval{start: startAbs, end: endAbs})
	}

	if len(intervals) == 0 {
		return nil
	}

	// Sort by start, then sweep, merging anything that touches or overlaps.
	for i := 1; i < len(intervals); i++ {
		for j := i; j > 0 && intervals[j].start < intervals[j-1].start; j-- {
			intervals[j], intervals[j-1] = intervals[j-1], intervals[j]
		}
	}

	total := 0
	current := intervals[0]
	for _, next := range intervals[1:] {
		if next.start <= current.end+1 {
			// Overlapping or adjacent: extend rather than add. Adjacent counts as
			// continuous because a role ending in June and the next starting in July is
			// unbroken employment, not a one-month gap.
			if next.end > current.end {
				current.end = next.end
			}
			continue
		}
		total += current.end - current.start + 1
		current = next
	}
	total += current.end - current.start + 1

	return &total
}

// absoluteMonth converts a yearMonth to a comparable month index.
func absoluteMonth(ym yearMonth) int {
	month := ym.month
	if month == 0 {
		month = 7
	}
	return ym.year*12 + month
}

// seniorityKeywords map title words to a seniority, most specific first.
//
// Ordered because titles combine words: "Senior Engineering Manager" must read as
// managerial rather than stopping at "Senior", and "Junior Developer" must not match
// "Developer" first.
var seniorityKeywords = []struct {
	keywords  []string
	seniority string
}{
	{[]string{"chief", "cto", "ceo", "cfo", "coo", "vp ", "vice president", "president", "founder", "partner"}, "Executive"},
	{[]string{"head of", "director", "principal", "distinguished", "fellow"}, "Executive"},
	{[]string{"intern", "internship", "trainee", "apprentice", "graduate", "junior", "jr ", "jr."}, "Junior"},
	{[]string{"senior", "snr", "sr ", "sr.", "lead", "staff", "manager", "architect"}, "Senior"},
}

// Experience thresholds for seniority, in months.
const (
	juniorCeilingMonths    = 24
	associateCeilingMonths = 72
)

// detectSeniority infers a seniority level from titles and total experience.
//
// Titles are weighed before duration, because a title is the candidate's own statement
// of their level while duration is our inference. Where they disagree — a five-year
// career with a "Head of" title — the title wins, since that is what an employer reads.
//
// Returns "" when neither signal is usable. An empty value makes matching fall back to
// not filtering on seniority, which is the right behaviour: guessing "Junior" for a CV
// we could not read would hide every senior role from someone who deserves them.
func detectSeniority(parsed *ParsedData) string {
	// The most recent role is the most informative, and the model is told to return
	// experience most recent first.
	for i, role := range parsed.Experience {
		title := strings.ToLower(" " + role.Title + " ")
		for _, entry := range seniorityKeywords {
			for _, keyword := range entry.keywords {
				if strings.Contains(title, keyword) {
					return entry.seniority
				}
			}
		}
		// Only the two most recent titles are considered. An "Intern" entry from eight
		// years ago says nothing about the candidate today.
		if i >= 1 {
			break
		}
	}

	if parsed.TotalExperienceMonths == nil {
		return ""
	}
	switch months := *parsed.TotalExperienceMonths; {
	case months <= juniorCeilingMonths:
		return "Junior"
	case months <= associateCeilingMonths:
		return "Associate"
	default:
		return "Senior"
	}
}
