package cv

// The structured-extraction contract.
//
// One design decision dominates this file: the model is asked for a verbatim EVIDENCE
// QUOTE for each extracted item, and the character offset is computed by locating that
// quote in the source text. It is not asked for offsets directly.
//
// Models are unreliable at counting characters, so a model-reported offset is usually
// wrong and always unverifiable. An evidence quote is verifiable: if the quote is not
// present verbatim in the CV, the model paraphrased or invented the item, and we can
// detect that. That makes the evidence requirement a cheap hallucination detector as
// well as the provenance the review UI needs.

// ExtractedCV is the structured form of a CV.
//
// Field names match the JSON schema below exactly. A mismatch would surface as a
// validated-but-empty struct, which is why the AI layer reports that case loudly.
type ExtractedCV struct {
	FullName       EvidencedString  `json:"full_name"`
	Email          EvidencedString  `json:"email"`
	Phone          EvidencedString  `json:"phone"`
	Location       EvidencedString  `json:"location"`
	Headline       EvidencedString  `json:"headline"`
	Summary        EvidencedString  `json:"summary"`
	Experience     []ExtractedRole  `json:"experience"`
	Education      []ExtractedStudy `json:"education"`
	Skills         []ExtractedSkill `json:"skills"`
	Certifications []ExtractedCert  `json:"certifications"`
	Languages      []ExtractedLang  `json:"languages"`
	// NotFound lists fields the model looked for and could not find. Asking for this
	// explicitly is what stops a model filling a gap with something plausible: it is
	// given a legitimate way to say "absent".
	NotFound []string `json:"not_found"`
}

// Confidence is the model's own assessment of an extracted value.
type Confidence string

const (
	ConfidenceHigh   Confidence = "high"
	ConfidenceMedium Confidence = "medium"
	ConfidenceLow    Confidence = "low"
)

// Valid reports whether c is a recognised level.
func (c Confidence) Valid() bool {
	switch c {
	case ConfidenceHigh, ConfidenceMedium, ConfidenceLow:
		return true
	default:
		return false
	}
}

// Score maps a level to a number for storage and for the review threshold.
func (c Confidence) Score() float64 {
	switch c {
	case ConfidenceHigh:
		return 0.95
	case ConfidenceMedium:
		return 0.7
	case ConfidenceLow:
		return 0.4
	default:
		return 0
	}
}

// EvidencedString is a single extracted value with its supporting quote.
type EvidencedString struct {
	Value      string     `json:"value"`
	Evidence   string     `json:"evidence"`
	Confidence Confidence `json:"confidence"`
}

// ExtractedRole is one employment entry.
type ExtractedRole struct {
	Title        string     `json:"title"`
	Employer     string     `json:"employer"`
	Location     string     `json:"location"`
	StartDate    string     `json:"start_date"`
	EndDate      string     `json:"end_date"`
	IsCurrent    bool       `json:"is_current"`
	Description  string     `json:"description"`
	Achievements []string   `json:"achievements"`
	Evidence     string     `json:"evidence"`
	Confidence   Confidence `json:"confidence"`
}

// ExtractedStudy is one education entry.
type ExtractedStudy struct {
	Qualification string     `json:"qualification"`
	Field         string     `json:"field"`
	Institution   string     `json:"institution"`
	StartDate     string     `json:"start_date"`
	EndDate       string     `json:"end_date"`
	Grade         string     `json:"grade"`
	Evidence      string     `json:"evidence"`
	Confidence    Confidence `json:"confidence"`
}

// ExtractedSkill is one skill.
type ExtractedSkill struct {
	Name string `json:"name"`
	// Category is the model's grouping, used to order the review UI. Advisory only —
	// canonical categorisation comes from the skill taxonomy.
	Category   string     `json:"category"`
	Evidence   string     `json:"evidence"`
	Confidence Confidence `json:"confidence"`
}

// ExtractedCert is one certification.
type ExtractedCert struct {
	Name       string     `json:"name"`
	Issuer     string     `json:"issuer"`
	IssuedDate string     `json:"issued_date"`
	Evidence   string     `json:"evidence"`
	Confidence Confidence `json:"confidence"`
}

