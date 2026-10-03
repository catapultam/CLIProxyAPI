package mgmtauth

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func newTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("write config.yaml: %v", err)
	}
	return NewStore(configPath, ""), configPath
}

func TestStoreMissingFileMeansNoAccount(t *testing.T) {
	store, _ := newTestStore(t)
	store.Load()
	if got := store.Get(); got != nil {
		t.Fatalf("Get() = %+v, want nil for a missing sidecar file", got)
	}
}

func TestStoreCorruptFileMeansNoAccountAndBroken(t *testing.T) {
	store, configPath := newTestStore(t)
	sidecarPath := ResolveStorePath(configPath, "")
	if err := os.WriteFile(sidecarPath, []byte("not json"), 0o600); err != nil {
		t.Fatalf("write corrupt sidecar: %v", err)
	}
	store.Load()
	if got := store.Get(); got != nil {
		t.Fatalf("Get() = %+v, want nil for a corrupt sidecar file", got)
	}

	// S6: a corrupt file must never be overwritten. Mutate refuses with a
	// 503 MutationError instead of calling fn.
	called := false
	_, err := store.Mutate(func(current *Account) (*Account, error) {
		called = true
		return &Account{Username: "admin", PasswordHash: "hash"}, nil
	})
	if called {
		t.Fatal("Mutate must not call fn while the store is broken")
	}
	var mutErr *MutationError
	if !errors.As(err, &mutErr) || mutErr.Status != http.StatusServiceUnavailable {
		t.Fatalf("Mutate error = %v, want a 503 MutationError", err)
	}

	// The on-disk corrupt content must be untouched.
	raw, errRead := os.ReadFile(sidecarPath)
	if errRead != nil {
		t.Fatalf("read sidecar: %v", errRead)
	}
	if string(raw) != "not json" {
		t.Fatalf("sidecar content = %q, want it untouched", raw)
	}
}

// TestStoreUnreadableFileIsBroken covers the "unreadable" half of S6: a
// sidecar that exists but cannot be read (here, a directory in its place)
// must also leave the store broken rather than silently proceeding.
func TestStoreUnreadableFileIsBroken(t *testing.T) {
	store, configPath := newTestStore(t)
	sidecarPath := ResolveStorePath(configPath, "")
	if err := os.Mkdir(sidecarPath, 0o700); err != nil {
		t.Fatalf("mkdir in place of sidecar: %v", err)
	}
	store.Load()
	if got := store.Get(); got != nil {
		t.Fatalf("Get() = %+v, want nil", got)
	}
	_, err := store.Mutate(func(current *Account) (*Account, error) {
		return &Account{Username: "admin", PasswordHash: "hash"}, nil
	})
	var mutErr *MutationError
	if !errors.As(err, &mutErr) || mutErr.Status != http.StatusServiceUnavailable {
		t.Fatalf("Mutate error = %v, want a 503 MutationError", err)
	}
}

func TestResolveStorePathPrecedence(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")

	// 4. Config file's directory, lowest precedence.
	if got := ResolveStorePath(configPath, ""); got != filepath.Join(dir, StoreFileName) {
		t.Fatalf("config-dir fallback = %q, want %q", got, filepath.Join(dir, StoreFileName))
	}

	// 3. authDir beats the config file's directory.
	authDir := filepath.Join(dir, "auths")
	if got := ResolveStorePath(configPath, authDir); got != filepath.Join(authDir, StoreFileName) {
		t.Fatalf("authDir = %q, want %q", got, filepath.Join(authDir, StoreFileName))
	}

	// 2. WRITABLE_PATH beats authDir.
	writable := filepath.Join(dir, "writable")
	t.Setenv("WRITABLE_PATH", writable)
	if got := ResolveStorePath(configPath, authDir); got != filepath.Join(filepath.Clean(writable), StoreFileName) {
		t.Fatalf("WRITABLE_PATH = %q, want %q", got, filepath.Join(filepath.Clean(writable), StoreFileName))
	}

	// 1. MANAGEMENT_LOGIN_FILE (a full path) beats everything.
	explicit := filepath.Join(dir, "explicit-login-store.dat")
	t.Setenv(managementLoginFileEnv, explicit)
	if got := ResolveStorePath(configPath, authDir); got != explicit {
		t.Fatalf("MANAGEMENT_LOGIN_FILE = %q, want %q", got, explicit)
	}
}

