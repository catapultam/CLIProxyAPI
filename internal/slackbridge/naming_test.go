package slackbridge

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// leaks reports what in text discloses sidB's setup: its address, folder,
// machine or session id.
func leaks(text string) []string {
	var out []string
	for _, s := range []string{"pc/", "other-bbbbbb", " · pc", sidB, sidB[:8]} {
		if strings.Contains(text, s) {
			out = append(out, s)
		}
	}
	return out
}

// The bridge names an unnamed agent "an agent" where non-owners read: no
// address, machine or session id.
func TestUnnamedAgentIsNamedPubliclyForNonOwners(t *testing.T) {
	clock := newTestClock()
	b, f, bus := newClockBridge(t, clock)
	bus.Hello(sidB, "pc", "/work/other", "", true)
	if _, _, err := b.state.allow("UJANE", "jane"); err != nil {
		t.Fatal(err)
	}
	// The unnamed agent DMs jane, so her plain DMs go to it.
	sendDM(t, b, bus, sidB, "jane", "hello jane")
	b.handleEvent("EvNm1", dmMsg("UJANE", "thanks", "1700011000.000001", ""))
	_ = claimOne(t, bus, sidB)
	drainJobs(t, b)
	if got := lastPost(t, f); got["text"] != "→ sent to `an agent`" || len(leaks(got["text"])) > 0 {
		t.Fatalf("dm_last reply = %+v", got)
	}
	// Offline: a top-level "address:" from jane still waits, and says so.
	clock.advance(time.Hour)
	b.handleEvent("EvNm2", dmMsg("UJANE", "pc/other-bbbbbb: later", "1700011000.000002", ""))
	_ = claimOne(t, bus, sidB)
	drainJobs(t, b)
	if got := lastPost(t, f); got["text"] != "`an agent` is offline; it gets this when it's back." {
		t.Fatalf("offline reply = %+v", got)
	}
	for _, p := range postsTo(f, "DUJANE") {
		if l := leaks(p["text"]); len(l) > 0 {
			t.Fatalf("leaked %v into jane's DM: %q", l, p["text"])
		}
	}
}

// In a group, the in-thread tag reply and the "can't run commands" reply
// name an unnamed agent publicly too.
func TestUnnamedAgentIsNamedPubliclyInGroups(t *testing.T) {
	b, f, bus := newTestBridge(t)
	b.handleEvent("EvNg1", foreignMsg("UALEX", "pc/other-bbbbbb: look", "1700011100.000002", "1700011100.000001"))
	_ = claimOne(t, bus, sidB)
	drainJobs(t, b)
	if got := lastPost(t, f); got["text"] != "→ sent to `an agent`" {
		t.Fatalf("tag reply = %+v", got)
	}
	b.handleEvent("EvNg2", foreignMsg("UALEX", "pc/other-bbbbbb: !compact", "1700011100.000003", ""))
	drainJobs(t, b)
	if got := lastPost(t, f); got["text"] != fmt.Sprintf("`an agent` can't run commands (agentbus plugin %s+ required)", "0.3.3") {
		t.Fatalf("can't-run reply = %+v", got)
	}
	for _, p := range postsTo(f, "GMPIM1") {
		if l := leaks(p["text"]); len(l) > 0 {
			t.Fatalf("leaked %v into the group: %q", l, p["text"])
		}
	}
}

// In an owner's DM the address still shows.
func TestOwnerDMStillNamesAgentsByAddress(t *testing.T) {
	b, f, bus := newTestBridge(t)
	sendDM(t, b, bus, sidB, "alex", "hello alex")
	b.handleEvent("EvNo1", dmMsg("UALEX", "thanks", "1700011200.000001", ""))
	_ = claimOne(t, bus, sidB)
	drainJobs(t, b)
	if got := lastPost(t, f); got["text"] != "→ sent to `pc/other-bbbbbb`" {
		t.Fatalf("owner dm_last reply = %+v", got)
	}
}

// The unlink notice tells the agent its answers there are dropped.
func TestUnlinkNoticeSaysAnswersAreDropped(t *testing.T) {
	b, _, bus := newTestBridge(t)
	linkGroup(t, b, bus, sidA, "flyer", "Nu1")
	b.handleEvent("EvNu2", foreignMsg("UALEX", "<@UBOT> unlink", "1700011300.000002", ""))
	drainJobs(t, b)
	n := claimNotice(t, bus, sidA)
	if !strings.Contains(n.Body, "answers to it are dropped") || strings.Contains(n.Body, "own thread") {
		t.Fatalf("notice = %q", n.Body)
	}
}

// The reply ring holds 10000 deliveries, so a guest flood doesn't push a
// private record out before its answer comes.
func TestReplyRingHoldsTenThousand(t *testing.T) {
	st, err := loadState("")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10000; i++ {
		st.replies = append(st.replies, replyRecord{ID: replyID(i), Channel: "D1", Session: "s", At: st.now()})
	}
	st.record(replyRecord{ID: "m_ffffff", Channel: "D1", Session: "s"})
	if _, ok := st.replyTarget(replyID(1), "s"); !ok {
		t.Fatal("record 1 of 10001 already dropped")
	}
	if _, ok := st.replyTarget(replyID(0), "s"); ok {
		t.Fatal("ring not capped")
	}
}
