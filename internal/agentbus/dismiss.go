package agentbus

// Dismisser is implemented by bridges that can withdraw a Slack message's
// receipt: the agent says the message wasn't meant for it, and the sender
// sees no reaction at all. Store calls it without holding its lock.
type Dismisser interface {
	// Dismissed clears the receipts of the ids delivered to sessionID (or to
	// a session it took over) and keeps them clear; other ids are ignored. It
	// returns how many it dismissed.
	Dismissed(sessionID string, ids []string) int
}

// Dismiss is an agent dismissing Slack messages that weren't meant for it
// (POST /dismiss, or the mod for a SendMessage of "ignore"). Only message ids
// count, and the bridge counts only those delivered to session id; they no
// longer count for /ack either. It returns how many were dismissed; an
// unknown session dismisses none.
func (s *Store) Dismiss(id string, ids []string) int {
	var valid []string
	for _, msgID := range ids {
		if validReplyTo.MatchString(msgID) {
			valid = append(valid, msgID)
		}
	}
	if len(valid) == 0 {
		return 0
	}
	s.mu.Lock()
	sess, ok := s.byID[id]
	if !ok {
		s.mu.Unlock()
		return 0
	}
	for _, msgID := range valid {
		if _, pending := sess.Unacked[msgID]; pending {
			delete(sess.Unacked, msgID)
			s.dirty = true
		}
	}
	if len(sess.Unacked) == 0 {
		sess.Unacked = nil
	}
	d, _ := s.bridge.(Dismisser)
	s.mu.Unlock()
	if d == nil {
		return 0
	}
	return d.Dismissed(id, valid)
}
