package identity

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/Joakim-animate90/myjobfyt/backend/internal/apperr"
	"github.com/Joakim-animate90/myjobfyt/backend/internal/platform/postgres"
	"github.com/google/uuid"
)

// Repository is the identity domain's persistence contract. The service depends on this
// interface, so it can be tested without a database and so a future storage change
// does not reach into the service layer.
type Repository interface {
	CreateUser(ctx context.Context, u *User) error
	CreateProfile(ctx context.Context, p *Profile) error
	UserByEmail(ctx context.Context, email string) (*User, error)
	UserByID(ctx context.Context, id uuid.UUID) (*User, error)
	ProfileByUserID(ctx context.Context, id uuid.UUID) (*Profile, error)
	UpdateProfile(ctx context.Context, p *Profile, phoneCiphertext []byte, phoneKeyID, phoneLastFour string) error
	UpdatePasswordHash(ctx context.Context, id uuid.UUID, hash string) error
	MarkEmailVerified(ctx context.Context, id uuid.UUID, at time.Time) error
	RecordSuccessfulLogin(ctx context.Context, id uuid.UUID, at time.Time) error
	RecordFailedLogin(ctx context.Context, id uuid.UUID, threshold int, window, lockFor time.Duration) (locked bool, err error)

	StoreRefreshToken(ctx context.Context, t StoredRefreshToken) error
	RefreshTokenByHash(ctx context.Context, hash []byte) (*StoredRefreshToken, error)
	RevokeRefreshToken(ctx context.Context, id uuid.UUID, reason string, rotatedTo *uuid.UUID) error
	RevokeRefreshTokenFamily(ctx context.Context, familyID uuid.UUID, reason string) (int64, error)
	RevokeAllRefreshTokensForUser(ctx context.Context, userID uuid.UUID, reason string) (int64, error)

	StoreVerificationToken(ctx context.Context, t VerificationToken) error
	ConsumeVerificationToken(ctx context.Context, hash []byte, purpose TokenPurpose, at time.Time) (*VerificationToken, error)

	AppendAuditLog(ctx context.Context, e AuditEntry) error
	PlanSlugForUser(ctx context.Context, userID uuid.UUID) (string, error)
}

// StoredRefreshToken is a refresh token as persisted.
type StoredRefreshToken struct {
	ID        uuid.UUID
	UserID    uuid.UUID
	TokenHash []byte
	FamilyID  uuid.UUID
	ExpiresAt time.Time
	RevokedAt *time.Time
	RotatedTo *uuid.UUID
	UserAgent string
	IP        string
	CreatedAt time.Time
}

// Active reports whether the token may still be exchanged.
func (t *StoredRefreshToken) Active(now time.Time) bool {
	return t.RevokedAt == nil && t.ExpiresAt.After(now)
}

// VerificationToken is a single-use token for email verification, password reset and
// similar flows.
type VerificationToken struct {
	ID         uuid.UUID
	UserID     uuid.UUID
	TokenHash  []byte
	Purpose    TokenPurpose
	Email      string
	ExpiresAt  time.Time
	ConsumedAt *time.Time
	CreatedAt  time.Time
}

// AuditEntry is one row of the append-only audit log.
type AuditEntry struct {
	UserID       *uuid.UUID
	ActorID      *uuid.UUID
	ActorRole    Role
	Action       string
	ResourceType string
	ResourceID   string
	IP           string
	UserAgent    string
	Metadata     map[string]any
}

// ErrEmailTaken means a live account already uses this address.
//
// The service converts this into a generic success response rather than surfacing it:
// a distinct "that email exists" reply turns the registration endpoint into an account
// enumeration oracle.
var ErrEmailTaken = errors.New("email already registered")

// PostgresRepository is the Postgres implementation.
type PostgresRepository struct {
	db *postgres.DB
}

// NewPostgresRepository builds the repository.
func NewPostgresRepository(db *postgres.DB) *PostgresRepository {
	return &PostgresRepository{db: db}
}

const userColumns = `
	id, email, coalesce(password_hash, ''), email_verified_at, status, role,
	last_login_at, failed_login_count, locked_until,
	deletion_requested_at, erase_after, created_at, updated_at`

