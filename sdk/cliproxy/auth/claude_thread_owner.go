package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

// Claude thread continuations (thread: {"type":"continue","previous_message_id":...})
// carry only the delta of a conversation. The thread state lives with the upstream
// account that produced previous_message_id, so a continuation routed to any other
// credential fails with a 404 "No thread state was found", and the client has to
// replay the whole conversation (losing its prompt cache). The owner store remembers
// which credential produced each assistant message so continuations can be routed
// back to it, including across restarts.
const (
	claudeThreadOwnerTTL        = 24 * time.Hour
	claudeThreadOwnerMaxEntries = 20000
	claudeThreadOwnerSaveEvery  = time.Minute
)

type claudeThreadOwnerEntry struct {
	AuthID string    `json:"auth_id"`
	SeenAt time.Time `json:"seen_at"`
}

type claudeThreadOwnerStore struct {
	mu      sync.Mutex
	saveMu  sync.Mutex
	entries map[string]claudeThreadOwnerEntry
	dirty   bool
	now     func() time.Time
}

type claudeThreadOwnerStateFile struct {
	Version int                               `json:"version"`
	Owners  map[string]claudeThreadOwnerEntry `json:"owners"`
}

var claudeThreadOwners = newClaudeThreadOwnerStore()

// claudeThreadOwnerStatePath is the file persistence was started with; empty
// keeps the owners in memory only.
var claudeThreadOwnerStatePath atomic.Value

func newClaudeThreadOwnerStore() *claudeThreadOwnerStore {
	return &claudeThreadOwnerStore{entries: make(map[string]claudeThreadOwnerEntry), now: time.Now}
}

// RecordClaudeThreadOwner remembers that authID produced the Claude message messageID.
func RecordClaudeThreadOwner(messageID, authID string) {
	claudeThreadOwners.record(messageID, authID)
}

// ClaudeThreadOwner returns the credential recorded as the producer of the Claude
// message messageID, or "" when it is unknown or expired.
func ClaudeThreadOwner(messageID string) string {
	return claudeThreadOwners.owner(messageID)
}

func (s *claudeThreadOwnerStore) record(messageID, authID string) {
	messageID = strings.TrimSpace(messageID)
	authID = strings.TrimSpace(authID)
	if s == nil || messageID == "" || authID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.entries[messageID] = claudeThreadOwnerEntry{AuthID: authID, SeenAt: now}
	s.dirty = true
	if len(s.entries) > claudeThreadOwnerMaxEntries {
		s.pruneLocked(now)
	}
}

// pruneLocked drops expired entries and, if the store is still over its cap,
// the oldest entries down to 90% of the cap so pruning is amortized.
func (s *claudeThreadOwnerStore) pruneLocked(now time.Time) {
	for id, entry := range s.entries {
		if now.Sub(entry.SeenAt) > claudeThreadOwnerTTL {
			delete(s.entries, id)
		}
	}
	if len(s.entries) <= claudeThreadOwnerMaxEntries {
		return
	}
	ids := make([]string, 0, len(s.entries))
	for id := range s.entries {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return s.entries[ids[i]].SeenAt.Before(s.entries[ids[j]].SeenAt) })
	keep := claudeThreadOwnerMaxEntries * 9 / 10
	for _, id := range ids[:len(ids)-keep] {
		delete(s.entries, id)
	}
}

