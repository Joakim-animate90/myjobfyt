package identity

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"time"

	"github.com/Joakim-animate90/myjobfyt/backend/internal/apperr"
	"github.com/Joakim-animate90/myjobfyt/backend/internal/config"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// Role mirrors the user_role database enum.
type Role string

const (
	RoleCandidate Role = "candidate"
	RoleSupport   Role = "support"
	RoleAdmin     Role = "admin"
	RoleSystem    Role = "system"
)

// Valid reports whether r is a known role. An unknown role must never be treated as
// permissive.
func (r Role) Valid() bool {
	switch r {
	case RoleCandidate, RoleSupport, RoleAdmin, RoleSystem:
		return true
	default:
		return false
	}
}

// Audience values scope a token to a surface. The browser extension gets a token that
// is useless against the main API, so a compromised extension cannot read a
// candidate's whole account.
const (
	AudienceAPI       = "myjobfyt-api"
	AudienceExtension = "myjobfyt-extension"
)

// Principal is the authenticated caller, as resolved from a validated access token.
type Principal struct {
	UserID   uuid.UUID
	Email    string
	Role     Role
	PlanSlug string
	TokenID  string
	Audience string
}

// IsAdmin reports whether the principal holds the admin role.
func (p Principal) IsAdmin() bool { return p.Role == RoleAdmin }

// Claims is the access-token payload.
type Claims struct {
	jwt.RegisteredClaims
	Email string `json:"email"`
	Role  Role   `json:"role"`
	Plan  string `json:"plan,omitempty"`
}

// TokenIssuer signs and verifies access tokens, and mints refresh tokens.
//
// RS256 rather than HS256 so the frontend and the browser extension can verify tokens
// from a published JWKS without ever holding a signing secret. With a shared HMAC
// secret, every verifier is also a forger.
type TokenIssuer struct {
	privateKey *rsa.PrivateKey
	publicKey  *rsa.PublicKey
	keyID      string
	issuer     string
	audience   string
	accessTTL  time.Duration
	refreshTTL time.Duration
}

// NewTokenIssuer parses the configured keypair and builds the issuer.
//
// In development, when no keypair is configured, an ephemeral one is generated so the
// platform runs with no setup. Config validation already makes this impossible in
// production, where an ephemeral key would invalidate every session on each restart.
func NewTokenIssuer(cfg config.AuthConfig, env config.Environment) (*TokenIssuer, error) {
	var (
		priv *rsa.PrivateKey
		pub  *rsa.PublicKey
		err  error
	)

	switch {
	case cfg.JWTPrivateKeyPEM != "":
		priv, err = parsePrivateKey(cfg.JWTPrivateKeyPEM)
		if err != nil {
			return nil, err
		}
		if cfg.JWTPublicKeyPEM != "" {
			pub, err = parsePublicKey(cfg.JWTPublicKeyPEM)
			if err != nil {
				return nil, err
			}
			// A mismatched pair would sign tokens nothing can verify, and the symptom
			// would be a baffling "invalid signature" on every request.
			if !pub.Equal(&priv.PublicKey) {
				return nil, errors.New("AUTH_JWT_PUBLIC_KEY_PEM does not match AUTH_JWT_PRIVATE_KEY_PEM")
			}
		} else {
			pub = &priv.PublicKey
		}

	case env.IsDevelopment():
		priv, err = rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			return nil, fmt.Errorf("generate ephemeral development key: %w", err)
		}
		pub = &priv.PublicKey

	default:
		return nil, errors.New("no JWT signing key configured")
	}

	return &TokenIssuer{
		privateKey: priv,
		publicKey:  pub,
		keyID:      cfg.JWTKeyID,
		issuer:     cfg.Issuer,
		audience:   cfg.Audience,
		accessTTL:  cfg.AccessTokenTTL,
		refreshTTL: cfg.RefreshTokenTTL,
	}, nil
}

