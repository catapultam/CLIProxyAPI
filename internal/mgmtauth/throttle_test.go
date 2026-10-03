package mgmtauth

import (
	"testing"
	"time"
)

func TestThrottleAllowsFirstAttempt(t *testing.T) {
	clock := newMockClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	th := NewThrottle(clock)
	ok, retryAfter := th.Allow()
	if !ok || retryAfter != 0 {
		t.Fatalf("Allow() = (%v, %v), want (true, 0)", ok, retryAfter)
	}
}

func TestThrottleBackoffSequence(t *testing.T) {
	clock := newMockClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	th := NewThrottle(clock)

	wantDelays := []time.Duration{
		1 * time.Second,
		2 * time.Second,
		4 * time.Second,
		8 * time.Second,
		16 * time.Second,
		30 * time.Second, // capped
		30 * time.Second, // stays capped
	}

	for i, want := range wantDelays {
		th.RecordFailure()
		ok, retryAfter := th.Allow()
		if ok {
			t.Fatalf("attempt %d: Allow() = true immediately after a failure, want blocked", i)
		}
		if retryAfter != want {
			t.Fatalf("attempt %d: retryAfter = %v, want %v", i, retryAfter, want)
		}
		// Advance the clock to just past the window so the next failure starts
		// its own countdown rather than accumulating on top of this one.
		clock.Advance(want)
	}
}

func TestThrottleBlocksWithinWindow(t *testing.T) {
	clock := newMockClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	th := NewThrottle(clock)

	th.RecordFailure() // opens a 1s window
	ok, retryAfter := th.Allow()
	if ok {
		t.Fatal("expected Allow() to block inside the backoff window")
	}
	if retryAfter != time.Second {
		t.Fatalf("retryAfter = %v, want 1s", retryAfter)
	}

	clock.Advance(500 * time.Millisecond)
	ok, retryAfter = th.Allow()
	if ok {
		t.Fatal("expected Allow() to still block halfway through the window")
	}
	if retryAfter != 500*time.Millisecond {
		t.Fatalf("retryAfter = %v, want 500ms", retryAfter)
	}

	clock.Advance(500 * time.Millisecond)
	ok, retryAfter = th.Allow()
	if !ok || retryAfter != 0 {
		t.Fatalf("Allow() = (%v, %v) once the window has elapsed, want (true, 0)", ok, retryAfter)
	}
}

func TestThrottleResetOnSuccess(t *testing.T) {
	clock := newMockClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	th := NewThrottle(clock)

	th.RecordFailure()
	th.RecordFailure()
	th.RecordFailure()
	th.Reset()

	ok, retryAfter := th.Allow()
	if !ok || retryAfter != 0 {
		t.Fatalf("Allow() after Reset = (%v, %v), want (true, 0)", ok, retryAfter)
	}

	// The next failure after a reset should restart the sequence at 1s, not
	// continue from where it left off.
	th.RecordFailure()
	_, retryAfter = th.Allow()
	if retryAfter != time.Second {
		t.Fatalf("retryAfter after reset+failure = %v, want 1s", retryAfter)
	}
}
