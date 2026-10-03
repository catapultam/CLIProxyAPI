package slackbridge

import (
	"testing"
	"time"
)

// A conversation that stays quiet for over linkAbsentTTL keeps its link
// while its session is live on the bus: liveness comes from the bus, not
// from the conversation's own traffic.
func TestQuietLinkKeptWhileSessionLive(t *testing.T) {
	clock := newTestClock()
	b, _, bus := newClockBridge(t, clock)
	linkGroup(t, b, bus, sidA, "flyer", "Q1")

	clock.advance(8 * 24 * time.Hour)
	bus.Touch(sidA)
	// Any save must not prune a link whose session is live.
	b.state.setDMLast("UALEX", sidA)
	b.handleEvent("EvQ2", foreignMsg("UBOB", "anyone there?", "1700005000.000002", ""))
	drainJobs(t, b)
	if m := claimOne(t, bus, sidA); m.Body != "anyone there?" || !m.Guest {
		t.Fatalf("msg = %+v", m)
	}
	if _, ok := b.state.conversation("GMPIM1"); !ok {
		t.Fatal("live session's link was dropped")
	}
}

// A link whose session has been absent from the bus for over linkAbsentTTL
// is pruned by the bridge's maintenance pass, and the next save leaves it
// out of the file.
func TestLinkOfAbsentSessionPrunedByMaintenance(t *testing.T) {
	clock := newTestClock()
	b, _, bus := newClockBridge(t, clock)
	linkGroup(t, b, bus, sidA, "flyer", "Q3")

	clock.advance(8 * 24 * time.Hour)
	b.maintain()
	if _, ok := b.state.conversation("GMPIM1"); ok {
		t.Fatal("absent session's link survived maintenance")
	}
	if err := b.state.flush(); err != nil {
		t.Fatal(err)
	}
	reloaded, err := loadState(b.cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, present := reloaded.convs["GMPIM1"]; present {
		t.Fatal("pruned link still saved")
	}
}
