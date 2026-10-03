package slackbridge

import (
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/agentbus"
)

const sidC = "cccccc11-2222-3333-4444-555555555555"

// postsSince returns the chat.postMessage forms after the first n.
func postsSince(f *fakeSlack, n int) []map[string]string {
	var out []map[string]string
	for i, c := range f.callsTo("chat.postMessage") {
		if i >= n {
			out = append(out, map[string]string{"channel": c.Form.Get("channel"), "text": c.Form.Get("text"), "thread_ts": c.Form.Get("thread_ts")})
		}
	}
	return out
}

// Item 2: a closed session gets nothing queued; the thread says it ended.
func TestThreadReplyToClosedSessionSaysEnded(t *testing.T) {
	b, f, bus := newTestBridge(t)
	root := threadOf(t, b, bus)
	bus.Bye(sidA)
	b.handleEvent("EvEnd1", msg("UALEX", "you there?", "1700000900.000001", root))
	b.handleEvent("EvEnd2", msg("UALEX", "!compact", "1700000900.000002", root))
	drainJobs(t, b)
	if bus.Pending(sidA) {
		t.Fatalf("queued for a closed session: %+v", bus.Claim(sidA))
	}
	posts := postsSince(f, 1)
	if len(posts) != 2 || posts[0]["text"] != sessionEnded || posts[1]["text"] != sessionEnded {
		t.Fatalf("replies = %+v", posts)
	}
}

// Item 2: after a handoff (/clear, /resume, /branch), the name, inbox,
// thread, DM routing and links follow the new session.
func TestHandOffMovesSlackRoutingToTheNewSession(t *testing.T) {
	b, f, bus := newTestBridge(t)
	root := threadOf(t, b, bus)
	b.handleEvent("EvHo1", dmMsg("UALEX", "flyer: hi", "1700000800.000001", ""))
	_ = deliveredID(t, bus, sidA)
	linkGroup(t, b, bus, sidA, "flyer", "Ho2")
	drainJobs(t, b)

	bus.Bye(sidA)
	bus.Hello(sidC, "pc", "", "", true)
	if err := bus.HandOff(sidA, sidC); err != nil {
		t.Fatalf("HandOff = %v", err)
	}
	if id, ok := bus.Resolve("flyer"); !ok || id != sidC {
		t.Fatalf("flyer = %q %v", id, ok)
	}
	// A Slack thread reply reaches the new session.
	b.handleEvent("EvHo3", msg("UALEX", "still there?", "1700000800.000003", root))
	if m := claimOne(t, bus, sidC); m.Body != "still there?" {
		t.Fatalf("thread reply = %+v", m)
	}
	// So does a plain DM (dm_last).
	b.handleEvent("EvHo4", dmMsg("UALEX", "and here?", "1700000800.000004", ""))
	if m := claimOne(t, bus, sidC); m.Body != "and here?" {
		t.Fatalf("dm = %+v", m)
	}
	// And a guest in the linked conversation.
	b.handleEvent("EvHo5", foreignMsg("UBOB", "hello", "1700000800.000005", ""))
	drainJobs(t, b)
	guest := claimOne(t, bus, sidC)
	if !guest.Guest {
		t.Fatalf("guest = %+v", guest)
	}
	// The new session posts into the old thread, and its answer to the
	// guest lands in the linked conversation.
	post(t, b, bus, sidC, "back")
	if got := lastPost(t, f); got["channel"] != "CAGENTS" || got["thread_ts"] != root || got["text"] != "back" {
		t.Fatalf("post = %+v", got)
	}
	sendReply(t, b, bus, sidC, "hi bob", guest.ID)
	if got := lastPost(t, f); got["channel"] != "GMPIM1" || !strings.HasSuffix(got["text"], "hi bob") {
		t.Fatalf("answer to guest = %+v", got)
	}
	if bus.Pending(sidA) {
		t.Fatalf("the old session got: %+v", bus.Claim(sidA))
	}
}

