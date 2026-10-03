package mgmtauth

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestThrottleReserveAllowsFirstAttempt(t *testing.T) {
	clock := newMockClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	th := NewThrottle(clock)
	release, ok, retryAfter := th.Reserve()
	if !ok || retryAfter != 0 || release == nil {
		t.Fatalf("Reserve() = (release=%v, ok=%v, retryAfter=%v), want a granted slot", release != nil, ok, retryAfter)
	}
	release(true)
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
		release, ok, _ := th.Reserve()
		if !ok {
			t.Fatalf("attempt %d: Reserve() blocked unexpectedly", i)
		}
		release(false)

		_, ok, retryAfter := th.Reserve()
		if ok {
			t.Fatalf("attempt %d: Reserve() = true immediately after a failure, want blocked", i)
		}
		if retryAfter != want {
			t.Fatalf("attempt %d: retryAfter = %v, want %v", i, retryAfter, want)
		}
		clock.Advance(want)
	}
}

func TestThrottleBlocksWithinWindow(t *testing.T) {
	clock := newMockClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	th := NewThrottle(clock)

	release, ok, _ := th.Reserve()
	if !ok {
		t.Fatal("expected first Reserve() to be granted")
	}
	release(false) // opens a 1s window

	_, ok, retryAfter := th.Reserve()
	if ok {
		t.Fatal("expected Reserve() to block inside the backoff window")
	}
	if retryAfter != time.Second {
		t.Fatalf("retryAfter = %v, want 1s", retryAfter)
	}

	clock.Advance(500 * time.Millisecond)
	_, ok, retryAfter = th.Reserve()
	if ok {
		t.Fatal("expected Reserve() to still block halfway through the window")
	}
	if retryAfter != 500*time.Millisecond {
		t.Fatalf("retryAfter = %v, want 500ms", retryAfter)
	}

	clock.Advance(500 * time.Millisecond)
	release, ok, retryAfter = th.Reserve()
	if !ok || retryAfter != 0 {
		t.Fatalf("Reserve() = (ok=%v, retryAfter=%v) once the window has elapsed, want (true, 0)", ok, retryAfter)
	}
	release(true)
}

func TestThrottleResetOnSuccess(t *testing.T) {
	clock := newMockClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	th := NewThrottle(clock)

	for i := 0; i < 3; i++ {
		release, ok, _ := th.Reserve()
		if !ok {
			t.Fatalf("attempt %d: expected Reserve() to be granted", i)
		}
		release(false)
		clock.Advance(time.Minute) // clear any backoff window before the next attempt
	}

	release, ok, _ := th.Reserve()
	if !ok {
		t.Fatal("expected Reserve() to be granted")
	}
	release(true)

	probeRelease, ok, retryAfter := th.Reserve()
	if !ok || retryAfter != 0 {
		t.Fatalf("Reserve() after success = (ok=%v, retryAfter=%v), want (true, 0)", ok, retryAfter)
	}
	probeRelease(true) // no backoff window was pending, so this stays a success

	// The next failure after a success should restart the sequence at 1s.
	release2, ok2, _ := th.Reserve()
	if !ok2 {
		t.Fatal("expected Reserve() to be granted")
	}
	release2(false)
	_, _, retryAfter3 := th.Reserve()
	if retryAfter3 != time.Second {
		t.Fatalf("retryAfter after success+failure = %v, want 1s", retryAfter3)
	}
}

// TestThrottleReserveSingleFlight is the core atomic-admission guarantee:
// under concurrent Reserve() calls, at most one caller is ever granted the
// slot until it is released. It never calls t.Sleep and relies only on the
// mutex inside Throttle for ordering.
func TestThrottleReserveSingleFlight(t *testing.T) {
	th := NewThrottle(SystemClock{})

	const n = 16
	var wg sync.WaitGroup
	var granted atomic.Int32
	start := make(chan struct{})

	// Pre-grant the first reservation synchronously so the test does not
	// depend on winning a goroutine race for "who gets in first": that part
	// is covered implicitly, but what matters here is that nobody else can
	// get in while the slot is held.
	firstRelease, ok, _ := th.Reserve()
	if !ok {
		t.Fatal("expected the first Reserve() to be granted")
	}
	granted.Add(1)

	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			<-start
			release, ok, _ := th.Reserve()
			if ok {
				granted.Add(1)
				release(false)
			}
		}()
	}
	close(start)
	wg.Wait()

	// Release the held slot only after all concurrent attempts have been
	// evaluated, to guarantee none of them could have observed it as free.
	firstRelease(true)

	if got := granted.Load(); got != 1 {
		t.Fatalf("granted = %d concurrent reservations while one was held, want exactly 1 (the pre-held one)", got)
	}
}
