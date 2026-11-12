package cv

import (
	"fmt"
	"sort"
	"strings"

	"github.com/Joakim-animate90/myjobfyt/backend/internal/apperr"
	"github.com/Joakim-animate90/myjobfyt/backend/internal/platform/pdfgen"
)

// TemplateID names a CV template.
type TemplateID string

const (
	// TemplateClassic is a single-column reverse-chronological CV. The default, and
	// deliberately the plainest: it is the layout applicant tracking systems parse most
	// reliably.
	TemplateClassic TemplateID = "classic"
	// TemplateCompact is the same structure with tighter spacing, for candidates whose
	// history does not fit two pages otherwise.
	TemplateCompact TemplateID = "compact"
)

// template describes one renderable layout.
type template struct {
	id TemplateID
	// nameSize and bodySize drive the whole layout; everything else is derived, so a
	// template is a few numbers rather than a separate copy of the builder.
	nameSize    float64
	bodySize    float64
	sectionGap  float64
	entryGap    float64
	description string
}

// templates is the allowlist.
//
// A map keyed by id, consulted by exact match. Rendering is never driven by a path, a
// filename or anything else a caller could point outside this set — an allowlist that
// can be escaped by a crafted id is not an allowlist.
var templates = map[TemplateID]template{
	TemplateClassic: {
		id: TemplateClassic, nameSize: 20, bodySize: 10, sectionGap: 18, entryGap: 10,
		description: "Single column, reverse chronological. The most reliably parsed layout.",
	},
	TemplateCompact: {
		id: TemplateCompact, nameSize: 17, bodySize: 9.5, sectionGap: 13, entryGap: 7,
		description: "The same structure with tighter spacing, for longer histories.",
	},
}

// TemplateInfo describes a template to a candidate choosing one.
type TemplateInfo struct {
	ID          TemplateID
	Description string
}

