package mgmtauth

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

// StoreFileName is the sidecar file holding the management panel login
// account, kept next to the active config file rather than inside it: a
// writer that mutates config.yaml (e.g. a stale PUT /v8/management/config)
// can no longer resurrect a revoked session-secret or a deleted passkey,
// and the account never leaks through a config view. The extension is
// deliberately not .json: the auth-file watcher and the /v8/management
// credentials endpoints only ever look at *.json files, so this name keeps
// the store invisible to both without needing any extra filtering code. The
// content is still plain JSON.
const StoreFileName = "management-login.dat"

// managementLoginFileEnv, when set, is the full path to the sidecar file,
// overriding every other resolution rule.
const managementLoginFileEnv = "MANAGEMENT_LOGIN_FILE"

// writablePathEnvVars mirrors internal/util.WritablePath's own lookup,
// duplicated here (rather than imported) to keep this package's dependency
// footprint limited to stdlib, go-webauthn, x/crypto, and logrus.
var writablePathEnvVars = []string{"WRITABLE_PATH", "writable_path"}

// ResolveStorePath picks the sidecar file location, in order:
//  1. MANAGEMENT_LOGIN_FILE, a full path, if set.
//  2. WRITABLE_PATH (or writable_path), a directory, if set.
//  3. authDir, the resolved auth-dir, if non-empty (persistent in the
//     default docker-compose, which mounts auths but only a single config
//     file).
//  4. The active config file's own directory.
//
// It returns "" (persistence unavailable) only when none of the above
// apply, i.e. configFilePath is itself empty and no override is set.
func ResolveStorePath(configFilePath, authDir string) string {
	if envPath := strings.TrimSpace(os.Getenv(managementLoginFileEnv)); envPath != "" {
		return envPath
	}
	for _, key := range writablePathEnvVars {
		if value, ok := os.LookupEnv(key); ok {
			if trimmed := strings.TrimSpace(value); trimmed != "" {
				return filepath.Join(filepath.Clean(trimmed), StoreFileName)
			}
		}
	}
	if authDir != "" {
		return filepath.Join(authDir, StoreFileName)
	}
	if configFilePath == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(configFilePath), StoreFileName)
}

// PasskeyRecord is the subset of a webauthn.Credential persisted for one
// registered passkey. Sign counters are intentionally not stored: synced
// passkeys always report 0, and persisting the counter would rewrite the
// store file on every login. RPID records which relying party id the
// passkey was registered under, so a later rp-id change cannot make a
// stale passkey appear usable (or count toward passkeys_available).
type PasskeyRecord struct {
	ID              string    `json:"id"`
	PublicKey       string    `json:"public_key"`
	AttestationType string    `json:"attestation_type,omitempty"`
	Transports      []string  `json:"transports,omitempty"`
	AAGUID          string    `json:"aaguid,omitempty"`
	BackupEligible  bool      `json:"backup_eligible,omitempty"`
	BackupState     bool      `json:"backup_state,omitempty"`
	RPID            string    `json:"rp_id"`
	Name            string    `json:"name,omitempty"`
	Created         time.Time `json:"created,omitempty"`
}

// Account is the full persisted management panel login account. A nil
// *Account anywhere in this package's API means "no account configured".
type Account struct {
	Username       string          `json:"username"`
	PasswordHash   string          `json:"password_hash"`
	UserHandle     string          `json:"user_handle"`
	SessionSecret  string          `json:"session_secret"`
	PasskeyRPID    string          `json:"passkey_rp_id,omitempty"`
	PasskeyOrigins []string        `json:"passkey_origins,omitempty"`
	Passkeys       []PasskeyRecord `json:"passkeys,omitempty"`
}

// Clone returns a deep copy of a (possibly nil) Account, so callers can
// never observe or mutate the store's internal state through a reference.
func (a *Account) Clone() *Account {
	if a == nil {
		return nil
	}
	out := *a
	if a.PasskeyOrigins != nil {
		out.PasskeyOrigins = append([]string(nil), a.PasskeyOrigins...)
	}
	if a.Passkeys != nil {
		out.Passkeys = make([]PasskeyRecord, len(a.Passkeys))
		for i, p := range a.Passkeys {
			out.Passkeys[i] = p
			if p.Transports != nil {
				out.Passkeys[i].Transports = append([]string(nil), p.Transports...)
			}
		}
	}
	return &out
}

// HasAccount reports whether a (possibly nil) Account represents a
// configured username/password account.
func HasAccount(a *Account) bool {
	return a != nil && a.Username != "" && a.PasswordHash != ""
}

