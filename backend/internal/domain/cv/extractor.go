package cv

import (
	"context"
	"fmt"

	"github.com/Joakim-animate90/myjobfyt/backend/internal/platform/ai"
)

// AIExtractor implements Extractor over the AI client.
//
// Thin by design: the schema and prompt live in this domain, the call policy lives in
// the AI layer, and this type only joins them. There is no logic here worth testing
// separately, which is the point — anything that needed testing would belong on one
// side or the other.
type AIExtractor struct {
	client *ai.Client
	// perCallTokenBudget caps one extraction independently of the monthly quota, so a
	// single enormous CV cannot consume a candidate's month.
	perCallTokenBudget int64
	engineVersion      string
}

// ExtractorConfig configures the extractor.
type ExtractorConfig struct {
	// PerCallTokenBudget defaults to defaultExtractionBudget.
	PerCallTokenBudget int64
	// Model overrides the AI layer's default. Normally left empty: extraction is the
	// call where accuracy matters most, so it gets the most capable model.
	Model ai.Model
	// Effort overrides the default. Extraction reads a messy document and decides what
	// is and is not stated, which rewards thinking, so it runs above the baseline.
	Effort ai.Effort
}

// defaultExtractionBudget bounds one extraction.
//
// Sized for a long CV plus a schema-heavy response plus two repair attempts. Beyond
// this the document is not a CV, and spending more on it is not in the candidate's
// interest either.
const defaultExtractionBudget = 80_000

// extractionEngineVersion identifies the prompt and schema that produced a result.
//
// Stored on every parsed row so an extraction can be traced to the logic that produced
// it. Bump it whenever the prompt or schema changes materially, otherwise a quality
// regression cannot be attributed and old rows cannot be told apart from new ones.
const extractionEngineVersion = "cv-extract-v1"

// NewAIExtractor builds the extractor.
func NewAIExtractor(client *ai.Client, cfg ExtractorConfig) *AIExtractor {
	budget := cfg.PerCallTokenBudget
	if budget <= 0 {
		budget = defaultExtractionBudget
	}
	return &AIExtractor{
		client:             client,
		perCallTokenBudget: budget,
		engineVersion:      extractionEngineVersion,
	}
}

// ExtractCV runs structured extraction over a CV's text.
func (e *AIExtractor) ExtractCV(ctx context.Context, rawText string) (ExtractedCV, ExtractionMeta, error) {
	role, task, label := ExtractionPrompt(rawText)

	req := ai.Request{
		Purpose: ai.PurposeCVExtract,
		Prompt: ai.Prompt{
			Role: role,
			Task: task,
			// The CV goes in a data block, never in the instructions. That is the only
			// route this layer offers, which is what makes it reliable.
			Data: []ai.UntrustedBlock{{Label: label, Content: rawText}},
		},
		Schema:         ExtractionSchema(),
		Effort:         ai.EffortHigh,
		MaxTokenBudget: e.perCallTokenBudget,
	}

	extracted, resp, err := ai.Generate[ExtractedCV](ctx, e.client, req)
	meta := ExtractionMeta{
		Model:         string(resp.Model),
		EngineVersion: e.engineVersion,
		TotalTokens:   resp.Usage.TotalTokens(),
		Attempts:      resp.Attempts,
	}
	if err != nil {
		return ExtractedCV{}, meta, err
	}

	// The schema constrains structure, not semantics. A response can satisfy it while
	// claiming a confidence level that does not exist, so the enum is checked here
	// before the value reaches scoring.
	if err := validateConfidenceLevels(extracted); err != nil {
		return ExtractedCV{}, meta, fmt.Errorf("%w: %v", ai.ErrSchemaViolation, err)
	}

	return extracted, meta, nil
}

// validateConfidenceLevels rejects an unrecognised confidence value.
//
// An unknown level scores 0, which would silently floor every field's confidence and
// flag a perfectly good extraction for review. Catching it as a schema violation gets
// one repair attempt instead.
func validateConfidenceLevels(extracted ExtractedCV) error {
	check := func(field string, value string, c Confidence) error {
		// A level is only required where there is a value to be confident about.
		if value == "" {
			return nil
		}
		if !c.Valid() {
			return fmt.Errorf("field %s has confidence %q, which is not one of high, medium or low",
				field, c)
		}
		return nil
	}

	if err := check("full_name", extracted.FullName.Value, extracted.FullName.Confidence); err != nil {
		return err
	}
	if err := check("email", extracted.Email.Value, extracted.Email.Confidence); err != nil {
		return err
	}
	if err := check("phone", extracted.Phone.Value, extracted.Phone.Confidence); err != nil {
		return err
	}
	if err := check("location", extracted.Location.Value, extracted.Location.Confidence); err != nil {
		return err
	}
	if err := check("headline", extracted.Headline.Value, extracted.Headline.Confidence); err != nil {
		return err
	}
	if err := check("summary", extracted.Summary.Value, extracted.Summary.Confidence); err != nil {
		return err
	}

	for i, r := range extracted.Experience {
		if err := check(fieldPath("experience", i, "confidence"), r.Employer, r.Confidence); err != nil {
			return err
		}
	}
	for i, s := range extracted.Education {
		if err := check(fieldPath("education", i, "confidence"), s.Institution, s.Confidence); err != nil {
			return err
		}
	}
	for i, s := range extracted.Skills {
		if err := check(fieldPath("skills", i, "confidence"), s.Name, s.Confidence); err != nil {
			return err
		}
	}
	for i, c := range extracted.Certifications {
		if err := check(fieldPath("certifications", i, "confidence"), c.Name, c.Confidence); err != nil {
			return err
		}
	}
	for i, l := range extracted.Languages {
		if err := check(fieldPath("languages", i, "confidence"), l.Language, l.Confidence); err != nil {
			return err
		}
	}
	return nil
}
