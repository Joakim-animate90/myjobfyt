package identity

import (
	"context"
	"errors"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/Joakim-animate90/myjobfyt/backend/internal/apperr"
	"github.com/Joakim-animate90/myjobfyt/backend/internal/config"
	"github.com/Joakim-animate90/myjobfyt/backend/internal/platform/dataenc"
	"github.com/Joakim-animate90/myjobfyt/backend/internal/platform/events"
	"github.com/google/uuid"
)

// TxManager declares the transaction boundary the service needs, without depending on
// the concrete Postgres type.
type TxManager interface {
	InTx(ctx context.Context, fn func(ctx context.Context) error) error
}

// Service holds the identity business logic.
//
// Handlers call this; nothing here touches HTTP, and nothing here writes SQL.
type Service struct {
	repo   Repository
	tx     TxManager
	hasher *PasswordHasher
	tokens *TokenIssuer
	cipher dataenc.Cipher
	bus    events.Publisher
	cfg    config.AuthConfig
	log    *slog.Logger
	now    func() time.Time
}

// ServiceDeps are the service's collaborators.
type ServiceDeps struct {
	Repo   Repository
	Tx     TxManager
	Hasher *PasswordHasher
	Tokens *TokenIssuer
	Cipher dataenc.Cipher
	Bus    events.Publisher
	Config config.AuthConfig
	Logger *slog.Logger
	// Now is injectable so lockout and expiry behaviour can be tested without sleeping.
	Now func() time.Time
}

// NewService builds the identity service.
func NewService(d ServiceDeps) *Service {
	now := d.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &Service{
		repo:   d.Repo,
		tx:     d.Tx,
		hasher: d.Hasher,
		tokens: d.Tokens,
		cipher: d.Cipher,
		bus:    d.Bus,
		cfg:    d.Config,
		log:    d.Logger,
		now:    now,
	}
}

// emailPattern is a deliberately permissive syntax check.
//
// Exhaustive RFC 5322 validation is a famous waste of effort: it rejects valid
// addresses and accepts undeliverable ones. The real validation is the verification
// email, so this only catches obvious nonsense before we try to send one.
var emailPattern = regexp.MustCompile(`^[^@\s]+@[^@\s.]+(\.[^@\s.]+)+$`)

const maxEmailLength = 254 // RFC 5321 limit on a forward path

// ValidateEmail checks address syntax.
func ValidateEmail(email string) error {
	email = NormaliseEmail(email)
	switch {
	case email == "":
		return apperr.ValidationFields([]apperr.FieldError{{
			Field: "email", Code: "required", Message: "an email address is required",
		}})
	case len(email) > maxEmailLength:
		return apperr.ValidationFields([]apperr.FieldError{{
			Field: "email", Code: "too_long", Message: "this email address is too long",
		}})
	case !emailPattern.MatchString(email):
		return apperr.ValidationFields([]apperr.FieldError{{
			Field: "email", Code: "invalid", Message: "must be a valid email address",
		}})
	}
	return nil
}

// RegisterInput is what registration needs.
type RegisterInput struct {
	Email         string
	Password      string
	FullName      string
	Timezone      string
	AcceptedTerms bool
	IP            string
	UserAgent     string
}

// RegisterResult reports the outcome.
//
// Created is false when the address already belonged to a live account. The HTTP layer
// returns the same response either way: distinguishing them would turn registration
// into an account-enumeration oracle, letting anyone test whether a given person uses
// the platform.
type RegisterResult struct {
	Created           bool
	UserID            uuid.UUID
	VerificationToken string // plaintext, for the email; never returned to a client
}

