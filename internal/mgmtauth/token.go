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
// management login design: 30 days. It applies when the caller asked to be
// remembered (remember=true, which is also the default when the login
// request omits the field entirely).
const DefaultLifetime = 30 * 24 * time.Hour

// BrowserSessionLifetime is the sliding session lifetime for a caller that
// explicitly declined to be remembered (remember=false): a short-lived
// "browser session" that also drops its cookie's Max-Age, so it dies when
// the browser closes.
const BrowserSessionLifetime = 12 * time.Hour

// LifetimeFor returns the sliding-session lifetime a token with the given
// remember flag should use: DefaultLifetime when remembered, otherwise
// BrowserSessionLifetime. Callers that issue or refresh a token should
// derive the lifetime through this helper so issuance and refresh can never
// drift apart.
func LifetimeFor(remember bool) time.Duration {
	if remember {
		return DefaultLifetime
	}
	return BrowserSessionLifetime
}

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
//
// Ephemeral is deliberately inverted from the public "remember" concept and
// tagged omitempty: a zero value (false) is indistinguishable from an
// absent field, and every token minted before "remember me" existed has no
// field here at all. Encoding the *negative* ("this is a short-lived
// browser session") means that absent-or-false both mean "remembered",
// which is exactly the required compatibility behavior -- old tokens, and
// tokens for callers who never opted out, keep being treated as
// remembered. Do not flip this back to a directly-named "remember" bool
// without also dropping omitempty, or explicit false becomes unrepresentable.
type tokenClaims struct {
	IssuedAt  int64  `json:"iat"`
	ExpiresAt int64  `json:"exp"`
	Method    Method `json:"m"`
	Ephemeral bool   `json:"eph,omitempty"`
}

// Claims is the verified, decoded content of a session token.
type Claims struct {
	IssuedAt  time.Time
	ExpiresAt time.Time
	Method    Method
	// Remember is true when the session should behave as a persistent,
	// 30-day sliding session (the historical behavior, and the default for
	// any token -- old or new -- that doesn't carry the underlying
	// "ephemeral" claim). It is false only for a token explicitly issued
	// with remember=false, a short-lived "browser session".
	Remember bool
}

// IssueToken signs a new session token for the given method, valid for
// lifetime starting at now. remember records the caller's "remember me"
// choice in the token claims (see tokenClaims.Ephemeral); it does not
// itself pick the lifetime -- pass mgmtauth.LifetimeFor(remember), or a
// test-specific duration, as lifetime. It returns the token and its expiry.
func IssueToken(secret []byte, method Method, now time.Time, lifetime time.Duration, remember bool) (string, time.Time, error) {
	if len(secret) == 0 {
		return "", time.Time{}, fmt.Errorf("mgmtauth: empty session secret")
	}
	expiresAt := now.Add(lifetime)
	raw, err := json.Marshal(tokenClaims{IssuedAt: now.Unix(), ExpiresAt: expiresAt.Unix(), Method: method, Ephemeral: !remember})
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
		Remember:  !payload.Ephemeral,
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
