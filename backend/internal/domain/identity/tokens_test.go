package identity_test

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"strings"
	"testing"
	"time"

	"github.com/Joakim-animate90/myjobfyt/backend/internal/apperr"
	"github.com/Joakim-animate90/myjobfyt/backend/internal/config"
	"github.com/Joakim-animate90/myjobfyt/backend/internal/domain/identity"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

func issuerConfig() config.AuthConfig {
	cfg := testAuthConfig()
	cfg.JWTKeyID = "test-key"
	cfg.Issuer = "myjobfyt"
	cfg.Audience = identity.AudienceAPI
	cfg.AccessTokenTTL = 15 * time.Minute
	cfg.RefreshTokenTTL = 720 * time.Hour
	return cfg
}

func newIssuer(t *testing.T) *identity.TokenIssuer {
	t.Helper()
	iss, err := identity.NewTokenIssuer(issuerConfig(), config.EnvTest)
	if err != nil {
		t.Fatalf("NewTokenIssuer: %v", err)
	}
	return iss
}

func TestIssueAndParseAccessToken(t *testing.T) {
	iss := newIssuer(t)
	userID := uuid.New()

	raw, expiresAt, err := iss.IssueAccessToken(userID, "a@example.com", identity.RoleCandidate, "pro")
	if err != nil {
		t.Fatalf("IssueAccessToken: %v", err)
	}
	if !expiresAt.After(time.Now()) {
		t.Fatalf("expiresAt = %v, want a future time", expiresAt)
	}

	principal, err := iss.ParseAccessToken(raw, identity.AudienceAPI)
	if err != nil {
		t.Fatalf("ParseAccessToken: %v", err)
	}
	if principal.UserID != userID {
		t.Errorf("UserID = %v, want %v", principal.UserID, userID)
	}
	if principal.Email != "a@example.com" {
		t.Errorf("Email = %q", principal.Email)
	}
	if principal.Role != identity.RoleCandidate {
		t.Errorf("Role = %q", principal.Role)
	}
	if principal.PlanSlug != "pro" {
		t.Errorf("PlanSlug = %q", principal.PlanSlug)
	}
	if principal.TokenID == "" {
		t.Error("TokenID is empty; per-token revocation would be impossible")
	}
}

func TestTokenHeaderCarriesTheKeyID(t *testing.T) {
	// Without kid in the header, rotating the signing key means a flag day.
	iss := newIssuer(t)
	raw, _, err := iss.IssueAccessToken(uuid.New(), "a@example.com", identity.RoleCandidate, "free")
	if err != nil {
		t.Fatalf("IssueAccessToken: %v", err)
	}

	segments := strings.Split(raw, ".")
	if len(segments) != 3 {
		t.Fatalf("token has %d segments, want 3", len(segments))
	}
	headerJSON, err := base64.RawURLEncoding.DecodeString(segments[0])
	if err != nil {
		t.Fatalf("decode header: %v", err)
	}
	var header map[string]any
	if err := json.Unmarshal(headerJSON, &header); err != nil {
		t.Fatalf("parse header: %v", err)
	}
	if header["kid"] != "test-key" {
		t.Errorf("header kid = %v, want test-key", header["kid"])
	}
	if header["alg"] != "RS256" {
		t.Errorf("header alg = %v, want RS256", header["alg"])
	}
}

func TestParseRejectsAlgNoneForgery(t *testing.T) {
	// The classic JWT attack: re-sign the payload with alg=none and the parser accepts
	// it as valid. Pinning the method is what prevents it.
	iss := newIssuer(t)

	claims := jwt.MapClaims{
		"sub":   uuid.NewString(),
		"iss":   "myjobfyt",
		"aud":   identity.AudienceAPI,
		"exp":   time.Now().Add(time.Hour).Unix(),
		"iat":   time.Now().Unix(),
		"role":  "admin",
		"email": "attacker@example.com",
	}
	unsigned := jwt.NewWithClaims(jwt.SigningMethodNone, claims)
	raw, err := unsigned.SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatalf("craft alg=none token: %v", err)
	}

	if _, err := iss.ParseAccessToken(raw, identity.AudienceAPI); err == nil {
		t.Fatal("an alg=none token was accepted")
	}
}

