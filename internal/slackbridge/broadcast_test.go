package slackbridge

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/agentbus"
)

// claimBroadcast claims sid's one message and checks it is alex's broadcast
// of body, marked as one.
func claimBroadcast(t *testing.T, bus *agentbus.Store, sid, body, via string) agentbus.Message {
	t.Helper()
	m := claimOne(t, bus, sid)
	if !m.FromUser || !m.Broadcast || m.Guest || m.SlackUser != "alex" || m.Body != body || m.Via != via || m.Command != nil {
		t.Fatalf("%s got %+v", sid, m)
	}
	return m
}

func TestOwnerBroadcastReachesLiveSessions(t *testing.T) {
	b, f, bus := newTestBridge(t)
	bus.Hello(sidC, "pc", "/work/gone", "gone", true)
	bus.Bye(sidC)

	b.handleEvent("EvB1", msg("UALEX", "all: status please", "1700011000.000001", ""))
	claimBroadcast(t, bus, sidA, "status please", "")
	mB := claimBroadcast(t, bus, sidB, "status please", "")
	if bus.Pending(sidC) {
		t.Fatal("an offline session got the broadcast")
	}
	drainJobs(t, b)
	reply := lastPost(t, f)
	if reply["channel"] != "CAGENTS" || reply["thread_ts"] != "1700011000.000001" || !strings.HasPrefix(reply["text"], "→ sent to 2 agents: ") ||
		!strings.Contains(reply["text"], "`flyer`") || !strings.Contains(reply["text"], "`pc/other-bbbbbb`") || strings.Contains(reply["text"], "gone") {
		t.Fatalf("reply = %+v", reply)
	}
	// An agent answers in the broadcast's thread; the thread is no agent's.
	sendReply(t, b, bus, sidB, "all good", mB.ID)
	if got := lastPost(t, f); got["channel"] != "CAGENTS" || got["thread_ts"] != "1700011000.000001" {
		t.Fatalf("answer = %+v", got)
	}
	if sid, ok := b.state.session("1700011000.000001"); ok {
		t.Fatalf("the broadcast's thread was linked to %s", sid)
	}

	// "@all" from an owner's DM: answers at the DM's top level; dm_last
	// stays as it was.
	b.handleEvent("EvB2", dmMsg("UALEX", "@all ship it", "1700011000.000002", ""))
	mA := claimBroadcast(t, bus, sidA, "ship it", agentbus.ViaDM)
	claimBroadcast(t, bus, sidB, "ship it", agentbus.ViaDM)
	drainJobs(t, b)
	if got := lastPost(t, f); got["channel"] != "DUALEX" || got["thread_ts"] != "" || !strings.HasPrefix(got["text"], "→ sent to 2 agents: ") {
		t.Fatalf("DM reply = %+v", got)
	}
	if sid, ok := b.state.dmLast("UALEX"); ok {
		t.Fatalf("dm_last = %s", sid)
	}
	sendReply(t, b, bus, sidA, "shipped", mA.ID)
	if got := lastPost(t, f); got["channel"] != "DUALEX" || got["thread_ts"] != "" {
		t.Fatalf("DM answer = %+v", got)
	}

	// In a thread, too.
	b.handleEvent("EvB3", msg("UALEX", "all: and here", "1700011000.000003", "1700011000.000001"))
	claimBroadcast(t, bus, sidA, "and here", "")
	claimBroadcast(t, bus, sidB, "and here", "")
}

// A non-owner's "all: …" is no broadcast: it takes the normal routes.
func TestNonOwnerAllTakesTheNormalRoutes(t *testing.T) {
	b, f, bus := newTestBridge(t)
	if _, _, err := b.state.allow("UJANE", "jane"); err != nil {
		t.Fatal(err)
	}
	// In the channel, "all" is no agent: the not-found help.
	b.handleEvent("EvBn1", msg("UJANE", "all: hi", "1700011100.000001", ""))
	drainJobs(t, b)
	if bus.Pending(sidA) || bus.Pending(sidB) {
		t.Fatal("a non-owner's all: was delivered")
	}
	if got := lastPostText(f); !strings.HasPrefix(got, "No agent called `all`.") {
		t.Fatalf("channel reply = %q", got)
	}
	// In their DM it falls back to dm_last (Part D).
	b.handleEvent("EvBn2", dmMsg("UJANE", "flyer: hello", "1700011100.000002", ""))
	_ = deliveredID(t, bus, sidA)
	b.handleEvent("EvBn3", dmMsg("UJANE", "all: hi", "1700011100.000003", ""))
	if m := claimOne(t, bus, sidA); m.Body != "all: hi" || m.Broadcast {
		t.Fatalf("DM fallback = %+v", m)
	}
	if bus.Pending(sidB) {
		t.Fatal("broadcast")
	}
	// A command needs a real agent.
	b.handleEvent("EvBn4", msg("UJANE", "all: !compact", "1700011100.000004", ""))
	drainJobs(t, b)
	if bus.Pending(sidA) || bus.Pending(sidB) {
		t.Fatal("a non-owner's command was delivered")
	}
	for _, p := range f.callsTo("chat.postMessage") {
		if p.Form.Get("text") == ownersOnlyBroadcast {
			t.Fatalf("refused as a broadcast: %+v", p.Form)
		}
	}
}

