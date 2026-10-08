package mgmtauth

import (
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"
)

func TestIssueAndVerifyToken(t *testing.T) {
	secret := []byte("super-secret-session-key")
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	token, expiresAt, err := IssueToken(secret, MethodPassword, now, DefaultLifetime, true)
	if err != nil {
		t.Fatalf("IssueToken: %v", err)
	}
	if expiresAt != now.Add(DefaultLifetime) {
		t.Fatalf("expiresAt = %v, want %v", expiresAt, now.Add(DefaultLifetime))
	}

	claims, err := VerifyToken(secret, token, now)
	if err != nil {
		t.Fatalf("VerifyToken: %v", err)
	}
	if claims.Method != MethodPassword {
		t.Fatalf("Method = %q, want %q", claims.Method, MethodPassword)
	}
	if !claims.Remember {
		t.Fatal("expected Remember to be true for a token issued with remember=true")
	}
	if !claims.IssuedAt.Equal(now) {
		t.Fatalf("IssuedAt = %v, want %v", claims.IssuedAt, now)
	}
	if !claims.ExpiresAt.Equal(expiresAt) {
		t.Fatalf("ExpiresAt = %v, want %v", claims.ExpiresAt, expiresAt)
	}
}

// TestIssueAndVerifyTokenNotRemembered exercises the remember=false path:
// the claim round-trips, and LifetimeFor picks BrowserSessionLifetime.
func TestIssueAndVerifyTokenNotRemembered(t *testing.T) {
	secret := []byte("super-secret-session-key")
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	token, expiresAt, err := IssueToken(secret, MethodPassword, now, LifetimeFor(false), false)
	if err != nil {
		t.Fatalf("IssueToken: %v", err)
	}
	if expiresAt != now.Add(BrowserSessionLifetime) {
		t.Fatalf("expiresAt = %v, want %v", expiresAt, now.Add(BrowserSessionLifetime))
	}

	claims, err := VerifyToken(secret, token, now)
	if err != nil {
		t.Fatalf("VerifyToken: %v", err)
	}
	if claims.Remember {
		t.Fatal("expected Remember to be false for a token issued with remember=false")
	}
}

// TestLegacyTokenWithoutEphemeralFieldIsRemembered hand-crafts a token with
// the pre-"remember me" payload shape (no "eph" field at all) and asserts
// it decodes as remembered. This is the compatibility guarantee: every
// token minted before this feature existed must keep behaving as a 30-day
// remembered session, and a round-trip through IssueToken(..., true) would
// produce a byte-identical payload to a token issued with remember=false in
// the degenerate case of the field being omitted either way -- so this test
// deliberately bypasses IssueToken and constructs the legacy JSON shape by
// hand to prove the decode path, not just the encode path.
func TestLegacyTokenWithoutEphemeralFieldIsRemembered(t *testing.T) {
	secret := []byte("super-secret-session-key")
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	expiresAt := now.Add(DefaultLifetime)

	legacyPayload := struct {
		IssuedAt  int64  `json:"iat"`
		ExpiresAt int64  `json:"exp"`
		Method    Method `json:"m"`
	}{IssuedAt: now.Unix(), ExpiresAt: expiresAt.Unix(), Method: MethodPassword}
	raw, err := json.Marshal(legacyPayload)
	if err != nil {
		t.Fatalf("marshal legacy payload: %v", err)
	}
	payloadB64 := base64.RawURLEncoding.EncodeToString(raw)
	sig := signPayload(secret, payloadB64)
	token := TokenPrefix + payloadB64 + "." + base64.RawURLEncoding.EncodeToString(sig)

	claims, err := VerifyToken(secret, token, now)
	if err != nil {
		t.Fatalf("VerifyToken: %v", err)
	}
	if !claims.Remember {
		t.Fatal("expected a legacy token without the ephemeral field to decode as remembered")
	}
	if claims.Method != MethodPassword {
		t.Fatalf("Method = %q, want %q", claims.Method, MethodPassword)
	}
}