func TestParseRejectsAnHMACTokenSignedWithThePublicKey(t *testing.T) {
	// The other classic attack: because the RSA public key is published via JWKS, an
	// attacker can use it as an HMAC secret and forge a token that a parser which does
	// not pin the algorithm will happily verify.
	iss := newIssuer(t)

	pubDER, err := x509.MarshalPKIXPublicKey(iss.PublicKey())
	if err != nil {
		t.Fatalf("marshal public key: %v", err)
	}
	pubPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER})

	claims := jwt.MapClaims{
		"sub":   uuid.NewString(),
		"iss":   "myjobfyt",
		"aud":   identity.AudienceAPI,
		"exp":   time.Now().Add(time.Hour).Unix(),
		"role":  "admin",
		"email": "attacker@example.com",
	}
	forged := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	raw, err := forged.SignedString(pubPEM)
	if err != nil {
		t.Fatalf("craft HMAC token: %v", err)
	}

	if _, err := iss.ParseAccessToken(raw, identity.AudienceAPI); err == nil {
		t.Fatal("an HMAC token signed with the public key was accepted")
	}
}

func TestParseRejectsATokenSignedByADifferentKey(t *testing.T) {
	iss := newIssuer(t)

	otherKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	claims := jwt.RegisteredClaims{
		Subject:   uuid.NewString(),
		Issuer:    "myjobfyt",
		Audience:  jwt.ClaimStrings{identity.AudienceAPI},
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
	}
	raw, err := jwt.NewWithClaims(jwt.SigningMethodRS256, claims).SignedString(otherKey)
	if err != nil {
		t.Fatalf("sign with foreign key: %v", err)
	}

	if _, err := iss.ParseAccessToken(raw, identity.AudienceAPI); err == nil {
		t.Fatal("a token signed by an unknown key was accepted")
	}
}

func TestParseRejectsAnExpiredTokenWithADistinctCode(t *testing.T) {
	// The frontend refreshes on token_expired but signs the user out on
	// token_invalid, so these must be distinguishable.
	cfg := issuerConfig()
	cfg.AccessTokenTTL = -2 * time.Minute // already expired, beyond the 60s leeway
	iss, err := identity.NewTokenIssuer(cfg, config.EnvTest)
	if err != nil {
		t.Fatalf("NewTokenIssuer: %v", err)
	}

	raw, _, err := iss.IssueAccessToken(uuid.New(), "a@example.com", identity.RoleCandidate, "free")
	if err != nil {
		t.Fatalf("IssueAccessToken: %v", err)
	}

	_, perr := iss.ParseAccessToken(raw, identity.AudienceAPI)
	e := apperr.As(perr)
	if e == nil {
		t.Fatalf("err = %v, want an *apperr.Error", perr)
	}
	if e.Kind != apperr.KindUnauthenticated {
		t.Errorf("Kind = %q, want unauthenticated", e.Kind)
	}
	if e.Code != "token_expired" {
		t.Errorf("Code = %q, want token_expired", e.Code)
	}
}

func TestExpiryLeewayAbsorbsSmallClockSkew(t *testing.T) {
	// A token that expired 10 seconds ago must still work: otherwise NTP drift between
	// pods produces random 401s that are near-impossible to diagnose.
	cfg := issuerConfig()
	cfg.AccessTokenTTL = -10 * time.Second
	iss, err := identity.NewTokenIssuer(cfg, config.EnvTest)
	if err != nil {
		t.Fatalf("NewTokenIssuer: %v", err)
	}

	raw, _, err := iss.IssueAccessToken(uuid.New(), "a@example.com", identity.RoleCandidate, "free")
	if err != nil {
		t.Fatalf("IssueAccessToken: %v", err)
	}
	if _, err := iss.ParseAccessToken(raw, identity.AudienceAPI); err != nil {
		t.Fatalf("a token 10s past expiry was rejected despite the leeway: %v", err)
	}
}