// Where the bot was only added, an owner's "All: …" without mentioning the
// bot is no broadcast, and the bot stays quiet.
func TestOwnerAllWhereTheBotWasOnlyAddedIsIgnored(t *testing.T) {
	b, f, bus := newTestBridge(t)
	other := messageEvent{Type: "message", Channel: "C0OTHER", ChannelType: "channel", User: "UALEX", Text: "All: lunch?", TS: "1700011150.000002"}
	b.handleEvent("EvBo1", foreignMsg("UALEX", "All: lunch?", "1700011150.000001", ""))
	b.handleEvent("EvBo2", other)
	drainJobs(t, b)
	if bus.Pending(sidA) || bus.Pending(sidB) {
		t.Fatal("broadcast from a conversation the bot was only added to")
	}
	if n := len(f.callsTo("chat.postMessage")); n != 0 {
		t.Fatalf("posts = %d", n)
	}
}

func TestBroadcastReplyNamesPublicly(t *testing.T) {
	b, f, bus := newTestBridge(t)
	// In a group DM it takes a mention of the bot.
	b.handleEvent("EvBp1", foreignMsg("UALEX", "<@UBOT> all: hi", "1700011200.000001", ""))
	claimBroadcast(t, bus, sidA, "hi", agentbus.ViaGroup)
	claimBroadcast(t, bus, sidB, "hi", agentbus.ViaGroup)
	drainJobs(t, b)
	got := lastPost(t, f)
	if got["channel"] != "GMPIM1" || strings.Contains(got["text"], "pc/") || strings.Contains(got["text"], "· pc") ||
		!strings.Contains(got["text"], "`flyer`") || !strings.Contains(got["text"], "`an agent`") {
		t.Fatalf("reply = %+v", got)
	}
}

func TestBroadcastReplyListsAtMost15(t *testing.T) {
	b, f, bus := newTestBridge(t)
	for i := 0; i < 17; i++ {
		sid := fmt.Sprintf("dddddd%02d-2222-3333-4444-555555555555", i)
		bus.Hello(sid, "pc", "/work/n", fmt.Sprintf("n%02d", i), true)
	}
	b.handleEvent("EvBl1", msg("UALEX", "all: hi", "1700011300.000001", ""))
	drainJobs(t, b)
	got := lastPost(t, f)["text"]
	if !strings.HasPrefix(got, "→ sent to 19 agents: ") || !strings.HasSuffix(got, " +4 more") || strings.Count(got, "`")/2 != helpLimit {
		t.Fatalf("reply = %q", got)
	}
}

func TestBroadcastWithNoAgentsOnline(t *testing.T) {
	b, f, bus := newTestBridge(t)
	bus.Bye(sidA)
	bus.Bye(sidB)
	b.handleEvent("EvBz1", msg("UALEX", "all: anyone?", "1700011400.000001", ""))
	drainJobs(t, b)
	if got := lastPostText(f); got != "No agents are online right now." {
		t.Fatalf("reply = %q", got)
	}
	if got := f.reactionsOn("CAGENTS", "1700011400.000001"); len(got) != 0 {
		t.Fatalf("reactions = %v", got)
	}
}

