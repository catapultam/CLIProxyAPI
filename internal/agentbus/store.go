// Package agentbus links Claude Code sessions across machines through the
// proxy they already send inference to: a registry of sessions learned from
// traffic, per-session inboxes, and delivery by request injection, long-poll
// (/wait), or explicit read.
package agentbus

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	// MaxBodyBytes caps one message body.
	MaxBodyBytes = 16 * 1024

	messageTTL      = 7 * 24 * time.Hour
	idleWithin      = 2 * time.Minute
	awayWithin      = 30 * time.Minute
	offlineListFor  = 24 * time.Hour
	leaseTTL        = 90 * time.Second
	unknownMachine  = "unknown"
	fallbackFolder  = "session"
	addressIDLength = 6
)

// Peer statuses.
const (
	StatusBusy    = "busy"
	StatusIdle    = "idle"
	StatusAway    = "away"
	StatusOffline = "offline"
)

var (
	ErrUnknownTarget = errors.New("unknown target")
	ErrUnknownSender = errors.New("unknown sender session")
	ErrBodyTooLarge  = errors.New("message body too large")
	ErrEmptyBody     = errors.New("message body is empty")
	ErrNameTaken     = errors.New("name already taken")
)

var unsafeAddressChars = regexp.MustCompile(`[^a-z0-9._-]+`)

