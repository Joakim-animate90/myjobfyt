package cv

import "fmt"

// ChangeKind describes what happened to a piece of the CV.
type ChangeKind string

const (
	// ChangeReworded means the same fact is expressed differently.
	ChangeReworded ChangeKind = "reworded"
	// ChangeReordered means an entry moved.
	ChangeReordered ChangeKind = "reordered"
	// ChangeOmitted means an entry or passage was left out.
	ChangeOmitted ChangeKind = "omitted"
)

// Change is one difference between the source CV and the tailored one.
type Change struct {
	Kind  ChangeKind
	Field string
	// Before and After are the candidate's own words on both sides. Shown to them, not
	// logged.
	Before string
	After  string
	// Reason is the model's explanation where it gave one.
	Reason string
}

// TailoringDiff is what the candidate reviews before a tailored CV is used.
//
// MJF-011 requires the candidate sees this before the tailored CV is attached to
// anything. Tailoring rewrites how their experience is described in their name, so
// approving it is their decision and they cannot make it without seeing what changed.
type TailoringDiff struct {
	Changes []Change
	// Rejected is non-empty when tailoring was refused, so the candidate can be told
	// what the platform declined to claim on their behalf rather than just that it
	// failed.
	Rejected []Violation
}

// Empty reports whether anything actually changed.
func (d TailoringDiff) Empty() bool { return len(d.Changes) == 0 && len(d.Rejected) == 0 }

// DiffTailoring compares a tailored CV against its source.
//
// Compares roles by their identity — employer plus title — rather than by position,
// because reordering is the main thing tailoring does and a positional diff would
// report every role as rewritten.
func DiffTailoring(source, tailored *ParsedData, omitted []OmittedItem, rationale string) TailoringDiff {
	var diff TailoringDiff
	if source == nil || tailored == nil {
		return diff
	}

	if source.Summary != tailored.Summary {
		diff.Changes = append(diff.Changes, Change{
			Kind: ChangeReworded, Field: "summary",
			Before: source.Summary, After: tailored.Summary, Reason: rationale,
		})
	}
	if source.Contact.Headline != tailored.Contact.Headline {
		diff.Changes = append(diff.Changes, Change{
			Kind: ChangeReworded, Field: "headline",
			Before: source.Contact.Headline, After: tailored.Contact.Headline,
		})
	}

	sourceAt := map[string]int{}
	for i, role := range source.Experience {
		sourceAt[roleIdentity(role)] = i
	}

	seen := map[string]bool{}
	for newIndex, role := range tailored.Experience {
		id := roleIdentity(role)
		seen[id] = true

		oldIndex, existed := sourceAt[id]
		if !existed {
			// Containment runs before this, so an unmatched role here means the diff
			// and the validator disagree. Reporting it is better than hiding it.
			diff.Changes = append(diff.Changes, Change{
				Kind: ChangeReworded, Field: fmt.Sprintf("experience[%d]", newIndex),
				After: role.Title + " at " + role.Employer,
			})
			continue
		}

		if oldIndex != newIndex {
			diff.Changes = append(diff.Changes, Change{
				Kind:   ChangeReordered,
				Field:  fmt.Sprintf("experience[%d]", newIndex),
				Before: fmt.Sprintf("position %d", oldIndex+1),
				After:  fmt.Sprintf("position %d", newIndex+1),
				Reason: role.Title + " at " + role.Employer,
			})
		}

		old := source.Experience[oldIndex]
		if old.Description != role.Description {
			diff.Changes = append(diff.Changes, Change{
				Kind:   ChangeReworded,
				Field:  fmt.Sprintf("experience[%d].description", newIndex),
				Before: old.Description, After: role.Description,
			})
		}
		if !equalStrings(old.Achievements, role.Achievements) {
			diff.Changes = append(diff.Changes, Change{
				Kind:   ChangeReworded,
				Field:  fmt.Sprintf("experience[%d].achievements", newIndex),
				Before: joinLines(old.Achievements), After: joinLines(role.Achievements),
			})
		}
	}

	// Roles the tailoring dropped. Reported even when the model did not list them,
	// because a role silently missing from their own CV is the change a candidate is
	// most likely to object to.
	for _, role := range source.Experience {
		id := roleIdentity(role)
		if seen[id] {
			continue
		}
		diff.Changes = append(diff.Changes, Change{
			Kind:   ChangeOmitted,
			Field:  "experience",
			Before: role.Title + " at " + role.Employer,
			Reason: reasonFor(omitted, role.Employer, role.Title),
		})
	}

	return diff
}

// roleIdentity keys a role by the facts tailoring may not change.
func roleIdentity(role Role) string {
	return NormaliseLabel(role.Employer) + "|" + NormaliseLabel(role.Title)
}

// reasonFor finds the model's stated reason for omitting something.
func reasonFor(omitted []OmittedItem, employer, title string) string {
	for _, item := range omitted {
		if containsFold(item.Item, employer) || containsFold(item.Item, title) {
			return item.Reason
		}
	}
	return "left out as less relevant to this role"
}

func containsFold(haystack, needle string) bool {
	if needle == "" {
		return false
	}
	return isSubsequence(normaliseWords(needle), normaliseWords(haystack))
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func joinLines(items []string) string {
	out := ""
	for i, item := range items {
		if i > 0 {
			out += "\n"
		}
		out += item
	}
	return out
}
