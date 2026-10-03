package mgmtauth

import (
	"encoding/base64"
	"fmt"
	"testing"
)

// buildHash constructs a syntactically valid argon2id hash string with the
// given parameters, for testing VerifyPassword's bounds checks without
// needing HashPassword to produce an out-of-range value itself.
func buildHash(m, t2, p int, saltLen, keyLen int) string {
	salt := make([]byte, saltLen)
	key := make([]byte, keyLen)
	return fmt.Sprintf("$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s",
		m, t2, p,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key))
}

func TestHashPasswordRoundTrip(t *testing.T) {
	hash, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if !IsHashed(hash) {
		t.Fatalf("expected IsHashed(%q) to be true", hash)
	}
	ok, err := VerifyPassword(hash, "correct horse battery staple")
	if err != nil {
		t.Fatalf("VerifyPassword: %v", err)
	}
	if !ok {
		t.Fatal("expected correct password to verify")
	}
}

func TestVerifyPasswordWrongPassword(t *testing.T) {
	hash, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	ok, err := VerifyPassword(hash, "wrong password")
	if err != nil {
		t.Fatalf("VerifyPassword: %v", err)
	}
	if ok {
		t.Fatal("expected wrong password to fail verification")
	}
}

func TestHashPasswordUniqueSalt(t *testing.T) {
	h1, err := HashPassword("same password")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	h2, err := HashPassword("same password")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if h1 == h2 {
		t.Fatal("expected two hashes of the same password to differ (random salt)")
	}
}

func TestVerifyPasswordMalformedHash(t *testing.T) {
	if _, err := VerifyPassword("not-a-hash", "whatever"); err == nil {
		t.Fatal("expected an error for a malformed hash")
	}
}

func TestIsHashedRejectsPlaintext(t *testing.T) {
	if IsHashed("plaintext-password") {
		t.Fatal("expected plaintext password to not look hashed")
	}
}

// TestVerifyPasswordRejectsOutOfRangeParams covers S2: a corrupt or
// maliciously crafted stored hash with absurd argon2 parameters must be
// rejected with an error, never panic and never attempt a wildly expensive
// or huge allocation.
func TestVerifyPasswordRejectsOutOfRangeParams(t *testing.T) {
	cases := []struct {
		name string
		hash string
	}{
		{"zero time cost", buildHash(65536, 0, 4, 16, 32)},
		{"zero parallelism", buildHash(65536, 3, 0, 16, 32)},
		{"memory below 8*p KiB", buildHash(16, 3, 4, 16, 32)},
		{"memory above 1 GiB KiB", buildHash(1<<21, 3, 4, 16, 32)},
		{"key length too short", buildHash(65536, 3, 4, 16, 8)},
		{"key length too long", buildHash(65536, 3, 4, 16, 128)},
		{"empty salt", buildHash(65536, 3, 4, 0, 32)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ok, err := VerifyPassword(tc.hash, "whatever")
			if err == nil {
				t.Fatalf("VerifyPassword(%q) = (%v, nil), want an error", tc.hash, ok)
			}
			if ok {
				t.Fatalf("VerifyPassword(%q) = (true, %v), want ok=false on error", tc.hash, err)
			}
		})
	}
}

func TestVerifyPasswordAcceptsBoundaryParams(t *testing.T) {
	// m == 8*p (the minimum argon2 allows), t == 1, p == 1, key len 16 and
	// 64 (the inclusive bounds) must all be accepted as well-formed, even
	// though the salt/key content itself is zeroed garbage (so the
	// comparison just needs to run and fail cleanly, not error out).
	cases := []string{
		buildHash(8, 1, 1, 16, 16),
		buildHash(8, 1, 1, 16, 64),
	}
	for _, hash := range cases {
		ok, err := VerifyPassword(hash, "whatever")
		if err != nil {
			t.Fatalf("VerifyPassword(%q) unexpected error: %v", hash, err)
		}
		if ok {
			t.Fatalf("VerifyPassword(%q) = true, want false (zeroed digest never matches)", hash)
		}
	}
}
