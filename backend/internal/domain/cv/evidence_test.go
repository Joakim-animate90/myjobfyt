package cv_test

import (
	"strings"
	"testing"

	"github.com/Joakim-animate90/myjobfyt/backend/internal/domain/cv"
)

const sourceCV = `Joakim Candidate
Senior Backend Engineer
Nairobi, Kenya | joakim@example.com

EXPERIENCE

Acme Corporation — Senior Backend Engineer
Jan 2021 - Present
Built distributed Go services handling 40k requests per second.
Led the migration from monolith to event-driven architecture.

Globex Ltd — Backend Engineer
2019 - 2021
Developed REST and gRPC APIs in Go and Python.

SKILLS
Go, PostgreSQL, Redis, NATS, Kubernetes, Terraform

EDUCATION
BSc Computer Science, University of Nairobi, 2015 - 2019`

func evidenced(value, evidence string, c cv.Confidence) cv.EvidencedString {
	return cv.EvidencedString{Value: value, Evidence: evidence, Confidence: c}
}

func TestExactEvidenceIsVerifiedWithRealOffsets(t *testing.T) {
	extracted := cv.ExtractedCV{
		FullName: evidenced("Joakim Candidate", "Joakim Candidate", cv.ConfidenceHigh),
	}

	report := cv.VerifyExtraction(extracted, sourceCV)

	if report.Verified != 1 || report.Unverified != 0 {
		t.Fatalf("verified=%d unverified=%d, want 1/0", report.Verified, report.Unverified)
	}
	field := report.Fields[0]
	if field.Status != cv.VerifiedExact {
		t.Errorf("status = %q, want exact", field.Status)
	}
	// The offsets must actually point at the value, or the review UI highlights the
	// wrong part of the document.
	if got := sourceCV[field.StartOffset:field.EndOffset]; got != "Joakim Candidate" {
		t.Errorf("the offsets point at %q, not the value", got)
	}
	if field.NeedsReview {
		t.Error("a high-confidence exact match was flagged for review")
	}
}

func TestWhitespaceNormalisedEvidenceIsVerified(t *testing.T) {
	// The normal case for PDF-extracted text: extraction inserts incidental spacing
	// the model did not reproduce. Rejecting these would flag most of a real CV.
	source := "Acme    Corporation\n\n—   Senior   Backend   Engineer"
	extracted := cv.ExtractedCV{
		Experience: []cv.ExtractedRole{{
			Employer:   "Acme Corporation",
			Title:      "Senior Backend Engineer",
			Evidence:   "Acme Corporation — Senior Backend Engineer",
			Confidence: cv.ConfidenceHigh,
		}},
	}

	report := cv.VerifyExtraction(extracted, source)

	if report.Unverified != 0 {
		t.Fatalf("unverified = %d, want 0; whitespace differences must not fail verification", report.Unverified)
	}
	for _, f := range report.Fields {
		if f.Status != cv.VerifiedNormalised {
			t.Errorf("%s status = %q, want normalised", f.Field, f.Status)
		}
		if f.StartOffset < 0 {
			t.Errorf("%s has no offset despite verifying", f.Field)
		}
	}
}

func TestOffsetsMapBackThroughMultiByteCharacters(t *testing.T) {
	// The em dash and accented characters are multi-byte. A naive offset map would
	// land mid-rune and the UI would render mojibake or slice panic.
	source := "Société Générale — Ingénieur Backend\nParis, France"
	extracted := cv.ExtractedCV{
		Experience: []cv.ExtractedRole{{
			Employer:   "Société Générale",
			Title:      "Ingénieur Backend",
			Evidence:   "Société Générale — Ingénieur Backend",
			Confidence: cv.ConfidenceHigh,
		}},
	}

	report := cv.VerifyExtraction(extracted, source)
	for _, f := range report.Fields {
		if !f.Status.Trustworthy() {
			t.Fatalf("%s was not verified: %q", f.Field, f.Status)
		}
		if f.StartOffset < 0 || f.EndOffset > len(source) {
			t.Fatalf("%s offsets %d-%d are outside the source", f.Field, f.StartOffset, f.EndOffset)
		}
		// Slicing at the reported offsets must not split a rune.
		snippet := source[f.StartOffset:f.EndOffset]
		if !utf8Valid(snippet) {
			t.Fatalf("%s offsets split a multi-byte character: %q", f.Field, snippet)
		}
	}
}

