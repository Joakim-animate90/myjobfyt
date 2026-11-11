package cv

import (
	"encoding/json"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ParsedData is the persisted structured form of a CV version.
//
// Separate from ExtractedCV, which is the model's raw output. The split matters: this
// type is what the rest of the platform reads, and it carries the verification verdict
// and the candidate's corrections, neither of which the model produces.
type ParsedData struct {
	CVVersionID uuid.UUID
	UserID      uuid.UUID

	Summary        string
	Contact        Contact
	Experience     []Role
	Education      []Study
	Certifications []Certification
	Languages      []Language

	// Provenance holds the per-field verification result, so the review UI can show
	// where each value came from and which ones need a human.
	Provenance VerificationReport

	// CorrectedFields lists the fields the candidate has edited. These are permanently
	// authoritative: a re-parse must never overwrite them. Silently reverting someone's
	// correction teaches them the product cannot be trusted, which is a much more
	// expensive problem than a stale field.
	CorrectedFields []string

	TotalExperienceMonths *int
	DetectedSeniority     string
	EngineVersion         string

	CreatedAt time.Time
	UpdatedAt time.Time
}

// Contact is the candidate's contact detail as written in the CV.
//
// Note this is the CV's version, not the profile's. They can legitimately differ — a CV
// may carry an old address — and the profile is authoritative for applications. Keeping
// both means the review UI can show the candidate what their CV actually says.
type Contact struct {
	FullName string `json:"full_name,omitempty"`
	Email    string `json:"email,omitempty"`
	Phone    string `json:"phone,omitempty"`
	Location string `json:"location,omitempty"`
	Headline string `json:"headline,omitempty"`
}

// Role is one employment entry.
type Role struct {
	Title        string   `json:"title"`
	Employer     string   `json:"employer"`
	Location     string   `json:"location,omitempty"`
	StartDate    string   `json:"start_date,omitempty"`
	EndDate      string   `json:"end_date,omitempty"`
	IsCurrent    bool     `json:"is_current"`
	Description  string   `json:"description,omitempty"`
	Achievements []string `json:"achievements,omitempty"`
	// DurationMonths is derived where the dates can be parsed, and nil where they
	// cannot. Nil rather than zero, because "we could not tell" and "zero months" are
	// different facts and experience scoring must not treat them alike.
	DurationMonths *int `json:"duration_months,omitempty"`
}

// Study is one education entry.
type Study struct {
	Qualification string `json:"qualification"`
	Field         string `json:"field,omitempty"`
	Institution   string `json:"institution"`
	StartDate     string `json:"start_date,omitempty"`
	EndDate       string `json:"end_date,omitempty"`
	Grade         string `json:"grade,omitempty"`
}

// Certification is one certification.
type Certification struct {
	Name       string `json:"name"`
	Issuer     string `json:"issuer,omitempty"`
	IssuedDate string `json:"issued_date,omitempty"`
}

// Language is one language with its stated proficiency.
type Language struct {
	Language    string `json:"language"`
	Proficiency string `json:"proficiency,omitempty"`
}

// Skill is one extracted skill, linked to the canonical taxonomy where a match exists.
type Skill struct {
	// SkillID is the canonical skill, or nil when nothing in the taxonomy matched.
	// Deliberately nullable rather than force-matched: recording "JS" against the wrong
	// canonical skill is worse than recording it unmatched, because skill overlap then
	// scores on a fiction.
	SkillID    *uuid.UUID
	RawLabel   string
	Years      *float64
	Confidence *float64
	Source     SkillSource
}

// SkillSource records where a skill came from.
type SkillSource string

const (
	// SkillSourceAI means the model extracted it from the CV.
	SkillSourceAI SkillSource = "ai"
	// SkillSourceCandidate means the candidate added or corrected it. Authoritative.
	SkillSourceCandidate SkillSource = "candidate"
	// SkillSourceTaxonomy means it was inferred from the canonical taxonomy.
	SkillSourceTaxonomy SkillSource = "taxonomy"
)

// IsCorrected reports whether a field is owned by the candidate.
func (p *ParsedData) IsCorrected(field string) bool {
	return slices.Contains(p.CorrectedFields, field)
}

// NeedsReview reports whether any field was flagged for the candidate to check.
func (p *ParsedData) NeedsReview() bool { return p.Provenance.NeedsReview > 0 }

// ReviewFields returns the fields the candidate should check, excluding any they have
// already corrected — asking someone to re-review their own correction is noise that
// teaches them to ignore the flag.
func (p *ParsedData) ReviewFields() []FieldProvenance {
	out := make([]FieldProvenance, 0, p.Provenance.NeedsReview)
	for _, f := range p.Provenance.Fields {
		if f.NeedsReview && !p.IsCorrected(f.Field) {
			out = append(out, f)
		}
	}
	return out
}

// FromExtraction builds persisted ParsedData from a model extraction and its
// verification report.
//
// Values whose evidence could not be located are kept, because the candidate may
// recognise a correct value the model quoted badly. They arrive flagged via the
// provenance report, and SafeToUse-style gating happens at the point of use rather than
// by discarding data here.
func FromExtraction(versionID, userID uuid.UUID, extracted ExtractedCV, report VerificationReport, engineVersion string) *ParsedData {
	parsed := &ParsedData{
		CVVersionID: versionID,
		UserID:      userID,
		Summary:     strings.TrimSpace(extracted.Summary.Value),
		Contact: Contact{
			FullName: strings.TrimSpace(extracted.FullName.Value),
			Email:    strings.TrimSpace(extracted.Email.Value),
			Phone:    strings.TrimSpace(extracted.Phone.Value),
			Location: strings.TrimSpace(extracted.Location.Value),
			Headline: strings.TrimSpace(extracted.Headline.Value),
		},
		Provenance:      report,
		CorrectedFields: []string{},
		EngineVersion:   engineVersion,
	}

	for _, r := range extracted.Experience {
		role := Role{
			Title:        strings.TrimSpace(r.Title),
			Employer:     strings.TrimSpace(r.Employer),
			Location:     strings.TrimSpace(r.Location),
			StartDate:    strings.TrimSpace(r.StartDate),
			EndDate:      strings.TrimSpace(r.EndDate),
			IsCurrent:    r.IsCurrent,
			Description:  strings.TrimSpace(r.Description),
			Achievements: trimAll(r.Achievements),
		}
		role.DurationMonths = roleDurationMonths(role)
		parsed.Experience = append(parsed.Experience, role)
	}

	for _, s := range extracted.Education {
		parsed.Education = append(parsed.Education, Study{
			Qualification: strings.TrimSpace(s.Qualification),
			Field:         strings.TrimSpace(s.Field),
			Institution:   strings.TrimSpace(s.Institution),
			StartDate:     strings.TrimSpace(s.StartDate),
			EndDate:       strings.TrimSpace(s.EndDate),
			Grade:         strings.TrimSpace(s.Grade),
		})
	}

	for _, c := range extracted.Certifications {
		parsed.Certifications = append(parsed.Certifications, Certification{
			Name:       strings.TrimSpace(c.Name),
			Issuer:     strings.TrimSpace(c.Issuer),
			IssuedDate: strings.TrimSpace(c.IssuedDate),
		})
	}

	for _, l := range extracted.Languages {
		parsed.Languages = append(parsed.Languages, Language{
			Language:    strings.TrimSpace(l.Language),
			Proficiency: strings.TrimSpace(l.Proficiency),
		})
	}

	parsed.TotalExperienceMonths = totalExperienceMonths(parsed.Experience)
	parsed.DetectedSeniority = detectSeniority(parsed)

	return parsed
}

// SkillsFromExtraction converts extracted skills, carrying the per-field confidence
// through from the verification report.
func SkillsFromExtraction(extracted ExtractedCV, report VerificationReport) []Skill {
	// Index the provenance by field path so each skill gets its own verified
	// confidence rather than an extraction-wide average.
	confidenceByField := make(map[string]float64, len(report.Fields))
	for _, f := range report.Fields {
		confidenceByField[f.Field] = f.Confidence
	}

	out := make([]Skill, 0, len(extracted.Skills))
	seen := make(map[string]struct{}, len(extracted.Skills))

	for i, s := range extracted.Skills {
		label := strings.TrimSpace(s.Name)
		if label == "" {
			continue
		}
		// A model occasionally lists the same skill twice from different sections. The
		// unique index on (cv_version_id, raw_label) would reject the second, failing
		// the whole write, so duplicates are dropped here.
		key := strings.ToLower(label)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}

		skill := Skill{RawLabel: label, Source: SkillSourceAI}
		if c, ok := confidenceByField[fieldPath("skills", i, "name")]; ok {
			skill.Confidence = &c
		}
		out = append(out, skill)
	}
	return out
}

func trimAll(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if t := strings.TrimSpace(s); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// MarshalContact and friends keep the jsonb column shapes in one place, so the
// repository does not build JSON inline.
func (p *ParsedData) MarshalContact() ([]byte, error)        { return json.Marshal(p.Contact) }
func (p *ParsedData) MarshalExperience() ([]byte, error)     { return marshalList(p.Experience) }
func (p *ParsedData) MarshalEducation() ([]byte, error)      { return marshalList(p.Education) }
func (p *ParsedData) MarshalCertifications() ([]byte, error) { return marshalList(p.Certifications) }
func (p *ParsedData) MarshalLanguages() ([]byte, error)      { return marshalList(p.Languages) }
func (p *ParsedData) MarshalProvenance() ([]byte, error)     { return json.Marshal(p.Provenance) }

// marshalList serialises a slice, emitting [] rather than null for an empty one.
//
// The database has a CHECK that experience and education are JSON arrays, and null
// fails it — so an empty CV section would otherwise reject the whole write.
func marshalList[T any](items []T) ([]byte, error) {
	if items == nil {
		return []byte("[]"), nil
	}
	return json.Marshal(items)
}