// Item 4: an answer to a guest after the conversation was unlinked is
// dropped (never posted in the home thread), and the agent is told.
func TestAnswerToUnlinkedConversationIsDroppedWithNotice(t *testing.T) {
	b, f, bus := newTestBridge(t)
	linkGroup(t, b, bus, sidA, "flyer", "Un1")
	b.handleEvent("EvUn2", foreignMsg("UBOB", "anyone?", "1700004400.000002", ""))
	drainJobs(t, b)
	guest := claimOne(t, bus, sidA)
	b.handleEvent("EvUn3", foreignMsg("UALEX", "<@UBOT> unlink", "1700004400.000003", ""))
	drainJobs(t, b)
	_ = claimNotice(t, bus, sidA)
	before := len(f.callsTo("chat.postMessage"))
	sendReply(t, b, bus, sidA, "still here?", guest.ID)
	if got := postsSince(f, before); len(got) != 0 {
		t.Fatalf("posted after unlink: %+v", got)
	}
	if n := claimNotice(t, bus, sidA); !strings.Contains(n.Body, "no longer reachable") {
		t.Fatalf("notice = %+v", n)
	}
}

// Item 5: a plain top-level DM routed by dm_last says where it went, in a
// thread under it.
func TestDMLastRoutingSaysWhereItWent(t *testing.T) {
	b, f, bus := newTestBridge(t)
	b.handleEvent("EvDl1", dmMsg("UALEX", "flyer: hi", "1700000600.000001", ""))
	_ = deliveredID(t, bus, sidA)
	drainJobs(t, b)
	before := len(f.callsTo("chat.postMessage"))
	b.handleEvent("EvDl2", dmMsg("UALEX", "and another thing", "1700000600.000002", ""))
	if m := claimOne(t, bus, sidA); m.Body != "and another thing" {
		t.Fatalf("msg = %+v", m)
	}
	drainJobs(t, b)
	got := postsSince(f, before)
	if len(got) != 1 || got[0]["channel"] != "DUALEX" || got[0]["thread_ts"] != "1700000600.000002" || got[0]["text"] != "→ sent to `flyer`" {
		t.Fatalf("posts = %+v", got)
	}
}

// Item 5: a bare top-level !command in a DM needs "name: !cmd"; dm_last
// never carries a command.
func TestBareTopLevelCommandInDMIsNotSentToDMLast(t *testing.T) {
	b, f, bus := newTestBridge(t)
	bus.SetModVersion(sidA, agentbus.MinCommandModVersion)
	b.handleEvent("EvBc1", dmMsg("UALEX", "flyer: hi", "1700000610.000001", ""))
	_ = deliveredID(t, bus, sidA)
	drainJobs(t, b)
	before := len(f.callsTo("chat.postMessage"))
	b.handleEvent("EvBc2", dmMsg("UALEX", "!compact", "1700000610.000002", ""))
	drainJobs(t, b)
	if bus.Pending(sidA) {
		t.Fatalf("a bare command went to dm_last: %+v", bus.Claim(sidA))
	}
	if got := postsSince(f, before); len(got) != 1 || got[0]["channel"] != "DUALEX" {
		t.Fatalf("replies = %+v", got)
	}
	// "name: !cmd" still works.
	b.handleEvent("EvBc3", dmMsg("UALEX", "flyer: !compact", "1700000610.000003", ""))
	if m := claimOne(t, bus, sidA); m.Command == nil || m.Command.Name != "compact" {
		t.Fatalf("addressed command = %+v", m)
	}
}