func TestLifetimeFor(t *testing.T) {
	if got := LifetimeFor(true); got != DefaultLifetime {
		t.Fatalf("LifetimeFor(true) = %v, want %v", got, DefaultLifetime)
	}
	if got := LifetimeFor(false); got != BrowserSessionLifetime {
		t.Fatalf("LifetimeFor(false) = %v, want %v", got, BrowserSessionLifetime)
	}
}

func TestVerifyTokenWrongSecret(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	token, _, err := IssueToken([]byte("secret-a"), MethodPassword, now, DefaultLifetime, true)
	if err != nil {
		t.Fatalf("IssueToken: %v", err)
	}
	if _, err := VerifyToken([]byte("secret-b"), token, now); err != ErrInvalidToken {
		t.Fatalf("VerifyToken error = %v, want ErrInvalidToken", err)
	}
}

func TestVerifyTokenExpired(t *testing.T) {
	secret := []byte("super-secret-session-key")
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	token, expiresAt, err := IssueToken(secret, MethodPasskey, now, time.Hour, true)
	if err != nil {
		t.Fatalf("IssueToken: %v", err)
	}

	if _, err := VerifyToken(secret, token, expiresAt.Add(-time.Second)); err != nil {
		t.Fatalf("expected token to still be valid just before expiry, got %v", err)
	}
	if _, err := VerifyToken(secret, token, expiresAt.Add(time.Second)); err != ErrTokenExpired {
		t.Fatalf("VerifyToken error = %v, want ErrTokenExpired", err)
	}
}

func TestVerifyTokenMalformed(t *testing.T) {
	secret := []byte("super-secret-session-key")
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	cases := []string{
		"",
		"not-a-token",
		"cpas_missing-dot",
		"cpas_bad-base64!!.bad-sig!!",
	}
	for _, token := range cases {
		if _, err := VerifyToken(secret, token, now); err != ErrInvalidToken {
			t.Errorf("token %q: error = %v, want ErrInvalidToken", token, err)
		}
	}
}

func TestShouldRefresh(t *testing.T) {
	secret := []byte("super-secret-session-key")
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	lifetime := 30 * 24 * time.Hour
	token, _, err := IssueToken(secret, MethodPassword, now, lifetime, true)
	if err != nil {
		t.Fatalf("IssueToken: %v", err)
	}
	claims, err := VerifyToken(secret, token, now)
	if err != nil {
		t.Fatalf("VerifyToken: %v", err)
	}

	if ShouldRefresh(claims, now, lifetime) {
		t.Fatal("expected a freshly issued token to not need a refresh")
	}
	// Advance past the halfway point of the lifetime.
	halfway := now.Add(lifetime/2 + time.Minute)
	if !ShouldRefresh(claims, halfway, lifetime) {
		t.Fatal("expected a token past its halfway point to need a refresh")
	}
}

// TestSessionSecretRotationInvalidatesOldTokens exercises the "sign out all
// devices" / password-change scenario: rotating session-secret must make
// every token signed with the old secret fail verification.
func TestSessionSecretRotationInvalidatesOldTokens(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	oldSecret := []byte("old-secret")
	newSecret := []byte("new-secret")

	oldToken, _, err := IssueToken(oldSecret, MethodPassword, now, DefaultLifetime, true)
	if err != nil {
		t.Fatalf("IssueToken: %v", err)
	}

	if _, err := VerifyToken(oldSecret, oldToken, now); err != nil {
		t.Fatalf("expected old token to verify against old secret, got %v", err)
	}
	if _, err := VerifyToken(newSecret, oldToken, now); err != ErrInvalidToken {
		t.Fatalf("expected old token to be rejected after rotation, got %v", err)
	}

	newToken, _, err := IssueToken(newSecret, MethodPassword, now, DefaultLifetime, true)
	if err != nil {
		t.Fatalf("IssueToken: %v", err)
	}
	if _, err := VerifyToken(newSecret, newToken, now); err != nil {
		t.Fatalf("expected fresh token signed with new secret to verify, got %v", err)
	}
}