func (s *claudeThreadOwnerStore) owner(messageID string) string {
	messageID = strings.TrimSpace(messageID)
	if s == nil || messageID == "" {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.entries[messageID]
	if !ok {
		return ""
	}
	if s.now().Sub(entry.SeenAt) > claudeThreadOwnerTTL {
		delete(s.entries, messageID)
		s.dirty = true
		return ""
	}
	return entry.AuthID
}

func (s *claudeThreadOwnerStore) empty() bool {
	if s == nil {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries) == 0
}

func (s *claudeThreadOwnerStore) save(path string) error {
	if s == nil || path == "" {
		return nil
	}
	s.saveMu.Lock()
	defer s.saveMu.Unlock()
	s.mu.Lock()
	if !s.dirty {
		s.mu.Unlock()
		return nil
	}
	s.pruneLocked(s.now())
	owners := make(map[string]claudeThreadOwnerEntry, len(s.entries))
	for id, entry := range s.entries {
		owners[id] = entry
	}
	s.dirty = false
	s.mu.Unlock()

	errWrite := writeClaudeThreadOwnerState(path, owners)
	if errWrite != nil {
		s.mu.Lock()
		s.dirty = true
		s.mu.Unlock()
	}
	return errWrite
}

func writeClaudeThreadOwnerState(path string, owners map[string]claudeThreadOwnerEntry) error {
	data, errMarshal := json.Marshal(claudeThreadOwnerStateFile{Version: 1, Owners: owners})
	if errMarshal != nil {
		return errMarshal
	}
	if errMkdir := os.MkdirAll(filepath.Dir(path), 0o700); errMkdir != nil {
		return errMkdir
	}
	tmp := path + ".tmp"
	if errWriteFile := os.WriteFile(tmp, data, 0o600); errWriteFile != nil {
		return errWriteFile
	}
	return os.Rename(tmp, path)
}

// load merges unexpired owners from path into the store. A missing file is fine.
func (s *claudeThreadOwnerStore) load(path string) {
	if s == nil || path == "" {
		return
	}
	data, errRead := os.ReadFile(path)
	if errRead != nil {
		if !errors.Is(errRead, os.ErrNotExist) {
			log.Warnf("claude thread owners: read state %s: %v", path, errRead)
		}
		return
	}
	var state claudeThreadOwnerStateFile
	if errUnmarshal := json.Unmarshal(data, &state); errUnmarshal != nil {
		log.Warnf("claude thread owners: parse state %s: %v", path, errUnmarshal)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	restored := 0
	for id, entry := range state.Owners {
		if id == "" || entry.AuthID == "" || now.Sub(entry.SeenAt) > claudeThreadOwnerTTL {
			continue
		}
		if current, ok := s.entries[id]; ok && current.SeenAt.After(entry.SeenAt) {
			continue
		}
		s.entries[id] = entry
		restored++
	}
	if len(s.entries) > claudeThreadOwnerMaxEntries {
		s.pruneLocked(now)
	}
	log.Infof("claude thread owners: restored %d owners from %s", restored, path)
}

// StartClaudeThreadOwnerPersistence loads the thread owner state from path and saves
// it periodically until ctx is done, with a final save on exit. An empty path keeps
// the owners in memory only.
func StartClaudeThreadOwnerPersistence(ctx context.Context, path string) {
	if path == "" {
		return
	}
	claudeThreadOwnerStatePath.Store(path)
	store := claudeThreadOwners
	store.load(path)
	// The saver keeps the store and path it started with rather than reading the
	// package variables from its own goroutine.
	go func() {
		ticker := time.NewTicker(claudeThreadOwnerSaveEvery)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				saveClaudeThreadOwners(store, path)
				return
			case <-ticker.C:
				saveClaudeThreadOwners(store, path)
			}
		}
	}()
}

// SaveClaudeThreadOwners writes the thread owner state to the file persistence
// was started with, when it changed. It is a no-op before persistence starts.
func SaveClaudeThreadOwners() {
	path, _ := claudeThreadOwnerStatePath.Load().(string)
	if path == "" {
		return
	}
	saveClaudeThreadOwners(claudeThreadOwners, path)
}

func saveClaudeThreadOwners(store *claudeThreadOwnerStore, path string) {
	if errSave := store.save(path); errSave != nil {
		log.Warnf("claude thread owners: save state %s: %v", path, errSave)
	}
}

// claudeThreadContinueOwner returns the credential that owns the thread a Claude
// "continue" request resumes, or "" when the request is not a continuation or the
// owner is unknown. Requests without a thread pay only a byte scan.
func claudeThreadContinueOwner(opts cliproxyexecutor.Options) string {
	body := opts.OriginalRequest
	if len(body) == 0 || claudeThreadOwners.empty() || !bytes.Contains(body, []byte("previous_message_id")) {
		return ""
	}
	thread := gjson.GetBytes(body, "thread")
	if !thread.IsObject() || thread.Get("type").String() != "continue" {
		return ""
	}
	return claudeThreadOwners.owner(thread.Get("previous_message_id").String())
}