// Register creates an account and issues an email-verification token.
func (s *Service) Register(ctx context.Context, in RegisterInput) (*RegisterResult, error) {
	if err := ValidateEmail(in.Email); err != nil {
		return nil, err
	}
	if err := ValidatePassword(in.Password); err != nil {
		return nil, err
	}
	if !in.AcceptedTerms {
		return nil, apperr.ValidationFields([]apperr.FieldError{{
			Field:   "accepted_terms",
			Code:    "required",
			Message: "you must accept the terms to create an account",
		}})
	}

	email := NormaliseEmail(in.Email)

	hash, err := s.hasher.Hash(in.Password)
	if err != nil {
		return nil, apperr.Internalf(err, "hash password during registration")
	}

	user := &User{
		ID:           uuid.New(),
		Email:        email,
		PasswordHash: hash,
		Status:       StatusActive,
		Role:         RoleCandidate,
	}

	verification, err := NewRefreshToken(uuid.Nil, s.now(), s.cfg.VerificationTTL)
	if err != nil {
		return nil, apperr.Internalf(err, "generate verification token")
	}

	err = s.tx.InTx(ctx, func(ctx context.Context) error {
		if cerr := s.repo.CreateUser(ctx, user); cerr != nil {
			return cerr
		}
		profile := &Profile{
			UserID:   user.ID,
			FullName: strings.TrimSpace(in.FullName),
			Timezone: in.Timezone,
		}
		if perr := s.repo.CreateProfile(ctx, profile); perr != nil {
			return perr
		}
		return s.repo.StoreVerificationToken(ctx, VerificationToken{
			ID:        uuid.New(),
			UserID:    user.ID,
			TokenHash: verification.Hash,
			Purpose:   PurposeEmailVerification,
			Email:     email,
			ExpiresAt: s.now().Add(s.cfg.VerificationTTL),
		})
	})

	if errors.Is(err, ErrEmailTaken) {
		// Log it so support can still diagnose a genuine duplicate, but tell the
		// caller nothing. The already-registered user receives no email either, since
		// sending one would confirm the account's existence to whoever submitted the
		// form.
		s.log.InfoContext(ctx, "registration attempted for an existing address",
			slog.String("email_domain", emailDomain(email)))
		return &RegisterResult{Created: false}, nil
	}
	if err != nil {
		return nil, err
	}

	s.audit(ctx, AuditEntry{
		UserID: &user.ID, ActorID: &user.ID, ActorRole: RoleCandidate,
		Action: "identity.registered", ResourceType: "user", ResourceID: user.ID.String(),
		IP: in.IP, UserAgent: in.UserAgent,
	})

	s.publish(ctx, events.TypeCandidateCreated, map[string]any{
		"user_id": user.ID.String(),
		"email":   email,
		"source":  "password",
	})

	return &RegisterResult{
		Created:           true,
		UserID:            user.ID,
		VerificationToken: verification.Plaintext,
	}, nil
}

// emailDomain returns just the domain, so a log line can carry useful signal about
// registration patterns without recording the address itself.
func emailDomain(email string) string {
	if i := strings.LastIndexByte(email, '@'); i >= 0 && i+1 < len(email) {
		return email[i+1:]
	}
	return "unknown"
}

// LoginInput is what sign-in needs.
type LoginInput struct {
	Email     string
	Password  string
	IP        string
	UserAgent string
}

// errInvalidCredentials is the single response to every sign-in failure.
//
// One message for "no such user", "wrong password" and "no password set on this
// account": any difference between them is an enumeration oracle.
func errInvalidCredentials() error {
	return apperr.Unauthenticated("invalid_credentials", "the email address or password is incorrect")
}

