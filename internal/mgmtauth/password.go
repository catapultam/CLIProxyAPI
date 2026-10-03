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

	if err := validateArgon2Params(timeCost, memory, threads, len(salt), len(expected)); err != nil {
		return false, err
	}

	actual := argon2.IDKey([]byte(password), salt, timeCost, memory, threads, uint32(len(expected)))
	return subtle.ConstantTimeCompare(actual, expected) == 1, nil
}

// Bounds enforced by validateArgon2Params on a stored hash's parameters
// before they are ever passed to argon2.IDKey. A hash is persisted only by
// HashPassword, which always produces values well inside these bounds; this
// guards against a corrupted or maliciously edited sidecar file driving
// argon2 into a huge allocation (an attacker-controlled "m") or a panic
// (argon2 requires time>=1, parallelism>=1, and memory>=8*parallelism KiB).
const (
	minArgon2Time        = 1
	minArgon2Parallelism = 1
	maxArgon2MemoryKiB   = 1 << 20 // 1 GiB, expressed in KiB
	minArgon2KeyLen      = 16
	maxArgon2KeyLen      = 64
)

func validateArgon2Params(timeCost, memory uint32, threads uint8, saltLen, keyLen int) error {
	if timeCost < minArgon2Time {
		return fmt.Errorf("mgmtauth: invalid hash parameters: t must be >= %d", minArgon2Time)
	}
	if threads < minArgon2Parallelism {
		return fmt.Errorf("mgmtauth: invalid hash parameters: p must be >= %d", minArgon2Parallelism)
	}
	minMemory := uint64(8) * uint64(threads)
	if uint64(memory) < minMemory {
		return fmt.Errorf("mgmtauth: invalid hash parameters: m must be >= 8*p (%d)", minMemory)
	}
	if memory > maxArgon2MemoryKiB {
		return fmt.Errorf("mgmtauth: invalid hash parameters: m must be <= %d KiB", maxArgon2MemoryKiB)
	}
	if keyLen < minArgon2KeyLen || keyLen > maxArgon2KeyLen {
		return fmt.Errorf("mgmtauth: invalid hash parameters: key length must be between %d and %d bytes", minArgon2KeyLen, maxArgon2KeyLen)
	}
	if saltLen == 0 {
		return fmt.Errorf("mgmtauth: invalid hash parameters: salt must not be empty")
	}
	return nil
}