func TestBroadcastAggregateReceipts(t *testing.T) {
	b, f, bus := newTestBridge(t)
	const ts = "1700011500.000001"
	b.handleEvent("EvBr1", msg("UALEX", "all: report", ts, ""))
	idA := claimBroadcast(t, bus, sidA, "report", "").ID
	idB := claimBroadcast(t, bus, sidB, "report", "").ID
	step := func(want string, change func()) {
		t.Helper()
		change()
		drainJobs(t, b)
		if got := f.reactionsOn("CAGENTS", ts); !reflect.DeepEqual(got, []string{want}) {
			t.Fatalf("reactions = %v, want %s", got, want)
		}
	}
	step(reactionQueued, func() {})
	// Any recipient that received it: 📨.
	step(reactionReceived, func() { b.Received([]string{idA}) })
	// One read, one not yet: still 📨.
	step(reactionReceived, func() { b.Read([]string{idA}) })
	step(reactionReceived, func() { b.Received([]string{idB}) })
	// The last one dismissed it: that counts as read, so all have: 👀.
	step(reactionRead, func() {
		if n := b.Dismissed(sidB, []string{idB}); n != 1 {
			t.Fatalf("dismissed = %d", n)
		}
	})

	// All read the plain way, too.
	const ts2 = "1700011500.000002"
	b.handleEvent("EvBr2", msg("UALEX", "all: again", ts2, ""))
	ids := []string{claimOne(t, bus, sidA).ID, claimOne(t, bus, sidB).ID}
	b.Read(ids[:1])
	drainJobs(t, b)
	if got := f.reactionsOn("CAGENTS", ts2); !reflect.DeepEqual(got, []string{reactionReceived}) {
		t.Fatalf("one read: %v", got)
	}
	b.Read(ids[1:])
	drainJobs(t, b)
	if got := f.reactionsOn("CAGENTS", ts2); !reflect.DeepEqual(got, []string{reactionRead}) {
		t.Fatalf("all read: %v", got)
	}
}

// Task 9 fix round 1 (I3): the aggregate is monotone in each recipient's
// own rank. It shows ⏳ while any recipient is working, or is done but
// another isn't done/read/dismissed yet (a recipient reaching done must
// never drag the group backward to 📨), and ✅ once every non-dismissed
// recipient is done, with at least one.
func TestBroadcastAggregateWithWorkingAndDone(t *testing.T) {
	b, f, bus := newTestBridge(t)
	const ts = "1700011500.000003"
	b.handleEvent("EvBr3", msg("UALEX", "all: status", ts, ""))
	idA := claimBroadcast(t, bus, sidA, "status", "").ID
	idB := claimBroadcast(t, bus, sidB, "status", "").ID
	step := func(want string, change func()) {
		t.Helper()
		change()
		drainJobs(t, b)
		if got := f.reactionsOn("CAGENTS", ts); !reflect.DeepEqual(got, []string{want}) {
			t.Fatalf("reactions = %v, want %s", got, want)
		}
	}
	step(reactionQueued, func() {})
	step(reactionWorking, func() { b.Working(sidA, []string{idA}) })
	// One received, one still working: still ⏳.
	step(reactionWorking, func() { b.Received([]string{idB}) })
	// idA finishes and marks done; idB is still only received. The group must not drop below
	// where it already was: ⏳, not 📨.
	step(reactionWorking, func() { bus.Done(sidA, []string{idA}) })
	// idB marks done too: now every recipient is done.
	step(reactionDone, func() { bus.Done(sidB, []string{idB}) })
}

// Task 9 fix round 1 (I3): one recipient working and another already done
// still shows ⏳ — the done recipient never masks the other's working.
func TestBroadcastAggregateWorkingAndDoneShowsWorking(t *testing.T) {
	b, f, bus := newTestBridge(t)
	const ts = "1700011500.000005"
	b.handleEvent("EvBr5", msg("UALEX", "all: ship it", ts, ""))
	idA := claimBroadcast(t, bus, sidA, "ship it", "").ID
	idB := claimBroadcast(t, bus, sidB, "ship it", "").ID
	drainJobs(t, b)
	if n := bus.Done(sidB, []string{idB}); n != 1 {
		t.Fatalf("done B = %d", n)
	}
	if n := bus.Working(sidA, []string{idA}); n != 1 {
		t.Fatalf("working A = %d", n)
	}
	drainJobs(t, b)
	if got := f.reactionsOn("CAGENTS", ts); !reflect.DeepEqual(got, []string{reactionWorking}) {
		t.Fatalf("A working, B done = %v", got)
	}
}

