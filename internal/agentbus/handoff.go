package agentbus

import (
	"errors"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
)

// maxMoveHops bounds how far Ack follows a chain of handoffs.
const maxMoveHops = 8

// ErrHandOffRefused is a /hello "previous" the Store won't let the new
// session inherit: unknown, the same id, still live, on another machine, or
// already inherited.
var ErrHandOffRefused = errors.New("handoff refused")

// SessionMover is implemented by bridges that keep routing per session id
// (threads, DM routing, conversation links). Store calls SessionMoved
// without holding its lock, after a session handed off to a new id.
type SessionMover interface {
	SessionMoved(oldID, newID string)
}

// HandOff lets session id take over session previous after /clear, /resume
// or /branch gave the same Claude Code session a new id (the mod sends the
// old id as /hello's "previous"). Only a known previous that is closed or
// offline, and on the same machine as id, can be inherited, and only once.
// The new session gets the old one's name (when it has none of its own),
// its queued messages and its unacknowledged ones, and the bridge re-points
// its Slack routing.
//
// Trust: like session ids themselves, previous is whatever the client
// sends. A client on the same machine could name another closed or offline
// session there and inherit it; that is the same trust a session id already
// carries (anyone who knows an id can wait or send as it), and is accepted.
func (s *Store) HandOff(previous, id string) error {
	previous, id = strings.TrimSpace(previous), strings.TrimSpace(id)
	if previous == "" || id == "" || previous == id {
		return ErrHandOffRefused
	}
	s.mu.Lock()
	old, okOld := s.byID[previous]
	cur, okCur := s.byID[id]
	switch {
	case !okOld || !okCur,
		old.MovedTo != "",
		s.statusLocked(old, s.now()) != StatusOffline,
		old.Machine == "" || !strings.EqualFold(old.Machine, cur.Machine):
		s.mu.Unlock()
		return ErrHandOffRefused
	}
	if old.Name != "" && cur.Name == "" {
		cur.Name, old.Name = old.Name, ""
	}
	if len(old.Inbox) > 0 {
		cur.Inbox = append(append([]Message(nil), old.Inbox...), cur.Inbox...)
		old.Inbox = nil
		s.capGuestLocked(cur)
		if cur.notify != nil {
			close(cur.notify)
			cur.notify = nil
		}
	}
	for msgID, at := range old.Unacked {
		if cur.Unacked == nil {
			cur.Unacked = make(map[string]time.Time, len(old.Unacked))
		}
		cur.Unacked[msgID] = at
	}
	old.Unacked = nil
	old.MovedTo, cur.MovedTo = id, ""
	s.dirty = true
	oldAddr, curAddr := s.addressLocked(old), s.addressLocked(cur)
	mover, _ := s.bridge.(SessionMover)
	s.mu.Unlock()
	log.Infof("agentbus: %s took over from %s", curAddr, oldAddr)
	if mover != nil {
		mover.SessionMoved(previous, id)
	}
	return nil
}

// ResolveLive is Resolve limited to sessions that aren't offline.
func (s *Store) ResolveLive(target string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, ok := s.resolveLocked(target)
	if !ok || s.statusLocked(s.byID[id], s.now()) == StatusOffline {
		return "", false
	}
	return id, true
}

// SessionStatus returns a known session's status, or "" for an unknown id.
func (s *Store) SessionStatus(id string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.byID[id]
	if !ok {
		return ""
	}
	return s.statusLocked(sess, s.now())
}
