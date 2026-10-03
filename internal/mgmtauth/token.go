package mgmtauth

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// TokenPrefix is prepended to every issued session token, e.g. "cpas_...".
const TokenPrefix = "cpas_"

// DefaultLifetime is the sliding session lifetime described by the
// management login design: 30 days.
const DefaultLifetime = 30 * 24 * time.Hour

// Method records how a session token was established.
type Method string

// Login methods a session token can carry. MethodKey is never minted into a
// token (management-key auth is checked per-request) but is used by callers
// as the GET session/status "method" value when the key itself authenticated
// the request.
const (
	MethodPassword Method = "password"
	MethodPasskey  Method = "passkey"
	MethodKey      Method = "key"
)

// ErrInvalidToken is returned for a malformed or incorrectly signed token.
var ErrInvalidToken = errors.New("mgmtauth: invalid session token")

// ErrTokenExpired is returned when a token's expiry has passed.
var ErrTokenExpired = errors.New("mgmtauth: session token expired")

// tokenClaims is the JSON payload signed inside a token.
type tokenClaims struct {
	IssuedAt  int64  `json:"iat"`
	ExpiresAt int64  `json:"exp"`
	Method    Method `json:"m"`
}

// Claims is the verified, decoded content of a session token.
type Claims struct {
	IssuedAt  time.Time
	ExpiresAt time.Time
	Method    Method
}

// IssueToken signs a new session token for the given method, valid for
// lifetime starting at now. It returns the token and its expiry.
func IssueToken(secret []byte, method Method, now time.Time, lifetime time.Duration) (string, time.Time, error) {
	if len(secret) == 0 {
		return "", time.Time{}, fmt.Errorf("mgmtauth: empty session secret")
	}
	expiresAt := now.Add(lifetime)
	raw, err := json.Marshal(tokenClaims{IssuedAt: now.Unix(), ExpiresAt: expiresAt.Unix(), Method: method})
	if err != nil {
		return "", time.Time{}, fmt.Errorf("mgmtauth: marshal claims: %w", err)
	}
	payloadB64 := base64.RawURLEncoding.EncodeToString(raw)
	sig := signPayload(secret, payloadB64)
	token := TokenPrefix + payloadB64 + "." + base64.RawURLEncoding.EncodeToString(sig)
	return token, expiresAt, nil
}

func signPayload(secret []byte, payloadB64 string) []byte {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(payloadB64))
	return mac.Sum(nil)
}

// VerifyToken checks the signature and expiry of a token minted by
// IssueToken. A token signed with a different secret (e.g. after sign-out-all
// or a password change rotates session-secret) fails signature verification.
func VerifyToken(secret []byte, token string, now time.Time) (Claims, error) {
	if len(secret) == 0 || !strings.HasPrefix(token, TokenPrefix) {
		return Claims{}, ErrInvalidToken
	}
	body := strings.TrimPrefix(token, TokenPrefix)
	dot := strings.LastIndex(body, ".")
	if dot < 0 {
		return Claims{}, ErrInvalidToken
	}
	payloadB64, sigB64 := body[:dot], body[dot+1:]

	sig, err := base64.RawURLEncoding.DecodeString(sigB64)
	if err != nil {
		return Claims{}, ErrInvalidToken
	}
	expected := signPayload(secret, payloadB64)
	if subtle.ConstantTimeCompare(sig, expected) != 1 {
		return Claims{}, ErrInvalidToken
	}

	raw, err := base64.RawURLEncoding.DecodeString(payloadB64)
	if err != nil {
		return Claims{}, ErrInvalidToken
	}
	var payload tokenClaims
	if err := json.Unmarshal(raw, &payload); err != nil {
		return Claims{}, ErrInvalidToken
	}

	claims := Claims{
		IssuedAt:  time.Unix(payload.IssuedAt, 0).UTC(),
		ExpiresAt: time.Unix(payload.ExpiresAt, 0).UTC(),
		Method:    payload.Method,
	}
	if !now.Before(claims.ExpiresAt) {
		return Claims{}, ErrTokenExpired
	}
	return claims, nil
}

// ShouldRefresh reports whether less than half of lifetime remains before
// claims expires, the sliding-refresh trigger from the management login
// design.
func ShouldRefresh(claims Claims, now time.Time, lifetime time.Duration) bool {
	remaining := claims.ExpiresAt.Sub(now)
	return remaining < lifetime/2
}
