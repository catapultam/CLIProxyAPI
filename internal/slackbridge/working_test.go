package slackbridge

import (
	"reflect"
	"testing"
)

// Task 9 addendum: a turn still running on a message, 15s after it started,
// shows ⏳; it sits between received and read in the normal advancing flow,
// so it never moves a receipt back (a late Received call after Read, or
// after Working itself, changes nothing), and Read still moves it on.
func TestWorkingSitsBetweenReceivedAndRead(t *testing.T) {
	b, f, bus := newTestBridge(t)
	root := threadOf(t, b, bus)
	id := queuedID(t, b, root, "41.1", "please rebase")
	b.Received([]string{id})
	drainJobs(t, b)
	if got := f.reactionsOn("CAGENTS", "41.1"); !reflect.DeepEqual(got, []string{reactionReceived}) {
		t.Fatalf("after received = %v", got)
	}
	if n := bus.Working(sidA, []string{id}); n != 1 {
		t.Fatalf("Working = %d", n)
	}
	drainJobs(t, b)
	if got := f.reactionsOn("CAGENTS", "41.1"); !reflect.DeepEqual(got, []string{reactionWorking}) {
		t.Fatalf("after working = %v", got)
	}
	// A late, redundant Received does nothing: working never moves back.
	b.Received([]string{id})
	drainJobs(t, b)
	if got := f.reactionsOn("CAGENTS", "41.1"); !reflect.DeepEqual(got, []string{reactionWorking}) {
		t.Fatalf("after a late received = %v", got)
	}
	b.Read([]string{id})
	drainJobs(t, b)
	if got := f.reactionsOn("CAGENTS", "41.1"); !reflect.DeepEqual(got, []string{reactionRead}) {
		t.Fatalf("after read = %v", got)
	}
	// Working after read (a slow /working call) never moves it back either.
	if n := bus.Working(sidA, []string{id}); n != 0 {
		t.Fatalf("Working after read = %d", n)
	}
	drainJobs(t, b)
	if got := f.reactionsOn("CAGENTS", "41.1"); !reflect.DeepEqual(got, []string{reactionRead}) {
		t.Fatalf("after a late working = %v", got)
	}
}

// Working never overrides a terminal done or dismissed.
func TestWorkingNeverOverridesDoneOrDismissed(t *testing.T) {
	b, f, bus := newTestBridge(t)
	root := threadOf(t, b, bus)
	id := queuedID(t, b, root, "41.2", "please rebase")
	if n := bus.Done(sidA, []string{id}); n != 1 {
		t.Fatalf("Done = %d", n)
	}
	drainJobs(t, b)
	if n := bus.Working(sidA, []string{id}); n != 0 {
		t.Fatalf("Working after done = %d", n)
	}
	drainJobs(t, b)
	if got := f.reactionsOn("CAGENTS", "41.2"); !reflect.DeepEqual(got, []string{reactionDone}) {
		t.Fatalf("after working = %v", got)
	}

	id2 := queuedID(t, b, root, "41.3", "please rebase again")
	if n := bus.Dismiss(sidA, []string{id2}); n != 1 {
		t.Fatalf("Dismiss = %d", n)
	}
	drainJobs(t, b)
	if n := bus.Working(sidA, []string{id2}); n != 0 {
		t.Fatalf("Working after dismiss = %d", n)
	}
	drainJobs(t, b)
	if got := f.reactionsOn("CAGENTS", "41.3"); len(got) != 0 {
		t.Fatalf("after working = %v", got)
	}
}

// Task 9 addendum: a session can mark working only messages delivered to it.
func TestWorkingIgnoresForeignIDs(t *testing.T) {
	b, f, bus := newTestBridge(t)
	b.handleEvent("EvWk1", msg("UALEX", "pc/other-bbbbbb: for b", "41.4", ""))
	other := deliveredID(t, bus, sidB)
	drainJobs(t, b)
	if n := bus.Working(sidA, []string{other, "m_00ff", "not-an-id"}); n != 0 {
		t.Fatalf("Working of foreign ids = %d", n)
	}
	drainJobs(t, b)
	if got := f.reactionsOn("CAGENTS", "41.4"); len(got) != 1 || got[0] != reactionQueued {
		t.Fatalf("foreign message's receipt = %v", got)
	}
}