// Templates lists the allowlisted templates, in a fixed order.
func Templates() []TemplateInfo {
	out := make([]TemplateInfo, 0, len(templates))
	for id, tpl := range templates {
		out = append(out, TemplateInfo{ID: id, Description: tpl.description})
	}
	// Sorted, because ranging a map is randomised and a list that reshuffled between
	// requests would make the chooser unusable.
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// ErrUnknownTemplate reports a template outside the allowlist.
var ErrUnknownTemplate = apperr.Validation("unknown_template",
	"That CV template does not exist.")

// BuildPDF renders a parsed CV using an allowlisted template.
//
// The renderer takes typed content, not markup: it has no concept of a URL, a file path
// or a stylesheet, so there is nothing for a crafted employer name or job title to
// reference. A candidate's CV contents cannot cause a fetch because no code path can
// fetch anything.
func BuildPDF(parsed *ParsedData, skills []Skill, id TemplateID) ([]byte, error) {
	tpl, ok := templates[id]
	if !ok {
		return nil, ErrUnknownTemplate
	}
	if parsed == nil {
		return nil, apperr.Validation("no_cv_content", "There is no parsed CV to build from yet.")
	}

	doc := pdfgen.Document{
		Title:  strings.TrimSpace(parsed.Contact.FullName),
		Blocks: layoutCV(parsed, skills, tpl),
	}

	pdf, err := pdfgen.Render(doc)
	if err != nil {
		var unrenderable *pdfgen.ErrUnrenderable
		if asUnrenderable(err, &unrenderable) {
			// The candidate's own words cannot be set in the base fonts. Their problem
			// to decide about, not ours to paper over by substituting characters.
			return nil, apperr.Validation("unrenderable_characters", "%s", unrenderable.Error())
		}
		return nil, apperr.Internalf(err, "render cv pdf")
	}
	return pdf, nil
}

func asUnrenderable(err error, target **pdfgen.ErrUnrenderable) bool {
	if e, ok := err.(*pdfgen.ErrUnrenderable); ok {
		*target = e
		return true
	}
	return false
}

// layoutCV turns a parsed CV into renderable blocks.
//
// One column, headings in reading order, no tables and no columns. That is not a visual
// preference: a two-column CV extracts interleaved, which is how a candidate's dates end
// up attached to the wrong employer inside an applicant tracking system.
func layoutCV(parsed *ParsedData, skills []Skill, tpl template) []pdfgen.Block {
	var blocks []pdfgen.Block

	add := func(b pdfgen.Block) {
		if strings.TrimSpace(b.Text) == "" {
			return
		}
		blocks = append(blocks, b)
	}

	add(pdfgen.Block{Text: parsed.Contact.FullName, Font: pdfgen.FontBold, Size: tpl.nameSize})
	add(pdfgen.Block{Text: parsed.Contact.Headline, Size: tpl.bodySize + 1, SpaceBefore: 4})
	add(pdfgen.Block{Text: contactLine(parsed.Contact), Size: tpl.bodySize, SpaceBefore: 4})

	section := func(title string) {
		add(pdfgen.Block{
			Text: title, Font: pdfgen.FontBold, Size: tpl.bodySize + 1,
			SpaceBefore: tpl.sectionGap, Rule: true,
		})
	}

	if strings.TrimSpace(parsed.Summary) != "" {
		section("SUMMARY")
		add(pdfgen.Block{Text: parsed.Summary, Size: tpl.bodySize, SpaceBefore: tpl.entryGap})
	}

	if len(parsed.Experience) > 0 {
		section("EXPERIENCE")
		for _, role := range parsed.Experience {
			add(pdfgen.Block{
				Text: roleHeading(role), Font: pdfgen.FontBold, Size: tpl.bodySize + 0.5,
				SpaceBefore: tpl.entryGap,
			})
			add(pdfgen.Block{Text: dateRange(role), Font: pdfgen.FontItalic, Size: tpl.bodySize - 1})
			add(pdfgen.Block{Text: role.Description, Size: tpl.bodySize, SpaceBefore: 2})
			for _, achievement := range role.Achievements {
				add(pdfgen.Block{Text: achievement, Size: tpl.bodySize, Bullet: true})
			}
		}
	}

	if len(parsed.Education) > 0 {
		section("EDUCATION")
		for _, study := range parsed.Education {
			add(pdfgen.Block{
				Text: studyHeading(study), Font: pdfgen.FontBold, Size: tpl.bodySize + 0.5,
				SpaceBefore: tpl.entryGap,
			})
			add(pdfgen.Block{Text: studyDates(study), Font: pdfgen.FontItalic, Size: tpl.bodySize - 1})
		}
	}

	if labels := skillLabels(parsed, skills); labels != "" {
		section("SKILLS")
		// Comma-separated prose rather than a grid. A grid is a table, and tables are
		// where ATS parsers lose content.
		add(pdfgen.Block{Text: labels, Size: tpl.bodySize, SpaceBefore: tpl.entryGap})
	}

	if len(parsed.Certifications) > 0 {
		section("CERTIFICATIONS")
		for _, cert := range parsed.Certifications {
			add(pdfgen.Block{Text: certLine(cert), Size: tpl.bodySize, Bullet: true, SpaceBefore: 2})
		}
	}

	if len(parsed.Languages) > 0 {
		section("LANGUAGES")
		add(pdfgen.Block{Text: languageLine(parsed.Languages), Size: tpl.bodySize, SpaceBefore: tpl.entryGap})
	}

	return blocks
}

// contactLine joins the contact details that exist, so a missing phone does not leave a
// stray separator.
func contactLine(c Contact) string {
	return joinNonEmpty(" | ", c.Email, c.Phone, c.Location)
}

func roleHeading(role Role) string {
	heading := joinNonEmpty(", ", role.Title, role.Employer)
	if role.Location != "" {
		heading += " (" + role.Location + ")"
	}
	return heading
}

func dateRange(role Role) string {
	end := role.EndDate
	if role.IsCurrent && strings.TrimSpace(end) == "" {
		end = "Present"
	}
	return joinNonEmpty(" - ", role.StartDate, end)
}

func studyHeading(study Study) string {
	qualification := joinNonEmpty(" in ", study.Qualification, study.Field)
	return joinNonEmpty(", ", qualification, study.Institution)
}

func studyDates(study Study) string {
	dates := joinNonEmpty(" - ", study.StartDate, study.EndDate)
	if study.Grade != "" {
		dates = joinNonEmpty(" | ", dates, study.Grade)
	}
	return dates
}

func certLine(cert Certification) string {
	line := joinNonEmpty(", ", cert.Name, cert.Issuer)
	return joinNonEmpty(" ", line, bracket(cert.IssuedDate))
}

func languageLine(languages []Language) string {
	parts := make([]string, 0, len(languages))
	for _, l := range languages {
		parts = append(parts, joinNonEmpty(" ", l.Language, bracket(l.Proficiency)))
	}
	return strings.Join(parts, ", ")
}

// skillLabels prefers the canonical skill list, falling back to what the CV claimed.
func skillLabels(parsed *ParsedData, skills []Skill) string {
	if len(skills) > 0 {
		labels := make([]string, 0, len(skills))
		for _, skill := range skills {
			if s := strings.TrimSpace(skill.RawLabel); s != "" {
				labels = append(labels, s)
			}
		}
		return strings.Join(labels, ", ")
	}
	return strings.Join(parsed.ClaimedSkills, ", ")
}

func bracket(s string) string {
	if strings.TrimSpace(s) == "" {
		return ""
	}
	return fmt.Sprintf("(%s)", s)
}

// joinNonEmpty joins the parts that have content, so an absent field never leaves a
// dangling separator on a document someone is judged by.
func joinNonEmpty(sep string, parts ...string) string {
	kept := make([]string, 0, len(parts))
	for _, part := range parts {
		if s := strings.TrimSpace(part); s != "" {
			kept = append(kept, s)
		}
	}
	return strings.Join(kept, sep)
}
