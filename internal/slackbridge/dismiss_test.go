package slackbridge

import (
	"context"
	"testing"
)

// Item 9: an agent dismisses a message that wasn't meant for it: every
// receipt reaction comes off, and none comes back.
func TestDismissClearsReceiptsForGood(t *testing.T) {
	b, f, bus := newTestBridge(t)
	root := threadOf(t, b, bus)
	id := queuedID(t, b, root, "30.1", "talking to bob, not you")
	b.Received([]string{id})
	drainJobs(t, b)
	if got := f.reactionsOn("CAGENTS", "30.1"); len(got) != 1 || got[0] != reactionReceived {
		t.Fatalf("before = %v", got)
	}
	if n := bus.Dismiss(sidA, []string{id}); n != 1 {
		t.Fatalf("Dismiss = %d", n)
	}
	drainJobs(t, b)
	if got := f.reactionsOn("CAGENTS", "30.1"); len(got) != 0 {
		t.Fatalf("after dismiss = %v", got)
	}
	// A later receipt (the mod's ack, an injection) never re-adds one.
	b.Received([]string{id})
	b.Read([]string{id})
	drainJobs(t, b)
	if got := f.reactionsOn("CAGENTS", "30.1"); len(got) != 0 {
		t.Fatalf("after a later read = %v", got)
	}
}

// Item 9: a session can dismiss only messages delivered to it.
func TestDismissIgnoresForeignIDs(t *testing.T) {
	b, f, bus := newTestBridge(t)
	b.handleEvent("EvDs1", msg("UALEX", "pc/other-bbbbbb: for b", "30.2", ""))
	other := deliveredID(t, bus, sidB)
	drainJobs(t, b)
	if n := bus.Dismiss(sidA, []string{other, "m_00ff", "not-an-id"}); n != 0 {
		t.Fatalf("Dismiss of foreign ids = %d", n)
	}
	drainJobs(t, b)
	if got := f.reactionsOn("CAGENTS", "30.2"); len(got) != 1 || got[0] != reactionQueued {
		t.Fatalf("foreign message's receipt = %v", got)
	}
}

// Minor: receipt jobs that run out of order (a retry, a slow queue) apply
// only the latest state, so no stale reaction is left behind.
func TestReceiptJobsApplyOnlyTheLatestState(t *testing.T) {
	b, f, bus := newTestBridge(t)
	root := threadOf(t, b, bus)
	b.handleEvent("EvRace", msg("UALEX", "quick", "30.3", root))
	id := deliveredID(t, bus, sidA)
	b.Received([]string{id})
	b.Read([]string{id})
	// Run the queued jobs newest first.
	var jobs []job
	for {
		select {
		case j := <-b.jobs:
			jobs = append(jobs, j)
			continue
		default:
		}
		break
	}
	for i := len(jobs) - 1; i >= 0; i-- {
		if err := jobs[i](context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if got := f.reactionsOn("CAGENTS", "30.3"); len(got) != 1 || got[0] != reactionRead {
		t.Fatalf("reactions = %v, want only %s", got, reactionRead)
	}
}