func TestResolveStorePathEmptyWithoutConfigOrOverride(t *testing.T) {
	if got := ResolveStorePath("", ""); got != "" {
		t.Fatalf("ResolveStorePath(\"\", \"\") = %q, want empty", got)
	}
}

func TestStoreMutateCreatesAndPersists(t *testing.T) {
	store, configPath := newTestStore(t)
	store.Load()

	acct, err := store.Mutate(func(current *Account) (*Account, error) {
		if current != nil {
			t.Fatalf("expected no existing account, got %+v", current)
		}
		return &Account{Username: "admin", PasswordHash: "hash", UserHandle: "handle", SessionSecret: "secret"}, nil
	})
	if err != nil {
		t.Fatalf("Mutate: %v", err)
	}
	if acct.Username != "admin" {
		t.Fatalf("Username = %q, want admin", acct.Username)
	}

	sidecarPath := ResolveStorePath(configPath, "")
	info, err := os.Stat(sidecarPath)
	if err != nil {
		t.Fatalf("stat sidecar: %v", err)
	}
	// Windows' permission model doesn't support Unix mode bits; os.Chmod
	// there only toggles the read-only attribute, so this check is only
	// meaningful on POSIX platforms.
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("sidecar mode = %v, want 0600", info.Mode().Perm())
	}

	if got := store.Get(); got == nil || got.Username != "admin" {
		t.Fatalf("Get() = %+v, want the newly created account", got)
	}
}

// TestStoreRoundTripsAcrossReload covers the sidecar persistence round trip:
// delete the last passkey, clear rp-id, change the password, then reload
// from a fresh Store instance pointed at the same file and confirm every
// change survived, including the ones that empty a field.
func TestStoreRoundTripsAcrossReload(t *testing.T) {
	store, configPath := newTestStore(t)
	store.Load()

	created := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	_, err := store.Mutate(func(current *Account) (*Account, error) {
		return &Account{
			Username:       "admin",
			PasswordHash:   "hash-v1",
			UserHandle:     "handle",
			SessionSecret:  "secret-v1",
			PasskeyRPID:    "mgmt.example.com",
			PasskeyOrigins: []string{"https://mgmt.example.com"},
			Passkeys: []PasskeyRecord{
				{ID: "cred-1", PublicKey: "pub-1", RPID: "mgmt.example.com", Name: "Only Key", Created: created},
			},
		}, nil
	})
	if err != nil {
		t.Fatalf("Mutate (create): %v", err)
	}

	// Delete the last passkey and clear rp-id/origins; also change the password.
	_, err = store.Mutate(func(current *Account) (*Account, error) {
		next := current.Clone()
		next.Passkeys = nil
		next.PasskeyRPID = ""
		next.PasskeyOrigins = nil
		next.PasswordHash = "hash-v2"
		next.SessionSecret = "secret-v2"
		return next, nil
	})
	if err != nil {
		t.Fatalf("Mutate (clear): %v", err)
	}

	// Reload from a brand new Store instance pointed at the same path,
	// simulating a process restart.
	reloaded := NewStore(configPath, "")
	reloaded.Load()
	got := reloaded.Get()
	if got == nil {
		t.Fatal("expected an account to load from the sidecar file")
	}
	if len(got.Passkeys) != 0 {
		t.Fatalf("Passkeys = %+v, want empty after deleting the last one", got.Passkeys)
	}
	if got.PasskeyRPID != "" {
		t.Fatalf("PasskeyRPID = %q, want empty after clearing it", got.PasskeyRPID)
	}
	if len(got.PasskeyOrigins) != 0 {
		t.Fatalf("PasskeyOrigins = %+v, want empty", got.PasskeyOrigins)
	}
	if got.PasswordHash != "hash-v2" {
		t.Fatalf("PasswordHash = %q, want hash-v2", got.PasswordHash)
	}
	if got.SessionSecret != "secret-v2" {
		t.Fatalf("SessionSecret = %q, want secret-v2", got.SessionSecret)
	}
}

