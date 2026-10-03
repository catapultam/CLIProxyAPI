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
	valid, bridge, ok := s.prepareMark(id, ids, true)
	if !ok {
		return 0
	}
	d, _ := bridge.(Dismisser)
	if d == nil {
		return 0
	}
	return d.Dismissed(id, valid)
}
