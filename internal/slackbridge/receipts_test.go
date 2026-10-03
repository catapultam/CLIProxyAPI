package slackbridge

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
)

// reactionCalls lists every reactions.add ("+name") and reactions.remove
// ("-name") on channel/ts, in order.
func reactionCalls(f *fakeSlack, channel, ts string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		if c.Form.Get("channel") != channel || c.Form.Get("timestamp") != ts {
			continue
		}
		switch c.Method {
		case "reactions.add":
			out = append(out, "+"+c.Form.Get("name"))
		case "reactions.remove":
			out = append(out, "-"+c.Form.Get("name"))
		}
	}
	return out
}

// queuedID delivers text from alex into sidA's thread as message ts and
// returns the bus message id.
func queuedID(t *testing.T, b *Bridge, root, ts, text string) string {
	t.Helper()
	b.handleEvent("Ev"+ts, msg("UALEX", text, ts, root))
	msgs := b.bus.Claim(sidA)
	if len(msgs) != 1 {
		t.Fatalf("msgs = %+v", msgs)
	}
	drainJobs(t, b)
	return msgs[0].ID
}

func TestReceiptsQueuedReceivedReadLeavesOnlyEyes(t *testing.T) {
	b, f, bus := newTestBridge(t)
	root := threadOf(t, b, bus)
	id := queuedID(t, b, root, "20.1", "please rebase")
	b.Received([]string{id})
	drainJobs(t, b)
	b.Read([]string{id})
	drainJobs(t, b)
	want := []string{"+inbox_tray", "+envelope_with_arrow", "-inbox_tray", "+eyes", "-envelope_with_arrow"}
	if got := reactionCalls(f, "CAGENTS", "20.1"); !reflect.DeepEqual(got, want) {
		t.Fatalf("reactions = %v", got)
	}
	if got := f.reactionsOn("CAGENTS", "20.1"); !reflect.DeepEqual(got, []string{"eyes"}) {
		t.Fatalf("left on the message = %v", got)
	}
}

func TestReceiptsInjectGoesFromQueuedToRead(t *testing.T) {
	b, f, bus := newTestBridge(t)
	root := threadOf(t, b, bus)
	id := queuedID(t, b, root, "20.2", "check logs")
	b.Read([]string{id})
	drainJobs(t, b)
	want := []string{"+inbox_tray", "+eyes", "-inbox_tray"}
	if got := reactionCalls(f, "CAGENTS", "20.2"); !reflect.DeepEqual(got, want) {
		t.Fatalf("reactions = %v", got)
	}
}

func TestReceiptsNeverMoveBackwards(t *testing.T) {
	b, f, bus := newTestBridge(t)
	root := threadOf(t, b, bus)
	id := queuedID(t, b, root, "20.3", "ship it")
	b.Read([]string{id})
	drainJobs(t, b)
	before := reactionCalls(f, "CAGENTS", "20.3")
	b.Received([]string{id})
	b.Read([]string{id, id})
	drainJobs(t, b)
	if got := reactionCalls(f, "CAGENTS", "20.3"); !reflect.DeepEqual(got, before) {
		t.Fatalf("late receipts changed reactions: %v -> %v", before, got)
	}
}

func TestReceiptsReplaceTheCommandGear(t *testing.T) {
	b, f, bus, _ := newCommandBridge(t)
	root := threadOf(t, b, bus)
	b.handleEvent("EvR4", msg("UALEX", "!compact", "20.4", root))
	msgs := bus.Claim(sidA)
	if len(msgs) != 1 || msgs[0].Command == nil {
		t.Fatalf("msgs = %+v", msgs)
	}
	drainJobs(t, b)
	b.Received([]string{msgs[0].ID})
	b.Read([]string{msgs[0].ID})
	drainJobs(t, b)
	want := []string{"+gear", "+envelope_with_arrow", "-gear", "+eyes", "-envelope_with_arrow"}
	if got := reactionCalls(f, "CAGENTS", "20.4"); !reflect.DeepEqual(got, want) {
		t.Fatalf("reactions = %v", got)
	}
}

func TestReceiptsInDM(t *testing.T) {
	b, f, bus := newTestBridge(t)
	b.handleEvent("EvR5", dmMsg("UALEX", "flyer: status?", "20.5", ""))
	msgs := bus.Claim(sidA)
	if len(msgs) != 1 {
		t.Fatalf("msgs = %+v", msgs)
	}
	drainJobs(t, b)
	b.Received([]string{msgs[0].ID})
	b.Read([]string{msgs[0].ID})
	drainJobs(t, b)
	if got := f.reactionsOn("DUALEX", "20.5"); !reflect.DeepEqual(got, []string{"eyes"}) {
		t.Fatalf("left on the DM = %v (calls %v)", got, reactionCalls(f, "DUALEX", "20.5"))
	}
}

