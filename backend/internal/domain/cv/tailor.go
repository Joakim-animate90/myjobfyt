package cv

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/Joakim-animate90/myjobfyt/backend/internal/platform/ai"
)

// Tailorer produces a tailored CV. An interface so the service can be tested against a
// provider that fabricates on demand, which is the case that matters.
type Tailorer interface {
	TailorCV(ctx context.Context, input TailoringInput) (TailoredCV, TailoringMeta, error)
}

// TailoringInput is one tailoring attempt.
type TailoringInput struct {
	// SourceCV is the candidate's parsed CV, the sole permitted source of fact.
	SourceCV *ParsedData
	// SourceSkills are the candidate's skills, which the model may reorder but not add to.
	SourceSkills []Skill
	// JobDescription is attacker-controlled text. It never reaches an instruction
	// position.
	JobDescription string
	// PreviousViolations, when set, makes this the stricter retry and names exactly
	// what the last attempt fabricated.
	PreviousViolations []Violation
}

// TailoringMeta reports what the call cost and which model produced it.
type TailoringMeta struct {
	Model         string
	EngineVersion string
	TotalTokens   int64
	Attempts      int
	// InjectionAttempt is text the model found in the job description that was
	// addressed to it. Worth recording: it is evidence of an attack on a specific
	// candidate's application.
	InjectionAttempt string
}

// AITailorer implements Tailorer over the AI client.
type AITailorer struct {
	client             *ai.Client
	perCallTokenBudget int64
}

// TailorerConfig configures the tailorer.
type TailorerConfig struct {
	PerCallTokenBudget int64
}

// defaultTailoringBudget bounds one tailoring attempt, including its stricter retry.
const defaultTailoringBudget = 60_000

// NewAITailorer builds the tailorer.
func NewAITailorer(client *ai.Client, cfg TailorerConfig) *AITailorer {
	budget := cfg.PerCallTokenBudget
	if budget <= 0 {
		budget = defaultTailoringBudget
	}
	return &AITailorer{client: client, perCallTokenBudget: budget}
}

// TailorCV runs one tailoring attempt.
func (t *AITailorer) TailorCV(ctx context.Context, input TailoringInput) (TailoredCV, TailoringMeta, error) {
	if input.SourceCV == nil {
		return TailoredCV{}, TailoringMeta{}, fmt.Errorf("tailoring requires a parsed source CV")
	}

	sourceJSON, err := json.Marshal(sourceFactsFor(input.SourceCV, input.SourceSkills))
	if err != nil {
		return TailoredCV{}, TailoringMeta{}, fmt.Errorf("marshal source CV: %w", err)
	}

	role, task := TailoringPrompt(input.PreviousViolations)

	req := ai.Request{
		Purpose: ai.PurposeCVTailor,
		Prompt: ai.Prompt{
			Role: role,
			Task: task,
			// Both blocks are untrusted. The CV is the candidate's own document and the
			// job description is a stranger's; neither reaches an instruction position.
			Data: []ai.UntrustedBlock{
				{Label: "CANDIDATE_CV", Content: string(sourceJSON)},
				{Label: "JOB_DESCRIPTION", Content: input.JobDescription},
			},
		},
		Schema:         TailoringSchema(),
		Effort:         ai.EffortHigh,
		MaxTokenBudget: t.perCallTokenBudget,
	}

	tailored, resp, err := ai.Generate[TailoredCV](ctx, t.client, req)
	meta := TailoringMeta{
		Model:            string(resp.Model),
		EngineVersion:    tailoringEngineVersion,
		TotalTokens:      resp.Usage.TotalTokens(),
		Attempts:         resp.Attempts,
		InjectionAttempt: tailored.InjectionAttempt,
	}
	if err != nil {
		return TailoredCV{}, meta, err
	}
	return tailored, meta, nil
}

// sourceFacts is the CV as the model is allowed to see it.
//
// Deliberately not the whole ParsedData: provenance, confidence scores and the
// candidate's corrected-field list are our bookkeeping, and including them invites the
// model to reason about which facts are "uncertain" and therefore adjustable. Every
// fact here is equally fixed.
type sourceFacts struct {
	Headline       string          `json:"headline"`
	Summary        string          `json:"summary"`
	Experience     []Role          `json:"experience"`
	Education      []Study         `json:"education"`
	Certifications []Certification `json:"certifications"`
	Skills         []string        `json:"skills"`
}

func sourceFactsFor(source *ParsedData, skills []Skill) sourceFacts {
	labels := make([]string, 0, len(skills))
	for _, skill := range skills {
		labels = append(labels, skill.RawLabel)
	}
	return sourceFacts{
		Headline:       source.Contact.Headline,
		Summary:        source.Summary,
		Experience:     source.Experience,
		Education:      source.Education,
		Certifications: source.Certifications,
		Skills:         labels,
	}
}
