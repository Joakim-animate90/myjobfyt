package identity

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"unicode"

	"github.com/Joakim-animate90/myjobfyt/backend/internal/apperr"
	"github.com/Joakim-animate90/myjobfyt/backend/internal/config"
	"golang.org/x/crypto/argon2"
)

// PasswordHasher hashes and verifies passwords with argon2id.
//
// The encoded hash carries its own parameters, so raising the cost later does not
// invalidate existing hashes: NeedsRehash reports when a stored hash was produced with
// weaker settings, and the password can be transparently upgraded at the next
// successful sign-in.
type PasswordHasher struct {
	memory      uint32
	iterations  uint32
	parallelism uint8
	saltLength  uint32
	keyLength   uint32
}

// NewPasswordHasher builds a hasher from configuration. Config validation has already
// enforced the OWASP minimum parameters.
func NewPasswordHasher(cfg config.AuthConfig) *PasswordHasher {
	return &PasswordHasher{
		memory:      cfg.Argon2Memory,
		iterations:  cfg.Argon2Iterations,
		parallelism: cfg.Argon2Parallelism,
		saltLength:  cfg.Argon2SaltLength,
		keyLength:   cfg.Argon2KeyLength,
	}
}

// MinPasswordLength is the floor for a new password.
//
// Twelve characters with no composition rules, rather than eight with "one uppercase,
// one digit, one symbol". Composition rules reliably produce "Password1!" and push
// users toward reuse; length is the parameter that actually raises the cost of an
// offline attack. NIST SP 800-63B says the same.
const MinPasswordLength = 12

// MaxPasswordLength bounds the input to argon2.
//
// Not a security limit but a denial-of-service one: argon2 hashes its input, so a
// 10 MB "password" would cost real CPU per sign-in attempt.
const MaxPasswordLength = 128

// Hash produces an encoded argon2id hash in the standard PHC string format.
func (h *PasswordHasher) Hash(password string) (string, error) {
	salt := make([]byte, h.saltLength)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate password salt: %w", err)
	}

	key := argon2.IDKey([]byte(password), salt, h.iterations, h.memory, h.parallelism, h.keyLength)

	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version,
		h.memory, h.iterations, h.parallelism,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	), nil
}

// ErrMismatchedPassword means the password does not match the hash.
var ErrMismatchedPassword = errors.New("password does not match")

// Verify checks a password against an encoded hash.
//
// The comparison is constant-time. A timing-variable comparison here would leak the
// hash a byte at a time, which is slow but entirely practical against a public
// endpoint.
func (h *PasswordHasher) Verify(password, encodedHash string) error {
	params, salt, key, err := decodeHash(encodedHash)
	if err != nil {
		return err
	}

	candidate := argon2.IDKey([]byte(password), salt,
		params.iterations, params.memory, params.parallelism, uint32(len(key)))

	if subtle.ConstantTimeCompare(key, candidate) != 1 {
		return ErrMismatchedPassword
	}
	return nil
}

// NeedsRehash reports whether a stored hash used weaker parameters than current
// configuration, so it can be upgraded after a successful sign-in.
func (h *PasswordHasher) NeedsRehash(encodedHash string) bool {
	params, _, key, err := decodeHash(encodedHash)
	if err != nil {
		// An undecodable hash cannot be verified against anyway; flagging it for rehash
		// is the safe answer.
		return true
	}
	return params.memory < h.memory ||
		params.iterations < h.iterations ||
		params.parallelism < h.parallelism ||
		uint32(len(key)) < h.keyLength
}

type hashParams struct {
	memory      uint32
	iterations  uint32
	parallelism uint8
}

func decodeHash(encoded string) (hashParams, []byte, []byte, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 {
		return hashParams{}, nil, nil, fmt.Errorf("malformed password hash: expected 6 segments, got %d", len(parts))
	}
	if parts[1] != "argon2id" {
		return hashParams{}, nil, nil, fmt.Errorf("unsupported password hash algorithm %q", parts[1])
	}

	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return hashParams{}, nil, nil, fmt.Errorf("malformed argon2 version: %w", err)
	}
	if version != argon2.Version {
		return hashParams{}, nil, nil, fmt.Errorf("unsupported argon2 version %d", version)
	}

	var p hashParams
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.memory, &p.iterations, &p.parallelism); err != nil {
		return hashParams{}, nil, nil, fmt.Errorf("malformed argon2 parameters: %w", err)
	}

	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return hashParams{}, nil, nil, fmt.Errorf("malformed argon2 salt: %w", err)
	}
	key, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return hashParams{}, nil, nil, fmt.Errorf("malformed argon2 key: %w", err)
	}

	return p, salt, key, nil
}

