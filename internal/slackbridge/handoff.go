package slackbridge

import (
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/agentbus"
	log "github.com/sirupsen/logrus"
)

// maxMoveHops bounds how far a chain of handoffs is followed.
const maxMoveHops = 8

// movedEntry records that a session handed off to To (at At).
type movedEntry struct {
	To string    `json:"to"`
	At time.Time `json:"at"`
}

var _ agentbus.SessionMover = (*Bridge)(nil)

// SessionMoved re-points the Slack routing of session oldID to newID after
// /clear, /resume or /branch gave the Claude Code session a new id (see
// agentbus.Store.HandOff): its home thread and every thread linked to it,
// where an owner moved its home, users' dm_last, linked conversations and
// top-level DM links. Reply records stay as they are; replyTarget treats
// both ids as one session. It implements agentbus.SessionMover; the Store
// calls it without its lock, and it only touches the state.
func (b *Bridge) SessionMoved(oldID, newID string) {
	if oldID == "" || newID == "" || oldID == newID {
		return
	}
	b.state.moveSession(oldID, newID)
	log.Infof("slack: routing of %s moved to %s", shortID(oldID), shortID(newID))
}

// shortID is a session id's prefix, for logs.
func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// moveSession re-points everything that routes to oldID to newID. When
// newID already has a home thread (a /resume back into an older session),
// it keeps its own, and oldID's thread stays linked to it.
func (st *state) moveSession(oldID, newID string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if ref, ok := st.threads[oldID]; ok {
		if _, has := st.threads[newID]; !has {
			st.threads[newID] = ref
		}
		delete(st.threads, oldID)
	}
	for ts, sid := range st.sessions {
		if sid == oldID {
			st.sessions[ts] = newID
		}
	}
	if home, ok := st.homes[oldID]; ok {
		if _, has := st.homes[newID]; !has {
			st.homes[newID] = home
			if owner, dm := st.homeDMs[oldID]; dm {
				st.homeDMs[newID] = owner
			}
		}
		delete(st.homes, oldID)
	}
	delete(st.homeDMs, oldID)
	for user, e := range st.dmLasts {
		if e.Session == oldID {
			e.Session = newID
			st.dmLasts[user] = e
		}
	}
	for channel, l := range st.convs {
		if l.Session == oldID {
			l.Session = newID
			st.convs[channel] = l
		}
	}
	for i := range st.dmLinks {
		if st.dmLinks[i].Session == oldID {
			st.dmLinks[i].Session = newID
		}
	}
	for old, e := range st.moved {
		if e.To == oldID {
			e.To = newID
			st.moved[old] = e
		}
	}
	delete(st.moved, newID)
	st.moved[oldID] = movedEntry{To: newID, At: st.now()}
	st.dirty = true
}

// currentLocked follows sid's handoffs to the session that has it now. The
// caller holds st.mu.
func (st *state) currentLocked(sid string) string {
	for hop := 0; hop < maxMoveHops; hop++ {
		e, ok := st.moved[sid]
		if !ok || e.To == "" || e.To == sid {
			break
		}
		sid = e.To
	}
	return sid
}

// current follows sid's handoffs to the session that has it now.
func (st *state) current(sid string) string {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.currentLocked(sid)
}

// sameSessionLocked reports whether a and b are one session, across
// handoffs. The caller holds st.mu.
func (st *state) sameSessionLocked(a, b string) bool {
	return a == b || st.currentLocked(a) == st.currentLocked(b)
}

// sameSession is sameSessionLocked for callers without st.mu.
func (st *state) sameSession(a, b string) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.sameSessionLocked(a, b)
}
