package agentbus

import "time"

const (
	// maxUnacked caps the ids per session that /wait handed out and the mod
	// hasn't acknowledged yet; the oldest go first.
	maxUnacked = 256
	// maxAckIDs caps the ids in one /ack.
	maxAckIDs = 100
)

// Receipts is implemented by bridges that show how far a Slack user's
// message got. Store calls it without holding its lock, with the ids of
// messages from Slack users only; ids the bridge doesn't know are ignored.
type Receipts interface {
	// Received reports messages the agentbus mod claimed through /wait.
	Received(ids []string)
	// Read reports messages the model got: injected into a request that
	// succeeded, or acknowledged by the mod (/ack) once the turn they started
	// completed, or once their command ran.
	Read(ids []string)
}

// slackIDs lists the ids of msgs that came from Slack users.
func slackIDs(msgs []Message) []string {
	var ids []string
	for _, m := range msgs {
		if m.FromUser {
			ids = append(ids, m.ID)
		}
	}
	return ids
}

// received records the Slack messages /wait handed to session id as awaiting
// the mod's /ack, and reports them to the bridge. The caller must not hold
// s.mu.
func (s *Store) received(id string, msgs []Message) {
	ids := slackIDs(msgs)
	if len(ids) == 0 {
		return
	}
	s.mu.Lock()
	if sess, ok := s.byID[id]; ok {
		s.noteUnackedLocked(sess, ids)
	}
	r, _ := s.bridge.(Receipts)
	s.mu.Unlock()
	if r != nil {
		r.Received(ids)
	}
}

// noteUnackedLocked adds ids to sess's unacknowledged set, dropping the
// oldest beyond maxUnacked. The caller holds s.mu.
func (s *Store) noteUnackedLocked(sess *session, ids []string) {
	if sess.Unacked == nil {
		sess.Unacked = make(map[string]time.Time, len(ids))
	}
	now := s.now()
	for _, id := range ids {
		sess.Unacked[id] = now
	}
	for len(sess.Unacked) > maxUnacked {
		oldest, oldestAt := "", time.Time{}
		for id, at := range sess.Unacked {
			if oldest == "" || at.Before(oldestAt) || (at.Equal(oldestAt) && id < oldest) {
				oldest, oldestAt = id, at
			}
		}
		delete(sess.Unacked, oldest)
	}
	s.dirty = true
}

// expireUnackedLocked drops unacknowledged ids older than messageTTL. The
// caller holds s.mu.
func (s *Store) expireUnackedLocked(sess *session) {
	cutoff := s.now().Add(-messageTTL)
	for id, at := range sess.Unacked {
		if !at.After(cutoff) {
			delete(sess.Unacked, id)
			s.dirty = true
		}
	}
	if len(sess.Unacked) == 0 {
		sess.Unacked = nil
	}
}

// Ack marks messages the mod got from /wait for session id as read and
// reports them to the bridge. Only ids /wait handed to that session and not
// yet acknowledged count; the rest are ignored. It returns how many counted.
func (s *Store) Ack(id string, ids []string) int {
	s.mu.Lock()
	sess, ok := s.byID[id]
	if !ok {
		s.mu.Unlock()
		return 0
	}
	s.expireUnackedLocked(sess)
	var acked []string
	for _, msgID := range ids {
		if _, pending := sess.Unacked[msgID]; pending {
			delete(sess.Unacked, msgID)
			acked = append(acked, msgID)
		}
	}
	if len(acked) > 0 {
		s.dirty = true
	}
	if len(sess.Unacked) == 0 {
		sess.Unacked = nil
	}
	r, _ := s.bridge.(Receipts)
	s.mu.Unlock()
	if r != nil && len(acked) > 0 {
		r.Read(acked)
	}
	return len(acked)
}