// Item 5: unlinking the DM a user's dm_last came from clears it.
func TestUnlinkClearsDMLast(t *testing.T) {
	b, _, bus := newTestBridge(t)
	b.handleEvent("EvUd1", dmMsg("UALEX", "<@UBOT> link flyer", "1700000620.000001", ""))
	drainJobs(t, b)
	_ = claimNotice(t, bus, sidA)
	if sid, ok := b.state.dmLast("UALEX"); !ok || sid != sidA {
		t.Fatalf("dmLast = %q %v", sid, ok)
	}
	b.handleEvent("EvUd2", dmMsg("UALEX", "<@UBOT> unlink", "1700000620.000002", ""))
	drainJobs(t, b)
	if sid, ok := b.state.dmLast("UALEX"); ok {
		t.Fatalf("dmLast survived the unlink: %q", sid)
	}
}

// Item 5: an agent's top-level DM post carries its header again when the
// previous top-level post there came from another agent.
func TestDMHeaderRepeatsAfterAnotherAgentPosted(t *testing.T) {
	b, f, bus := newTestBridge(t)
	headerA := sessionHeader(outboundFor(bus, sidA, "flyer", ""))
	headerB := sessionHeader(outboundFor(bus, sidB, "", ""))
	sendDM(t, b, bus, sidA, "alex", "one")
	sendDM(t, b, bus, sidA, "alex", "two")
	sendDM(t, b, bus, sidB, "alex", "three")
	sendDM(t, b, bus, sidA, "alex", "four")
	var texts []string
	for _, p := range postsTo(f, "DUALEX") {
		texts = append(texts, p["text"])
	}
	want := []string{headerA + "\none", "two", headerB + "\nthree", headerA + "\nfour"}
	if strings.Join(texts, "|") != strings.Join(want, "|") {
		t.Fatalf("DM posts = %q, want %q", texts, want)
	}
}

// Item 6: an in-thread tag reaches only a live session, and says so.
func TestThreadTagResolvesOnlyLiveSessions(t *testing.T) {
	clock := newTestClock()
	b, f, bus := newClockBridge(t, clock)
	bus.Hello(sidB, "pc", "/work/other", "other", true)
	root := threadOf(t, b, bus)

	before := len(f.callsTo("chat.postMessage"))
	b.handleEvent("EvTl1", msg("UALEX", "other: look", "1700003000.000001", root))
	if m := claimOne(t, bus, sidB); m.Body != "look" {
		t.Fatalf("tagged = %+v", m)
	}
	drainJobs(t, b)
	if got := postsSince(f, before); len(got) != 1 || got[0]["thread_ts"] != root || got[0]["text"] != "→ sent to `other`" {
		t.Fatalf("tag replies = %+v", got)
	}

	// sidB goes offline; sidA stays.
	clock.advance(time.Hour)
	bus.Hello(sidA, "pc", "/work/flyer", "flyer", true)
	b.handleEvent("EvTl2", msg("UALEX", "other: look again", "1700003000.000002", root))
	if bus.Pending(sidB) {
		t.Fatalf("an offline session was tagged: %+v", bus.Claim(sidB))
	}
	if m := claimOne(t, bus, sidA); m.Body != "other: look again" {
		t.Fatalf("thread's agent got %+v", m)
	}
}

// Item 6: a top-level "name:" still queues for an offline agent, and says it
// is offline.
func TestTopLevelAddressToOfflineAgentQueuesAndSaysOffline(t *testing.T) {
	clock := newTestClock()
	b, f, bus := newClockBridge(t, clock)
	clock.advance(time.Hour)
	before := len(f.callsTo("chat.postMessage"))
	b.handleEvent("EvOf1", msg("UALEX", "flyer: when you're back", "1700003100.000001", ""))
	if m := claimOne(t, bus, sidA); m.Body != "when you're back" {
		t.Fatalf("queued = %+v", m)
	}
	drainJobs(t, b)
	got := postsSince(f, before)
	if len(got) != 1 || got[0]["thread_ts"] != "1700003100.000001" || !strings.Contains(got[0]["text"], "`flyer` is offline") {
		t.Fatalf("replies = %+v", got)
	}
}
