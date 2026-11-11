// Package dataenc provides authenticated column-level encryption for the small number
// of fields that must not be readable from a database dump: candidate phone numbers and
// user-supplied SMTP passwords.
//
// AES-256-GCM, so ciphertext is tamper-evident as well as confidential. Each value
// carries the id of the key that encrypted it, which is what makes key rotation
// possible without a flag day: new writes use the current key while old values stay
// readable until a background job re-encrypts them.
package dataenc

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
)

// KeyLength is the required key size in bytes (AES-256).
const KeyLength = 32

// Cipher encrypts and decrypts field values.
type Cipher interface {
	// Encrypt returns the ciphertext and the id of the key used.
	Encrypt(plaintext []byte) (ciphertext []byte, keyID string, err error)
	// Decrypt recovers a plaintext encrypted under keyID.
	Decrypt(ciphertext []byte, keyID string) ([]byte, error)
}

// AESCipher is the AES-256-GCM implementation with a keyring for rotation.
type AESCipher struct {
	primaryID string
	keys      map[string]cipher.AEAD
}

// Key is one entry in the keyring.
type Key struct {
	ID string
	// Material must be exactly KeyLength bytes.
	Material []byte
}

// ErrNoKeys means the cipher was constructed without any usable key.
var ErrNoKeys = errors.New("no encryption keys configured")

// New builds a cipher. The first key is the primary and is used for all new writes;
// the rest remain available for decrypting values written before the last rotation.
func New(keys []Key) (*AESCipher, error) {
	if len(keys) == 0 {
		return nil, ErrNoKeys
	}

	ring := make(map[string]cipher.AEAD, len(keys))
	for _, k := range keys {
		if k.ID == "" {
			return nil, errors.New("encryption key has an empty id")
		}
		if len(k.Material) != KeyLength {
			return nil, fmt.Errorf("encryption key %q is %d bytes, want %d", k.ID, len(k.Material), KeyLength)
		}
		block, err := aes.NewCipher(k.Material)
		if err != nil {
			return nil, fmt.Errorf("build cipher for key %q: %w", k.ID, err)
		}
		aead, err := cipher.NewGCM(block)
		if err != nil {
			return nil, fmt.Errorf("build GCM for key %q: %w", k.ID, err)
		}
		if _, exists := ring[k.ID]; exists {
			return nil, fmt.Errorf("duplicate encryption key id %q", k.ID)
		}
		ring[k.ID] = aead
	}

	return &AESCipher{primaryID: keys[0].ID, keys: ring}, nil
}

// PrimaryKeyID returns the id new writes are encrypted under.
func (c *AESCipher) PrimaryKeyID() string { return c.primaryID }

// Encrypt seals plaintext under the primary key.
//
// The nonce is random per call and prepended to the ciphertext. GCM nonce reuse under
// the same key is catastrophic — it leaks the XOR of two plaintexts and the
// authentication key — so it is generated from crypto/rand on every single call and
// never derived from anything else.
func (c *AESCipher) Encrypt(plaintext []byte) ([]byte, string, error) {
	aead, ok := c.keys[c.primaryID]
	if !ok {
		return nil, "", ErrNoKeys
	}

	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, "", fmt.Errorf("generate nonce: %w", err)
	}

	// Seal appends to the nonce slice, so the result is nonce||ciphertext||tag.
	sealed := aead.Seal(nonce, nonce, plaintext, []byte(c.primaryID))
	return sealed, c.primaryID, nil
}

// Decrypt opens a ciphertext written under keyID.
//
// The key id is passed as GCM's additional authenticated data, so a value cannot be
// moved between keys or have its recorded key id swapped without the authentication
// check failing.
func (c *AESCipher) Decrypt(ciphertext []byte, keyID string) ([]byte, error) {
	aead, ok := c.keys[keyID]
	if !ok {
		return nil, fmt.Errorf("no key with id %q; it may have been retired before its values were re-encrypted", keyID)
	}
	if len(ciphertext) < aead.NonceSize() {
		return nil, errors.New("ciphertext is shorter than the nonce")
	}

	nonce, sealed := ciphertext[:aead.NonceSize()], ciphertext[aead.NonceSize():]
	plaintext, err := aead.Open(nil, nonce, sealed, []byte(keyID))
	if err != nil {
		// Deliberately vague: distinguishing "wrong key" from "tampered ciphertext"
		// gives an attacker a decryption oracle.
		return nil, errors.New("ciphertext failed authentication")
	}
	return plaintext, nil
}

// ParseKeys decodes a keyring from its configuration form: a list of
// "id:base64-material" entries, primary first.
func ParseKeys(entries []string) ([]Key, error) {
	keys := make([]Key, 0, len(entries))
	for i, entry := range entries {
		id, encoded, found := splitOnce(entry, ':')
		if !found || id == "" || encoded == "" {
			return nil, fmt.Errorf("encryption key %d is not in id:base64 form", i)
		}
		material, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			material, err = base64.RawURLEncoding.DecodeString(encoded)
			if err != nil {
				return nil, fmt.Errorf("encryption key %q is not valid base64: %w", id, err)
			}
		}
		keys = append(keys, Key{ID: id, Material: material})
	}
	return keys, nil
}

func splitOnce(s string, sep byte) (string, string, bool) {
	for i := 0; i < len(s); i++ {
		if s[i] == sep {
			return s[:i], s[i+1:], true
		}
	}
	return s, "", false
}

// GenerateKey produces a new random key, for the key-generation command.
func GenerateKey() ([]byte, error) {
	k := make([]byte, KeyLength)
	if _, err := rand.Read(k); err != nil {
		return nil, fmt.Errorf("generate encryption key: %w", err)
	}
	return k, nil
}