func TestReceiptsIgnoreUnknownAndOldRecords(t *testing.T) {
	b, f, _ := newTestBridge(t)
	// A record from before receipts has no ts.
	b.state.record(replyRecord{ID: "m_0ld", Channel: "CAGENTS", ThreadTS: "1.1", Session: sidA})
	b.Received([]string{"m_0ld", "m_dead", ""})
	b.Read([]string{"m_0ld"})
	drainJobs(t, b)
	if n := len(f.callsTo("reactions.add")) + len(f.callsTo("reactions.remove")); n != 0 {
		t.Fatalf("%d reaction calls for unknown ids", n)
	}
}

// The full path: the bus reports the mod's /wait claim and its /ack.
func TestReceiptsThroughTheBus(t *testing.T) {
	b, f, bus, _ := newCommandBridge(t)
	root := threadOf(t, b, bus)
	b.handleEvent("EvR7", msg("UALEX", "run the tests", "20.7", root))
	drainJobs(t, b)
	msgs := bus.ClaimForWait(sidA, "0.3.4")
	if len(msgs) != 1 {
		t.Fatalf("msgs = %+v", msgs)
	}
	drainJobs(t, b)
	if got := f.reactionsOn("CAGENTS", "20.7"); !reflect.DeepEqual(got, []string{"envelope_with_arrow"}) {
		t.Fatalf("after /wait = %v", got)
	}
	if n := bus.Ack(sidB, []string{msgs[0].ID}); n != 0 {
		t.Fatal("another session acknowledged the message")
	}
	if n := bus.Ack(sidA, []string{msgs[0].ID}); n != 1 {
		t.Fatal("ack not counted")
	}
	drainJobs(t, b)
	if got := f.reactionsOn("CAGENTS", "20.7"); !reflect.DeepEqual(got, []string{"eyes"}) {
		t.Fatalf("after /ack = %v", got)
	}
}

// A receipt can beat the bridge's own record of the delivery (the waiter
// claims the message the moment the bus queues it); the delivery then shows
// the receipt instead of the queued reaction.
func TestReceiptBeforeRecordIsKept(t *testing.T) {
	st, err := loadState("")
	if err != nil {
		t.Fatal(err)
	}
	if changes := st.advanceReceipts([]string{"m_ea"}, reactionReceived); len(changes) != 0 {
		t.Fatalf("changes = %+v", changes)
	}
	if got := st.record(replyRecord{ID: "m_ea", Channel: "C", TS: "1.2", Session: "s", Receipt: reactionQueued}); got != reactionReceived {
		t.Fatalf("record = %q", got)
	}
	if changes := st.advanceReceipts([]string{"m_ea"}, reactionRead); !reflect.DeepEqual(changes, []receiptChange{{channel: "C", ts: "1.2", add: reactionRead, remove: reactionReceived}}) {
		t.Fatalf("changes = %+v", changes)
	}
	// An early receipt is used once.
	if got := st.record(replyRecord{ID: "m_eb", Channel: "C", TS: "1.3", Session: "s", Receipt: reactionQueued}); got != reactionQueued {
		t.Fatalf("record = %q", got)
	}
}

func TestReceiptsArePersisted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "slack-state.json")
	st, _ := loadState(path)
	st.record(replyRecord{ID: "m_aa", Channel: "CAGENTS", ThreadTS: "1.1", TS: "1.5", Session: "s", Receipt: reactionQueued})
	st.advanceReceipts([]string{"m_aa"}, reactionRead)
	loaded, errLoad := loadState(path)
	if errLoad != nil {
		t.Fatal(errLoad)
	}
	if changes := loaded.advanceReceipts([]string{"m_aa"}, reactionReceived); len(changes) != 0 {
		t.Fatalf("a receipt moved back after a reload: %+v", changes)
	}
	loaded.mu.Lock()
	r, _ := loaded.replyLocked("m_aa")
	loaded.mu.Unlock()
	if r.TS != "1.5" || r.Receipt != reactionRead {
		t.Fatalf("loaded record = %+v", r)
	}
}

func TestRemoveReactionIgnoresNoReaction(t *testing.T) {
	f := newFakeSlack(t)
	a := newAPI(f.apiBase())
	if err := a.removeReaction(context.Background(), "x", "C", "1.1", "inbox_tray"); err != nil {
		t.Fatalf("no_reaction: %v", err)
	}
	f.setFail("reactions.remove", "message_not_found")
	if err := a.removeReaction(context.Background(), "x", "C", "1.1", "inbox_tray"); err == nil {
		t.Fatal("message_not_found ignored")
	}
}
