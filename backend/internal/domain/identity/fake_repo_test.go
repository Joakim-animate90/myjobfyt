package identity_test

import (
	"context"
	"sync"
	"time"

	"github.com/Joakim-animate90/myjobfyt/backend/internal/apperr"
	"github.com/Joakim-animate90/myjobfyt/backend/internal/domain/identity"
	"github.com/Joakim-animate90/myjobfyt/backend/internal/platform/events"
	"github.com/google/uuid"
)

// fakeRepo is an in-memory Repository. It reproduces the behaviours the service
// depends on — case-insensitive unique email, single-use verification tokens, the
// failed-login window — because those are exactly what the service's logic is built
// on, and a fake that gets them wrong would test nothing useful.
type fakeRepo struct {
	mu sync.Mutex

	users        map[uuid.UUID]*identity.User
	emailIndex   map[string]uuid.UUID
	profiles     map[uuid.UUID]*identity.Profile
	refresh      map[string]*identity.StoredRefreshToken // keyed by string(hash)
	refreshByID  map[uuid.UUID]*identity.StoredRefreshToken
	verification map[string]*identity.VerificationToken
	auditEntries []identity.AuditEntry
	planSlug     string

	// Injectable failures for the paths where the service must degrade rather than fail.
	planErr  error
	auditErr error
	now      func() time.Time
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{
		users:        map[uuid.UUID]*identity.User{},
		emailIndex:   map[string]uuid.UUID{},
		profiles:     map[uuid.UUID]*identity.Profile{},
		refresh:      map[string]*identity.StoredRefreshToken{},
		refreshByID:  map[uuid.UUID]*identity.StoredRefreshToken{},
		verification: map[string]*identity.VerificationToken{},
		planSlug:     "free",
		now:          func() time.Time { return time.Now().UTC() },
	}
}

func (f *fakeRepo) CreateUser(_ context.Context, u *identity.User) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, exists := f.emailIndex[u.Email]; exists {
		return identity.ErrEmailTaken
	}
	clone := *u
	clone.CreatedAt = f.now()
	clone.UpdatedAt = clone.CreatedAt
	f.users[u.ID] = &clone
	f.emailIndex[u.Email] = u.ID
	u.CreatedAt, u.UpdatedAt = clone.CreatedAt, clone.UpdatedAt
	return nil
}

func (f *fakeRepo) CreateProfile(_ context.Context, p *identity.Profile) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	clone := *p
	clone.CreatedAt = f.now()
	clone.UpdatedAt = clone.CreatedAt
	if clone.Timezone == "" {
		clone.Timezone = "UTC"
	}
	f.profiles[p.UserID] = &clone
	return nil
}

func (f *fakeRepo) UserByEmail(_ context.Context, email string) (*identity.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id, ok := f.emailIndex[identity.NormaliseEmail(email)]
	if !ok {
		return nil, apperr.NotFound("user")
	}
	clone := *f.users[id]
	return &clone, nil
}

func (f *fakeRepo) UserByID(_ context.Context, id uuid.UUID) (*identity.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.users[id]
	if !ok {
		return nil, apperr.NotFound("user")
	}
	clone := *u
	return &clone, nil
}

func (f *fakeRepo) ProfileByUserID(_ context.Context, id uuid.UUID) (*identity.Profile, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.profiles[id]
	if !ok {
		return nil, apperr.NotFound("profile")
	}
	clone := *p
	return &clone, nil
}

func (f *fakeRepo) UpdateProfile(_ context.Context, p *identity.Profile, phoneCiphertext []byte, phoneKeyID, phoneLastFour string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	existing, ok := f.profiles[p.UserID]
	if !ok {
		return apperr.NotFound("profile")
	}
	clone := *p
	clone.UpdatedAt = f.now()
	// Mirror the coalesce in the SQL: absent ciphertext leaves the stored phone alone.
	if phoneCiphertext == nil {
		clone.HasPhone = existing.HasPhone
		clone.PhoneLastFour = existing.PhoneLastFour
	} else {
		clone.HasPhone = true
		clone.PhoneLastFour = phoneLastFour
	}
	f.profiles[p.UserID] = &clone
	*p = clone
	return nil
}

func (f *fakeRepo) UpdatePasswordHash(_ context.Context, id uuid.UUID, hash string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.users[id]
	if !ok {
		return apperr.NotFound("user")
	}
	u.PasswordHash = hash
	return nil
}

func (f *fakeRepo) MarkEmailVerified(_ context.Context, id uuid.UUID, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.users[id]
	if !ok {
		return apperr.NotFound("user")
	}
	if u.EmailVerifiedAt == nil {
		u.EmailVerifiedAt = &at
	}
	return nil
}

func (f *fakeRepo) RecordSuccessfulLogin(_ context.Context, id uuid.UUID, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.users[id]
	if !ok {
		return apperr.NotFound("user")
	}
	u.LastLoginAt = &at
	u.FailedLoginCount = 0
	u.LockedUntil = nil
	return nil
}