// ExtractedLang is one language.
type ExtractedLang struct {
	Language    string     `json:"language"`
	Proficiency string     `json:"proficiency"`
	Evidence    string     `json:"evidence"`
	Confidence  Confidence `json:"confidence"`
}

// evidencedStringSchema is the schema fragment for a single evidenced value.
func evidencedStringSchema(description string) map[string]any {
	return map[string]any{
		"type":        "object",
		"description": description,
		"properties": map[string]any{
			"value": map[string]any{
				"type":        "string",
				"description": "The extracted value, exactly as written in the CV. Empty string if absent.",
			},
			"evidence": map[string]any{
				"type": "string",
				"description": "A short VERBATIM quote from the CV containing this value, " +
					"copied character for character. Empty string if the value is absent.",
			},
			"confidence": map[string]any{
				"type": "string",
				"enum": []string{"high", "medium", "low"},
			},
		},
		"required":             []string{"value", "evidence", "confidence"},
		"additionalProperties": false,
	}
}

// evidenceProperty is the shared evidence field for list items.
var evidenceProperty = map[string]any{
	"type": "string",
	"description": "A short VERBATIM quote from the CV supporting this entry, copied " +
		"character for character from the source. Do not paraphrase, correct spelling, " +
		"or reformat — the quote is located in the source text to verify the entry.",
}

var confidenceProperty = map[string]any{
	"type": "string",
	"enum": []string{"high", "medium", "low"},
}

// ExtractionSchema returns the JSON schema the model's response must satisfy.
//
// additionalProperties is false throughout and every field is required, which is what
// structured outputs needs to constrain generation tightly. A loose schema lets the
// model omit fields and the result validates while being useless.
func ExtractionSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"full_name": evidencedStringSchema("The candidate's full name."),
			"email":     evidencedStringSchema("The candidate's email address."),
			"phone":     evidencedStringSchema("The candidate's phone number, as written."),
			"location":  evidencedStringSchema("The candidate's location, as written."),
			"headline":  evidencedStringSchema("A professional headline or current title, if the CV states one."),
			"summary":   evidencedStringSchema("The CV's own summary or profile section, if present."),

			"experience": map[string]any{
				"type":        "array",
				"description": "Employment history, most recent first.",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"title":    map[string]any{"type": "string", "description": "Job title exactly as written."},
						"employer": map[string]any{"type": "string", "description": "Employer name exactly as written."},
						"location": map[string]any{"type": "string", "description": "Role location, or empty string."},
						"start_date": map[string]any{
							"type": "string",
							"description": "Start date in the format written in the CV (e.g. '2019', " +
								"'Jan 2019', '2019-01'). Do not normalise or infer a date the CV does not give.",
						},
						"end_date": map[string]any{
							"type":        "string",
							"description": "End date as written, or empty string if the role is current.",
						},
						"is_current":  map[string]any{"type": "boolean"},
						"description": map[string]any{"type": "string", "description": "Role description as written, or empty string."},
						"achievements": map[string]any{
							"type":        "array",
							"items":       map[string]any{"type": "string"},
							"description": "Bullet points under this role, each as written.",
						},
						"evidence":   evidenceProperty,
						"confidence": confidenceProperty,
					},
					"required": []string{
						"title", "employer", "location", "start_date", "end_date",
						"is_current", "description", "achievements", "evidence", "confidence",
					},
					"additionalProperties": false,
				},
			},

			"education": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"qualification": map[string]any{"type": "string", "description": "Degree or qualification as written."},
						"field":         map[string]any{"type": "string", "description": "Subject or field, or empty string."},
						"institution":   map[string]any{"type": "string"},
						"start_date":    map[string]any{"type": "string", "description": "As written, or empty string."},
						"end_date":      map[string]any{"type": "string", "description": "As written, or empty string."},
						"grade":         map[string]any{"type": "string", "description": "Grade or classification, or empty string."},
						"evidence":      evidenceProperty,
						"confidence":    confidenceProperty,
					},
					"required": []string{
						"qualification", "field", "institution", "start_date",
						"end_date", "grade", "evidence", "confidence",
					},
					"additionalProperties": false,
				},
			},

			"skills": map[string]any{
				"type":        "array",
				"description": "Skills the CV explicitly states. Do not infer skills from job titles.",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"name":       map[string]any{"type": "string", "description": "Skill name as written."},
						"category":   map[string]any{"type": "string", "description": "A grouping such as 'language', 'framework', 'tool', 'other'."},
						"evidence":   evidenceProperty,
						"confidence": confidenceProperty,
					},
					"required":             []string{"name", "category", "evidence", "confidence"},
					"additionalProperties": false,
				},
			},

			"certifications": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"name":        map[string]any{"type": "string"},
						"issuer":      map[string]any{"type": "string", "description": "Issuing body, or empty string."},
						"issued_date": map[string]any{"type": "string", "description": "As written, or empty string."},
						"evidence":    evidenceProperty,
						"confidence":  confidenceProperty,
					},
					"required":             []string{"name", "issuer", "issued_date", "evidence", "confidence"},
					"additionalProperties": false,
				},
			},

			"languages": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"language":    map[string]any{"type": "string"},
						"proficiency": map[string]any{"type": "string", "description": "As written, or empty string."},
						"evidence":    evidenceProperty,
						"confidence":  confidenceProperty,
					},
					"required":             []string{"language", "proficiency", "evidence", "confidence"},
					"additionalProperties": false,
				},
			},

			"not_found": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "string",
					"enum": []string{
						"full_name", "email", "phone", "location", "headline", "summary",
						"experience", "education", "skills", "certifications", "languages",
					},
				},
				"description": "Fields you looked for and could not find in the CV. " +
					"Use this instead of guessing a plausible value.",
			},
		},
		"required": []string{
			"full_name", "email", "phone", "location", "headline", "summary",
			"experience", "education", "skills", "certifications", "languages", "not_found",
		},
		"additionalProperties": false,
	}
}

