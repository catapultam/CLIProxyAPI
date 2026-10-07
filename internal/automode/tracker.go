// Package automode tracks, per Claude Code session, whether Claude Code's
// auto mode is still using the server-side classifier review, has fallen
// back to the client's own (billed) classifier, or is not in auto mode at
// all. Claude Code only surfaces this to the user locally (its `/status`
// shows "Auto mode server: Enabled/Disabled"); this package reconstructs the
// same fact server-side from what Claude Code sends with every request, so a
// status line can display it without asking the client.
package automode

import (
	"sync"
	"time"
)

// Mode values reported in State.Mode.
const (
	ModeServer = "server" // auto mode, server-side classifier review
	ModeLocal  = "local"  // auto mode, fell back to the local (billed) classifier
	ModeOff    = "off"    // not in auto mode
)

// maxEntries bounds memory: once more than this many sessions are tracked,
// the least-recently-updated ones are evicted.
const maxEntries = 5000

// maxAge is how long a session is kept without a fresh observation before
// it is pruned as stale.
const maxAge = 24 * time.Hour

// State is the last known auto-mode status for a session.
type State struct {
	// Mode is one of ModeServer, ModeLocal, or ModeOff.
	Mode string
	// Since is when Mode last changed.
	Since time.Time
	// Updated is when this session was last observed, regardless of
	// whether Mode changed.
	Updated time.Time
}

// Tracker holds the latest auto-mode State per session. It is safe for
// concurrent use.
type Tracker struct {
	mu      sync.Mutex
	entries map[string]State
}

// NewTracker creates an empty Tracker.
func NewTracker() *Tracker {
	return &Tracker{entries: make(map[string]State)}
}

// defaultTracker is the process-wide tracker used by the package-level
// Observe/Lookup helpers.
var defaultTracker = NewTracker()

// Observe records an observation of session's auto-mode status using the
// default tracker. See Tracker.Observe.
func Observe(sessionID string, afk bool, safeguards bool, now time.Time) {
	defaultTracker.Observe(sessionID, afk, safeguards, now)
}

// Lookup returns the last known State for session using the default
// tracker. See Tracker.Lookup.
func Lookup(sessionID string) (State, bool) {
	return defaultTracker.Lookup(sessionID)
}

// modeFor maps the raw signals observed on a request to a Mode.
func modeFor(afk, safeguards bool) string {
	switch {
	case afk && safeguards:
		return ModeServer
	case afk:
		return ModeLocal
	default:
		return ModeOff
	}
}

// Observe records an observation of session's auto-mode status: afk is
// whether the request's anthropic-beta header carried afk-mode-2026-01-31,
// and safeguards is whether the request body carried a top-level
// `safeguards` field. Since is only updated when the resulting Mode differs
// from the previously recorded one. Observe also opportunistically prunes
// stale and excess entries.
func (t *Tracker) Observe(sessionID string, afk bool, safeguards bool, now time.Time) {
	if sessionID == "" {
		return
	}
	mode := modeFor(afk, safeguards)

	t.mu.Lock()
	defer t.mu.Unlock()

	since := now
	if existing, ok := t.entries[sessionID]; ok && existing.Mode == mode {
		since = existing.Since
	}
	t.entries[sessionID] = State{Mode: mode, Since: since, Updated: now}

	t.pruneLocked(now)
}

// Lookup returns the last known State for session, and whether it was
// found.
func (t *Tracker) Lookup(sessionID string) (State, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	state, ok := t.entries[sessionID]
	return state, ok
}

// pruneLocked drops entries not updated within maxAge, then evicts the
// least-recently-updated entries until at most maxEntries remain. Callers
// must hold t.mu.
func (t *Tracker) pruneLocked(now time.Time) {
	for id, state := range t.entries {
		if now.Sub(state.Updated) > maxAge {
			delete(t.entries, id)
		}
	}

	for len(t.entries) > maxEntries {
		var oldestID string
		var oldestUpdated time.Time
		first := true
		for id, state := range t.entries {
			if first || state.Updated.Before(oldestUpdated) {
				oldestID = id
				oldestUpdated = state.Updated
				first = false
			}
		}
		delete(t.entries, oldestID)
	}
}