// Login authenticates and issues a session.
func (s *Service) Login(ctx context.Context, in LoginInput) (*Session, error) {
	email := NormaliseEmail(in.Email)
	now := s.now()

	user, err := s.repo.UserByEmail(ctx, email)
	if err != nil {
		if apperr.KindOf(err) == apperr.KindNotFound {
			// Spend comparable work on a non-existent account so response time does
			// not reveal whether the address is registered.
			s.burnPasswordWork(in.Password)
			return nil, errInvalidCredentials()
		}
		return nil, err
	}

	if user.IsLocked(now) {
		s.audit(ctx, AuditEntry{
			UserID: &user.ID, Action: "identity.login_blocked_locked",
			ResourceType: "user", ResourceID: user.ID.String(),
			IP: in.IP, UserAgent: in.UserAgent,
		})
		retryAfter := int(user.LockedUntil.Sub(now).Seconds()) + 1
		return nil, apperr.RateLimited(retryAfter).
			WithMeta("reason", "too_many_failed_attempts")
	}

	if !user.Status.CanAuthenticate() {
		s.audit(ctx, AuditEntry{
			UserID: &user.ID, Action: "identity.login_blocked_status",
			ResourceType: "user", ResourceID: user.ID.String(),
			IP: in.IP, UserAgent: in.UserAgent,
			Metadata: map[string]any{"status": string(user.Status)},
		})
		return nil, apperr.Forbidden("account_unavailable", "this account is not available")
	}

	if !user.HasPassword() {
		// An OAuth-only account. Same generic error: saying "use Google instead" would
		// confirm the address is registered and reveal how.
		s.burnPasswordWork(in.Password)
		return nil, errInvalidCredentials()
	}

	if verr := s.hasher.Verify(in.Password, user.PasswordHash); verr != nil {
		if !errors.Is(verr, ErrMismatchedPassword) {
			// A malformed stored hash is a data problem, not a wrong password.
			s.log.ErrorContext(ctx, "stored password hash could not be decoded",
				slog.String("user_id", user.ID.String()), slog.Any("error", verr))
		}
		locked, lerr := s.repo.RecordFailedLogin(ctx, user.ID,
			s.cfg.MaxFailedLogins, s.cfg.LockoutWindow, s.cfg.LockoutDuration)
		if lerr != nil {
			s.log.ErrorContext(ctx, "failed to record a failed login", slog.Any("error", lerr))
		}
		s.audit(ctx, AuditEntry{
			UserID: &user.ID, Action: "identity.login_failed",
			ResourceType: "user", ResourceID: user.ID.String(),
			IP: in.IP, UserAgent: in.UserAgent,
			Metadata: map[string]any{"locked": locked},
		})
		return nil, errInvalidCredentials()
	}

	// Transparently upgrade a hash made under weaker parameters. This is the only
	// moment the plaintext password is available, so it is the only chance to do it.
	if s.hasher.NeedsRehash(user.PasswordHash) {
		if newHash, herr := s.hasher.Hash(in.Password); herr == nil {
			if uerr := s.repo.UpdatePasswordHash(ctx, user.ID, newHash); uerr != nil {
				s.log.WarnContext(ctx, "password rehash could not be stored", slog.Any("error", uerr))
			}
		}
	}

	if err := s.repo.RecordSuccessfulLogin(ctx, user.ID, now); err != nil {
		s.log.WarnContext(ctx, "failed to record a successful login", slog.Any("error", err))
	}

	session, err := s.issueSession(ctx, user, in.IP, in.UserAgent, uuid.Nil)
	if err != nil {
		return nil, err
	}

	s.audit(ctx, AuditEntry{
		UserID: &user.ID, ActorID: &user.ID, ActorRole: user.Role,
		Action: "identity.login_succeeded", ResourceType: "user", ResourceID: user.ID.String(),
		IP: in.IP, UserAgent: in.UserAgent,
	})
	return session, nil
}

// burnPasswordWork performs a hash so that a failed sign-in against a non-existent or
// passwordless account costs roughly what a real verification costs.
//
// Without this, the endpoint answers "is this address registered?" through response
// timing alone, which is a trivially automatable enumeration channel.
func (s *Service) burnPasswordWork(password string) {
	if password == "" {
		password = "placeholder-for-timing-equalisation"
	}
	_, _ = s.hasher.Hash(password)
}