func (f *fakeRepo) RecordFailedLogin(_ context.Context, id uuid.UUID, threshold int, window, lockFor time.Duration) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.users[id]
	if !ok {
		return false, apperr.NotFound("user")
	}
	now := f.now()
	if u.UpdatedAt.Before(now.Add(-window)) {
		u.FailedLoginCount = 1
	} else {
		u.FailedLoginCount++
	}
	u.UpdatedAt = now
	if u.FailedLoginCount >= threshold {
		until := now.Add(lockFor)
		u.LockedUntil = &until
		return true, nil
	}
	return false, nil
}

func (f *fakeRepo) StoreRefreshToken(_ context.Context, t identity.StoredRefreshToken) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	clone := t
	clone.CreatedAt = f.now()
	f.refresh[string(t.TokenHash)] = &clone
	f.refreshByID[t.ID] = &clone
	return nil
}

func (f *fakeRepo) RefreshTokenByHash(_ context.Context, hash []byte) (*identity.StoredRefreshToken, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.refresh[string(hash)]
	if !ok {
		return nil, apperr.NotFound("refresh token")
	}
	clone := *t
	return &clone, nil
}

func (f *fakeRepo) RevokeRefreshToken(_ context.Context, id uuid.UUID, reason string, rotatedTo *uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.refreshByID[id]
	if !ok {
		return nil
	}
	if t.RevokedAt == nil {
		now := f.now()
		t.RevokedAt = &now
		t.RotatedTo = rotatedTo
	}
	return nil
}

func (f *fakeRepo) RevokeRefreshTokenFamily(_ context.Context, familyID uuid.UUID, reason string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := f.now()
	var n int64
	for _, t := range f.refreshByID {
		if t.FamilyID == familyID && t.RevokedAt == nil {
			revoked := now
			t.RevokedAt = &revoked
			n++
		}
	}
	return n, nil
}

func (f *fakeRepo) RevokeAllRefreshTokensForUser(_ context.Context, userID uuid.UUID, reason string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := f.now()
	var n int64
	for _, t := range f.refreshByID {
		if t.UserID == userID && t.RevokedAt == nil {
			revoked := now
			t.RevokedAt = &revoked
			n++
		}
	}
	return n, nil
}

func (f *fakeRepo) StoreVerificationToken(_ context.Context, t identity.VerificationToken) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	clone := t
	clone.CreatedAt = f.now()
	f.verification[string(t.TokenHash)] = &clone
	return nil
}

func (f *fakeRepo) ConsumeVerificationToken(_ context.Context, hash []byte, purpose identity.TokenPurpose, at time.Time) (*identity.VerificationToken, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.verification[string(hash)]
	// Unknown, wrong purpose, already consumed and expired are all one error, exactly
	// as the SQL produces, so the endpoint cannot be used to probe token validity.
	if !ok || t.Purpose != purpose || t.ConsumedAt != nil || !t.ExpiresAt.After(at) {
		return nil, apperr.Validation("token_invalid", "this link is invalid or has already been used")
	}
	consumed := at
	t.ConsumedAt = &consumed
	clone := *t
	return &clone, nil
}

func (f *fakeRepo) AppendAuditLog(_ context.Context, e identity.AuditEntry) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.auditErr != nil {
		return f.auditErr
	}
	f.auditEntries = append(f.auditEntries, e)
	return nil
}

func (f *fakeRepo) PlanSlugForUser(_ context.Context, _ uuid.UUID) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.planErr != nil {
		return "", f.planErr
	}
	return f.planSlug, nil
}

// actions returns the recorded audit action names.
func (f *fakeRepo) actions() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.auditEntries))
	for _, e := range f.auditEntries {
		out = append(out, e.Action)
	}
	return out
}

func (f *fakeRepo) hasAction(action string) bool {
	for _, a := range f.actions() {
		if a == action {
			return true
		}
	}
	return false
}

// verificationPlaintextFor finds the stored token whose hash matches plaintext.
func (f *fakeRepo) storedVerificationCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.verification)
}

// passthroughTx runs fn with no transaction. The real transactional behaviour is
// covered by the integration tests against Postgres; here the point is the business
// logic, not the isolation level.
type passthroughTx struct {
	calls int
}

func (p *passthroughTx) InTx(ctx context.Context, fn func(context.Context) error) error {
	p.calls++
	return fn(ctx)
}

// recordingBus captures published events.
type recordingBus struct {
	mu     sync.Mutex
	events []publishedEvent
	err    error
}

type publishedEvent struct {
	Type    string
	Payload any
}

func (b *recordingBus) Publish(ctx context.Context, eventType string, version int, payload any) error {
	return b.PublishWithMeta(ctx, eventType, version, events.Meta{}, payload)
}

func (b *recordingBus) PublishWithMeta(_ context.Context, eventType string, _ int, _ events.Meta, payload any) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.err != nil {
		return b.err
	}
	b.events = append(b.events, publishedEvent{Type: eventType, Payload: payload})
	return nil
}

func (b *recordingBus) types() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]string, 0, len(b.events))
	for _, e := range b.events {
		out = append(out, e.Type)
	}
	return out
}

func (b *recordingBus) has(eventType string) bool {
	for _, t := range b.types() {
		if t == eventType {
			return true
		}
	}
	return false
}
