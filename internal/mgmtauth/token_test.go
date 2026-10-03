package mgmtauth

import (
	"testing"
	"time"
)

func TestIssueAndVerifyToken(t *testing.T) {
	secret := []byte("super-secret-session-key")
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	token, expiresAt, err := IssueToken(secret, MethodPassword, now, DefaultLifetime)
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
	if !claims.IssuedAt.Equal(now) {
		t.Fatalf("IssuedAt = %v, want %v", claims.IssuedAt, now)
	}
	if !claims.ExpiresAt.Equal(expiresAt) {
		t.Fatalf("ExpiresAt = %v, want %v", claims.ExpiresAt, expiresAt)
	}
}

func TestVerifyTokenWrongSecret(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	token, _, err := IssueToken([]byte("secret-a"), MethodPassword, now, DefaultLifetime)
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
	token, expiresAt, err := IssueToken(secret, MethodPasskey, now, time.Hour)
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
	token, _, err := IssueToken(secret, MethodPassword, now, lifetime)
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

	oldToken, _, err := IssueToken(oldSecret, MethodPassword, now, DefaultLifetime)
	if err != nil {
		t.Fatalf("IssueToken: %v", err)
	}

	if _, err := VerifyToken(oldSecret, oldToken, now); err != nil {
		t.Fatalf("expected old token to verify against old secret, got %v", err)
	}
	if _, err := VerifyToken(newSecret, oldToken, now); err != ErrInvalidToken {
		t.Fatalf("expected old token to be rejected after rotation, got %v", err)
	}

	newToken, _, err := IssueToken(newSecret, MethodPassword, now, DefaultLifetime)
	if err != nil {
		t.Fatalf("IssueToken: %v", err)
	}
	if _, err := VerifyToken(newSecret, newToken, now); err != nil {
		t.Fatalf("expected fresh token signed with new secret to verify, got %v", err)
	}
}
