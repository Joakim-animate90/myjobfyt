package identity_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/Joakim-animate90/myjobfyt/backend/internal/apperr"
	"github.com/Joakim-animate90/myjobfyt/backend/internal/config"
	"github.com/Joakim-animate90/myjobfyt/backend/internal/domain/identity"
	"github.com/Joakim-animate90/myjobfyt/backend/internal/platform/dataenc"
	"github.com/Joakim-animate90/myjobfyt/backend/internal/platform/events"
	"github.com/google/uuid"
)

const goodPassword = "correct-horse-battery-staple"

type harness struct {
	svc   *identity.Service
	repo  *fakeRepo
	bus   *recordingBus
	tx    *passthroughTx
	clock *testClock
}

type testClock struct{ t time.Time }

func (c *testClock) now() time.Time          { return c.t }
func (c *testClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newHarness(t *testing.T) *harness {
	t.Helper()

	cfg := issuerConfig()
	cfg.MaxFailedLogins = 3
	cfg.LockoutWindow = 15 * time.Minute
	cfg.LockoutDuration = 15 * time.Minute
	cfg.VerificationTTL = 24 * time.Hour

	tokens, err := identity.NewTokenIssuer(cfg, config.EnvTest)
	if err != nil {
		t.Fatalf("NewTokenIssuer: %v", err)
	}

	cipher, err := dataenc.New([]dataenc.Key{{ID: "test", Material: make([]byte, dataenc.KeyLength)}})
	if err != nil {
		t.Fatalf("dataenc.New: %v", err)
	}

	clock := &testClock{t: time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)}
	repo := newFakeRepo()
	repo.now = clock.now
	bus := &recordingBus{}
	tx := &passthroughTx{}

	svc := identity.NewService(identity.ServiceDeps{
		Repo:   repo,
		Tx:     tx,
		Hasher: identity.NewPasswordHasher(cfg),
		Tokens: tokens,
		Cipher: cipher,
		Bus:    bus,
		Config: cfg,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:    clock.now,
	})

	return &harness{svc: svc, repo: repo, bus: bus, tx: tx, clock: clock}
}

