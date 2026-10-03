package agentbus

// Worker is implemented by bridges that mark a Slack message working: a
// turn handling it is still running. Store calls it without holding its
// lock.
type Worker interface {
	// Working marks the ids delivered to sessionID (or a session it took
	// over) working, when that is further along than their current receipt
	// (it never moves a receipt back, including off done or dismissed).
	// Other ids are ignored. It returns how many it marked.
	Working(sessionID string, ids []string) int
}

// Working is a turn still running on Slack messages, 15s after it started
// (POST /working, or the mod for a SendMessage/reply of "working"). Only
// message ids count, and the bridge counts only those delivered to session
// id. It returns how many were marked; an unknown session marks none.
//
// Unlike Dismiss and Done, this leaves Unacked alone: the turn isn't done
// with these messages yet, and the eventual /ack still has to find them
// there to move them on to read.
func (s *Store) Working(id string, ids []string) int {
	valid, bridge, ok := s.prepareMark(id, ids, false)
	if !ok {
		return 0
	}
	w, _ := bridge.(Worker)
	if w == nil {
		return 0
	}
	return w.Working(id, valid)
}
