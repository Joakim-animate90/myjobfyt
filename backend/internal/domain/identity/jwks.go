package identity

import "encoding/base64"

// base64URLUint encodes a big-endian unsigned integer as base64url without padding,
// which is the JWK representation for RSA modulus and exponent values.
func base64URLUint(b []byte) string {
	// Leading zero bytes are not part of the value and some verifiers reject them.
	i := 0
	for i < len(b)-1 && b[i] == 0 {
		i++
	}
	return base64.RawURLEncoding.EncodeToString(b[i:])
}

// bigEndianExponent renders the RSA public exponent as a minimal big-endian byte slice.
func bigEndianExponent(e int) []byte {
	if e <= 0 {
		return []byte{0}
	}
	var buf []byte
	for v := e; v > 0; v >>= 8 {
		buf = append([]byte{byte(v & 0xff)}, buf...)
	}
	return buf
}