// Message is one queued message.
type Message struct {
	ID        string    `json:"id"`
	From      string    `json:"from"`
	To        string    `json:"to"`
	Body      string    `json:"body"`
	ReplyTo   string    `json:"reply_to,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// Peer is one session as other sessions see it.
type Peer struct {
	Address  string    `json:"address"`
	Name     string    `json:"name,omitempty"`
	Machine  string    `json:"machine"`
	Cwd      string    `json:"cwd,omitempty"`
	Status   string    `json:"status"`
	LastSeen time.Time `json:"last_seen"`
}

type session struct {
	ID          string    `json:"id"`
	Name        string    `json:"name,omitempty"`
	Machine     string    `json:"machine,omitempty"`
	Cwd         string    `json:"cwd,omitempty"`
	FirstSeen   time.Time `json:"first_seen"`
	LastRequest time.Time `json:"last_request"`
	WaiterSeen  time.Time `json:"waiter_seen"`
	Mod         bool      `json:"mod,omitempty"`
	Waits       bool      `json:"waits,omitempty"`
	Closed      bool      `json:"closed,omitempty"`
	NoteSent    bool      `json:"note_sent,omitempty"`
	NotedPeers  string    `json:"noted_peers,omitempty"`
	Inbox       []Message `json:"inbox,omitempty"`
	inflight    int
	waiterGen   uint64
	notify      chan struct{}
}

func (s *session) lastSeen() time.Time {
	if s.WaiterSeen.After(s.LastRequest) {
		return s.WaiterSeen
	}
	return s.LastRequest
}

// Store holds sessions and inboxes. It is safe for concurrent use.
type Store struct {
	path  string
	now   func() time.Time
	mu    sync.Mutex
	byID  map[string]*session
	dirty bool
	// waitTimeout bounds one /wait long-poll (defaultWaitTimeout when zero).
	waitTimeout time.Duration
}

// NewStore returns an empty store persisted at path (empty disables saving).
func NewStore(path string, now func() time.Time) *Store {
	if now == nil {
		now = time.Now
	}
	return &Store{path: path, now: now, byID: make(map[string]*session)}
}

func (s *Store) get(id string) *session {
	sess, ok := s.byID[id]
	if !ok {
		sess = &session{ID: id, FirstSeen: s.now()}
		s.byID[id] = sess
		s.dirty = true
	}
	return sess
}

// Touch records a request from a session (main thread or subagent).
func (s *Store) Touch(id string) {
	if id == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.get(id)
	sess.LastRequest = s.now()
	sess.Closed = false
	s.dirty = true
}

// BeginRequest and EndRequest bracket an in-flight main-thread request.
func (s *Store) BeginRequest(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.get(id).inflight++
}

func (s *Store) EndRequest(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess := s.byID[id]; sess != nil && sess.inflight > 0 {
		sess.inflight--
	}
}

// Hello records what a client reports: machine, cwd and an optional friendly
// name. A name already used by another session is ignored. /hello and /wait
// are also reachable from curl and the legacy wait.sh hook, so reaching this
// method does not by itself mean the agentbus mod is running: mod is true
// only when the caller sent an explicit marker (the /hello body's "mod"
// field, or /wait's "mod=1" query parameter). Once set, Mod is never cleared
// back to false.
func (s *Store) Hello(id, machine, cwd, name string, mod bool) {
	if id == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.get(id)
	if mod {
		sess.Mod = true
	}
	sess.Closed = false
	if machine = strings.TrimSpace(machine); machine != "" {
		sess.Machine = machine
	}
	if cwd = strings.TrimSpace(cwd); cwd != "" {
		sess.Cwd = cwd
	}
	sess.WaiterSeen = s.now()
	if name = strings.TrimSpace(name); name != "" && !strings.EqualFold(name, sess.Name) && s.nameFree(name, id) {
		sess.Name = name
	}
	s.dirty = true
}

func (s *Store) nameFree(name, exceptID string) bool {
	now := s.now()
	lower := strings.ToLower(name)
	for id, other := range s.byID {
		if id == exceptID || s.statusLocked(other, now) == StatusOffline {
			continue
		}
		if strings.EqualFold(other.Name, name) {
			return false
		}
		if s.addressLocked(other) == lower {
			return false
		}
	}
	return true
}

// SetName claims or (with an empty name) clears a session's friendly name.
func (s *Store) SetName(id, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.byID[id]
	if !ok {
		return ErrUnknownSender
	}
	name = strings.TrimSpace(name)
	if name != "" && !s.nameFree(name, id) {
		return ErrNameTaken
	}
	sess.Name = name
	s.dirty = true
	return nil
}

func sanitizeAddressPart(v string) string {
	v = unsafeAddressChars.ReplaceAllString(strings.ToLower(strings.TrimSpace(v)), "-")
	return strings.Trim(v, "-")
}

func (s *Store) addressLocked(sess *session) string {
	machine := sanitizeAddressPart(sess.Machine)
	if machine == "" {
		machine = unknownMachine
	}
	folder := ""
	if sess.Cwd != "" {
		folder = sanitizeAddressPart(filepath.Base(strings.ReplaceAll(sess.Cwd, `\`, "/")))
	}
	if folder == "" {
		folder = fallbackFolder
	}
	short := sess.ID
	if len(short) > addressIDLength {
		short = short[:addressIDLength]
	}
	return machine + "/" + folder + "-" + strings.ToLower(short)
}

// Address is the session's automatic address.
func (s *Store) Address(id string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.byID[id]
	if !ok {
		return ""
	}
	return s.addressLocked(sess)
}

// Resolve finds a session by friendly name or address (case-insensitive).
func (s *Store) Resolve(target string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.resolveLocked(target)
}

func (s *Store) resolveLocked(target string) (string, bool) {
	target = strings.TrimSpace(target)
	if target == "" {
		return "", false
	}
	for id, sess := range s.byID {
		if sess.Name != "" && strings.EqualFold(sess.Name, target) {
			return id, true
		}
	}
	lower := strings.ToLower(target)
	for id, sess := range s.byID {
		if s.addressLocked(sess) == lower {
			return id, true
		}
	}
	return "", false
}

func newMessageID() string {
	var b [10]byte
	_, _ = rand.Read(b[:])
	return "m_" + hex.EncodeToString(b[:])
}

// Send queues a message from a known session to a name or address.
func (s *Store) Send(fromID, to, body, replyTo string) (Message, error) {
	if strings.TrimSpace(body) == "" {
		return Message{}, ErrEmptyBody
	}
	if len(body) > MaxBodyBytes {
		return Message{}, ErrBodyTooLarge
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	from, ok := s.byID[fromID]
	if !ok {
		return Message{}, ErrUnknownSender
	}
	targetID, ok := s.resolveLocked(to)
	if !ok {
		return Message{}, ErrUnknownTarget
	}
	target := s.byID[targetID]
	msg := Message{
		ID:        newMessageID(),
		From:      s.addressLocked(from),
		To:        s.addressLocked(target),
		Body:      body,
		ReplyTo:   strings.TrimSpace(replyTo),
		CreatedAt: s.now(),
	}
	if from.Name != "" {
		msg.From = from.Name + " (" + msg.From + ")"
	}
	target.Inbox = append(target.Inbox, msg)
	if target.notify != nil {
		close(target.notify)
		target.notify = nil
	}
	s.dirty = true
	return msg, nil
}

func (s *Store) expireLocked(sess *session) {
	cutoff := s.now().Add(-messageTTL)
	kept := sess.Inbox[:0]
	for _, m := range sess.Inbox {
		if m.CreatedAt.After(cutoff) {
			kept = append(kept, m)
		}
	}
	if len(kept) != len(sess.Inbox) {
		s.dirty = true
	}
	sess.Inbox = kept
}

// Claim removes and returns every pending message for a session.
func (s *Store) Claim(id string) []Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.byID[id]
	if !ok {
		return nil
	}
	s.expireLocked(sess)
	if len(sess.Inbox) == 0 {
		return nil
	}
	out := sess.Inbox
	sess.Inbox = nil
	s.dirty = true
	return out
}

// Pending reports whether a session has unclaimed messages.
func (s *Store) Pending(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.byID[id]
	if !ok {
		return false
	}
	s.expireLocked(sess)
	return len(sess.Inbox) > 0
}

// Return puts claimed messages back at the front of a session's inbox, for
// deliveries that failed.
func (s *Store) Return(id string, msgs []Message) {
	if len(msgs) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.get(id)
	sess.Inbox = append(append([]Message(nil), msgs...), sess.Inbox...)
	if sess.notify != nil {
		close(sess.notify)
		sess.notify = nil
	}
	s.dirty = true
}

// Notify returns a channel that is closed when a message arrives for id.
func (s *Store) Notify(id string) <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.get(id)
	if sess.notify == nil {
		sess.notify = make(chan struct{})
	}
	return sess.notify
}

// NewWaiter starts a wait for id and returns its generation; an older
// generation is superseded.
func (s *Store) NewWaiter(id string) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.get(id)
	sess.waiterGen++
	sess.WaiterSeen = s.now()
	sess.Waits = true
	sess.Closed = false
	return sess.waiterGen
}

// WaiterCurrent reports whether gen is still the newest waiter for id, and
// refreshes the waiter's last-seen time when it is.
func (s *Store) WaiterCurrent(id string, gen uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.byID[id]
	if !ok || sess.waiterGen != gen {
		return false
	}
	sess.WaiterSeen = s.now()
	return true
}

// Bye marks a known session as closed, so it reports offline immediately and
// drops out of Peers. Unknown ids are ignored; Bye never creates a session.
// The session itself stays in byID (as an offline entry) so its id remains
// resolvable afterward.
func (s *Store) Bye(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.byID[id]
	if !ok {
		return
	}
	sess.Closed = true
	s.dirty = true
}

func (s *Store) statusLocked(sess *session, now time.Time) string {
	switch {
	case sess.Closed:
		return StatusOffline
	case sess.inflight > 0:
		return StatusBusy
	case sess.Waits:
		// The lease is the waiter: an interactive session whose waiter
		// stopped refreshing for leaseTTL is gone, even if it made a plain
		// request more recently.
		if now.Sub(sess.WaiterSeen) <= leaseTTL {
			return StatusIdle
		}
		return StatusOffline
	case now.Sub(sess.WaiterSeen) <= idleWithin:
		return StatusIdle
	case now.Sub(sess.lastSeen()) <= awayWithin:
		return StatusAway
	default:
		return StatusOffline
	}
}

// Peers lists sessions that are not offline, most recent first.
func (s *Store) Peers() []Peer {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	out := make([]Peer, 0, len(s.byID))
	for _, sess := range s.byID {
		status := s.statusLocked(sess, now)
		if status == StatusOffline {
			continue
		}
		machine := sess.Machine
		if machine == "" {
			machine = unknownMachine
		}
		out = append(out, Peer{
			Address:  s.addressLocked(sess),
			Name:     sess.Name,
			Machine:  machine,
			Cwd:      sess.Cwd,
			Status:   status,
			LastSeen: sess.lastSeen(),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].LastSeen.Equal(out[j].LastSeen) {
			return out[i].LastSeen.After(out[j].LastSeen)
		}
		return out[i].Address < out[j].Address
	})
	return out
}

type stateFile struct {
	Version  int        `json:"version"`
	Sessions []*session `json:"sessions"`
}

// Save writes the store when it changed since the last save.
func (s *Store) Save() error {
	if s.path == "" {
		return nil
	}
	s.mu.Lock()
	if !s.dirty {
		s.mu.Unlock()
		return nil
	}
	state := stateFile{Version: 1}
	cutoff := s.now().Add(-offlineListFor)
	for _, sess := range s.byID {
		s.expireLocked(sess)
		if sess.lastSeen().Before(cutoff) && len(sess.Inbox) == 0 && sess.Name == "" {
			continue
		}
		cp := *sess
		cp.Inbox = append([]Message(nil), sess.Inbox...)
		state.Sessions = append(state.Sessions, &cp)
	}
	s.dirty = false
	s.mu.Unlock()

	data, errMarshal := json.Marshal(state)
	if errMarshal == nil {
		if errMkdir := os.MkdirAll(filepath.Dir(s.path), 0o700); errMkdir != nil {
			errMarshal = errMkdir
		}
	}
	if errMarshal == nil {
		tmp := s.path + ".tmp"
		if errWrite := os.WriteFile(tmp, data, 0o600); errWrite != nil {
			errMarshal = errWrite
		} else if errRename := os.Rename(tmp, s.path); errRename != nil {
			errMarshal = errRename
		}
	}
	if errMarshal != nil {
		s.mu.Lock()
		s.dirty = true
		s.mu.Unlock()
	}
	return errMarshal
}

// Load restores sessions and inboxes; a missing file is not an error.
func (s *Store) Load() error {
	if s.path == "" {
		return nil
	}
	data, errRead := os.ReadFile(s.path)
	if errRead != nil {
		if os.IsNotExist(errRead) {
			return nil
		}
		return errRead
	}
	var state stateFile
	if errUnmarshal := json.Unmarshal(data, &state); errUnmarshal != nil {
		return errUnmarshal
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sess := range state.Sessions {
		if sess == nil || sess.ID == "" {
			continue
		}
		s.byID[sess.ID] = sess
	}
	return nil
}