// issueSession mints the access and refresh tokens and persists the refresh hash.
func (s *Service) issueSession(ctx context.Context, user *User, ip, userAgent string, familyID uuid.UUID) (*Session, error) {
	plan, err := s.repo.PlanSlugForUser(ctx, user.ID)
	if err != nil {
		// A billing lookup failure must not block sign-in; fall back to the free tier,
		// which is the conservative direction.
		s.log.WarnContext(ctx, "could not resolve plan; defaulting to free", slog.Any("error", err))
		plan = "free"
	}

	accessToken, accessExpiry, err := s.tokens.IssueAccessToken(user.ID, user.Email, user.Role, plan)
	if err != nil {
		return nil, apperr.Internalf(err, "issue access token")
	}

	refresh, err := NewRefreshToken(familyID, s.now(), s.tokens.RefreshTTL())
	if err != nil {
		return nil, apperr.Internalf(err, "mint refresh token")
	}

	stored := StoredRefreshToken{
		ID:        uuid.New(),
		UserID:    user.ID,
		TokenHash: refresh.Hash,
		FamilyID:  refresh.FamilyID,
		ExpiresAt: refresh.ExpiresAt,
		UserAgent: userAgent,
		IP:        ip,
	}
	if err := s.repo.StoreRefreshToken(ctx, stored); err != nil {
		return nil, err
	}

	profile, perr := s.repo.ProfileByUserID(ctx, user.ID)
	if perr != nil && apperr.KindOf(perr) != apperr.KindNotFound {
		s.log.WarnContext(ctx, "could not load profile for session", slog.Any("error", perr))
	}

	return &Session{
		AccessToken:      accessToken,
		AccessExpiresAt:  accessExpiry,
		RefreshToken:     refresh.Plaintext,
		RefreshExpiresAt: refresh.ExpiresAt,
		User:             user,
		Profile:          profile,
	}, nil
}

// Refresh exchanges a refresh token for a new session, rotating the token.
//
// Reuse of an already-rotated token revokes the entire family. That is the standard
// detection for a stolen refresh token: the legitimate client and the thief cannot both
// hold the newest token, so whichever presents a superseded one proves a copy exists,
// and the only safe response is to invalidate the chain and force re-authentication.
func (s *Service) Refresh(ctx context.Context, plaintext, ip, userAgent string) (*Session, error) {
	if strings.TrimSpace(plaintext) == "" {
		return nil, apperr.Unauthenticated("refresh_token_missing", "no session to refresh")
	}

	hash := HashRefreshToken(plaintext)
	now := s.now()

	stored, err := s.repo.RefreshTokenByHash(ctx, hash)
	if err != nil {
		if apperr.KindOf(err) == apperr.KindNotFound {
			return nil, apperr.Unauthenticated("refresh_token_invalid", "please sign in again")
		}
		return nil, err
	}

	if stored.RevokedAt != nil {
		revoked, rerr := s.repo.RevokeRefreshTokenFamily(ctx, stored.FamilyID, "reuse_detected")
		if rerr != nil {
			s.log.ErrorContext(ctx, "could not revoke a token family after reuse", slog.Any("error", rerr))
		}
		s.log.WarnContext(ctx, "refresh token reuse detected; family revoked",
			slog.String("user_id", stored.UserID.String()),
			slog.String("family_id", stored.FamilyID.String()),
			slog.Int64("tokens_revoked", revoked),
		)
		s.audit(ctx, AuditEntry{
			UserID: &stored.UserID, Action: "identity.refresh_reuse_detected",
			ResourceType: "refresh_token_family", ResourceID: stored.FamilyID.String(),
			IP: ip, UserAgent: userAgent,
			Metadata: map[string]any{"tokens_revoked": revoked},
		})
		return nil, apperr.Unauthenticated("session_revoked", "your session was ended for security reasons; please sign in again")
	}

	if !stored.ExpiresAt.After(now) {
		return nil, apperr.Unauthenticated("refresh_token_expired", "your session has expired; please sign in again")
	}

	user, err := s.repo.UserByID(ctx, stored.UserID)
	if err != nil {
		return nil, err
	}
	if !user.Status.CanAuthenticate() {
		if _, rerr := s.repo.RevokeRefreshTokenFamily(ctx, stored.FamilyID, "account_unavailable"); rerr != nil {
			s.log.ErrorContext(ctx, "could not revoke family for an unavailable account", slog.Any("error", rerr))
		}
		return nil, apperr.Forbidden("account_unavailable", "this account is not available")
	}

	var session *Session
	err = s.tx.InTx(ctx, func(ctx context.Context) error {
		newSession, serr := s.issueSession(ctx, user, ip, userAgent, stored.FamilyID)
		if serr != nil {
			return serr
		}
		// Revoke the presented token only after the replacement is persisted, so a
		// failure part-way through cannot leave the client with no usable token.
		if rerr := s.repo.RevokeRefreshToken(ctx, stored.ID, "rotated", nil); rerr != nil {
			return rerr
		}
		session = newSession
		return nil
	})
	if err != nil {
		return nil, err
	}
	return session, nil
}