// ValidatePassword checks a new password against the platform's policy.
//
// The policy is length plus a rejection list of the obvious, not a composition
// checklist. Returning every problem at once rather than the first lets the UI show a
// complete picture instead of making the user guess repeatedly.
func ValidatePassword(password string) error {
	var problems []apperr.FieldError

	// Count runes, not bytes: a 12-character password in a non-Latin script would
	// otherwise be judged by its UTF-8 length and accepted or rejected arbitrarily.
	length := len([]rune(password))

	switch {
	case length == 0:
		problems = append(problems, apperr.FieldError{
			Field: "password", Code: "required", Message: "a password is required",
		})
	case length < MinPasswordLength:
		problems = append(problems, apperr.FieldError{
			Field:   "password",
			Code:    "too_short",
			Message: fmt.Sprintf("must be at least %d characters", MinPasswordLength),
		})
	case length > MaxPasswordLength:
		problems = append(problems, apperr.FieldError{
			Field:   "password",
			Code:    "too_long",
			Message: fmt.Sprintf("must be at most %d characters", MaxPasswordLength),
		})
	}

	if length > 0 && isAllSameRune(password) {
		problems = append(problems, apperr.FieldError{
			Field: "password", Code: "too_simple", Message: "must not be a single repeated character",
		})
	}

	if isCommonPassword(password) {
		problems = append(problems, apperr.FieldError{
			Field:   "password",
			Code:    "too_common",
			Message: "this password appears in lists of commonly used passwords",
		})
	}

	// Control characters are almost always a paste accident or an injection attempt,
	// and they survive badly through clients.
	for _, r := range password {
		if unicode.IsControl(r) {
			problems = append(problems, apperr.FieldError{
				Field: "password", Code: "invalid_characters", Message: "must not contain control characters",
			})
			break
		}
	}

	if len(problems) > 0 {
		return apperr.ValidationFields(problems)
	}
	return nil
}

func isAllSameRune(s string) bool {
	runes := []rune(s)
	if len(runes) < 2 {
		return true
	}
	for _, r := range runes[1:] {
		if r != runes[0] {
			return false
		}
	}
	return true
}

// commonPasswords is a deliberately small list of the passwords that dominate every
// breach corpus, long enough to pass the length check.
//
// A real deployment should check a full corpus (a k-anonymity range query against Have
// I Been Pwned, or a local bloom filter of the top million). This list is the floor,
// not the ceiling, and is documented as such in docs/SECURITY.md.
var commonPasswords = map[string]struct{}{
	"password":        {},
	"password1":       {},
	"password123":     {},
	"password1234":    {},
	"passw0rd123":     {},
	"123456789012":    {},
	"1234567890123":   {},
	"qwertyuiop123":   {},
	"administrator":   {},
	"letmein12345":    {},
	"welcome12345":    {},
	"iloveyou1234":    {},
	"myjobfyt1234":    {},
	"changeme1234":    {},
	"qwerty12345":     {},
	"abc123456789":    {},
	"football1234":    {},
	"monkey123456":    {},
	"dragon123456":    {},
	"sunshine1234":    {},
	"princess1234":    {},
	"trustno11234":    {},
	"baseball1234":    {},
	"superman1234":    {},
	"starwars1234":    {},
	"whatever1234":    {},
	"qazwsxedc123":    {},
	"zxcvbnm12345":    {},
	"asdfghjkl123":    {},
	"p@ssw0rd1234":    {},
	"correcthorse":    {},
	"thisisapassword": {},
}

func isCommonPassword(password string) bool {
	_, found := commonPasswords[strings.ToLower(strings.TrimSpace(password))]
	return found
}
