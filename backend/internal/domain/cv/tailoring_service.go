package cv

import (
	"context"
	"log/slog"

	"github.com/Joakim-animate90/myjobfyt/backend/internal/apperr"
	"github.com/google/uuid"
)

// TailoringOutcome is the result of a tailoring request.
//
// Carries the report and the diff whether or not it succeeded, because a rejection is
// something the candidate is entitled to see: it tells them the platform refused to
// make a claim on their behalf.
type TailoringOutcome struct {
	// Tailored is the validated CV. Nil unless Containment.Passed.
	Tailored *ParsedData
	// Containment is the verdict from the final attempt.
	Containment ContainmentReport
	// Diff is what changed against the source, for the candidate to review before the
	// tailored CV is used anywhere.
	Diff TailoringDiff
	Meta TailoringMeta
	// Attempts counts generation attempts, so a model that needs the stricter retry
	// routinely is visible rather than merely slow.
	Attempts int
}

// TailoringService turns a job description into a tailored CV, or refuses.
type TailoringService struct {
	repo    Repository
	tailor  Tailorer
	log     *slog.Logger
	maxTrys int
}

// NewTailoringService builds the service.
func NewTailoringService(repo Repository, tailor Tailorer, log *slog.Logger) *TailoringService {
	return &TailoringService{
		repo:   repo,
		tailor: tailor,
		log:    log,
		// One generation plus one stricter retry. A third attempt is not worth a
		// candidate's tokens: a model that has fabricated twice, the second time with
		// its own violations quoted back at it, is not going to stop on the third.
		maxTrys: 2,
	}
}

// ErrTailoringUnsafe reports that tailoring could not be completed without fabricating.
//
// Deliberately a distinct, terminal error. The caller must never fall back to showing
// the unvalidated output, so this cannot be confused with a transient provider failure
// that would be retried.
var ErrTailoringUnsafe = apperr.Unprocessable("tailoring_unsafe",
	"We could not tailor this CV without claiming something it does not say, so we did not change it.")

// TailorForJob produces a tailored CV for a job, or refuses to produce one.
//
// The containment validator is the gate, not the prompt. Every path out of this
// function either returns output that has passed it or returns an error; there is
// deliberately no branch that returns unvalidated output, however many attempts were
// made or however close the result looked.
func (s *TailoringService) TailorForJob(
	ctx context.Context, userID, versionID uuid.UUID, jobDescription string,
) (TailoringOutcome, error) {
	source, err := s.repo.ParsedDataByVersion(ctx, userID, versionID)
	if err != nil {
		return TailoringOutcome{}, err
	}
	skills, err := s.repo.SkillsByVersion(ctx, userID, versionID)
	if err != nil {
		return TailoringOutcome{}, err
	}

	log := s.log.With(
		slog.String("cv_version_id", versionID.String()),
		slog.String("user_id", userID.String()),
	)

	var (
		outcome    TailoringOutcome
		violations []Violation
	)

	for attempt := 1; attempt <= s.maxTrys; attempt++ {
		tailored, meta, err := s.tailor.TailorCV(ctx, TailoringInput{
			SourceCV:           source,
			SourceSkills:       skills,
			JobDescription:     jobDescription,
			PreviousViolations: violations,
		})
		outcome.Meta = meta
		outcome.Attempts = attempt
		if err != nil {
			// A provider failure is not a containment failure, and must stay
			// distinguishable: one is retryable, the other is a refusal.
			return outcome, err
		}

		if meta.InjectionAttempt != "" {
			// Recorded at warn: this is evidence of an attack aimed at one candidate's
			// application. The quoted text is attacker-controlled, so it is not logged;
			// its presence is what matters here.
			log.WarnContext(ctx, "the job description contained text addressed to the model",
				slog.Int("attempt", attempt),
				slog.Int("injection_text_length", len(meta.InjectionAttempt)),
			)
		}

		candidate := tailored.ToParsedData()
		candidate.CVVersionID = versionID
		candidate.UserID = userID

		report := CheckContainment(source, skills, candidate)
		outcome.Containment = report

		if report.Passed {
			outcome.Tailored = candidate
			outcome.Diff = DiffTailoring(source, candidate, tailored.Omitted, tailored.Rationale)
			log.InfoContext(ctx, "cv tailored",
				slog.Int("attempt", attempt),
				slog.String("model", meta.Model),
				slog.Int64("total_tokens", meta.TotalTokens),
			)
			return outcome, nil
		}

		// Summary counts violations by kind and never prints their values, which are
		// the candidate's CV contents or whatever an injection put there.
		log.WarnContext(ctx, "tailored CV failed factual containment",
			slog.Int("attempt", attempt),
			slog.String("containment", report.Summary()),
		)
		violations = report.Violations
	}

	// Two attempts, the second with its own fabrications quoted back. Refusing is the
	// correct outcome: a missing feature is recoverable, a fabricated CV sent to an
	// employer is not.
	log.ErrorContext(ctx, "tailoring abandoned; the model kept introducing facts the CV does not contain",
		slog.Int("attempts", outcome.Attempts),
		slog.String("containment", outcome.Containment.Summary()),
	)
	outcome.Diff = TailoringDiff{Rejected: outcome.Containment.Violations}
	return outcome, ErrTailoringUnsafe
}
