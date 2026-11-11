package dataenc_test

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/Joakim-animate90/myjobfyt/backend/internal/platform/dataenc"
)

func testKey(t *testing.T, id string, fill byte) dataenc.Key {
	t.Helper()
	return dataenc.Key{ID: id, Material: bytes.Repeat([]byte{fill}, dataenc.KeyLength)}
}

func TestEncryptDecryptRoundTrip(t *testing.T) {
	c, err := dataenc.New([]dataenc.Key{testKey(t, "k1", 0x01)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	plaintext := []byte("+254700000000")
	ciphertext, keyID, err := c.Encrypt(plaintext)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if keyID != "k1" {
		t.Errorf("keyID = %q, want k1", keyID)
	}
	if bytes.Contains(ciphertext, plaintext) {
		t.Fatal("the plaintext appears verbatim inside the ciphertext")
	}

	got, err := c.Decrypt(ciphertext, keyID)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Fatalf("Decrypt = %q, want %q", got, plaintext)
	}
}

func TestEncryptUsesAFreshNonceEveryCall(t *testing.T) {
	// GCM nonce reuse under one key leaks the XOR of the two plaintexts and the
	// authentication key. This is the single most important property here.
	c, err := dataenc.New([]dataenc.Key{testKey(t, "k1", 0x01)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	plaintext := []byte("+254700000000")
	seen := make(map[string]struct{}, 200)
	for i := 0; i < 200; i++ {
		ciphertext, _, err := c.Encrypt(plaintext)
		if err != nil {
			t.Fatalf("Encrypt: %v", err)
		}
		// The nonce is the first 12 bytes for standard GCM.
		nonce := string(ciphertext[:12])
		if _, dup := seen[nonce]; dup {
			t.Fatalf("nonce reused at iteration %d", i)
		}
		seen[nonce] = struct{}{}
	}
}

func TestDecryptRejectsTamperedCiphertext(t *testing.T) {
	c, err := dataenc.New([]dataenc.Key{testKey(t, "k1", 0x01)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ciphertext, keyID, err := c.Encrypt([]byte("+254700000000"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	// Flip a bit in the body. GCM must detect it.
	tampered := bytes.Clone(ciphertext)
	tampered[len(tampered)-1] ^= 0x01
	if _, err := c.Decrypt(tampered, keyID); err == nil {
		t.Fatal("tampered ciphertext was accepted")
	}

	// Flip a bit in the nonce.
	tamperedNonce := bytes.Clone(ciphertext)
	tamperedNonce[0] ^= 0x01
	if _, err := c.Decrypt(tamperedNonce, keyID); err == nil {
		t.Fatal("ciphertext with a tampered nonce was accepted")
	}
}

func TestDecryptRejectsASwappedKeyID(t *testing.T) {
	// The key id is bound in as additional authenticated data, so relabelling a stored
	// value to claim a different key must fail rather than silently produce garbage.
	c, err := dataenc.New([]dataenc.Key{
		testKey(t, "k2", 0x02),
		testKey(t, "k1", 0x01),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ciphertext, keyID, err := c.Encrypt([]byte("secret"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if keyID != "k2" {
		t.Fatalf("keyID = %q, want the primary k2", keyID)
	}

	if _, err := c.Decrypt(ciphertext, "k1"); err == nil {
		t.Fatal("decryption succeeded under the wrong key id")
	}
}

func TestDecryptWithARetiredKeyGivesAnActionableError(t *testing.T) {
	encrypting, err := dataenc.New([]dataenc.Key{testKey(t, "old", 0x01)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ciphertext, keyID, err := encrypting.Encrypt([]byte("secret"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	// The operator rotated and dropped the old key before re-encrypting its values.
	rotated, err := dataenc.New([]dataenc.Key{testKey(t, "new", 0x02)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, derr := rotated.Decrypt(ciphertext, keyID)
	if derr == nil {
		t.Fatal("decryption succeeded with the key removed")
	}
	// The message must name the missing key: this is a recoverable operational
	// mistake, and the fix is to put the key back.
	if !strings.Contains(derr.Error(), "old") {
		t.Fatalf("error %q does not name the missing key id", derr)
	}
}

func TestKeyringSupportsRotation(t *testing.T) {
	// The practical requirement: after rotation, old values still decrypt and new
	// values use the new key.
	old := testKey(t, "k1", 0x01)
	oldOnly, err := dataenc.New([]dataenc.Key{old})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	legacyCiphertext, legacyKeyID, err := oldOnly.Encrypt([]byte("written before rotation"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	rotated, err := dataenc.New([]dataenc.Key{testKey(t, "k2", 0x02), old})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	got, err := rotated.Decrypt(legacyCiphertext, legacyKeyID)
	if err != nil {
		t.Fatalf("legacy value does not decrypt after rotation: %v", err)
	}
	if string(got) != "written before rotation" {
		t.Fatalf("legacy plaintext = %q", got)
	}

	_, newKeyID, err := rotated.Encrypt([]byte("written after rotation"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if newKeyID != "k2" {
		t.Fatalf("new writes used key %q, want the new primary k2", newKeyID)
	}
}

func TestNewRejectsBadKeyrings(t *testing.T) {
	cases := map[string][]dataenc.Key{
		"empty keyring": {},
		"empty key id":  {{ID: "", Material: bytes.Repeat([]byte{1}, dataenc.KeyLength)}},
		"short key":     {{ID: "k1", Material: []byte("too short")}},
		"long key":      {{ID: "k1", Material: bytes.Repeat([]byte{1}, 64)}},
		"duplicate ids": {
			{ID: "k1", Material: bytes.Repeat([]byte{1}, dataenc.KeyLength)},
			{ID: "k1", Material: bytes.Repeat([]byte{2}, dataenc.KeyLength)},
		},
	}
	for name, keys := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := dataenc.New(keys); err == nil {
				t.Fatal("New accepted an invalid keyring")
			}
		})
	}
}

func TestDecryptRejectsTruncatedInput(t *testing.T) {
	c, err := dataenc.New([]dataenc.Key{testKey(t, "k1", 0x01)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, input := range [][]byte{nil, {}, []byte("short")} {
		if _, err := c.Decrypt(input, "k1"); err == nil {
			t.Fatalf("Decrypt accepted %d-byte input", len(input))
		}
	}
}

func TestParseKeys(t *testing.T) {
	material, err := dataenc.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	encoded := base64.StdEncoding.EncodeToString(material)

	keys, err := dataenc.ParseKeys([]string{"primary:" + encoded})
	if err != nil {
		t.Fatalf("ParseKeys: %v", err)
	}
	if len(keys) != 1 || keys[0].ID != "primary" {
		t.Fatalf("keys = %+v", keys)
	}
	if !bytes.Equal(keys[0].Material, material) {
		t.Fatal("decoded material does not match")
	}

	// And it must build a working cipher, which is the point.
	c, err := dataenc.New(keys)
	if err != nil {
		t.Fatalf("New from parsed keys: %v", err)
	}
	ct, id, err := c.Encrypt([]byte("x"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if _, err := c.Decrypt(ct, id); err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
}

func TestParseKeysRejectsMalformedEntries(t *testing.T) {
	for _, entry := range []string{"no-colon", ":missing-id", "id:", "id:not!base64!"} {
		if _, err := dataenc.ParseKeys([]string{entry}); err == nil {
			t.Errorf("ParseKeys(%q) succeeded, want an error", entry)
		}
	}
}

func TestGenerateKeyProducesDistinctFullLengthKeys(t *testing.T) {
	seen := make(map[string]struct{}, 50)
	for i := 0; i < 50; i++ {
		k, err := dataenc.GenerateKey()
		if err != nil {
			t.Fatalf("GenerateKey: %v", err)
		}
		if len(k) != dataenc.KeyLength {
			t.Fatalf("key is %d bytes, want %d", len(k), dataenc.KeyLength)
		}
		if _, dup := seen[string(k)]; dup {
			t.Fatalf("duplicate key at iteration %d", i)
		}
		seen[string(k)] = struct{}{}
	}
}