func TestExtensionTokenIsNotValidAgainstTheMainAPI(t *testing.T) {
	// The extension runs alongside arbitrary web pages. Its credential must be useless
	// against the main API if it leaks.
	iss := newIssuer(t)
	userID := uuid.New()

	raw, _, err := iss.IssueExtensionToken(userID, "a@example.com", identity.RoleCandidate, "pro", 10*time.Minute)
	if err != nil {
		t.Fatalf("IssueExtensionToken: %v", err)
	}

	if _, err := iss.ParseAccessToken(raw, identity.AudienceExtension); err != nil {
		t.Fatalf("extension token rejected on its own audience: %v", err)
	}

	_, err = iss.ParseAccessToken(raw, identity.AudienceAPI)
	if err == nil {
		t.Fatal("an extension token was accepted against the main API audience")
	}
	if e := apperr.As(err); e == nil || e.Code != "token_wrong_audience" {
		t.Fatalf("err = %v, want token_wrong_audience", err)
	}
}

func TestExtensionTokenTTLIsCapped(t *testing.T) {
	iss := newIssuer(t)
	for _, requested := range []time.Duration{0, -time.Hour, 48 * time.Hour} {
		_, expiresAt, err := iss.IssueExtensionToken(uuid.New(), "a@example.com", identity.RoleCandidate, "free", requested)
		if err != nil {
			t.Fatalf("IssueExtensionToken(%v): %v", requested, err)
		}
		if ttl := time.Until(expiresAt); ttl > time.Hour {
			t.Errorf("requested %v produced a %v TTL; it must be capped", requested, ttl)
		}
	}
}

func TestParseRejectsAnUnknownRole(t *testing.T) {
	// A role the application does not recognise must not fall through as something
	// permissive.
	cfg := issuerConfig()
	iss, err := identity.NewTokenIssuer(cfg, config.EnvTest)
	if err != nil {
		t.Fatalf("NewTokenIssuer: %v", err)
	}

	raw, _, err := iss.IssueAccessToken(uuid.New(), "a@example.com", identity.Role("superuser"), "free")
	if err != nil {
		t.Fatalf("IssueAccessToken: %v", err)
	}
	if _, err := iss.ParseAccessToken(raw, identity.AudienceAPI); err == nil {
		t.Fatal("a token carrying an unknown role was accepted")
	}
}

func TestMismatchedKeypairIsRejectedAtStartup(t *testing.T) {
	// Signing with one key and verifying with another produces "invalid signature" on
	// every request, which is a baffling symptom. Fail at boot instead.
	privA, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	privB, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	privPEM := pem.EncodeToMemory(&pem.Block{
		Type: "PRIVATE KEY", Bytes: mustPKCS8(t, privA),
	})
	pubDER, err := x509.MarshalPKIXPublicKey(&privB.PublicKey)
	if err != nil {
		t.Fatalf("marshal public key: %v", err)
	}
	pubPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER})

	cfg := issuerConfig()
	cfg.JWTPrivateKeyPEM = string(privPEM)
	cfg.JWTPublicKeyPEM = string(pubPEM)

	if _, err := identity.NewTokenIssuer(cfg, config.EnvProduction); err == nil {
		t.Fatal("a mismatched keypair was accepted")
	}
}

func TestProductionRequiresAConfiguredKey(t *testing.T) {
	cfg := issuerConfig()
	cfg.JWTPrivateKeyPEM = ""
	if _, err := identity.NewTokenIssuer(cfg, config.EnvProduction); err == nil {
		t.Fatal("production accepted an ephemeral key; every restart would invalidate all sessions")
	}
	if _, err := identity.NewTokenIssuer(cfg, config.EnvDevelopment); err != nil {
		t.Fatalf("development should generate an ephemeral key: %v", err)
	}
}

