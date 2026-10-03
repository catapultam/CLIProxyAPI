package mgmtauth

import (
	"sync"
	"time"
)

// maxThrottleDelay caps the login backoff window.
const maxThrottleDelay = 30 * time.Second

// Throttle implements the global password-login backoff described by the
// management login design: every client on the tailnet forwards reaches the
// proxy as the same handful of addresses, so per-IP banning would lock out
// the admin along with an attacker. Instead each failure opens a wait window
// before the next attempt is accepted (1s, 2s, 4s... capped at 30s); the
// counter is global to the single account and resets on success. Throttle
// never sleeps: Allow reports whether an attempt may proceed right now.
type Throttle struct {
	clock Clock

	mu          sync.Mutex
	failures    int
	nextAllowed time.Time
}

// NewThrottle creates a Throttle driven by clock. A nil clock uses
// SystemClock.
func NewThrottle(clock Clock) *Throttle {
	if clock == nil {
		clock = SystemClock{}
	}
	return &Throttle{clock: clock}
}

// Allow reports whether an attempt may proceed now. When it returns false,
// retryAfter is how long the caller should wait before trying again.
func (t *Throttle) Allow() (ok bool, retryAfter time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.clock.Now()
	if now.Before(t.nextAllowed) {
		return false, t.nextAllowed.Sub(now)
	}
	return true, 0
}

// RecordFailure registers a failed attempt and extends the backoff window.
func (t *Throttle) RecordFailure() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.failures++
	shift := t.failures - 1
	if shift > 5 { // 1<<5 = 32s, already past the 30s cap.
		shift = 5
	}
	delay := time.Duration(1<<uint(shift)) * time.Second
	if delay > maxThrottleDelay {
		delay = maxThrottleDelay
	}
	t.nextAllowed = t.clock.Now().Add(delay)
}

// Reset clears the failure counter and backoff window after a successful
// login.
func (t *Throttle) Reset() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.failures = 0
	t.nextAllowed = time.Time{}
}
