package agentbus

// Doner is implemented by bridges that mark a Slack message done: the agent
// believes it has fully answered it. Store calls it without holding its
// lock.
type Doner interface {
	// Done marks the ids delivered to sessionID (or a session it took over)
	// done, replacing any earlier receipt, including dismissed: a later Done
	// always wins. Other ids are ignored. It returns how many it marked.
	Done(sessionID string, ids []string) int
}

// Done is an agent marking Slack messages done, believing it fully answered
// them (POST /done, or the mod for a SendMessage/reply of "done"). Only
// message ids count, and the bridge counts only those delivered to session
// id; they no longer count for /ack either. It returns how many were
// marked; an unknown session marks none.
func (s *Store) Done(id string, ids []string) int {
	valid, bridge, ok := s.prepareMark(id, ids, true)
	if !ok {
		return 0
	}
	d, _ := bridge.(Doner)
	if d == nil {
		return 0
	}
	return d.Done(id, valid)
}
