package agentbus

import "testing"

// handoffMarkBridge is a movingBridge that also answers Dismissed and Done,
// counting every id it's given: this test is about the Store's own
// handoff-following and Unacked-clearing, not about a bridge's ownership
// check (covered separately in the slackbridge package).
type handoffMarkBridge struct {
	movingBridge
}

func (h *handoffMarkBridge) Dismissed(_ string, ids []string) int { return len(ids) }
func (h *handoffMarkBridge) Done(_ string, ids []string) int      { return len(ids) }

// M6: Dismiss (and Done, the same code path) follows a handoff the way Ack
// does, clearing Unacked at the successor under the id the mod captured,
// so a later /ack never double-reports it.
func TestDismissFollowsHandoffAndClearsUnacked(t *testing.T) {
	s, _ := newTestStore(t)
	b := &handoffMarkBridge{movingBridge: movingBridge{fakeReceiptBridge: fakeReceiptBridge{fakeOwnerBridge: fakeOwnerBridge{fakeBridge: fakeBridge{users: []string{"alex"}}}}}}
	b.store = s
	s.SetBridge(b)
	capableSession(s, sidA, "/work/flyer", "flyer")
	s.SetModVersion(sidA, MinAckModVersion)
	_, handed, err := s.DeliverVia(sidA, "first", "alex", "")
	if err != nil {
		t.Fatal(err)
	}
	if got := s.ClaimForWait(sidA, true, MinAckModVersion); len(got) != 1 {
		t.Fatalf("claimed %+v", got)
	}
	s.Bye(sidA)
	s.Hello(sidC, "pc", "", "", true)
	if err := s.HandOff(sidA, sidC); err != nil {
		t.Fatalf("HandOff = %v", err)
	}
	// Dismiss under the old id (the mod captured it); it still counts, by following the
	// handoff the same way Ack does.
	if n := s.Dismiss(sidA, []string{handed}); n != 1 {
		t.Fatalf("Dismiss through the old id counted %d", n)
	}
	// It was cleared from the successor's Unacked too, so a later /ack never double-reports it.
	if n := s.Ack(sidC, []string{handed}); n != 0 {
		t.Fatalf("a dismissed id was still ack-able under the new session: %d", n)
	}
}
