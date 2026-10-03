package slackbridge

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"sync"
)

var (
	errConfigUser = errors.New("user is set in config.yaml")
	errNotAllowed = errors.New("user is not allowed")
)

type allowedUser struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	// config marks users seeded from allowed-emails; they are not persisted
	// and can't be removed from Slack.
	config bool
}

type stateFile struct {
	Threads map[string]string `json:"threads"`
	Allowed []allowedUser     `json:"allowed"`
}

// state holds session threads and the allowlist. Every change is written
// through to disk; changes are rare.
type state struct {
	path     string
	mu       sync.Mutex
	threads  map[string]string // session id -> thread ts
	sessions map[string]string // thread ts -> session id
	users    []allowedUser     // config users first
}

// loadState reads path; a missing file is an empty state. On a corrupt file it
// returns an empty state and the error.
func loadState(path string) (*state, error) {
	st := &state{path: path, threads: map[string]string{}, sessions: map[string]string{}}
	if path == "" {
		return st, nil
	}
	data, errRead := os.ReadFile(path)
	if errRead != nil {
		if os.IsNotExist(errRead) {
			return st, nil
		}
		return st, errRead
	}
	var file stateFile
	if errJSON := json.Unmarshal(data, &file); errJSON != nil {
		return st, errJSON
	}
	for sid, ts := range file.Threads {
		st.threads[sid] = ts
		st.sessions[ts] = sid
	}
	st.users = file.Allowed
	return st, nil
}

// seed puts config users first. A persisted Slack-added entry for the same ID
// is replaced by the config entry, and clashing labels are renumbered.
func (st *state) seed(config []allowedUser) {
	st.mu.Lock()
	defer st.mu.Unlock()
	previous := st.users
	st.users = nil
	seen := map[string]bool{}
	for _, u := range config {
		if seen[u.ID] {
			continue
		}
		seen[u.ID] = true
		u.Label = st.uniqueLabelLocked(u.Label)
		u.config = true
		st.users = append(st.users, u)
	}
	for _, u := range previous {
		if seen[u.ID] || u.config {
			continue
		}
		seen[u.ID] = true
		u.Label = st.uniqueLabelLocked(u.Label)
		st.users = append(st.users, u)
	}
}

func (st *state) uniqueLabelLocked(base string) string {
	taken := func(label string) bool {
		for _, u := range st.users {
			if u.Label == label {
				return true
			}
		}
		return false
	}
	if !taken(base) {
		return base
	}
	for i := 2; ; i++ {
		if label := base + strconv.Itoa(i); !taken(label) {
			return label
		}
	}
}

func (st *state) user(id string) (allowedUser, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	for _, u := range st.users {
		if u.ID == id {
			return u, true
		}
	}
	return allowedUser{}, false
}

func (st *state) labels() []string {
	st.mu.Lock()
	defer st.mu.Unlock()
	out := make([]string, 0, len(st.users))
	for _, u := range st.users {
		out = append(out, u.Label)
	}
	return out
}

// mentionIDs maps label to user ID.
func (st *state) mentionIDs() map[string]string {
	st.mu.Lock()
	defer st.mu.Unlock()
	out := make(map[string]string, len(st.users))
	for _, u := range st.users {
		out[u.Label] = u.ID
	}
	return out
}

// idLabels maps user ID to label.
func (st *state) idLabels() map[string]string {
	st.mu.Lock()
	defer st.mu.Unlock()
	out := make(map[string]string, len(st.users))
	for _, u := range st.users {
		out[u.ID] = u.Label
	}
	return out
}

// allow adds a user with a frozen label made from rawLabel. It returns the
// existing entry and false when the user is already allowed.
func (st *state) allow(id, rawLabel string) (allowedUser, bool, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	for _, u := range st.users {
		if u.ID == id {
			return u, false, nil
		}
	}
	u := allowedUser{ID: id, Label: st.uniqueLabelLocked(sanitizeLabel(rawLabel))}
	st.users = append(st.users, u)
	return u, true, st.saveLocked()
}

func (st *state) remove(id string) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	for i, u := range st.users {
		if u.ID != id {
			continue
		}
		if u.config {
			return errConfigUser
		}
		st.users = append(st.users[:i], st.users[i+1:]...)
		return st.saveLocked()
	}
	return errNotAllowed
}

func (st *state) thread(sid string) (string, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	ts, ok := st.threads[sid]
	return ts, ok
}

func (st *state) session(ts string) (string, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	sid, ok := st.sessions[ts]
	return sid, ok
}

// setThread links a session to a thread unless it already has one.
func (st *state) setThread(sid, ts string) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	if _, ok := st.threads[sid]; ok {
		return false
	}
	st.threads[sid] = ts
	st.sessions[ts] = sid
	if errSave := st.saveLocked(); errSave != nil {
		logSaveError(errSave)
	}
	return true
}

func (st *state) saveLocked() error {
	if st.path == "" {
		return nil
	}
	file := stateFile{Threads: st.threads}
	for _, u := range st.users {
		if !u.config {
			file.Allowed = append(file.Allowed, u)
		}
	}
	data, errJSON := json.Marshal(file)
	if errJSON != nil {
		return errJSON
	}
	if errMkdir := os.MkdirAll(filepath.Dir(st.path), 0o700); errMkdir != nil {
		return errMkdir
	}
	tmp := st.path + ".tmp"
	if errWrite := os.WriteFile(tmp, data, 0o600); errWrite != nil {
		return errWrite
	}
	return os.Rename(tmp, st.path)
}
