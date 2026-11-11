// Package identity owns users, authentication, sessions and candidate profiles.
//
// Every other domain depends on this one for the security principal, and nothing here
// depends on any other domain. Tenant scoping starts here: a User's id is the
// partition key for all candidate data in the platform.
package identity

import (
	"strings"
	"time"

	"github.com/google/uuid"
)

// Status mirrors the user_status database enum.
type Status string

const (
	StatusActive          Status = "active"
	StatusSuspended       Status = "suspended"
	StatusPendingDeletion Status = "pending_deletion"
	StatusDeleted         Status = "deleted"
)

// CanAuthenticate reports whether an account in this state may sign in.
//
// pending_deletion still can: the candidate has a 7-day grace period in which they must
// be able to sign in and cancel. Automation is halted separately, at the point of work,
// not by locking them out of their own account.
func (s Status) CanAuthenticate() bool {
	return s == StatusActive || s == StatusPendingDeletion
}

// CanRunAutomation reports whether the account may discover jobs or submit
// applications.
func (s Status) CanRunAutomation() bool { return s == StatusActive }

// User is the security principal and the root of all candidate-owned data.
type User struct {
	ID                  uuid.UUID
	Email               string
	PasswordHash        string // empty for OAuth-only accounts
	EmailVerifiedAt     *time.Time
	Status              Status
	Role                Role
	LastLoginAt         *time.Time
	FailedLoginCount    int
	LockedUntil         *time.Time
	DeletionRequestedAt *time.Time
	EraseAfter          *time.Time
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

// IsVerified reports whether the email address has been confirmed.
func (u *User) IsVerified() bool { return u.EmailVerifiedAt != nil }

// IsLocked reports whether the account is currently locked out after repeated failed
// sign-in attempts.
func (u *User) IsLocked(now time.Time) bool {
	return u.LockedUntil != nil && u.LockedUntil.After(now)
}

// HasPassword reports whether password sign-in is possible for this account.
func (u *User) HasPassword() bool { return u.PasswordHash != "" }

// Profile holds the candidate facts used to fill application forms and personalise
// outreach.
type Profile struct {
	UserID            uuid.UUID
	FullName          string
	PhoneLastFour     string // the only part of the phone ever returned to a client
	HasPhone          bool
	City              string
	Country           string
	Timezone          string
	LinkedInURL       string
	PortfolioURL      string
	GitHubURL         string
	WorkAuthorisation string
	SalaryMin         *float64
	SalaryMax         *float64
	SalaryCurrency    string
	SalaryPeriod      string
	NoticePeriodDays  *int
	OpenToRelocation  bool
	Headline          string
	CompletenessPct   int
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// FormAutofillReady reports whether the profile has what automated form submission
// needs.
//
// Phone and city are the fields the application-form channel requires. Checking it here
// lets the UI block enabling auto-form at the point of configuration, rather than
// letting every application fail later with a confusing "blocked" status.
func (p *Profile) FormAutofillReady() bool {
	return p.HasPhone && strings.TrimSpace(p.City) != ""
}

// profileWeights assigns each field its share of the completeness percentage.
//
// The weights reflect what the pipeline actually needs, not how a form looks: name,
// phone and city gate form auto-fill, so they carry most of the score. A profile at
// 100% is one that can use every submission channel.
var profileWeights = []struct {
	name   string
	weight int
	filled func(*Profile) bool
}{
	{"full_name", 20, func(p *Profile) bool { return strings.TrimSpace(p.FullName) != "" }},
	{"phone", 20, func(p *Profile) bool { return p.HasPhone }},
	{"city", 15, func(p *Profile) bool { return strings.TrimSpace(p.City) != "" }},
	{"country", 10, func(p *Profile) bool { return strings.TrimSpace(p.Country) != "" }},
	{"headline", 10, func(p *Profile) bool { return strings.TrimSpace(p.Headline) != "" }},
	{"linkedin", 10, func(p *Profile) bool { return strings.TrimSpace(p.LinkedInURL) != "" }},
	{"work_authorisation", 10, func(p *Profile) bool { return strings.TrimSpace(p.WorkAuthorisation) != "" }},
	{"salary", 5, func(p *Profile) bool { return p.SalaryMin != nil || p.SalaryMax != nil }},
}

// Completeness computes the profile completeness percentage.
func (p *Profile) Completeness() int {
	total := 0
	for _, f := range profileWeights {
		if f.filled(p) {
			total += f.weight
		}
	}
	if total > 100 {
		total = 100
	}
	return total
}

// MissingForCompleteness lists the unfilled fields, so the UI can tell the candidate
// exactly what to do next instead of showing a bare percentage.
func (p *Profile) MissingForCompleteness() []string {
	var missing []string
	for _, f := range profileWeights {
		if !f.filled(p) {
			missing = append(missing, f.name)
		}
	}
	return missing
}

// NormaliseEmail canonicalises an address for storage and comparison.
//
// Trim and lowercase only. Deliberately NOT stripping dots or +tags: those rules are
// Gmail-specific, and applying them universally would merge genuinely distinct
// addresses on providers that treat them as significant.
func NormaliseEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// Session is what a successful authentication returns.
type Session struct {
	AccessToken      string
	AccessExpiresAt  time.Time
	RefreshToken     string
	RefreshExpiresAt time.Time
	User             *User
	Profile          *Profile
}

// TokenPurpose mirrors the verification-token purpose column.
type TokenPurpose string

const (
	PurposeEmailVerification TokenPurpose = "email_verification"
	PurposePasswordReset     TokenPurpose = "password_reset"
	PurposeEmailChange       TokenPurpose = "email_change"
	PurposeAccountDeletion   TokenPurpose = "account_deletion"
)
