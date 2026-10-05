package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
)

// TokenBytes is the entropy of session, CSRF and pre-session tokens.
const TokenBytes = 32

// NewToken returns TokenBytes random bytes encoded as base64url.
func NewToken() (string, error) {
	b := make([]byte, TokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// HashToken returns the hex SHA-256 stored in the database instead of the token.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// TokensEqual compares two tokens in constant time.
func TokensEqual(a, b string) bool {
	return a != "" && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// PreSessionCSRF derives the login form CSRF token from the pre-session
// cookie value: HMAC-SHA256(key, "login:"+cookie). An attacker able to plant
// a cookie still cannot forge the matching form value without the key.
func PreSessionCSRF(key []byte, preSession string) string {
	m := hmac.New(sha256.New, key)
	m.Write([]byte("login:" + preSession))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// CheckPreSessionCSRF verifies a login form token.
func CheckPreSessionCSRF(key []byte, preSession, formToken string) bool {
	if preSession == "" || formToken == "" {
		return false
	}
	return TokensEqual(PreSessionCSRF(key, preSession), formToken)
}
