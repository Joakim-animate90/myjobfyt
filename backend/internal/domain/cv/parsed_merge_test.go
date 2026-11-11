package cv_test

import (
	"testing"

	"github.com/Joakim-animate90/myjobfyt/backend/internal/domain/cv"
	"github.com/google/uuid"
)

func freshParse() *cv.ParsedData {
	return &cv.ParsedData{
		CVVersionID: uuid.New(),
		Summary:     "Fresh summary from the model",
		Contact: cv.Contact{
			FullName: "J. Candidate",
			Email:    "fresh@example.com",
			Phone:    "+254700000001",
			Location: "Mombasa",
			Headline: "Fresh headline",
		},
		Experience: []cv.Role{{Employer: "Fresh Co", Title: "Engineer"}},
		Education:  []cv.Study{{Institution: "Fresh University"}},
		Languages:  []cv.Language{{Language: "en"}},
	}
}

func existingWithCorrections(fields ...string) *cv.ParsedData {
	months := 96
	return &cv.ParsedData{
		Summary: "The summary the candidate wrote themselves",
		Contact: cv.Contact{
			FullName: "Joakim Candidate",
			Email:    "corrected@example.com",
			Phone:    "+254700123456",
			Location: "Nairobi",
			Headline: "Corrected headline",
		},
		Experience:            []cv.Role{{Employer: "Acme Corporation", Title: "Senior Engineer"}},
		Education:             []cv.Study{{Institution: "University of Nairobi"}},
		Languages:             []cv.Language{{Language: "sw"}},
		CorrectedFields:       fields,
		TotalExperienceMonths: &months,
		DetectedSeniority:     "Senior",
	}
}

func TestFirstParseNeedsNoMerge(t *testing.T) {
	fresh := freshParse()
	merged := cv.MergePreservingCorrections(fresh, nil)

	if merged.Contact.Email != "fresh@example.com" {
		t.Fatalf("email = %q, want the fresh value", merged.Contact.Email)
	}
	if len(merged.CorrectedFields) != 0 {
		t.Fatalf("CorrectedFields = %v, want empty", merged.CorrectedFields)
	}
}

func TestExistingDataWithNoCorrectionsIsFullyReplaced(t *testing.T) {
	// A re-parse should pick up improved extraction. Only corrections are sacred.
	merged := cv.MergePreservingCorrections(freshParse(), existingWithCorrections())

	if merged.Contact.Email != "fresh@example.com" {
		t.Errorf("email = %q, want the fresh value", merged.Contact.Email)
	}
	if merged.Experience[0].Employer != "Fresh Co" {
		t.Errorf("employer = %q, want the fresh value", merged.Experience[0].Employer)
	}
}

func TestCorrectedScalarsArePreservedIndividually(t *testing.T) {
	// Unlike list items, a scalar is unambiguously the same field across re-parses, so
	// correcting one must not freeze its neighbours.
	merged := cv.MergePreservingCorrections(freshParse(), existingWithCorrections("email"))

	if merged.Contact.Email != "corrected@example.com" {
		t.Fatalf("email = %q, want the correction preserved", merged.Contact.Email)
	}
	// Everything else must take the fresh value.
	if merged.Contact.Phone != "+254700000001" {
		t.Errorf("phone = %q; correcting the email froze the phone too", merged.Contact.Phone)
	}
	if merged.Contact.FullName != "J. Candidate" {
		t.Errorf("full_name = %q; correcting the email froze the name too", merged.Contact.FullName)
	}
}

func TestEachScalarFieldCanBePreserved(t *testing.T) {
	cases := map[string]func(*cv.ParsedData) string{
		"full_name": func(p *cv.ParsedData) string { return p.Contact.FullName },
		"email":     func(p *cv.ParsedData) string { return p.Contact.Email },
		"phone":     func(p *cv.ParsedData) string { return p.Contact.Phone },
		"location":  func(p *cv.ParsedData) string { return p.Contact.Location },
		"headline":  func(p *cv.ParsedData) string { return p.Contact.Headline },
		"summary":   func(p *cv.ParsedData) string { return p.Summary },
	}

	for field, read := range cases {
		t.Run(field, func(t *testing.T) {
			existing := existingWithCorrections(field)
			merged := cv.MergePreservingCorrections(freshParse(), existing)

			if read(merged) != read(existing) {
				t.Fatalf("%s = %q, want the preserved value %q", field, read(merged), read(existing))
			}
		})
	}
}