func TestFabricatedEmployerIsCaught(t *testing.T) {
	// The single worst extraction error: an employer the candidate never worked for,
	// which would then appear on applications sent in their name.
	extracted := cv.ExtractedCV{
		Experience: []cv.ExtractedRole{{
			Employer:   "Google",
			Title:      "Staff Engineer",
			StartDate:  "2015",
			Evidence:   "Google — Staff Engineer, 2015 - 2019",
			Confidence: cv.ConfidenceHigh, // the model was confident and wrong
		}},
	}

	report := cv.VerifyExtraction(extracted, sourceCV)

	if report.Verified != 0 {
		t.Fatalf("verified = %d, want 0; nothing in this entry is in the source", report.Verified)
	}
	if report.Unverified != 3 {
		t.Fatalf("unverified = %d, want 3 (employer, title, start_date)", report.Unverified)
	}
	for _, f := range report.Fields {
		if f.Status != cv.Unverified {
			t.Errorf("%s status = %q, want unverified", f.Field, f.Status)
		}
		if !f.NeedsReview {
			t.Errorf("%s was not flagged for review", f.Field)
		}
		// A claimed high confidence must not survive failed verification, or the UI
		// would present invented data as reliable.
		if f.Confidence >= cv.ConfidenceHigh.Score() {
			t.Errorf("%s kept confidence %.2f despite failing verification", f.Field, f.Confidence)
		}
		if f.ModelConfidence != cv.ConfidenceHigh {
			t.Errorf("%s lost the model's claimed confidence; a confidently-wrong model should stay visible", f.Field)
		}
	}
}

func TestParaphrasedEvidenceIsCaught(t *testing.T) {
	// Subtler than fabrication: the value is right but the model wrote its own summary
	// instead of quoting. That means we cannot prove where the value came from, so it
	// cannot be treated as verified.
	extracted := cv.ExtractedCV{
		Experience: []cv.ExtractedRole{{
			Employer:   "Acme Corporation",
			Evidence:   "The candidate worked at Acme as a senior engineer from 2021",
			Confidence: cv.ConfidenceHigh,
		}},
	}

	report := cv.VerifyExtraction(extracted, sourceCV)
	if report.Verified != 0 {
		t.Fatal("a paraphrased quote was accepted as verification")
	}
	if !report.Fields[0].NeedsReview {
		t.Error("the paraphrased entry was not flagged for review")
	}
}

func TestCaseInsensitiveMatchIsVerifiedButPenalised(t *testing.T) {
	extracted := cv.ExtractedCV{
		Skills: []cv.ExtractedSkill{{
			Name:       "postgresql",
			Evidence:   "go, postgresql, redis",
			Confidence: cv.ConfidenceHigh,
		}},
	}

	report := cv.VerifyExtraction(extracted, sourceCV)
	if report.Verified != 1 {
		t.Fatalf("verified = %d, want 1", report.Verified)
	}
	field := report.Fields[0]
	if field.Status != cv.VerifiedCaseInsensitive {
		t.Fatalf("status = %q, want case_insensitive", field.Status)
	}
	// Verified, but the model reformatted something it was told to copy verbatim.
	if field.Confidence >= cv.ConfidenceHigh.Score() {
		t.Errorf("confidence %.2f was not penalised for inexact copying", field.Confidence)
	}
}

