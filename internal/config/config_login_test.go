package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/mgmtauth"
)

func TestLoginPasswordHashedOnLoadLegacyPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	body := "remote-management:\n  secret-key: test-key\n  login:\n    username: admin\n    password: super-secret-password\n"
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.RemoteManagement.Login.Password != "" {
		t.Fatalf("expected plaintext password to be cleared, got %q", cfg.RemoteManagement.Login.Password)
	}
	if !mgmtauth.IsHashed(cfg.RemoteManagement.Login.PasswordHash) {
		t.Fatalf("expected password-hash to be argon2id, got %q", cfg.RemoteManagement.Login.PasswordHash)
	}
	ok, errVerify := mgmtauth.VerifyPassword(cfg.RemoteManagement.Login.PasswordHash, "super-secret-password")
	if errVerify != nil || !ok {
		t.Fatalf("expected the hash to verify the original password, ok=%v err=%v", ok, errVerify)
	}
	if cfg.RemoteManagement.Login.SessionSecret == "" {
		t.Fatal("expected session-secret to be generated once an account exists")
	}
	if cfg.RemoteManagement.Login.UserHandle == "" {
		t.Fatal("expected user-handle to be generated once an account exists")
	}

	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(saved), "super-secret-password") {
		t.Fatal("expected the plaintext password to not be persisted")
	}
	if !strings.Contains(string(saved), "password-hash") {
		t.Fatal("expected the hashed password to be persisted")
	}

	// Loading again must not re-hash an already-hashed value nor regenerate
	// the secret/handle that were just written.
	reloaded, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig (second pass): %v", err)
	}
	if reloaded.RemoteManagement.Login.PasswordHash != cfg.RemoteManagement.Login.PasswordHash {
		t.Fatal("expected the password hash to be stable across reloads")
	}
	if reloaded.RemoteManagement.Login.SessionSecret != cfg.RemoteManagement.Login.SessionSecret {
		t.Fatal("expected session-secret to be stable across reloads")
	}
}

func TestLoginPasswordHashedOnLoadV8Path(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	body := "management:\n  secret-key: test-key\n  login:\n    username: admin\n    password: super-secret-password\n"
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !mgmtauth.IsHashed(cfg.RemoteManagement.Login.PasswordHash) {
		t.Fatal("expected password-hash to be argon2id")
	}
	if strings.Contains(string(saved), "super-secret-password") {
		t.Fatal("expected the plaintext password to not be persisted")
	}
	if strings.Contains(string(saved), "remote-management") {
		t.Fatal("expected the v8 'management' key path to be preserved, not migrated to legacy naming")
	}
}

func TestLoginWithoutAccountDoesNotGenerateSecrets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("remote-management:\n  secret-key: test-key\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.RemoteManagement.Login.SessionSecret != "" || cfg.RemoteManagement.Login.UserHandle != "" {
		t.Fatal("expected no session-secret/user-handle without a configured account")
	}
}

func TestParseConfigBytesHashesLoginPasswordWithoutPersisting(t *testing.T) {
	data := []byte("remote-management:\n  secret-key: test-key\n  login:\n    username: admin\n    password: super-secret-password\n")
	cfg, err := ParseConfigBytes(data)
	if err != nil {
		t.Fatalf("ParseConfigBytes: %v", err)
	}
	if cfg.RemoteManagement.Login.Password != "" {
		t.Fatal("expected plaintext password to be cleared in-memory")
	}
	if !mgmtauth.IsHashed(cfg.RemoteManagement.Login.PasswordHash) {
		t.Fatal("expected password-hash to be argon2id")
	}
}

// TestLoginExcludedFromJSONConfigView mirrors secret-key's own exclusion: the
// entire RemoteManagement struct (and therefore Login) carries json:"-" on
// Config, so a JSON marshal of the config must not leak the account at all.
func TestLoginExcludedFromJSONConfigView(t *testing.T) {
	cfg := &Config{}
	cfg.RemoteManagement.Login.Username = "admin"
	cfg.RemoteManagement.Login.PasswordHash = "$argon2id$v=19$m=65536,t=3,p=4$salt$hash"
	cfg.RemoteManagement.Login.SessionSecret = "top-secret-session-secret"

	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	for _, leak := range []string{"top-secret-session-secret", "password-hash", "session-secret", "\"login\""} {
		if strings.Contains(string(raw), leak) {
			t.Fatalf("expected JSON config view to exclude management login; found %q in %s", leak, raw)
		}
	}
}
