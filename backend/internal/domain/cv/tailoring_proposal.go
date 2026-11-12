package cv

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/Joakim-animate90/myjobfyt/backend/internal/apperr"
	"github.com/Joakim-animate90/myjobfyt/backend/internal/platform/redisx"
	"github.com/google/uuid"
)

// TailoringProposal is a previewed tailoring awaiting the candidate's decision.
//
// Held server-side rather than returned for the client to post back. A client could
// alter the content between preview and save, and the containment verdict would then
// describe something other than what was stored — which is the one thing this feature
// cannot allow.
type TailoringProposal struct {
	ID           uuid.UUID `json:"id"`
	UserID       uuid.UUID `json:"user_id"`
	SourceID     uuid.UUID `json:"source_version_id"`
	JobPostingID uuid.UUID `json:"job_posting_id"`
	// Tailored is the validated output. Stored as produced, so accept can re-check it.
	Tailored *ParsedData `json:"tailored"`
	Model    string      `json:"model"`
	// EngineVersion identifies the prompt and schema that produced it, so a proposal
	// made before a prompt change is attributable afterwards.
	EngineVersion string    `json:"engine_version"`
	CreatedAt     time.Time `json:"created_at"`
}

// ProposalStore holds previewed tailorings between the preview and the decision.
type ProposalStore interface {
	Save(ctx context.Context, proposal TailoringProposal) error
	// Load returns the proposal, scoped by user. A proposal that has expired or belongs
	// to someone else reports not-found.
	Load(ctx context.Context, userID, proposalID uuid.UUID) (TailoringProposal, error)
	Delete(ctx context.Context, userID, proposalID uuid.UUID) error
}

// proposalTTL bounds how long a preview stays acceptable.
//
// Long enough to read a diff properly, short enough that a tailoring is not accepted
// against a CV the candidate has since edited. Accept re-validates against the current
// source anyway, so this is a convenience bound rather than a safety one.
const proposalTTL = time.Hour

// RedisProposalStore keeps proposals in Redis.
//
// Redis is right here precisely because this data is disposable: losing a proposal
// costs the candidate one click, and the alternative — a Postgres table of
// half-accepted CVs — is a table someone has to reap.
type RedisProposalStore struct {
	redis *redisx.Client
	ttl   time.Duration
}

// NewRedisProposalStore builds the store.
func NewRedisProposalStore(redis *redisx.Client) *RedisProposalStore {
	return &RedisProposalStore{redis: redis, ttl: proposalTTL}
}

// proposalKey scopes the key by user, so a guessed or leaked proposal id is useless to
// anyone else. The ownership check is in the key itself rather than in a comparison
// that a later refactor could drop.
func proposalKey(userID, proposalID uuid.UUID) string {
	return fmt.Sprintf("cv:tailor:proposal:%s:%s", userID, proposalID)
}

func (s *RedisProposalStore) Save(ctx context.Context, proposal TailoringProposal) error {
	body, err := json.Marshal(proposal)
	if err != nil {
		return apperr.Internalf(err, "marshal tailoring proposal")
	}
	key := proposalKey(proposal.UserID, proposal.ID)
	if err := s.redis.Raw().Set(ctx, key, body, s.ttl).Err(); err != nil {
		return apperr.Internalf(err, "store tailoring proposal")
	}
	return nil
}

func (s *RedisProposalStore) Load(ctx context.Context, userID, proposalID uuid.UUID) (TailoringProposal, error) {
	body, err := s.redis.Raw().Get(ctx, proposalKey(userID, proposalID)).Bytes()
	if err != nil {
		// Expired, never existed, or belongs to someone else — all indistinguishable to
		// the caller on purpose, since telling them apart would confirm that someone
		// else's proposal id is real.
		return TailoringProposal{}, apperr.NotFound("tailoring proposal")
	}

	var proposal TailoringProposal
	if err := json.Unmarshal(body, &proposal); err != nil {
		return TailoringProposal{}, apperr.Internalf(err, "decode tailoring proposal")
	}
	if proposal.UserID != userID {
		// Belt and braces with the key scoping: a key built wrongly by a future change
		// must not become a cross-tenant read.
		return TailoringProposal{}, apperr.NotFound("tailoring proposal")
	}
	return proposal, nil
}

func (s *RedisProposalStore) Delete(ctx context.Context, userID, proposalID uuid.UUID) error {
	if err := s.redis.Raw().Del(ctx, proposalKey(userID, proposalID)).Err(); err != nil {
		return apperr.Internalf(err, "delete tailoring proposal")
	}
	return nil
}