func TestConfiguredKeypairRoundTrips(t *testing.T) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	privPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: mustPKCS8(t, priv)})
	pubDER, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		t.Fatalf("marshal public key: %v", err)
	}
	pubPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER})

	cfg := issuerConfig()
	cfg.JWTPrivateKeyPEM = string(privPEM)
	cfg.JWTPublicKeyPEM = string(pubPEM)

	iss, err := identity.NewTokenIssuer(cfg, config.EnvProduction)
	if err != nil {
		t.Fatalf("NewTokenIssuer: %v", err)
	}
	raw, _, err := iss.IssueAccessToken(uuid.New(), "a@example.com", identity.RoleAdmin, "premium")
	if err != nil {
		t.Fatalf("IssueAccessToken: %v", err)
	}
	p, err := iss.ParseAccessToken(raw, identity.AudienceAPI)
	if err != nil {
		t.Fatalf("ParseAccessToken: %v", err)
	}
	if !p.IsAdmin() {
		t.Error("IsAdmin() = false for an admin token")
	}
}

func mustPKCS8(t *testing.T, key *rsa.PrivateKey) []byte {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal pkcs8: %v", err)
	}
	return der
}

func TestNewRefreshTokenIsRandomAndHashed(t *testing.T) {
	seen := make(map[string]struct{}, 200)
	for i := 0; i < 200; i++ {
		rt, err := identity.NewRefreshToken(uuid.Nil, time.Now(), time.Hour)
		if err != nil {
			t.Fatalf("NewRefreshToken: %v", err)
		}
		if rt.Plaintext == "" {
			t.Fatal("plaintext is empty")
		}
		if _, dup := seen[rt.Plaintext]; dup {
			t.Fatalf("duplicate refresh token at iteration %d", i)
		}
		seen[rt.Plaintext] = struct{}{}

		// The stored value must be the hash, never the token itself: a database
		// disclosure must not hand over live sessions.
		if string(rt.Hash) == rt.Plaintext {
			t.Fatal("Hash equals the plaintext")
		}
		if len(rt.Hash) != 32 {
			t.Fatalf("Hash is %d bytes, want 32 (sha256)", len(rt.Hash))
		}
		if rt.FamilyID == uuid.Nil {
			t.Fatal("FamilyID was not assigned; theft detection needs a family")
		}
	}
}

func TestRefreshTokenRotationKeepsTheFamily(t *testing.T) {
	// Rotation must stay inside the family so that reuse of a superseded token can be
	// traced to the chain it belongs to and the whole family revoked.
	first, err := identity.NewRefreshToken(uuid.Nil, time.Now(), time.Hour)
	if err != nil {
		t.Fatalf("NewRefreshToken: %v", err)
	}
	second, err := identity.NewRefreshToken(first.FamilyID, time.Now(), time.Hour)
	if err != nil {
		t.Fatalf("NewRefreshToken: %v", err)
	}

	if second.FamilyID != first.FamilyID {
		t.Fatalf("rotation changed the family: %v -> %v", first.FamilyID, second.FamilyID)
	}
	if second.Plaintext == first.Plaintext {
		t.Fatal("rotation reused the same token value")
	}
}

func TestHashRefreshTokenIsDeterministic(t *testing.T) {
	// Lookup depends on this: the hash computed from a presented token must match the
	// one stored at issue time.
	rt, err := identity.NewRefreshToken(uuid.Nil, time.Now(), time.Hour)
	if err != nil {
		t.Fatalf("NewRefreshToken: %v", err)
	}
	recomputed := identity.HashRefreshToken(rt.Plaintext)
	if string(recomputed) != string(rt.Hash) {
		t.Fatal("HashRefreshToken is not deterministic; token lookup would always miss")
	}
	if string(identity.HashRefreshToken("different")) == string(rt.Hash) {
		t.Fatal("different inputs produced the same hash")
	}
}

func TestRoleValidRejectsUnknownValues(t *testing.T) {
	for _, r := range []identity.Role{identity.RoleCandidate, identity.RoleSupport, identity.RoleAdmin, identity.RoleSystem} {
		if !r.Valid() {
			t.Errorf("Role(%q).Valid() = false", r)
		}
	}
	for _, r := range []identity.Role{"", "superuser", "Admin", "root"} {
		if identity.Role(r).Valid() {
			t.Errorf("Role(%q).Valid() = true, want false", r)
		}
	}
}
