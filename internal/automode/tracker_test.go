package automode

import (
	"strconv"
	"testing"
	"time"
)

func TestObserveModeMapping(t *testing.T) {
	tracker := NewTracker()
	now := time.Unix(1000, 0)

	cases := []struct {
		name       string
		afk        bool
		safeguards bool
		want       string
	}{
		{"server review", true, true, ModeServer},
		{"local fallback", true, false, ModeLocal},
		{"not auto mode", false, false, ModeOff},
		{"not auto mode with stray safeguards", false, true, ModeOff},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sessionID := "session-" + tc.name
			tracker.Observe(sessionID, tc.afk, tc.safeguards, now)
			state, ok := tracker.Lookup(sessionID)
			if !ok {
				t.Fatalf("Lookup(%q) not found", sessionID)
			}
			if state.Mode != tc.want {
				t.Fatalf("Mode = %q, want %q", state.Mode, tc.want)
			}
			if !state.Since.Equal(now) || !state.Updated.Equal(now) {
				t.Fatalf("Since/Updated = %v/%v, want %v", state.Since, state.Updated, now)
			}
		})
	}
}

func TestObserveSinceTracksModeChangesOnly(t *testing.T) {
	tracker := NewTracker()
	t0 := time.Unix(1000, 0)
	t1 := t0.Add(time.Minute)
	t2 := t1.Add(time.Minute)

	tracker.Observe("sess", true, true, t0) // server
	state, _ := tracker.Lookup("sess")
	if state.Mode != ModeServer || !state.Since.Equal(t0) {
		t.Fatalf("after first observe: %+v", state)
	}

	// One local observation after server is not yet a fallback.
	tracker.Observe("sess", true, false, t1)
	state, _ = tracker.Lookup("sess")
	if state.Mode != ModeServer || !state.Since.Equal(t0) || !state.Updated.Equal(t1) {
		t.Fatalf("after one local observation: %+v, want Mode=%s Since=%v Updated=%v", state, ModeServer, t0, t1)
	}

	// The second in a row confirms it: Since is when the run began.
	tracker.Observe("sess", true, false, t2)
	state, _ = tracker.Lookup("sess")
	if state.Mode != ModeLocal || !state.Since.Equal(t1) || !state.Updated.Equal(t2) {
		t.Fatalf("after confirmed fallback: %+v, want Mode=%s Since=%v Updated=%v", state, ModeLocal, t1, t2)
	}

	// Repeated same mode: Since stays, Updated advances.
	t3 := t2.Add(time.Minute)
	tracker.Observe("sess", true, false, t3)
	state, _ = tracker.Lookup("sess")
	if state.Mode != ModeLocal || !state.Since.Equal(t1) || !state.Updated.Equal(t3) {
		t.Fatalf("after repeated mode: %+v, want Mode=%s Since=%v Updated=%v", state, ModeLocal, t1, t3)
	}
}

func TestObserveSideRequestDoesNotFlipServer(t *testing.T) {
	tracker := NewTracker()
	t0 := time.Unix(1000, 0)

	tracker.Observe("sess", true, true, t0)
	tracker.Observe("sess", true, false, t0.Add(time.Second)) // side request
	tracker.Observe("sess", true, true, t0.Add(2*time.Second))
	tracker.Observe("sess", true, false, t0.Add(3*time.Second)) // another one
	state, _ := tracker.Lookup("sess")
	if state.Mode != ModeServer || !state.Since.Equal(t0) {
		t.Fatalf("interleaved side requests flipped the state: %+v", state)
	}
}

func TestLookupUnknownSession(t *testing.T) {
	tracker := NewTracker()
	if _, ok := tracker.Lookup("missing"); ok {
		t.Fatal("Lookup(missing) found an entry, want none")
	}
}

func TestObserveIgnoresEmptySessionID(t *testing.T) {
	tracker := NewTracker()
	tracker.Observe("", true, true, time.Unix(1000, 0))
	if _, ok := tracker.Lookup(""); ok {
		t.Fatal("Observe with empty sessionID created an entry")
	}
}

func TestPruneByAge(t *testing.T) {
	tracker := NewTracker()
	t0 := time.Unix(1000, 0)
	tracker.Observe("old", true, true, t0)

	// Just under maxAge: still present.
	tracker.Observe("other", true, true, t0.Add(maxAge-time.Second))
	if _, ok := tracker.Lookup("old"); !ok {
		t.Fatal("entry pruned before maxAge elapsed")
	}

	// Past maxAge relative to "old"'s last update: pruned on the next Observe.
	tracker.Observe("other", true, true, t0.Add(maxAge+time.Second))
	if _, ok := tracker.Lookup("old"); ok {
		t.Fatal("stale entry was not pruned")
	}
	if _, ok := tracker.Lookup("other"); !ok {
		t.Fatal("fresh entry was incorrectly pruned")
	}
}

func TestPruneByCapEvictsOldestUpdated(t *testing.T) {
	tracker := NewTracker()
	base := time.Unix(1000, 0)

	for i := 0; i < maxEntries; i++ {
		tracker.Observe("sess-"+strconv.Itoa(i), true, true, base.Add(time.Duration(i)*time.Second))
	}
	if _, ok := tracker.Lookup("sess-0"); !ok {
		t.Fatal("oldest entry missing before the cap was exceeded")
	}

	// One more entry pushes the tracker over the cap; the least-recently-
	// updated entry (sess-0) must be evicted, newer ones kept.
	tracker.Observe("sess-overflow", true, true, base.Add(time.Duration(maxEntries)*time.Second))

	if _, ok := tracker.Lookup("sess-0"); ok {
		t.Fatal("oldest-updated entry was not evicted at cap")
	}
	if _, ok := tracker.Lookup("sess-1"); !ok {
		t.Fatal("next-oldest entry was incorrectly evicted")
	}
	if _, ok := tracker.Lookup("sess-overflow"); !ok {
		t.Fatal("newly observed entry missing after eviction")
	}
}

func TestPackageLevelHelpersUseDefaultTracker(t *testing.T) {
	sessionID := "pkg-level-session-unique-id"
	now := time.Unix(2000, 0)
	Observe(sessionID, true, true, now)
	state, ok := Lookup(sessionID)
	if !ok {
		t.Fatal("Lookup via package-level helper found nothing")
	}
	if state.Mode != ModeServer {
		t.Fatalf("Mode = %q, want %q", state.Mode, ModeServer)
	}
}
