package slackbridge

import (
	"reflect"
	"testing"
)

// Task 9 fix round 1 (C1): working and read are peers below done and
// dismissed: a turn still running 15s after it started shows ⏳; the turn
// completing shows 👀; a later explicit "working" (a long task that
// continues) shows ⏳ again; done finishes it for good.
func TestReceiptSequenceWorkingReadWorkingDone(t *testing.T) {
	b, f, bus := newTestBridge(t)
	root := threadOf(t, b, bus)
	id := queuedID(t, b, root, "41.1", "please rebase")
	b.Received([]string{id})
	drainJobs(t, b)
	if got := f.reactionsOn("CAGENTS", "41.1"); !reflect.DeepEqual(got, []string{reactionReceived}) {
		t.Fatalf("after received = %v", got)
	}
	// The 15s timer: ⏳.
	if n := bus.Working(sidA, []string{id}); n != 1 {
		t.Fatalf("Working = %d", n)
	}
	drainJobs(t, b)
	if got := f.reactionsOn("CAGENTS", "41.1"); !reflect.DeepEqual(got, []string{reactionWorking}) {
		t.Fatalf("after working = %v", got)
	}
	// The turn completes: /ack moves it to 👀.
	b.Read([]string{id})
	drainJobs(t, b)
	if got := f.reactionsOn("CAGENTS", "41.1"); !reflect.DeepEqual(got, []string{reactionRead}) {
		t.Fatalf("after read = %v", got)
	}
	// The task continues in a later turn: explicit "working" moves it back to ⏳. Working and
	// read are peers, so this is allowed, unlike a plain Received trying to move it back.
	if n := bus.Working(sidA, []string{id}); n != 1 {
		t.Fatalf("Working after read = %d", n)
	}
	drainJobs(t, b)
	if got := f.reactionsOn("CAGENTS", "41.1"); !reflect.DeepEqual(got, []string{reactionWorking}) {
		t.Fatalf("after working again = %v", got)
	}
	// A late, redundant Received never moves it back below working/read.
	b.Received([]string{id})
	drainJobs(t, b)
	if got := f.reactionsOn("CAGENTS", "41.1"); !reflect.DeepEqual(got, []string{reactionWorking}) {
		t.Fatalf("after a late received = %v", got)
	}
	// Done finishes it for good.
	if n := bus.Done(sidA, []string{id}); n != 1 {
		t.Fatalf("Done = %d", n)
	}
	drainJobs(t, b)
	if got := f.reactionsOn("CAGENTS", "41.1"); !reflect.DeepEqual(got, []string{reactionDone}) {
		t.Fatalf("after done = %v", got)
	}
}

// Working counts an id as owned (like Done and Dismiss do) even when the record is already done:
// the state doesn't move, but the id was still delivered to this session.
func TestWorkingAfterDoneCountsButStaysDone(t *testing.T) {
	b, f, bus := newTestBridge(t)
	root := threadOf(t, b, bus)
	id := queuedID(t, b, root, "41.2", "please rebase")
	if n := bus.Done(sidA, []string{id}); n != 1 {
		t.Fatalf("Done = %d", n)
	}
	drainJobs(t, b)
	if n := bus.Working(sidA, []string{id}); n != 1 {
		t.Fatalf("Working after done = %d", n)
	}
	drainJobs(t, b)
	if got := f.reactionsOn("CAGENTS", "41.2"); !reflect.DeepEqual(got, []string{reactionDone}) {
		t.Fatalf("after working = %v", got)
	}
}

// Working after dismiss counts (owned, unexpired), and dismiss still wins: no reaction.
func TestWorkingAfterDismissShowsNoReaction(t *testing.T) {
	b, f, bus := newTestBridge(t)
	root := threadOf(t, b, bus)
	id := queuedID(t, b, root, "41.3", "please rebase again")
	if n := bus.Dismiss(sidA, []string{id}); n != 1 {
		t.Fatalf("Dismiss = %d", n)
	}
	drainJobs(t, b)
	if n := bus.Working(sidA, []string{id}); n != 1 {
		t.Fatalf("Working after dismiss = %d", n)
	}
	drainJobs(t, b)
	if got := f.reactionsOn("CAGENTS", "41.3"); len(got) != 0 {
		t.Fatalf("after working = %v", got)
	}
}

// Dismiss after working removes ⏳: dismiss still clears everything.
func TestDismissAfterWorkingShowsNoReaction(t *testing.T) {
	b, f, bus := newTestBridge(t)
	root := threadOf(t, b, bus)
	id := queuedID(t, b, root, "41.5", "please rebase")
	if n := bus.Working(sidA, []string{id}); n != 1 {
		t.Fatalf("Working = %d", n)
	}
	drainJobs(t, b)
	if got := f.reactionsOn("CAGENTS", "41.5"); !reflect.DeepEqual(got, []string{reactionWorking}) {
		t.Fatalf("after working = %v", got)
	}
	if n := bus.Dismiss(sidA, []string{id}); n != 1 {
		t.Fatalf("Dismiss = %d", n)
	}
	drainJobs(t, b)
	if got := f.reactionsOn("CAGENTS", "41.5"); len(got) != 0 {
		t.Fatalf("after dismiss = %v", got)
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
