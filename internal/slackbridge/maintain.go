package slackbridge

import (
	"context"
	"time"
)

const (
	// flushEvery is how often pending state changes are written to disk.
	// A crash loses at most this much routing state (receipts, dm_last,
	// thread links); allowlist and link changes are written at once.
	flushEvery = 2 * time.Second
	// maintainEvery is how often the bridge refreshes link liveness, prunes
	// state of sessions long gone, and refreshes its own display name.
	maintainEvery = 10 * time.Minute
	// sessionAbsentTTL is how long a session's threads, thread links and
	// home stay after the bus last saw it.
	sessionAbsentTTL = 7 * 24 * time.Hour
)

// runMaintenance flushes state changes every flushEvery and runs maintain
// every maintainEvery, until ctx ends. Stop flushes once more afterwards.
func (b *Bridge) runMaintenance(ctx context.Context) {
	flush := time.NewTicker(flushEvery)
	defer flush.Stop()
	housekeeping := time.NewTicker(maintainEvery)
	defer housekeeping.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-flush.C:
			if errFlush := b.state.flush(); errFlush != nil {
				logSaveError(errFlush)
			}
		case <-housekeeping.C:
			b.maintain()
			b.refreshBotName(ctx)
		}
	}
}

// maintain is the bridge's periodic housekeeping: it refreshes link
// liveness from the bus and drops the links of absent sessions, then drops
// the threads, thread links and homes of sessions the bus hasn't seen for
// sessionAbsentTTL. The bus is asked without any state lock held.
func (b *Bridge) maintain() {
	b.refreshLinks()
	if b.bus == nil {
		return
	}
	seen := map[string]time.Time{}
	for _, sid := range b.state.routedSessions() {
		if at, ok := b.bus.SessionSeen(sid); ok {
			seen[sid] = at
		}
	}
	b.state.pruneSessions(seen)
}

// flush writes the state file when it has unsaved changes.
func (st *state) flush() error {
	st.mu.Lock()
	defer st.mu.Unlock()
	if !st.dirty {
		return nil
	}
	return st.saveLocked()
}

// routedLocked is the set of session ids threads, thread links and homes
// route to. The caller holds st.mu.
func (st *state) routedLocked() map[string]bool {
	set := map[string]bool{}
	for sid := range st.threads {
		set[sid] = true
	}
	for _, sid := range st.sessions {
		set[sid] = true
	}
	for sid := range st.homes {
		set[sid] = true
	}
	return set
}

// routedSessions lists the session ids threads, thread links and homes
// route to.
func (st *state) routedSessions() []string {
	st.mu.Lock()
	defer st.mu.Unlock()
	set := st.routedLocked()
	out := make([]string, 0, len(set))
	for sid := range set {
		out = append(out, sid)
	}
	return out
}

// pruneSessions notes when each routed session was last on the bus (seen;
// a session the bus doesn't know is noted as seen now the first time), then
// drops the threads, thread links and homes of sessions not seen for
// sessionAbsentTTL, and handoff records older than that.
func (st *state) pruneSessions(seen map[string]time.Time) {
	st.mu.Lock()
	defer st.mu.Unlock()
	now := st.now()
	cutoff := now.Add(-sessionAbsentTTL)
	routed := st.routedLocked()
	for sid := range routed {
		last, known := st.seen[sid]
		if at, ok := seen[sid]; ok && (!known || at.After(last)) {
			last, known = at, true
		}
		if !known {
			last = now
		}
		if !last.Equal(st.seen[sid]) {
			st.seen[sid] = last
			st.dirty = true
		}
		if last.After(cutoff) {
			continue
		}
		delete(st.threads, sid)
		delete(st.homes, sid)
		delete(st.homeDMs, sid)
		for ts, owner := range st.sessions {
			if owner == sid {
				delete(st.sessions, ts)
			}
		}
		delete(st.seen, sid)
		st.dirty = true
	}
	for sid := range st.seen {
		if !routed[sid] {
			delete(st.seen, sid)
			st.dirty = true
		}
	}
	for old, e := range st.moved {
		if !e.At.After(cutoff) {
			delete(st.moved, old)
			st.dirty = true
		}
	}
}