// Logout revokes the presented refresh token.
//
// Idempotent and silent about unknown tokens: signing out must always appear to
// succeed, and an error here would only tell a caller whether a token was real.
func (s *Service) Logout(ctx context.Context, plaintext, ip, userAgent string) error {
	if strings.TrimSpace(plaintext) == "" {
		return nil
	}

	stored, err := s.repo.RefreshTokenByHash(ctx, HashRefreshToken(plaintext))
	if err != nil {
		if apperr.KindOf(err) == apperr.KindNotFound {
			return nil
		}
		return err
	}

	if err := s.repo.RevokeRefreshToken(ctx, stored.ID, "logout", nil); err != nil {
		return err
	}
	s.audit(ctx, AuditEntry{
		UserID: &stored.UserID, ActorID: &stored.UserID,
		Action: "identity.logout", ResourceType: "refresh_token", ResourceID: stored.ID.String(),
		IP: ip, UserAgent: userAgent,
	})
	return nil
}

// LogoutEverywhere revokes every live session for a user.
func (s *Service) LogoutEverywhere(ctx context.Context, userID uuid.UUID, ip, userAgent string) (int64, error) {
	n, err := s.repo.RevokeAllRefreshTokensForUser(ctx, userID, "logout_all")
	if err != nil {
		return 0, err
	}
	s.audit(ctx, AuditEntry{
		UserID: &userID, ActorID: &userID,
		Action: "identity.logout_all", ResourceType: "user", ResourceID: userID.String(),
		IP: ip, UserAgent: userAgent,
		Metadata: map[string]any{"sessions_revoked": n},
	})
	return n, nil
}

// VerifyEmail consumes a verification token and marks the address confirmed.
func (s *Service) VerifyEmail(ctx context.Context, plaintext, ip, userAgent string) error {
	if strings.TrimSpace(plaintext) == "" {
		return apperr.Validation("token_missing", "this link is invalid")
	}

	now := s.now()
	hash := HashRefreshToken(plaintext)

	var userID uuid.UUID
	err := s.tx.InTx(ctx, func(ctx context.Context) error {
		token, terr := s.repo.ConsumeVerificationToken(ctx, hash, PurposeEmailVerification, now)
		if terr != nil {
			return terr
		}
		userID = token.UserID
		return s.repo.MarkEmailVerified(ctx, token.UserID, now)
	})
	if err != nil {
		return err
	}

	s.audit(ctx, AuditEntry{
		UserID: &userID, ActorID: &userID,
		Action: "identity.email_verified", ResourceType: "user", ResourceID: userID.String(),
		IP: ip, UserAgent: userAgent,
	})
	s.publish(ctx, events.TypeEmailVerified, map[string]any{
		"user_id":     userID.String(),
		"verified_at": now,
	})
	return nil
}

// CurrentUser loads a user and profile for the authenticated principal.
func (s *Service) CurrentUser(ctx context.Context, userID uuid.UUID) (*User, *Profile, error) {
	user, err := s.repo.UserByID(ctx, userID)
	if err != nil {
		return nil, nil, err
	}
	profile, err := s.repo.ProfileByUserID(ctx, userID)
	if err != nil {
		return nil, nil, err
	}
	return user, profile, nil
}