// extractionRole is the stable, cacheable part of the extraction prompt.
//
// Identical across every extraction call, so it carries the cache breakpoint. The rules
// about not inferring and not normalising are here rather than in the per-call task for
// exactly that reason — and because they are the rules that matter most.
const extractionRole = `You extract structured data from CVs for a job-application platform.

Your output is used to match the candidate against real job postings and, later, to
prepare applications sent to real employers in the candidate's name. An invented detail
becomes a lie the candidate has to defend in an interview. Accuracy matters more than
completeness.

Rules:

1. Extract only what the CV states. Never infer, never complete a partial detail, and
   never add an employer, title, date, qualification, certification or skill that is not
   written in the document.
2. Copy values as written. Do not normalise dates, expand abbreviations, correct
   spelling, or reformat. "Jan 2019" stays "Jan 2019"; it does not become "2019-01".
3. Do not infer skills from job titles or employers. A "Senior Go Engineer" has the
   skills the CV lists, not the skills the title implies.
4. For every entry, provide a short VERBATIM quote from the CV in the evidence field,
   copied character for character. The quote is located in the source text to verify the
   entry, so a paraphrase will cause the entry to be flagged.
5. If a field is absent, return an empty string (or an empty array) and list the field
   name in not_found. An empty value is a correct answer; a plausible guess is not.
6. Set confidence honestly: high when the CV states the value plainly, medium when the
   layout makes it ambiguous, low when you are reading between the lines. A low-confidence
   entry is shown to the candidate for review, so marking uncertainty is useful rather
   than a failure.`

// extractionTask is the per-call instruction. Deliberately short: everything stable
// lives in the role, where it caches.
const extractionTask = `Extract the structured data defined by the response schema from
the CV in the data block. Follow every rule in your instructions, in particular the
evidence quotes and the not_found list.`

// ExtractionPrompt builds the prompt for a CV's raw text.
func ExtractionPrompt(rawText string) (role, task, label string) {
	return extractionRole, extractionTask, "CANDIDATE_CV"
}
