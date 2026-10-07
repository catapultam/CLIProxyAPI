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

	// Mode changes server -> local: Since moves to the change time.
	tracker.Observe("sess", true, false, t1)
	state, _ = tracker.Lookup("sess")
	if state.Mode != ModeLocal || !state.Since.Equal(t1) {
		t.Fatalf("after mode change: %+v, want Mode=%s Since=%v", state, ModeLocal, t1)
	}
	if !state.Updated.Equal(t1) {
		t.Fatalf("Updated after mode change = %v, want %v", state.Updated, t1)
	}

	// Repeated same mode: Since stays at the change time, Updated advances.
	tracker.Observe("sess", true, false, t2)
	state, _ = tracker.Lookup("sess")
	if state.Mode != ModeLocal || !state.Since.Equal(t1) {
		t.Fatalf("after repeated mode: %+v, want Mode=%s Since=%v", state, ModeLocal, t1)
	}
	if !state.Updated.Equal(t2) {
		t.Fatalf("Updated after repeated mode = %v, want %v", state.Updated, t2)
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
