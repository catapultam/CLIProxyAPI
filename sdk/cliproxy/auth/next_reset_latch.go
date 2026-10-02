package auth

import (
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// nextResetLatchRetry is the Retry-After offered for a latched credential whose
// expected reset has passed but whose usage endpoint has not yet confirmed it.
const nextResetLatchRetry = 2 * time.Minute

// nextResetEnabled is set while the next-reset strategy is the active routing
// strategy. The exhaustion latch only applies then.
var nextResetEnabled atomic.Bool

// SetNextResetEnabled turns the next-reset exhaustion latch on or off.
func SetNextResetEnabled(enabled bool) { nextResetEnabled.Store(enabled) }

// nextResetLatchStore remembers credentials that reported a full window. A
// latched credential stays unavailable until a snapshot observed after the
// latch shows room again, so a fully used credential is never retried on the
// clock alone. In practice the confirming snapshot comes from the usage
// poller, because no traffic reaches a latched credential.
type nextResetLatchStore struct {
	mu    sync.Mutex
	since map[string]time.Time
}

var nextResetLatches = &nextResetLatchStore{since: make(map[string]time.Time)}

func (l *nextResetLatchStore) get(authID string) (time.Time, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	since, ok := l.since[authID]
	return since, ok
}

func (l *nextResetLatchStore) set(authID string, since time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, exists := l.since[authID]; !exists {
		l.since[authID] = since
	}
}

func (l *nextResetLatchStore) clear(authID string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.since, authID)
}

// nextResetWindowFull reports a window that was full when observed, whatever
// its reset time says now.
func nextResetWindowFull(w nextResetWindow) bool {
	return w.Known && (w.Rejected || w.UsedPct >= 100)
}

func nextResetTracked(auth *Auth) bool {
	if auth == nil {
		return false
	}
	provider := strings.ToLower(strings.TrimSpace(auth.Provider))
	if provider != "claude" && provider != "codex" {
		return false
	}
	return !strings.EqualFold(auth.Attributes["auth_kind"], "apikey")
}

// nextResetBlocked updates the latch for auth and reports whether it must be
// skipped, with the time the caller may expect it back.
func nextResetBlocked(auth *Auth, now time.Time) (bool, time.Time) {
	if !nextResetEnabled.Load() || !nextResetTracked(auth) {
		return false, time.Time{}
	}
	snap, ok := nextResetView(auth, nextResetPolled)
	full := ok && (nextResetWindowFull(snap.Short) || nextResetWindowFull(snap.Weekly))
	since, latched := nextResetLatches.get(auth.ID)
	switch {
	case full && !latched:
		since = snap.ObservedAt
		if since.IsZero() {
			since = now
		}
		nextResetLatches.set(auth.ID, since)
	case latched && ok && !full && snap.ObservedAt.After(since):
		nextResetLatches.clear(auth.ID)
		return false, time.Time{}
	case !latched:
		return false, time.Time{}
	}
	until := now.Add(nextResetLatchRetry)
	for _, w := range []nextResetWindow{snap.Short, snap.Weekly} {
		if nextResetWindowFull(w) && w.ResetsAt.After(until) {
			until = w.ResetsAt
		}
	}
	return true, until
}

// nextResetIsLatched reports whether auth is currently latched, without
// re-evaluating it.
func nextResetIsLatched(authID string) bool {
	_, latched := nextResetLatches.get(authID)
	return latched
}