func parsePrivateKey(pemStr string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(pemStr))
	if block == nil {
		return nil, errors.New("JWT private key is not valid PEM")
	}

	if key, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		rsaKey, ok := key.(*rsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("JWT private key is %T, want an RSA key", key)
		}
		return rsaKey, nil
	}

	// Older tooling emits PKCS#1; accept it rather than making the operator convert.
	key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse JWT private key: %w", err)
	}
	return key, nil
}

func parsePublicKey(pemStr string) (*rsa.PublicKey, error) {
	block, _ := pem.Decode([]byte(pemStr))
	if block == nil {
		return nil, errors.New("JWT public key is not valid PEM")
	}

	if key, err := x509.ParsePKIXPublicKey(block.Bytes); err == nil {
		rsaKey, ok := key.(*rsa.PublicKey)
		if !ok {
			return nil, fmt.Errorf("JWT public key is %T, want an RSA key", key)
		}
		return rsaKey, nil
	}

	key, err := x509.ParsePKCS1PublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse JWT public key: %w", err)
	}
	return key, nil
}

// IssueAccessToken signs a short-lived access token for the given principal.
func (t *TokenIssuer) IssueAccessToken(userID uuid.UUID, email string, role Role, plan string) (string, time.Time, error) {
	return t.issueFor(userID, email, role, plan, t.audience, t.accessTTL)
}

// IssueExtensionToken signs a token scoped to the browser-extension surface.
//
// Short-lived and narrowly scoped on purpose: the extension runs in a hostile
// environment (the candidate's browser, alongside arbitrary pages), so its credential
// must be worth as little as possible if it leaks.
func (t *TokenIssuer) IssueExtensionToken(userID uuid.UUID, email string, role Role, plan string, ttl time.Duration) (string, time.Time, error) {
	if ttl <= 0 || ttl > time.Hour {
		ttl = 15 * time.Minute
	}
	return t.issueFor(userID, email, role, plan, AudienceExtension, ttl)
}

func (t *TokenIssuer) issueFor(userID uuid.UUID, email string, role Role, plan, audience string, ttl time.Duration) (string, time.Time, error) {
	now := time.Now().UTC()
	expiresAt := now.Add(ttl)

	claims := Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   userID.String(),
			Issuer:    t.issuer,
			Audience:  jwt.ClaimStrings{audience},
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(expiresAt),
			// A unique jti makes individual-token revocation possible later without a
			// token-format change.
			ID: uuid.NewString(),
		},
		Email: email,
		Role:  role,
		Plan:  plan,
	}

	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	token.Header["kid"] = t.keyID

	signed, err := token.SignedString(t.privateKey)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("sign access token: %w", err)
	}
	return signed, expiresAt, nil
}

