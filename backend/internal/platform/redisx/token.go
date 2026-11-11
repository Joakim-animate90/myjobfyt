package redisx

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
)

// randomToken returns a 128-bit random identifier used as a lock fencing token.
func randomToken() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate lock token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