// Task 9: when every recipient dismisses a broadcast and none marks it
// done, the aggregate shows no reaction at all.
func TestBroadcastAggregateAllDismissedShowsNoReaction(t *testing.T) {
	b, f, bus := newTestBridge(t)
	const ts = "1700011500.000004"
	b.handleEvent("EvBr4", msg("UALEX", "all: noise", ts, ""))
	idA := claimBroadcast(t, bus, sidA, "noise", "").ID
	idB := claimBroadcast(t, bus, sidB, "noise", "").ID
	drainJobs(t, b)
	if n := bus.Dismiss(sidA, []string{idA}); n != 1 {
		t.Fatalf("dismiss A = %d", n)
	}
	drainJobs(t, b)
	// One recipient dismissed, the other hasn't yet: a dismissal alone
	// counts as received, same as before this counted toward "done or
	// dismissed" too.
	if got := f.reactionsOn("CAGENTS", ts); !reflect.DeepEqual(got, []string{reactionReceived}) {
		t.Fatalf("one dismissed: %v", got)
	}
	if n := bus.Dismiss(sidB, []string{idB}); n != 1 {
		t.Fatalf("dismiss B = %d", n)
	}
	drainJobs(t, b)
	if got := f.reactionsOn("CAGENTS", ts); len(got) != 0 {
		t.Fatalf("all dismissed: %v", got)
	}
}

func TestBroadcastCommand(t *testing.T) {
	b, f, bus, dir := newCommandBridge(t)
	writeCommand(t, dir, "screenshot.yaml", shellYAML, mtime0)
	const ts = "1700011600.000001"
	b.handleEvent("EvBc1", msg("UALEX", "all: !compact now", ts, ""))
	m := claimOne(t, bus, sidA)
	if m.Command == nil || m.Command.Command != "compact" || m.Command.Args != "now" || m.SlackUserID != "UALEX" || !m.Broadcast {
		t.Fatalf("command = %+v", m)
	}
	if bus.Pending(sidB) {
		t.Fatalf("a session without the plugin got the command: %+v", bus.Claim(sidB))
	}
	drainJobs(t, b)
	if got := lastPost(t, f); got["thread_ts"] != ts || got["text"] != "→ sent `!compact` to 1 agent: `flyer` (skipped: 1 without plugin 0.3.3+)" {
		t.Fatalf("reply = %+v", got)
	}
	if got := f.reactionsOn("CAGENTS", ts); !reflect.DeepEqual(got, []string{reactionCommand}) {
		t.Fatalf("reactions = %v", got)
	}
	// The command's report goes to the broadcast's thread.
	sendReply(t, b, bus, sidA, "✅ !compact: done", m.ID)
	if got := lastPost(t, f); got["thread_ts"] != ts {
		t.Fatalf("report = %+v", got)
	}

	// Shell commands are refused for a broadcast.
	b.handleEvent("EvBc2", msg("UALEX", "all: !screenshot", "1700011600.000002", ""))
	drainJobs(t, b)
	if bus.Pending(sidA) || bus.Pending(sidB) {
		t.Fatal("a shell command was broadcast")
	}
	if got := lastPostText(f); got != shellNoBroadcast {
		t.Fatalf("shell reply = %q", got)
	}
}

func TestClankerBroadcast(t *testing.T) {
	b, f, bus := newTestBridge(t)
	if _, _, err := b.state.allow("UJANE", "jane"); err != nil {
		t.Fatal(err)
	}
	if got := slash(t, b, "UJANE", "all: hi"); got != ownersOnlyBroadcast {
		t.Fatalf("non-owner ack = %q", got)
	}
	got := slash(t, b, "UALEX", "all: hi there")
	if !strings.HasPrefix(got, "→ sent to 2 agents: ") || !strings.HasSuffix(got, "Answers arrive in your DM with @clanker-bro.") {
		t.Fatalf("ack = %q", got)
	}
	drainJobs(t, b)
	claimBroadcast(t, bus, sidA, "hi there", agentbus.ViaSlash)
	mB := claimBroadcast(t, bus, sidB, "hi there", agentbus.ViaSlash)
	sendReply(t, b, bus, sidB, "hello", mB.ID)
	if got := lastPost(t, f); got["channel"] != "DUALEX" || got["thread_ts"] != "" {
		t.Fatalf("answer = %+v", got)
	}
	// The ack was the reply: nothing else was posted.
	if n := len(f.callsTo("chat.postMessage")); n != 1 {
		t.Fatalf("posts = %d", n)
	}
}
