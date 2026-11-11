package identity_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/Joakim-animate90/myjobfyt/backend/internal/apperr"
	"github.com/Joakim-animate90/myjobfyt/backend/internal/config"
	"github.com/Joakim-animate90/myjobfyt/backend/internal/domain/identity"
)

// testAuthConfig uses the lowest parameters config validation permits, so the test
// suite stays fast while still exercising the real algorithm.
func testAuthConfig() config.AuthConfig {
	return config.AuthConfig{
		Argon2Memory:      19456,
		Argon2Iterations:  2,
		Argon2Parallelism: 1,
		Argon2SaltLength:  16,
		Argon2KeyLength:   32,
	}
}

func TestHashAndVerifyRoundTrip(t *testing.T) {
	h := identity.NewPasswordHasher(testAuthConfig())
	const password = "correct-horse-battery-staple"

	encoded, err := h.Hash(password)
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if err := h.Verify(password, encoded); err != nil {
		t.Fatalf("Verify with the correct password: %v", err)
	}
}

func TestVerifyRejectsAWrongPassword(t *testing.T) {
	h := identity.NewPasswordHasher(testAuthConfig())
	encoded, err := h.Hash("correct-horse-battery-staple")
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}

	for _, wrong := range []string{
		"correct-horse-battery-stapl",  // one character short
		"correct-horse-battery-staplf", // one character different
		"Correct-Horse-Battery-Staple", // case differs
		"",
	} {
		if err := h.Verify(wrong, encoded); !errors.Is(err, identity.ErrMismatchedPassword) {
			t.Errorf("Verify(%q) = %v, want ErrMismatchedPassword", wrong, err)
		}
	}
}

func TestHashIsSaltedSoIdenticalPasswordsDiffer(t *testing.T) {
	// Without a per-hash salt, a database disclosure reveals which accounts share a
	// password, and one cracked hash cracks them all.
	h := identity.NewPasswordHasher(testAuthConfig())
	const password = "correct-horse-battery-staple"

	first, err := h.Hash(password)
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	second, err := h.Hash(password)
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}

	if first == second {
		t.Fatal("two hashes of the same password are identical; the salt is not random")
	}
	// Both must still verify.
	if err := h.Verify(password, first); err != nil {
		t.Errorf("first hash does not verify: %v", err)
	}
	if err := h.Verify(password, second); err != nil {
		t.Errorf("second hash does not verify: %v", err)
	}
}

func TestEncodedHashCarriesItsParameters(t *testing.T) {
	// Self-describing hashes are what make a cost increase possible without
	// invalidating every existing password.
	h := identity.NewPasswordHasher(testAuthConfig())
	encoded, err := h.Hash("correct-horse-battery-staple")
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}

	if !strings.HasPrefix(encoded, "$argon2id$") {
		t.Fatalf("hash does not declare argon2id: %q", encoded)
	}
	if !strings.Contains(encoded, "m=19456,t=2,p=1") {
		t.Fatalf("hash does not carry its parameters: %q", encoded)
	}
	if n := len(strings.Split(encoded, "$")); n != 6 {
		t.Fatalf("hash has %d segments, want the 6 of PHC format: %q", n, encoded)
	}
}

func TestVerifyRejectsMalformedHashes(t *testing.T) {
	h := identity.NewPasswordHasher(testAuthConfig())

	cases := map[string]string{
		"empty":                "",
		"not a hash":           "hunter2",
		"too few segments":     "$argon2id$v=19$m=19456,t=2,p=1$c2FsdA",
		"wrong algorithm":      "$bcrypt$v=19$m=19456,t=2,p=1$c2FsdA$a2V5",
		"unsupported version":  "$argon2id$v=13$m=19456,t=2,p=1$c2FsdA$a2V5",
		"malformed parameters": "$argon2id$v=19$memory=lots$c2FsdA$a2V5",
		"bad base64 salt":      "$argon2id$v=19$m=19456,t=2,p=1$!!!!$a2V5",
		"bad base64 key":       "$argon2id$v=19$m=19456,t=2,p=1$c2FsdA$!!!!",
	}
	for name, encoded := range cases {
		t.Run(name, func(t *testing.T) {
			err := h.Verify("anything", encoded)
			if err == nil {
				t.Fatal("Verify accepted a malformed hash")
			}
			// It must be a decode failure, not a silent mismatch: these are different
			// problems and conflating them hides a corrupt database.
			if errors.Is(err, identity.ErrMismatchedPassword) {
				t.Fatalf("malformed hash reported as a password mismatch: %v", err)
			}
		})
	}
}