// MutationError lets a Store.Mutate callback report a specific outcome
// (e.g. "not found") distinct from an unexpected internal failure, which
// Store.Mutate reports as a plain error instead.
type MutationError struct {
	Status  int
	Message string
}

func (e *MutationError) Error() string { return e.Message }

// Store holds the management panel login account in memory, backed by a
// JSON sidecar file. It is loaded once at startup (Load); there is no file
// watcher, so a hand edit to the sidecar file needs a process restart to
// take effect. All mutations go through Mutate, which deep-copies the
// current account, runs the caller's function, persists the result
// atomically, and only then publishes it to readers. A persist failure
// leaves the live in-memory state unchanged and is reported to the caller.
//
// If Load hits a non-missing-file error (unreadable or corrupt), the store
// enters a "broken" state: Get reports no account (so the management key
// keeps working), and Mutate refuses outright rather than ever writing over
// a file whose content it could not safely read first.
type Store struct {
	path string

	mu        sync.RWMutex
	account   *Account
	broken    bool
	brokenErr error
}

// NewStore creates a Store backed by the sidecar file resolved from
// configFilePath/authDir via ResolveStorePath. Call Load once before
// serving requests.
func NewStore(configFilePath, authDir string) *Store {
	return &Store{path: ResolveStorePath(configFilePath, authDir)}
}

// Load reads the sidecar file into memory. A missing file means no account
// is configured, which is not an error. An unreadable or corrupt file is
// logged and puts the store into the "broken" state described on Store.
func (s *Store) Load() {
	if s.path == "" {
		s.mu.Lock()
		s.account = nil
		s.mu.Unlock()
		return
	}

	data, err := os.ReadFile(s.path)
	if err != nil {
		s.mu.Lock()
		s.account = nil
		if !os.IsNotExist(err) {
			s.broken = true
			s.brokenErr = err
		}
		s.mu.Unlock()
		if !os.IsNotExist(err) {
			log.WithError(err).WithField("path", s.path).Error("mgmtauth: failed to read management login store; the store is unavailable until this is fixed and the process restarts")
		}
		return
	}

	var acct Account
	if err := json.Unmarshal(data, &acct); err != nil {
		log.WithError(err).WithField("path", s.path).Error("mgmtauth: management login store is corrupt; the store is unavailable until this is fixed and the process restarts")
		s.mu.Lock()
		s.account = nil
		s.broken = true
		s.brokenErr = err
		s.mu.Unlock()
		return
	}

	s.mu.Lock()
	s.account = acct.Clone()
	s.mu.Unlock()
}

// Get returns a deep copy of the current account, or nil if none exists
// (including when the store is broken).
func (s *Store) Get() *Account {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.account.Clone()
}

// Mutate runs fn against a deep copy of the current account (nil if none
// exists), persists the result fn returns, and only then publishes it as
// the new in-memory state. If fn returns an error (including a
// *MutationError), or persistence fails, the live state is left exactly as
// it was and the error is returned to the caller. If the store is broken
// (Load hit an unreadable/corrupt file), Mutate refuses immediately with a
// 503 *MutationError without calling fn, so it never writes over content it
// never safely read.
func (s *Store) Mutate(fn func(current *Account) (*Account, error)) (*Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.broken {
		return nil, &MutationError{
			Status:  http.StatusServiceUnavailable,
			Message: fmt.Sprintf("management login store is unavailable: %v", s.brokenErr),
		}
	}

	next, err := fn(s.account.Clone())
	if err != nil {
		return nil, err
	}
	if next == nil {
		return nil, errors.New("mgmtauth: Store.Mutate function returned a nil account")
	}
	if err := s.persist(next); err != nil {
		return nil, fmt.Errorf("mgmtauth: failed to persist management login store: %w", err)
	}
	s.account = next.Clone()
	return s.account.Clone(), nil
}

// persist writes acct to the sidecar file atomically: a temp file in the
// same directory, fsynced, then renamed over the destination.
func (s *Store) persist(acct *Account) error {
	if s.path == "" {
		return errors.New("mgmtauth: no config file path configured; cannot persist the login store")
	}
	data, err := json.MarshalIndent(acct, "", "  ")
	if err != nil {
		return err
	}

	dir := filepath.Dir(s.path)
	tmp, err := os.CreateTemp(dir, "management-login-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, s.path); err != nil {
		return err
	}
	cleanup = false

	// Best-effort: fsync the directory entry too, so the rename itself
	// survives a crash immediately after. Windows has no equivalent (and
	// os.Open of a directory there isn't usable for this), so this is a
	// no-op there.
	if runtime.GOOS != "windows" {
		if dirFile, errOpen := os.Open(dir); errOpen == nil {
			_ = dirFile.Sync()
			_ = dirFile.Close()
		}
	}
	return nil
}