func TestACorrectionInsideAListPreservesTheWholeSection(t *testing.T) {
	// Merging one item of a re-extracted list is not well-defined: the new extraction
	// may have found a different number of roles, or ordered them differently, so
	// index 0 need not be the same role. Preserving the whole section is the
	// conservative choice — it never loses a correction.
	merged := cv.MergePreservingCorrections(freshParse(), existingWithCorrections("experience[0].employer"))

	if len(merged.Experience) != 1 || merged.Experience[0].Employer != "Acme Corporation" {
		t.Fatalf("experience = %+v, want the preserved section", merged.Experience)
	}
	// Derived values must follow the data they came from, or the stored total would
	// describe roles that are no longer there.
	if merged.TotalExperienceMonths == nil || *merged.TotalExperienceMonths != 96 {
		t.Errorf("TotalExperienceMonths = %v, want the preserved 96", merged.TotalExperienceMonths)
	}
	if merged.DetectedSeniority != "Senior" {
		t.Errorf("DetectedSeniority = %q, want the preserved value", merged.DetectedSeniority)
	}
	// Other sections still take the fresh value.
	if merged.Education[0].Institution != "Fresh University" {
		t.Errorf("education was frozen by an experience correction: %+v", merged.Education)
	}
}

func TestEachSectionCanBePreserved(t *testing.T) {
	for _, field := range []string{
		"experience", "education", "certifications", "languages",
		"experience[2].title", "education[0].institution", "languages[1].proficiency",
	} {
		t.Run(field, func(t *testing.T) {
			merged := cv.MergePreservingCorrections(freshParse(), existingWithCorrections(field))
			existing := existingWithCorrections(field)

			switch {
			case field == "experience" || field == "experience[2].title":
				if merged.Experience[0].Employer != existing.Experience[0].Employer {
					t.Fatalf("experience was not preserved: %+v", merged.Experience)
				}
			case field == "education" || field == "education[0].institution":
				if merged.Education[0].Institution != existing.Education[0].Institution {
					t.Fatalf("education was not preserved: %+v", merged.Education)
				}
			case field == "languages" || field == "languages[1].proficiency":
				if merged.Languages[0].Language != existing.Languages[0].Language {
					t.Fatalf("languages were not preserved: %+v", merged.Languages)
				}
			}
		})
	}
}

func TestCorrectionListSurvivesTheMerge(t *testing.T) {
	// Dropping the list would mean the NEXT re-parse overwrote the correction. The
	// candidate's ownership is permanent, not single-use.
	merged := cv.MergePreservingCorrections(freshParse(), existingWithCorrections("email", "experience[0].employer"))

	if len(merged.CorrectedFields) != 2 {
		t.Fatalf("CorrectedFields = %v, want both carried forward", merged.CorrectedFields)
	}

	// Prove it by merging a second time, as a second re-parse would.
	second := cv.MergePreservingCorrections(freshParse(), merged)
	if second.Contact.Email != "corrected@example.com" {
		t.Fatalf("after a second re-parse email = %q; the correction was lost", second.Contact.Email)
	}
	if second.Experience[0].Employer != "Acme Corporation" {
		t.Fatalf("after a second re-parse employer = %q; the correction was lost", second.Experience[0].Employer)
	}
}

func TestUnrecognisedCorrectionPathIsIgnoredRatherThanTreatedAsAWildcard(t *testing.T) {
	// A path naming no known section must not silently preserve everything — that
	// would freeze a CV permanently on one bad write.
	merged := cv.MergePreservingCorrections(freshParse(), existingWithCorrections("nonsense", "", "some.unknown[0].path"))

	if merged.Contact.Email != "fresh@example.com" {
		t.Errorf("email = %q; an unrecognised path preserved it", merged.Contact.Email)
	}
	if merged.Experience[0].Employer != "Fresh Co" {
		t.Errorf("experience = %+v; an unrecognised path preserved it", merged.Experience)
	}
}

func TestSkillsAreCorrectedDetection(t *testing.T) {
	// Skills live in their own table, so the merge cannot handle them and the
	// repository must know to leave the stored rows alone.
	if cv.SkillsAreCorrected(nil) {
		t.Error("SkillsAreCorrected(nil) = true")
	}
	if cv.SkillsAreCorrected(existingWithCorrections("email")) {
		t.Error("an email correction was read as a skills correction")
	}
	for _, field := range []string{"skills", "skills[0].name", "skills[3]"} {
		if !cv.SkillsAreCorrected(existingWithCorrections(field)) {
			t.Errorf("SkillsAreCorrected(%q) = false", field)
		}
	}
}

