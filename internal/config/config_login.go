package config

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/mgmtauth"
	"gopkg.in/yaml.v3"
)

// normalizeLoginAccount hashes a plaintext management.login.password the
// same way LoadConfigOptional/ParseConfigBytes already hash
// remote-management.secret-key, and lazily generates session-secret /
// user-handle for an account that has a username and password hash but is
// still missing them (e.g. one written by hand directly into config.yaml).
// It returns the set of login.* leaf fields that changed (YAML key -> new
// value), so a caller that persists to disk only rewrites those.
func (cfg *Config) normalizeLoginAccount() (changedFields map[string]string, err error) {
	if cfg == nil {
		return nil, nil
	}
	login := &cfg.RemoteManagement.Login
	changedFields = make(map[string]string)

	if login.Password != "" {
		if mgmtauth.IsHashed(login.Password) {
			login.PasswordHash = login.Password
		} else {
			hashed, errHash := mgmtauth.HashPassword(login.Password)
			if errHash != nil {
				return nil, fmt.Errorf("failed to hash management login password: %w", errHash)
			}
			login.PasswordHash = hashed
		}
		login.Password = ""
		changedFields["password-hash"] = login.PasswordHash
		changedFields["password"] = ""
	}

	hasAccount := login.Username != "" && login.PasswordHash != ""
	if hasAccount && login.SessionSecret == "" {
		secret, errGen := generateLoginSecret()
		if errGen != nil {
			return nil, fmt.Errorf("failed to generate management login session secret: %w", errGen)
		}
		login.SessionSecret = secret
		changedFields["session-secret"] = secret
	}
	if hasAccount && login.UserHandle == "" {
		handle, errGen := generateLoginSecret()
		if errGen != nil {
			return nil, fmt.Errorf("failed to generate management login user handle: %w", errGen)
		}
		login.UserHandle = handle
		changedFields["user-handle"] = handle
	}

	return changedFields, nil
}

// generateLoginSecret returns 32 random bytes, base64url-encoded.
func generateLoginSecret() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// persistLoginAccountFields writes the given login.* fields back to
// configFile, preserving comments and ordering, the same way
// LoadConfigOptional persists a freshly-hashed secret-key. It mirrors
// secret-key's own v8-alias detection: a document already using the
// 'management' top-level key gets updates under 'management.login.*',
// otherwise under 'remote-management.login.*'.
func persistLoginAccountFields(configFile string, data []byte, changedFields map[string]string) error {
	if len(changedFields) == 0 {
		return nil
	}
	prefix := "remote-management"
	var source yaml.Node
	if yaml.Unmarshal(data, &source) == nil && len(source.Content) > 0 &&
		yamlPath(expandConfigAliases(source.Content[0]), "management.login") != nil {
		prefix = "management"
	}

	for key, value := range changedFields {
		path := []string{prefix, "login", key}
		if errSave := SaveConfigPreserveCommentsUpdateNestedScalar(configFile, path, value); errSave != nil {
			return errSave
		}
	}
	return nil
}