func TestNeedsRehashDetectsWeakerStoredParameters(t *testing.T) {
	weak := identity.NewPasswordHasher(testAuthConfig())

	strongCfg := testAuthConfig()
	strongCfg.Argon2Memory = 65536
	strongCfg.Argon2Iterations = 3
	strongCfg.Argon2Parallelism = 2
	strong := identity.NewPasswordHasher(strongCfg)

	weakHash, err := weak.Hash("correct-horse-battery-staple")
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	strongHash, err := strong.Hash("correct-horse-battery-staple")
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}

	if !strong.NeedsRehash(weakHash) {
		t.Error("a hash made with weaker parameters was not flagged for rehash")
	}
	if strong.NeedsRehash(strongHash) {
		t.Error("a hash made with current parameters was flagged for rehash")
	}
	// Stronger-than-current is not a reason to downgrade.
	if weak.NeedsRehash(strongHash) {
		t.Error("a stronger hash was flagged for rehash; it would be downgraded")
	}
	// An undecodable hash cannot be verified, so flagging it is the safe answer.
	if !strong.NeedsRehash("garbage") {
		t.Error("an undecodable hash was not flagged for rehash")
	}
}

func TestCrossVerificationWorksAcrossParameterChanges(t *testing.T) {
	// The practical requirement behind self-describing hashes: a password hashed under
	// the old cost must keep working after the cost is raised.
	weak := identity.NewPasswordHasher(testAuthConfig())
	strongCfg := testAuthConfig()
	strongCfg.Argon2Memory = 65536
	strongCfg.Argon2Iterations = 3
	strong := identity.NewPasswordHasher(strongCfg)

	const password = "correct-horse-battery-staple"
	oldHash, err := weak.Hash(password)
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}

	if err := strong.Verify(password, oldHash); err != nil {
		t.Fatalf("the new hasher cannot verify an old hash: %v", err)
	}
}

func TestValidatePasswordAcceptsSensibleChoices(t *testing.T) {
	// Length, not composition. A long passphrase of lowercase words is a good password
	// and the policy must not reject it in favour of "P@ssw0rd!".
	for _, password := range []string{
		"correct horse battery staple",
		"my cat walks on the keyboard",
		"xK9#mPq2$vL8@nR4",
		"ανοιχτήπόρταμεκλειδί",
		"日本語のパスワードですとても長い",
	} {
		if err := identity.ValidatePassword(password); err != nil {
			t.Errorf("ValidatePassword(%q) = %v, want nil", password, err)
		}
	}
}

func TestValidatePasswordRejectsWeakChoices(t *testing.T) {
	cases := []struct {
		name     string
		password string
		wantCode string
	}{
		{"empty", "", "required"},
		{"too short", "short", "too_short"},
		{"one under the floor", strings.Repeat("a", identity.MinPasswordLength-1), "too_short"},
		{"too long", strings.Repeat("a", identity.MaxPasswordLength+1), "too_long"},
		{"single repeated character", strings.Repeat("a", identity.MinPasswordLength), "too_simple"},
		{"common password", "password1234", "too_common"},
		{"common password, mixed case", "Password123", "too_common"},
		{"control characters", "abcdefgh\x00ijkl", "invalid_characters"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := identity.ValidatePassword(tc.password)
			if err == nil {
				t.Fatalf("ValidatePassword(%q) = nil, want a %s failure", tc.password, tc.wantCode)
			}
			e := apperr.As(err)
			if e == nil {
				t.Fatalf("err = %v, want an *apperr.Error", err)
			}
			found := false
			for _, f := range e.Fields {
				if f.Code == tc.wantCode {
					found = true
				}
			}
			if !found {
				t.Fatalf("fields = %+v, want one with code %q", e.Fields, tc.wantCode)
			}
		})
	}
}

func TestValidatePasswordCountsRunesNotBytes(t *testing.T) {
	// Twelve multi-byte characters are twelve characters. Measuring bytes would accept
	// this and reject a twelve-character ASCII password inconsistently.
	twelveRunes := "日本語日本語日本語日本語" // 12 runes, 36 bytes
	if got := len([]rune(twelveRunes)); got != 12 {
		t.Fatalf("test fixture has %d runes, want 12", got)
	}
	if err := identity.ValidatePassword(twelveRunes); err != nil {
		t.Fatalf("ValidatePassword on 12 multi-byte characters = %v, want nil", err)
	}

	elevenRunes := "日本語日本語日本語日本" // 11 runes
	if err := identity.ValidatePassword(elevenRunes); err == nil {
		t.Fatal("ValidatePassword accepted 11 characters; it counted bytes, not runes")
	}
}

func BenchmarkHash(b *testing.B) {
	h := identity.NewPasswordHasher(testAuthConfig())
	for i := 0; i < b.N; i++ {
		if _, err := h.Hash("correct-horse-battery-staple"); err != nil {
			b.Fatal(err)
		}
	}
}
