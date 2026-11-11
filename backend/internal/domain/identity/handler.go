package identity

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/Joakim-animate90/myjobfyt/backend/internal/apperr"
	"github.com/Joakim-animate90/myjobfyt/backend/internal/config"
	"github.com/Joakim-animate90/myjobfyt/backend/internal/httpx"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// Mailer sends the transactional messages this domain triggers.
//
// Defined here as the narrow interface the handler needs, so identity does not depend
// on a mail implementation and tests can assert on what would have been sent.
type Mailer interface {
	SendVerificationEmail(to, token string) error
}

// Handler exposes the identity domain over HTTP.
//
// It contains no business logic: it binds, delegates, and shapes responses. Every
// decision worth testing lives in the service.
type Handler struct {
	svc        *Service
	mailer     Mailer
	cfg        config.AuthConfig
	env        config.Environment
	log        *slog.Logger
	trustProxy bool
}

// NewHandler builds the identity HTTP handler.
func NewHandler(svc *Service, mailer Mailer, cfg config.AuthConfig, env config.Environment, trustProxy bool, log *slog.Logger) *Handler {
	return &Handler{svc: svc, mailer: mailer, cfg: cfg, env: env, trustProxy: trustProxy, log: log}
}

// refreshCookieName is the httpOnly cookie carrying the refresh token.
//
// A cookie rather than a response body field: JavaScript must not be able to read a
// refresh token, because that is what turns any XSS into persistent account access.
// The access token does go in the body, since it is short-lived and the client needs
// it for the Authorization header.
const refreshCookieName = "myjobfyt_refresh"

// refreshCookiePath scopes the cookie to the auth endpoints, so it is not attached to
// every API request that has no use for it.
const refreshCookiePath = "/v1/auth"

// RouteDeps are the middleware the router composes these routes with.
//
// Passed in rather than constructed here so this package stays unaware of how
// authentication and throttling are wired.
type RouteDeps struct {
	// RequireAuth rejects unauthenticated requests.
	RequireAuth func(http.Handler) http.Handler
	// CredentialRateLimit is the strict, fail-closed limiter.
	CredentialRateLimit func(http.Handler) http.Handler
}

// RegisterRoutes mounts the identity routes.
//
// The strict credential limiter is applied ONLY to the endpoints that accept or mint
// credentials. Applying it more broadly is a real foot-gun: a candidate browsing their
// own settings would be throttled after a handful of requests, and a client polling
// JWKS to validate tokens would be locked out of verifying anything. Those routes get
// the general API limiter from the router instead.
func (h *Handler) RegisterRoutes(r chi.Router, deps RouteDeps) {
	r.Route("/auth", func(r chi.Router) {
		// Brute-force surface: register, login and the token endpoints.
		r.Group(func(r chi.Router) {
			if deps.CredentialRateLimit != nil {
				r.Use(deps.CredentialRateLimit)
			}
			r.Post("/register", h.register)
			r.Post("/login", h.login)
			r.Post("/refresh", h.refresh)
			r.Post("/verify-email", h.verifyEmail)
		})

		// Signing out must always work, so it is never behind the strict limiter: a
		// throttled logout would leave a user unable to end a session they are trying
		// to end, which is the opposite of a security control.
		r.Post("/logout", h.logout)

		// Public verification key. Clients fetch it to validate tokens, so throttling
		// it would break verification for everyone behind one NAT.
		r.Get("/jwks", h.jwks)
	})

	r.Group(func(r chi.Router) {
		r.Use(deps.RequireAuth)
		r.Get("/me", h.me)
		r.Patch("/me/profile", h.updateProfile)
		r.Post("/me/logout-all", h.logoutAll)

		// A password change verifies the current password, so it is a credential
		// endpoint and belongs behind the strict limiter even though it is
		// authenticated.
		r.Group(func(r chi.Router) {
			if deps.CredentialRateLimit != nil {
				r.Use(deps.CredentialRateLimit)
			}
			r.Post("/me/password", h.changePassword)
		})
	})
}