// TestStoreMutatePersistFailureLeavesStateUnchanged verifies that when the
// sidecar write fails, Mutate reports an error and the previously loaded
// in-memory state is untouched.
func TestStoreMutatePersistFailureLeavesStateUnchanged(t *testing.T) {
	store, _ := newTestStore(t)
	store.Load()

	if _, err := store.Mutate(func(current *Account) (*Account, error) {
		return &Account{Username: "admin", PasswordHash: "hash-v1", UserHandle: "handle", SessionSecret: "secret-v1"}, nil
	}); err != nil {
		t.Fatalf("initial Mutate: %v", err)
	}

	// Point the store at a directory that cannot exist as a file path
	// component, so persist's CreateTemp fails deterministically and
	// portably (no permission trickery needed).
	validPath := store.path
	store.path = filepath.Join(validPath, "nested", StoreFileName)

	_, err := store.Mutate(func(current *Account) (*Account, error) {
		next := current.Clone()
		next.PasswordHash = "hash-v2"
		return next, nil
	})
	if err == nil {
		t.Fatal("expected Mutate to fail when persistence fails")
	}

	// Restore the valid path and verify the live state is still the
	// original account, not the attempted (failed) mutation.
	store.path = validPath
	got := store.Get()
	if got == nil || got.PasswordHash != "hash-v1" {
		t.Fatalf("Get() after failed persist = %+v, want unchanged hash-v1", got)
	}
}

func TestStoreMutateFnErrorLeavesStateUnchanged(t *testing.T) {
	store, _ := newTestStore(t)
	store.Load()

	if _, err := store.Mutate(func(current *Account) (*Account, error) {
		return &Account{Username: "admin", PasswordHash: "hash-v1"}, nil
	}); err != nil {
		t.Fatalf("initial Mutate: %v", err)
	}

	sentinel := &MutationError{Status: 404, Message: "passkey not found"}
	_, err := store.Mutate(func(current *Account) (*Account, error) {
		return nil, sentinel
	})
	if err != sentinel {
		t.Fatalf("Mutate error = %v, want the sentinel MutationError", err)
	}

	got := store.Get()
	if got == nil || got.PasswordHash != "hash-v1" {
		t.Fatalf("Get() after fn error = %+v, want unchanged hash-v1", got)
	}
}

func TestAccountCloneIsIndependent(t *testing.T) {
	original := &Account{
		Username:       "admin",
		PasskeyOrigins: []string{"https://a.example.com"},
		Passkeys:       []PasskeyRecord{{ID: "1", Transports: []string{"internal"}}},
	}
	clone := original.Clone()
	clone.PasskeyOrigins[0] = "https://mutated.example.com"
	clone.Passkeys[0].Transports[0] = "mutated"
	clone.Passkeys[0].ID = "mutated"

	if original.PasskeyOrigins[0] != "https://a.example.com" {
		t.Fatalf("mutating the clone's origins affected the original: %v", original.PasskeyOrigins)
	}
	if original.Passkeys[0].Transports[0] != "internal" || original.Passkeys[0].ID != "1" {
		t.Fatalf("mutating the clone's passkeys affected the original: %+v", original.Passkeys)
	}
}

func TestHasAccount(t *testing.T) {
	if HasAccount(nil) {
		t.Fatal("HasAccount(nil) = true, want false")
	}
	if HasAccount(&Account{Username: "admin"}) {
		t.Fatal("HasAccount with no password hash = true, want false")
	}
	if !HasAccount(&Account{Username: "admin", PasswordHash: "hash"}) {
		t.Fatal("HasAccount with username+hash = false, want true")
	}
}