// ProfileUpdate carries the fields a profile update may change.
//
// Pointers distinguish "not supplied" from "cleared", so a partial update from one
// section of the settings UI cannot blank fields it never showed.
type ProfileUpdate struct {
	FullName          *string
	Phone             *string
	City              *string
	Country           *string
	Timezone          *string
	LinkedInURL       *string
	PortfolioURL      *string
	GitHubURL         *string
	WorkAuthorisation *string
	SalaryMin         *float64
	SalaryMax         *float64
	SalaryCurrency    *string
	SalaryPeriod      *string
	NoticePeriodDays  *int
	OpenToRelocation  *bool
	Headline          *string
}

var e164Pattern = regexp.MustCompile(`^\+[1-9]\d{6,14}$`)

// UpdateProfile applies a partial profile update.
func (s *Service) UpdateProfile(ctx context.Context, userID uuid.UUID, in ProfileUpdate, ip, userAgent string) (*Profile, error) {
	profile, err := s.repo.ProfileByUserID(ctx, userID)
	if err != nil {
		return nil, err
	}

	var fieldErrors []apperr.FieldError
	changed := make([]string, 0, 8)

	applyString := func(name string, dst *string, src *string, validate func(string) *apperr.FieldError) {
		if src == nil {
			return
		}
		value := strings.TrimSpace(*src)
		if validate != nil {
			if fe := validate(value); fe != nil {
				fieldErrors = append(fieldErrors, *fe)
				return
			}
		}
		if *dst != value {
			*dst = value
			changed = append(changed, name)
		}
	}

	applyString("full_name", &profile.FullName, in.FullName, func(v string) *apperr.FieldError {
		if len([]rune(v)) > 200 {
			return &apperr.FieldError{Field: "full_name", Code: "too_long", Message: "must be at most 200 characters"}
		}
		return nil
	})
	applyString("city", &profile.City, in.City, func(v string) *apperr.FieldError {
		if len([]rune(v)) > 120 {
			return &apperr.FieldError{Field: "city", Code: "too_long", Message: "must be at most 120 characters"}
		}
		return nil
	})
	applyString("country", &profile.Country, in.Country, func(v string) *apperr.FieldError {
		if v == "" {
			return nil
		}
		if len(v) != 2 {
			return &apperr.FieldError{Field: "country", Code: "invalid", Message: "must be a two-letter ISO country code"}
		}
		return nil
	})
	applyString("timezone", &profile.Timezone, in.Timezone, func(v string) *apperr.FieldError {
		if v == "" {
			return nil
		}
		// Validated against the tz database rather than a regex: an unparseable zone
		// would break every scheduled discovery window for this candidate.
		if _, lerr := time.LoadLocation(v); lerr != nil {
			return &apperr.FieldError{Field: "timezone", Code: "invalid", Message: "must be a valid IANA timezone, e.g. Africa/Nairobi"}
		}
		return nil
	})
	applyString("linkedin_url", &profile.LinkedInURL, in.LinkedInURL, validateURLField("linkedin_url"))
	applyString("portfolio_url", &profile.PortfolioURL, in.PortfolioURL, validateURLField("portfolio_url"))
	applyString("github_url", &profile.GitHubURL, in.GitHubURL, validateURLField("github_url"))
	applyString("work_authorisation", &profile.WorkAuthorisation, in.WorkAuthorisation, func(v string) *apperr.FieldError {
		if len([]rune(v)) > 500 {
			return &apperr.FieldError{Field: "work_authorisation", Code: "too_long", Message: "must be at most 500 characters"}
		}
		return nil
	})
	applyString("headline", &profile.Headline, in.Headline, func(v string) *apperr.FieldError {
		if len([]rune(v)) > 200 {
			return &apperr.FieldError{Field: "headline", Code: "too_long", Message: "must be at most 200 characters"}
		}
		return nil
	})
	applyString("salary_currency", &profile.SalaryCurrency, in.SalaryCurrency, func(v string) *apperr.FieldError {
		if v != "" && len(v) != 3 {
			return &apperr.FieldError{Field: "salary_currency", Code: "invalid", Message: "must be a three-letter ISO currency code"}
		}
		return nil
	})
	applyString("salary_period", &profile.SalaryPeriod, in.SalaryPeriod, func(v string) *apperr.FieldError {
		switch v {
		case "", "hour", "day", "week", "month", "year":
			return nil
		default:
			return &apperr.FieldError{Field: "salary_period", Code: "invalid", Message: "must be one of hour, day, week, month, year"}
		}
	})

	if in.SalaryMin != nil {
		profile.SalaryMin = in.SalaryMin
		changed = append(changed, "salary_min")
	}
	if in.SalaryMax != nil {
		profile.SalaryMax = in.SalaryMax
		changed = append(changed, "salary_max")
	}
	// Checked here as well as by the database CHECK, so the client gets a field-level
	// error rather than a 500 from a constraint violation.
	if profile.SalaryMin != nil && profile.SalaryMax != nil && *profile.SalaryMin > *profile.SalaryMax {
		fieldErrors = append(fieldErrors, apperr.FieldError{
			Field: "salary_max", Code: "out_of_order", Message: "must be greater than or equal to the minimum",
		})
	}
	if in.NoticePeriodDays != nil {
		if *in.NoticePeriodDays < 0 || *in.NoticePeriodDays > 365 {
			fieldErrors = append(fieldErrors, apperr.FieldError{
				Field: "notice_period_days", Code: "out_of_range", Message: "must be between 0 and 365",
			})
		} else {
			profile.NoticePeriodDays = in.NoticePeriodDays
			changed = append(changed, "notice_period_days")
		}
	}
	if in.OpenToRelocation != nil {
		profile.OpenToRelocation = *in.OpenToRelocation
		changed = append(changed, "open_to_relocation")
	}

	// The phone number is the only encrypted field here. It is validated strictly
	// because an unparseable number silently breaks form auto-fill much later.
	var (
		phoneCiphertext []byte
		phoneKeyID      string
		phoneLastFour   string
	)
	if in.Phone != nil {
		phone := strings.ReplaceAll(strings.TrimSpace(*in.Phone), " ", "")
		switch {
		case phone == "":
			// Clearing a phone number is not supported through this path: it would
			// silently disable form auto-fill on every active loop. The UI offers an
			// explicit action for that instead.
			fieldErrors = append(fieldErrors, apperr.FieldError{
				Field: "phone", Code: "required",
				Message: "a phone number is required; use the remove-phone action to clear it",
			})
		case !e164Pattern.MatchString(phone):
			fieldErrors = append(fieldErrors, apperr.FieldError{
				Field: "phone", Code: "invalid",
				Message: "must be in international format, for example +254700000000",
			})
		default:
			ct, keyID, eerr := s.cipher.Encrypt([]byte(phone))
			if eerr != nil {
				return nil, apperr.Internalf(eerr, "encrypt phone number")
			}
			phoneCiphertext, phoneKeyID = ct, keyID
			phoneLastFour = phone[len(phone)-4:]
			profile.HasPhone = true
			profile.PhoneLastFour = phoneLastFour
			changed = append(changed, "phone")
		}
	}

	if len(fieldErrors) > 0 {
		return nil, apperr.ValidationFields(fieldErrors)
	}

	profile.CompletenessPct = profile.Completeness()

	if err := s.repo.UpdateProfile(ctx, profile, phoneCiphertext, phoneKeyID, phoneLastFour); err != nil {
		return nil, err
	}

	if len(changed) > 0 {
		s.audit(ctx, AuditEntry{
			UserID: &userID, ActorID: &userID, ActorRole: RoleCandidate,
			Action: "identity.profile_updated", ResourceType: "candidate_profile",
			ResourceID: userID.String(), IP: ip, UserAgent: userAgent,
			// Field names only; never their values, which are personal data.
			Metadata: map[string]any{"changed_fields": changed},
		})
		s.publish(ctx, events.TypeProfileUpdated, map[string]any{
			"user_id":          userID.String(),
			"changed_fields":   changed,
			"completeness_pct": profile.CompletenessPct,
		})
	}

	return profile, nil
}