// ParseAccessToken verifies a token and returns the principal.
//
// The signing method is pinned to RS256. Without that, a token with alg=none or an
// HMAC token signed with the public key would be accepted — the two classic JWT
// forgeries, both of which this library's generic parser would otherwise allow.
func (t *TokenIssuer) ParseAccessToken(raw string, expectedAudience string) (Principal, error) {
	if expectedAudience == "" {
		expectedAudience = t.audience
	}

	parsed, err := jwt.ParseWithClaims(raw, &Claims{},
		func(token *jwt.Token) (any, error) {
			if token.Method.Alg() != jwt.SigningMethodRS256.Alg() {
				return nil, fmt.Errorf("unexpected signing method %q", token.Method.Alg())
			}
			return t.publicKey, nil
		},
		jwt.WithValidMethods([]string{jwt.SigningMethodRS256.Alg()}),
		jwt.WithIssuer(t.issuer),
		jwt.WithAudience(expectedAudience),
		// Small leeway for clock skew between the signer and the verifier; large enough
		// to absorb NTP drift, small enough that an expired token is not usefully
		// extended.
		jwt.WithLeeway(60*time.Second),
		jwt.WithExpirationRequired(),
	)
	if err != nil {
		switch {
		case errors.Is(err, jwt.ErrTokenExpired):
			return Principal{}, apperr.Unauthenticated("token_expired", "your session has expired")
		case errors.Is(err, jwt.ErrTokenNotValidYet):
			return Principal{}, apperr.Unauthenticated("token_not_yet_valid", "the token is not yet valid")
		case errors.Is(err, jwt.ErrTokenInvalidAudience):
			return Principal{}, apperr.Unauthenticated("token_wrong_audience", "this token is not valid for this API")
		default:
			return Principal{}, apperr.Unauthenticated("token_invalid", "the token is not valid").WithInternal(err)
		}
	}

	claims, ok := parsed.Claims.(*Claims)
	if !ok {
		return Principal{}, apperr.Unauthenticated("token_invalid", "the token is not valid")
	}

	userID, err := uuid.Parse(claims.Subject)
	if err != nil {
		return Principal{}, apperr.Unauthenticated("token_invalid", "the token subject is not a user id").WithInternal(err)
	}
	// An unrecognised role must not fall through as something permissive.
	if !claims.Role.Valid() {
		return Principal{}, apperr.Unauthenticated("token_invalid", "the token carries an unknown role")
	}

	return Principal{
		UserID:   userID,
		Email:    claims.Email,
		Role:     claims.Role,
		PlanSlug: claims.Plan,
		TokenID:  claims.ID,
		Audience: expectedAudience,
	}, nil
}

// PublicKey exposes the verification key for the JWKS endpoint.
func (t *TokenIssuer) PublicKey() *rsa.PublicKey { return t.publicKey }

// KeyID returns the key identifier published in the token header and in JWKS.
func (t *TokenIssuer) KeyID() string { return t.keyID }

// AccessTTL returns the access-token lifetime, for the login response.
func (t *TokenIssuer) AccessTTL() time.Duration { return t.accessTTL }

// RefreshTTL returns the refresh-token lifetime.
func (t *TokenIssuer) RefreshTTL() time.Duration { return t.refreshTTL }

// refreshTokenBytes is the entropy of a refresh token. 256 bits is far beyond
// brute-force, and these are opaque so there is no reason to economise.
const refreshTokenBytes = 32

// RefreshToken is a freshly minted refresh token: the plaintext goes to the client in
// an httpOnly cookie, the hash goes to the database.
type RefreshToken struct {
	Plaintext string
	Hash      []byte
	FamilyID  uuid.UUID
	ExpiresAt time.Time
}

// NewRefreshToken mints a refresh token for a rotation family.
//
// Passing uuid.Nil starts a new family, which happens on sign-in. Rotation passes the
// existing family so that reuse of a superseded token can be traced back and the whole
// family revoked.
//
// issuedAt is explicit rather than read from the wall clock. Expiry is security
// behaviour, and a function that reaches for time.Now() internally cannot be tested
// against an injected clock — the caller's notion of "now" and the token's would
// silently disagree.
func NewRefreshToken(familyID uuid.UUID, issuedAt time.Time, ttl time.Duration) (RefreshToken, error) {
	raw := make([]byte, refreshTokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return RefreshToken{}, fmt.Errorf("generate refresh token: %w", err)
	}

	if familyID == uuid.Nil {
		familyID = uuid.New()
	}

	plaintext := base64.RawURLEncoding.EncodeToString(raw)
	return RefreshToken{
		Plaintext: plaintext,
		Hash:      HashRefreshToken(plaintext),
		FamilyID:  familyID,
		ExpiresAt: issuedAt.UTC().Add(ttl),
	}, nil
}

// HashRefreshToken hashes a refresh token for storage and lookup.
//
// A plain SHA-256 rather than argon2: the token is 256 bits of uniform randomness, so
// there is no dictionary to attack and no benefit to a slow hash. It also has to be
// cheap because it runs on the lookup path of every token refresh.
func HashRefreshToken(plaintext string) []byte {
	sum := sha256.Sum256([]byte(plaintext))
	return sum[:]
}
