package cv

import (
	"context"
	"log/slog"
	"time"

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
	repo      Repository
	tailor    Tailorer
	proposals ProposalStore
	log       *slog.Logger
	maxTrys   int
}

// NewTailoringService builds the service.
//
// proposals may be nil, which leaves preview working and accept unavailable. That is a
// supported degraded mode rather than a failure: a candidate can still see what
// tailoring would do.
func NewTailoringService(repo Repository, tailor Tailorer, proposals ProposalStore, log *slog.Logger) *TailoringService {
	return &TailoringService{
		repo:      repo,
		tailor:    tailor,
		proposals: proposals,
		log:       log,
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

// ErrAcceptUnavailable reports that previews cannot be accepted on this deployment.
var ErrAcceptUnavailable = apperr.Unprocessable("tailoring_accept_unavailable",
	"Tailored CVs cannot be saved on this deployment.")

// Preview tailors a CV and records the proposal, returning it with its diff.
//
// The proposal is held server-side rather than handed to the client to post back: a
// client could alter it between preview and save, and the containment verdict would
// then describe something other than what was stored.
func (s *TailoringService) Preview(
	ctx context.Context, userID, versionID, jobPostingID uuid.UUID, jobDescription string,
) (TailoringOutcome, uuid.UUID, error) {
	outcome, err := s.TailorForJob(ctx, userID, versionID, jobDescription)
	if err != nil {
		return outcome, uuid.Nil, err
	}
	if s.proposals == nil {
		// Preview still works; there is simply nothing to accept later.
		return outcome, uuid.Nil, nil
	}

	proposal := TailoringProposal{
		ID: uuid.New(), UserID: userID, SourceID: versionID, JobPostingID: jobPostingID,
		Tailored: outcome.Tailored, Model: outcome.Meta.Model,
		EngineVersion: outcome.Meta.EngineVersion, CreatedAt: time.Now().UTC(),
	}
	if err := s.proposals.Save(ctx, proposal); err != nil {
		return outcome, uuid.Nil, err
	}
	return outcome, proposal.ID, nil
}

// Accept stores a previewed tailoring as a new CV version.
//
// Containment is re-checked here against the CURRENT source rather than trusted from
// the preview. The source may have been re-parsed or corrected in between, and a
// verdict reached against an older version of the facts is not a verdict about what is
// being stored. It also means that even a forged or tampered proposal cannot produce a
// fabricated CV: the gate is the check, never a flag someone set earlier.
func (s *TailoringService) Accept(ctx context.Context, userID, proposalID uuid.UUID) (*Version, error) {
	if s.proposals == nil {
		return nil, ErrAcceptUnavailable
	}

	proposal, err := s.proposals.Load(ctx, userID, proposalID)
	if err != nil {
		return nil, err
	}

	source, err := s.repo.ParsedDataByVersion(ctx, userID, proposal.SourceID)
	if err != nil {
		return nil, err
	}
	skills, err := s.repo.SkillsByVersion(ctx, userID, proposal.SourceID)
	if err != nil {
		return nil, err
	}

	log := s.log.With(
		slog.String("cv_version_id", proposal.SourceID.String()),
		slog.String("user_id", userID.String()),
	)

	report := CheckContainment(source, skills, proposal.Tailored)
	if !report.Passed {
		// Either the source changed under the proposal or the proposal was tampered
		// with. Both mean the same thing here: this must not be stored.
		log.WarnContext(ctx, "a previewed tailoring no longer passes containment; refusing to store it",
			slog.String("containment", report.Summary()))
		return nil, ErrTailoringUnsafe
	}

	sourceVersion, err := s.repo.VersionByID(ctx, userID, proposal.SourceID)
	if err != nil {
		return nil, err
	}

	passed := true
	version := &Version{
		ID:         uuid.New(),
		DocumentID: sourceVersion.DocumentID,
		UserID:     userID,
		// A tailored CV is generated text, not an uploaded document. It gets its own
		// storage key so later rendering has somewhere to put the file.
		StorageKey:              "tenants/" + userID.String() + "/tailored/" + uuid.New().String(),
		ChecksumSHA256:          make([]byte, 32),
		AVStatus:                AVClean,
		ParseStatus:             ParseParsed,
		DerivedFromVersionID:    &proposal.SourceID,
		TailoredForJobPostingID: &proposal.JobPostingID,
		ContainmentPassed:       &passed,
	}

	tailoredSkills := make([]Skill, 0, len(proposal.Tailored.ClaimedSkills))
	for _, label := range proposal.Tailored.ClaimedSkills {
		tailoredSkills = append(tailoredSkills, Skill{RawLabel: label, Source: SkillSourceAI})
	}

	if err := s.repo.StoreTailoredVersion(ctx, version, proposal.Tailored, tailoredSkills); err != nil {
		return nil, err
	}

	// Best effort: a proposal left behind expires on its own, and failing the request
	// after the version is safely stored would be worse than a stale key.
	if derr := s.proposals.Delete(ctx, userID, proposalID); derr != nil {
		log.WarnContext(ctx, "could not clear an accepted tailoring proposal", slog.Any("error", derr))
	}

	log.InfoContext(ctx, "tailored CV accepted",
		slog.String("tailored_version_id", version.ID.String()),
		slog.Int("version_no", version.VersionNo),
	)
	return version, nil
}