func validateURLField(field string) func(string) *apperr.FieldError {
	return func(v string) *apperr.FieldError {
		if v == "" {
			return nil
		}
		if len(v) > 500 {
			return &apperr.FieldError{Field: field, Code: "too_long", Message: "must be at most 500 characters"}
		}
		// Scheme-restricted on purpose: these values are rendered as links, and
		// javascript: or data: URLs would be a stored-XSS vector in any client that
		// forgets to sanitise.
		if !strings.HasPrefix(v, "https://") && !strings.HasPrefix(v, "http://") {
			return &apperr.FieldError{Field: field, Code: "invalid", Message: "must be an http or https URL"}
		}
		return nil
	}
}

// ChangePassword replaces the password and revokes every other session.
func (s *Service) ChangePassword(ctx context.Context, userID uuid.UUID, current, next, ip, userAgent string) error {
	if err := ValidatePassword(next); err != nil {
		return err
	}

	user, err := s.repo.UserByID(ctx, userID)
	if err != nil {
		return err
	}
	if !user.HasPassword() {
		return apperr.PreconditionFailed("no_password_set",
			"this account signs in with a connected provider; set a password first")
	}
	if verr := s.hasher.Verify(current, user.PasswordHash); verr != nil {
		return apperr.Unauthenticated("current_password_incorrect", "your current password is incorrect")
	}
	if current == next {
		return apperr.ValidationFields([]apperr.FieldError{{
			Field: "new_password", Code: "unchanged", Message: "the new password must differ from the current one",
		}})
	}

	hash, err := s.hasher.Hash(next)
	if err != nil {
		return apperr.Internalf(err, "hash new password")
	}

	var revoked int64
	err = s.tx.InTx(ctx, func(ctx context.Context) error {
		if uerr := s.repo.UpdatePasswordHash(ctx, userID, hash); uerr != nil {
			return uerr
		}
		// Every existing session is revoked. If the password was changed because it
		// was compromised, leaving other sessions alive defeats the point.
		n, rerr := s.repo.RevokeAllRefreshTokensForUser(ctx, userID, "password_changed")
		revoked = n
		return rerr
	})
	if err != nil {
		return err
	}

	s.audit(ctx, AuditEntry{
		UserID: &userID, ActorID: &userID, ActorRole: user.Role,
		Action: "identity.password_changed", ResourceType: "user", ResourceID: userID.String(),
		IP: ip, UserAgent: userAgent,
		Metadata: map[string]any{"sessions_revoked": revoked},
	})
	return nil
}

