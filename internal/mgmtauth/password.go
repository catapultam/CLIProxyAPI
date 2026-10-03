package mgmtauth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// argon2idPrefix identifies a hash produced by HashPassword, mirroring the
// bcrypt-prefix check config.looksLikeBcrypt already uses for secret-key.
const argon2idPrefix = "$argon2id$"

// Argon2id parameters. These match the example in the management login
// design spec and sit within OWASP's recommended range for an
// interactively-verified password.
const (
	argonTime    = 3
	argonMemory  = 64 * 1024
	argonThreads = 4
	argonKeyLen  = 32
	argonSaltLen = 16
)

// IsHashed reports whether s already looks like an argon2id hash produced by
// HashPassword, so a config loader can tell a stored hash apart from a
// plaintext value that still needs hashing.
func IsHashed(s string) bool {
	return strings.HasPrefix(s, argon2idPrefix)
}

// HashPassword hashes a plaintext password using argon2id with a fresh random
// salt. The result is self-describing: it embeds the parameters used so
// VerifyPassword needs no external configuration to check it later.
func HashPassword(password string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("mgmtauth: generate salt: %w", err)
	}
	hash := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s",
		argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(hash)), nil
}

// VerifyPassword reports whether password matches hash, a value produced by
// HashPassword. A malformed hash is reported as an error rather than a
// mismatch so callers can distinguish "wrong password" from "corrupt config".
func VerifyPassword(hash, password string) (bool, error) {
	parts := strings.Split(hash, "$")
	// Expected shape: ["", "argon2id", "v=19", "m=...,t=...,p=...", "<salt>", "<hash>"].
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false, fmt.Errorf("mgmtauth: unrecognized password hash format")
	}

	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return false, fmt.Errorf("mgmtauth: parse hash version: %w", err)
	}

	var memory, timeCost uint32
	var threads uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &timeCost, &threads); err != nil {
		return false, fmt.Errorf("mgmtauth: parse hash parameters: %w", err)
	}

	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false, fmt.Errorf("mgmtauth: decode hash salt: %w", err)
	}

	expected, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false, fmt.Errorf("mgmtauth: decode hash digest: %w", err)
	}

	actual := argon2.IDKey([]byte(password), salt, timeCost, memory, threads, uint32(len(expected)))
	return subtle.ConstantTimeCompare(actual, expected) == 1, nil
}
