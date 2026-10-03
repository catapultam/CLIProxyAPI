package agentbus

// prepareMark is the shared first half of Dismiss, Done and Working:
// it validates ids (message-id format), looks up session id, and, when
// clearUnacked is set, removes them from Unacked at that session and every
// session it handed off to, the same way Ack follows the chain, so a later
// /ack never re-reports an id Dismiss or Done already settled. It returns
// the valid ids and the bridge interface (nil if none is set); ok is false
// for an unknown session or no valid ids. The caller must not hold s.mu;
// this takes and releases it itself, so the bridge is always returned
// without it held.
//
// Working passes clearUnacked false: the turn isn't done with the message
// yet, and the eventual /ack still has to find it in Unacked to move the
// receipt on to read.
func (s *Store) prepareMark(id string, ids []string, clearUnacked bool) (valid []string, bridge Bridge, ok bool) {
	for _, msgID := range ids {
		if validReplyTo.MatchString(msgID) {
			valid = append(valid, msgID)
		}
	}
	if len(valid) == 0 {
		return nil, nil, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, found := s.byID[id]
	if !found {
		return nil, nil, false
	}
	if clearUnacked {
		rest := valid
		for hop := 0; sess != nil && hop < maxMoveHops && len(rest) > 0; hop++ {
			s.expireUnackedLocked(sess)
			var left []string
			for _, msgID := range rest {
				if _, pending := sess.Unacked[msgID]; pending {
					delete(sess.Unacked, msgID)
					s.dirty = true
				} else {
					left = append(left, msgID)
				}
			}
			if len(sess.Unacked) == 0 {
				sess.Unacked = nil
			}
			rest = left
			sess = s.byID[sess.MovedTo]
		}
	}
	return valid, s.bridge, true
}
