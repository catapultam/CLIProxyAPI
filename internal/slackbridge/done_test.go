package slackbridge

import (
	"reflect"
	"testing"
)

// Task 9: an agent marks a message done once it believes it has fully
// answered it: ✅ replaces whatever receipt was showing, and a later
// Received or Read (a race, a retry) never moves it back.
func TestDoneReplacesEachEarlierState(t *testing.T) {
	b, f, bus := newTestBridge(t)
	root := threadOf(t, b, bus)
	id := queuedID(t, b, root, "40.1", "please rebase")
	for _, step := range []func(){
		func() { b.Received([]string{id}) },
		func() { b.Read([]string{id}) },
	} {
		step()
	}
	drainJobs(t, b)
	if got := f.reactionsOn("CAGENTS", "40.1"); !reflect.DeepEqual(got, []string{reactionRead}) {
		t.Fatalf("before done = %v", got)
	}
	if n := bus.Done(sidA, []string{id}); n != 1 {
		t.Fatalf("Done = %d", n)
	}
	drainJobs(t, b)
	if got := f.reactionsOn("CAGENTS", "40.1"); !reflect.DeepEqual(got, []string{reactionDone}) {
		t.Fatalf("after done = %v", got)
	}
	// A later receipt (a race, a retry) never moves it back.
	b.Received([]string{id})
	b.Read([]string{id})
	drainJobs(t, b)
	if got := f.reactionsOn("CAGENTS", "40.1"); !reflect.DeepEqual(got, []string{reactionDone}) {
		t.Fatalf("after a later read = %v", got)
	}
}

// Task 9: whichever of done and dismiss happens last wins, in either order.
func TestDoneThenDismissShowsNoReaction(t *testing.T) {
	b, f, bus := newTestBridge(t)
	root := threadOf(t, b, bus)
	id := queuedID(t, b, root, "40.2", "please rebase")
	if n := bus.Done(sidA, []string{id}); n != 1 {
		t.Fatalf("Done = %d", n)
	}
	drainJobs(t, b)
	if got := f.reactionsOn("CAGENTS", "40.2"); !reflect.DeepEqual(got, []string{reactionDone}) {
		t.Fatalf("after done = %v", got)
	}
	if n := bus.Dismiss(sidA, []string{id}); n != 1 {
		t.Fatalf("Dismiss = %d", n)
	}
	drainJobs(t, b)
	if got := f.reactionsOn("CAGENTS", "40.2"); len(got) != 0 {
		t.Fatalf("after dismiss = %v", got)
	}
}

func TestDismissThenDoneShowsCheck(t *testing.T) {
	b, f, bus := newTestBridge(t)
	root := threadOf(t, b, bus)
	id := queuedID(t, b, root, "40.3", "please rebase")
	if n := bus.Dismiss(sidA, []string{id}); n != 1 {
		t.Fatalf("Dismiss = %d", n)
	}
	drainJobs(t, b)
	if got := f.reactionsOn("CAGENTS", "40.3"); len(got) != 0 {
		t.Fatalf("after dismiss = %v", got)
	}
	if n := bus.Done(sidA, []string{id}); n != 1 {
		t.Fatalf("Done = %d", n)
	}
	drainJobs(t, b)
	if got := f.reactionsOn("CAGENTS", "40.3"); !reflect.DeepEqual(got, []string{reactionDone}) {
		t.Fatalf("after done = %v", got)
	}
}

// Task 9: a session can mark done only messages delivered to it.
func TestDoneIgnoresForeignIDs(t *testing.T) {
	b, f, bus := newTestBridge(t)
	b.handleEvent("EvDn1", msg("UALEX", "pc/other-bbbbbb: for b", "40.4", ""))
	other := deliveredID(t, bus, sidB)
	drainJobs(t, b)
	if n := bus.Done(sidA, []string{other, "m_00ff", "not-an-id"}); n != 0 {
		t.Fatalf("Done of foreign ids = %d", n)
	}
	drainJobs(t, b)
	if got := f.reactionsOn("CAGENTS", "40.4"); len(got) != 1 || got[0] != reactionQueued {
		t.Fatalf("foreign message's receipt = %v", got)
	}
}