func TestMissingEvidenceForAPresentValueIsFlagged(t *testing.T) {
	extracted := cv.ExtractedCV{
		FullName: evidenced("Joakim Candidate", "", cv.ConfidenceHigh),
	}
	report := cv.VerifyExtraction(extracted, sourceCV)

	if report.Unverified != 1 {
		t.Fatalf("unverified = %d, want 1", report.Unverified)
	}
	if report.Fields[0].Status != cv.NoEvidence {
		t.Errorf("status = %q, want no_evidence", report.Fields[0].Status)
	}
}

func TestAbsentValuesNeedNoEvidence(t *testing.T) {
	// An empty value is a correct answer. Demanding evidence for it would flag every
	// CV that simply does not list a phone number.
	extracted := cv.ExtractedCV{
		FullName: evidenced("Joakim Candidate", "Joakim Candidate", cv.ConfidenceHigh),
		Phone:    evidenced("", "", ""),
		Email:    evidenced("  ", "", ""),
		NotFound: []string{"phone", "email"},
	}

	report := cv.VerifyExtraction(extracted, sourceCV)
	if len(report.Fields) != 1 {
		t.Fatalf("%d fields were checked, want 1; absent values must not be checked", len(report.Fields))
	}
	if report.Unverified != 0 {
		t.Fatalf("unverified = %d, want 0", report.Unverified)
	}
}

func TestAcceptableRejectsAMostlyFabricatedExtraction(t *testing.T) {
	// A third of entries unlocatable means the model was not reading the document.
	// Persisting that gives the candidate a profile full of invented detail that
	// matching then acts on.
	var roles []cv.ExtractedRole
	// Two real, eight invented.
	roles = append(roles,
		cv.ExtractedRole{Employer: "Acme Corporation", Evidence: "Acme Corporation", Confidence: cv.ConfidenceHigh},
		cv.ExtractedRole{Employer: "Globex Ltd", Evidence: "Globex Ltd", Confidence: cv.ConfidenceHigh},
	)
	for i := 0; i < 8; i++ {
		roles = append(roles, cv.ExtractedRole{
			Employer:   "Invented Co " + string(rune('A'+i)),
			Evidence:   "Invented Co " + string(rune('A'+i)),
			Confidence: cv.ConfidenceHigh,
		})
	}

	report := cv.VerifyExtraction(cv.ExtractedCV{Experience: roles}, sourceCV)

	if report.Acceptable() {
		t.Fatalf("an extraction with %.0f%% unverified entries was accepted",
			report.UnverifiedRatio()*100)
	}
}

func TestAcceptableToleratesAFewUnlocatableQuotes(t *testing.T) {
	// PDF extraction mangles text and a model occasionally tidies a quote. Rejecting a
	// whole extraction over one bad quote would fail most real CVs.
	roles := []cv.ExtractedRole{
		{Employer: "Acme Corporation", Evidence: "Acme Corporation", Confidence: cv.ConfidenceHigh},
		{Employer: "Globex Ltd", Evidence: "Globex Ltd", Confidence: cv.ConfidenceHigh},
	}
	skills := []cv.ExtractedSkill{
		{Name: "Go", Evidence: "Go, PostgreSQL", Confidence: cv.ConfidenceHigh},
		{Name: "Kubernetes", Evidence: "Kubernetes, Terraform", Confidence: cv.ConfidenceHigh},
		{Name: "Rust", Evidence: "Rust and WebAssembly", Confidence: cv.ConfidenceMedium}, // not in the CV
	}

	report := cv.VerifyExtraction(cv.ExtractedCV{Experience: roles, Skills: skills}, sourceCV)

	if !report.Acceptable() {
		t.Fatalf("one bad quote in five entries was rejected (ratio %.2f)", report.UnverifiedRatio())
	}
	if report.Unverified != 1 {
		t.Fatalf("unverified = %d, want 1", report.Unverified)
	}
}