// ParseBearer adapts the service to the transport's PrincipalParser, converting the
// domain principal into the transport one.
func (h *Handler) ParseBearer(token, audience string) (httpx.Principal, error) {
	p, err := h.svc.ParsePrincipal(token, audience)
	if err != nil {
		return httpx.Principal{}, err
	}
	return httpx.Principal{
		UserID:   p.UserID.String(),
		Email:    p.Email,
		Role:     string(p.Role),
		PlanSlug: p.PlanSlug,
		TokenID:  p.TokenID,
	}, nil
}

// --- requests and responses -------------------------------------------------

type registerRequest struct {
	Email         string `json:"email" validate:"required"`
	Password      string `json:"password" validate:"required"`
	FullName      string `json:"full_name" validate:"omitempty,max=200"`
	Timezone      string `json:"timezone" validate:"omitempty,max=64"`
	AcceptedTerms bool   `json:"accepted_terms"`
}

type sessionResponse struct {
	AccessToken string           `json:"access_token"`
	TokenType   string           `json:"token_type"`
	ExpiresIn   int              `json:"expires_in"`
	ExpiresAt   time.Time        `json:"expires_at"`
	User        userResponse     `json:"user"`
	Profile     *profileResponse `json:"profile,omitempty"`
}

type userResponse struct {
	ID              string     `json:"id"`
	Email           string     `json:"email"`
	Role            string     `json:"role"`
	Status          string     `json:"status"`
	EmailVerified   bool       `json:"email_verified"`
	EmailVerifiedAt *time.Time `json:"email_verified_at,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
}

type profileResponse struct {
	FullName          string   `json:"full_name"`
	HasPhone          bool     `json:"has_phone"`
	PhoneLastFour     string   `json:"phone_last_four,omitempty"`
	City              string   `json:"city"`
	Country           string   `json:"country"`
	Timezone          string   `json:"timezone"`
	LinkedInURL       string   `json:"linkedin_url"`
	PortfolioURL      string   `json:"portfolio_url"`
	GitHubURL         string   `json:"github_url"`
	WorkAuthorisation string   `json:"work_authorisation"`
	SalaryMin         *float64 `json:"salary_min,omitempty"`
	SalaryMax         *float64 `json:"salary_max,omitempty"`
	SalaryCurrency    string   `json:"salary_currency,omitempty"`
	SalaryPeriod      string   `json:"salary_period,omitempty"`
	NoticePeriodDays  *int     `json:"notice_period_days,omitempty"`
	OpenToRelocation  bool     `json:"open_to_relocation"`
	Headline          string   `json:"headline"`
	CompletenessPct   int      `json:"completeness_pct"`
	// MissingFields tells the UI exactly what to prompt for, instead of leaving it to
	// infer the gaps from a bare percentage.
	MissingFields     []string `json:"missing_fields"`
	FormAutofillReady bool     `json:"form_autofill_ready"`
}

func toUserResponse(u *User) userResponse {
	return userResponse{
		ID:              u.ID.String(),
		Email:           u.Email,
		Role:            string(u.Role),
		Status:          string(u.Status),
		EmailVerified:   u.IsVerified(),
		EmailVerifiedAt: u.EmailVerifiedAt,
		CreatedAt:       u.CreatedAt,
	}
}

func toProfileResponse(p *Profile) *profileResponse {
	if p == nil {
		return nil
	}
	missing := p.MissingForCompleteness()
	if missing == nil {
		missing = []string{}
	}
	return &profileResponse{
		FullName:          p.FullName,
		HasPhone:          p.HasPhone,
		PhoneLastFour:     p.PhoneLastFour,
		City:              p.City,
		Country:           p.Country,
		Timezone:          p.Timezone,
		LinkedInURL:       p.LinkedInURL,
		PortfolioURL:      p.PortfolioURL,
		GitHubURL:         p.GitHubURL,
		WorkAuthorisation: p.WorkAuthorisation,
		SalaryMin:         p.SalaryMin,
		SalaryMax:         p.SalaryMax,
		SalaryCurrency:    p.SalaryCurrency,
		SalaryPeriod:      p.SalaryPeriod,
		NoticePeriodDays:  p.NoticePeriodDays,
		OpenToRelocation:  p.OpenToRelocation,
		Headline:          p.Headline,
		CompletenessPct:   p.CompletenessPct,
		MissingFields:     missing,
		FormAutofillReady: p.FormAutofillReady(),
	}
}

// --- handlers ---------------------------------------------------------------

func (h *Handler) register(w http.ResponseWriter, r *http.Request) {
	var in registerRequest
	if err := httpx.Bind(r, &in); err != nil {
		httpx.Error(w, r, err)
		return
	}

	res, err := h.svc.Register(r.Context(), RegisterInput{
		Email:         in.Email,
		Password:      in.Password,
		FullName:      in.FullName,
		Timezone:      in.Timezone,
		AcceptedTerms: in.AcceptedTerms,
		IP:            httpx.ClientIP(r, h.trustProxy),
		UserAgent:     r.UserAgent(),
	})
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	if res.Created && h.mailer != nil {
		if mErr := h.mailer.SendVerificationEmail(NormaliseEmail(in.Email), res.VerificationToken); mErr != nil {
			// The account exists; the email can be resent. Failing the request would
			// leave the candidate unable to retry registration for an address that is
			// now taken.
			h.log.ErrorContext(r.Context(), "verification email could not be sent",
				slog.String("user_id", res.UserID.String()), slog.Any("error", mErr))
		}
	}

	// Identical response whether or not the account was created, so this endpoint
	// cannot be used to discover which addresses are registered.
	httpx.JSON(w, r, http.StatusAccepted, map[string]any{
		"message": "if that address can be registered, a verification email is on its way",
	})
}

type loginRequest struct {
	Email    string `json:"email" validate:"required"`
	Password string `json:"password" validate:"required"`
}

func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
	var in loginRequest
	if err := httpx.Bind(r, &in); err != nil {
		httpx.Error(w, r, err)
		return
	}

	session, err := h.svc.Login(r.Context(), LoginInput{
		Email:     in.Email,
		Password:  in.Password,
		IP:        httpx.ClientIP(r, h.trustProxy),
		UserAgent: r.UserAgent(),
	})
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	h.setRefreshCookie(w, session)
	httpx.JSON(w, r, http.StatusOK, h.toSessionResponse(session))
}

func (h *Handler) refresh(w http.ResponseWriter, r *http.Request) {
	token := h.refreshTokenFromRequest(r)

	session, err := h.svc.Refresh(r.Context(), token,
		httpx.ClientIP(r, h.trustProxy), r.UserAgent())
	if err != nil {
		// Clear the cookie on any refresh failure, so a browser holding a dead token
		// stops retrying with it on every page load.
		h.clearRefreshCookie(w)
		httpx.Error(w, r, err)
		return
	}

	h.setRefreshCookie(w, session)
	httpx.JSON(w, r, http.StatusOK, h.toSessionResponse(session))
}

func (h *Handler) logout(w http.ResponseWriter, r *http.Request) {
	token := h.refreshTokenFromRequest(r)

	// The cookie is cleared regardless of the outcome: from the user's point of view,
	// signing out must always work.
	h.clearRefreshCookie(w)

	if err := h.svc.Logout(r.Context(), token,
		httpx.ClientIP(r, h.trustProxy), r.UserAgent()); err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.NoContent(w)
}

func (h *Handler) logoutAll(w http.ResponseWriter, r *http.Request) {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	userID, err := uuid.Parse(principal.UserID)
	if err != nil {
		httpx.Error(w, r, apperr.Internalf(err, "principal carries an unparseable user id"))
		return
	}

	n, err := h.svc.LogoutEverywhere(r.Context(), userID,
		httpx.ClientIP(r, h.trustProxy), r.UserAgent())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	h.clearRefreshCookie(w)
	httpx.JSON(w, r, http.StatusOK, map[string]any{"sessions_revoked": n})
}

type verifyEmailRequest struct {
	Token string `json:"token" validate:"required"`
}

func (h *Handler) verifyEmail(w http.ResponseWriter, r *http.Request) {
	var in verifyEmailRequest
	if err := httpx.Bind(r, &in); err != nil {
		httpx.Error(w, r, err)
		return
	}

	if err := h.svc.VerifyEmail(r.Context(), in.Token,
		httpx.ClientIP(r, h.trustProxy), r.UserAgent()); err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{"message": "your email address is verified"})
}

func (h *Handler) me(w http.ResponseWriter, r *http.Request) {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	userID, err := uuid.Parse(principal.UserID)
	if err != nil {
		httpx.Error(w, r, apperr.Internalf(err, "principal carries an unparseable user id"))
		return
	}

	user, profile, err := h.svc.CurrentUser(r.Context(), userID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	httpx.JSON(w, r, http.StatusOK, map[string]any{
		"user":    toUserResponse(user),
		"profile": toProfileResponse(profile),
		"plan":    principal.PlanSlug,
	})
}

// updateProfileRequest uses pointers throughout so an omitted field is distinguishable
// from one explicitly set to empty. Without that, a settings page that posts only its
// own section would blank everything else.
type updateProfileRequest struct {
	FullName          *string  `json:"full_name" validate:"omitempty,max=200"`
	Phone             *string  `json:"phone" validate:"omitempty,max=20"`
	City              *string  `json:"city" validate:"omitempty,max=120"`
	Country           *string  `json:"country" validate:"omitempty,len=2"`
	Timezone          *string  `json:"timezone" validate:"omitempty,max=64"`
	LinkedInURL       *string  `json:"linkedin_url" validate:"omitempty,max=500"`
	PortfolioURL      *string  `json:"portfolio_url" validate:"omitempty,max=500"`
	GitHubURL         *string  `json:"github_url" validate:"omitempty,max=500"`
	WorkAuthorisation *string  `json:"work_authorisation" validate:"omitempty,max=500"`
	SalaryMin         *float64 `json:"salary_min" validate:"omitempty,gte=0"`
	SalaryMax         *float64 `json:"salary_max" validate:"omitempty,gte=0"`
	SalaryCurrency    *string  `json:"salary_currency" validate:"omitempty,len=3"`
	SalaryPeriod      *string  `json:"salary_period" validate:"omitempty,oneof=hour day week month year"`
	NoticePeriodDays  *int     `json:"notice_period_days" validate:"omitempty,gte=0,lte=365"`
	OpenToRelocation  *bool    `json:"open_to_relocation"`
	Headline          *string  `json:"headline" validate:"omitempty,max=200"`
}

func (h *Handler) updateProfile(w http.ResponseWriter, r *http.Request) {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	userID, err := uuid.Parse(principal.UserID)
	if err != nil {
		httpx.Error(w, r, apperr.Internalf(err, "principal carries an unparseable user id"))
		return
	}

	var in updateProfileRequest
	if err := httpx.Bind(r, &in); err != nil {
		httpx.Error(w, r, err)
		return
	}

	profile, err := h.svc.UpdateProfile(r.Context(), userID, ProfileUpdate{
		FullName:          in.FullName,
		Phone:             in.Phone,
		City:              in.City,
		Country:           in.Country,
		Timezone:          in.Timezone,
		LinkedInURL:       in.LinkedInURL,
		PortfolioURL:      in.PortfolioURL,
		GitHubURL:         in.GitHubURL,
		WorkAuthorisation: in.WorkAuthorisation,
		SalaryMin:         in.SalaryMin,
		SalaryMax:         in.SalaryMax,
		SalaryCurrency:    in.SalaryCurrency,
		SalaryPeriod:      in.SalaryPeriod,
		NoticePeriodDays:  in.NoticePeriodDays,
		OpenToRelocation:  in.OpenToRelocation,
		Headline:          in.Headline,
	}, httpx.ClientIP(r, h.trustProxy), r.UserAgent())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	httpx.JSON(w, r, http.StatusOK, map[string]any{"profile": toProfileResponse(profile)})
}

type changePasswordRequest struct {
	CurrentPassword string `json:"current_password" validate:"required"`
	NewPassword     string `json:"new_password" validate:"required"`
}

func (h *Handler) changePassword(w http.ResponseWriter, r *http.Request) {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	userID, err := uuid.Parse(principal.UserID)
	if err != nil {
		httpx.Error(w, r, apperr.Internalf(err, "principal carries an unparseable user id"))
		return
	}

	var in changePasswordRequest
	if err := httpx.Bind(r, &in); err != nil {
		httpx.Error(w, r, err)
		return
	}

	if err := h.svc.ChangePassword(r.Context(), userID, in.CurrentPassword, in.NewPassword,
		httpx.ClientIP(r, h.trustProxy), r.UserAgent()); err != nil {
		httpx.Error(w, r, err)
		return
	}

	// Every session was revoked, including this one, so the cookie must go too.
	h.clearRefreshCookie(w)
	httpx.JSON(w, r, http.StatusOK, map[string]any{
		"message": "your password was changed and all sessions were signed out",
	})
}

// jwks publishes the verification key so the frontend and extension can validate
// access tokens without calling the API.
func (h *Handler) jwks(w http.ResponseWriter, r *http.Request) {
	key := h.svc.Tokens().PublicKey()

	httpx.JSON(w, r, http.StatusOK, map[string]any{
		"keys": []map[string]any{{
			"kty": "RSA",
			"use": "sig",
			"alg": "RS256",
			"kid": h.svc.Tokens().KeyID(),
			"n":   base64URLUint(key.N.Bytes()),
			"e":   base64URLUint(bigEndianExponent(key.E)),
		}},
	})
}

// --- cookies ----------------------------------------------------------------

// refreshTokenFromRequest reads the refresh token from the cookie, falling back to the
// request body for non-browser clients (the CLI and the extension), which have no
// cookie jar.
func (h *Handler) refreshTokenFromRequest(r *http.Request) string {
	if c, err := r.Cookie(refreshCookieName); err == nil && c.Value != "" {
		return c.Value
	}
	var body struct {
		RefreshToken string `json:"refresh_token"`
	}
	// A missing or malformed body is not an error here: the absent-token case is
	// handled by the service, which treats it as "no session".
	_ = httpx.Bind(r, &body)
	return body.RefreshToken
}

func (h *Handler) setRefreshCookie(w http.ResponseWriter, session *Session) {
	http.SetCookie(w, &http.Cookie{
		Name:    refreshCookieName,
		Value:   session.RefreshToken,
		Path:    refreshCookiePath,
		Domain:  h.cfg.CookieDomain,
		Expires: session.RefreshExpiresAt,
		MaxAge:  int(time.Until(session.RefreshExpiresAt).Seconds()),
		// HttpOnly is the point of using a cookie at all: script must not be able to
		// read a refresh token, or any XSS becomes persistent account access.
		HttpOnly: true,
		Secure:   h.cfg.CookieSecure,
		// Lax rather than Strict: Strict would drop the cookie on a cross-site
		// navigation back into the app (following a verification link, returning from
		// an OAuth provider), silently signing the user out. Lax still blocks the
		// cross-site POSTs that CSRF relies on.
		SameSite: http.SameSiteLaxMode,
	})
}

func (h *Handler) clearRefreshCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     refreshCookieName,
		Value:    "",
		Path:     refreshCookiePath,
		Domain:   h.cfg.CookieDomain,
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   h.cfg.CookieSecure,
		SameSite: http.SameSiteLaxMode,
	})
}

func (h *Handler) toSessionResponse(s *Session) sessionResponse {
	return sessionResponse{
		AccessToken: s.AccessToken,
		TokenType:   "Bearer",
		ExpiresIn:   int(time.Until(s.AccessExpiresAt).Seconds()),
		ExpiresAt:   s.AccessExpiresAt,
		User:        toUserResponse(s.User),
		Profile:     toProfileResponse(s.Profile),
	}
}