func scanUser(row interface {
	Scan(dest ...any) error
}) (*User, error) {
	var u User
	err := row.Scan(
		&u.ID, &u.Email, &u.PasswordHash, &u.EmailVerifiedAt, &u.Status, &u.Role,
		&u.LastLoginAt, &u.FailedLoginCount, &u.LockedUntil,
		&u.DeletionRequestedAt, &u.EraseAfter, &u.CreatedAt, &u.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// CreateUser inserts a user, mapping the unique-email collision to ErrEmailTaken.
func (r *PostgresRepository) CreateUser(ctx context.Context, u *User) error {
	const q = `
		INSERT INTO users (id, email, password_hash, status, role)
		VALUES ($1, $2, nullif($3, ''), $4, $5)
		RETURNING created_at, updated_at`

	err := r.db.Querier(ctx).QueryRow(ctx, q, u.ID, u.Email, u.PasswordHash, u.Status, u.Role).
		Scan(&u.CreatedAt, &u.UpdatedAt)
	if err != nil {
		// The partial unique index name, not the constraint name: a partial unique
		// index reports itself as the constraint.
		if postgres.IsUniqueViolation(err, "users_email_live_uniq") {
			return ErrEmailTaken
		}
		return apperr.Internalf(err, "insert user")
	}
	return nil
}

// CreateProfile inserts the candidate profile row that accompanies every user.
func (r *PostgresRepository) CreateProfile(ctx context.Context, p *Profile) error {
	const q = `
		INSERT INTO candidate_profiles (user_id, full_name, timezone, completeness_pct)
		VALUES ($1, nullif($2, ''), coalesce(nullif($3, ''), 'UTC'), $4)
		RETURNING created_at, updated_at`

	err := r.db.Querier(ctx).QueryRow(ctx, q, p.UserID, p.FullName, p.Timezone, p.Completeness()).
		Scan(&p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return apperr.Internalf(err, "insert candidate profile")
	}
	return nil
}

// UserByEmail looks up a live user by normalised email.
func (r *PostgresRepository) UserByEmail(ctx context.Context, email string) (*User, error) {
	q := `SELECT ` + userColumns + ` FROM users WHERE email = $1 AND deleted_at IS NULL`

	u, err := scanUser(r.db.Querier(ctx).QueryRow(ctx, q, NormaliseEmail(email)))
	if err != nil {
		if postgres.IsNoRows(err) {
			return nil, apperr.NotFound("user")
		}
		return nil, apperr.Internalf(err, "select user by email")
	}
	return u, nil
}

// UserByID looks up a live user by id.
func (r *PostgresRepository) UserByID(ctx context.Context, id uuid.UUID) (*User, error) {
	q := `SELECT ` + userColumns + ` FROM users WHERE id = $1 AND deleted_at IS NULL`

	u, err := scanUser(r.db.Querier(ctx).QueryRow(ctx, q, id))
	if err != nil {
		if postgres.IsNoRows(err) {
			return nil, apperr.NotFound("user")
		}
		return nil, apperr.Internalf(err, "select user by id")
	}
	return u, nil
}

// ProfileByUserID loads the candidate profile.
//
// Note that phone_ciphertext is never selected: the service has no legitimate reason to
// hold a decryptable phone number in memory on a read path, and the last four digits
// are enough for the UI to confirm which number is stored.
func (r *PostgresRepository) ProfileByUserID(ctx context.Context, id uuid.UUID) (*Profile, error) {
	const q = `
		SELECT user_id,
		       coalesce(full_name, ''),
		       coalesce(phone_last_four, ''),
		       phone_ciphertext IS NOT NULL,
		       coalesce(city, ''), coalesce(country, ''), timezone,
		       coalesce(linkedin_url, ''), coalesce(portfolio_url, ''), coalesce(github_url, ''),
		       coalesce(work_authorisation, ''),
		       salary_min, salary_max, coalesce(salary_currency, ''), coalesce(salary_period, ''),
		       notice_period_days, open_to_relocation, coalesce(headline, ''),
		       completeness_pct, created_at, updated_at
		FROM candidate_profiles
		WHERE user_id = $1`

	var p Profile
	err := r.db.Querier(ctx).QueryRow(ctx, q, id).Scan(
		&p.UserID, &p.FullName, &p.PhoneLastFour, &p.HasPhone,
		&p.City, &p.Country, &p.Timezone,
		&p.LinkedInURL, &p.PortfolioURL, &p.GitHubURL,
		&p.WorkAuthorisation,
		&p.SalaryMin, &p.SalaryMax, &p.SalaryCurrency, &p.SalaryPeriod,
		&p.NoticePeriodDays, &p.OpenToRelocation, &p.Headline,
		&p.CompletenessPct, &p.CreatedAt, &p.UpdatedAt,
	)
	if err != nil {
		if postgres.IsNoRows(err) {
			return nil, apperr.NotFound("profile")
		}
		return nil, apperr.Internalf(err, "select candidate profile")
	}
	return &p, nil
}

// UpdateProfile writes the profile.
//
// The phone arguments are separate from the Profile struct because the ciphertext never
// belongs in a model that gets serialised towards a client. Passing nil ciphertext
// leaves the stored phone untouched, so a partial profile update cannot silently erase
// it.
func (r *PostgresRepository) UpdateProfile(ctx context.Context, p *Profile, phoneCiphertext []byte, phoneKeyID, phoneLastFour string) error {
	const q = `
		UPDATE candidate_profiles SET
			full_name          = nullif($2, ''),
			city               = nullif($3, ''),
			country            = nullif($4, ''),
			timezone           = coalesce(nullif($5, ''), 'UTC'),
			linkedin_url       = nullif($6, ''),
			portfolio_url      = nullif($7, ''),
			github_url         = nullif($8, ''),
			work_authorisation = nullif($9, ''),
			salary_min         = $10,
			salary_max         = $11,
			salary_currency    = nullif($12, ''),
			salary_period      = nullif($13, ''),
			notice_period_days = $14,
			open_to_relocation = $15,
			headline           = nullif($16, ''),
			completeness_pct   = $17,
			phone_ciphertext   = coalesce($18, phone_ciphertext),
			phone_key_id       = coalesce(nullif($19, ''), phone_key_id),
			phone_last_four    = coalesce(nullif($20, ''), phone_last_four)
		WHERE user_id = $1
		RETURNING updated_at`

	err := r.db.Querier(ctx).QueryRow(ctx, q,
		p.UserID, p.FullName, p.City, p.Country, p.Timezone,
		p.LinkedInURL, p.PortfolioURL, p.GitHubURL, p.WorkAuthorisation,
		p.SalaryMin, p.SalaryMax, p.SalaryCurrency, p.SalaryPeriod,
		p.NoticePeriodDays, p.OpenToRelocation, p.Headline,
		p.CompletenessPct,
		phoneCiphertext, phoneKeyID, phoneLastFour,
	).Scan(&p.UpdatedAt)
	if err != nil {
		if postgres.IsNoRows(err) {
			return apperr.NotFound("profile")
		}
		return apperr.Internalf(err, "update candidate profile")
	}
	return nil
}

// UpdatePasswordHash replaces the stored hash, used for password changes and for the
// transparent rehash after a cost increase.
func (r *PostgresRepository) UpdatePasswordHash(ctx context.Context, id uuid.UUID, hash string) error {
	const q = `UPDATE users SET password_hash = $2 WHERE id = $1 AND deleted_at IS NULL`
	tag, err := r.db.Querier(ctx).Exec(ctx, q, id, hash)
	if err != nil {
		return apperr.Internalf(err, "update password hash")
	}
	if tag.RowsAffected() == 0 {
		return apperr.NotFound("user")
	}
	return nil
}

// MarkEmailVerified records email verification. Idempotent: a candidate clicking the
// link twice must not be an error, and the first verification time is the one kept.
func (r *PostgresRepository) MarkEmailVerified(ctx context.Context, id uuid.UUID, at time.Time) error {
	const q = `
		UPDATE users SET email_verified_at = coalesce(email_verified_at, $2)
		WHERE id = $1 AND deleted_at IS NULL`
	tag, err := r.db.Querier(ctx).Exec(ctx, q, id, at)
	if err != nil {
		return apperr.Internalf(err, "mark email verified")
	}
	if tag.RowsAffected() == 0 {
		return apperr.NotFound("user")
	}
	return nil
}

// RecordSuccessfulLogin stamps the login and clears the failure counter and lock.
func (r *PostgresRepository) RecordSuccessfulLogin(ctx context.Context, id uuid.UUID, at time.Time) error {
	const q = `
		UPDATE users
		SET last_login_at = $2, failed_login_count = 0, locked_until = NULL
		WHERE id = $1`
	if _, err := r.db.Querier(ctx).Exec(ctx, q, id, at); err != nil {
		return apperr.Internalf(err, "record successful login")
	}
	return nil
}

// RecordFailedLogin increments the failure counter and locks the account once the
// threshold is reached inside the window.
//
// The counter is reset when the last failure is older than the window, so a user who
// mistypes their password twice a month is never locked out, while a burst of attempts
// is. Doing this in one statement avoids a read-modify-write race where parallel
// attempts each read the same count.
func (r *PostgresRepository) RecordFailedLogin(ctx context.Context, id uuid.UUID, threshold int, window, lockFor time.Duration) (bool, error) {
	const q = `
		UPDATE users SET
			failed_login_count = CASE
				WHEN updated_at < now() - $3::interval THEN 1
				ELSE failed_login_count + 1
			END,
			locked_until = CASE
				WHEN (CASE WHEN updated_at < now() - $3::interval THEN 1
				           ELSE failed_login_count + 1 END) >= $2
				THEN now() + $4::interval
				ELSE locked_until
			END
		WHERE id = $1
		RETURNING locked_until IS NOT NULL AND locked_until > now()`

	var locked bool
	err := r.db.Querier(ctx).QueryRow(ctx, q, id, threshold,
		intervalArg(window), intervalArg(lockFor)).Scan(&locked)
	if err != nil {
		if postgres.IsNoRows(err) {
			return false, apperr.NotFound("user")
		}
		return false, apperr.Internalf(err, "record failed login")
	}
	return locked, nil
}

func intervalArg(d time.Duration) string {
	return fmt.Sprintf("%d milliseconds", d.Milliseconds())
}

// StoreRefreshToken persists a refresh token hash.
func (r *PostgresRepository) StoreRefreshToken(ctx context.Context, t StoredRefreshToken) error {
	const q = `
		INSERT INTO refresh_tokens (id, user_id, token_hash, family_id, expires_at, user_agent, ip)
		VALUES ($1, $2, $3, $4, $5, nullif($6, ''), $7)`

	if _, err := r.db.Querier(ctx).Exec(ctx, q,
		t.ID, t.UserID, t.TokenHash, t.FamilyID, t.ExpiresAt, t.UserAgent, nullableIP(t.IP),
	); err != nil {
		return apperr.Internalf(err, "insert refresh token")
	}
	return nil
}

// nullableIP converts a client address to something the inet column accepts, returning
// nil for anything unparseable rather than failing the write. Losing the IP on an audit
// row is regrettable; failing a sign-in because a proxy sent an odd value is worse.
func nullableIP(s string) *string {
	if s == "" {
		return nil
	}
	if net.ParseIP(s) == nil {
		return nil
	}
	return &s
}

// RefreshTokenByHash loads a refresh token, including revoked and expired ones.
//
// Revoked tokens are returned deliberately: detecting reuse of an already-rotated token
// is the whole point of the family mechanism, and that is impossible if the lookup
// filters them out.
func (r *PostgresRepository) RefreshTokenByHash(ctx context.Context, hash []byte) (*StoredRefreshToken, error) {
	const q = `
		SELECT id, user_id, token_hash, family_id, expires_at, revoked_at, rotated_to,
		       coalesce(user_agent, ''), coalesce(host(ip), ''), created_at
		FROM refresh_tokens
		WHERE token_hash = $1`

	var t StoredRefreshToken
	err := r.db.Querier(ctx).QueryRow(ctx, q, hash).Scan(
		&t.ID, &t.UserID, &t.TokenHash, &t.FamilyID, &t.ExpiresAt,
		&t.RevokedAt, &t.RotatedTo, &t.UserAgent, &t.IP, &t.CreatedAt,
	)
	if err != nil {
		if postgres.IsNoRows(err) {
			return nil, apperr.NotFound("refresh token")
		}
		return nil, apperr.Internalf(err, "select refresh token")
	}
	return &t, nil
}

// RevokeRefreshToken revokes one token, optionally recording what replaced it.
func (r *PostgresRepository) RevokeRefreshToken(ctx context.Context, id uuid.UUID, reason string, rotatedTo *uuid.UUID) error {
	const q = `
		UPDATE refresh_tokens
		SET revoked_at = coalesce(revoked_at, now()),
		    revoked_reason = coalesce(revoked_reason, $2),
		    rotated_to = coalesce(rotated_to, $3)
		WHERE id = $1`
	if _, err := r.db.Querier(ctx).Exec(ctx, q, id, reason, rotatedTo); err != nil {
		return apperr.Internalf(err, "revoke refresh token")
	}
	return nil
}

// RevokeRefreshTokenFamily revokes every live token in a rotation family.
//
// Called when a superseded token is replayed, which means either a stolen token or a
// client bug. Both are handled the same way: destroy the chain and force a fresh
// authentication.
func (r *PostgresRepository) RevokeRefreshTokenFamily(ctx context.Context, familyID uuid.UUID, reason string) (int64, error) {
	const q = `
		UPDATE refresh_tokens
		SET revoked_at = now(), revoked_reason = $2
		WHERE family_id = $1 AND revoked_at IS NULL`
	tag, err := r.db.Querier(ctx).Exec(ctx, q, familyID, reason)
	if err != nil {
		return 0, apperr.Internalf(err, "revoke refresh token family")
	}
	return tag.RowsAffected(), nil
}

// RevokeAllRefreshTokensForUser signs the user out everywhere. Used on password change
// and on admin suspension.
func (r *PostgresRepository) RevokeAllRefreshTokensForUser(ctx context.Context, userID uuid.UUID, reason string) (int64, error) {
	const q = `
		UPDATE refresh_tokens
		SET revoked_at = now(), revoked_reason = $2
		WHERE user_id = $1 AND revoked_at IS NULL`
	tag, err := r.db.Querier(ctx).Exec(ctx, q, userID, reason)
	if err != nil {
		return 0, apperr.Internalf(err, "revoke all refresh tokens")
	}
	return tag.RowsAffected(), nil
}

// StoreVerificationToken persists a single-use token hash.
func (r *PostgresRepository) StoreVerificationToken(ctx context.Context, t VerificationToken) error {
	const q = `
		INSERT INTO email_verification_tokens (id, user_id, token_hash, purpose, email, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6)`
	if _, err := r.db.Querier(ctx).Exec(ctx, q,
		t.ID, t.UserID, t.TokenHash, t.Purpose, t.Email, t.ExpiresAt,
	); err != nil {
		return apperr.Internalf(err, "insert verification token")
	}
	return nil
}

// ConsumeVerificationToken atomically marks a token used and returns it.
//
// The WHERE clause carries the single-use and expiry conditions, so two concurrent
// requests cannot both redeem the same token: exactly one UPDATE affects a row.
func (r *PostgresRepository) ConsumeVerificationToken(ctx context.Context, hash []byte, purpose TokenPurpose, at time.Time) (*VerificationToken, error) {
	const q = `
		UPDATE email_verification_tokens
		SET consumed_at = $3
		WHERE token_hash = $1
		  AND purpose = $2
		  AND consumed_at IS NULL
		  AND expires_at > $3
		RETURNING id, user_id, token_hash, purpose, email, expires_at, consumed_at, created_at`

	var t VerificationToken
	err := r.db.Querier(ctx).QueryRow(ctx, q, hash, purpose, at).Scan(
		&t.ID, &t.UserID, &t.TokenHash, &t.Purpose, &t.Email,
		&t.ExpiresAt, &t.ConsumedAt, &t.CreatedAt,
	)
	if err != nil {
		if postgres.IsNoRows(err) {
			// Unknown, already used, or expired — all indistinguishable to the caller
			// on purpose, so the endpoint cannot be used to probe token validity.
			return nil, apperr.Validation("token_invalid", "this link is invalid or has already been used")
		}
		return nil, apperr.Internalf(err, "consume verification token")
	}
	return &t, nil
}

// AppendAuditLog writes an audit row.
func (r *PostgresRepository) AppendAuditLog(ctx context.Context, e AuditEntry) error {
	const q = `
		INSERT INTO audit_logs (user_id, actor_id, actor_role, action, resource_type,
		                        resource_id, ip, user_agent, metadata)
		VALUES ($1, $2, nullif($3, '')::user_role, $4, nullif($5, ''), nullif($6, ''),
		        $7, nullif($8, ''), $9)`

	metadata := e.Metadata
	if metadata == nil {
		metadata = map[string]any{}
	}

	if _, err := r.db.Querier(ctx).Exec(ctx, q,
		e.UserID, e.ActorID, string(e.ActorRole), e.Action, e.ResourceType,
		e.ResourceID, nullableIP(e.IP), e.UserAgent, metadata,
	); err != nil {
		return apperr.Internalf(err, "append audit log")
	}
	return nil
}

// PlanSlugForUser resolves the user's effective plan, defaulting to free.
//
// Only an active or trialing subscription confers a paid plan: past_due and incomplete
// deliberately fall back to free, so a failed payment cannot keep granting paid
// entitlements indefinitely.
func (r *PostgresRepository) PlanSlugForUser(ctx context.Context, userID uuid.UUID) (string, error) {
	const q = `
		SELECT p.slug
		FROM subscriptions s
		JOIN plans p ON p.id = s.plan_id
		WHERE s.user_id = $1 AND s.status IN ('active', 'trialing')`

	var slug string
	err := r.db.Querier(ctx).QueryRow(ctx, q, userID).Scan(&slug)
	if err != nil {
		if postgres.IsNoRows(err) {
			return "free", nil
		}
		return "", apperr.Internalf(err, "select plan for user")
	}
	return slug, nil
}
