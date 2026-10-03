package mgmtauth

import (
	"sync"
	"time"
)

// maxThrottleDelay caps the login backoff window.
const maxThrottleDelay = 30 * time.Second

// Throttle implements the global password-verification backoff described by
// the management login design: every client on the tailnet forwards reaches
// the proxy as the same handful of addresses, so per-IP banning would lock
// out the admin along with an attacker. Instead each failure opens a wait
// window before the next attempt is accepted (1s, 2s, 4s... capped at 30s);
// the counter is global to the single account and resets on success.
//
// Reserve/Release provide atomic single-flight admission: at most one
// password verification is ever in flight, so a burst of concurrent guesses
// cannot all reach argon2 at once and cannot all observe "not yet blocked"
// before any of them finishes. Throttle never sleeps: Reserve reports
// immediately whether an attempt may proceed.
type Throttle struct {
	clock Clock

	mu          sync.Mutex
	inFlight    bool
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

// Reserve attempts to admit a single verification attempt. When ok is true,
// the caller holds the single in-flight slot and must call the returned
// release function exactly once with the verification's outcome, regardless
// of path (success, failure, or error). When ok is false, no slot was
// granted -- either another verification is already in flight, or the
// backoff window from a prior failure has not elapsed -- and retryAfter is
// how long the caller should wait before trying again; release is nil.
func (t *Throttle) Reserve() (release func(success bool), ok bool, retryAfter time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.inFlight {
		// Another verification is already running. Treat it the same as a
		// backoff window so callers always get a usable retry hint.
		retryAfter = time.Second
		if remaining := t.nextAllowed.Sub(t.clock.Now()); remaining > retryAfter {
			retryAfter = remaining
		}
		return nil, false, retryAfter
	}

	now := t.clock.Now()
	if now.Before(t.nextAllowed) {
		return nil, false, t.nextAllowed.Sub(now)
	}

	t.inFlight = true
	return t.release, true, 0
}

// release is the Reserve-returned closure's underlying implementation,
// bound to a single *Throttle via a method value so Reserve never
// allocates a new closure capturing loop state.
func (t *Throttle) release(success bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.inFlight = false
	if success {
		t.failures = 0
		t.nextAllowed = time.Time{}
		return
	}
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