func TestReviewFieldsExcludeAlreadyCorrectedOnes(t *testing.T) {
	// Asking someone to re-review their own correction is noise that teaches them to
	// ignore the flag.
	parsed := &cv.ParsedData{
		CorrectedFields: []string{"email"},
		Provenance: cv.VerificationReport{
			NeedsReview: 2,
			Fields: []cv.FieldProvenance{
				{Field: "email", NeedsReview: true},
				{Field: "phone", NeedsReview: true},
				{Field: "full_name", NeedsReview: false},
			},
		},
	}

	review := parsed.ReviewFields()
	if len(review) != 1 {
		t.Fatalf("%d fields need review, want 1: %+v", len(review), review)
	}
	if review[0].Field != "phone" {
		t.Fatalf("review field = %q, want phone", review[0].Field)
	}
	if !parsed.IsCorrected("email") || parsed.IsCorrected("phone") {
		t.Error("IsCorrected is wrong")
	}
}

func TestFromExtractionDerivesAndTrims(t *testing.T) {
	extracted := cv.ExtractedCV{
		FullName: cv.EvidencedString{Value: "  Joakim Candidate  ", Evidence: "Joakim Candidate", Confidence: cv.ConfidenceHigh},
		Experience: []cv.ExtractedRole{{
			Title:        "  Senior Engineer  ",
			Employer:     " Acme Corporation ",
			StartDate:    "Jan 2020",
			EndDate:      "Dec 2021",
			Achievements: []string{"  Built things  ", "", "   "},
			Evidence:     "Acme Corporation",
			Confidence:   cv.ConfidenceHigh,
		}},
	}
	report := cv.VerifyExtraction(extracted, "Joakim Candidate\nAcme Corporation\nJan 2020 - Dec 2021")

	parsed := cv.FromExtraction(uuid.New(), uuid.New(), extracted, report, "cv-extract-v1")

	if parsed.Contact.FullName != "Joakim Candidate" {
		t.Errorf("full name was not trimmed: %q", parsed.Contact.FullName)
	}
	if parsed.Experience[0].Employer != "Acme Corporation" {
		t.Errorf("employer was not trimmed: %q", parsed.Experience[0].Employer)
	}
	// Blank achievements must be dropped, not stored as empty bullets.
	if len(parsed.Experience[0].Achievements) != 1 {
		t.Errorf("achievements = %v, want the blanks dropped", parsed.Experience[0].Achievements)
	}
	if parsed.Experience[0].DurationMonths == nil || *parsed.Experience[0].DurationMonths != 24 {
		t.Errorf("DurationMonths = %v, want 24", parsed.Experience[0].DurationMonths)
	}
	if parsed.TotalExperienceMonths == nil || *parsed.TotalExperienceMonths != 24 {
		t.Errorf("TotalExperienceMonths = %v, want 24", parsed.TotalExperienceMonths)
	}
	if parsed.DetectedSeniority != "Senior" {
		t.Errorf("DetectedSeniority = %q, want Senior from the title", parsed.DetectedSeniority)
	}
	if parsed.EngineVersion != "cv-extract-v1" {
		t.Errorf("EngineVersion = %q", parsed.EngineVersion)
	}
	// Empty sections must marshal as [] not null, or the database CHECK rejects them.
	education, err := parsed.MarshalEducation()
	if err != nil {
		t.Fatalf("MarshalEducation: %v", err)
	}
	if string(education) != "[]" {
		t.Errorf("empty education marshalled as %q, want []", education)
	}
}

func TestSkillsFromExtractionDeduplicatesAndCarriesConfidence(t *testing.T) {
	// A model occasionally lists the same skill twice from different sections. The
	// unique index would reject the second and fail the whole write.
	extracted := cv.ExtractedCV{
		Skills: []cv.ExtractedSkill{
			{Name: "Go", Evidence: "Go, PostgreSQL", Confidence: cv.ConfidenceHigh},
			{Name: "go", Evidence: "go and python", Confidence: cv.ConfidenceMedium},
			{Name: "  ", Evidence: "", Confidence: cv.ConfidenceLow},
			{Name: "PostgreSQL", Evidence: "Go, PostgreSQL", Confidence: cv.ConfidenceHigh},
		},
	}
	report := cv.VerifyExtraction(extracted, "Skills: Go, PostgreSQL")

	skills := cv.SkillsFromExtraction(extracted, report)

	if len(skills) != 2 {
		t.Fatalf("%d skills, want 2 after dedupe and blank removal: %+v", len(skills), skills)
	}
	for _, s := range skills {
		if s.Source != cv.SkillSourceAI {
			t.Errorf("skill %q source = %q, want ai", s.RawLabel, s.Source)
		}
		if s.Confidence == nil {
			t.Errorf("skill %q carries no confidence", s.RawLabel)
		}
		// Not force-matched to the taxonomy: recording "JS" against the wrong canonical
		// skill is worse than leaving it unmatched, because overlap then scores on a
		// fiction.
		if s.SkillID != nil {
			t.Errorf("skill %q was matched to a taxonomy entry during extraction", s.RawLabel)
		}
	}
}
