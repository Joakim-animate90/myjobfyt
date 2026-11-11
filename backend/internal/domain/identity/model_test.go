package identity_test

import (
	"testing"
	"time"

	"github.com/Joakim-animate90/myjobfyt/backend/internal/domain/identity"
)

func TestNormaliseEmail(t *testing.T) {
	cases := map[string]string{
		"  A@Example.COM  ": "a@example.com",
		"user@example.com":  "user@example.com",
		"USER@EXAMPLE.COM":  "user@example.com",
		// Dots and +tags are preserved: those rules are Gmail-specific, and applying
		// them universally would merge genuinely distinct addresses elsewhere.
		"first.last+jobs@example.com": "first.last+jobs@example.com",
	}
	for in, want := range cases {
		if got := identity.NormaliseEmail(in); got != want {
			t.Errorf("NormaliseEmail(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestStatusCanAuthenticate(t *testing.T) {
	// pending_deletion must still be able to sign in: the 7-day grace period is
	// pointless if the candidate is locked out of the account they want to recover.
	cases := map[identity.Status]bool{
		identity.StatusActive:          true,
		identity.StatusPendingDeletion: true,
		identity.StatusSuspended:       false,
		identity.StatusDeleted:         false,
	}
	for status, want := range cases {
		if got := status.CanAuthenticate(); got != want {
			t.Errorf("Status(%q).CanAuthenticate() = %v, want %v", status, got, want)
		}
	}
}

func TestStatusCanRunAutomation(t *testing.T) {
	// Automation stops the moment deletion is requested, even though sign-in continues.
	if identity.StatusPendingDeletion.CanRunAutomation() {
		t.Error("a pending-deletion account may still run automation; it must not")
	}
	if !identity.StatusActive.CanRunAutomation() {
		t.Error("an active account cannot run automation")
	}
}

func TestUserIsLocked(t *testing.T) {
	now := time.Now()
	past := now.Add(-time.Minute)
	future := now.Add(time.Minute)

	if (&identity.User{}).IsLocked(now) {
		t.Error("a user with no lock is reported as locked")
	}
	if (&identity.User{LockedUntil: &past}).IsLocked(now) {
		t.Error("an expired lock is still reported as locked")
	}
	if !(&identity.User{LockedUntil: &future}).IsLocked(now) {
		t.Error("an active lock is not reported as locked")
	}
}

func TestProfileFormAutofillReady(t *testing.T) {
	// These are the two fields the form channel needs. Checking them up front is what
	// lets the UI block the toggle instead of letting applications fail later.
	cases := []struct {
		name    string
		profile identity.Profile
		want    bool
	}{
		{"complete", identity.Profile{HasPhone: true, City: "Nairobi"}, true},
		{"no phone", identity.Profile{City: "Nairobi"}, false},
		{"no city", identity.Profile{HasPhone: true}, false},
		{"blank city", identity.Profile{HasPhone: true, City: "   "}, false},
		{"empty", identity.Profile{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.profile.FormAutofillReady(); got != tc.want {
				t.Fatalf("FormAutofillReady() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestProfileCompleteness(t *testing.T) {
	empty := identity.Profile{}
	if got := empty.Completeness(); got != 0 {
		t.Errorf("empty profile completeness = %d, want 0", got)
	}

	salary := 50000.0
	notice := 30
	full := identity.Profile{
		FullName:          "Candidate Name",
		HasPhone:          true,
		City:              "Nairobi",
		Country:           "KE",
		Headline:          "Senior Go Engineer",
		LinkedInURL:       "https://linkedin.com/in/example",
		WorkAuthorisation: "EU citizen",
		SalaryMin:         &salary,
		NoticePeriodDays:  &notice,
	}
	if got := full.Completeness(); got != 100 {
		t.Errorf("fully populated profile completeness = %d, want 100 (missing: %v)",
			got, full.MissingForCompleteness())
	}

	partial := identity.Profile{FullName: "Name", HasPhone: true, City: "Nairobi"}
	got := partial.Completeness()
	if got <= 0 || got >= 100 {
		t.Errorf("partial profile completeness = %d, want strictly between 0 and 100", got)
	}
}

func TestProfileCompletenessNeverExceeds100(t *testing.T) {
	salary := 1.0
	p := identity.Profile{
		FullName: "n", HasPhone: true, City: "c", Country: "KE", Headline: "h",
		LinkedInURL: "l", WorkAuthorisation: "w", SalaryMin: &salary, SalaryMax: &salary,
		PortfolioURL: "p", GitHubURL: "g", OpenToRelocation: true,
	}
	if got := p.Completeness(); got > 100 {
		t.Fatalf("completeness = %d, want at most 100 (the database CHECK would reject it)", got)
	}
}

func TestMissingForCompletenessNamesTheGaps(t *testing.T) {
	// A bare percentage tells the candidate nothing actionable; the field list does.
	p := identity.Profile{FullName: "Name"}
	missing := p.MissingForCompleteness()

	if len(missing) == 0 {
		t.Fatal("an almost-empty profile reported nothing missing")
	}
	for _, name := range missing {
		if name == "full_name" {
			t.Error("full_name is reported missing although it is set")
		}
	}

	found := false
	for _, name := range missing {
		if name == "phone" {
			found = true
		}
	}
	if !found {
		t.Errorf("phone is not reported missing: %v", missing)
	}
}

func TestUserHelpers(t *testing.T) {
	verified := time.Now()
	if !(&identity.User{EmailVerifiedAt: &verified}).IsVerified() {
		t.Error("IsVerified() = false for a verified user")
	}
	if (&identity.User{}).IsVerified() {
		t.Error("IsVerified() = true for an unverified user")
	}
	if (&identity.User{}).HasPassword() {
		t.Error("HasPassword() = true for an OAuth-only account")
	}
	if !(&identity.User{PasswordHash: "$argon2id$..."}).HasPassword() {
		t.Error("HasPassword() = false for a password account")
	}
}