func TestAcceptableRejectsAWhollyInventedSparseExtraction(t *testing.T) {
	// Below the ratio threshold's sample size, a single verified entry is required —
	// otherwise a two-entry extraction with both invented would pass on sample size.
	report := cv.VerifyExtraction(cv.ExtractedCV{
		Experience: []cv.ExtractedRole{
			{Employer: "Nowhere Inc", Evidence: "Nowhere Inc", Confidence: cv.ConfidenceHigh},
		},
	}, sourceCV)

	if report.Acceptable() {
		t.Fatal("a sparse extraction with nothing verified was accepted")
	}
}

func TestEmptyExtractionIsAcceptable(t *testing.T) {
	// An empty CV is a legitimate outcome for a document with nothing extractable, and
	// is handled by the parse-failure path rather than by rejecting verification.
	report := cv.VerifyExtraction(cv.ExtractedCV{}, sourceCV)
	if !report.Acceptable() {
		t.Fatal("an empty extraction was rejected")
	}
	if report.Verified != 0 || report.Unverified != 0 {
		t.Fatalf("verified=%d unverified=%d, want 0/0", report.Verified, report.Unverified)
	}
}

func TestAverageConfidenceExcludesUnverifiedEntries(t *testing.T) {
	// Including them would let fabricated entries drag the average down and disguise a
	// mostly-good extraction as a bad one.
	report := cv.VerifyExtraction(cv.ExtractedCV{
		Experience: []cv.ExtractedRole{
			{Employer: "Acme Corporation", Evidence: "Acme Corporation", Confidence: cv.ConfidenceHigh},
			{Employer: "Nowhere Inc", Evidence: "Nowhere Inc", Confidence: cv.ConfidenceHigh},
		},
	}, sourceCV)

	if report.AverageConfidence < cv.ConfidenceHigh.Score()*0.9 {
		t.Fatalf("AverageConfidence = %.2f; the unverified entry dragged it down", report.AverageConfidence)
	}
}

func TestLowModelConfidenceIsFlaggedForReviewEvenWhenVerified(t *testing.T) {
	// A located quote proves where a value came from, not that the model read the
	// layout correctly. Low confidence still deserves a human look.
	extracted := cv.ExtractedCV{
		Experience: []cv.ExtractedRole{{
			Employer:   "Globex Ltd",
			Evidence:   "Globex Ltd",
			Confidence: cv.ConfidenceLow,
		}},
	}
	report := cv.VerifyExtraction(extracted, sourceCV)

	if report.Fields[0].Status != cv.VerifiedExact {
		t.Fatalf("status = %q, want exact", report.Fields[0].Status)
	}
	if !report.Fields[0].NeedsReview {
		t.Error("a low-confidence but verified field was not flagged for review")
	}
	if report.NeedsReview != 1 {
		t.Errorf("NeedsReview = %d, want 1", report.NeedsReview)
	}
}

func TestFieldPathsIdentifyListEntries(t *testing.T) {
	// The review UI needs to know which entry a flag belongs to.
	extracted := cv.ExtractedCV{
		Experience: []cv.ExtractedRole{
			{Employer: "Acme Corporation", Evidence: "Acme Corporation", Confidence: cv.ConfidenceHigh},
			{Employer: "Globex Ltd", Evidence: "Globex Ltd", Confidence: cv.ConfidenceHigh},
		},
	}
	report := cv.VerifyExtraction(extracted, sourceCV)

	paths := make(map[string]bool, len(report.Fields))
	for _, f := range report.Fields {
		paths[f.Field] = true
	}
	for _, want := range []string{"experience[0].employer", "experience[1].employer"} {
		if !paths[want] {
			t.Errorf("missing field path %q; got %v", want, paths)
		}
	}
}