func (h *harness) register(t *testing.T, email string) *identity.RegisterResult {
	t.Helper()
	res, err := h.svc.Register(context.Background(), identity.RegisterInput{
		Email: email, Password: goodPassword, FullName: "Test Candidate",
		AcceptedTerms: true, IP: "203.0.113.7", UserAgent: "test",
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	return res
}

// ---------------------------------------------------------------------------
// registration
// ---------------------------------------------------------------------------

func TestRegisterCreatesUserProfileAndVerificationToken(t *testing.T) {
	h := newHarness(t)

	res := h.register(t, "candidate@example.com")

	if !res.Created {
		t.Fatal("Created = false for a fresh address")
	}
	if res.VerificationToken == "" {
		t.Fatal("no verification token was issued; the account could never be verified")
	}
	if h.repo.storedVerificationCount() != 1 {
		t.Fatalf("stored verification tokens = %d, want 1", h.repo.storedVerificationCount())
	}

	user, err := h.repo.UserByEmail(context.Background(), "candidate@example.com")
	if err != nil {
		t.Fatalf("user was not created: %v", err)
	}
	if user.Role != identity.RoleCandidate {
		t.Errorf("Role = %q, want candidate; registration must never grant elevated roles", user.Role)
	}
	if user.IsVerified() {
		t.Error("a freshly registered account is already verified")
	}
	if !strings.HasPrefix(user.PasswordHash, "$argon2id$") {
		t.Errorf("stored hash is not argon2id: %q", user.PasswordHash)
	}
	if user.PasswordHash == goodPassword {
		t.Fatal("the password was stored in plaintext")
	}

	if _, err := h.repo.ProfileByUserID(context.Background(), user.ID); err != nil {
		t.Fatalf("profile was not created alongside the user: %v", err)
	}
	// User and profile must be created atomically, or a crash between them leaves an
	// account that cannot be used.
	if h.tx.calls == 0 {
		t.Error("registration did not run inside a transaction")
	}
	if !h.bus.has(events.TypeCandidateCreated) {
		t.Errorf("no candidate.created event published: %v", h.bus.types())
	}
	if !h.repo.hasAction("identity.registered") {
		t.Errorf("registration was not audited: %v", h.repo.actions())
	}
}

func TestRegisterNormalisesTheEmail(t *testing.T) {
	h := newHarness(t)
	h.register(t, "  Candidate@Example.COM  ")

	if _, err := h.repo.UserByEmail(context.Background(), "candidate@example.com"); err != nil {
		t.Fatalf("the email was not normalised before storage: %v", err)
	}
}

func TestRegisterDoesNotRevealAnExistingAccount(t *testing.T) {
	// A distinct "that email is taken" response turns registration into an
	// account-enumeration oracle: anyone could test whether a given person uses the
	// platform. The second attempt must look exactly like a success.
	h := newHarness(t)
	h.register(t, "candidate@example.com")

	res, err := h.svc.Register(context.Background(), identity.RegisterInput{
		Email: "candidate@example.com", Password: "a-completely-different-password",
		FullName: "Impostor", AcceptedTerms: true,
	})
	if err != nil {
		t.Fatalf("the duplicate registration returned an error, which reveals the account: %v", err)
	}
	if res.Created {
		t.Fatal("Created = true for an address that already exists")
	}
	if res.VerificationToken != "" {
		t.Fatal("a verification token was issued for an existing account; the owner would receive a confusing email")
	}
	// And the original password must still work: a duplicate registration must not
	// overwrite anything.
	if _, err := h.svc.Login(context.Background(), identity.LoginInput{
		Email: "candidate@example.com", Password: goodPassword,
	}); err != nil {
		t.Fatalf("the original credentials stopped working after a duplicate registration: %v", err)
	}
}

func TestRegisterCaseInsensitiveDuplicateIsAlsoSuppressed(t *testing.T) {
	h := newHarness(t)
	h.register(t, "candidate@example.com")

	res, err := h.svc.Register(context.Background(), identity.RegisterInput{
		Email: "CANDIDATE@EXAMPLE.COM", Password: goodPassword, AcceptedTerms: true,
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if res.Created {
		t.Fatal("a case-variant duplicate created a second account")
	}
}

func TestRegisterRejectsBadInput(t *testing.T) {
	cases := []struct {
		name  string
		input identity.RegisterInput
		field string
	}{
		{"missing email", identity.RegisterInput{Password: goodPassword, AcceptedTerms: true}, "email"},
		{"malformed email", identity.RegisterInput{Email: "not-an-email", Password: goodPassword, AcceptedTerms: true}, "email"},
		{"short password", identity.RegisterInput{Email: "a@example.com", Password: "short", AcceptedTerms: true}, "password"},
		{"common password", identity.RegisterInput{Email: "a@example.com", Password: "password1234", AcceptedTerms: true}, "password"},
		{"terms not accepted", identity.RegisterInput{Email: "a@example.com", Password: goodPassword}, "accepted_terms"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			_, err := h.svc.Register(context.Background(), tc.input)
			if err == nil {
				t.Fatal("Register accepted invalid input")
			}
			e := apperr.As(err)
			if e == nil || e.Kind != apperr.KindValidation {
				t.Fatalf("err = %v, want a validation error", err)
			}
			found := false
			for _, f := range e.Fields {
				if f.Field == tc.field {
					found = true
				}
			}
			if !found {
				t.Fatalf("fields = %+v, want one for %q", e.Fields, tc.field)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// login
// ---------------------------------------------------------------------------

func TestLoginIssuesAUsableSession(t *testing.T) {
	h := newHarness(t)
	h.register(t, "candidate@example.com")

	session, err := h.svc.Login(context.Background(), identity.LoginInput{
		Email: "candidate@example.com", Password: goodPassword,
		IP: "203.0.113.7", UserAgent: "test-agent",
	})
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	if session.AccessToken == "" || session.RefreshToken == "" {
		t.Fatal("Login returned an incomplete session")
	}
	if !session.AccessExpiresAt.After(h.clock.now()) {
		t.Error("the access token is already expired")
	}
	if !session.RefreshExpiresAt.After(session.AccessExpiresAt) {
		t.Error("the refresh token expires before the access token, so refreshing would be impossible")
	}
	if session.Profile == nil {
		t.Error("the session carries no profile; the client would have to make a second call to render anything")
	}

	principal, err := h.svc.ParsePrincipal(session.AccessToken, identity.AudienceAPI)
	if err != nil {
		t.Fatalf("the issued access token does not validate: %v", err)
	}
	if principal.Email != "candidate@example.com" {
		t.Errorf("principal email = %q", principal.Email)
	}
	if principal.PlanSlug != "free" {
		t.Errorf("principal plan = %q, want free", principal.PlanSlug)
	}
}

func TestLoginGivesOneGenericErrorForEveryFailureMode(t *testing.T) {
	// Any difference between "no such account", "wrong password" and "this account has
	// no password" is an enumeration oracle. They must be indistinguishable.
	h := newHarness(t)
	h.register(t, "candidate@example.com")

	cases := map[string]identity.LoginInput{
		"unknown address": {Email: "nobody@example.com", Password: goodPassword},
		"wrong password":  {Email: "candidate@example.com", Password: "wrong-password-entirely"},
		"empty password":  {Email: "candidate@example.com", Password: ""},
	}

	var codes []string
	for name, in := range cases {
		_, err := h.svc.Login(context.Background(), in)
		if err == nil {
			t.Fatalf("%s: Login succeeded", name)
		}
		e := apperr.As(err)
		if e == nil {
			t.Fatalf("%s: err = %v, want an *apperr.Error", name, err)
		}
		if e.Kind != apperr.KindUnauthenticated {
			t.Errorf("%s: Kind = %q, want unauthenticated", name, e.Kind)
		}
		codes = append(codes, e.Code)
	}
	for _, c := range codes {
		if c != codes[0] {
			t.Fatalf("failure codes differ across modes (%v); this leaks which accounts exist", codes)
		}
	}
}

func TestLoginLocksTheAccountAfterRepeatedFailures(t *testing.T) {
	h := newHarness(t)
	h.register(t, "candidate@example.com")

	// Threshold is 3 in the harness.
	for i := 0; i < 3; i++ {
		if _, err := h.svc.Login(context.Background(), identity.LoginInput{
			Email: "candidate@example.com", Password: "wrong-password-entirely",
		}); err == nil {
			t.Fatalf("attempt %d succeeded with a wrong password", i+1)
		}
	}

	// Now even the CORRECT password must be refused, with a rate-limit error rather
	// than a credential error, so the client can tell the user to wait.
	_, err := h.svc.Login(context.Background(), identity.LoginInput{
		Email: "candidate@example.com", Password: goodPassword,
	})
	if err == nil {
		t.Fatal("the correct password was accepted while the account was locked")
	}
	e := apperr.As(err)
	if e == nil || e.Kind != apperr.KindRateLimited {
		t.Fatalf("err = %v, want a rate-limited error", err)
	}
	if _, ok := e.Meta["retry_after_seconds"]; !ok {
		t.Error("the lockout error carries no retry hint")
	}
	if !h.repo.hasAction("identity.login_blocked_locked") {
		t.Errorf("the blocked attempt was not audited: %v", h.repo.actions())
	}
}

func TestLockoutExpiresAndTheCorrectPasswordWorksAgain(t *testing.T) {
	h := newHarness(t)
	h.register(t, "candidate@example.com")

	for i := 0; i < 3; i++ {
		_, _ = h.svc.Login(context.Background(), identity.LoginInput{
			Email: "candidate@example.com", Password: "wrong-password-entirely",
		})
	}
	h.clock.advance(16 * time.Minute) // past the 15-minute lockout

	if _, err := h.svc.Login(context.Background(), identity.LoginInput{
		Email: "candidate@example.com", Password: goodPassword,
	}); err != nil {
		t.Fatalf("sign-in still refused after the lockout expired: %v", err)
	}
}

func TestSuccessfulLoginClearsTheFailureCounter(t *testing.T) {
	// Otherwise a user who mistypes twice today and twice tomorrow is eventually
	// locked out despite never having a burst of failures.
	h := newHarness(t)
	h.register(t, "candidate@example.com")

	for i := 0; i < 2; i++ {
		_, _ = h.svc.Login(context.Background(), identity.LoginInput{
			Email: "candidate@example.com", Password: "wrong-password-entirely",
		})
	}
	if _, err := h.svc.Login(context.Background(), identity.LoginInput{
		Email: "candidate@example.com", Password: goodPassword,
	}); err != nil {
		t.Fatalf("Login: %v", err)
	}

	// Two more failures must not reach the threshold of three.
	for i := 0; i < 2; i++ {
		_, _ = h.svc.Login(context.Background(), identity.LoginInput{
			Email: "candidate@example.com", Password: "wrong-password-entirely",
		})
	}
	if _, err := h.svc.Login(context.Background(), identity.LoginInput{
		Email: "candidate@example.com", Password: goodPassword,
	}); err != nil {
		t.Fatalf("the counter was not reset by the successful sign-in: %v", err)
	}
}

func TestLoginRefusesASuspendedAccount(t *testing.T) {
	h := newHarness(t)
	res := h.register(t, "candidate@example.com")

	h.repo.mu.Lock()
	h.repo.users[res.UserID].Status = identity.StatusSuspended
	h.repo.mu.Unlock()

	_, err := h.svc.Login(context.Background(), identity.LoginInput{
		Email: "candidate@example.com", Password: goodPassword,
	})
	if apperr.KindOf(err) != apperr.KindForbidden {
		t.Fatalf("err = %v, want forbidden", err)
	}
}

func TestLoginStillWorksDuringTheDeletionGracePeriod(t *testing.T) {
	// The 7-day grace period is pointless if the candidate is locked out of the
	// account they want to recover.
	h := newHarness(t)
	res := h.register(t, "candidate@example.com")

	h.repo.mu.Lock()
	h.repo.users[res.UserID].Status = identity.StatusPendingDeletion
	h.repo.mu.Unlock()

	if _, err := h.svc.Login(context.Background(), identity.LoginInput{
		Email: "candidate@example.com", Password: goodPassword,
	}); err != nil {
		t.Fatalf("a pending-deletion account could not sign in to cancel: %v", err)
	}
}

func TestLoginSucceedsWhenThePlanLookupFails(t *testing.T) {
	// A billing outage must not become an authentication outage. The fallback is the
	// free tier, which is the conservative direction.
	h := newHarness(t)
	h.register(t, "candidate@example.com")
	h.repo.planErr = errors.New("subscriptions table unavailable")

	session, err := h.svc.Login(context.Background(), identity.LoginInput{
		Email: "candidate@example.com", Password: goodPassword,
	})
	if err != nil {
		t.Fatalf("Login failed because the plan lookup failed: %v", err)
	}
	principal, err := h.svc.ParsePrincipal(session.AccessToken, identity.AudienceAPI)
	if err != nil {
		t.Fatalf("ParsePrincipal: %v", err)
	}
	if principal.PlanSlug != "free" {
		t.Errorf("plan = %q, want the conservative free fallback", principal.PlanSlug)
	}
}

func TestLoginSucceedsWhenTheAuditWriteFails(t *testing.T) {
	// Refusing a sign-in because the audit table is unavailable turns a logging
	// problem into an authentication problem.
	h := newHarness(t)
	h.register(t, "candidate@example.com")
	h.repo.auditErr = errors.New("audit_logs is unavailable")

	if _, err := h.svc.Login(context.Background(), identity.LoginInput{
		Email: "candidate@example.com", Password: goodPassword,
	}); err != nil {
		t.Fatalf("Login failed because the audit write failed: %v", err)
	}
}

func TestRegisterSucceedsWhenTheEventPublishFails(t *testing.T) {
	h := newHarness(t)
	h.bus.err = errors.New("nats unavailable")

	res, err := h.svc.Register(context.Background(), identity.RegisterInput{
		Email: "candidate@example.com", Password: goodPassword, AcceptedTerms: true,
	})
	if err != nil {
		t.Fatalf("Register failed because the event publish failed: %v", err)
	}
	if !res.Created {
		t.Fatal("the account was not created")
	}
}

// ---------------------------------------------------------------------------
// refresh and token theft
// ---------------------------------------------------------------------------

func TestRefreshRotatesTheToken(t *testing.T) {
	h := newHarness(t)
	h.register(t, "candidate@example.com")
	first, err := h.svc.Login(context.Background(), identity.LoginInput{
		Email: "candidate@example.com", Password: goodPassword,
	})
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	second, err := h.svc.Refresh(context.Background(), first.RefreshToken, "203.0.113.7", "test")
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if second.RefreshToken == first.RefreshToken {
		t.Fatal("the refresh token was not rotated; a leaked token would stay valid forever")
	}
	if second.AccessToken == "" {
		t.Fatal("Refresh returned no access token")
	}
	if _, err := h.svc.ParsePrincipal(second.AccessToken, identity.AudienceAPI); err != nil {
		t.Fatalf("the refreshed access token does not validate: %v", err)
	}
}

func TestReusingARotatedRefreshTokenRevokesTheWholeFamily(t *testing.T) {
	// The scenario: an attacker copies a refresh token. Both they and the legitimate
	// client try to use it. Whoever presents the superseded token proves a copy
	// exists, and the only safe response is to destroy the chain.
	h := newHarness(t)
	h.register(t, "candidate@example.com")
	first, err := h.svc.Login(context.Background(), identity.LoginInput{
		Email: "candidate@example.com", Password: goodPassword,
	})
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	second, err := h.svc.Refresh(context.Background(), first.RefreshToken, "203.0.113.7", "legit")
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	// Replay the superseded token.
	_, err = h.svc.Refresh(context.Background(), first.RefreshToken, "198.51.100.9", "attacker")
	if err == nil {
		t.Fatal("a superseded refresh token was accepted")
	}
	e := apperr.As(err)
	if e == nil || e.Code != "session_revoked" {
		t.Fatalf("err = %v, want session_revoked", err)
	}

	// The legitimate client's newest token must now also be dead: we cannot tell which
	// party is which, so the entire family goes.
	if _, err := h.svc.Refresh(context.Background(), second.RefreshToken, "203.0.113.7", "legit"); err == nil {
		t.Fatal("the newest token in a compromised family still works")
	}
	if !h.repo.hasAction("identity.refresh_reuse_detected") {
		t.Errorf("token reuse was not audited: %v", h.repo.actions())
	}
}

func TestRefreshRejectsUnknownExpiredAndEmptyTokens(t *testing.T) {
	h := newHarness(t)
	h.register(t, "candidate@example.com")
	session, err := h.svc.Login(context.Background(), identity.LoginInput{
		Email: "candidate@example.com", Password: goodPassword,
	})
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	if _, err := h.svc.Refresh(context.Background(), "", "", ""); err == nil {
		t.Error("an empty refresh token was accepted")
	}
	if _, err := h.svc.Refresh(context.Background(), "not-a-real-token", "", ""); err == nil {
		t.Error("an unknown refresh token was accepted")
	}

	h.clock.advance(721 * time.Hour) // past the 720h refresh TTL
	if _, err := h.svc.Refresh(context.Background(), session.RefreshToken, "", ""); err == nil {
		t.Error("an expired refresh token was accepted")
	}
}

func TestRefreshRefusesASuspendedAccountAndKillsTheFamily(t *testing.T) {
	h := newHarness(t)
	res := h.register(t, "candidate@example.com")
	session, err := h.svc.Login(context.Background(), identity.LoginInput{
		Email: "candidate@example.com", Password: goodPassword,
	})
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	h.repo.mu.Lock()
	h.repo.users[res.UserID].Status = identity.StatusSuspended
	h.repo.mu.Unlock()

	if _, err := h.svc.Refresh(context.Background(), session.RefreshToken, "", ""); apperr.KindOf(err) != apperr.KindForbidden {
		t.Fatalf("err = %v, want forbidden", err)
	}
	// Suspension must take effect immediately, not when the refresh token happens to
	// expire a month later.
	h.repo.mu.Lock()
	stored := h.repo.refresh[string(identity.HashRefreshToken(session.RefreshToken))]
	revoked := stored.RevokedAt != nil
	h.repo.mu.Unlock()
	if !revoked {
		t.Fatal("the refresh token survived account suspension")
	}
}

// ---------------------------------------------------------------------------
// logout
// ---------------------------------------------------------------------------

func TestLogoutRevokesTheTokenAndIsIdempotent(t *testing.T) {
	h := newHarness(t)
	h.register(t, "candidate@example.com")
	session, err := h.svc.Login(context.Background(), identity.LoginInput{
		Email: "candidate@example.com", Password: goodPassword,
	})
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	if err := h.svc.Logout(context.Background(), session.RefreshToken, "", ""); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	if _, err := h.svc.Refresh(context.Background(), session.RefreshToken, "", ""); err == nil {
		t.Fatal("the refresh token still works after logout")
	}
	// Signing out twice, or with a token we have never seen, must still appear to
	// succeed: an error would reveal whether a token was real.
	if err := h.svc.Logout(context.Background(), session.RefreshToken, "", ""); err != nil {
		t.Errorf("a second logout returned an error: %v", err)
	}
	if err := h.svc.Logout(context.Background(), "never-existed", "", ""); err != nil {
		t.Errorf("logout with an unknown token returned an error: %v", err)
	}
	if err := h.svc.Logout(context.Background(), "", "", ""); err != nil {
		t.Errorf("logout with no token returned an error: %v", err)
	}
}

func TestLogoutEverywhereRevokesEverySession(t *testing.T) {
	h := newHarness(t)
	res := h.register(t, "candidate@example.com")

	var sessions []*identity.Session
	for i := 0; i < 3; i++ {
		s, err := h.svc.Login(context.Background(), identity.LoginInput{
			Email: "candidate@example.com", Password: goodPassword,
		})
		if err != nil {
			t.Fatalf("Login %d: %v", i, err)
		}
		sessions = append(sessions, s)
	}

	n, err := h.svc.LogoutEverywhere(context.Background(), res.UserID, "", "")
	if err != nil {
		t.Fatalf("LogoutEverywhere: %v", err)
	}
	if n != 3 {
		t.Errorf("revoked %d sessions, want 3", n)
	}
	for i, s := range sessions {
		if _, err := h.svc.Refresh(context.Background(), s.RefreshToken, "", ""); err == nil {
			t.Errorf("session %d still works after logout-everywhere", i)
		}
	}
}

// ---------------------------------------------------------------------------
// email verification
// ---------------------------------------------------------------------------

func TestVerifyEmailMarksTheAccountVerified(t *testing.T) {
	h := newHarness(t)
	res := h.register(t, "candidate@example.com")

	if err := h.svc.VerifyEmail(context.Background(), res.VerificationToken, "", ""); err != nil {
		t.Fatalf("VerifyEmail: %v", err)
	}

	user, err := h.repo.UserByID(context.Background(), res.UserID)
	if err != nil {
		t.Fatalf("UserByID: %v", err)
	}
	if !user.IsVerified() {
		t.Fatal("the account is still unverified")
	}
	if !h.bus.has(events.TypeEmailVerified) {
		t.Errorf("no email.verified event: %v", h.bus.types())
	}
}

func TestVerificationTokenIsSingleUse(t *testing.T) {
	h := newHarness(t)
	res := h.register(t, "candidate@example.com")

	if err := h.svc.VerifyEmail(context.Background(), res.VerificationToken, "", ""); err != nil {
		t.Fatalf("first VerifyEmail: %v", err)
	}
	if err := h.svc.VerifyEmail(context.Background(), res.VerificationToken, "", ""); err == nil {
		t.Fatal("the verification token was accepted twice")
	}
}

func TestVerifyEmailRejectsUnknownAndExpiredTokens(t *testing.T) {
	h := newHarness(t)
	res := h.register(t, "candidate@example.com")

	if err := h.svc.VerifyEmail(context.Background(), "never-issued", "", ""); err == nil {
		t.Error("an unknown verification token was accepted")
	}
	if err := h.svc.VerifyEmail(context.Background(), "", "", ""); err == nil {
		t.Error("an empty verification token was accepted")
	}

	h.clock.advance(25 * time.Hour) // past the 24h verification TTL
	if err := h.svc.VerifyEmail(context.Background(), res.VerificationToken, "", ""); err == nil {
		t.Error("an expired verification token was accepted")
	}
}

// ---------------------------------------------------------------------------
// profile
// ---------------------------------------------------------------------------

func strptr(s string) *string { return &s }

func TestUpdateProfileAppliesOnlySuppliedFields(t *testing.T) {
	// A partial update from one section of the settings UI must not blank fields that
	// section never showed.
	h := newHarness(t)
	res := h.register(t, "candidate@example.com")
	ctx := context.Background()

	if _, err := h.svc.UpdateProfile(ctx, res.UserID, identity.ProfileUpdate{
		City:    strptr("Nairobi"),
		Country: strptr("KE"),
	}, "", ""); err != nil {
		t.Fatalf("UpdateProfile: %v", err)
	}

	// A second update touching only the headline must leave the city alone.
	profile, err := h.svc.UpdateProfile(ctx, res.UserID, identity.ProfileUpdate{
		Headline: strptr("Senior Go Engineer"),
	}, "", "")
	if err != nil {
		t.Fatalf("UpdateProfile: %v", err)
	}
	if profile.City != "Nairobi" {
		t.Errorf("City = %q, want it preserved across a partial update", profile.City)
	}
	if profile.Headline != "Senior Go Engineer" {
		t.Errorf("Headline = %q", profile.Headline)
	}
	// The name came from registration and must also survive.
	if profile.FullName != "Test Candidate" {
		t.Errorf("FullName = %q, want it preserved", profile.FullName)
	}
}

func TestUpdateProfileEncryptsThePhoneAndExposesOnlyTheLastFour(t *testing.T) {
	h := newHarness(t)
	res := h.register(t, "candidate@example.com")

	profile, err := h.svc.UpdateProfile(context.Background(), res.UserID, identity.ProfileUpdate{
		Phone: strptr("+254700123456"),
	}, "", "")
	if err != nil {
		t.Fatalf("UpdateProfile: %v", err)
	}

	if !profile.HasPhone {
		t.Fatal("HasPhone = false after setting a phone number")
	}
	if profile.PhoneLastFour != "3456" {
		t.Errorf("PhoneLastFour = %q, want 3456", profile.PhoneLastFour)
	}
	// The model returned towards a client must carry no field that could hold the full
	// number; only the last four digits exist on it.
	if strings.Contains(profile.PhoneLastFour, "254700") {
		t.Fatal("the full phone number leaked into the client-facing model")
	}
}

func TestUpdateProfileValidatesThePhoneFormat(t *testing.T) {
	// An unparseable number silently breaks form auto-fill much later, so it is
	// rejected up front.
	h := newHarness(t)
	res := h.register(t, "candidate@example.com")

	for _, phone := range []string{"0700123456", "254700123456", "+0700123456", "not a phone", "+1", ""} {
		_, err := h.svc.UpdateProfile(context.Background(), res.UserID, identity.ProfileUpdate{
			Phone: strptr(phone),
		}, "", "")
		if err == nil {
			t.Errorf("UpdateProfile accepted phone %q", phone)
			continue
		}
		if apperr.KindOf(err) != apperr.KindValidation {
			t.Errorf("phone %q: err = %v, want a validation error", phone, err)
		}
	}
}

func TestUpdateProfileRejectsNonHTTPURLs(t *testing.T) {
	// These values are rendered as links; a javascript: URL would be stored XSS in any
	// client that forgets to sanitise.
	h := newHarness(t)
	res := h.register(t, "candidate@example.com")

	for _, url := range []string{
		"javascript:alert(1)",
		"data:text/html,<script>alert(1)</script>",
		"file:///etc/passwd",
		"linkedin.com/in/example",
	} {
		_, err := h.svc.UpdateProfile(context.Background(), res.UserID, identity.ProfileUpdate{
			LinkedInURL: strptr(url),
		}, "", "")
		if err == nil {
			t.Errorf("UpdateProfile accepted URL %q", url)
		}
	}

	if _, err := h.svc.UpdateProfile(context.Background(), res.UserID, identity.ProfileUpdate{
		LinkedInURL: strptr("https://linkedin.com/in/example"),
	}, "", ""); err != nil {
		t.Errorf("a valid https URL was rejected: %v", err)
	}
}

func TestUpdateProfileRejectsAnInvalidTimezone(t *testing.T) {
	// An unparseable zone would break every scheduled discovery window for this
	// candidate, and the symptom would appear days later.
	h := newHarness(t)
	res := h.register(t, "candidate@example.com")

	if _, err := h.svc.UpdateProfile(context.Background(), res.UserID, identity.ProfileUpdate{
		Timezone: strptr("Mars/Olympus_Mons"),
	}, "", ""); err == nil {
		t.Fatal("an invalid timezone was accepted")
	}
	if _, err := h.svc.UpdateProfile(context.Background(), res.UserID, identity.ProfileUpdate{
		Timezone: strptr("Africa/Nairobi"),
	}, "", ""); err != nil {
		t.Fatalf("a valid IANA timezone was rejected: %v", err)
	}
}

func TestUpdateProfileRejectsAnInvertedSalaryRange(t *testing.T) {
	// Caught in the service so the client gets a field error rather than a 500 from
	// the database CHECK.
	h := newHarness(t)
	res := h.register(t, "candidate@example.com")

	min, max := 90000.0, 50000.0
	_, err := h.svc.UpdateProfile(context.Background(), res.UserID, identity.ProfileUpdate{
		SalaryMin: &min, SalaryMax: &max,
	}, "", "")
	e := apperr.As(err)
	if e == nil || e.Kind != apperr.KindValidation {
		t.Fatalf("err = %v, want a validation error", err)
	}
	if len(e.Fields) == 0 || e.Fields[0].Field != "salary_max" {
		t.Fatalf("fields = %+v, want one on salary_max", e.Fields)
	}
}

func TestUpdateProfileRecalculatesCompleteness(t *testing.T) {
	h := newHarness(t)
	res := h.register(t, "candidate@example.com")

	before, err := h.repo.ProfileByUserID(context.Background(), res.UserID)
	if err != nil {
		t.Fatalf("ProfileByUserID: %v", err)
	}

	after, err := h.svc.UpdateProfile(context.Background(), res.UserID, identity.ProfileUpdate{
		Phone:   strptr("+254700123456"),
		City:    strptr("Nairobi"),
		Country: strptr("KE"),
	}, "", "")
	if err != nil {
		t.Fatalf("UpdateProfile: %v", err)
	}
	if after.CompletenessPct <= before.CompletenessPct {
		t.Fatalf("completeness went from %d to %d after filling three fields",
			before.CompletenessPct, after.CompletenessPct)
	}
	if !after.FormAutofillReady() {
		t.Error("FormAutofillReady() = false although both phone and city are set")
	}
}

func TestUpdateProfileAuditsFieldNamesButNotValues(t *testing.T) {
	// Audit metadata is retained for years. Field names are useful; the values are
	// personal data and must not be copied there.
	h := newHarness(t)
	res := h.register(t, "candidate@example.com")

	if _, err := h.svc.UpdateProfile(context.Background(), res.UserID, identity.ProfileUpdate{
		Phone: strptr("+254700123456"),
		City:  strptr("Nairobi"),
	}, "", ""); err != nil {
		t.Fatalf("UpdateProfile: %v", err)
	}

	h.repo.mu.Lock()
	entries := append([]identity.AuditEntry(nil), h.repo.auditEntries...)
	h.repo.mu.Unlock()

	var found bool
	for _, e := range entries {
		if e.Action != "identity.profile_updated" {
			continue
		}
		found = true
		changed, ok := e.Metadata["changed_fields"].([]string)
		if !ok {
			t.Fatalf("changed_fields is %T, want []string", e.Metadata["changed_fields"])
		}
		for _, name := range changed {
			if strings.Contains(name, "254700") || strings.Contains(name, "Nairobi") {
				t.Fatalf("an audit entry carries a field VALUE: %q", name)
			}
		}
	}
	if !found {
		t.Errorf("the profile update was not audited: %v", h.repo.actions())
	}
}

// ---------------------------------------------------------------------------
// password change
// ---------------------------------------------------------------------------

func TestChangePasswordRevokesEverySession(t *testing.T) {
	// If the password was changed because it leaked, leaving other sessions alive
	// defeats the point entirely.
	h := newHarness(t)
	res := h.register(t, "candidate@example.com")

	var sessions []*identity.Session
	for i := 0; i < 2; i++ {
		s, err := h.svc.Login(context.Background(), identity.LoginInput{
			Email: "candidate@example.com", Password: goodPassword,
		})
		if err != nil {
			t.Fatalf("Login: %v", err)
		}
		sessions = append(sessions, s)
	}

	const newPassword = "an-entirely-different-passphrase"
	if err := h.svc.ChangePassword(context.Background(), res.UserID, goodPassword, newPassword, "", ""); err != nil {
		t.Fatalf("ChangePassword: %v", err)
	}

	for i, s := range sessions {
		if _, err := h.svc.Refresh(context.Background(), s.RefreshToken, "", ""); err == nil {
			t.Errorf("session %d survived the password change", i)
		}
	}
	if _, err := h.svc.Login(context.Background(), identity.LoginInput{
		Email: "candidate@example.com", Password: newPassword,
	}); err != nil {
		t.Fatalf("the new password does not work: %v", err)
	}
	if _, err := h.svc.Login(context.Background(), identity.LoginInput{
		Email: "candidate@example.com", Password: goodPassword,
	}); err == nil {
		t.Fatal("the old password still works")
	}
}

func TestChangePasswordRequiresTheCurrentPassword(t *testing.T) {
	h := newHarness(t)
	res := h.register(t, "candidate@example.com")

	err := h.svc.ChangePassword(context.Background(), res.UserID,
		"not-the-current-password", "an-entirely-different-passphrase", "", "")
	if apperr.KindOf(err) != apperr.KindUnauthenticated {
		t.Fatalf("err = %v, want unauthenticated", err)
	}
}

func TestChangePasswordRejectsAnUnchangedOrWeakNewPassword(t *testing.T) {
	h := newHarness(t)
	res := h.register(t, "candidate@example.com")

	if err := h.svc.ChangePassword(context.Background(), res.UserID, goodPassword, goodPassword, "", ""); err == nil {
		t.Error("the same password was accepted as a change")
	}
	if err := h.svc.ChangePassword(context.Background(), res.UserID, goodPassword, "short", "", ""); err == nil {
		t.Error("a password below the length floor was accepted")
	}
	if err := h.svc.ChangePassword(context.Background(), res.UserID, goodPassword, "password1234", "", ""); err == nil {
		t.Error("a common password was accepted")
	}
}

// ---------------------------------------------------------------------------
// current user
// ---------------------------------------------------------------------------

func TestCurrentUserReturnsUserAndProfile(t *testing.T) {
	h := newHarness(t)
	res := h.register(t, "candidate@example.com")

	user, profile, err := h.svc.CurrentUser(context.Background(), res.UserID)
	if err != nil {
		t.Fatalf("CurrentUser: %v", err)
	}
	if user.Email != "candidate@example.com" {
		t.Errorf("Email = %q", user.Email)
	}
	if profile.UserID != res.UserID {
		t.Errorf("profile belongs to %v, want %v", profile.UserID, res.UserID)
	}
}

func TestCurrentUserReportsNotFoundForAnUnknownID(t *testing.T) {
	h := newHarness(t)
	if _, _, err := h.svc.CurrentUser(context.Background(), uuid.New()); apperr.KindOf(err) != apperr.KindNotFound {
		t.Fatalf("err = %v, want not found", err)
	}
}

func TestValidateEmail(t *testing.T) {
	valid := []string{
		"a@b.co", "first.last@example.com", "user+tag@example.co.uk",
		"UPPER@EXAMPLE.COM", "  spaced@example.com  ",
	}
	for _, email := range valid {
		if err := identity.ValidateEmail(email); err != nil {
			t.Errorf("ValidateEmail(%q) = %v, want nil", email, err)
		}
	}

	invalid := []string{
		"", "no-at-sign", "@example.com", "user@", "user@localhost",
		"user@@example.com", "user name@example.com",
		strings.Repeat("a", 250) + "@example.com",
	}
	for _, email := range invalid {
		if err := identity.ValidateEmail(email); err == nil {
			t.Errorf("ValidateEmail(%q) = nil, want an error", email)
		}
	}
}