// ParsePrincipal validates an access token. Used by the HTTP auth middleware.
func (s *Service) ParsePrincipal(raw, audience string) (Principal, error) {
	return s.tokens.ParseAccessToken(raw, audience)
}

// Tokens exposes the issuer for the JWKS endpoint.
func (s *Service) Tokens() *TokenIssuer { return s.tokens }

// audit writes an audit entry, logging rather than failing the request on error.
//
// A failed audit write must not fail the operation it describes: refusing a sign-in
// because the audit table is unavailable converts a logging outage into an
// authentication outage. The error is logged loudly instead, and an alert on that log
// is the right place to notice it.
func (s *Service) audit(ctx context.Context, e AuditEntry) {
	if err := s.repo.AppendAuditLog(ctx, e); err != nil {
		s.log.ErrorContext(ctx, "audit log write failed",
			slog.String("action", e.Action), slog.Any("error", err))
	}
}

// publish emits a domain event, logging rather than failing on error.
//
// Called after the transaction has committed, so a publish failure leaves committed
// state with no event. That is the lesser evil: publishing inside the transaction would
// risk announcing work that then rolled back, and consumers are idempotent so a
// reconciliation job can safely re-emit.
func (s *Service) publish(ctx context.Context, eventType string, payload any) {
	if s.bus == nil {
		return
	}
	if err := s.bus.Publish(ctx, eventType, 1, payload); err != nil {
		s.log.ErrorContext(ctx, "event publish failed",
			slog.String("event_type", eventType), slog.Any("error", err))
	}
}