func TestConfidenceLevels(t *testing.T) {
	for _, c := range []cv.Confidence{cv.ConfidenceHigh, cv.ConfidenceMedium, cv.ConfidenceLow} {
		if !c.Valid() {
			t.Errorf("Confidence(%q).Valid() = false", c)
		}
		if c.Score() <= 0 || c.Score() > 1 {
			t.Errorf("Confidence(%q).Score() = %v, want a value in (0,1]", c, c.Score())
		}
	}
	for _, c := range []cv.Confidence{"", "HIGH", "certain"} {
		if cv.Confidence(c).Valid() {
			t.Errorf("Confidence(%q).Valid() = true, want false", c)
		}
		if cv.Confidence(c).Score() != 0 {
			t.Errorf("Confidence(%q).Score() = %v, want 0", c, cv.Confidence(c).Score())
		}
	}
	if cv.ConfidenceHigh.Score() <= cv.ConfidenceMedium.Score() ||
		cv.ConfidenceMedium.Score() <= cv.ConfidenceLow.Score() {
		t.Error("confidence scores are not ordered high > medium > low")
	}
}

func TestVerificationStatusTrustworthiness(t *testing.T) {
	trustworthy := []cv.VerificationStatus{
		cv.VerifiedExact, cv.VerifiedNormalised, cv.VerifiedCaseInsensitive,
	}
	for _, s := range trustworthy {
		if !s.Trustworthy() {
			t.Errorf("%q.Trustworthy() = false", s)
		}
	}
	for _, s := range []cv.VerificationStatus{cv.Unverified, cv.NoEvidence, "unknown"} {
		if s.Trustworthy() {
			t.Errorf("%q.Trustworthy() = true, want false", s)
		}
	}
}

func TestExtractionSchemaShape(t *testing.T) {
	schema := cv.ExtractionSchema()

	// A loose schema lets the model omit fields and the result validates while being
	// useless, so every top-level field must be required and extras forbidden.
	if schema["additionalProperties"] != false {
		t.Error("the schema permits additional properties")
	}
	required, ok := schema["required"].([]string)
	if !ok {
		t.Fatalf("required is %T, want []string", schema["required"])
	}
	for _, field := range []string{
		"full_name", "experience", "education", "skills",
		"certifications", "languages", "not_found",
	} {
		found := false
		for _, r := range required {
			if r == field {
				found = true
			}
		}
		if !found {
			t.Errorf("%q is not required by the schema", field)
		}
	}

	// Evidence must be required on list items, or the verification check has nothing to
	// work with.
	props := schema["properties"].(map[string]any)
	experience := props["experience"].(map[string]any)
	items := experience["items"].(map[string]any)
	itemRequired := items["required"].([]string)

	hasEvidence := false
	for _, r := range itemRequired {
		if r == "evidence" {
			hasEvidence = true
		}
	}
	if !hasEvidence {
		t.Error("experience items do not require an evidence quote")
	}
}

func TestExtractionPromptForbidsInference(t *testing.T) {
	// These are the rules that keep a fabricated line off a candidate's CV. If one is
	// deleted, this test should fail.
	role, task, label := cv.ExtractionPrompt("some cv text")

	// Matched case-insensitively: the assertion is that the rule is present, not how
	// it happens to be capitalised in a sentence.
	lowered := strings.ToLower(role)
	for _, rule := range []string{
		"never infer",
		"copy values as written",
		"do not infer skills from job titles",
		"verbatim",
		"not_found",
	} {
		if !strings.Contains(lowered, rule) {
			t.Errorf("the extraction role is missing the rule %q", rule)
		}
	}
	if task == "" {
		t.Error("the extraction task is empty")
	}
	if label != "CANDIDATE_CV" {
		t.Errorf("label = %q, want CANDIDATE_CV", label)
	}
	// The CV text must not be baked into the prompt — it goes in a data block.
	if strings.Contains(role, "some cv text") || strings.Contains(task, "some cv text") {
		t.Fatal("the CV text leaked into the prompt instructions")
	}
}

func utf8Valid(s string) bool {
	for _, r := range s {
		if r == '�' {
			return false
		}
	}
	return true
}
